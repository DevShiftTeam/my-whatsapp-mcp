package app

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/media"
	"sync"
	"time"
)

type primaryOriginal interface {
	Close() error
	Metadata() media.OriginalMetadata
}
type primaryCleanup struct {
	work     archive.ExtractionWork
	original primaryOriginal
	cancel   context.CancelFunc
	renewed  <-chan struct{}
}
type primaryMediaJobs struct {
	store    archive.ExtractionStore
	open     func(context.Context, archive.ReadScope, archive.MediaID) (primaryOriginal, error)
	produce  func(context.Context, archive.ExtractionWork, primaryOriginal) (archive.ExtractionResult, error)
	versions []archive.ExtractionVersion
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
	pending  []primaryCleanup
	code     string
}

func (r *resources) startPrimaryMedia(store *archive.Store, extractor *media.Extractor, versions []archive.ExtractionVersion) {
	ctx, cancel := context.WithCancel(context.Background())
	jobs := &primaryMediaJobs{store: store, versions: append([]archive.ExtractionVersion(nil), versions...), cancel: cancel, done: make(chan struct{})}
	jobs.open = func(ctx context.Context, scope archive.ReadScope, id archive.MediaID) (primaryOriginal, error) {
		return r.media.OpenOriginal(ctx, scope, id)
	}
	jobs.produce = func(ctx context.Context, work archive.ExtractionWork, owner primaryOriginal) (archive.ExtractionResult, error) {
		original, ok := owner.(*media.Original)
		if !ok {
			return archive.ExtractionResult{}, media.ErrInvalidRequest
		}
		return extractor.ExtractPrimary(ctx, work, original)
	}
	r.primaryJobs = jobs
	go jobs.run(ctx, store, r)
}
func (j *primaryMediaJobs) run(ctx context.Context, store *archive.Store, r *resources) {
	defer close(j.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		account, err := store.BoundAccount(ctx)
		if err == nil && account != "" && !r.compute.Snapshot().Paused {
			for _, version := range j.versions {
				work, err := j.store.ClaimExtraction(ctx, archive.ExtractionClaim{Account: account, Version: version, TTL: 2 * time.Minute})
				if err != nil {
					continue
				}
				input, err := j.store.LoadExtractionInput(ctx, work.Lease)
				if err != nil {
					j.retry(work, "source_unavailable", false)
					continue
				}
				if input.Version != version || input.Lease != work.Lease {
					j.retry(work, "invalid_input", true)
					continue
				}
				err = j.extract(ctx, input)
				j.mu.Lock()
				j.code = media.ExtractionCode(err)
				j.mu.Unlock()
				if errors.Is(err, media.ErrCleanup) {
					return
				}
				if err != nil {
					code := "extraction_failed"
					terminal := false
					switch {
					case errors.Is(err, media.ErrModelMissing):
						code = "model_unavailable"
					case media.ExtractionCode(err) == "waiting_for_disk_budget":
						code = "waiting_for_disk_budget"
					case errors.Is(err, media.ErrInvalidRequest), errors.Is(err, media.ErrCorrupt), errors.Is(err, media.ErrUnsupported):
						code = "invalid_input"
						terminal = true
					case ctx.Err() != nil:
						code = "cancelled"
					}
					j.retry(work, code, terminal)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (j *primaryMediaJobs) extract(ctx context.Context, work archive.ExtractionWork) error {
	scope := archive.ReadScope{Account: work.Source.Account, Principal: "media-indexer", PolicyGeneration: work.Source.Version.PolicyGeneration, AllChats: true}
	original, err := j.open(ctx, scope, work.MediaID)
	if err != nil {
		return err
	}
	// Renewal is independent of request cancellation until the full native and
	// original cleanup boundary is acknowledged. TTL never releases physical input.
	renewal, stopRenewal := context.WithCancel(context.Background())
	workCtx, cancelWork := context.WithCancel(ctx)
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-renewal.Done():
				return
			case <-ticker.C:
				bounded, cancel := context.WithTimeout(renewal, 2*time.Second)
				_, e := j.store.RenewExtraction(bounded, work.Lease, 2*time.Minute)
				cancel()
				if e != nil {
					cancelWork()
					return
				}
			}
		}
	}()
	owned := primaryCleanup{work: work, original: original, cancel: stopRenewal, renewed: renewed}
	metadata := original.Metadata()
	if metadata.ArtifactID != work.ArtifactID || metadata.SHA256 != work.OriginalSHA256 || metadata.Source != work.Source || metadata.MediaID != work.MediaID {
		cancelWork()
		if err = original.Close(); err != nil {
			j.retain(owned)
			return media.ErrCleanup
		}
		stopRenewal()
		<-renewed
		return media.ErrInvalidRequest
	}
	result, extractErr := j.produce(workCtx, work, original)
	cancelWork()
	if errors.Is(extractErr, media.ErrCleanup) {
		j.retain(owned)
		return media.ErrCleanup
	}
	if err = original.Close(); err != nil {
		j.retain(owned)
		return media.ErrCleanup
	}
	stopRenewal()
	<-renewed
	if extractErr != nil {
		return extractErr
	}
	if result.Lease != work.Lease || result.ContentHash != work.ContentHash {
		return media.ErrInvalidRequest
	}
	committed, err := j.store.CommitExtraction(ctx, result)
	if err != nil {
		return err
	}
	if !committed {
		return media.ErrInvalidRequest
	}
	return nil
}
func (j *primaryMediaJobs) retain(owned primaryCleanup) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.pending = append(j.pending, owned)
}

// Caller invokes this only after Extractor.Close acknowledged all native owners.
func (j *primaryMediaJobs) finishCleanup() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	for len(j.pending) > 0 {
		owned := j.pending[0]
		if err := owned.original.Close(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := j.store.RetryExtraction(ctx, archive.JobRetry{Lease: owned.work.Lease, Code: "cancelled", After: 5 * time.Second})
		cancel()
		if err != nil {
			return err
		}
		if owned.cancel != nil {
			owned.cancel()
		}
		if owned.renewed != nil {
			<-owned.renewed
		}
		j.pending = j.pending[1:]
	}
	return nil
}
func (j *primaryMediaJobs) retry(work archive.ExtractionWork, code string, terminal bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = j.store.RetryExtraction(ctx, archive.JobRetry{Lease: work.Lease, Code: code, After: 5 * time.Second, Terminal: terminal})
}
