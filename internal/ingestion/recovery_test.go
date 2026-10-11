package ingestion

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func awaitRecoveryRequest(t *testing.T, r *Runtime) RecoveryRequest {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if request, ok := r.RecoveryRequest(); ok {
			if request.Token == 0 {
				t.Fatal("recovery witness has no runtime identity")
			}
			return request
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("consumed recovery signal did not produce a guarded witness")
		}
	}
}

func recoveryConnectionCalls(c *fakeConnection) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectCalls
}

func releaseRecoveryBarrier(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func recoveryFixture(t *testing.T, store *recordingStore) (*Runtime, *handoffConnection) {
	t.Helper()
	r := testRuntime(store)
	c := &handoffConnection{refreshConnection: &refreshConnection{fakeConnection: &fakeConnection{}}, activity: make(chan string, 16)}
	r.connection = c
	t.Cleanup(func() { _ = r.Close() })
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitHandoffActivity(t, c, "connect")
	return r, c
}

type recoveryDrainStore struct {
	*recordingStore
	bodyStarted, deleteStarted, offlineStarted chan struct{}
	bodyRelease, deleteRelease, offlineRelease chan struct{}
	bodyOnce, deleteOnce, offlineOnce          sync.Once
	drainErr                                   error
}

func (s *recoveryDrainStore) ApplySourceEvent(ctx context.Context, event archive.SourceEvent, generation archive.Generation) (archive.ApplyResult, error) {
	if event.Kind == archive.EventOriginal && s.bodyStarted != nil {
		s.bodyOnce.Do(func() { close(s.bodyStarted) })
		<-s.bodyRelease
	}
	if event.Kind == archive.EventDelete && s.deleteStarted != nil {
		s.deleteOnce.Do(func() { close(s.deleteStarted) })
		<-s.deleteRelease
	}
	return s.recordingStore.ApplySourceEvent(ctx, event, generation)
}

func (s *recoveryDrainStore) SetSyncStatus(ctx context.Context, update archive.SyncUpdate) error {
	if update.GapCode == "offline_gap" && s.offlineStarted != nil {
		s.offlineOnce.Do(func() { close(s.offlineStarted) })
		<-s.offlineRelease
	}
	return s.recordingStore.SetSyncStatus(ctx, update)
}

func (s *recoveryDrainStore) Drain(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.order = append(s.order, "drain")
	s.mu.Unlock()
	return s.drainErr
}

func (s *recoveryDrainStore) EndObservation(ctx context.Context, observation archive.ObservationID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.recordingStore.EndObservation(ctx, observation)
}

func TestRecoveryWitnessWaitsForDurableOfflineGap(t *testing.T) {
	base := fixtureStore()
	s := &recoveryDrainStore{recordingStore: base, offlineStarted: make(chan struct{}), offlineRelease: make(chan struct{})}
	r, c := recoveryFixture(t, base)
	r.store = s
	t.Cleanup(func() { releaseRecoveryBarrier(s.offlineRelease) })
	h := c.currentHandler(t)
	h(&events.Disconnected{})
	select {
	case <-s.offlineStarted:
	case <-time.After(time.Second):
		t.Fatal("disconnection did not reach the durable offline gap barrier")
	}
	if _, ok := r.RecoveryRequest(); ok {
		t.Fatal("runtime offered recovery before the offline gap committed")
	}
	close(s.offlineRelease)
	request := awaitRecoveryRequest(t, r)
	if request.Reason != "disconnected" {
		t.Fatal("runtime did not preserve the finite disconnect recovery reason")
	}
	base.mu.Lock()
	gap := slices.ContainsFunc(base.syncs, func(u archive.SyncUpdate) bool { return u.GapCode == "offline_gap" && u.CoverageUnknown })
	base.mu.Unlock()
	if !gap || recoveryConnectionCalls(c.fakeConnection) != 1 {
		t.Fatal("recovery witness reset coverage or dialed from the source worker")
	}
}

func TestRecoveryDrainsAcceptedSourceAndDeletionBeforeNewObservation(t *testing.T) {
	base := fixtureStore()
	s := &recoveryDrainStore{recordingStore: base, bodyStarted: make(chan struct{}), bodyRelease: make(chan struct{}), deleteStarted: make(chan struct{}), deleteRelease: make(chan struct{})}
	r, c := recoveryFixture(t, base)
	r.store = s
	t.Cleanup(func() { releaseRecoveryBarrier(s.bodyRelease); releaseRecoveryBarrier(s.deleteRelease) })
	h := c.currentHandler(t)
	h(&events.Disconnected{})
	request := awaitRecoveryRequest(t, r)
	h(liveMessage("recovery-body", "synthetic-recovery-source"))
	select {
	case <-s.bodyStarted:
	case <-time.After(time.Second):
		t.Fatal("accepted source did not reach its commit barrier")
	}
	controlDone := make(chan struct{})
	go func() {
		h(&events.DeleteForMe{ChatJID: types.NewJID("200", types.DefaultUserServer), SenderJID: types.NewJID("200", types.DefaultUserServer), MessageID: "recovery-delete", Timestamp: time.Unix(2000, 0)})
		close(controlDone)
	}()
	select {
	case <-s.deleteStarted:
	case <-time.After(time.Second):
		t.Fatal("accepted deletion did not reach the independent control barrier")
	}
	result := make(chan error, 1)
	go func() {
		started, err := r.Recover(context.Background(), request)
		if err == nil && !started {
			err = errors.New("guarded recovery did not start")
		}
		result <- err
	}()
	waitHandoffActivity(t, c, "disconnect")
	base.mu.Lock()
	order := append([]string(nil), base.order...)
	base.mu.Unlock()
	if slices.Contains(order, "end") || recoveryConnectionCalls(c.fakeConnection) != 1 {
		t.Fatal("recovery ended the observation or dialed while accepted source/control work remained")
	}
	close(s.bodyRelease)
	close(s.deleteRelease)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("guarded recovery did not join source/control work")
	}
	<-controlDone
	waitHandoffActivity(t, c, "connect")
	base.mu.Lock()
	order = append([]string(nil), base.order...)
	eventsApplied := append([]archive.SourceEvent(nil), base.events...)
	base.mu.Unlock()
	if !slices.Equal(order, []string{"begin", "apply", "apply", "drain", "end", "begin"}) || len(eventsApplied) != 2 || !slices.ContainsFunc(eventsApplied, func(e archive.SourceEvent) bool { return e.Kind == archive.EventDelete }) {
		t.Fatalf("joined recovery omitted or reordered accepted work: %v", order)
	}
	if started, err := r.Recover(context.Background(), request); started || err != nil || recoveryConnectionCalls(c.fakeConnection) != 2 {
		t.Fatal("duplicate consumed recovery witness dialed again")
	}
}

