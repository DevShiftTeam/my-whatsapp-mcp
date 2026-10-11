package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/media"
	"github.com/Bodokh/my-whatsapp-mcp/internal/transcription"
)

var errTranscriptionConfig = errors.New("transcription_config_invalid")

const voiceRetryInterval = 5 * time.Second

// Voice processing is configured at launch, independently of native previews.
// An unavailable service never prevents the archive from opening or being read.
type voiceOwner struct {
	mu      sync.Mutex
	backend *transcription.Backend
	jobs    *primaryMediaJobs
	code    string
	cancel  context.CancelFunc
	done    chan struct{}
}

func validateTranscriptionConfig(cfg Config) error {
	if cfg.Transcription == nil {
		return nil
	}
	if transcription.ValidateOptions(*cfg.Transcription) != nil {
		return errTranscriptionConfig
	}
	if cfg.Media != nil && cfg.Media.Experimental {
		for _, version := range cfg.Media.ExtractionVersions {
			if version.Kind == archive.JobTranscription {
				return errTranscriptionConfig
			}
		}
		if cfg.Media.Runtimes.Speech.Path != "" {
			return errTranscriptionConfig
		}
	}
	return nil
}

func (r *resources) startVoice(store *archive.Store, opts transcription.Options) {
	ctx, cancel := context.WithCancel(context.Background())
	owner := &voiceOwner{cancel: cancel, done: make(chan struct{}), code: "transcription_connecting"}
	r.voice = owner
	r.voiceStatus.Store(owner)
	go owner.run(ctx, store, r, opts)
}

func (o *voiceOwner) run(ctx context.Context, store *archive.Store, r *resources, opts transcription.Options) {
	defer close(o.done)
	ticker := time.NewTicker(voiceRetryInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		backend, err := transcription.New(probe, opts)
		cancel()
		if err == nil {
			jobs, workCtx := r.newPrimaryMediaJobs(store, []archive.ExtractionVersion{backend.Version()})
			// Reuse the complete durable lease/original/commit boundary. The
			// service receives verified originals, never paths or download URLs.
			var nextProbe time.Time
			var probeErr error
			jobs.ready = func(ctx context.Context) error {
				if time.Now().Before(nextProbe) {
					return probeErr
				}
				bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
				probeErr = backend.Ready(bounded)
				cancel()
				nextProbe = time.Now().Add(voiceRetryInterval)
				return probeErr
			}
			jobs.produce = func(ctx context.Context, work archive.ExtractionWork, input primaryOriginal) (archive.ExtractionResult, error) {
				original, ok := input.(*media.Original)
				if !ok {
					return archive.ExtractionResult{}, media.ErrInvalidRequest
				}
				result, err := backend.ExtractPrimary(ctx, work, original)
				if err != nil {
					// The next iteration probes availability before claiming a
					// job, so outages do not exhaust durable retry attempts.
					nextProbe = time.Time{}
				}
				return result, err
			}
			o.mu.Lock()
			o.backend, o.jobs, o.code = backend, jobs, "transcription_ready"
			o.mu.Unlock()
			stop := context.AfterFunc(ctx, jobs.cancel)
			jobs.run(workCtx, store, r)
			stop()
			jobs.cancel()
			return
		}
		o.mu.Lock()
		o.code = primaryExtractionCode(err)
		if o.code == "" {
			o.code = "transcription_unavailable"
		}
		o.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (o *voiceOwner) close(ctx context.Context) error {
	o.cancel()
	select {
	case <-o.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.backend != nil {
		if err := o.backend.Close(ctx); err != nil {
			return err
		}
	}
	if o.jobs != nil {
		return o.jobs.finishCleanup()
	}
	return nil
}

func (o *voiceOwner) progress(status *archive.Progress) {
	o.mu.Lock()
	code, jobs := o.code, o.jobs
	o.mu.Unlock()
	if jobs != nil {
		jobs.mu.Lock()
		if jobs.code != "" && jobs.code != "ready" && jobs.code != "no_text" && jobs.code != "silent" {
			code = jobs.code
		}
		jobs.mu.Unlock()
	}
	if code == "transcription_ready" && status.Code != "" && status.Code != "media_pending" && status.Code != "transcription_ready" {
		return
	}
	status.Code = code
	if code != "transcription_ready" {
		status.State = "unavailable"
	} else if status.State == "" || status.State == "not_configured" || status.State == "unavailable" {
		status.State = "pending"
		if status.Pending == 0 && status.Complete > 0 {
			status.State = "ready"
		}
	}
}

type voiceArchiveReader struct {
	*archive.Store
	resources *resources
}

func (r *resources) archiveReader(store *archive.Store) archive.Reader {
	return &voiceArchiveReader{Store: store, resources: r}
}

func (r *voiceArchiveReader) Status(ctx context.Context, scope archive.ReadScope) (archive.StatusSnapshot, error) {
	status, _, err := r.StatusGuarded(ctx, scope)
	return status, err
}

func (r *voiceArchiveReader) StatusGuarded(ctx context.Context, scope archive.ReadScope) (archive.StatusSnapshot, archive.ResponseGuard, error) {
	owner := r.resources.voiceStatus.Load()
	if owner == nil {
		return r.Store.StatusGuarded(ctx, scope)
	}
	owner.mu.Lock()
	backend := owner.backend
	owner.mu.Unlock()
	var status archive.StatusSnapshot
	var guard archive.ResponseGuard
	var err error
	if backend == nil {
		status, guard, err = r.Store.StatusGuarded(ctx, scope)
	} else {
		// Status, voice counts, expiry and rolling horizons come from one
		// archive read frame. Retrieval keeps its normal output and final
		// response checks, including credential revocation and source expiry.
		status, guard, err = r.Store.StatusWithExtractionGuarded(ctx, scope, backend.Version())
	}
	if err != nil {
		return archive.StatusSnapshot{}, archive.ResponseGuard{}, err
	}
	owner.progress(&status.Media)
	return status, guard, nil
}
