package ingestion

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

type historyObservationContextKey struct{}
type historyUncertaintyStore interface {
	RecordHistoryUncertainty(context.Context, string, archive.HistoryUncertaintyReason, archive.ObservationID) error
}

func (r *Runtime) captureHistoryObservation(ctx context.Context) context.Context {
	if _, present := ctx.Value(historyObservationContextKey{}).(archive.ObservationID); present {
		return ctx
	}
	r.mu.Lock()
	observation := r.observation
	r.mu.Unlock()
	return context.WithValue(ctx, historyObservationContextKey{}, observation)
}
func (r *Runtime) recordHistoryUncertainty(ctx context.Context, reason archive.HistoryUncertaintyReason) error {
	store, ok := r.store.(historyUncertaintyStore)
	observation, _ := ctx.Value(historyObservationContextKey{}).(archive.ObservationID)
	if !ok {
		return archive.ErrArchiveUnavailable
	}
	if err := store.RecordHistoryUncertainty(ctx, r.SourceAccount(), reason, observation); err != nil {
		return err
	}
	retainIncompleteHistoryRaw(ctx)
	return nil
}

// Only complete authenticated history uses this dispatcher. Unsupported findings
// never excuse an error from a recognized negative handler. Discarded metadata is
// not passed to the SDK projector, mapping, names or frontier paths.
func (r *Runtime) projectPartialHistoryRecord(ctx context.Context, chat types.JID, web *waWeb.WebMessageInfo, a historyAnalysis) error {
	kind := web.GetMessageStubType()
	recognized := kind == waWeb.WebMessageInfo_REVOKE || kind == waWeb.WebMessageInfo_ADMIN_REVOKE || kind == waWeb.WebMessageInfo_CIPHERTEXT
	if recognized || benignHistoryStub(kind) && a.reason() == "" {
		if err := r.validateHistoryControlScope(ctx, chat, web); err != nil {
			return err
		}
	}
	if web.GetEphemeralExpirationTimestamp() > 0 || web.GetEphemeralStartTimestamp() > 0 || web.GetEphemeralDuration() > 0 {
		if err := r.projectPartialHistoryExpiry(ctx, chat, web); err != nil {
			return err
		}
	}
	if recognized {
		// These fields are all the existing handler's inputs. Additional unsupported
		// metadata remains represented by a durable uncertainty after the negative.
		control := &waWeb.WebMessageInfo{Key: proto.Clone(web.Key).(*waCommon.MessageKey), Participant: web.Participant, MessageStubType: web.MessageStubType, OriginalSelfAuthorUserJIDString: web.OriginalSelfAuthorUserJIDString}
		if err := r.projectBodylessHistory(ctx, chat, control); err != nil {
			return err
		}
	}
	reason := a.reason()
	if !recognized && !benignHistoryStub(kind) {
		if reason == "" {
			reason = archive.HistoryUncertaintyUnsupportedNotice
		}
	}
	if reason != "" {
		return r.recordHistoryUncertainty(ctx, reason)
	}
	if recognized {
		return nil
	}
	return r.projectBodylessHistory(ctx, chat, web)
}

// This authority check is intentionally independent of the zero-output fallback.
// Unused malformed notice actors do not become authority through normalization.
func (r *Runtime) validateHistoryControlScope(ctx context.Context, chat types.JID, web *waWeb.WebMessageInfo) error {
	key := web.GetKey()
	if key == nil || key.ID == nil || key.GetID() == "" || len(key.GetID()) > 256 || !utf8.ValidString(key.GetID()) || strings.IndexByte(key.GetID(), 0) >= 0 || key.FromMe == nil || !partialHistoryJID(key.GetRemoteJID(), true) || !partialHistoryJID(chat.String(), true) {
		return historyBodylessFailure(web, "record_structure")
	}
	expected, err := r.canonical(ctx, chat.String())
	if err != nil {
		return err
	}
	actual, err := r.canonical(ctx, key.GetRemoteJID())
	if err != nil || expected != actual {
		return historyBodylessFailure(web, "record_jid_mismatch")
	}
	for _, actor := range []*string{key.Participant, web.Participant, web.OriginalSelfAuthorUserJIDString} {
		if actor != nil && !partialHistoryJID(*actor, false) {
			return historyBodylessFailure(web, "record_structure")
		}
	}
	if web.GetMessageStubType() != waWeb.WebMessageInfo_ADMIN_REVOKE {
		own, err := r.canonical(ctx, r.SourceAccount())
		if err != nil {
			return err
		}
		var author string
		for _, claim := range []*string{key.Participant, web.Participant} {
			if claim == nil {
				continue
			}
			canonical, err := r.canonical(ctx, *claim)
			if err != nil {
				return err
			}
			if author != "" && author != canonical {
				return historyBodylessFailure(web, "record_structure")
			}
			author = canonical
		}
		if author != "" && ((author == own) != key.GetFromMe() || !key.GetFromMe() && chat.Server != types.GroupServer && author != expected) {
			return historyBodylessFailure(web, "record_structure")
		}
	}
	return nil
}
func (r *Runtime) projectPartialHistoryExpiry(ctx context.Context, chat types.JID, web *waWeb.WebMessageInfo) error {
	if err := r.validateHistoryControlScope(ctx, chat, web); err != nil {
		return err
	}
	if web.GetKey().GetParticipant() != "" && web.GetParticipant() != "" {
		a, err := r.canonical(ctx, web.GetKey().GetParticipant())
		if err != nil {
			return err
		}
		b, err := r.canonical(ctx, web.GetParticipant())
		if err != nil || a != b {
			return historyBodylessFailure(web, "record_structure")
		}
	}
	event, err := r.client.ParseWebMessage(chat, web)
	if err != nil {
		return historyBodylessFailure(web, "record_parse")
	}
	key, err := r.envelopeKey(ctx, event.Info)
	if err != nil {
		return err
	}
	own, err := r.canonical(ctx, r.SourceAccount())
	if err != nil {
		return err
	}
	if (key.Sender == own) != key.FromMe {
		return historyBodylessFailure(web, "record_structure")
	}
	event.SourceWebMsg = web
	source := archive.SourceEvent{Key: key}
	if err := r.sourceExpiry(ctx, &source, event); err != nil {
		return err
	}
	store, ok := r.store.(interface {
		RecordRebuildRestriction(context.Context, archive.SourceKey, time.Time, bool) error
	})
	if !ok || r.config.Rebuild == nil || source.ExpiresAt.IsZero() {
		return historyBodylessFailure(web, "record_safety_metadata")
	}
	return store.RecordRebuildRestriction(ctx, key, source.ExpiresAt, false)
}