func TestRecoveryRepeatsAllRefusalGatesAfterJoinedStop(t *testing.T) {
	for _, cause := range []string{"logged_out", "stream_replaced", "temporary_ban", "client_outdated", "connect_failure", "stream_error", "cat_refresh_error", "unsafe", "paused", "closed", "session_deleted", "explicit_stop"} {
		t.Run(cause, func(t *testing.T) {
			r, c := recoveryFixture(t, fixtureStore())
			c.currentHandler(t)(&events.Disconnected{})
			request := awaitRecoveryRequest(t, r)
			c.stopHook = func() {
				if terminalConnectionEvent(cause) || cause == "cat_refresh_error" {
					r.connectionDiagnostic("event", cause, nil, 0, false)
					return
				}
				r.mu.Lock()
				switch cause {
				case "unsafe":
					r.status.ReconciliationRequired = true
				case "paused":
					r.status.Paused = true
				case "closed":
					r.closed = true
				case "session_deleted":
					r.sessionDeleted = true
					r.status.HasSession = false
				case "explicit_stop":
					r.recoveryDesired = false
				}
				r.mu.Unlock()
			}
			started, err := r.Recover(context.Background(), request)
			if started || (err != nil && !errors.Is(err, archive.ErrReconciliationRequired)) || recoveryConnectionCalls(c.fakeConnection) != 1 {
				t.Fatalf("post-drain refusal %q authorized another dial", cause)
			}
			c.stopHook = nil
			r.mu.Lock()
			r.closed = false
			r.mu.Unlock()
		})
	}
}

