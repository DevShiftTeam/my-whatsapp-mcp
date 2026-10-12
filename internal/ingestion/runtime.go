package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/socket"
	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type connection interface {
	ConnectContext(context.Context) error
	Disconnect()
	AddEventHandler(whatsmeow.EventHandler) uint32
	RemoveEventHandler(uint32) bool
	GetQRChannel(context.Context) (<-chan whatsmeow.QRChannelItem, error)
}
type queuedEvent struct {
	event              any
	bytes              int64
	run, recoveryEpoch uint64
}

// The coverage gap retains no SDK message metadata or undecrypted payload.
type unreadableMessageGap struct{}

// These two SDK signals belong only to the current explicit unlinked Pair.
// Runtime.mu protects the flags; the captured context retires with that run.
type pairingHandoff struct {
	observation                   archive.ObservationID
	context                       context.Context
	success, reconnect, scheduled bool
}

type Runtime struct {
	config                    Config
	store                     archive.ConnectorStore
	callbacks                 Callbacks
	client                    *whatsmeow.Client
	connection                connection
	session                   interface{ Close() error }
	op                        sync.Mutex
	mu                        sync.Mutex
	status                    Status
	account                   string
	closed                    bool
	quiesced                  bool
	sessionCloseFailed        bool
	sessionDeleted            bool             // Runtime.mu; constructor snapshot and typed logout/deletion events
	recoveryDesired           bool             // Runtime.mu; explicit owner connection intent
	recoveryRun               uint64           // Runtime.mu; monotonic, unlike store observation IDs
	recoveryEpoch             uint64           // Runtime.mu; restored/Connected invalidation generation
	recoveryNextToken         uint64           // Runtime.mu; stable identity for a bounded retry chain
	recovery                  *recoveryWitness // Runtime.mu
	recoveryRestoredAt        time.Time        // Runtime.mu; SDK success watermark of the restored outage
	recoveryTimeoutSuccess    time.Time        // Runtime.mu; earliest valid success in the current timeout wave
	recoveryDisconnectPending bool             // Runtime.mu; accepted current-run loss awaits durable gap commit
	recoveryRunStartedAt      time.Time        // Runtime.mu; SDK timeout metadata must belong to this run
	connectionAttempts        uint64           // Runtime.mu; actual owned SDK dial authorization
	observation               archive.ObservationID
	runCtx                    context.Context
	cancel                    context.CancelFunc
	queue                     chan queuedEvent
	queueBytes                int64
	admissions                int
	waitingBytes              int64 // Runtime.mu; separately bounded retained callback payloads
	waitingAdmissions         int   // Runtime.mu; no delivery goroutines are created
	admissionChanged          chan struct{}
	admissionTimeout          time.Duration // private test hook; zero uses the production deadline
	queueClosed               bool
	deliveriesDone            chan struct{}
	deliveryStopDone          chan struct{}
	connectDone               chan struct{}
	socketStopDone            chan struct{}
	deliveryStopping          chan struct{} // deliveryMu; closed before removing the SDK handler
	workerDone                chan struct{}
	qrDone                    chan struct{}
	qrEnded                   bool            // Runtime.mu; terminal QR delivery for the current observation
	pairingHandoff            *pairingHandoff // Runtime.mu; nil for saved-session Connect
	handler                   uint32
	deliveryMu                sync.Mutex
	accepting                 bool
	deliveries                sync.WaitGroup
	activeMu                  sync.Mutex
	activeCancel              context.CancelFunc
	optionalMu                sync.Mutex
	optional                  *optionalNetwork
	optionalJoined            bool
	backfillRequests          int
	lastBackfillExplicit      time.Time // Runtime.optionalMu, retained across loop restarts
	sendHistoryRequest        func(context.Context, *waE2E.Message) error
	download                  func(context.Context, whatsmeow.DownloadableMessage, whatsmeow.File) error

	lastRebuildRequest  time.Time // optionalMu; courtesy throttle, durable ledger owns rate
	rebuildActiveMu     sync.Mutex
	rebuildActiveCancel context.CancelFunc
	rebuildActiveDone   chan struct{}
	sendRebuildRequest  func(context.Context, string, *waE2E.Message) error // test seam
	diagnosticMu        sync.Mutex
	legacyMediaReceiver bool // private isolated-test hook; public New never enables it
}

func New(config Config, store archive.ConnectorStore, callbacks Callbacks) (*Runtime, error) {
	if store == nil || (config.UnknownHistoryPolicy != UnknownHistoryStrict && config.UnknownHistoryPolicy != UnknownHistoryPartial) {
		return nil, archive.ErrInvalidEvent
	}
	if err := configureRebuild(context.Background(), &config, store); err != nil {
		return nil, err
	}
	if config.DiagnosticDir != "" {
		if err := privateDirectory(config.DiagnosticDir, true); err != nil {
			return nil, err
		}
	}
	if config.BodyQueueCount == 0 {
		config.BodyQueueCount = 64
	}
	if config.BodyQueueBytes == 0 {
		config.BodyQueueBytes = 16 << 20
	}
	if config.MediaMax == 0 {
		config.MediaMax = 25 << 20
	}
	if config.BodyQueueCount < 1 || config.BodyQueueCount > 256 || config.BodyQueueBytes < 1 || config.BodyQueueBytes > 64<<20 || config.MediaMax < 1 || config.MediaMax > 64<<20 || config.BackfillRequests < 0 || config.BackfillRequests > 32 {
		return nil, archive.ErrInvalidEvent
	}
	if config.TemporaryDir == "" {
		return nil, archive.ErrUnsafeDataPath
	}
	if err := privateDirectory(config.TemporaryDir, true); err != nil {
		return nil, err
	}
	container, client, err := openSession(context.Background(), config.SessionDir)
	if err != nil {
		return nil, err
	}
	if config.Rebuild != nil && (client.Store.ID == nil || client.Store.ID.ToNonAD().String() != config.Rebuild.Account || config.Rebuild.DeviceID != "" && client.Store.ID.String() != config.Rebuild.DeviceID) {
		_ = container.Close()
		return nil, archive.ErrUnauthorized
	}
	max, _ := mediaCiphertextMaximum(config.MediaMax)
	if compressedMax+1024 > max {
		max = compressedMax + 1024
	}
	client.SetMediaHTTPClient(&http.Client{Transport: &capTransport{base: http.DefaultTransport, max: max}, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	r := newRuntime(config, store, callbacks, client, client)
	r.session = container
	return r, nil
}
func newRuntime(config Config, store archive.ConnectorStore, callbacks Callbacks, client *whatsmeow.Client, c connection) *Runtime {
	client.Log = newPhoneResponseLogger(client.Log)
	return &Runtime{config: config, store: store, callbacks: callbacks, client: client, connection: c, quiesced: true, optionalJoined: true, sessionDeleted: client.Store.Deleted, status: Status{Connection: "disconnected", HasSession: client.Store.ID != nil && !client.Store.Deleted, Codes: []string{"coverage_unknown", "phone_behavior_not_run", "session_metadata_parity_not_run", "session_canary_audit_not_run"}}}
}

// Quiesced permits explicit owner reset even when an unsafe marker is retained.
// A deadline while stopping delivery leaves this false and forbids deletion.
func (r *Runtime) Quiesced() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.quiesced && r.optionalJoined && !r.sessionCloseFailed
}
func (r *Runtime) SourceAccount() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.account != "" {
		return r.account
	}
	if r.client.Store.ID != nil {
		return r.client.Store.ID.ToNonAD().String()
	}
	return ""
}
func (r *Runtime) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	s.Codes = append([]string(nil), s.Codes...)
	return s
}

