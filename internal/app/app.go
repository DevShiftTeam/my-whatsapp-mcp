// Package app owns the single local archive, connector and instance lease.
package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"github.com/Bodokh/my-whatsapp-mcp/internal/transcription"
	"github.com/skip2/go-qrcode"
)

type Config struct {
	RebuildHistoryPolicy   archive.RebuildHistoryPolicy
	UnknownHistoryPolicy   ingestion.UnknownHistoryPolicy
	RebuildAncestorLockFDs []int
	HistoricalView         *HistoricalViewConfig
	RebuildSourceLockFD    *int
	Snapshot               *SnapshotConfig
	DataDir                string
	Demo                   bool
	DemoAcceptanceFixtures bool
	Offline                bool
	Native                 *NativeConfig
	Media                  *MediaConfig
	Transcription          *transcription.Options
	MCP                    *MCPConfig
	ExecutablePath         string
	DockerContainerName    string
	ReservedRemoteOrigins  []string
	OriginalBudgetBytes    *int64
	BackfillRequests       int
	instanceGuard          *appdata.InstanceLock
}

type connector interface {
	Pair(context.Context) error
	Connect(context.Context) error
	Pause(context.Context) error
	Resume(context.Context) error
	Disconnect(context.Context) error
	Close() error
	Status() ingestion.Status
	SourceAccount() string
	Quiesced() bool
}
type runtimeFactory func(ingestion.Config, archive.ConnectorStore, ingestion.Callbacks) (connector, error)

// Application serializes lifecycle mutations separately from callbacks. Callbacks
// never acquire ops or data, so stopping a connector can safely wait for them.
type Application struct {
	rebuildHistoryPolicy          archive.RebuildHistoryPolicy
	unknownHistoryPolicy          ingestion.UnknownHistoryPolicy
	historicalView                *historicalViewOwner
	snapshot                      *SnapshotConfig
	rebuild                       *ingestion.RebuildConfig
	rebuildSourceLease            *LiveRebuildSourceGuards
	ownerAuth                     ownerPasswordLifetime
	remoteLifecycle               remoteLifecycle
	managedMCP                    managedMCP
	ops                           sync.Mutex
	data                          sync.RWMutex
	stateMu                       sync.Mutex
	paths                         appdata.Paths
	lease                         *appdata.InstanceLock
	store                         *archive.Store
	resources                     *resources
	openResources                 func(context.Context, appdata.Paths, *archive.Store, Config, bool) (*resources, error)
	nativeCredentialID            archive.CredentialID
	authState                     applicationState
	nativeRegistry                publishedNativeRegistry
	remoteRegistry                publishedRemoteRegistry
	remoteRecoveryPending         bool
	nativeRecoveryPending         bool
	nativeEndpoint                mcpserver.NativeEndpoint
	mcp                           *mcpListeners
	mcpConfig                     MCPConfig
	mcpEnabled                    bool
	executablePath                string
	dockerContainerName           string
	reservedRemoteOrigins         []string
	recoveryOwner                 recoveryLifecycle
	localAI                       *localAIOwner
	backfillRequests              int
	transcriptionConfig           *transcription.Options
	connectionOwner               connectionRecoveryOwner
	connectionWake                chan struct{}
	runtime                       connector
	factory                       runtimeFactory
	demo, offline, paused, closed bool
	cleanupComplete               bool
	status                        ingestion.Status
	qrCode                        string
	qrExpires                     time.Time
	queryContext                  context.Context
	queryCancel                   context.CancelFunc
}

var _ localui.Application = (*Application)(nil)

func New(ctx context.Context, cfg Config) (*Application, error) {
	return newApplication(ctx, cfg, func(c ingestion.Config, s archive.ConnectorStore, cb ingestion.Callbacks) (connector, error) {
		return ingestion.New(c, s, cb)
	})
}

func newApplication(ctx context.Context, cfg Config, factory runtimeFactory) (*Application, error) {
	return newApplicationWithResources(ctx, cfg, factory, openResources)
}