func TestRecoveryQueuedDisconnectCannotOverrideAcceptedConnected(t *testing.T) {
	base := fixtureStore()
	s := &recoveryDrainStore{recordingStore: base, offlineStarted: make(chan struct{}), offlineRelease: make(chan struct{})}
	r, c := recoveryFixture(t, base)
	r.store = s
	t.Cleanup(func() { releaseRecoveryBarrier(s.offlineRelease) })
	h := c.currentHandler(t)
	h(&events.Disconnected{})
	select {
	case <-s.offlineStarted:
	case <-time.After(time.Second):
		t.Fatal("disconnect gap barrier did not start")
	}
	h(&events.Connected{})
	close(s.offlineRelease)
	waitRefreshConnection(t, r, "connected")
	if _, ok := r.RecoveryRequest(); ok || recoveryConnectionCalls(c.fakeConnection) != 1 {
		t.Fatal("queued disconnect recreated recovery after an accepted Connected signal")
	}
}

func TestRecoveryRefusesPausedClosedDeletedUnsafeAndExplicitStop(t *testing.T) {
	for _, cause := range []string{"paused", "closed", "session_deleted", "unsafe", "explicit_stop"} {
		t.Run(cause, func(t *testing.T) {
			r, c := recoveryFixture(t, fixtureStore())
			c.currentHandler(t)(&events.Disconnected{})
			request := awaitRecoveryRequest(t, r)
			r.mu.Lock()
			switch cause {
			case "paused":
				r.status.Paused = true
			case "closed":
				r.closed = true
			case "session_deleted":
				r.sessionDeleted = true
			case "unsafe":
				r.status.ReconciliationRequired = true
			case "explicit_stop":
				r.recoveryDesired = false
			}
			r.mu.Unlock()
			started, err := r.Recover(context.Background(), request)
			if started || err != nil || recoveryConnectionCalls(c.fakeConnection) != 1 {
				t.Fatal("refused recovery changed the socket lifetime")
			}
			r.mu.Lock()
			r.closed = false
			r.mu.Unlock()
		})
	}
}

func TestRecoverySignalsRejectRestoredStaleDuplicateAndSocketSnapshots(t *testing.T) {
	r, c := recoveryFixture(t, fixtureStore())
	h := c.currentHandler(t)
	lastSuccess := time.Now().Add(-4 * time.Minute)
	r.mu.Lock()
	r.recoveryRunStartedAt = time.Now().Add(-5 * time.Minute)
	r.mu.Unlock()
	h(&events.KeepAliveTimeout{ErrorCount: 1, LastSuccess: lastSuccess})
	request := awaitRecoveryRequest(t, r)
	if request.Reason != "keepalive_timeout" {
		t.Fatal("timeout reason changed")
	}
	h(&events.KeepAliveTimeout{ErrorCount: 2, LastSuccess: lastSuccess})
	if duplicate, ok := r.RecoveryRequest(); !ok || duplicate != request {
		t.Fatal("duplicate keepalive signal reset the bounded retry identity")
	}
	h(&events.KeepAliveRestored{})
	if _, ok := r.RecoveryRequest(); ok {
		t.Fatal("restored keepalive retained recovery authority")
	}
	h(&events.KeepAliveTimeout{ErrorCount: 3, LastSuccess: lastSuccess})
	if _, ok := r.RecoveryRequest(); ok {
		t.Fatal("stale timeout after restoration recreated authority")
	}
	if started, err := r.Recover(context.Background(), request); started || err != nil {
		t.Fatal("restored recovery witness was not invalidated")
	}
	if err := r.Reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitHandoffActivity(t, c, "disconnect")
	waitHandoffActivity(t, c, "connect")
	h(&events.Disconnected{})
	if _, ok := r.RecoveryRequest(); ok {
		t.Fatal("retired observation handler created recovery authority")
	}
	current := c.currentHandler(t)
	for _, event := range []*events.KeepAliveTimeout{
		{ErrorCount: 1, LastSuccess: time.Now().Add(-time.Minute)},
		{ErrorCount: 1},
		{ErrorCount: 1, LastSuccess: time.Now().Add(time.Minute)},
		{ErrorCount: -1, LastSuccess: lastSuccess},
	} {
		current(event)
		if _, ok := r.RecoveryRequest(); ok {
			t.Fatal("unqualified timeout created recovery authority")
		}
	}
	r.mu.Lock()
	r.status.ConnectionDiagnostic.SocketStateKnown = true
	r.status.ConnectionDiagnostic.SocketConnected = true
	r.status.ConnectionDiagnostic.SocketAuthenticated = true
	r.mu.Unlock()
	current(&events.Disconnected{})
	request = awaitRecoveryRequest(t, r)
	if d := r.Status().ConnectionDiagnostic; d.SocketStateKnown || d.SocketConnected || d.SocketAuthenticated {
		t.Fatal("disconnection retained stale SDK socket/authentication proof")
	}
	current(&events.Connected{})
	if started, err := r.Recover(context.Background(), request); started || err != nil || recoveryConnectionCalls(c.fakeConnection) != 2 {
		t.Fatal("accepted Connected did not immediately invalidate recovery")
	}
}