func terminalConnectionEvent(event string) bool {
	switch event {
	case "logged_out", "connect_failure", "client_outdated", "temporary_ban", "stream_error", "stream_replaced", "cat_refresh_error":
		return true
	default:
		return false
	}
}

func connectionErrorClass(err error) (string, int) {
	if err == nil {
		return "none", 0
	}
	if errors.Is(err, context.Canceled) {
		return "canceled", 0
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline", 0
	}
	for _, known := range []struct {
		err   error
		class string
	}{
		{archive.ErrReconciliationRequired, "reconciliation_required"},
		{archive.ErrQueueLimitExceeded, "queue_limit_exceeded"},
		{archive.ErrArchiveUnavailable, "archive_unavailable"},
		{archive.ErrSourceRevisionConflict, "source_revision_conflict"},
		{archive.ErrIdentityAliasConflict, "identity_alias_conflict"},
		{archive.ErrMutationMetadataMissing, "mutation_metadata_missing"},
		{archive.ErrPolicyChanged, "policy_changed"},
		{archive.ErrInvalidPolicy, "invalid_policy"},
		{archive.ErrInvalidEvent, "invalid_event"},
	} {
		if errors.Is(err, known.err) {
			return known.class, 0
		}
	}
	if errors.Is(err, wmstore.ErrDeviceDeleted) {
		return "device_deleted", 0
	}
	if errors.Is(err, whatsmeow.ErrAlreadyConnected) {
		return "already_connected", 0
	}
	if errors.Is(err, archive.ErrSourceUnavailable) {
		return "source_unavailable", 0
	}
	var status socket.ErrWithStatusCode
	if errors.As(err, &status) && status.StatusCode >= 100 && status.StatusCode <= 599 {
		return "http_status", status.StatusCode
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return "network_timeout", 0
	}
	var operation *net.OpError
	if errors.As(err, &operation) && operation.Op == "dial" &&
		(errors.Is(operation.Err, syscall.ENETDOWN) || errors.Is(operation.Err, syscall.ENETUNREACH) || errors.Is(operation.Err, syscall.EHOSTUNREACH)) {
		return "network_unavailable", 0
	}
	if errors.Is(err, socket.ErrDialFailed) {
		return "dial", 0
	}
	return "unknown", 0
}

func (r *Runtime) connectionDiagnostic(phase, event string, err error, reason int, onConnect bool) {
	class, code := connectionErrorClass(err)
	if class == "http_status" {
		reason = code
	}
	r.mu.Lock()
	d := &r.status.ConnectionDiagnostic
	if event == "disconnected" || terminalConnectionEvent(event) {
		d.SocketStateKnown, d.SocketConnected, d.SocketAuthenticated = false, false, false
	}
	if terminalConnectionEvent(event) || class == "device_deleted" {
		r.invalidateRecoveryLocked()
	}
	if event == "logged_out" || class == "device_deleted" {
		r.sessionDeleted = true
		r.status.HasSession = false
	}
	// Later disconnection and owner cleanup must not erase the first login
	// failure. A fresh Runtime is required for a deleted linked device.
	if terminalConnectionEvent(d.Event) || (err == nil && d.ErrorClass != "" && d.ErrorClass != "none") {
		r.mu.Unlock()
		r.notify()
		return
	}
	d.Phase, d.Event, d.ErrorClass, d.Reason, d.OnConnect = phase, event, class, reason, onConnect
	r.mu.Unlock()
	r.notify()
}

func (r *Runtime) connectionFailure(phase string, err error) error {
	r.connectionDiagnostic(phase, "none", err, 0, false)
	recordRuntimeFailure(phase, "connection_failed", err)
	return err
}

// Diagnostic fields are closed codes. Error text may contain message bodies,
// account identifiers, signed URLs, credentials, or private filesystem paths.
func runtimeFailureDiagnostic(stage, code string, err error) []byte {
	switch stage {
	case "start", "runtime_closed", "session_close_failed", "session_deleted", "reconciliation_required", "optional_join", "prior_stop", "policy", "session_missing", "recovery_refused", "observation", "recovery_policy", "connect", "recovery_start", "post_pair_start", "post_pair_stop", "qr_channel":
	case "stop_handler", "stop_deliveries", "stop_source", "stop_connect", "stop_socket", "stop_qr", "stop_sync", "stop_store", "stop_observation", "stop_optional":
	case "source_message", "unreadable_account", "unreadable_sync", "connected_account", "connected_sync", "backfill_request", "disconnected_sync", "app_state_sync", "control_invalidation", "message_structure", "message_limit", "queue_admission", "history_event":
	case "source_policy", "source_canonical", "source_alias", "source_session_alias", "source_sender_alias", "source_recipient_alias", "source_names", "source_expiry", "source_body", "source_apply", "source_sync":
	default:
		stage = "unknown"
	}
	switch code {
	case "connection_failed", "optional_join_failed", "drain_failed", "reconciliation_required", "archive_unavailable", "history_corrupt", "history_limit_exceeded", "ambiguous_message_structure", "ambiguous_edit_structure", "mutation_metadata_missing", "queue_limit_exceeded", "identity_alias_conflict", "source_revision_conflict", "unsafe_mutation_projection", "control_admission_failed", "source_operation_failed", "source_parent_canceled", "source_parent_deadline":
	default:
		code = "unknown"
	}
	class, _ := connectionErrorClass(err)
	encoded, _ := json.Marshal(struct {
		Event string `json:"event"`
		Stage string `json:"stage"`
		Code  string `json:"code"`
		Class string `json:"class"`
	}{"runtime_failure", stage, code, class})
	return append(encoded, '\n')
}

