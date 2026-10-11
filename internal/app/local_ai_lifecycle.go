package app

import (
	"context"
	"errors"

	"github.com/Bodokh/wa-indexer/internal/inference"
	"github.com/Bodokh/wa-indexer/internal/workerlife"
)

// AI-only drain leaves the archive, originals, model operations, MCP and shared
// admission controllers alive. The caller holds lifecycle serialization, but
// never the app data/network/archive commit lock while waiting for cleanup.
func (r *resources) drainAI(ctx context.Context) error {
	if r.runCancel != nil {
		r.runCancel()
	}
	if r.mediaJobs != nil {
		r.mediaJobs.cancel()
	}
	if r.primaryJobs != nil {
		r.primaryJobs.cancel()
	}
	for _, done := range []<-chan struct{}{r.runDone, mediaJobDone(r), primaryJobDone(r)} {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.extraction != nil {
		if err := r.extraction.Close(ctx); err != nil {
			return err
		}
	}
	if r.primaryJobs != nil {
		if err := r.primaryJobs.finishCleanup(); err != nil {
			return err
		}
	}
	if r.mediaJobs != nil {
		if err := r.mediaJobs.closeOriginals(); err != nil {
			return err
		}
	}
	if r.native != nil {
		closeErr := r.native.Close()
		if err := r.native.AwaitCleanup(ctx); err != nil {
			return err
		}
		if closeErr != nil && !errors.Is(closeErr, inference.ErrCleanup) {
			return closeErr
		}
	}
	if r.workerExecutable != nil {
		if err := r.workerExecutable.Close(); err != nil {
			return err
		}
	}
	return nil
}
func mediaJobDone(r *resources) <-chan struct{} {
	if r.mediaJobs == nil {
		return nil
	}
	return r.mediaJobs.done
}
func primaryJobDone(r *resources) <-chan struct{} {
	if r.primaryJobs == nil {
		return nil
	}
	return r.primaryJobs.done
}

// Only after acknowledged drain, under the app data lock, may pointers disappear.
func (r *resources) finishAIDrain() {
	r.runCancel = nil
	r.runDone = nil
	r.mediaJobs = nil
	r.primaryJobs = nil
	r.extraction = nil
	r.mediaExtractor = nil
	r.native = nil
	r.workerExecutable = nil
	r.workerExecutableReady = false
	r.index.mu.Lock()
	r.index.service = nil
	r.index.mu.Unlock()
}

func (r *resources) prepareBundledWorker(ctx context.Context, cfg *NativeConfig) error {
	if cfg == nil || cfg.bundle == nil {
		return nil
	}
	if r.workerExecutable != nil && r.workerExecutableReady {
		cfg.WorkerExecutable = r.workerExecutable.Path()
		return nil
	}
	if r.workerExecutable != nil {
		if err := r.workerExecutable.Close(); err != nil {
			return err
		}
		r.workerExecutable = nil
	}
	if err := r.initializeExecutableNamespace(ctx, r.paths); err != nil {
		return err
	}
	mapping, exists := cfg.bundle.entries[aiMappingKey("encoder", cfg.Profile.ID, "")]
	if !exists {
		return errBundledAI
	}
	file, err := cfg.bundle.assets.duplicate(mapping.WorkerPath)
	if err != nil {
		return err
	}
	lease, prepareErr := r.namespace.Prepare(ctx, file, 64<<20)
	closeErr := file.Close()
	if lease != nil {
		r.workerExecutable = lease
	}
	if prepareErr != nil || closeErr != nil || lease == nil {
		return errBundledAI
	}
	r.workerExecutableReady = true
	cfg.WorkerExecutable = lease.Path()
	return nil
}

func (b *bundledAI) workerLifetime() workerlife.Config {
	return workerlife.Config{ExecutableFile: b.assets.files["Contents/MacOS/wa-indexer"], Args: []string{workerlife.HolderModeArgument}}
}