func TestRecoveryDrainFailureRemainsStrict(t *testing.T) {
	base := fixtureStore()
	r, c := recoveryFixture(t, base)
	r.store = &recoveryDrainStore{recordingStore: base, drainErr: archive.ErrArchiveUnavailable}
	c.currentHandler(t)(&events.Disconnected{})
	request := awaitRecoveryRequest(t, r)
	started, err := r.Recover(context.Background(), request)
	if started || !errors.Is(err, archive.ErrArchiveUnavailable) || !r.Status().ReconciliationRequired || recoveryConnectionCalls(c.fakeConnection) != 1 {
		t.Fatal("failed durable drain authorized recovery or weakened strict archive protection")
	}
}

func TestRecoveryStartupTransientFailureRetainsOneGuardedRetryChain(t *testing.T) {
	r := testRuntime(fixtureStore())
	c := &diagnosticConnection{fakeConnection: &fakeConnection{}, err: fmt.Errorf("%w: %w", socket.ErrDialFailed, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH})}
	r.connection = c
	t.Cleanup(func() { _ = r.Close() })
	if err := r.Connect(context.Background()); !errors.Is(err, archive.ErrSourceUnavailable) {
		t.Fatal("startup failure changed its public error")
	}
	request := awaitRecoveryRequest(t, r)
	if request.Reason != "startup_failure" {
		t.Fatal("failed startup lacks a guarded retry reason")
	}
	if attempted, err := r.Recover(context.Background(), request); !attempted || !errors.Is(err, archive.ErrSourceUnavailable) {
		t.Fatal("failed retry did not report its authorized connection attempt")
	}
	if next, ok := r.RecoveryRequest(); !ok || next != request {
		t.Fatal("failed typed transient retry replaced the bounded chain identity")
	}
	c.err = nil
	if started, err := r.Recover(context.Background(), request); !started || err != nil || recoveryConnectionCalls(c.fakeConnection) != 3 {
		t.Fatal("safe transient chain could not recover")
	}
	if _, ok := r.RecoveryRequest(); ok {
		t.Fatal("successful retry retained its consumed witness")
	}
}

func TestRecoveryTransientClassifierRejectsUnqualifiedFailures(t *testing.T) {
	for _, item := range []struct {
		err       error
		transient bool
	}{
		{context.DeadlineExceeded, true},
		{fmt.Errorf("%w: %w", socket.ErrDialFailed, &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{IsTimeout: true}}), true},
		{fmt.Errorf("%w: %w", socket.ErrDialFailed, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}), true},
		{fmt.Errorf("%w: %w", socket.ErrDialFailed, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EACCES}), false},
		{fmt.Errorf("%w: %w", socket.ErrDialFailed, &net.DNSError{IsNotFound: true}), false},
		{socket.ErrWithStatusCode{StatusCode: 408}, true},
		{socket.ErrWithStatusCode{StatusCode: 500}, true},
		{socket.ErrWithStatusCode{StatusCode: 504}, true},
		{socket.ErrWithStatusCode{StatusCode: 505}, false},
		{socket.ErrWithStatusCode{StatusCode: 403}, false},
		{context.Canceled, false},
		{socket.ErrDialFailed, false},
		{opaqueConnectionError{}, false},
	} {
		class, reason := connectionErrorClass(item.err)
		if got := TransientConnectionFailure(ConnectionDiagnostic{ErrorClass: class, Reason: reason}); got != item.transient {
			t.Fatalf("typed retry class %s/%d = %t", class, reason, got)
		}
	}
}

