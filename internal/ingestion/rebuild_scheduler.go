package ingestion

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"time"
)

// WakeRebuild only wakes the existing bounded optional worker. The operation
// ledger, current policy and shared durable cadence authorize every request.
func (r *Runtime) WakeRebuild() {
	if r.config.Rebuild != nil {
		r.WakeOriginalDownloads()
	}
}

// DrainRebuild follows a durable owner pause/cancel. The active marker precedes
// request registration, so pre-pause sends are canceled and joined while any
// later registration is rejected by the writer. Live ingestion remains active.
func (r *Runtime) DrainRebuild(ctx context.Context) error {
	if ctx == nil {
		return archive.ErrInvalidEvent
	}
	r.rebuildActiveMu.Lock()
	cancel, done := r.rebuildActiveCancel, r.rebuildActiveDone
	r.rebuildActiveMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (r *Runtime) beginRebuildAttempt(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	r.rebuildActiveMu.Lock()
	r.rebuildActiveCancel = cancel
	r.rebuildActiveDone = done
	r.rebuildActiveMu.Unlock()
	return ctx, func() {
		cancel()
		r.rebuildActiveMu.Lock()
		r.rebuildActiveCancel = nil
		r.rebuildActiveDone = nil
		close(done)
		r.rebuildActiveMu.Unlock()
	}
}

// Each turn dispatches at most one request from a bounded metadata window.
// Persisted spend alternates copy/discovery preference across runtime restarts;
// the caller continues ordinary attachment work after this bounded step.
func (r *Runtime) optionalRebuildStep(n *optionalNetwork) bool {
	c := r.config.Rebuild
	if c == nil || c.Requests == 0 || !r.optionalReady() {
		return false
	}
	cadence := optionalCooldown
	if r.config.RebuildHistoryPolicy == archive.RebuildHistoryDelivered {
		cadence = archive.DeliveredHistoryRequestInterval
	}
	r.optionalMu.Lock()
	limited := !r.lastRebuildRequest.IsZero() && time.Since(r.lastRebuildRequest) < cadence
	if !limited {
		r.lastRebuildRequest = time.Now()
	}
	r.optionalMu.Unlock()
	if limited {
		return false
	}
	store, ok := r.store.(rebuildStore)
	if !ok {
		r.optionalOutcome(ErrMediaUnavailable)
		return false
	}
	attempt, finish := n.attempt(2 * time.Second)
	defer finish()
	ctx, joined := r.beginRebuildAttempt(attempt)
	defer joined()
	p, err := store.RebuildDispatchState(ctx)
	if err != nil {
		r.optionalOutcome(mediaFailure(err, ErrMediaUnavailable))
		return false
	}
	if p == nil || p.Generation != c.ObservationID || p.State != "running" || !p.LastAttemptAt.IsZero() && time.Since(p.LastAttemptAt) < cadence {
		return false
	}
	if r.config.RebuildHistoryPolicy == archive.RebuildHistoryDelivered {
		return r.optionalDeliveredHistoryStep(ctx, p)
	}
	discoveryFirst := (p.CopySpent+p.DiscoverySpent)%2 == 1
	for pass := 0; pass < 2; pass++ {
		if ctx.Err() != nil || !r.optionalReady() {
			return false
		}
		discovery := (pass == 0) == discoveryFirst
		if discovery {
			if p.DiscoverySpent >= p.DiscoveryLimit {
				continue
			}
			q, queryErr := store.NextRebuildDiscovery(ctx)
			if queryErr != nil {
				r.optionalOutcome(mediaFailure(queryErr, ErrMediaUnavailable))
				continue
			}
			if q == nil {
				continue
			}
			if q.OperationID != p.OperationID || q.Generation != p.Generation || q.OperationRevision != p.Revision || q.PolicyGeneration != p.PolicyGeneration {
				continue
			}
			dispatched, sendErr := r.dispatchRebuildDiscovery(ctx, *q)
			if sendErr != nil {
				r.optionalOutcome(mediaFailure(sendErr, ErrMediaUnavailable))
			}
			if dispatched {
				return true
			}
		} else {
			if p.CopySpent >= p.CopyLimit {
				continue
			}
			anchors, queryErr := store.NextRebuildCandidates(ctx, c.Requests)
			if queryErr != nil {
				r.optionalOutcome(mediaFailure(queryErr, ErrMediaUnavailable))
				continue
			}
			if len(anchors) > c.Requests || len(anchors) > 32 {
				r.optionalOutcome(ErrMediaLimit)
				return false
			}
			for _, anchor := range anchors {
				if ctx.Err() != nil || !r.optionalReady() {
					return false
				}
				dispatched, sendErr := r.dispatchRebuildCopy(ctx, anchor, p)
				if sendErr != nil {
					r.optionalOutcome(mediaFailure(sendErr, ErrMediaUnavailable))
				}
				if dispatched {
					return true
				}
			}
		}
	}
	return false
}
