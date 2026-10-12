package ingestion

import (
	"context"
	"errors"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type rebuildStore interface {
	RebuildDispatchState(context.Context) (*archive.RebuildProgress, error)
	NextRebuildCandidates(context.Context, int) ([]archive.SourceAnchor, error)
	NextRebuildDiscovery(context.Context) (*archive.RebuildDiscovery, error)
	RegisterRebuildDiscovery(context.Context, archive.RebuildDiscovery) error
	DeferRebuildCandidate(context.Context, archive.SourceKey) error
	PendingRebuildRequest(context.Context, archive.RebuildRequest) error
	MatchRebuildRequest(context.Context, string, archive.SourceKey, string, archive.Generation) (bool, error)
	RebuildGuard(context.Context, archive.SourceKey) (archive.RebuildGuard, error)
	RecordRebuildDiscovery(context.Context, archive.SourceAnchor) error
	RecordRebuildRestriction(context.Context, archive.SourceKey, time.Time, bool) error
}

func configureRebuild(ctx context.Context, config *Config, store archive.ConnectorStore) error {
	if source, ok := store.(interface {
		RebuildProvenance(context.Context) (*archive.RebuildProvenance, error)
	}); ok {
		p, err := source.RebuildProvenance(ctx)
		if err != nil {
			return err
		}
		if p != nil {
			if config.Rebuild == nil {
				config.Rebuild = &RebuildConfig{ObservationID: p.Generation, Account: p.Account, DeviceID: p.DeviceID, Boundary: p.Boundary, Requests: 32}
			}
			if config.Rebuild.ObservationID != p.Generation || config.Rebuild.Account != p.Account || !config.Rebuild.Boundary.Equal(p.Boundary) {
				return archive.ErrInvalidEvent
			}
			if config.Rebuild.DeviceID != "" && config.Rebuild.DeviceID != p.DeviceID {
				return archive.ErrInvalidEvent
			}
			c := *config.Rebuild
			c.DeviceID = p.DeviceID
			config.Rebuild = &c
		} else if config.Rebuild != nil {
			return archive.ErrInvalidEvent
		}
	}
	if config.Rebuild == nil {
		return nil
	}
	c := config.Rebuild
	if c.ObservationID == "" || len(c.ObservationID) > 128 || c.Account == "" || c.Boundary.IsZero() || c.Requests < 0 || c.Requests > 32 {
		return archive.ErrInvalidEvent
	}
	if _, ok := store.(rebuildStore); !ok {
		return archive.ErrInvalidEvent
	}
	// Copy caller-owned configuration before any asynchronous work.
	copied := *c
	config.Rebuild = &copied
	return nil
}

func (r *Runtime) rebuildAdmission(ctx context.Context, e *archive.SourceEvent, event *events.Message, policy archive.IngestionPolicy) (bool, error) {
	c := r.config.Rebuild
	if c == nil {
		return true, nil
	}
	if e.Key.Account != c.Account {
		return false, archive.ErrUnauthorized
	}
	old := !e.OriginalDateKnown || e.OriginalSentAt.Before(c.Boundary)
	if old && event.UnavailableRequestID == "" && e.Kind == archive.EventOriginal {
		return false, r.rebuildDiscovery(ctx, event)
	}
	if err := r.recordRebuildRestriction(ctx, e, event); err != nil {
		return false, err
	}
	// In the pinned SDK only authenticated own-primary placeholder responses set
	// this field (handleProtocolMessage: IsFromMe and Sender.Device==0). The SDK
	// then replaces the envelope with ParseWebMessage of the exact returned item.
	if event.UnavailableRequestID != "" {
		if event.SourceWebMsg == nil {
			return false, nil
		}
		store, ok := r.store.(rebuildStore)
		if !ok {
			return false, archive.ErrInvalidEvent
		}
		match, err := store.MatchRebuildRequest(ctx, event.UnavailableRequestID, e.Key, c.ObservationID, policy.Generation)
		if err != nil || !match {
			return false, err
		}
		e.RebuildRequestID = event.UnavailableRequestID
		return true, nil // writer repeats matching and consumes atomically with admission
	}
	if old {
		return false, nil // edit invalidation was already applied; old positive edit awaits fetch
	}
	return true, nil
}

func (r *Runtime) rebuildDiscovery(ctx context.Context, event *events.Message) error {
	c := r.config.Rebuild
	if c == nil {
		return nil
	}
	key, err := r.envelopeKey(ctx, event.Info)
	if err != nil {
		return err
	}
	if key.Account != c.Account {
		return archive.ErrUnauthorized
	}
	candidate := archive.SourceEvent{Key: key, OriginalSentAt: event.Info.Timestamp, OriginalDateKnown: !event.Info.Timestamp.IsZero()}
	if err = r.sourceExpiry(ctx, &candidate, event); err != nil {
		return err
	}
	if err = r.recordRebuildRestriction(ctx, &candidate, event); err != nil {
		return err
	}
	if event.Info.Timestamp.IsZero() || rebuildViewOnce(event) {
		return nil
	}
	store, ok := r.store.(rebuildStore)
	if !ok {
		return archive.ErrInvalidEvent
	}
	guard, err := store.RebuildGuard(ctx, key)
	if err != nil {
		return err
	}
	if guard.Deleted || guard.ViewOnce || !guard.ChatClearCutoff.IsZero() && !candidate.OriginalSentAt.After(guard.ChatClearCutoff) {
		return nil
	}
	if !guard.ExpiresAt.IsZero() && (candidate.ExpiresAt.IsZero() || guard.ExpiresAt.Before(candidate.ExpiresAt)) {
		candidate.ExpiresAt = guard.ExpiresAt
	}
	if !candidate.ExpiresAt.IsZero() && !candidate.ExpiresAt.After(time.Now()) {
		return nil
	}
	// Private discovery is retained independently of the current positive policy.
	// Current policy is checked by bounded selectors and again at registration.
	anchor := archive.SourceAnchor{Key: key, OriginalSentAt: event.Info.Timestamp}
	if err = store.RecordRebuildDiscovery(ctx, anchor); err != nil {
		if errors.Is(err, archive.ErrQueueLimitExceeded) {
			return nil
		} // positive capacity never drops controls
		return err
	}
	r.WakeRebuild()
	return nil
}

func (r *Runtime) requestRebuildCopy(ctx context.Context, anchor archive.SourceAnchor) error {
	store, ok := r.store.(rebuildStore)
	if !ok {
		return archive.ErrInvalidEvent
	}
	p, err := store.RebuildDispatchState(ctx)
	if err != nil {
		return err
	}
	if p == nil || p.State != "running" {
		return archive.ErrNoWork
	}
	_, err = r.dispatchRebuildCopy(ctx, anchor, p)
	return err
}

func (r *Runtime) rebuildTransport(ctx context.Context, anchor archive.SourceAnchor, expectedPolicy archive.Generation) (types.JID, types.JID, error) {
	zero := types.EmptyJID
	c := r.config.Rebuild
	if c == nil || anchor.Key.Account != c.Account || r.client.Store.ID == nil || r.client.Store.ID.ToNonAD().String() != c.Account {
		return zero, zero, archive.ErrUnauthorized
	}
	store, ok := r.store.(rebuildStore)
	if !ok {
		return zero, zero, archive.ErrInvalidEvent
	}
	policy, err := r.store.SourcePolicy(ctx, c.Account)
	if err != nil {
		return zero, zero, err
	}
	if policy.Generation != expectedPolicy {
		return zero, zero, archive.ErrPolicyChanged
	}
	guard, err := store.RebuildGuard(ctx, anchor.Key)
	if err != nil {
		return zero, zero, err
	}
	if guard.Deleted || guard.ViewOnce || !guard.ChatClearCutoff.IsZero() && !anchor.OriginalSentAt.After(guard.ChatClearCutoff) || !policyPermits(policy, archive.SourceEvent{Key: anchor.Key, OriginalSentAt: anchor.OriginalSentAt, OriginalDateKnown: !anchor.OriginalSentAt.IsZero(), ExpiresAt: guard.ExpiresAt}, time.Now()) {
		return zero, zero, archive.ErrSourceUnavailable
	}
	chat, err := types.ParseJID(anchor.Key.Chat)
	if err != nil || chat.User == "" || chat != chat.ToNonAD() || chat.Server != types.DefaultUserServer && chat.Server != types.HiddenUserServer && chat.Server != types.GroupServer {
		return zero, zero, archive.ErrInvalidEvent
	}
	sender, err := types.ParseJID(anchor.Key.Sender)
	if err != nil || sender.User == "" || sender != sender.ToNonAD() || sender.Server != types.DefaultUserServer && sender.Server != types.HiddenUserServer {
		return zero, zero, archive.ErrInvalidEvent
	}
	chat, err = r.voiceRedeliveryTransportJID(ctx, c.Account, chat)
	if err != nil {
		return zero, zero, err
	}
	sender, err = r.voiceRedeliveryTransportJID(ctx, c.Account, sender)
	if err != nil {
		return zero, zero, err
	}
	return chat, sender, nil
}

// A true result means the durable send authority was spent, even when the
// network result is unknown. Such keys remain excluded from automatic retry.
func (r *Runtime) dispatchRebuildCopy(ctx context.Context, anchor archive.SourceAnchor, p *archive.RebuildProgress) (bool, error) {
	chat, sender, err := r.rebuildTransport(ctx, anchor, p.PolicyGeneration)
	if err != nil {
		return false, r.deferRebuildTransport(ctx, anchor.Key, err)
	}
	request := r.client.BuildUnavailableMessageRequest(chat, sender, anchor.Key.Message)
	keys := request.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetPlaceholderMessageResendRequest()
	if len(keys) != 1 || keys[0].GetMessageKey().GetFromMe() != anchor.Key.FromMe {
		return false, r.deferRebuildTransport(ctx, anchor.Key, ErrMediaInvalidAdmission)
	}
	if !r.optionalReady() {
		return false, ErrMediaCanceled
	}
	id := r.client.GenerateMessageID()
	store := r.store.(rebuildStore)
	if err = store.PendingRebuildRequest(ctx, archive.RebuildRequest{ID: id, Generation: r.config.Rebuild.ObservationID, OperationID: p.OperationID, OperationRevision: p.Revision, Key: anchor.Key, PolicyGeneration: p.PolicyGeneration, CreatedAt: time.Now()}); err != nil {
		return false, err
	}
	return true, r.sendRebuildPeer(ctx, id, request)
}
func (r *Runtime) dispatchRebuildDiscovery(ctx context.Context, q archive.RebuildDiscovery) (bool, error) {
	chat, sender, err := r.rebuildTransport(ctx, q.Anchor, q.PolicyGeneration)
	if err != nil {
		return false, r.deferRebuildTransport(ctx, q.Anchor.Key, err)
	}
	info := types.MessageInfo{ID: q.Anchor.Key.Message, Timestamp: q.Anchor.OriginalSentAt, MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: q.Anchor.Key.FromMe}}
	request := r.client.BuildHistorySyncRequest(&info, 50)
	if !r.optionalReady() {
		return false, ErrMediaCanceled
	}
	if err = r.store.(rebuildStore).RegisterRebuildDiscovery(ctx, q); err != nil {
		return false, err
	}
	// The ID proves dispatch accounting only. No batch body correlation is inferred.
	return true, r.sendRebuildPeer(ctx, q.ID, request)
}
func (r *Runtime) sendRebuildPeer(ctx context.Context, id string, request *waE2E.Message) error {
	if r.sendRebuildRequest != nil {
		return r.sendRebuildRequest(ctx, id, request)
	}
	_, err := r.client.SendMessage(ctx, r.client.Store.ID.ToNonAD(), request, whatsmeow.SendRequestExtra{ID: id, Peer: true})
	return err
}