func TestRecoveryCancellationAfterStopDrainsWithoutNewDialOrUnsafeMarker(t *testing.T) {
	base := fixtureStore()
	s := &recoveryDrainStore{recordingStore: base, bodyStarted: make(chan struct{}), bodyRelease: make(chan struct{})}
	r, c := recoveryFixture(t, base)
	r.store = s
	t.Cleanup(func() { releaseRecoveryBarrier(s.bodyRelease) })
	h := c.currentHandler(t)
	h(&events.Disconnected{})
	request := awaitRecoveryRequest(t, r)
	h(liveMessage("recovery-canceled-body", "synthetic-cancellation-source"))
	select {
	case <-s.bodyStarted:
	case <-time.After(time.Second):
		t.Fatal("accepted source barrier did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		attempted, err := r.Recover(ctx, request)
		if attempted {
			err = errors.New("canceled recovery dialed")
		}
		result <- err
	}()
	waitHandoffActivity(t, c, "disconnect")
	cancel()
	close(s.bodyRelease)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("recovery did not retain caller cancellation after clean drain")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled recovery did not finish its independent drain")
	}
	base.mu.Lock()
	unsafe, applied := len(base.unsafe), len(base.events)
	base.mu.Unlock()
	if unsafe != 0 || applied != 1 || !r.Quiesced() || r.Status().ReconciliationRequired || recoveryConnectionCalls(c.fakeConnection) != 1 {
		t.Fatal("supervisor cancellation poisoned or discarded accepted source work")
	}
}

func TestRecoveryDurableUnsafeMarkerBlocksBeforeAndDuringDrain(t *testing.T) {
	for _, duringDrain := range []bool{false, true} {
		t.Run(map[bool]string{false: "before", true: "during"}[duringDrain], func(t *testing.T) {
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			c := &handoffConnection{refreshConnection: &refreshConnection{fakeConnection: &fakeConnection{}}, activity: make(chan string, 16)}
			r.connection = c
			if err := r.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitHandoffActivity(t, c, "connect")
			c.currentHandler(t)(&events.Disconnected{})
			request := awaitRecoveryRequest(t, r)
			markUnsafe := func() {
				if err := store.MarkUnsafe(context.Background(), durableAccount, "identity_alias_conflict"); err != nil {
					t.Error("durable unsafe marker could not commit")
				}
			}
			if duringDrain {
				c.stopHook = markUnsafe
			} else {
				markUnsafe()
			}
			attempted, err := r.Recover(context.Background(), request)
			if attempted || !errors.Is(err, archive.ErrReconciliationRequired) || recoveryConnectionCalls(c.fakeConnection) != 1 {
				t.Fatal("durable source safety refusal authorized a fresh dial")
			}
			c.stopHook = nil
			if _, err := store.SourcePolicy(context.Background(), durableAccount); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Fatal("runtime recovery cleared the durable unsafe marker")
			}
		})
	}
}

type recoveryPreparationStore struct {
	*recordingStore
	stage            string
	started, release chan struct{}
	once             sync.Once
}

func (s *recoveryPreparationStore) Policy(ctx context.Context) (archive.ArchivePolicy, error) {
	if s.stage == "policy" {
		s.once.Do(func() { close(s.started) })
		<-s.release
	}
	return s.recordingStore.Policy(ctx)
}

func (s *recoveryPreparationStore) BeginObservation(ctx context.Context, account string) (archive.ObservationID, error) {
	if s.stage == "observation" {
		s.once.Do(func() { close(s.started) })
		<-s.release
	}
	return s.recordingStore.BeginObservation(ctx, account)
}

