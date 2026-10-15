package ingestion

import (
	"context"
	"slices"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// The owner-selected bulk strategy uses a fixed bounded page request.
// Phone yield is not guaranteed by this requested count.
const deliveredHistoryPageCount = 500

type deliveredHistoryStore interface {
	RebuildHistorySelection(context.Context) (*archive.RebuildHistorySelection, error)
	CaptureDeliveredHistoryAdmission(context.Context) (*archive.DeliveredHistoryAdmission, error)
	NextDeliveredHistoryPage(context.Context) (*archive.DeliveredHistoryPage, error)
	RegisterDeliveredHistoryPage(context.Context, archive.DeliveredHistoryPage) error
	ObserveDeliveredHistoryAnchor(context.Context, archive.DeliveredHistoryAdmission, archive.SourceAnchor) error
}

func configureDeliveredHistory(ctx context.Context, config *Config, store archive.ConnectorStore) error {
	if config.RebuildHistoryPolicy == "" {
		config.RebuildHistoryPolicy = archive.RebuildHistorySelectedOriginal
	}
	if config.RebuildHistoryPolicy != archive.RebuildHistorySelectedOriginal && config.RebuildHistoryPolicy != archive.RebuildHistoryDelivered {
		return archive.ErrInvalidPolicy
	}
	config.rebuildHistorySelection = nil
	source, ok := store.(interface {
		RebuildHistorySelection(context.Context) (*archive.RebuildHistorySelection, error)
	})
	if !ok {
		if config.RebuildHistoryPolicy == archive.RebuildHistoryDelivered {
			return archive.ErrInvalidPolicy
		}
		return nil
	}
	selection, err := source.RebuildHistorySelection(ctx)
	if err != nil {
		return err
	}
	if selection == nil {
		if config.RebuildHistoryPolicy == archive.RebuildHistoryDelivered {
			return archive.ErrInvalidPolicy
		}
		return nil
	}
	if config.Rebuild == nil || selection.Policy != config.RebuildHistoryPolicy || selection.Account != config.Rebuild.Account || selection.ObservationGeneration != config.Rebuild.ObservationID {
		return archive.ErrInvalidPolicy
	}
	if selection.Policy == archive.RebuildHistoryDelivered {
		if selection.StrategyEpoch == 0 {
			return archive.ErrInvalidPolicy
		}
		if _, ok := store.(deliveredHistoryStore); !ok {
			return archive.ErrInvalidPolicy
		}
	}
	copy := *selection
	config.rebuildHistorySelection = &copy
	return nil
}

// This token exists only for one authenticated notification, captured before
// download. It is never installed in mutable runtime state or inferred from IDs.
type deliveredHistoryChunk struct {
	admission archive.DeliveredHistoryAdmission
	frontiers map[string]archive.SourceAnchor
}

func (r *Runtime) captureDeliveredHistory(ctx context.Context) (*deliveredHistoryChunk, error) {
	if r.config.RebuildHistoryPolicy != archive.RebuildHistoryDelivered {
		return nil, nil
	}
	selection := r.config.rebuildHistorySelection
	store, ok := r.store.(deliveredHistoryStore)
	if !ok || selection == nil || r.config.Rebuild == nil || selection.Account != r.SourceAccount() {
		return nil, archive.ErrInvalidEvent
	}
	admission, err := store.CaptureDeliveredHistoryAdmission(ctx)
	if err != nil {
		return nil, err
	}
	if admission == nil || admission.StrategyEpoch != selection.StrategyEpoch || admission.ObservationGeneration != selection.ObservationGeneration || admission.PolicyGeneration == 0 {
		return nil, archive.ErrInvalidEvent
	}
	return &deliveredHistoryChunk{admission: *admission, frontiers: make(map[string]archive.SourceAnchor)}, nil
}

func (r *Runtime) deliveredAdmission(ctx context.Context, e *archive.SourceEvent, event *events.Message, admission *archive.DeliveredHistoryAdmission) (bool, error) {
	selection := r.config.rebuildHistorySelection
	if r.config.RebuildHistoryPolicy != archive.RebuildHistoryDelivered || selection == nil || e.Key.Account != selection.Account || admission.StrategyEpoch != selection.StrategyEpoch || admission.ObservationGeneration != selection.ObservationGeneration || event.UnavailableRequestID != "" || e.RebuildRequestID != "" {
		return false, archive.ErrInvalidEvent
	}
	// Negative restrictions retain authority even when this positive's captured
	// policy, epoch, operation or body budget is no longer admissible.
	if err := r.recordRebuildRestriction(ctx, e, event); err != nil {
		return false, err
	}
	copy := *admission
	e.DeliveredHistory = &copy
	return true, nil // the writer verifies the token in the source transaction
}

func (r *Runtime) projectDeliveredHistoryMessage(ctx context.Context, event *events.Message, chunk *deliveredHistoryChunk) error {
	err := r.messageWithDeliveredAdmission(ctx, event, &chunk.admission)
	if err == archive.ErrSourceRevisionConflict {
		retainIncompleteHistoryRaw(ctx)
		return nil // committed quarantine; no positive or frontier promotion
	}
	if err != nil && !r.recordMediaOutcome(err) {
		return err
	}
	if isMutation(event) {
		return nil
	}
	// Retain all validated discovery metadata under the existing bounded archive
	// rules, including filtered positives. No body is passed through this path.
	if err := r.rebuildDiscovery(ctx, event); err != nil {
		return err
	}
	if event.Info.Timestamp.IsZero() || rebuildViewOnce(event) {
		return nil
	}
	key, err := r.envelopeKey(ctx, event.Info)
	if err != nil {
		return err
	}
	anchor := archive.SourceAnchor{Key: key, OriginalSentAt: event.Info.Timestamp}
	old, found := chunk.frontiers[key.Chat]
	if !found && len(chunk.frontiers) >= 32 {
		return nil // bounded hint window; durable source metadata is already kept
	}
	if !found || deliveredAnchorBefore(anchor, old) {
		chunk.frontiers[key.Chat] = anchor
	}
	return nil
}

func deliveredAnchorBefore(a, b archive.SourceAnchor) bool {
	if !a.OriginalSentAt.Equal(b.OriginalSentAt) {
		return a.OriginalSentAt.Before(b.OriginalSentAt)
	}
	if a.Key.Sender != b.Key.Sender {
		return a.Key.Sender < b.Key.Sender
	}
	if a.Key.Message != b.Key.Message {
		return a.Key.Message < b.Key.Message
	}
	return !a.Key.FromMe && b.Key.FromMe
}

func (r *Runtime) commitDeliveredFrontiers(ctx context.Context, chunk *deliveredHistoryChunk) error {
	if chunk == nil {
		return nil
	}
	store, ok := r.store.(deliveredHistoryStore)
	if !ok {
		return archive.ErrInvalidEvent
	}
	chats := make([]string, 0, len(chunk.frontiers))
	for chat := range chunk.frontiers {
		chats = append(chats, chat)
	}
	slices.Sort(chats)
	for _, chat := range chats {
		if err := store.ObserveDeliveredHistoryAnchor(ctx, chunk.admission, chunk.frontiers[chat]); err != nil {
			if err == archive.ErrPolicyChanged || err == archive.ErrNoWork || err == archive.ErrSourceUnavailable {
				continue // stale or blocked hints never gain current traversal authority
			}
			return err
		}
	}
	r.WakeRebuild()
	return nil
}

func (r *Runtime) optionalDeliveredHistoryStep(ctx context.Context, progress *archive.RebuildProgress) bool {
	store, ok := r.store.(deliveredHistoryStore)
	selection := r.config.rebuildHistorySelection
	if !ok || selection == nil || progress.DiscoverySpent >= progress.DiscoveryLimit || !r.optionalReady() {
		return false
	}
	page, err := store.NextDeliveredHistoryPage(ctx)
	if err != nil {
		r.optionalOutcome(mediaFailure(err, ErrMediaUnavailable))
		return false
	}
	if page == nil || page.StrategyEpoch != selection.StrategyEpoch || page.Generation != progress.Generation || page.OperationID != progress.OperationID || page.OperationRevision != progress.Revision || page.PolicyGeneration != progress.PolicyGeneration {
		return false
	}
	chat, sender, err := r.rebuildTransport(ctx, page.Anchor, page.PolicyGeneration)
	if err != nil {
		err = r.deferRebuildTransport(ctx, page.Anchor.Key, err)
		r.optionalOutcome(mediaFailure(err, ErrMediaUnavailable))
		return false
	}
	info := types.MessageInfo{ID: page.Anchor.Key.Message, Timestamp: page.Anchor.OriginalSentAt, MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: page.Anchor.Key.FromMe}}
	request := r.client.BuildHistorySyncRequest(&info, deliveredHistoryPageCount)
	if !r.optionalReady() {
		return false
	}
	if err = store.RegisterDeliveredHistoryPage(ctx, *page); err != nil {
		r.optionalOutcome(mediaFailure(err, ErrMediaUnavailable))
		return false
	}
	// This durable ID accounts only for a page attempt, never per-key proof.
	if err = r.sendRebuildPeer(ctx, page.ID, request); err != nil {
		r.optionalOutcome(mediaFailure(err, ErrMediaUnavailable))
	}
	return true
}