func recordRuntimeFailure(stage, code string, err error) {
	_, _ = os.Stderr.Write(runtimeFailureDiagnostic(stage, code, err))
}

// Return the original error unchanged: source revision rejection is meaningful
// only after its exact store quarantine result, and callers compare that value.
func sourceOperationFailure(ctx context.Context, stage string, err error) error {
	if err != nil {
		code := "source_operation_failed"
		if errors.Is(ctx.Err(), context.Canceled) {
			code = "source_parent_canceled"
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = "source_parent_deadline"
		}
		recordRuntimeFailure(stage, code, err)
	}
	return err
}

// Query only after the owned ConnectContext call returns or Connected is
// consumed. Status itself never blocks behind the SDK socket lock during dial.
func (r *Runtime) snapshotSocketState() {
	if c, ok := r.connection.(interface {
		IsConnected() bool
		IsLoggedIn() bool
	}); ok {
		connected, authenticated := c.IsConnected(), c.IsLoggedIn()
		r.mu.Lock()
		d := &r.status.ConnectionDiagnostic
		if d.Event != "disconnected" && !terminalConnectionEvent(d.Event) {
			d.SocketStateKnown, d.SocketConnected, d.SocketAuthenticated = true, connected, authenticated
		}
		r.mu.Unlock()
	}
}

