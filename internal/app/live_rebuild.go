package app

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"time"
)

type rebuildWaker interface{ WakeRebuild() }
type rebuildDrainer interface{ DrainRebuild(context.Context) error }

// RebuildProgress belongs to authenticated owner composition, not scoped MCP.
func (a *Application) RebuildProgress(ctx context.Context) (*archive.RebuildProgress, error) {
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return nil, archive.ErrArchiveUnavailable
	}
	if a.rebuild == nil || a.snapshot != nil {
		return nil, nil
	}
	return a.store.RebuildProgress(ctx)
}

func (a *Application) ControlRebuild(ctx context.Context, input archive.RebuildControl) (*archive.RebuildProgress, error) {
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed || a.rebuild == nil || a.snapshot != nil {
		return nil, archive.ErrSourceUnavailable
	}
	value, err := a.store.ControlRebuild(ctx, input)
	if err != nil {
		return nil, err
	}
	// Replayed command IDs return current state. An old pause replay must not
	// cancel a newer resumed attempt, and old resume must not wake a later pause.
	if value != nil && (value.State == "paused" || value.State == "cancelled") {
		if drain, ok := a.runtime.(rebuildDrainer); ok {
			if err := drain.DrainRebuild(ctx); err != nil {
				return nil, err
			}
		} else if a.runtime != nil && !a.offline {
			return nil, archive.ErrSourceUnavailable
		}
	} else if input.Action == "resume" || input.Action == "extend" {
		a.wakeRebuild()
	}
	return value, nil
}

// The caller owns app.data. Ledger state remains the dispatch authority; this
// merely prompts the one existing optional network worker to inspect it.
func (a *Application) wakeRebuild() {
	if a.rebuild == nil || a.paused || a.resources == nil || a.resources.recoveryPending.Load() {
		return
	}
	if wake, ok := a.runtime.(rebuildWaker); ok {
		wake.WakeRebuild()
	}
}

func liveRebuildRuntimeConfig(paths appdata.Paths, provenance *archive.RebuildProvenance, requests int) (*ingestion.RebuildConfig, error) {
	if provenance == nil {
		return nil, nil
	}
	if provenance.Generation == "" || provenance.Account == "" || provenance.Boundary.IsZero() || provenance.Boundary.After(time.Now()) || paths.WorkingDir == "" || requests < 1 || requests > productBackfillLimit {
		return nil, archive.ErrReconciliationRequired
	}
	return &ingestion.RebuildConfig{ObservationID: provenance.Generation, Account: provenance.Account, Boundary: provenance.Boundary, Requests: requests}, nil
}

// The original device owner stays excluded until every destination resource is
// closed. A retry retains either lease whose close acknowledgement failed.
func (a *Application) closeInstanceOwnership() error {
	if err := a.lease.Close(); err != nil {
		return err
	}
	if err := a.rebuildSourceLease.Close(); err != nil {
		return err
	}
	a.cleanupComplete = true
	return nil
}

func (a *Application) liveRebuildMetadata() *archive.RebuildStatus {
	if a.rebuild == nil {
		return nil
	}
	return &archive.RebuildStatus{Mode: "live_rebuild", Coverage: "partial", CurrentPhoneState: "unknown"}
}