func newApplicationWithResources(ctx context.Context, cfg Config, factory runtimeFactory, open func(context.Context, appdata.Paths, *archive.Store, Config, bool) (*resources, error)) (*Application, error) {
	cfg = copyApplicationConfig(cfg)
	if cfg.RebuildHistoryPolicy != "" && cfg.RebuildHistoryPolicy != archive.RebuildHistorySelectedOriginal && cfg.RebuildHistoryPolicy != archive.RebuildHistoryDelivered {
		return nil, archive.ErrInvalidPolicy
	}
	if cfg.RebuildHistoryPolicy == archive.RebuildHistoryDelivered && (cfg.Demo || cfg.Snapshot != nil) {
		return nil, archive.ErrInvalidPolicy
	}
	if cfg.UnknownHistoryPolicy != ingestion.UnknownHistoryStrict && cfg.UnknownHistoryPolicy != ingestion.UnknownHistoryPartial {
		return nil, archive.ErrInvalidPolicy
	}
	if err := validateTranscriptionConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.BackfillRequests == 0 {
		cfg.BackfillRequests = productBackfillLimit
	}
	if cfg.BackfillRequests < 0 || cfg.BackfillRequests > productBackfillLimit {
		return nil, archive.ErrInvalidPolicy
	}
	if cfg.DemoAcceptanceFixtures && !cfg.Demo {
		return nil, errDemoAcceptance
	}
	if cfg.ExecutablePath == "" {
		cfg.ExecutablePath, _ = os.Executable()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.OriginalBudgetBytes != nil && (*cfg.OriginalBudgetBytes < archive.MinimumOriginalBudgetBytes || *cfg.OriginalBudgetBytes > archive.MaximumOriginalBudgetBytes) {
		return nil, archive.ErrInvalidExtension
	}
	root, err := resolveDataRoot(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Snapshot == nil {
		if err := appdata.RefuseLiveRebuildSource(root); err != nil {
			return nil, err
		}
	}
	var paths appdata.Paths
	if cfg.Snapshot != nil {
		paths, err = appdata.ExistingPaths(root)
	} else {
		paths, err = appdata.Prepare(root)
	}
	if err != nil {
		return nil, err
	}
	if err = validateHistoricalSnapshot(paths, cfg); err != nil {
		return nil, err
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		return nil, err
	}
	state, exists, err := readState(paths, lease)
	if err != nil {
		lease.Close()
		return nil, err
	}
	mode := "real"
	if cfg.Demo {
		mode = "demo"
	}
	if exists && state.Mode != mode {
		lease.Close()
		return nil, appdata.ErrUnsafeDataPath
	}
	if cfg.Snapshot != nil && (!exists || state.Mode != "real") {
		lease.Close()
		return nil, archive.ErrHistoricalSnapshotReadOnly
	}
	rebuildSourceLease, err := AcquireLiveRebuildSourceGuard(paths.Root, cfg.RebuildSourceLockFD, cfg.RebuildAncestorLockFDs...)
	if err != nil {
		lease.Close()
		return nil, err
	}
	var store *archive.Store
	if cfg.Snapshot != nil {
		store, err = archive.OpenHistoricalSnapshot(ctx, paths.ArchiveDir)
	} else {
		store, err = archive.Open(ctx, paths.ArchiveDir, archive.Options{})
	}
	if err != nil {
		lease.Close()
		rebuildSourceLease.Close()
		return nil, err
	}
	var startupResources *resources
	var startupRuntime connector
	var startupHistorical *historicalViewOwner
	cleanup := func(e error) (*Application, error) {
		// A failed constructor still owns every resource until cleanup is
		// acknowledged; caller cancellation cannot release the instance early.
		if startupRuntime != nil {
			for {
				err := startupRuntime.Close()
				if err == nil && startupRuntime.Quiesced() {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		drainResources(startupResources)
		for startupHistorical.close() != nil {
			time.Sleep(50 * time.Millisecond)
		}
		for store.Close() != nil {
			time.Sleep(50 * time.Millisecond)
		}
		for lease.Close() != nil {
			time.Sleep(50 * time.Millisecond)
		}
		for rebuildSourceLease.Close() != nil {
			time.Sleep(50 * time.Millisecond)
		}
		return nil, e
	}
	provenance, err := store.RebuildProvenance(ctx)
	if err != nil {
		return cleanup(err)
	}
	if rebuildSourceLease != nil && (provenance == nil || *provenance != rebuildSourceLease.provenance) {
		return cleanup(archive.ErrReconciliationRequired)
	}
	rebuild, err := liveRebuildRuntimeConfig(paths, provenance, cfg.BackfillRequests)
	if err != nil {
		return cleanup(err)
	}
	if (cfg.RebuildSourceLockFD != nil || len(cfg.RebuildAncestorLockFDs) != 0) && rebuild == nil {
		return cleanup(archive.ErrReconciliationRequired)
	}
	if rebuild != nil && (cfg.Demo || cfg.Snapshot != nil || rebuildSourceLease == nil) {
		return cleanup(archive.ErrReconciliationRequired)
	}
	historyPolicy := archive.RebuildHistorySelectedOriginal
	if cfg.Snapshot == nil {
		selection, selectionErr := store.SelectRebuildHistoryPolicy(ctx, archive.RebuildHistorySelectionOptions{
			Policy:                  cfg.RebuildHistoryPolicy,
			AcceptDeliveredSnapshot: cfg.RebuildHistoryPolicy == archive.RebuildHistoryDelivered,
		})
		if selectionErr != nil {
			return cleanup(selectionErr)
		}
		if selection != nil {
			historyPolicy = selection.Policy
		}
	}
	account, err := store.BoundAccount(ctx)
	if err != nil {
		return cleanup(err)
	}
	if cfg.OriginalBudgetBytes != nil {
		budget, err := store.GetOriginalBudget(ctx)
		if err != nil {
			return cleanup(err)
		}
		if _, err = store.SetOriginalBudget(ctx, archive.OriginalBudgetUpdate{MaximumBytes: *cfg.OriginalBudgetBytes, ExpectedRevision: budget.Revision}); err != nil {
			return cleanup(err)
		}
	}
	if (cfg.Demo && account != "" && account != demoAccount) || (!cfg.Demo && account == demoAccount) {
		return cleanup(appdata.ErrUnsafeDataPath)
	}
	if !exists {
		state = applicationState{Version: 1, Mode: mode}
		if err = writeState(paths, lease, state); err != nil {
			return cleanup(err)
		}
	}
	queries, cancel := context.WithCancel(context.Background())
	cfg.instanceGuard = lease
	a := &Application{rebuildHistoryPolicy: historyPolicy, unknownHistoryPolicy: cfg.UnknownHistoryPolicy, snapshot: cfg.Snapshot, rebuild: rebuild, rebuildSourceLease: rebuildSourceLease, paths: paths, lease: lease, store: store, factory: factory, openResources: openResources, demo: cfg.Demo, offline: cfg.Offline, paused: state.Paused, nativeCredentialID: state.NativeCredentialID, authState: state, queryContext: queries, queryCancel: cancel, mcpEnabled: !cfg.Offline, executablePath: cfg.ExecutablePath, dockerContainerName: cfg.DockerContainerName, reservedRemoteOrigins: cfg.ReservedRemoteOrigins, mcpConfig: copyMCPConfig(cfg.MCP), backfillRequests: cfg.BackfillRequests, transcriptionConfig: cfg.Transcription, connectionWake: make(chan struct{}, 1)}
	a.historicalView, err = openHistoricalView(ctx, paths, store, cfg, rebuildSourceLease)
	startupHistorical = a.historicalView
	if err != nil {
		cancel()
		return cleanup(err)
	}
	if cfg.Snapshot != nil {
		a.resources, err = openHistoricalResources(ctx, paths, store, cfg)
	} else {
		a.resources, err = open(ctx, paths, store, cfg, state.Paused)
	}
	startupResources = a.resources
	if err != nil {
		cancel()
		return cleanup(err)
	}
	if !cfg.Demo && !cfg.Offline && cfg.Snapshot == nil {
		a.runtime, err = a.makeRuntime()
		startupRuntime = a.runtime
		if err != nil {
			cancel()
			return cleanup(err)
		}
		linked := a.runtime.SourceAccount()
		if account != "" && linked != "" && account != linked {
			cancel()
			return cleanup(archive.ErrUnauthorized)
		}
	}
	if err := ctx.Err(); err != nil {
		cancel()
		return cleanup(err)
	}
	// Auth recovery failure only fences MCP; owner UI and keyword remain usable.
	if a.mcpEnabled {
		_ = a.startMCP(ctx)
	} else {
		a.nativeRecoveryPending = a.reconcileNativeClients(ctx) != nil
	}
	if cfg.Snapshot != nil {
		return a, nil
	}
	a.startRecoveryAndBackground(cfg)
	a.startLocalAI(cfg)
	a.wakeOriginalDownloads()
	a.startConnectionRecovery(true)
	return a, nil
}

func drainResources(r *resources) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := r.close(ctx)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (a *Application) makeRuntime() (connector, error) {
	if a.snapshot != nil {
		return nil, archive.ErrHistoricalSnapshotReadOnly
	}
	return a.factory(ingestion.Config{RebuildHistoryPolicy: a.rebuildHistoryPolicy, UnknownHistoryPolicy: a.unknownHistoryPolicy, SessionDir: a.paths.SessionDir, TemporaryDir: a.paths.WorkingDir, BackfillRequests: a.backfillRequests, Rebuild: a.rebuild, DiagnosticDir: a.paths.WorkingDir}, a.store, ingestion.Callbacks{QRChanged: a.qrChanged, StatusChanged: a.statusChanged, MediaAdmitter: a.resources.admitMedia})
}

func resolveDataRoot(cfg Config) (string, error) {
	if !cfg.Demo {
		return cfg.DataDir, nil
	}
	normal, err := appdata.DefaultRoot()
	if err != nil {
		return "", err
	}
	if cfg.DataDir == "" {
		return filepath.Join(filepath.Dir(normal), "WA Indexer Demo"), nil
	}
	custom, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return "", appdata.ErrUnsafeDataPath
	}
	if filepath.Clean(custom) == filepath.Clean(normal) {
		return "", appdata.ErrUnsafeDataPath
	}
	return cfg.DataDir, nil
}
func (a *Application) qrChanged(value ingestion.QRNotification) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.qrCode = ""
	a.qrExpires = time.Time{}
	if a.status.Connection == "pairing" && value.Event == "code" && value.Code != "" && value.Timeout > 0 && value.Timeout <= 5*time.Minute {
		a.qrCode = value.Code
		a.qrExpires = time.Now().Add(value.Timeout)
	}
}
func (a *Application) statusChanged(value ingestion.Status) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.status = value
	if value.Connection != "pairing" {
		a.qrCode = ""
		a.qrExpires = time.Time{}
	}
	a.wakeConnectionRecovery()
}
func (a *Application) clearQR() {
	a.stateMu.Lock()
	a.qrCode = ""
	a.qrExpires = time.Time{}
	a.stateMu.Unlock()
}

func (a *Application) Setup(ctx context.Context) (localui.SetupState, error) {
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return localui.SetupState{}, archive.ErrArchiveUnavailable
	}
	policy, err := a.store.Policy(ctx)
	value := localui.SetupState{Configured: policy.Generation != 0, Policy: policy, Demo: a.demo, ArchiveView: a.historicalViewMetadata(), HistoricalSnapshot: a.snapshotMetadata(), Rebuild: a.liveRebuildMetadata()}
	if err == nil && a.rebuild != nil && a.snapshot == nil {
		value.Recovery, err = a.store.RebuildProgress(ctx)
	}
	return value, err
}

func (a *Application) SetPolicy(ctx context.Context, policy archive.ArchivePolicy, expected archive.Generation) (archive.ArchivePolicy, error) {
	if a.snapshot != nil {
		return archive.ArchivePolicy{}, archive.ErrHistoricalSnapshotReadOnly
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed {
		return archive.ArchivePolicy{}, archive.ErrArchiveUnavailable
	}
	previous, err := a.store.Policy(ctx)
	if err != nil {
		return archive.ArchivePolicy{}, err
	}
	restart := false
	if a.runtime != nil {
		snapshot := a.runtime.Status()
		restart = !a.paused && (snapshot.Connection == "connected" || snapshot.Connection == "pairing")
		if restart {
			if err := a.runtime.Pause(ctx); err != nil {
				return archive.ArchivePolicy{}, err
			}
		}
	}
	value, err := a.store.SetPolicy(ctx, policy, expected)
	if restart {
		if resumeErr := a.runtime.Resume(ctx); err == nil && resumeErr != nil {
			err = resumeErr
		}
	}
	if err == nil {
		a.requestSelectedBackfill(ctx, previous, value)
		a.wakeOriginalDownloads()
		a.wakeRebuild()
	}
	return value, err
}

func (a *Application) StartPairing(ctx context.Context) error {
	if a.snapshot != nil {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	if err := a.stopConnectionRecovery(ctx); err != nil {
		return err
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	if err := a.stopConnectionRecovery(ctx); err != nil {
		return err
	}
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed {
		return archive.ErrArchiveUnavailable
	}
	policy, err := a.store.Policy(ctx)
	if err != nil {
		return err
	}
	if policy.Generation == 0 {
		return archive.ErrInvalidPolicy
	}
	diagnostic, err := a.store.DiagnosticStatus(ctx)
	if err != nil {
		return err
	}
	if diagnostic.ReconciliationRequired {
		return archive.ErrReconciliationRequired
	}
	if a.paused {
		return archive.ErrInvalidPolicy
	}
	if a.demo {
		return a.seedDemo(ctx, policy.Generation)
	}
	if a.runtime == nil {
		a.runtime, err = a.makeRuntime()
		if err != nil {
			return err
		}
	}
	bound, err := a.store.BoundAccount(ctx)
	if err != nil {
		return err
	}
	linked := a.runtime.SourceAccount()
	if bound != "" && linked != "" && bound != linked {
		return archive.ErrUnauthorized
	}
	if err = a.persistConnectionIntent(true); err != nil {
		return err
	}
	defer a.startConnectionRecovery(false)
	if linked != "" {
		return a.runtime.Connect(ctx)
	}
	return a.runtime.Pair(ctx)
}

func (a *Application) PairingState(ctx context.Context) (localui.PairingState, error) {
	if err := ctx.Err(); err != nil {
		return localui.PairingState{}, err
	}
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return localui.PairingState{}, archive.ErrArchiveUnavailable
	}
	a.stateMu.Lock()
	runtimeStatus := a.status
	available := a.qrCode != "" && time.Now().Before(a.qrExpires)
	if !available {
		a.qrCode = ""
		a.qrExpires = time.Time{}
	}
	expiry := time.Time{}
	if available {
		expiry = a.qrExpires
	}
	a.stateMu.Unlock()
	if a.runtime != nil {
		current := a.runtime.Status()
		current.ReconciliationRequired = current.ReconciliationRequired || runtimeStatus.ReconciliationRequired
		runtimeStatus = current
	}
	state := string(runtimeStatus.Connection)
	if runtimeStatus.ReconciliationRequired {
		state = "reconciliation_required"
	} else if state == "" {
		state = "disconnected"
	}
	if state != "pairing" {
		available = false
		expiry = time.Time{}
	}
	return localui.PairingState{State: state, QRAvailable: available, ExpiresAt: expiry, HasSession: runtimeStatus.HasSession, ConnectionDiagnostic: safeConnectionDiagnostic(runtimeStatus.ConnectionDiagnostic)}, nil
}

func (a *Application) QRPNG(ctx context.Context) ([]byte, error) {
	if a.snapshot != nil {
		return nil, archive.ErrHistoricalSnapshotReadOnly
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return nil, archive.ErrArchiveUnavailable
	}
	a.stateMu.Lock()
	code := a.qrCode
	expiry := a.qrExpires
	a.stateMu.Unlock()
	if code == "" || !time.Now().Before(expiry) {
		return nil, archive.ErrSourceUnavailable
	}
	data, err := qrcode.Encode(code, qrcode.Medium, 320)
	if err != nil {
		return nil, archive.ErrSourceUnavailable
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	a.stateMu.Lock()
	current := a.qrCode == code && a.qrExpires == expiry && time.Now().Before(expiry)
	a.stateMu.Unlock()
	if !current {
		return nil, archive.ErrSourceUnavailable
	}
	return data, nil
}

func (a *Application) Pause(ctx context.Context) error {
	if a.snapshot != nil {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	if err := a.stopConnectionRecovery(ctx); err != nil {
		return err
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	if err := a.stopConnectionRecovery(ctx); err != nil {
		return err
	}
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed {
		return archive.ErrArchiveUnavailable
	}
	if a.paused {
		a.resources.setPaused(true)
		if err := a.persistPaused(true); err != nil {
			return err
		}
		return a.syncConnection(ctx, "paused")
	}
	if a.runtime != nil {
		if err := a.runtime.Pause(ctx); err != nil {
			return err
		}
	}
	a.paused = true
	a.resources.setPaused(true)
	if err := a.persistPaused(true); err != nil {
		return err
	}
	a.clearQR()
	return a.syncConnection(ctx, "paused")
}
func (a *Application) Resume(ctx context.Context) error {
	if a.snapshot != nil {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	if err := a.stopConnectionRecovery(ctx); err != nil {
		return err
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	if err := a.stopConnectionRecovery(ctx); err != nil {
		return err
	}
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed {
		return archive.ErrArchiveUnavailable
	}
	wasPaused := a.paused
	if err := a.persistConnectionIntent(true); err != nil {
		return err
	}
	defer a.startConnectionRecovery(false)
	// Resume is an explicit owner connection choice. Persist the unpaused state
	// before asking the existing runtime to reconnect its saved session.
	if err := a.persistPaused(false); err != nil {
		return err
	}
	if a.runtime != nil {
		if err := a.runtime.Resume(ctx); err != nil {
			// A failed start must remain paused across restarts. The runtime
			// fails closed before returning; never advertise a resumed state.
			if persistErr := a.persistPaused(wasPaused); persistErr != nil {
				return persistErr
			}
			return err
		}
	}
	a.paused = false
	a.resources.setPaused(false)
	if policy, err := a.store.Policy(ctx); err == nil {
		a.requestSelectedBackfill(ctx, archive.ArchivePolicy{}, policy)
	}
	a.wakeOriginalDownloads()
	connection := archive.ConnectionState("disconnected")
	if a.runtime != nil {
		connection = a.runtime.Status().Connection
		if connection == "" || connection == "paused" {
			connection = "disconnected"
		}
	}
	return a.syncConnection(ctx, connection)
}
func (a *Application) Disconnect(ctx context.Context) error {
	if a.snapshot != nil {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	if err := a.stopConnectionRecovery(ctx); err != nil {
		return err
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	if err := a.stopConnectionRecovery(ctx); err != nil {
		return err
	}
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed {
		return archive.ErrArchiveUnavailable
	}
	// A manual disconnect must survive a process restart even if network drain
	// fails. Commit its intent before touching the connector.
	if err := a.persistConnectionIntent(false); err != nil {
		return err
	}
	if a.runtime != nil {
		if err := a.runtime.Disconnect(ctx); err != nil {
			return err
		}
	}
	a.clearQR()
	connection := archive.ConnectionState("disconnected")
	if a.paused {
		connection = "paused"
	}
	return a.syncConnection(ctx, connection)
}
func (a *Application) syncConnection(ctx context.Context, connection archive.ConnectionState) error {
	account, err := a.store.BoundAccount(ctx)
	if err != nil || account == "" {
		return err
	}
	return a.store.SetSyncStatus(ctx, archive.SyncUpdate{Account: account, Connection: connection, CoverageUnknown: true})
}

func (a *Application) DeleteLocalData(ctx context.Context, confirmed bool) error {
	if a.rebuild != nil {
		return archive.ErrRebuildSessionTransition
	}
	if a.snapshot != nil {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	if !confirmed {
		return archive.ErrInvalidPolicy
	}
	if err := a.beginRemoteOperation(true); err != nil {
		return err
	}
	resetComplete := false
	defer func() { a.endRemoteOperation(resetComplete) }()
	// Recovery can hold data.RLock through a bounded dial. Cancel and join it
	// after fencing remote admission, before remote release needs data.Lock.
	if err := a.stopConnectionRecovery(ctx); err != nil {
		return err
	}
	if err := a.stopRemoteAccess(ctx); err != nil {
		return err
	}
	a.closeOwnerPassword()
	if err := a.stopRecovery(ctx); err != nil {
		return err
	}
	if err := a.stopLocalAI(ctx); err != nil {
		return err
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	if err := a.stopConnectionRecovery(ctx); err != nil {
		return err
	}
	a.queryCancel()
	a.data.Lock()
	defer a.data.Unlock()
	defer func() {
		if !a.closed {
			a.queryContext, a.queryCancel = context.WithCancel(context.Background())
		}
	}()
	if a.closed {
		return archive.ErrArchiveUnavailable
	}
	if err := a.stopRuntime(ctx); err != nil {
		return err
	}
	if err := a.closeMCP(ctx); err != nil {
		return err
	}
	if err := a.resources.close(ctx); err != nil {
		return err
	}
	if err := a.historicalView.close(); err != nil {
		return err
	}
	a.historicalView = nil
	if err := a.store.Close(); err != nil {
		a.store = nil
		a.closed = true
		return err
	}
	a.store = nil
	if err := appdata.ResetOwned(a.paths, a.lease); err != nil {
		a.closed = true
		return err
	}
	mode := "real"
	if a.demo {
		mode = "demo"
	}
	if err := writeState(a.paths, a.lease, applicationState{Version: 1, Mode: mode}); err != nil {
		a.closed = true
		return err
	}
	store, err := archive.Open(ctx, a.paths.ArchiveDir, archive.Options{})
	if err != nil {
		a.closed = true
		return err
	}
	a.store = store
	resetConfig := Config{Demo: a.demo, Offline: a.offline, instanceGuard: a.lease, ExecutablePath: a.executablePath, Transcription: a.transcriptionConfig}
	resetConfig.Native, resetConfig.Media, _ = LoadBundledAI(a.executablePath)
	a.resources, err = a.openResources(ctx, a.paths, store, resetConfig, false)
	if err != nil {
		if closeErr := a.resources.close(ctx); closeErr != nil {
			return closeErr
		}
		if closeErr := store.Close(); closeErr != nil {
			return closeErr
		}
		a.store = nil
		a.closed = true
		return err
	}
	a.paused = false
	a.nativeCredentialID = ""
	a.authState = applicationState{Version: 1, Mode: mode}
	a.nativeRegistry.replace(nil)
	a.nativeRecoveryPending = false
	a.stateMu.Lock()
	a.status = ingestion.Status{}
	a.qrCode = ""
	a.qrExpires = time.Time{}
	a.stateMu.Unlock()
	// Deletion recreates only the empty owner archive. A fresh explicit pairing
	// action constructs a connector; no deleted WhatsApp session is reused.
	a.startRecoveryAndBackground(resetConfig)
	a.startLocalAI(resetConfig)
	if a.mcpConfig.Remote != nil && a.mcpConfig.Remote.Managed {
		a.mcpConfig.Remote = nil
	}
	a.remoteRecoveryPending = false
	resetComplete = true
	return nil
}

func (a *Application) stopRuntime(ctx context.Context) error {
	if a.runtime == nil {
		return nil
	}
	disconnectErr := a.runtime.Disconnect(ctx)
	closeErr := a.runtime.Close()
	if !a.runtime.Quiesced() {
		if disconnectErr != nil {
			return disconnectErr
		}
		if closeErr != nil {
			return closeErr
		}
		return archive.ErrArchiveUnavailable
	}
	if closeErr != nil && !errors.Is(closeErr, archive.ErrReconciliationRequired) {
		return closeErr
	}
	// Unsafe archive markers remain until the explicit ResetOwned below. A
	// drained, closed connector can safely release its old archive for reset.
	a.runtime = nil
	return nil
}

func (a *Application) Close(ctx context.Context) error {
	if err := a.beginRemoteOperation(true); err != nil {
		return err
	}
	defer a.endRemoteOperation(false)
	// Recovery can hold data.RLock through a bounded dial. Cancel and join it
	// after fencing remote admission, before remote release needs data.Lock.
	if err := a.stopConnectionRecovery(ctx); err != nil {
		return err
	}
	if err := a.stopRemoteAccess(ctx); err != nil {
		return err
	}
	a.closeOwnerPassword()
	if err := a.stopRecovery(ctx); err != nil {
		return err
	}
	if err := a.stopLocalAI(ctx); err != nil {
		return err
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	if err := a.stopConnectionRecovery(ctx); err != nil {
		return err
	}
	a.queryCancel()
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed {
		if a.lease != nil {
			return a.closeInstanceOwnership()
		}
		return nil
	}
	if err := a.closeMCP(ctx); err != nil {
		return err
	}
	// If drain fails, never clear archive safety markers; still close handles and
	// release ownership so the next startup can report reconciliation_required.
	var result error
	if a.runtime != nil {
		result = a.runtime.Disconnect(ctx)
		if err := a.runtime.Close(); result == nil {
			result = err
		}
		if !a.runtime.Quiesced() {
			// Keep the archive and instance lease while any worker can still
			// touch them. Process exit releases the lease after termination.
			if result == nil {
				result = archive.ErrArchiveUnavailable
			}
			a.queryContext, a.queryCancel = context.WithCancel(context.Background())
			return result
		}
		// The joined runtime may finish its independent drain after the caller
		// deadline. Commit terminal suppression before discarding its metadata.
		if err := a.persistTerminalConnectionIntent(context.Background()); err != nil {
			return err
		}
		a.runtime = nil
	}
	if a.resources != nil {
		if err := a.resources.close(ctx); err != nil {
			return err // Native or filesystem writers still own the archive and instance.
		}
	}
	if err := appdata.RemoveOwnerURL(a.paths, a.lease); result == nil {
		result = err
	}
	if err := a.historicalView.close(); err != nil {
		return err
	}
	a.historicalView = nil
	if a.store != nil {
		if err := a.store.Close(); result == nil {
			result = err
		}
		a.store = nil
	}
	if err := a.closeInstanceOwnership(); err != nil && result == nil {
		result = err
	}
	a.closed = true
	a.clearQR()
	return result
}
func (a *Application) WriteOwnerURL(url string) error {
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return archive.ErrArchiveUnavailable
	}
	return appdata.WriteOwnerURL(a.paths, a.lease, url)
}
func (a *Application) RemoveOwnerURL() error {
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return nil
	}
	return appdata.RemoveOwnerURL(a.paths, a.lease)
}

func (a *Application) query(ctx context.Context) (context.Context, func()) {
	linked, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.queryContext, cancel)
	return linked, func() { stop(); cancel() }
}
func (a *Application) scope(ctx context.Context) (archive.ReadScope, error) {
	policy, err := a.store.Policy(ctx)
	if err != nil {
		return archive.ReadScope{}, err
	}
	account, err := a.store.BoundAccount(ctx)
	if err != nil {
		return archive.ReadScope{}, err
	}
	if account == "" || policy.Generation == 0 {
		return archive.ReadScope{}, archive.ErrUnauthorized
	}
	return archive.ReadScope{Account: account, Principal: "owner", PolicyGeneration: policy.Generation, AllChats: true}, nil
}
func (a *Application) Search(ctx context.Context, r archive.SearchRequest) (archive.SearchPage, error) {
	if !liveSearchHandles(r) {
		return archive.SearchPage{}, archive.ErrInvalidQuery
	}
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return archive.SearchPage{}, archive.ErrArchiveUnavailable
	}
	ctx, done := a.query(ctx)
	defer done()
	scope, err := a.scope(ctx)
	if err != nil {
		return archive.SearchPage{}, err
	}
	return a.resources.retrieval.Search(ctx, scope, r)
}
func (a *Application) Context(ctx context.Context, r archive.ContextRequest) (archive.ContextPage, error) {
	if foreignArchiveHandle(string(r.MessageID)) {
		return archive.ContextPage{}, archive.ErrInvalidQuery
	}
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return archive.ContextPage{}, archive.ErrArchiveUnavailable
	}
	ctx, done := a.query(ctx)
	defer done()
	scope, err := a.scope(ctx)
	if err != nil {
		return archive.ContextPage{}, err
	}
	return a.store.Context(ctx, scope, r)
}
func (a *Application) Chats(ctx context.Context, r archive.ChatListRequest) (archive.DiscoveryPage, error) {
	if foreignArchiveHandle(r.Cursor) {
		return archive.DiscoveryPage{}, archive.ErrInvalidQuery
	}
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return archive.DiscoveryPage{}, archive.ErrArchiveUnavailable
	}
	ctx, done := a.query(ctx)
	defer done()
	return a.store.DiscoveryChats(ctx, r)
}
func (a *Application) Status(ctx context.Context) (archive.StatusSnapshot, error) {
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return archive.StatusSnapshot{}, archive.ErrArchiveUnavailable
	}
	ctx, done := a.query(ctx)
	defer done()
	return a.statusLocked(ctx)
}
func (a *Application) statusLocked(ctx context.Context) (archive.StatusSnapshot, error) {
	diagnostic, err := a.store.DiagnosticStatus(ctx)
	if err != nil {
		return archive.StatusSnapshot{}, err
	}
	status := diagnostic
	if !diagnostic.ReconciliationRequired {
		scope, scopeErr := a.scope(ctx)
		if scopeErr == nil {
			status, err = a.resources.archiveReader(a.store).Status(ctx, scope)
			if err != nil {
				return archive.StatusSnapshot{}, err
			}
		} else if !errors.Is(scopeErr, archive.ErrUnauthorized) {
			return archive.StatusSnapshot{}, scopeErr
		}
	}
	a.stateMu.Lock()
	cachedRuntimeStatus := a.status
	a.stateMu.Unlock()
	runtimeStatus := cachedRuntimeStatus
	if a.runtime != nil {
		runtimeStatus = a.runtime.Status()
		if cachedRuntimeStatus.ReconciliationRequired {
			runtimeStatus.ReconciliationRequired = true
			runtimeStatus.Codes = safeDiagnosticCodes(runtimeStatus.Codes, cachedRuntimeStatus.Codes, false)
		}
	}
	status.ConnectionDiagnostic = safeConnectionDiagnostic(runtimeStatus.ConnectionDiagnostic)
	status.Codes = safeDiagnosticCodes(status.Codes, runtimeStatus.Codes, status.ReconciliationRequired || runtimeStatus.ReconciliationRequired)
	if status.ReconciliationRequired || runtimeStatus.ReconciliationRequired {
		status.ReconciliationRequired = true
		status.Connection = "reconciliation_required"
		status.Chats = nil
		status.Messages = 0
	} else if status.Snapshot.PolicyGeneration == 0 {
		status.Connection = "setup_required"
	} else if a.paused {
		status.Connection = "paused"
		status.Paused = true
	} else if a.runtime != nil {
		connection := runtimeStatus.Connection
		if connection != "" {
			status.Connection = connection
		}
		if status.Connection == "" {
			status.Connection = "disconnected"
		}
	} else {
		status.Connection = "disconnected"
	}
	status.Paused = a.paused
	a.resources.progress(&status)
	if status.ReconciliationRequired {
		status.Keyword.State = "unavailable"
		status.Keyword.Code = "reconciliation_required"
		for _, progress := range []*archive.Progress{&status.Keyword, &status.Semantic, &status.Media} {
			progress.Pending = 0
			progress.Complete = 0
			progress.Failed = 0
		}
	}
	if a.demo {
		status.Codes = append(status.Codes, "demo")
	}
	if a.snapshot != nil {
		status.Connection = "historical_snapshot"
		status.Paused = true
		status.Coverage = "partial"
		status.HistoricalSnapshot = a.snapshotMetadata()
		status.Keyword.Pending = 0
		status.Semantic = archive.Progress{State: "unavailable", Code: "historical_snapshot_read_only"}
		status.Media = archive.Progress{State: "unavailable", Code: "historical_snapshot_read_only"}
		if a.resources.snapshotTranscriptionReady {
			status.Media.Code = "service_ready_processing_disabled"
		}
	}
	return status, nil
}

// CleanupComplete is a terminal acknowledgement, not a PID/state inference.
func (a *Application) CleanupComplete() bool {
	a.data.RLock()
	defer a.data.RUnlock()
	return a.cleanupComplete
}