// Security metadata from authenticated withheld bodies is still negative
// authority. This DTO-free boundary never passes content or media descriptors.
func (r *Runtime) recordRebuildRestriction(ctx context.Context, e *archive.SourceEvent, event *events.Message) error {
	viewOnce := rebuildViewOnce(event)
	if e.ExpiresAt.IsZero() && !viewOnce {
		return nil
	}
	store, ok := r.store.(rebuildStore)
	if !ok {
		r.failClosed("control_admission_failed")
		return archive.ErrInvalidEvent
	}
	if err := store.RecordRebuildRestriction(ctx, e.Key, e.ExpiresAt, viewOnce); err != nil {
		r.failClosed("control_admission_failed")
		return err
	}
	return nil
}
func rebuildViewOnce(event *events.Message) bool {
	if event == nil {
		return false
	}
	m := event.Message
	return event.IsViewOnce || event.IsViewOnceV2 || event.IsViewOnceV2Extension || m.GetImageMessage().GetViewOnce() || m.GetAudioMessage().GetViewOnce() || m.GetVideoMessage().GetViewOnce() || m.GetExtendedTextMessage().GetViewOnce()
}

// History edits carry authoritative negative effects while their uncorrelated
// positive bodies remain excluded. Preserve expiry/view-once before invalidation.
func (r *Runtime) rebuildHistoryMutation(ctx context.Context, event *events.Message) error {
	e, _, err := r.trustedMutation(ctx, event)
	if err != nil {
		return err
	}
	if e.Kind != archive.EventDelete {
		if err = r.sourceExpiry(ctx, &e, event); err != nil {
			return err
		}
		if err = r.recordRebuildRestriction(ctx, &e, event); err != nil {
			return err
		}
	}
	_, err = r.apply(ctx, e)
	if err == archive.ErrSourceRevisionConflict {
		// The writer committed this message's quarantine and scoped gap.
		// Continue unrelated history without admitting the withheld edit body.
		return nil
	}
	return err
}

// Only preparation failures are retryable deferrals. Registered requests are
// never routed here: their unknown outcomes stay excluded by durable evidence.
func (r *Runtime) deferRebuildTransport(ctx context.Context, key archive.SourceKey, cause error) error {
	if ctx.Err() != nil {
		return cause
	}
	if cause == ErrMediaUnavailable || cause == ErrMediaSourceChanged || cause == ErrMediaInvalidAdmission || cause == archive.ErrInvalidEvent {
		if err := r.store.(rebuildStore).DeferRebuildCandidate(ctx, key); err != nil {
			return err
		}
	}
	return cause
}
