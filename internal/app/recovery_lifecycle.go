package app

import (
	"context"
	"github.com/Bodokh/wa-indexer/internal/media"
	"sync"
	"time"
)

type recoveryLifecycle struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func (a *Application) startRecoveryAndBackground(cfg Config) {
	if !a.resources.recoveryPending.Load() {
		a.resources.startBackground(a.store, cfg)
	}
	mcpAttempted := a.mcpStartupComplete()
	if !a.resources.recoveryPending.Load() && mcpAttempted {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.recoveryOwner.mu.Lock()
	a.recoveryOwner.cancel = cancel
	a.recoveryOwner.done = make(chan struct{})
	done := a.recoveryOwner.done
	a.recoveryOwner.mu.Unlock()
	go func() {
		defer close(done)
		defer cancel()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			a.ops.Lock()
			a.data.Lock()
			if a.closed || ctx.Err() != nil {
				a.data.Unlock()
				a.ops.Unlock()
				return
			}
			if a.resources.recoveryPending.Load() {
				_, _ = a.resumeRecovery(ctx, cfg)
			}
			if !mcpAttempted {
				account, err := a.store.BoundAccount(ctx)
				if err == nil && account != "" {
					_ = a.startMCP(ctx)
					mcpAttempted = a.mcpStartupComplete()
					a.resources.startBackground(a.store, cfg)
				}
			}
			complete := !a.resources.recoveryPending.Load() && mcpAttempted
			a.data.Unlock()
			a.ops.Unlock()
			if complete {
				return
			}
		}
	}()
}

// Configured transports retry independently. A local success does not hide a
// remote bind/recovery failure, and all retries belong to the app's joined owner.
func (a *Application) mcpStartupComplete() bool {
	if !a.mcpEnabled {
		return true
	}
	if a.mcp == nil {
		return false
	}
	local, remote := a.mcp.status()
	return local == "ready" && (a.mcpConfig.Remote == nil || remote == "ready")
}
func (a *Application) resumeRecovery(ctx context.Context, cfg Config) (bool, error) {
	r := a.resources
	if r == nil || r.recovery == nil {
		return false, errExtractionRecovery
	}
	complete, err := r.recovery.step(ctx, a.store)
	if err != nil || !complete {
		return false, err
	}
	report, err := r.media.Reconcile(ctx, 256)
	if err != nil {
		return false, err
	}
	if report.Active != 0 {
		return false, media.ErrActiveUse
	}
	if report.Cleaned != 0 {
		return false, nil
	}
	// Initialization and reader replacement happen under the same lifecycle/data
	// boundary. Existing MCP readers use the stable borrowed router.
	if err = r.initializeRuntime(ctx, a.store, cfg); err != nil {
		return false, err
	}
	r.recoveryPending.Store(false)
	r.startBackground(a.store, cfg)
	a.wakeOriginalDownloads()
	return true, nil
}
func (a *Application) stopRecovery(ctx context.Context) error {
	a.recoveryOwner.mu.Lock()
	cancel, done := a.recoveryOwner.cancel, a.recoveryOwner.done
	if cancel != nil {
		cancel()
	}
	a.recoveryOwner.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
