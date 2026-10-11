package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
)

func TestConnectionIntentDefaultsOffAndPreservesPrivateState(t *testing.T) {
	var state applicationState
	if err := json.Unmarshal([]byte(`{"version":1,"mode":"real","paused":true,"remote_clients":{"unknown":"retain"}}`), &state); err != nil {
		t.Fatal(err)
	}
	if state.DesiredConnected {
		t.Fatal("legacy state enabled automatic connection")
	}
	state.DesiredConnected = true
	encoded, err := encodeState(state)
	if err != nil {
		t.Fatal(err)
	}
	var reopened applicationState
	if err = json.Unmarshal(encoded, &reopened); err != nil {
		t.Fatal(err)
	}
	if !reopened.DesiredConnected || !reopened.Paused || !reopened.RemoteInvalid {
		t.Fatal("connection intent or unrelated private state did not survive")
	}
	var wire map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["remote_clients"]) != `{"unknown":"retain"}` {
		t.Fatal("connection persistence changed credential recovery bytes")
	}
}

// This fixture exposes only synthetic account metadata. It never creates a real
// WhatsApp session, starts an SDK client or loads an existing user's database.
type connectionRecoveryFixture struct {
	mu                       sync.Mutex
	account                  string
	status                   ingestion.Status
	connectCalls             int
	connectStarted           chan struct{}
	blockConnect             bool
	canceled                 chan struct{}
	request                  ingestion.RecoveryRequest
	recoveryCalls            int
	recoveryStarts           []time.Time
	recoveryStarted          chan struct{}
	blockRecovery            bool
	recoveryCanceled         chan struct{}
	recoveryError            error
	disconnectCheck          func() error
	resumeFailure            error
	closeHook                func()
	hideRequestAfterRecovery bool
	requestHidden            bool
}

func (f *connectionRecoveryFixture) SourceAccount() string { return f.account }
func (f *connectionRecoveryFixture) Status() ingestion.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}
func (f *connectionRecoveryFixture) Quiesced() bool { return true }
func (f *connectionRecoveryFixture) Close() error {
	f.mu.Lock()
	hook := f.closeHook
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}
func (f *connectionRecoveryFixture) Pair(ctx context.Context) error { return f.Connect(ctx) }
func (f *connectionRecoveryFixture) Connect(ctx context.Context) error {
	f.mu.Lock()
	f.connectCalls++
	if f.connectStarted != nil {
		select {
		case f.connectStarted <- struct{}{}:
		default:
		}
	}
	block := f.blockConnect
	if !block {
		f.status.Connection = "connected"
		f.status.Paused = false
	}
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		if f.canceled != nil {
			select {
			case f.canceled <- struct{}{}:
			default:
			}
		}
		return ctx.Err()
	}
	return nil
}
func (f *connectionRecoveryFixture) Pause(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.Paused = true
	f.status.Connection = "paused"
	return nil
}
func (f *connectionRecoveryFixture) Resume(ctx context.Context) error {
	f.mu.Lock()
	err := f.resumeFailure
	if err != nil {
		f.status.Connection = "disconnected"
		f.status.Paused = false
		f.status.ConnectionDiagnostic = ingestion.ConnectionDiagnostic{Event: "none", ErrorClass: "network_timeout"}
		f.request = ingestion.RecoveryRequest{Token: 1, Reason: "startup_failure"}
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.Connect(ctx)
}
func (f *connectionRecoveryFixture) Disconnect(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.disconnectCheck != nil {
		if err := f.disconnectCheck(); err != nil {
			return err
		}
	}
	f.status.Connection = "disconnected"
	return nil
}
func (f *connectionRecoveryFixture) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connectCalls
}
func (f *connectionRecoveryFixture) RecoveryRequest() (ingestion.RecoveryRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.request, f.request.Token != 0 && !f.requestHidden
}
func (f *connectionRecoveryFixture) Recover(ctx context.Context, request ingestion.RecoveryRequest) (bool, error) {
	f.mu.Lock()
	if request != f.request {
		f.mu.Unlock()
		return false, nil
	}
	f.recoveryCalls++
	f.recoveryStarts = append(f.recoveryStarts, time.Now())
	if !f.blockRecovery && f.recoveryError == nil && f.hideRequestAfterRecovery {
		f.requestHidden = true
	}
	if f.recoveryStarted != nil {
		select {
		case f.recoveryStarted <- struct{}{}:
		default:
		}
	}
	block, err := f.blockRecovery, f.recoveryError
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		if f.recoveryCanceled != nil {
			select {
			case f.recoveryCanceled <- struct{}{}:
			default:
			}
		}
		return true, ctx.Err()
	}
	return true, err
}
func (f *connectionRecoveryFixture) recoveries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recoveryCalls
}