func TestRecoveryWithdrawalDuringPreparationNeverDials(t *testing.T) {
	for _, stage := range []string{"policy", "observation"} {
		t.Run(stage, func(t *testing.T) {
			base := fixtureStore()
			r, c := recoveryFixture(t, base)
			c.currentHandler(t)(&events.Disconnected{})
			request := awaitRecoveryRequest(t, r)
			s := &recoveryPreparationStore{recordingStore: base, stage: stage, started: make(chan struct{}), release: make(chan struct{})}
			r.store = s
			t.Cleanup(func() { releaseRecoveryBarrier(s.release) })
			result := make(chan error, 1)
			go func() {
				attempted, err := r.Recover(context.Background(), request)
				if attempted {
					err = errors.New("withdrawn recovery entered dial")
				}
				result <- err
			}()
			select {
			case <-s.started:
			case <-time.After(time.Second):
				t.Fatal("recovery preparation barrier did not start")
			}
			paused := make(chan error, 1)
			go func() { paused <- r.Pause(context.Background()) }()
			deadline := time.Now().Add(time.Second)
			for {
				r.mu.Lock()
				desired := r.recoveryDesired
				r.mu.Unlock()
				if !desired {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("manual pause did not withdraw in-flight recovery")
				}
				time.Sleep(time.Millisecond)
			}
			close(s.release)
			select {
			case err := <-result:
				if !errors.Is(err, archive.ErrSourceUnavailable) {
					t.Fatal("withdrawn preparation did not retain its fixed refusal")
				}
			case <-time.After(time.Second):
				t.Fatal("withdrawn recovery did not complete")
			}
			select {
			case err := <-paused:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("pause did not join withdrawn recovery")
			}
			if recoveryConnectionCalls(c.fakeConnection) != 1 || !r.Quiesced() || !r.Status().Paused || r.Status().ReconciliationRequired {
				t.Fatal("withdrawal during preparation dialed or left unsafe owned workers")
			}
		})
	}
}

func TestRecoveryHealthRestorationDuringCommittedDrainFinishesSafeReplacement(t *testing.T) {
	for _, connected := range []bool{false, true} {
		r, c := recoveryFixture(t, fixtureStore())
		c.currentHandler(t)(&events.Disconnected{})
		request := awaitRecoveryRequest(t, r)
		c.stopHook = func() {
			r.mu.Lock()
			run := r.recoveryRun
			r.mu.Unlock()
			if connected {
				r.noteRecoverySignal(&events.Connected{}, run)
			} else {
				r.noteRecoverySignal(&events.KeepAliveRestored{}, run)
			}
		}
		attempted, err := r.Recover(context.Background(), request)
		c.stopHook = nil
		if !attempted || err != nil || recoveryConnectionCalls(c.fakeConnection) != 2 {
			t.Fatal("old-run health restoration abandoned an already-committed replacement")
		}
	}
}

type recoveryDisconnectConnection struct{ *diagnosticConnection }

