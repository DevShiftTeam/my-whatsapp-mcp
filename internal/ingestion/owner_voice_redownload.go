package ingestion

import (
	"context"
	"errors"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Known IDs are consumed even after expiry or completion. They never obtain
// delivered-history or rebuild authority over the archived parent body.
func (r *Runtime) ownerVoiceDescriptorReply(ctx context.Context, event *events.Message) (handled bool, resultErr error) {
	if event == nil || event.UnavailableRequestID == "" {
		return false, nil
	}
	store, ok := r.store.(archive.OwnerVoiceRecoveryStore)
	if !ok {
		return false, nil
	}
	if len(event.UnavailableRequestID) > 128 {
		return true, archive.ErrInvalidEvent
	}
	bounded, done := context.WithTimeout(ctx, 5*time.Second)
	defer done()
	intent, known, err := store.LookupOwnerVoiceRequest(bounded, event.UnavailableRequestID)
	if err != nil {
		return true, err
	}
	if !known {
		return false, nil
	}
	// Diagnostic persistence is best effort and cannot replace the admission
	// result, skip negative controls, or grant body/descriptor authority.
	diagnostic := "voice_reply_invalid_envelope"
	defer func() {
		// Operational failures are not evidence that the phone reply is bad.
		// Preserve their original error without writing a reply-specific reason.
		if diagnostic != "" && (resultErr == nil || errors.Is(resultErr, archive.ErrInvalidEvent)) {
			_, _ = store.RecordOwnerVoiceReplyDiagnostic(bounded, event.UnavailableRequestID, intent.Source, diagnostic)
		}
	}()
	if event.SourceWebMsg == nil || !recordWithinLimit(event) || validateMessageStructure(event) != nil {
		return true, archive.ErrInvalidEvent
	}
	// For unavailable-copy replies, the pinned SDK authenticates the own-primary
	// response before attaching its request ID and SourceWebMsg. Reparse that exact returned item; envelope claims must agree.
	parsed, err := r.client.ParseWebMessage(types.EmptyJID, event.SourceWebMsg)
	if err != nil || parsed == nil {
		return true, archive.ErrInvalidEvent
	}
	key, err := r.envelopeKey(bounded, parsed.Info)
	if err != nil {
		return true, err
	}
	envelope, err := r.envelopeKey(bounded, event.Info)
	if err != nil || key != envelope || !parsed.Info.Timestamp.Equal(event.Info.Timestamp) || !proto.Equal(parsed.Message, event.Message) {
		return true, archive.ErrInvalidEvent
	}
	if parsed.Message == nil {
		if key != intent.Key {
			diagnostic = "voice_reply_key_mismatch"
			return true, archive.ErrInvalidEvent
		}
		diagnostic = "voice_reply_bodyless"
		return true, r.projectBodylessHistory(bounded, parsed.Info.Chat, event.SourceWebMsg)
	}
	if isMutation(parsed) || isMutation(event) {
		negative, editedBody, err := r.trustedMutation(bounded, parsed)
		if err != nil {
			return true, err
		}
		if negative.Key != intent.Key {
			diagnostic = "voice_reply_key_mismatch"
			return true, archive.ErrInvalidEvent
		}
		diagnostic = "voice_reply_mutation"
		if negative.Kind != archive.EventDelete {
			if err = r.sourceExpiry(bounded, &negative, parsed); err != nil {
				return true, err
			}
			edited := *parsed
			edited.Message = editedBody
			viewOnce := rebuildViewOnce(parsed) || rebuildViewOnce(event) || rebuildViewOnce(&edited)
			if !negative.ExpiresAt.IsZero() || viewOnce {
				if err = store.RecordOwnerVoiceRestriction(bounded, event.UnavailableRequestID, negative.Key, negative.ExpiresAt, viewOnce); err != nil {
					return true, err
				}
			}
		}
		_, err = r.apply(bounded, negative)
		return true, err
	}
	if key != intent.Key {
		diagnostic = "voice_reply_key_mismatch"
		return true, archive.ErrInvalidEvent
	}
	if !parsed.Info.Timestamp.Equal(intent.OriginalSentAt) || parsed.Info.Timestamp.UnixMilli() != intent.SourceRevision {
		diagnostic = "voice_reply_original_mismatch"
		return true, archive.ErrInvalidEvent
	}
	e := archive.SourceEvent{Key: key, Kind: archive.EventOriginal, OriginalSentAt: parsed.Info.Timestamp, OriginalDateKnown: !parsed.Info.Timestamp.IsZero(), SourceRevision: parsed.Info.Timestamp.UnixMilli()}
	if err = r.sourceExpiry(bounded, &e, parsed); err != nil {
		return true, err
	}
	viewOnce := rebuildViewOnce(parsed) || rebuildViewOnce(event)
	if !e.ExpiresAt.IsZero() || viewOnce {
		if err = store.RecordOwnerVoiceRestriction(bounded, event.UnavailableRequestID, key, e.ExpiresAt, viewOnce); err != nil {
			return true, err
		}
	}
	if viewOnce {
		diagnostic = "voice_reply_restricted"
		return true, nil
	}
	if parsed.Message.GetAudioMessage() == nil {
		diagnostic = "voice_reply_no_audio"
		return true, nil
	}
	diagnostic = "voice_reply_invalid_descriptor"
	pending, err := r.projectBody(bounded, &e, parsed.Message)
	defer wipePending(pending)
	if err != nil {
		return true, err
	}
	if len(e.Media) != 1 || e.Media[0].Kind != string(archive.MessageVoice) || e.Media[0].ViewOnce || len(e.Media[0].ProtocolReference) == 0 {
		return true, nil
	}
	diagnostic = "voice_reply_not_accepted"
	accepted, err := store.AcceptOwnerVoiceDescriptor(bounded, archive.OwnerVoiceDescriptorReply{RequestID: event.UnavailableRequestID, Key: key, OriginalSentAt: e.OriginalSentAt, SourceRevision: e.SourceRevision, ExpiresAt: e.ExpiresAt, Media: e.Media[0]})
	if accepted && err == nil {
		diagnostic = ""
		r.WakeOriginalDownloads()
	}
	return true, err
}
