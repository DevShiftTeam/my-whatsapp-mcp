package ingestion

import (
	"context"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Known IDs are consumed even after expiry or completion. They never obtain
// delivered-history or rebuild authority over the archived parent body.
func (r *Runtime) ownerVoiceDescriptorReply(ctx context.Context, event *events.Message) (bool, error) {
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
			return true, archive.ErrInvalidEvent
		}
		return true, r.projectBodylessHistory(bounded, parsed.Info.Chat, event.SourceWebMsg)
	}
	if isMutation(parsed) || isMutation(event) {
		negative, editedBody, err := r.trustedMutation(bounded, parsed)
		if err != nil {
			return true, err
		}
		if negative.Key != intent.Key {
			return true, archive.ErrInvalidEvent
		}
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
	if key != intent.Key || !parsed.Info.Timestamp.Equal(intent.OriginalSentAt) || parsed.Info.Timestamp.UnixMilli() != intent.SourceRevision {
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
	if viewOnce || parsed.Message.GetAudioMessage() == nil {
		return true, nil
	}
	pending, err := r.projectBody(bounded, &e, parsed.Message)
	defer wipePending(pending)
	if err != nil {
		return true, err
	}
	if len(e.Media) != 1 || e.Media[0].Kind != string(archive.MessageVoice) || e.Media[0].ViewOnce || len(e.Media[0].ProtocolReference) == 0 {
		return true, nil
	}
	accepted, err := store.AcceptOwnerVoiceDescriptor(bounded, archive.OwnerVoiceDescriptorReply{RequestID: event.UnavailableRequestID, Key: key, OriginalSentAt: e.OriginalSentAt, SourceRevision: e.SourceRevision, ExpiresAt: e.ExpiresAt, Media: e.Media[0]})
	if accepted && err == nil {
		r.WakeOriginalDownloads()
	}
	return true, err
}