func connectionReason(reason events.ConnectFailureReason) int {
	switch reason {
	case 400, 401, 402, 403, 405, 406, 409, 413, 414, 415, 418, 500, 501, 503:
		return int(reason)
	default:
		return 0
	}
}
func (r *Runtime) notify() {
	if r.callbacks.StatusChanged != nil {
		r.callbacks.StatusChanged(r.Status())
	}
}
func (r *Runtime) Pair(ctx context.Context) error {
	r.op.Lock()
	defer r.op.Unlock()
	r.setRecoveryDesired(true)
	return r.start(ctx, true)
}
func (r *Runtime) Connect(ctx context.Context) error {
	r.op.Lock()
	defer r.op.Unlock()
	r.setRecoveryDesired(true)
	return r.start(ctx, false)
}
func (r *Runtime) Reconnect(ctx context.Context) error {
	r.op.Lock()
	defer r.op.Unlock()
	r.setRecoveryDesired(true)
	if err := r.stop(ctx); err != nil {
		return err
	}
	return r.start(ctx, false)
}
func (r *Runtime) Pause(ctx context.Context) error {
	r.setRecoveryDesired(false)
	r.op.Lock()
	defer r.op.Unlock()
	r.setRecoveryDesired(false)
	if err := r.stop(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	r.status.Paused = true
	r.mu.Unlock()
	r.notify()
	return nil
}
func (r *Runtime) Resume(ctx context.Context) error {
	r.op.Lock()
	defer r.op.Unlock()
	r.setRecoveryDesired(true)
	if r.client.Store.ID == nil {
		r.stopOptional()
		if err := r.joinOptional(ctx); err != nil {
			return err
		}
		r.mu.Lock()
		r.status.Paused = false
		r.mu.Unlock()
		r.notify()
		return nil
	}
	return r.start(ctx, false)
}
func (r *Runtime) Disconnect(ctx context.Context) error {
	r.setRecoveryDesired(false)
	r.op.Lock()
	defer r.op.Unlock()
	r.setRecoveryDesired(false)
	return r.stop(ctx)
}
func (r *Runtime) Close() error {
	r.setRecoveryDesired(false)
	r.op.Lock()
	defer r.op.Unlock()
	r.setRecoveryDesired(false)
	if r.closed {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := r.stop(ctx)
	r.mu.Lock()
	drained := r.quiesced
	r.mu.Unlock()
	if !drained {
		return err
	}
	if r.session != nil {
		if closeErr := r.session.Close(); closeErr != nil {
			r.mu.Lock()
			r.sessionCloseFailed = true
			r.mu.Unlock()
			return archive.ErrSourceUnavailable
		}
	}
	r.mu.Lock()
	r.sessionCloseFailed = false
	r.mu.Unlock()
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return err
}
func (r *Runtime) start(ctx context.Context, pair bool) error {
	return r.startConnection(ctx, pair, "connect")
}
func (r *Runtime) startConnection(ctx context.Context, pair bool, connectPhase string, expectedRecovery ...RecoveryRequest) error {
	var recoveryRequest RecoveryRequest
	if len(expectedRecovery) == 1 {
		recoveryRequest = expectedRecovery[0]
	}
	r.mu.Lock()
	deleted, ended := r.sessionDeleted, r.qrEnded
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return r.connectionFailure("start", err)
	}
	if r.closed {
		return r.connectionFailure("runtime_closed", archive.ErrSourceUnavailable)
	}
	r.mu.Lock()
	closeFailed := r.sessionCloseFailed
	r.mu.Unlock()
	if closeFailed {
		return r.connectionFailure("session_close_failed", archive.ErrSourceUnavailable)
	}
	if deleted {
		r.connectionDiagnostic("session_deleted", "none", wmstore.ErrDeviceDeleted, 0, false)
		recordRuntimeFailure("session_deleted", "connection_failed", wmstore.ErrDeviceDeleted)
		return archive.ErrSourceUnavailable
	}
	if r.Status().ReconciliationRequired {
		return r.connectionFailure("reconciliation_required", archive.ErrReconciliationRequired)
	}
	if ended && r.observation != "" && r.linkedPairingSession() {
		// Pair and saved-session Connect share this late-link no-op, after all
		// refusal gates. Never tear down a just-linked/authenticated socket.
		r.mu.Lock()
		recovering := r.status.Connection == "disconnected"
		if recovering {
			r.status.Connection = "pairing"
		}
		r.mu.Unlock()
		if recovering {
			r.connectionDiagnostic("awaiting_authentication", "none", nil, 0, false)
		} else {
			r.notify()
		}
		return nil
	}
	r.mu.Lock()
	newAttempt := connectPhase == "connect" && (r.observation == "" || r.status.Connection == "disconnected")
	if newAttempt || (connectPhase != "connect" && !terminalConnectionEvent(r.status.ConnectionDiagnostic.Event)) {
		r.status.ConnectionDiagnostic = ConnectionDiagnostic{Phase: "start", Event: "none", ErrorClass: "none"}
	}
	r.mu.Unlock()
	if r.observation == "" {
		r.stopOptional()
		if err := r.joinOptional(ctx); err != nil {
			return r.connectionFailure("optional_join", err)
		}
	}
	if r.observation != "" {
		if r.Status().Connection != "disconnected" {
			return nil
		}
		// The request controls a new dial, while already accepted source work
		// belongs to the previous observation and needs its own finite drain.
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), recoveryDrainTimeout)
		err := r.stop(drainCtx)
		cancelDrain()
		if err != nil {
			return r.connectionFailure("prior_stop", err)
		}
		if err := ctx.Err(); err != nil {
			return r.connectionFailure("start", err)
		}
	}
	policy, err := r.store.Policy(ctx)
	if err != nil {
		return r.connectionFailure("policy", err)
	}
	if policy.Generation == 0 || (policy.DateMode != archive.DateSince && policy.DateMode != archive.DateAllAvailable) || (policy.ChatMode != archive.ChatsAll && policy.ChatMode != archive.ChatsSelective) {
		return r.connectionFailure("policy", archive.ErrInvalidPolicy)
	}
	if !pair && r.client.Store.ID == nil {
		r.mu.Lock()
		r.status.HasSession = false
		r.mu.Unlock()
		return r.connectionFailure("session_missing", archive.ErrSourceUnavailable)
	}
	if pair && r.client.Store.ID == nil {
		if err := configureInitialHistoryPayload(r.client, policy, time.Now()); err != nil {
			return r.connectionFailure("policy", err)
		}
	}
	if connectPhase == "recovery_start" && !r.recoveryStartAuthorized(recoveryRequest) {
		return r.connectionFailure("recovery_refused", archive.ErrSourceUnavailable)
	}
	observation, err := r.store.BeginObservation(ctx, r.SourceAccount())
	if err != nil {
		return r.connectionFailure("observation", err)
	}
	r.observation = observation
	r.mu.Lock()
	r.recoveryRun++
	callbackRun := r.recoveryRun
	r.recoveryRunStartedAt = time.Now()
	r.recoveryRestoredAt = time.Time{}
	r.recoveryTimeoutSuccess = time.Time{}
	r.recoveryDisconnectPending = false
	if r.recovery != nil {
		r.recovery.run = callbackRun
		r.recovery.claimed = false
		r.recovery.ready = false
		r.recovery.keepAlive = false
		r.recovery.disconnected = false
	}
	r.mu.Unlock()
	r.runCtx, r.cancel = context.WithCancel(context.Background())
	r.client.BackgroundEventCtx = r.runCtx
	var handoff *pairingHandoff
	if pair && r.client.Store.ID == nil {
		handoff = &pairingHandoff{observation: observation, context: r.runCtx}
	}
	var qr <-chan whatsmeow.QRChannelItem
	if pair && r.client.Store.ID == nil {
		qr, err = r.connection.GetQRChannel(r.runCtx)
		if err != nil {
			r.connectionDiagnostic("qr_channel", "none", err, 0, false)
			recordRuntimeFailure("qr_channel", "connection_failed", err)
			_ = r.stop(ctx)
			return archive.ErrSourceUnavailable
		}
	}
	r.queue = make(chan queuedEvent, r.config.BodyQueueCount)
	r.workerDone = make(chan struct{})
	r.qrDone = make(chan struct{})
	r.queueBytes = 0
	r.admissions = 0
	r.waitingBytes = 0
	r.waitingAdmissions = 0
	r.admissionChanged = make(chan struct{})
	r.queueClosed = false
	r.deliveriesDone = nil
	r.deliveryStopDone = nil
	r.connectDone = nil
	r.socketStopDone = nil
	r.mu.Lock()
	r.quiesced = false
	r.qrEnded = false
	r.pairingHandoff = handoff
	r.mu.Unlock()
	r.deliveryMu.Lock()
	r.deliveryStopping = make(chan struct{})
	r.accepting = true
	r.deliveryMu.Unlock()
	callbackContext := r.runCtx
	r.handler = r.connection.AddEventHandler(func(event any) {
		if callbackContext.Err() != nil {
			return
		}
		r.deliverRun(event, callbackRun)
		if _, ok := event.(*events.ManualLoginReconnect); ok {
			r.notePairingHandoff(handoff, false)
		}
	})
	go r.work()
	go r.watchQR(qr, handoff)
	r.startOptional()
	r.mu.Lock()
	r.status.Paused = false
	if pair && r.client.Store.ID == nil {
		r.status.Connection = "pairing"
	} else {
		r.status.Connection = "disconnected"
	}
	r.mu.Unlock()
	r.notify()
	// ConnectContext retains its context for the socket's entire lifetime.
	// Caller cancellation begins an owned drain; it cannot cancel source work
	// that the SDK admitted before its connect call returned.
	r.connectionDiagnostic(connectPhase, "none", nil, 0, false)
	// Policy and observation preparation may block while a manual lifecycle
	// operation withdraws recovery intent before it can acquire op. Recheck
	// authorization at the actual dial boundary, and clean a refused new run.
	r.mu.Lock()
	authorized := connectPhase != "recovery_start" || r.recoveryStartAuthorizedLocked(recoveryRequest)
	if authorized {
		r.connectionAttempts++
	}
	r.mu.Unlock()
	if !authorized {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), recoveryDrainTimeout)
		defer cleanupCancel()
		if cleanupErr := r.stop(cleanupCtx); cleanupErr != nil {
			return cleanupErr
		}
		return r.connectionFailure("recovery_refused", archive.ErrSourceUnavailable)
	}
	r.connectDone = make(chan struct{})
	connectDone := r.connectDone
	connectResult := make(chan error, 1)
	ownedConnection, runCtx := r.connection, r.runCtx
	go func() {
		connectResult <- ownedConnection.ConnectContext(runCtx)
		close(connectDone)
	}()
	select {
	case err = <-connectResult:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		r.connectionDiagnostic(connectPhase, "none", err, 0, false)
		recordRuntimeFailure(connectPhase, "connection_failed", err)
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), recoveryDrainTimeout)
		defer cancelCleanup()
		if cleanupErr := r.stop(cleanupCtx); cleanupErr != nil {
			return cleanupErr
		}
		r.noteStartupRecoveryFailure(connectPhase)
		return archive.ErrSourceUnavailable
	}
	// The SDK releases its socket lock before returning from ConnectContext.
	// A still-pending dial is joined by stop before any socket-state query.
	r.snapshotSocketState()
	r.mu.Lock()
	if r.status.ConnectionDiagnostic.Phase == connectPhase {
		r.status.ConnectionDiagnostic.Phase = "awaiting_authentication"
	}
	r.mu.Unlock()
	r.notify()
	return nil
}
func (r *Runtime) watchQR(qr <-chan whatsmeow.QRChannelItem, handoff *pairingHandoff) {
	defer close(r.qrDone)
	if qr == nil {
		return
	}
	observation := r.observation
	for {
		select {
		case <-r.runCtx.Done():
			return
		case item, ok := <-qr:
			if !ok {
				r.endPairingQR(observation)
				return
			}
			n := QRNotification{Event: item.Event, Timeout: item.Timeout}
			if item.Event == "code" {
				n.Code = item.Code
			} else if item.Event != "success" && item.Event != "timeout" {
				n.Event = "error"
			}
			if r.callbacks.QRChanged != nil {
				r.callbacks.QRChanged(n)
			}
			if item.Event == "success" {
				r.mu.Lock()
				if !r.sessionDeleted {
					r.status.HasSession = true
				}
				r.mu.Unlock()
				r.notePairingHandoff(handoff, true)
				return
			}
			if n.Event == "timeout" {
				r.endPairingQR(observation)
				return
			}
		}
	}
}

