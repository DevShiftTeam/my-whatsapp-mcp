package app

import (
	"context"
	"errors"
	"os"
	"sync/atomic"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/compute"
	"github.com/Bodokh/my-whatsapp-mcp/internal/diskspace"
	"github.com/Bodokh/my-whatsapp-mcp/internal/inference"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"github.com/Bodokh/my-whatsapp-mcp/internal/media"
	"github.com/Bodokh/my-whatsapp-mcp/internal/modelstore"
	"github.com/Bodokh/my-whatsapp-mcp/internal/retrieval"
	"github.com/Bodokh/my-whatsapp-mcp/internal/workerlife"
)

// NativeConfig is explicit immutable native configuration. No candidate profile
// becomes a production default merely because its files are installed.
type NativeConfig struct {
	bundle           *bundledAI
	Profile          inference.Profile
	WorkerExecutable string
	ModelProfileID   string
	RuntimeLease     func(context.Context) (inference.ComponentLease, error)
	Experimental     bool
	IndexVersion     *archive.IndexVersion
	WorkerLifetime   workerlife.Config
}
type nativeOwner interface {
	inference.Encoder
	inference.PassageTokenizer
	Close() error
	AwaitCleanup(context.Context) error
	Unload(context.Context) error
}
type resources struct {
	snapshotTranscriptionReady bool

	instanceGuard         *appdata.InstanceLock
	paths                 appdata.Paths
	namespace             executableNamespace
	namespaceReady        bool
	workerExecutable      workerlife.ExecutableLease
	workerExecutableReady bool
	bundles               []*bundledAI
	namespaceBootstrap    diskspace.Lease
	namespaceMkdir        func(*os.Root) error
	modelBootstrap        diskspace.Lease
	modelMetadataReady    bool
	modelDevice           uint64
	modelOperations       modelOperations
	compute               *compute.ThreadBudgetScheduler
	disk                  *diskspace.Controller
	models                *modelstore.Store
	media                 *media.Store
	demoAcceptance        *demoAcceptanceOwner
	demoAcceptanceEnabled bool
	native                nativeOwner
	retrieval             *retrievalRouter
	recovery              *extractionRecovery
	recoveryPending       atomic.Bool
	index                 indexLifetime
	runCancel             context.CancelFunc
	runDone               chan struct{}
	extraction            extractionOwner
	mediaExtractor        *media.Extractor
	mediaJobs             *mediaJobs
	primaryJobs           *primaryMediaJobs
	voice                 *voiceOwner
	voiceStatus           atomic.Pointer[voiceOwner]
	nativeConfigured      bool
	mediaConfigured       bool
}

func openResources(ctx context.Context, paths appdata.Paths, store *archive.Store, cfg Config, paused bool) (*resources, error) {
	r := &resources{paths: paths, instanceGuard: cfg.instanceGuard, nativeConfigured: cfg.Native != nil, mediaConfigured: cfg.Media != nil, demoAcceptanceEnabled: cfg.DemoAcceptanceFixtures}
	if cfg.Native != nil && cfg.Native.bundle != nil {
		r.bundles = append(r.bundles, cfg.Native.bundle)
	}
	if cfg.Media != nil && cfg.Media.bundle != nil && (len(r.bundles) == 0 || r.bundles[0] != cfg.Media.bundle) {
		r.bundles = append(r.bundles, cfg.Media.bundle)
	}
	var err error
	r.compute, err = compute.NewThreadBudgetScheduler(2)
	if err != nil {
		return r, err
	}
	r.compute.SetPaused(paused)
	r.disk, err = diskspace.New(paths.Root, diskspace.Options{})
	if err != nil {
		r.compute.Close()
		return r, err
	}
	fail := func(e error) (*resources, error) { return r, e }
	physical, err := r.disk.Snapshot()
	if err != nil || physical.Device == 0 {
		return fail(errNativeNamespace)
	}
	r.media, err = media.Open(paths, store, media.Options{Admission: r, ExpectedDevice: physical.Device})
	if err != nil {
		return fail(err)
	}
	if cfg.instanceGuard == nil {
		return fail(appdata.ErrLockRequired)
	}
	account, err := store.BoundAccount(ctx)
	if err != nil {
		return fail(err)
	}
	r.recovery, err = recoverExtractionPins(ctx, store, account)
	r.recoveryPending.Store(err != nil || !r.recovery.complete)
	service, err := retrieval.NewService(retrieval.Config{Reader: r.archiveReader(store), Candidates: store, Index: store.ActiveIndex})
	if err != nil {
		return fail(err)
	}
	r.retrieval = &retrievalRouter{}
	r.retrieval.set(service)
	if r.recoveryPending.Load() {
		return r, nil
	}
	for {
		report, e := r.media.Reconcile(ctx, 256)
		if e != nil {
			return fail(e)
		}
		if report.Active != 0 {
			return fail(media.ErrActiveUse)
		}
		if report.Cleaned == 0 {
			break
		}
	}
	if err = r.initializeRuntime(ctx, store, cfg); err != nil {
		return fail(err)
	}
	return r, nil
}