func (c *recoveryDisconnectConnection) ConnectContext(ctx context.Context) error {
	if err := c.diagnosticConnection.ConnectContext(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	h := c.handler
	c.mu.Unlock()
	h(&events.Disconnected{})
	return nil
}

func TestRecoveryDisconnectedDuringDialCannotRegainStaleSocketProof(t *testing.T) {
	r := testRuntime(fixtureStore())
	c := &recoveryDisconnectConnection{diagnosticConnection: &diagnosticConnection{fakeConnection: &fakeConnection{}, connected: true, authenticated: true}}
	r.connection = c
	t.Cleanup(func() { _ = r.Close() })
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitRecoveryRequest(t, r)
	if d := r.Status().ConnectionDiagnostic; d.SocketStateKnown || d.SocketConnected || d.SocketAuthenticated {
		t.Fatal("post-dial SDK authentication snapshot overrode accepted Disconnected proof")
	}
}

type recoveryLossDuringDialConnection struct {
	*handoffConnection
	runtime       *Runtime
	waitForCommit bool
}

func (c *recoveryLossDuringDialConnection) ConnectContext(ctx context.Context) error {
	if err := c.handoffConnection.ConnectContext(ctx); err != nil {
		return err
	}
	if recoveryConnectionCalls(c.fakeConnection) != 2 {
		return nil
	}
	c.mu.Lock()
	h := c.handler
	c.mu.Unlock()
	h(&events.Disconnected{})
	if c.waitForCommit {
		deadline := time.Now().Add(time.Second)
		for {
			c.runtime.mu.Lock()
			idle := c.runtime.admissions == 0
			c.runtime.mu.Unlock()
			if idle {
				break
			}
			if time.Now().After(deadline) {
				return context.DeadlineExceeded
			}
			time.Sleep(time.Millisecond)
		}
	}
	return nil
}

func TestRecoveryDisconnectDuringSuccessfulDialPreservesBoundedChain(t *testing.T) {
	for _, commitBeforeReturn := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit_after_return", true: "commit_before_return"}[commitBeforeReturn], func(t *testing.T) {
			base := fixtureStore()
			r, c := recoveryFixture(t, base)
			c.currentHandler(t)(&events.Disconnected{})
			request := awaitRecoveryRequest(t, r)
			s := &recoveryDrainStore{recordingStore: base}
			if !commitBeforeReturn {
				s.offlineStarted, s.offlineRelease = make(chan struct{}), make(chan struct{})
				t.Cleanup(func() { releaseRecoveryBarrier(s.offlineRelease) })
			}
			r.store = s
			r.connection = &recoveryLossDuringDialConnection{handoffConnection: c, runtime: r, waitForCommit: commitBeforeReturn}
			attempted, err := r.Recover(context.Background(), request)
			if !attempted || err != nil {
				t.Fatal("SDK startup result changed for the new loss regression")
			}
			if !commitBeforeReturn {
				select {
				case <-s.offlineStarted:
				case <-time.After(time.Second):
					t.Fatal("new run disconnection did not reach durable gap barrier")
				}
				if _, ready := r.RecoveryRequest(); ready {
					t.Fatal("new run gap authorized recovery before its durable commit")
				}
				close(s.offlineRelease)
			}
			next := awaitRecoveryRequest(t, r)
			if next != request || recoveryConnectionCalls(c.fakeConnection) != 2 {
				t.Fatal("new run loss discarded or reset the bounded pre-authentication chain")
			}
			c.currentHandler(t)(&events.Connected{})
			waitRefreshConnection(t, r, "connected")
			if _, ready := r.RecoveryRequest(); ready {
				t.Fatal("actual Connected did not retire the recovery chain")
			}
			c.currentHandler(t)(&events.Disconnected{})
			if later := awaitRecoveryRequest(t, r); later.Token == request.Token {
				t.Fatal("genuine connection progress could not establish a distinct later outage")
			}
		})
	}
}

func TestRecoveryRejectsOldKeepAliveDispatchedToNewHandler(t *testing.T) {
	r, c := recoveryFixture(t, fixtureStore())
	oldLastSuccess := time.Now().Add(-4 * time.Minute)
	// The SDK dispatch goroutine reads the current handler list when it runs,
	// so a new handler can receive timeout metadata emitted by an older run.
	c.currentHandler(t)(&events.KeepAliveTimeout{ErrorCount: 1, LastSuccess: oldLastSuccess})
	if _, ready := r.RecoveryRequest(); ready {
		t.Fatal("old keepalive metadata was attributed to the newly started owned run")
	}
}

func TestRecoveryKeepAliveUsesSDKSuccessWatermarkInsteadOfCallbackTime(t *testing.T) {
	for _, restored := range []bool{false, true} {
		t.Run(map[bool]string{false: "after_connected", true: "after_restored"}[restored], func(t *testing.T) {
			r, c := recoveryFixture(t, fixtureStore())
			r.mu.Lock()
			r.recoveryRunStartedAt = time.Now().Add(-10 * time.Minute)
			r.mu.Unlock()
			h := c.currentHandler(t)
			newSuccess := time.Now().Add(-4 * time.Minute)
			var previous RecoveryRequest
			if restored {
				oldSuccess := time.Now().Add(-8 * time.Minute)
				h(&events.KeepAliveTimeout{ErrorCount: 1, LastSuccess: oldSuccess})
				previous = awaitRecoveryRequest(t, r)
				h(&events.KeepAliveRestored{})
				h(&events.KeepAliveTimeout{ErrorCount: 2, LastSuccess: oldSuccess})
				if _, ready := r.RecoveryRequest(); ready {
					t.Fatal("the restored outage's stale timeout recreated recovery authority")
				}
			} else {
				// The SDK starts keepalive before dispatching Connected, so its
				// valid last-success timestamp can precede this callback time.
				h(&events.Connected{})
				waitRefreshConnection(t, r, "connected")
			}
			h(&events.KeepAliveTimeout{ErrorCount: 1, LastSuccess: newSuccess})
			next := awaitRecoveryRequest(t, r)
			if next.Reason != "keepalive_timeout" || (restored && next.Token == previous.Token) {
				t.Fatal("a genuine later ping outage was confused with stale SDK metadata")
			}
		})
	}
}