func (r *Runtime) notePairingHandoff(handoff *pairingHandoff, success bool) {
	if handoff == nil {
		return
	}
	r.mu.Lock()
	if r.pairingHandoff != handoff || handoff.context.Err() != nil || r.sessionDeleted || r.status.ReconciliationRequired || terminalConnectionEvent(r.status.ConnectionDiagnostic.Event) {
		r.mu.Unlock()
		return
	}
	if success {
		handoff.success = true
	} else {
		handoff.reconnect = true
	}
	ready := handoff.success && handoff.reconnect && !handoff.scheduled
	if ready {
		handoff.scheduled = true
	}
	r.mu.Unlock()
	if ready {
		// SDK dispatch holds its handler lock. Stop/remove must run after return.
		go r.completePairing(handoff.observation)
	}
}

// Only a terminal QR channel ends the attempt. SDK error notifications can be
// nonterminal and a later linking code must still be delivered.
func (r *Runtime) endPairingQR(observation archive.ObservationID) {
	r.mu.Lock()
	if r.observation != observation || r.status.Connection != "pairing" {
		r.mu.Unlock()
		return
	}
	r.qrEnded = true
	r.mu.Unlock()
	if r.callbacks.QRChanged != nil {
		r.callbacks.QRChanged(QRNotification{Event: "disconnected"})
	}
	if r.linkedPairingSession() {
		r.notify()
		return
	}
	r.mu.Lock()
	if r.observation == observation && r.status.Connection == "pairing" && !r.status.HasSession {
		r.status.Connection = "disconnected"
		d := &r.status.ConnectionDiagnostic
		if !terminalConnectionEvent(d.Event) && (d.ErrorClass == "" || d.ErrorClass == "none") {
			d.Phase, d.Event, d.ErrorClass, d.Reason, d.OnConnect = "event", "disconnected", "none", 0, false
		}
	}
	r.mu.Unlock()
	r.notify()
}

func (r *Runtime) linkedPairingSession() bool {
	r.mu.Lock()
	deleted, linked := r.sessionDeleted, r.status.HasSession
	r.mu.Unlock()
	if deleted {
		return false
	}
	if r.client.Store.ID != nil {
		r.mu.Lock()
		if !r.sessionDeleted {
			r.status.HasSession = true
		}
		r.mu.Unlock()
		return true
	}
	if linked {
		return true
	}
	if c, ok := r.connection.(interface{ IsLoggedIn() bool }); ok {
		return c.IsLoggedIn()
	}
	return false
}

// This one post-pair connection belongs to the explicit Pair operation. General
// connector reconnects remain disabled and require an application lifecycle call.
func (r *Runtime) completePairing(observation archive.ObservationID) {
	r.op.Lock()
	defer r.op.Unlock()
	r.mu.Lock()
	refused := r.sessionDeleted || r.status.ReconciliationRequired || terminalConnectionEvent(r.status.ConnectionDiagnostic.Event)
	r.mu.Unlock()
	if r.closed || r.observation != observation || r.client.Store.ID == nil || refused || r.runCtx == nil || r.runCtx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.stop(ctx); err != nil {
		r.connectionDiagnostic("post_pair_stop", "none", err, 0, false)
		return
	}
	// A terminal SDK event may have been admitted before stop disabled delivery
	// and recorded while the owned handler/worker join was in progress.
	r.mu.Lock()
	refused = r.sessionDeleted || r.status.ReconciliationRequired || terminalConnectionEvent(r.status.ConnectionDiagnostic.Event)
	r.mu.Unlock()
	if refused {
		return
	}
	// startConnection retains its typed failure before the existing cleanup.
	_ = r.startConnection(ctx, false, "post_pair_start")
}
func (r *Runtime) stop(ctx context.Context) error {
	r.mu.Lock()
	r.pairingHandoff = nil
	r.mu.Unlock()
	r.stopOptional()
	if r.observation == "" {
		if err := r.joinOptional(ctx); err != nil {
			recordRuntimeFailure("stop_optional", "optional_join_failed", err)
			return err
		}
		r.mu.Lock()
		r.status.Connection = "disconnected"
		r.quiesced = true
		r.mu.Unlock()
		if r.callbacks.QRChanged != nil {
			r.callbacks.QRChanged(QRNotification{Event: "disconnected"})
		}
		r.notify()
		return nil
	}
	r.deliveryMu.Lock()
	if r.accepting {
		r.accepting = false
		if r.deliveryStopping != nil {
			close(r.deliveryStopping)
		}
	}
	r.deliveryMu.Unlock()
	// Accepted history downloads are source work: their remaining records may
	// contain deletions. Keep their context alive while the source worker drains.
	// Optional transfers were canceled above; a failed join below still fences
	// the archive and cancels source work through failClosed.
	if r.deliveryStopDone == nil {
		r.deliveryStopDone = make(chan struct{})
		done := r.deliveryStopDone
		handler := r.handler
		go func() { r.connection.RemoveEventHandler(handler); close(done) }()
	}
	select {
	case <-r.deliveryStopDone:
	case <-ctx.Done():
		r.failClosedAt("stop_handler", "drain_failed", ctx.Err())
		r.startSocketStop()
		return ctx.Err()
	}
	// Remove admission before waiting, so no Add can race with Wait.
	if r.deliveriesDone == nil {
		r.deliveriesDone = make(chan struct{})
		done := r.deliveriesDone
		go func() { r.deliveries.Wait(); close(done) }()
	}
	select {
	case <-r.deliveriesDone:
	case <-ctx.Done():
		r.failClosedAt("stop_deliveries", "reconciliation_required", ctx.Err())
		r.startSocketStop()
		return ctx.Err()
	}
	if r.queue != nil {
		if !r.queueClosed {
			close(r.queue)
			r.queueClosed = true
		}
		select {
		case <-r.workerDone:
		case <-ctx.Done():
			r.failClosedAt("stop_source", "reconciliation_required", ctx.Err())
			r.startSocketStop()
			return ctx.Err()
		}
		r.queue = nil
	}
	// Accepted history may need the socket for media metadata. Cancel its run
	// only after the source worker drains, then interrupt and join an empty or
	// failed startup dial before taking the SDK's socket shutdown lock.
	if r.cancel != nil {
		r.cancel()
	}
	if r.connectDone != nil {
		select {
		case <-r.connectDone:
		case <-ctx.Done():
			r.failClosedAt("stop_connect", "drain_failed", ctx.Err())
			r.startSocketStop()
			return ctx.Err()
		}
	}
	// Retain this cleanup owner across retries if SDK shutdown misses the bound.
	select {
	case <-r.startSocketStop():
	case <-ctx.Done():
		r.failClosedAt("stop_socket", "drain_failed", ctx.Err())
		return ctx.Err()
	}
	if r.qrDone != nil {
		select {
		case <-r.qrDone:
		case <-ctx.Done():
			r.failClosedAt("stop_qr", "reconciliation_required", ctx.Err())
			return ctx.Err()
		}
	}
	if r.callbacks.QRChanged != nil {
		r.callbacks.QRChanged(QRNotification{Event: "disconnected"})
	}
	r.mu.Lock()
	r.status.Connection = "disconnected"
	unsafe := r.status.ReconciliationRequired
	r.mu.Unlock()
	if unsafe {
		if err := r.joinOptional(ctx); err != nil {
			recordRuntimeFailure("stop_optional", "optional_join_failed", err)
			return err
		}
		r.mu.Lock()
		r.quiesced = true
		r.mu.Unlock()
		r.notify()
		return archive.ErrReconciliationRequired
	}
	if account := r.SourceAccount(); account != "" {
		if err := r.store.SetSyncStatus(ctx, archive.SyncUpdate{Account: account, Connection: "disconnected", CoverageUnknown: true}); err != nil {
			r.failClosedAt("stop_sync", "archive_unavailable", err)
			return err
		}
	}
	if drain, ok := r.store.(interface{ Drain(context.Context) error }); ok {
		if err := drain.Drain(ctx); err != nil {
			r.failClosedAt("stop_store", "archive_unavailable", err)
			return err
		}
	}
	if err := r.store.EndObservation(ctx, r.observation); err != nil {
		r.failClosedAt("stop_observation", "reconciliation_required", err)
		return err
	}
	r.observation = ""
	r.queue = nil
	// The source observation ends before an optional-only join. A failed media
	// cleanup must not leave a false lost-source marker after process restart.
	if err := r.joinOptional(ctx); err != nil {
		recordRuntimeFailure("stop_optional", "optional_join_failed", err)
		return err
	}
	r.mu.Lock()
	r.quiesced = true
	r.mu.Unlock()
	r.notify()
	return nil
}