func (r *resources) initializeRuntime(ctx context.Context, store *archive.Store, cfg Config) error {
	var err error
	if err = r.initializeModels(ctx); err != nil {
		return err
	}
	if cfg.Native != nil && cfg.Native.Experimental || cfg.Media != nil && cfg.Media.Experimental {
		if err := r.initializeExecutableNamespace(ctx, r.paths); err != nil {
			return err
		}
		if cfg.Native != nil {
			value := *cfg.Native
			value.WorkerLifetime = r.configureWorkerLifetime(value.WorkerLifetime, cfg.ExecutablePath)
			cfg.Native = &value
		}
		if cfg.Media != nil {
			value := *cfg.Media
			value.WorkerLifetime = r.configureWorkerLifetime(value.WorkerLifetime, cfg.ExecutablePath)
			cfg.Media = &value
		}
	}
	// Product defaults never enable a candidate. Experimental is an explicit
	// internal developer opt-in, unavailable from HTTP or ordinary launch flags.
	var encoder retrieval.QueryEncoder
	if r.native != nil {
		encoder = r.native
	}
	if cfg.Native != nil && cfg.Native.Experimental && r.native == nil {
		nativeCfg := *cfg.Native
		if err := r.prepareBundledWorker(ctx, &nativeCfg); err != nil {
			return err
		}
		supervisor, supervisorErr := inference.NewSupervisor(inference.Config{
			Profile: nativeCfg.Profile, Executable: nativeCfg.WorkerExecutable, AllowExperimental: true,
			WorkerLifetime: nativeCfg.WorkerLifetime,
			InstanceGuard: func() (*os.File, error) {
				if r.instanceGuard == nil {
					return nil, inference.ErrUnavailable
				}
				return r.instanceGuard.WorkerGuard()
			},
			Acquire: func(ctx context.Context, profile inference.Profile) (inference.ComponentLease, error) {
				return r.acquireNative(ctx, &nativeCfg, profile)
			},
			Admit: func(ctx context.Context, class inference.AdmissionClass, threads int) (inference.ComputeLease, error) {
				switch class {
				case inference.ForegroundQuery:
					return r.compute.Acquire(ctx, compute.ForegroundQuery, threads)
				case inference.BackgroundText:
					return r.compute.Acquire(ctx, compute.BackgroundText, threads)
				default:
					return nil, inference.ErrUnavailable
				}
			},
		})
		if supervisorErr == nil {
			r.native = supervisor
			encoder = supervisor
		}
	}
	if err = r.configureMedia(cfg.Media); err != nil {
		return err
	}
	retrievalCfg := retrieval.Config{Reader: r.archiveReader(store), Candidates: store, Index: store.ActiveIndex}
	if encoder != nil {
		retrievalCfg.Encoder = encoder
	}
	service, err := retrieval.NewService(retrievalCfg)
	if err != nil {
		return err
	}
	r.retrieval.set(service)
	return nil
}

func (r *resources) Reserve(ctx context.Context, request diskspace.Request) (diskspace.Lease, error) {
	if r.recoveryPending.Load() {
		return nil, errExtractionRecovery
	}
	return r.disk.Reserve(ctx, request)
}