func TestRecoveryDelayedRestoreCannotDiscardFreshAwaitingAuthenticationChain(t *testing.T) {
	r, c := recoveryFixture(t, fixtureStore())
	c.currentHandler(t)(&events.Disconnected{})
	request := awaitRecoveryRequest(t, r)
	if attempted, err := r.Recover(context.Background(), request); !attempted || err != nil {
		t.Fatal("replacement did not enter owned startup")
	}
	c.currentHandler(t)(&events.KeepAliveRestored{})
	c.currentHandler(t)(&events.Disconnected{})
	if next := awaitRecoveryRequest(t, r); next != request {
		t.Fatal("untimed delayed restoration discarded a fresh run's bounded pre-authentication chain")
	}
}

func TestRecoveryReorderedRestoreCannotSuppressNewerSDKSuccess(t *testing.T) {
	r, c := recoveryFixture(t, fixtureStore())
	r.mu.Lock()
	r.recoveryRunStartedAt = time.Now().Add(-10 * time.Minute)
	r.mu.Unlock()
	h := c.currentHandler(t)
	oldSuccess, newSuccess := time.Now().Add(-8*time.Minute), time.Now().Add(-4*time.Minute)
	h(&events.KeepAliveTimeout{ErrorCount: 1, LastSuccess: oldSuccess})
	previous := awaitRecoveryRequest(t, r)
	h(&events.KeepAliveTimeout{ErrorCount: 1, LastSuccess: newSuccess})
	h(&events.KeepAliveRestored{})
	h(&events.KeepAliveTimeout{ErrorCount: 2, LastSuccess: newSuccess})
	if next := awaitRecoveryRequest(t, r); next.Token == previous.Token {
		t.Fatal("a delayed restoration confused a newer failed ping wave with the restored old wave")
	}
}

func TestRecoveryDurableDisconnectOutranksRestorationOfKeepAliveWave(t *testing.T) {
	for _, restoreBeforeCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore_after_commit", true: "restore_before_commit"}[restoreBeforeCommit], func(t *testing.T) {
			base := fixtureStore()
			r, c := recoveryFixture(t, base)
			r.mu.Lock()
			r.recoveryRunStartedAt = time.Now().Add(-5 * time.Minute)
			r.mu.Unlock()
			h := c.currentHandler(t)
			h(&events.KeepAliveTimeout{ErrorCount: 1, LastSuccess: time.Now().Add(-4 * time.Minute)})
			request := awaitRecoveryRequest(t, r)
			s := &recoveryDrainStore{recordingStore: base, offlineStarted: make(chan struct{}), offlineRelease: make(chan struct{})}
			r.store = s
			t.Cleanup(func() { releaseRecoveryBarrier(s.offlineRelease) })
			committed := make(chan struct{}, 1)
			r.callbacks.StatusChanged = func(Status) {
				base.mu.Lock()
				gap := slices.ContainsFunc(base.syncs, func(u archive.SyncUpdate) bool { return u.GapCode == "offline_gap" })
				base.mu.Unlock()
				if gap {
					select {
					case committed <- struct{}{}:
					default:
					}
				}
			}
			h(&events.Disconnected{})
			select {
			case <-s.offlineStarted:
			case <-time.After(time.Second):
				t.Fatal("disconnect did not reach its durable gap barrier")
			}
			if restoreBeforeCommit {
				h(&events.KeepAliveRestored{})
				h(&events.KeepAliveTimeout{ErrorCount: 2, LastSuccess: time.Now().Add(-210 * time.Second)})
				if _, ready := r.RecoveryRequest(); ready {
					t.Fatal("pending source loss offered recovery before its durable commit")
				}
			}
			close(s.offlineRelease)
			select {
			case <-committed:
			case <-time.After(time.Second):
				t.Fatal("disconnect did not commit its independent durable gap")
			}
			if !restoreBeforeCommit {
				h(&events.KeepAliveRestored{})
			}
			if current := awaitRecoveryRequest(t, r); current != request {
				t.Fatal("advisory restoration erased durable disconnect authority from a keepalive-origin chain")
			}
		})
	}
}