func connectionRecoveryFactory(f *connectionRecoveryFixture) runtimeFactory {
	return func(ingestion.Config, archive.ConnectorStore, ingestion.Callbacks) (connector, error) { return f, nil }
}

func prepareConnectionRecoveryArchive(t *testing.T, root string, desired, paused, configured, unsafe bool) {
	t.Helper()
	ctx := context.Background()
	a, err := newApplication(ctx, Config{DataDir: root, Offline: true}, connectionRecoveryFactory(&connectionRecoveryFixture{}))
	if err != nil {
		t.Fatal(err)
	}
	if configured {
		if _, err = a.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err = a.store.SetSyncStatus(ctx, archive.SyncUpdate{Account: "synthetic-account", Connection: "disconnected", CoverageUnknown: true}); err != nil {
		t.Fatal(err)
	}
	if unsafe {
		if err = a.store.MarkUnsafe(ctx, "synthetic-account", "drain_failed"); err != nil {
			t.Fatal(err)
		}
	}
	state := a.authState
	state.DesiredConnected, state.Paused = desired, paused
	if err = writeState(a.paths, a.lease, state); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSavedSessionConnectionStartupRequiresExplicitIntent(t *testing.T) {
	for _, test := range []struct {
		name                                                  string
		desired, paused, configured, unsafe, session, offline bool
		want                                                  bool
	}{
		{"saved_intent", true, false, true, false, true, false, true},
		{"legacy_missing_intent", false, false, true, false, true, false, false},
		{"paused", true, true, true, false, true, false, false},
		{"unconfigured", true, false, false, false, true, false, false},
		{"unsafe", true, false, true, true, true, false, false},
		{"session_missing", true, false, true, false, false, false, false},
		{"offline", true, false, true, false, true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := privateTestRoot(t)
			prepareConnectionRecoveryArchive(t, root, test.desired, test.paused, test.configured, test.unsafe)
			f := &connectionRecoveryFixture{account: "synthetic-account", status: ingestion.Status{Connection: "disconnected", HasSession: test.session}, connectStarted: make(chan struct{}, 1)}
			a, err := newApplication(context.Background(), Config{DataDir: root, Offline: test.offline}, connectionRecoveryFactory(f))
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close(context.Background())
			if test.want {
				select {
				case <-f.connectStarted:
				case <-time.After(time.Second):
					t.Fatal("saved opted-in linked session did not connect asynchronously")
				}
			} else {
				select {
				case <-f.connectStarted:
					t.Fatal("startup connected without all guards")
				case <-time.After(30 * time.Millisecond):
				}
			}
		})
	}
}

func TestManualDisconnectClearsPersistedConnectionIntentBeforeRestart(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	prepareConnectionRecoveryArchive(t, root, false, false, true, false)
	f := &connectionRecoveryFixture{account: "synthetic-account", status: ingestion.Status{Connection: "disconnected", HasSession: true}, connectStarted: make(chan struct{}, 4)}
	a, err := newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.StartPairing(ctx); err != nil {
		t.Fatal(err)
	}
	state, _, err := readState(a.paths, a.lease)
	if err != nil || !state.DesiredConnected {
		t.Fatal("explicit Connect did not persist desired connection")
	}
	if err = a.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	state, _, err = readState(a.paths, a.lease)
	if err != nil || state.DesiredConnected {
		t.Fatal("manual Disconnect retained automatic connection intent")
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	before := f.calls()
	a, err = newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	time.Sleep(30 * time.Millisecond)
	if f.calls() != before {
		t.Fatal("restart undid manual Disconnect")
	}
}

func TestPauseCancelsStartupAttemptBeforeWaitingForLifecycle(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	prepareConnectionRecoveryArchive(t, root, true, false, true, false)
	f := &connectionRecoveryFixture{account: "synthetic-account", status: ingestion.Status{Connection: "disconnected", HasSession: true}, connectStarted: make(chan struct{}, 1), blockConnect: true, canceled: make(chan struct{}, 1)}
	a, err := newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	select {
	case <-f.connectStarted:
	case <-time.After(time.Second):
		t.Fatal("startup connection did not begin")
	}
	pauseCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err = a.Pause(pauseCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.canceled:
	default:
		t.Fatal("Pause did not cancel and join pending connection")
	}
	state, _, err := readState(a.paths, a.lease)
	if err != nil || !state.Paused || !state.DesiredConnected {
		t.Fatal("Pause failed to preserve intent while persisting paused state")
	}
	if err = a.StartPairing(ctx); !errors.Is(err, archive.ErrInvalidPolicy) {
		t.Fatal("paused application connected")
	}
}

func TestConnectionRecoveryCoalescesSignalsAndCapsBackoff(t *testing.T) {
	root := privateTestRoot(t)
	prepareConnectionRecoveryArchive(t, root, false, false, true, false)
	f := &connectionRecoveryFixture{account: "synthetic-account", status: ingestion.Status{Connection: "disconnected", HasSession: true}, recoveryStarted: make(chan struct{}, 8), recoveryError: archive.ErrSourceUnavailable}
	a, err := newApplication(context.Background(), Config{DataDir: root}, connectionRecoveryFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	a.connectionOwner.settings = connectionRecoverySettings{pollInterval: time.Millisecond, attemptTimeout: time.Second, backoff: []time.Duration{2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond, 16 * time.Millisecond, 32 * time.Millisecond}, maxAttempts: 5}
	if err = a.StartPairing(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status.Connection = "disconnected"
	f.status.ConnectionDiagnostic = ingestion.ConnectionDiagnostic{Event: "none", ErrorClass: "network_timeout"}
	f.request = ingestion.RecoveryRequest{Token: 1, Reason: "disconnected"}
	f.mu.Unlock()
	for i := 0; i < 128; i++ {
		a.wakeConnectionRecovery()
	}
	for i := 0; i < 5; i++ {
		select {
		case <-f.recoveryStarted:
		case <-time.After(time.Second):
			t.Fatal("bounded transient recovery did not run")
		}
	}
	for i := 0; i < 128; i++ {
		a.wakeConnectionRecovery()
	}
	time.Sleep(40 * time.Millisecond)
	if f.recoveries() != 5 {
		t.Fatal("duplicate signals reset exhausted retry budget")
	}
	f.mu.Lock()
	starts := append([]time.Time(nil), f.recoveryStarts...)
	f.mu.Unlock()
	for i, delay := range []time.Duration{4 * time.Millisecond, 8 * time.Millisecond, 16 * time.Millisecond, 32 * time.Millisecond} {
		if starts[i+1].Sub(starts[i]) < delay {
			t.Fatal("retry bypassed its increasing backoff")
		}
	}
}

func TestConnectionRecoveryCancellationJoinsBeforeArchiveRelease(t *testing.T) {
	for _, action := range []string{"pause", "disconnect", "close", "delete"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			root := privateTestRoot(t)
			prepareConnectionRecoveryArchive(t, root, false, false, true, false)
			f := &connectionRecoveryFixture{account: "synthetic-account", status: ingestion.Status{Connection: "disconnected", HasSession: true}, recoveryStarted: make(chan struct{}, 1), blockRecovery: true, recoveryCanceled: make(chan struct{}, 1)}
			a, err := newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close(ctx)
			a.connectionOwner.settings = connectionRecoverySettings{pollInterval: time.Millisecond, attemptTimeout: time.Minute, backoff: []time.Duration{time.Millisecond}, maxAttempts: 5}
			if err = a.StartPairing(ctx); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.status.Connection = "disconnected"
			f.request = ingestion.RecoveryRequest{Token: 1, Reason: "disconnected"}
			f.mu.Unlock()
			a.wakeConnectionRecovery()
			select {
			case <-f.recoveryStarted:
			case <-time.After(time.Second):
				t.Fatal("recovery did not start")
			}
			operationCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			switch action {
			case "pause":
				err = a.Pause(operationCtx)
			case "disconnect":
				err = a.Disconnect(operationCtx)
			case "close":
				err = a.Close(operationCtx)
			case "delete":
				err = a.DeleteLocalData(operationCtx, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-f.recoveryCanceled:
			default:
				t.Fatal("lifecycle operation returned before attempt canceled and joined")
			}
			if action == "close" && !a.cleanupComplete {
				t.Fatal("close did not release joined resources")
			}
			if f.recoveries() != 1 {
				t.Fatal("canceled recovery restarted")
			}
		})
	}
}

func TestConnectionRecoveryTerminalCausePersistsSuppressionAcrossRestart(t *testing.T) {
	for _, event := range []string{"logged_out", "connect_failure", "client_outdated", "temporary_ban", "stream_error", "stream_replaced", "cat_refresh_error"} {
		t.Run(event, func(t *testing.T) {
			ctx := context.Background()
			root := privateTestRoot(t)
			prepareConnectionRecoveryArchive(t, root, false, false, true, false)
			f := &connectionRecoveryFixture{account: "synthetic-account", status: ingestion.Status{Connection: "disconnected", HasSession: true}, connectStarted: make(chan struct{}, 4)}
			a, err := newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
			if err != nil {
				t.Fatal(err)
			}
			if err = a.StartPairing(ctx); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.status.ConnectionDiagnostic = ingestion.ConnectionDiagnostic{Event: event, ErrorClass: "network_timeout"}
			f.request = ingestion.RecoveryRequest{Token: 1, Reason: "disconnected"}
			f.mu.Unlock()
			// Close races the callback wake deliberately: graceful shutdown must
			// persist the current terminal cause even if the owner has not run.
			a.wakeConnectionRecovery()
			if err = a.Close(ctx); err != nil {
				t.Fatal(err)
			}
			before := f.calls()
			f.mu.Lock()
			f.status.ConnectionDiagnostic = ingestion.ConnectionDiagnostic{}
			f.mu.Unlock()
			a, err = newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close(ctx)
			state, _, err := readState(a.paths, a.lease)
			if err != nil || state.DesiredConnected {
				t.Fatal("terminal cause did not suppress persisted startup intent")
			}
			time.Sleep(10 * time.Millisecond)
			if f.calls() != before || f.recoveries() != 0 {
				t.Fatal("terminal cause automatically recovered")
			}
		})
	}
}

func TestManualDisconnectPersistsIntentBeforeConnectorFailure(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	prepareConnectionRecoveryArchive(t, root, false, false, true, false)
	f := &connectionRecoveryFixture{account: "synthetic-account", status: ingestion.Status{Connection: "disconnected", HasSession: true}}
	a, err := newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { f.mu.Lock(); f.disconnectCheck = nil; f.mu.Unlock(); a.Close(ctx) }()
	if err = a.StartPairing(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.disconnectCheck = func() error {
		state, _, readErr := readState(a.paths, a.lease)
		if readErr != nil || state.DesiredConnected {
			t.Error("connector touched before durable manual disconnect suppression")
		}
		return archive.ErrSourceUnavailable
	}
	f.mu.Unlock()
	if err = a.Disconnect(ctx); !errors.Is(err, archive.ErrSourceUnavailable) {
		t.Fatal("synthetic drain failure changed")
	}
	state, _, err := readState(a.paths, a.lease)
	if err != nil || state.DesiredConnected {
		t.Fatal("network drain failure restored connection intent")
	}
}

func TestUnpausedResumeTransientFailureStartsBoundedRecovery(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	prepareConnectionRecoveryArchive(t, root, false, false, true, false)
	f := &connectionRecoveryFixture{account: "synthetic-account", status: ingestion.Status{Connection: "disconnected", HasSession: true}, resumeFailure: archive.ErrSourceUnavailable, recoveryStarted: make(chan struct{}, 1)}
	a, err := newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	a.connectionOwner.settings = connectionRecoverySettings{pollInterval: time.Millisecond, attemptTimeout: time.Second, backoff: []time.Duration{time.Millisecond}, maxAttempts: 5}
	if err = a.Resume(ctx); !errors.Is(err, archive.ErrSourceUnavailable) {
		t.Fatal("explicit transient Resume error changed")
	}
	select {
	case <-f.recoveryStarted:
	case <-time.After(time.Second):
		t.Fatal("unpaused transient Resume had no bounded recovery owner")
	}
	state, _, err := readState(a.paths, a.lease)
	if err != nil || state.Paused || !state.DesiredConnected {
		t.Fatal("explicit unpaused Resume lost its durable choice")
	}
}

func TestClosePersistsTerminalSuppressionAfterCallerExpiresDuringDrain(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	prepareConnectionRecoveryArchive(t, root, false, false, true, false)
	f := &connectionRecoveryFixture{account: "synthetic-account", status: ingestion.Status{Connection: "disconnected", HasSession: true}}
	a, err := newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.StartPairing(ctx); err != nil {
		t.Fatal(err)
	}
	// Isolate Close from the owner so only the joined shutdown lane can commit
	// this new terminal witness, even if the caller expires during that drain.
	if err = a.stopConnectionRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	closeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	f.mu.Lock()
	f.status.ConnectionDiagnostic = ingestion.ConnectionDiagnostic{Event: "client_outdated", ErrorClass: "none"}
	f.closeHook = cancel
	f.mu.Unlock()
	_ = a.Close(closeCtx)
	if closeCtx.Err() == nil {
		t.Fatal("synthetic joined drain did not expire caller")
	}
	// Finish any remaining resource cleanup with a live caller context. The
	// joined runtime's terminal metadata must not have been discarded first.
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status.ConnectionDiagnostic = ingestion.ConnectionDiagnostic{}
	f.closeHook = nil
	f.mu.Unlock()
	before := f.calls()
	a, err = newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	state, _, err := readState(a.paths, a.lease)
	if err != nil || state.DesiredConnected {
		t.Fatal("caller expiry discarded terminal startup suppression")
	}
	time.Sleep(10 * time.Millisecond)
	if f.calls() != before {
		t.Fatal("caller expiry allowed terminal startup reconnect")
	}
}

func TestConnectionRecoveryJoinCancelsPendingDataLockAcquisition(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	prepareConnectionRecoveryArchive(t, root, false, false, true, false)
	f := &connectionRecoveryFixture{account: "synthetic-account", status: ingestion.Status{Connection: "disconnected", HasSession: true}}
	a, err := newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	if err = a.StartPairing(ctx); err != nil {
		t.Fatal(err)
	}
	if err = a.stopConnectionRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate a lifecycle/data writer still holding the boundary. A canceled
	// owner must join without waiting for that unrelated operation to release.
	a.data.Lock()
	a.startConnectionRecovery(false)
	time.Sleep(20 * time.Millisecond)
	stopCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	err = a.stopConnectionRecovery(stopCtx)
	cancel()
	a.data.Unlock()
	if err != nil {
		t.Fatal("owner cancellation waited behind an uncancelable data lock")
	}
}

func TestStartupFailureRecoveryBudgetCountsInitialAttempt(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	prepareConnectionRecoveryArchive(t, root, false, false, true, false)
	f := &connectionRecoveryFixture{account: "synthetic-account", status: ingestion.Status{Connection: "disconnected", HasSession: true}, recoveryStarted: make(chan struct{}, 8), recoveryError: archive.ErrSourceUnavailable}
	a, err := newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	a.connectionOwner.settings = connectionRecoverySettings{pollInterval: time.Millisecond, attemptTimeout: time.Second, backoff: []time.Duration{time.Millisecond}, maxAttempts: 5}
	if err = a.StartPairing(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status.Connection = "disconnected"
	f.status.ConnectionDiagnostic = ingestion.ConnectionDiagnostic{Event: "none", ErrorClass: "network_timeout"}
	f.request = ingestion.RecoveryRequest{Token: 1, Reason: "startup_failure"}
	f.mu.Unlock()
	a.wakeConnectionRecovery()
	for i := 0; i < 4; i++ {
		select {
		case <-f.recoveryStarted:
		case <-time.After(time.Second):
			t.Fatal("startup transient recovery stopped before its bounded budget")
		}
	}
	for i := 0; i < 128; i++ {
		a.wakeConnectionRecovery()
	}
	time.Sleep(20 * time.Millisecond)
	if f.recoveries() != 4 {
		t.Fatal("startup failure exceeded five attempts including its initial Connect")
	}
}

func TestConnectionRecoveryNilDialWithActiveWitnessKeepsBoundedBudget(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	prepareConnectionRecoveryArchive(t, root, false, false, true, false)
	f := &connectionRecoveryFixture{account: "synthetic-account", status: ingestion.Status{Connection: "disconnected", HasSession: true}, recoveryStarted: make(chan struct{}, 8)}
	a, err := newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	a.connectionOwner.settings = connectionRecoverySettings{pollInterval: time.Millisecond, attemptTimeout: time.Second, backoff: []time.Duration{time.Millisecond}, maxAttempts: 5}
	if err = a.StartPairing(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status.Connection = "disconnected"
	f.request = ingestion.RecoveryRequest{Token: 1, Reason: "disconnected"}
	f.mu.Unlock()
	a.wakeConnectionRecovery()
	// The connector's nil dial result does not retire a durable disconnection
	// that happened before return; the same witness must retain its finite cap.
	for i := 0; i < 5; i++ {
		select {
		case <-f.recoveryStarted:
		case <-time.After(time.Second):
			t.Fatal("nil dial retired an unresolved durable recovery witness")
		}
	}
	for i := 0; i < 128; i++ {
		a.wakeConnectionRecovery()
	}
	time.Sleep(20 * time.Millisecond)
	if f.recoveries() != 5 {
		t.Fatal("nil dial reset the active witness budget")
	}
}

func TestConnectionRecoveryNilDialDelayedWitnessRetainsBudget(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	prepareConnectionRecoveryArchive(t, root, false, false, true, false)
	f := &connectionRecoveryFixture{account: "synthetic-account", status: ingestion.Status{Connection: "disconnected", HasSession: true}, recoveryStarted: make(chan struct{}, 8), hideRequestAfterRecovery: true}
	a, err := newApplication(ctx, Config{DataDir: root}, connectionRecoveryFactory(f))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	a.connectionOwner.settings = connectionRecoverySettings{pollInterval: time.Millisecond, attemptTimeout: time.Second, backoff: []time.Duration{time.Millisecond}, maxAttempts: 5}
	if err = a.StartPairing(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status.Connection = "disconnected"
	f.request = ingestion.RecoveryRequest{Token: 1, Reason: "disconnected"}
	f.mu.Unlock()
	a.wakeConnectionRecovery()
	for i := 0; i < 5; i++ {
		select {
		case <-f.recoveryStarted:
		case <-time.After(time.Second):
			t.Fatal("delayed same-token durable witness was retired after nil dial")
		}
		// A nil dial awaiting source progress must remain inert while the
		// runtime reports no eligible durable disconnection witness.
		time.Sleep(5 * time.Millisecond)
		if f.recoveries() != i+1 {
			t.Fatal("nil dial retried without a ready runtime witness")
		}
		f.mu.Lock()
		f.requestHidden = false
		f.mu.Unlock()
		a.wakeConnectionRecovery()
	}
	time.Sleep(20 * time.Millisecond)
	if f.recoveries() != 5 {
		t.Fatal("delayed source commits replenished a no-progress retry budget")
	}
}
