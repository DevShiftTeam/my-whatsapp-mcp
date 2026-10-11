package app

import (
	"context"
	"github.com/Bodokh/wa-indexer/internal/diskspace"
	"github.com/Bodokh/wa-indexer/internal/modelstore"
)

// Only the acquired-instance activation boundary may create model control
// directories. Existing unfinished input ownership must be acknowledged first.
func (r *resources) initializeModels(ctx context.Context) error {
	if r.recovery == nil || !r.recovery.complete || r.instanceGuard == nil {
		return errExtractionRecovery
	}
	if r.models != nil {
		return r.ackModelBootstrap()
	}
	physical, err := r.disk.Snapshot()
	if err != nil || physical.Device == 0 {
		return errNativeNamespace
	}
	r.modelDevice = physical.Device
	catalog, err := modelstore.DefaultCatalog()
	if err != nil {
		return err
	}
	if r.modelBootstrap == nil {
		r.modelBootstrap, err = r.disk.Reserve(ctx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: 64 << 10})
		if err != nil {
			return err
		}
	}
	r.models, err = modelstore.Open(r.paths.ModelDir, catalog, modelstore.Options{ExpectedDevice: physical.Device, ReserveSpace: func(ctx context.Context, growth int64) (modelstore.SpaceReservation, error) {
		return r.Reserve(ctx, diskspace.Request{Class: diskspace.ModelInstall, MaximumGrowthBytes: growth})
	}})
	if err != nil {
		return err
	}
	r.modelMetadataReady = true
	// Open acknowledges its anchored metadata writes; subsequent payload writes
	// use the ordinary gate and retain their own physical growth reservation.
	return r.ackModelBootstrap()
}

func (r *resources) ackModelBootstrap() error {
	if r.modelBootstrap == nil {
		return nil
	}
	if !r.modelMetadataReady {
		// An uncertain metadata sync cannot lose its physical growth owner.
		// Retry only the same bounded control directory initialization, including
		// on shutdown when payload admission has already been stopped.
		catalog, err := modelstore.DefaultCatalog()
		if err != nil || r.modelDevice == 0 {
			return errExtractionRecovery
		}
		control, err := modelstore.Open(r.paths.ModelDir, catalog, modelstore.Options{ExpectedDevice: r.modelDevice, ReserveSpace: func(context.Context, int64) (modelstore.SpaceReservation, error) { return nil, errExtractionRecovery }})
		if err != nil {
			return err
		}
		r.models = control
		r.modelMetadataReady = true
		if err = control.Close(); err != nil {
			return err
		}
		r.models = nil
	}
	if err := r.modelBootstrap.Release(); err != nil {
		return err
	}
	r.modelBootstrap = nil
	r.modelMetadataReady = false
	return nil
}