// Lifecycle operations hold r.op while creating or joining this single owner.
func (r *Runtime) startSocketStop() <-chan struct{} {
	if r.socketStopDone == nil {
		r.socketStopDone = make(chan struct{})
		done := r.socketStopDone
		ownedConnection, connectDone := r.connection, r.connectDone
		go func() {
			if connectDone != nil {
				<-connectDone
			}
			ownedConnection.Disconnect()
			close(done)
		}()
	}
	return r.socketStopDone
}

func (r *Runtime) deliver(event any) {
	r.mu.Lock()
	run := r.recoveryRun
	r.mu.Unlock()
	r.deliverRun(event, run)
}

func (r *Runtime) deliverRun(event any, run uint64) {
	r.deliveryMu.Lock()
	if !r.accepting {
		r.deliveryMu.Unlock()
		return
	}
	r.deliveries.Add(1)
	stopping := r.deliveryStopping
	r.deliveryMu.Unlock()
	defer r.deliveries.Done()
	r.mu.Lock()
	currentRun := r.recoveryRun == run
	r.mu.Unlock()
	if !currentRun {
		return
	}
	if r.Status().ReconciliationRequired {
		return
	}
	recoveryEpoch := r.noteRecoverySignal(event, run)
	switch e := event.(type) {
	case *events.Contact, *events.PushName, *events.BusinessName:
		// Cache changes supply optional labels, never authoritative source work.
		// Coalesce without retaining an event or consuming source queue capacity.
		r.wakeSourceNames()
		return
	case *events.Connected:
		r.connectionDiagnostic("event", "connected", nil, 0, false)
	case *events.Disconnected:
		r.connectionDiagnostic("event", "disconnected", nil, 0, false)
	case *events.LoggedOut:
		r.connectionDiagnostic("event", "logged_out", nil, connectionReason(e.Reason), e.OnConnect)
	case *events.ManualLoginReconnect:
		r.connectionDiagnostic("event", "manual_login_reconnect", nil, 0, false)
	case *events.ConnectFailure:
		r.connectionDiagnostic("event", "connect_failure", nil, connectionReason(e.Reason), false)
	case *events.ClientOutdated:
		r.connectionDiagnostic("event", "client_outdated", nil, 0, false)
	case *events.TemporaryBan:
		r.connectionDiagnostic("event", "temporary_ban", nil, 0, false)
	case *events.StreamError:
		r.connectionDiagnostic("event", "stream_error", nil, 0, false)
	case *events.StreamReplaced:
		r.connectionDiagnostic("event", "stream_replaced", nil, 0, false)
	case *events.CATRefreshError:
		r.connectionDiagnostic("event", "cat_refresh_error", nil, 0, false)
	}
	switch e := event.(type) {
	case *events.AppStateSyncError:
		collection, class, syncKind := appStateFailureCodes(e)
		// The SDK rejected this state snapshot. Persist its coverage gap before
		// continuing independently verified messages and received controls.
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		err := r.ensureAccount(ctx)
		if err == nil {
			err = r.store.SetSyncStatus(ctx, archive.SyncUpdate{Account: r.SourceAccount(), Connection: r.Status().Connection, GapCode: "app_state_sync_error", CoverageUnknown: true})
		}
		cancel()
		if err != nil {
			r.failClosedAt("app_state_sync", "archive_unavailable", err, collection, class, syncKind)
			return
		}
		r.mu.Lock()
		found := false
		for _, code := range r.status.Codes {
			found = found || code == "app_state_sync_error"
		}
		if !found && !r.status.ReconciliationRequired {
			// Later connection progress or sync success cannot establish that
			// every missed action was recovered. Retain the first fixed witness.
			r.status.Codes = append(r.status.Codes, collection, class, syncKind, "app_state_sync_error")
		}
		r.mu.Unlock()
		r.notify()
		return
	case *events.UndecryptableMessage:
		event = unreadableMessageGap{}
	case *events.HistorySync:
		r.failClosedAt("history_event", "history_corrupt", nil)
		return
	}
	if m, ok := event.(*events.Message); ok {
		if err := validateMessageStructure(m); err != nil {
			r.failClosedAt("message_structure", "ambiguous_message_structure", err)
			return
		}
		if !recordWithinLimit(m) && m.Message.GetProtocolMessage().GetHistorySyncNotification() == nil {
			r.failClosedAt("message_limit", "history_limit_exceeded", errLimitExceeded)
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	controlled, err := r.applyInvalidation(ctx, event)
	cancel()
	if controlled {
		if err != nil {
			if err == archive.ErrSourceRevisionConflict {
				// The writer committed this message's quarantine and scoped gap.
				// Do not enqueue its content or stop unrelated source messages.
				r.notify()
				return
			}
			if !errors.Is(err, archive.ErrInvalidEvent) {
				r.failClosedAt("control_invalidation", "reconciliation_required", err)
			}
			return
		}
		r.cancelActive()
		r.mu.Lock()
		r.status.Controls++
		r.mu.Unlock()
		m, ok := event.(*events.Message)
		if !ok || mutationProtocol(m).GetType() == waE2E.ProtocolMessage_REVOKE {
			return
		}
	}
	switch event.(type) {
	case *events.Message, *events.Connected, *events.Disconnected, *events.LoggedOut, unreadableMessageGap:
	default:
		return
	}
	size := int64(64)
	if m, ok := event.(*events.Message); ok {
		size += retainedBytes(m)
	}
	r.admit(event, size, stopping, run, recoveryEpoch)
}

type itemBytes int64

// Keep temporary bursts in their existing SDK callbacks rather than dropping
// history envelopes, which may contain deletions after readable message bodies.
// Queued/in-flight and waiting payloads each have the configured count/byte
// bound. These ledgers do not bound memory retained elsewhere by the SDK.
func (r *Runtime) admit(event any, size int64, stopping <-chan struct{}, run, recoveryEpoch uint64) {
	if size > r.config.BodyQueueBytes {
		r.rejectAdmission()
		return
	}
	var timer *time.Timer
	var deadline time.Time
	waiting, interrupted := false, false
	var runDone <-chan struct{}
	if r.runCtx != nil {
		runDone = r.runCtx.Done()
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
		if waiting {
			r.mu.Lock()
			r.waitingAdmissions--
			r.waitingBytes -= size
			r.mu.Unlock()
		}
	}()
	for {
		r.mu.Lock()
		if r.status.ReconciliationRequired {
			r.mu.Unlock()
			return
		}
		if waiting && !time.Now().Before(deadline) {
			r.mu.Unlock()
			r.rejectAdmission()
			return
		}
		if r.admissions < r.config.BodyQueueCount && size <= r.config.BodyQueueBytes-r.queueBytes {
			if waiting {
				r.waitingAdmissions--
				r.waitingBytes -= size
				waiting = false
			}
			r.queueBytes += size
			r.admissions++
			r.mu.Unlock()
			// Admission includes the in-flight item, so the buffered queue has
			// room. A violated internal invariant must still stop the archive.
			select {
			case r.queue <- queuedEvent{event: event, bytes: size, run: run, recoveryEpoch: recoveryEpoch}:
			default:
				r.release(itemBytes(size))
				r.rejectAdmission()
			}
			return
		}
		if interrupted {
			// Stop wakes callbacks before SDK handler removal can wait on them.
			// An accepted envelope that cannot drain is not a clean observation.
			r.mu.Unlock()
			r.rejectAdmission()
			return
		}
		if !waiting {
			if r.waitingAdmissions >= r.config.BodyQueueCount || size > r.config.BodyQueueBytes-r.waitingBytes {
				r.mu.Unlock()
				r.rejectAdmission()
				return
			}
			r.waitingAdmissions++
			r.waitingBytes += size
			waiting = true
			timeout := r.admissionTimeout
			if timeout <= 0 {
				timeout = 30 * time.Second
			}
			deadline = time.Now().Add(timeout)
			timer = time.NewTimer(timeout)
		}
		if r.admissionChanged == nil {
			r.admissionChanged = make(chan struct{})
		}
		changed := r.admissionChanged
		r.mu.Unlock()
		select {
		case <-changed:
		case <-stopping:
			interrupted = true
		case <-runDone:
			interrupted = true
		case <-timer.C:
			interrupted = true
		}
	}
}

func (r *Runtime) rejectAdmission() {
	r.mu.Lock()
	if r.status.ReconciliationRequired {
		r.mu.Unlock()
		return
	}
	r.status.Dropped++
	r.mu.Unlock()
	r.failClosedAt("queue_admission", "queue_limit_exceeded", archive.ErrQueueLimitExceeded)
}

func (r *Runtime) wakeAdmissionLocked() {
	if r.waitingAdmissions != 0 && r.admissionChanged != nil {
		close(r.admissionChanged)
		r.admissionChanged = make(chan struct{})
	}
}

func (r *Runtime) release(bytes itemBytes) {
	r.mu.Lock()
	r.queueBytes -= int64(bytes)
	r.admissions--
	idle := r.admissions == 0
	r.wakeAdmissionLocked()
	r.mu.Unlock()
	if idle {
		r.WakeOriginalDownloads()
	}
}
func (r *Runtime) work() {
	defer close(r.workerDone)
	for item := range r.queue {
		r.consume(item)
	}
}
func (r *Runtime) consume(item queuedEvent) {
	defer r.release(itemBytes(item.bytes))
	r.mu.Lock()
	unsafe := r.status.ReconciliationRequired
	r.mu.Unlock()
	if unsafe {
		return
	}
	var err error
	stage := "source_message"
	switch e := item.event.(type) {
	case *events.Message:
		err = r.messageWithMediaOutcome(r.runCtx, e)
	case unreadableMessageGap:
		stage = "unreadable_account"
		err = r.ensureAccount(r.runCtx)
		if err == nil {
			stage = "unreadable_sync"
			err = r.store.SetSyncStatus(r.runCtx, archive.SyncUpdate{Account: r.SourceAccount(), Connection: r.Status().Connection, GapCode: "undecryptable_message", CoverageUnknown: true})
		}
		if err != nil {
			r.failClosedAt(stage, "archive_unavailable", err)
			return
		}
		r.mu.Lock()
		found := false
		for _, code := range r.status.Codes {
			found = found || code == "undecryptable_message"
		}
		if !found {
			r.status.Codes = append(r.status.Codes, "undecryptable_message")
		}
		r.mu.Unlock()
	case *events.Connected:
		stage = "connected_account"
		err = r.ensureAccount(r.runCtx)
		if err == nil {
			r.mu.Lock()
			r.status.Connection = "connected"
			r.qrEnded = false
			if !r.sessionDeleted {
				r.status.HasSession = true
			}
			r.mu.Unlock()
			stage = "connected_sync"
			err = r.store.SetSyncStatus(r.runCtx, archive.SyncUpdate{Account: r.SourceAccount(), Connection: "connected", CoverageUnknown: true})
		}
		if err == nil {
			r.snapshotSocketState()
			r.connectionDiagnostic("connected", "connected", nil, 0, false)
			r.wakeSourceNames()
			stage = "backfill_request"
			err = r.requestBackfill(r.runCtx)
			r.WakeOriginalDownloads()
		}
	case *events.Disconnected, *events.LoggedOut:
		stage = "disconnected_sync"
		r.mu.Lock()
		r.status.Connection = "disconnected"
		r.mu.Unlock()
		err = r.store.SetSyncStatus(r.runCtx, archive.SyncUpdate{Account: r.SourceAccount(), Connection: "disconnected", GapCode: "offline_gap", CoverageUnknown: true})
		if err == nil {
			if _, disconnected := item.event.(*events.Disconnected); disconnected {
				r.noteConsumedDisconnect(item.run, item.recoveryEpoch)
			}
		}
	}
	if err != nil && !r.recordMediaOutcome(err) && !errors.Is(err, archive.ErrInvalidEvent) && !errors.Is(err, archive.ErrPolicyChanged) {
		r.failClosedAt(stage, "reconciliation_required", err)
	}
	r.notify()
}

func (r *Runtime) recordMediaOutcome(err error) bool {
	var outcome MediaError
	if !errors.As(err, &outcome) || !outcome.valid() {
		return false
	}
	// Optional retention follows a valid source commit. It cannot establish a
	// missed source/control observation or stop scanning later history records.
	r.mu.Lock()
	r.status.MediaCode = outcome.Error()
	r.mu.Unlock()
	return true
}

func (r *Runtime) messageWithMediaOutcome(ctx context.Context, event *events.Message) error {
	err := r.message(ctx, event)
	if err == archive.ErrSourceRevisionConflict {
		// This exact store error is returned after the quarantine transaction
		// commits. The scoped durable gap is the witness; retain no global code.
		return nil
	}
	if r.recordMediaOutcome(err) {
		return nil
	}
	return err
}
func (r *Runtime) cancelActive() {
	r.activeMu.Lock()
	cancel := r.activeCancel
	r.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.cancelOptionalAttempt()
}

// App-state diagnostics contain only closed codes, never SDK names or error text.
func appStateFailureCodes(event *events.AppStateSyncError) (collection, class, syncKind string) {
	collection, class, syncKind = "app_state_collection_unknown", "app_state_error_unknown", "app_state_incremental_sync"
	if event == nil {
		return
	}
	switch event.Name {
	case appstate.WAPatchCriticalBlock:
		collection = "app_state_collection_critical_block"
	case appstate.WAPatchCriticalUnblockLow:
		collection = "app_state_collection_critical_unblock_low"
	case appstate.WAPatchRegularLow:
		collection = "app_state_collection_regular_low"
	case appstate.WAPatchRegularHigh:
		collection = "app_state_collection_regular_high"
	case appstate.WAPatchRegular:
		collection = "app_state_collection_regular"
	}
	var syntax *json.SyntaxError
	var typeError *json.UnmarshalTypeError
	switch {
	case errors.Is(event.Error, appstate.ErrMismatchingLTHash):
		class = "app_state_error_lthash"
	case errors.Is(event.Error, appstate.ErrMismatchingPatchMAC):
		class = "app_state_error_patch_mac"
	case errors.Is(event.Error, appstate.ErrMismatchingContentMAC):
		class = "app_state_error_content_mac"
	case errors.Is(event.Error, appstate.ErrMismatchingIndexMAC):
		class = "app_state_error_index_mac"
	case errors.Is(event.Error, context.Canceled):
		class = "app_state_error_canceled"
	case errors.Is(event.Error, context.DeadlineExceeded):
		class = "app_state_error_deadline"
	case errors.As(event.Error, &syntax):
		class = "app_state_error_json_syntax"
	case errors.As(event.Error, &typeError):
		class = "app_state_error_json_type"
	}
	if event.FullSync {
		syncKind = "app_state_full_sync"
	}
	return
}

func (r *Runtime) failClosed(code string, diagnosticCodes ...string) {
	r.failClosedAt("unknown", code, nil, diagnosticCodes...)
}

func (r *Runtime) failClosedAt(stage, code string, err error, diagnosticCodes ...string) {
	defer recordRuntimeFailure(stage, code, err)
	r.mu.Lock()
	already := r.status.ReconciliationRequired
	if !already {
		r.status.Codes = append(r.status.Codes, diagnosticCodes...)
	}
	r.status.ReconciliationRequired = true
	r.invalidateRecoveryLocked()
	r.status.Connection = "reconciliation_required"
	found := false
	for _, v := range r.status.Codes {
		if v == code {
			found = true
		}
	}
	if !found {
		r.status.Codes = append(r.status.Codes, code)
	}
	r.wakeAdmissionLocked()
	r.mu.Unlock()
	r.cancelActive()
	if r.cancel != nil {
		r.cancel()
	}
	if already {
		return
	}
	reason := code
	switch code {
	case "archive_unavailable":
		reason = "control_admission_failed"
	case "reconciliation_required":
		reason = "drain_failed"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := r.store.MarkUnsafe(ctx, r.SourceAccount(), archive.UnsafeReason(reason)); err != nil {
		r.mu.Lock()
		r.status.Codes = append(r.status.Codes, "archive_unavailable")
		r.mu.Unlock()
	}
	if r.SourceAccount() != "" {
		_ = r.store.SetSyncStatus(ctx, archive.SyncUpdate{Account: r.SourceAccount(), GapCode: reason, CoverageUnknown: true})
	}
	r.notify()
}

// Sum roots conservatively: whatsmeow retains the raw/source protobuf alongside
// the unwrapped body. Shared graph bytes may count twice; none may be uncharged.
func retainedBytes(e *events.Message) int64 {
	return int64(proto.Size(e.Message) + proto.Size(e.RawMessage) + proto.Size(e.SourceWebMsg) + len(e.Info.ID) + len(e.Info.PushName) + len(e.UnavailableRequestID))
}
func recordWithinLimit(e *events.Message) bool {
	return proto.Size(e.Message) <= recordMax && proto.Size(e.RawMessage) <= recordMax && proto.Size(e.SourceWebMsg) <= recordMax
}
