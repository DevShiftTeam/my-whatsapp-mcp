package app

import (
	"context"

	"github.com/Bodokh/wa-indexer/internal/archive"

	"github.com/Bodokh/wa-indexer/internal/compute"
	"github.com/Bodokh/wa-indexer/internal/media"
	"github.com/Bodokh/wa-indexer/internal/workerlife"
)

// MediaConfig accepts only trusted audited bundle/runtime inputs. It is never
// decoded from HTTP or inferred from executables found on PATH.
type MediaConfig struct {
	bundle             *bundledAI
	OCRProfile         string
	Experimental       bool
	Runtimes           media.RuntimePaths
	Language           media.SpeechLanguage
	Residency          compute.ResidencyPolicy
	WorkerLifetime     workerlife.Config
	ExtractionVersions []archive.ExtractionVersion
}
type extractionOwner interface{ Close(context.Context) error }

func (r *resources) configureMedia(cfg *MediaConfig) error {
	if cfg == nil || !cfg.Experimental {
		return nil
	}
	if r.extraction != nil {
		return nil
	}
	if r.instanceGuard == nil {
		return media.ErrUnavailable
	}
	if err := cfg.Residency.Validate(); err != nil {
		return err
	}
	var before func(context.Context) error
	if cfg.Residency == compute.SerialModels && r.native != nil {
		before = r.native.Unload
	}
	extractor, err := media.NewExtractor(media.ExtractorOptions{
		Store: r.media, Scheduler: r.compute, Runtimes: cfg.Runtimes, Language: cfg.Language, OCRProfile: cfg.OCRProfile,
		BeforeHeavy:   before,
		InstanceGuard: r.instanceGuard, WorkerLifetime: cfg.WorkerLifetime,
		ExtractionVersions: append([]archive.ExtractionVersion(nil), cfg.ExtractionVersions...),
		Acquire:            r.acquireMediaModel,
	})
	if err != nil {
		return err
	}
	r.extraction = extractor
	r.mediaExtractor = extractor
	return nil
}

func (r *resources) acquireMediaModel(ctx context.Context, id string) (media.MediaModelLease, error) {
	lease, err := r.models.Acquire(ctx, id)
	if lease == nil {
		return nil, err
	}
	// A nonnil partial lease remains owned even when acquisition also fails.
	return lease, err
}