func (r *resources) admitMedia(ctx context.Context, request ingestion.MediaDownloadRequest) (ingestion.DownloadAdmission, error) {
	if r.recoveryPending.Load() {
		return nil, errExtractionRecovery
	}
	return r.media.MediaAdmitter(ctx, request)
}

// Start background work only after the archive, resources and connector have
// passed the complete constructor boundary.
func (r *resources) startBackground(store *archive.Store, cfg Config) {
	if r.recoveryPending.Load() {
		return
	}
	if cfg.Transcription != nil && !cfg.Offline && r.voice == nil {
		r.startVoice(store, *cfg.Transcription)
	}
	if cfg.Native != nil && cfg.Native.Experimental && cfg.Native.IndexVersion != nil && r.runDone == nil {
		r.startIndexing(store, *cfg.Native.IndexVersion)
	}
	if cfg.Media != nil && cfg.Media.Experimental && r.mediaExtractor != nil && r.primaryJobs == nil {
		r.startPrimaryMedia(store, r.mediaExtractor, cfg.Media.ExtractionVersions)
	}
}

func (r *resources) close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if r.voice != nil {
		r.voice.cancel()
	}
	// Cancellation requests shutdown; admission guards remain actor-owned until
	// AwaitCleanup acknowledges complete process-group and component cleanup.
	if r.mediaJobs != nil {
		r.mediaJobs.cancel()
	}
	if r.primaryJobs != nil {
		r.primaryJobs.cancel()
	}
	if r.runCancel != nil {
		r.runCancel()
	}
	if r.compute != nil {
		r.compute.Close()
	}
	// Stop new physical allocation immediately. Active leases retain their
	// anchor/accounting; the final Close below also reports any sticky close error.
	if r.disk != nil {
		_ = r.disk.Close()
	}
	if err := r.modelOperations.stop(ctx); err != nil {
		return err
	}
	if r.runDone != nil {
		select {
		case <-r.runDone:
			r.runDone = nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.mediaJobs != nil {
		select {
		case <-r.mediaJobs.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.voice != nil {
		if err := r.voice.close(ctx); err != nil {
			return err
		}
		r.voice = nil
		r.voiceStatus.Store(nil)
	}
	if r.extraction != nil {
		if err := r.extraction.Close(ctx); err != nil {
			return err
		}
		r.extraction = nil
	}
	if r.primaryJobs != nil {
		select {
		case <-r.primaryJobs.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := r.primaryJobs.finishCleanup(); err != nil {
			return err
		}
		r.primaryJobs = nil
	}
	if r.mediaJobs != nil {
		if err := r.mediaJobs.closeOriginals(); err != nil {
			return err
		}
		r.mediaJobs = nil
	}
	if r.native != nil {
		closeErr := r.native.Close()
		if err := r.native.AwaitCleanup(ctx); err != nil {
			return err
		}
		if closeErr != nil && !errors.Is(closeErr, inference.ErrCleanup) {
			return closeErr
		}
		r.native = nil
	}
	if r.workerExecutable != nil {
		if err := r.workerExecutable.Close(); err != nil {
			return err
		}
		r.workerExecutable = nil
		r.workerExecutableReady = false
	}
	if r.demoAcceptance != nil {
		if err := r.demoAcceptance.Close(ctx); err != nil {
			return err
		}
		r.demoAcceptance = nil
	}
	if r.media != nil {
		if err := r.media.Close(); err != nil {
			return err
		}
		r.media = nil
	}
	if r.namespace != nil {
		if err := r.namespace.Close(); err != nil {
			return errNativeNamespace
		}
		r.namespace = nil
	}
	if err := r.ackNamespaceBootstrap(r.paths); err != nil {
		return err
	}
	if r.models != nil {
		if err := r.models.Close(); err != nil {
			return err
		}
		r.models = nil
	}
	if err := r.ackModelBootstrap(); err != nil {
		return err
	}
	if r.disk != nil {
		if err := r.disk.Close(); err != nil {
			return err
		}
		r.disk = nil
	}
	for len(r.bundles) != 0 {
		if err := r.bundles[0].assets.Close(); err != nil {
			return err
		}
		r.bundles = r.bundles[1:]
	}
	return nil
}
