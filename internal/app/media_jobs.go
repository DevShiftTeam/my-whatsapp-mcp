package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/media"
)

type originalOwner interface{ Close() error }
type mediaJobs struct {
	store   *archive.Store
	version archive.IndexVersion
	mu      sync.Mutex
	pending []originalOwner
	code    string
	open    func(context.Context, archive.ReadScope, archive.MediaID) (originalOwner, error)
	extract func(context.Context, archive.Job, originalOwner) ([]archive.Document, error)
	cancel  context.CancelFunc
	done    chan struct{}
}

// extractJob returns cleanup-incomplete with the original still retained. The
// caller stops this worker; resource shutdown waits for Extractor.Close before
// trying any retained original again.
func (j *mediaJobs) extractJob(ctx context.Context, job archive.Job) error {
	if len(job.Sources) != 1 {
		return media.ErrInvalidRequest
	}
	source := job.Sources[0]
	scope := archive.ReadScope{Account: source.Account, Principal: "media-indexer", PolicyGeneration: source.Version.PolicyGeneration, AllChats: true}
	original, err := j.open(ctx, scope, job.MediaID)
	if err != nil {
		return err
	}
	documents, extractErr := j.extract(ctx, job, original)
	if errors.Is(extractErr, media.ErrCleanup) {
		j.mu.Lock()
		j.pending = append(j.pending, original)
		j.mu.Unlock()
		return media.ErrCleanup
	}
	if err = original.Close(); err != nil {
		j.mu.Lock()
		j.pending = append(j.pending, original)
		j.mu.Unlock()
		return media.ErrCleanup
	}
	if extractErr != nil {
		return extractErr
	}
	_, err = j.store.CommitJob(ctx, archive.JobResult{Lease: job.Lease, Generation: job.Generation, Version: job.Version, Sources: job.Sources, ContentHash: job.ContentHash, Documents: documents})
	return err
}
func (j *mediaJobs) closeOriginals() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	for len(j.pending) > 0 {
		if err := j.pending[0].Close(); err != nil {
			return err
		}
		j.pending = j.pending[1:]
	}
	return nil
}
func (j *mediaJobs) run(ctx context.Context, r *resources) {
	defer close(j.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	kinds := []archive.JobKind{archive.JobOCR, archive.JobCaption, archive.JobTranscription}
	for {
		if ctx.Err() != nil {
			return
		}
		account, err := j.store.BoundAccount(ctx)
		if err == nil && account != "" && !r.compute.Snapshot().Paused {
			for _, kind := range kinds {
				job, e := j.store.ClaimJob(ctx, archive.JobClaim{Account: account, Kind: kind, TTL: 2 * time.Minute})
				if e != nil {
					continue
				}
				if job.Version != j.version {
					j.retry(job, "invalid_input", true)
					continue
				}
				input, e := j.store.LoadJobInput(ctx, job.Lease)
				if e != nil {
					j.retry(job, "source_unavailable", false)
					continue
				}
				job = input.Job
				work, cancel := context.WithCancel(ctx)
				renewed := make(chan struct{})
				go func() {
					defer close(renewed)
					tick := time.NewTicker(30 * time.Second)
					defer tick.Stop()
					for {
						select {
						case <-work.Done():
							return
						case <-tick.C:
							if _, e := j.store.RenewJob(work, job.Lease, 2*time.Minute); e != nil {
								cancel()
								return
							}
						}
					}
				}()
				e = j.extractJob(work, job)
				j.mu.Lock()
				j.code = media.ExtractionCode(e)
				j.mu.Unlock()
				cancel()
				<-renewed
				if errors.Is(e, media.ErrCleanup) {
					return
				}
				if e != nil {
					code := "extraction_failed"
					terminal := false
					switch {
					case errors.Is(e, media.ErrModelMissing):
						code = "model_unavailable"
					case media.ExtractionCode(e) == "waiting_for_disk_budget":
						code = "waiting_for_disk_budget"
					case errors.Is(e, media.ErrUnsupported), errors.Is(e, media.ErrCorrupt), errors.Is(e, media.ErrSilent):
						code = "unsupported"
						terminal = true
					case ctx.Err() != nil:
						code = "cancelled"
					}
					j.retry(job, code, terminal)
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
func (j *mediaJobs) retry(job archive.Job, code string, terminal bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = j.store.RetryJob(ctx, archive.JobRetry{Lease: job.Lease, Code: code, After: 5 * time.Second, Terminal: terminal})
}
func (r *resources) startMediaJobs(store *archive.Store, extractor *media.Extractor, version archive.IndexVersion) {
	ctx, cancel := context.WithCancel(context.Background())
	jobs := &mediaJobs{store: store, version: version, cancel: cancel, done: make(chan struct{})}
	jobs.open = func(ctx context.Context, scope archive.ReadScope, id archive.MediaID) (originalOwner, error) {
		return r.media.OpenOriginal(ctx, scope, id)
	}
	jobs.extract = func(ctx context.Context, job archive.Job, owner originalOwner) ([]archive.Document, error) {
		original, ok := owner.(*media.Original)
		if !ok {
			return nil, media.ErrInvalidRequest
		}
		return extractor.Extract(ctx, job, original)
	}
	r.mediaJobs = jobs
	go jobs.run(ctx, r)
}
