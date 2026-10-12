package remoteaccess

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryStore struct {
	mu      sync.Mutex
	intent  Intent
	secret  Secret
	saveErr error
	loadErr error
	removed bool
}

func (s *memoryStore) Load(context.Context) (Intent, Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.intent, append(Secret{}, s.secret...), s.loadErr
}
func (s *memoryStore) Save(_ context.Context, i Intent, k Secret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.intent = i
	s.secret = append(Secret{}, k...)
	return nil
}
func (s *memoryStore) Remove(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intent = Intent{}
	s.secret = nil
	s.removed = true
	return nil
}

type testSession struct {
	listener net.Listener
	origin   string
	fail     chan error
	closes   atomic.Int32
}

func newTestSession(t *testing.T) *testSession {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return &testSession{listener: l, origin: "https://saved.ngrok-free.app", fail: make(chan error, 1)}
}
func (s *testSession) Origin() string              { return s.origin }
func (s *testSession) Listener() net.Listener      { return s.listener }
func (s *testSession) Done() <-chan error          { return s.fail }
func (s *testSession) Close(context.Context) error { s.closes.Add(1); s.listener.Close(); return nil }

type openCall struct {
	intent Intent
	notice func(Notice)
	reply  chan openResult
	ctx    context.Context
}
type openResult struct {
	session Session
	err     error
}
type testAdapter struct{ calls chan openCall }

func (a *testAdapter) Open(ctx context.Context, i Intent, k Secret, notify func(Notice)) (Session, error) {
	call := openCall{i, notify, make(chan openResult, 1), ctx}
	a.calls <- call
	select {
	case result := <-call.reply:
		return result.session, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type clockRequest struct {
	duration time.Duration
	tick     chan time.Time
}
type testClock struct{ requests chan clockRequest }

func (c *testClock) After(d time.Duration) <-chan time.Time {
	r := clockRequest{d, make(chan time.Time, 1)}
	c.requests <- r
	return r.tick
}
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case x := <-ch:
		return x
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for test event")
		var z T
		return z
	}
}
func until(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		runtime.Gosched()
	}
}
func fixture(t *testing.T, store *memoryStore, prepare func(context.Context, Intent, string, uint64) (Handlers, error)) (*Manager, *testAdapter, *testClock) {
	t.Helper()
	a := &testAdapter{calls: make(chan openCall, 20)}
	c := &testClock{requests: make(chan clockRequest, 20)}
	m, err := newManager(ManagerConfig{Store: store, Adapters: map[Provider]Adapter{TailscalePrivate: a}, Prepare: prepare}, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return m, a, c
}
func basicHandlers(context.Context, Intent, string, uint64) (Handlers, error) {
	return Handlers{MCP: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "mcp") })}, nil
}

// Generic lifecycle tests discover an authenticated provider origin. ngrok
// requires its account-assigned origin at setup, so these use private Tailscale.
func managerIntent() Intent {
	i := storedIntent()
	i.Provider = TailscalePrivate
	i.NodeName = "test-archive"
	return i
}
func configure(t *testing.T, m *Manager) {
	t.Helper()
	i := managerIntent()
	i.SavedOrigin = ""
	if err := m.Configure(context.Background(), i, Secret("synthetic")); err != nil {
		t.Fatal(err)
	}
}
func TestManagerDisabledAndCorruptStartup(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		s := &memoryStore{}
		if corrupt {
			s.loadErr = errors.New("raw provider token")
		}
		m, a, _ := fixture(t, s, basicHandlers)
		state := "disabled"
		if corrupt {
			state = "needs_setup"
		}
		if got := m.Status(); got.State != state || got.MCPReady {
			t.Fatalf("startup status %#v", got)
		}
		select {
		case <-a.calls:
			t.Fatal("disabled opened provider")
		default:
		}
	}
}
func TestManagerPersistsBeforePrepareAndServesSocket(t *testing.T) {
	s := &memoryStore{}
	prepared := make(chan uint64, 1)
	released := make(chan uint64, 1)
	var m *Manager
	prepare := func(ctx context.Context, i Intent, origin string, g uint64) (Handlers, error) {
		saved, _, _ := s.Load(ctx)
		if saved.SavedOrigin != origin {
			t.Error("origin not persisted before Prepare")
		}
		_ = m.Status()
		prepared <- g
		return Handlers{MCP: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "authorized-app-handler") }), Release: func(context.Context) error { _ = m.Status(); released <- g; return nil }}, nil
	}
	m, a, _ := fixture(t, s, prepare)
	configure(t, m)
	call := recv(t, a.calls)
	session := newTestSession(t)
	call.reply <- openResult{session: session}
	generation := recv(t, prepared)
	until(t, func() bool { return m.Status().State == "online" })
	req, _ := http.NewRequest("GET", "http://"+session.listener.Addr().String()+"/mcp", nil)
	req.Host = "saved.ngrok-free.app"
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(data) != "authorized-app-handler" {
		t.Fatal("app handler not used")
	}
	if err := m.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recv(t, released) != generation || session.closes.Load() != 1 {
		t.Fatal("generation not released once")
	}
	got, key, _ := s.Load(context.Background())
	if got.Enabled || got.SavedOrigin != session.origin || string(key) != "synthetic" {
		t.Fatal("disable lost identity")
	}
	call.notice(Notice{State: "online", Code: "raw-token", AuthURL: "https://secret.example/token"})
	if m.Status().State != "disabled" || m.OwnerNotice().AuthURL != "" {
		t.Fatal("stale callback changed disabled generation")
	}
}
func TestManagerSaveAndPrepareFailuresCloseUnservedListener(t *testing.T) {
	for _, fail := range []string{"save", "prepare"} {
		t.Run(fail, func(t *testing.T) {
			s := &memoryStore{}
			called := atomic.Int32{}
			m, a, c := fixture(t, s, func(context.Context, Intent, string, uint64) (Handlers, error) {
				called.Add(1)
				return Handlers{}, errors.New("raw prepare password")
			})
			configure(t, m)
			call := recv(t, a.calls)
			if fail == "save" {
				s.mu.Lock()
				s.saveErr = errors.New("disk secret")
				s.mu.Unlock()
			}
			session := newTestSession(t)
			call.reply <- openResult{session: session}
			until(t, func() bool { return session.closes.Load() == 1 })
			conn, err := net.Dial("tcp", session.listener.Addr().String())
			if err == nil {
				conn.Close()
				t.Fatal("unprepared ingress left open")
			}
			if fail == "save" && called.Load() != 0 {
				t.Fatal("Prepare after failed save")
			}
			select {
			case <-c.requests:
				t.Fatal("unsafe setup failure retried")
			default:
			}
		})
	}
}
func TestManagerBoundedRetryAndTerminalErrors(t *testing.T) {
	s := &memoryStore{}
	m, a, c := fixture(t, s, basicHandlers)
	configure(t, m)
	for n := 0; n < 9; n++ {
		call := recv(t, a.calls)
		call.reply <- openResult{err: errors.New("provider auth URL secret")}
		request := recv(t, c.requests)
		expected := time.Second * time.Duration(1<<n)
		if expected > time.Minute {
			expected = time.Minute
		}
		if request.duration < expected || request.duration > min(time.Minute, expected+expected/4) {
			t.Fatalf("retry outside bounds: %v", request.duration)
		}
		request.tick <- time.Now()
	}
	call := recv(t, a.calls)
	call.reply <- openResult{err: ErrCredentialsRejected}
	until(t, func() bool { return m.Status().State == "credentials_rejected" })
	select {
	case <-c.requests:
		t.Fatal("credentials retry")
	default:
	}
	if err := m.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, terminal := range []struct {
		err   error
		state string
	}{{ErrPermissionRequired, "permission_required"}, {ErrServiceLimit, "service_limit_reached"}} {
		configure(t, m)
		call = recv(t, a.calls)
		call.reply <- openResult{err: terminal.err}
		until(t, func() bool { return m.Status().State == terminal.state })
		select {
		case <-c.requests:
			t.Fatal("terminal error retried")
		default:
		}
		if err := m.Disable(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
func TestManagerCancellationAndIdentityPin(t *testing.T) {
	s := &memoryStore{}
	m, a, c := fixture(t, s, basicHandlers)
	configure(t, m)
	call := recv(t, a.calls)
	call.reply <- openResult{err: ErrTransient}
	_ = recv(t, c.requests)
	if err := m.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	call.notice(Notice{State: "permission_required", Code: "raw", AuthURL: "https://login.example/token"})
	if m.Status().State != "disabled" {
		t.Fatal("stale removal notice")
	}
	configure(t, m)
	call = recv(t, a.calls)
	session := newTestSession(t)
	call.reply <- openResult{session: session}
	until(t, func() bool { return m.Status().State == "online" })
	if err := m.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	configure(t, m)
	call = recv(t, a.calls)
	other := newTestSession(t)
	other.origin = "https://replacement.ngrok-free.app"
	call.reply <- openResult{session: other}
	until(t, func() bool { return other.closes.Load() == 1 })
	if got, _, _ := s.Load(context.Background()); got.SavedOrigin != session.origin {
		t.Fatal("failed reconnect changed saved origin")
	}
}
func TestManagerJoinsHandlersBeforeReleaseAndNextPrepare(t *testing.T) {
	s := &memoryStore{}
	entered := make(chan struct{})
	leave := make(chan struct{})
	released := make(chan struct{})
	var prepares atomic.Int32
	m, a, _ := fixture(t, s, func(context.Context, Intent, string, uint64) (Handlers, error) {
		prepares.Add(1)
		return Handlers{MCP: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-leave }), Release: func(context.Context) error { close(released); return nil }}, nil
	})
	configure(t, m)
	call := recv(t, a.calls)
	session := newTestSession(t)
	call.reply <- openResult{session: session}
	until(t, func() bool { return m.Status().State == "online" })
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		req, _ := http.NewRequest("GET", "http://"+session.listener.Addr().String()+"/mcp", nil)
		req.Host = "saved.ngrok-free.app"
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
	}()
	recv(t, entered)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- m.Disable(ctx) }()
	until(t, func() bool { return m.Status().Code == "remote_stopping" })
	cancel()
	if err := recv(t, stopped); !errors.Is(err, ErrTransient) {
		t.Fatalf("bounded disable=%v", err)
	}
	select {
	case <-released:
		t.Fatal("released while handler running")
	default:
	}
	close(leave)
	recv(t, clientDone)
	if err := m.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	recv(t, released)
	if prepares.Load() != 1 {
		t.Fatal("unexpected prepare")
	}
}

func TestCallbacksCanReenterWithoutLifecycleDeadlock(t *testing.T) {
	s := &memoryStore{}
	var m *Manager
	results := make(chan error, 6)
	var generations []uint64
	var lock sync.Mutex
	prepare := func(ctx context.Context, i Intent, origin string, g uint64) (Handlers, error) {
		_ = m.Status()
		_ = m.OwnerNotice()
		results <- m.Disable(context.Background())
		lock.Lock()
		generations = append(generations, g)
		lock.Unlock()
		return Handlers{MCP: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), Release: func(context.Context) error { _ = m.Status(); results <- m.Remove(context.Background()); return nil }}, nil
	}
	m, a, c := fixture(t, s, prepare)
	configure(t, m)
	first := recv(t, a.calls)
	one := newTestSession(t)
	first.reply <- openResult{session: one}
	if !errors.Is(recv(t, results), ErrBusy) {
		t.Fatal("Prepare lifecycle reentry not refused")
	}
	until(t, func() bool { return m.Status().State == "online" })
	one.fail <- ErrTransient
	if !errors.Is(recv(t, results), ErrBusy) {
		t.Fatal("Release lifecycle reentry not refused")
	}
	retry := recv(t, c.requests)
	retry.tick <- time.Now()
	second := recv(t, a.calls)
	two := newTestSession(t)
	second.reply <- openResult{session: two}
	if !errors.Is(recv(t, results), ErrBusy) {
		t.Fatal("second Prepare lifecycle reentry not refused")
	}
	until(t, func() bool { return m.Status().State == "online" })
	first.notice(Notice{State: "permission_required", AuthURL: "https://login.example/stale"})
	if m.OwnerNotice().AuthURL != "" {
		t.Fatal("old attempt notice changed newer session")
	}
	lock.Lock()
	if len(generations) != 2 || generations[0] >= generations[1] {
		t.Fatal("reconnect reused Prepare generation")
	}
	lock.Unlock()
	if err := m.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(recv(t, results), ErrBusy) {
		t.Fatal("Disable Release lifecycle reentry not refused")
	}
}
func TestManagerCleanupFailureFencesReplacement(t *testing.T) {
	s := &memoryStore{}
	fail := atomic.Bool{}
	fail.Store(true)
	var prepared atomic.Int32
	m, a, _ := fixture(t, s, func(context.Context, Intent, string, uint64) (Handlers, error) {
		prepared.Add(1)
		return Handlers{MCP: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), Release: func(context.Context) error {
			if fail.Load() {
				return errors.New("provider secret")
			}
			return nil
		}}, nil
	})
	configure(t, m)
	call := recv(t, a.calls)
	session := newTestSession(t)
	call.reply <- openResult{session: session}
	until(t, func() bool { return m.Status().State == "online" })
	if err := m.Disable(context.Background()); !errors.Is(err, ErrPrepare) {
		t.Fatalf("cleanup failure = %v", err)
	}
	if err := m.Configure(context.Background(), managerIntent(), Secret("new")); !errors.Is(err, ErrPrepare) {
		t.Fatalf("new generation bypassed failed release: %v", err)
	}
	if prepared.Load() != 1 {
		t.Fatal("new Prepare before cleanup")
	}
	fail.Store(false)
	if err := m.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestPreparePartialFailureIsReleased(t *testing.T) {
	s := &memoryStore{}
	released := make(chan struct{}, 1)
	m, a, _ := fixture(t, s, func(context.Context, Intent, string, uint64) (Handlers, error) {
		return Handlers{Release: func(context.Context) error { released <- struct{}{}; return nil }}, errors.New("partial app mutation")
	})
	configure(t, m)
	call := recv(t, a.calls)
	session := newTestSession(t)
	call.reply <- openResult{session: session}
	recv(t, released)
	until(t, func() bool { return m.Status().Code == ErrPrepare.Error() })
	if session.closes.Load() != 1 {
		t.Fatal("partial Prepare listener remained")
	}
}
func TestManagerMCPReadinessAndClosePreservesIntent(t *testing.T) {
	s := &memoryStore{}
	m, a, _ := fixture(t, s, basicHandlers)
	var ready atomic.Bool
	m.cfg.MCPReady = func() bool {
		_ = m.OwnerNotice()
		if !errors.Is(m.Disable(context.Background()), ErrBusy) {
			t.Error("MCPReady lifecycle reentry not refused")
		}
		return ready.Load()
	}
	configure(t, m)
	call := recv(t, a.calls)
	session := newTestSession(t)
	call.reply <- openResult{session: session}
	until(t, func() bool { return m.Status().State == "online" })
	if m.Status().MCPReady {
		t.Fatal("online inferred application readiness")
	}
	ready.Store(true)
	if !m.Status().MCPReady {
		t.Fatal("app readiness ignored")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	saved, _, _ := s.Load(context.Background())
	if !saved.Enabled || saved.SavedOrigin == "" {
		t.Fatal("Close removed reconnect intent")
	}
	restarted, a2, _ := fixture(t, s, basicHandlers)
	call2 := recv(t, a2.calls)
	if call2.intent.SavedOrigin != saved.SavedOrigin {
		t.Fatal("restart lost pinned authority")
	}
	if err := restarted.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestManagerPermissionNoticeOwnerOnly(t *testing.T) {
	s := &memoryStore{}
	m, a, _ := fixture(t, s, basicHandlers)
	configure(t, m)
	call := recv(t, a.calls)
	call.notice(Notice{State: "permission_required", Code: "raw-secret", AuthURL: "https://login.example/device-secret"})
	call.reply <- openResult{err: ErrPermissionRequired}
	until(t, func() bool { return m.Status().Code == ErrPermissionRequired.Error() })
	if m.OwnerNotice().AuthURL != "https://login.example/device-secret" {
		t.Fatal("owner lost sign-in URL")
	}
	if m.Status().Code == "raw-secret" {
		t.Fatal("raw notice leaked")
	}
}

func TestReconnectStaleReleaseCannotClearNewGeneration(t *testing.T) {
	s := &memoryStore{}
	var lock sync.Mutex
	var active uint64
	releases := make(chan func(context.Context) error, 2)
	m, a, c := fixture(t, s, func(_ context.Context, _ Intent, _ string, g uint64) (Handlers, error) {
		lock.Lock()
		active = g
		lock.Unlock()
		release := func(context.Context) error {
			lock.Lock()
			defer lock.Unlock()
			if active == g {
				active = 0
			}
			return nil
		}
		releases <- release
		return Handlers{MCP: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), Release: release}, nil
	})
	configure(t, m)
	call := recv(t, a.calls)
	one := newTestSession(t)
	call.reply <- openResult{session: one}
	oldRelease := recv(t, releases)
	until(t, func() bool { return m.Status().State == "online" })
	one.fail <- ErrTransient
	retry := recv(t, c.requests)
	retry.tick <- time.Now()
	call = recv(t, a.calls)
	two := newTestSession(t)
	call.reply <- openResult{session: two}
	_ = recv(t, releases)
	until(t, func() bool { return m.Status().State == "online" })
	lock.Lock()
	current := active
	lock.Unlock()
	if err := oldRelease(context.Background()); err != nil {
		t.Fatal(err)
	}
	lock.Lock()
	if active != current || active == 0 {
		t.Fatal("stale release cleared current app route")
	}
	lock.Unlock()
}

// Regression for retained cleanup: cancellation bounds the owner call, while
// the one old cleanup remains fenced until its callback actually returns.
func TestRetainedCleanupDeadlineKeepsSingleOwner(t *testing.T) {
	s := &memoryStore{}
	retryEntered := make(chan context.Context, 1)
	allowJoin := make(chan struct{})
	var releaseCalls atomic.Int32
	var prepares atomic.Int32
	m, a, _ := fixture(t, s, func(context.Context, Intent, string, uint64) (Handlers, error) {
		prepares.Add(1)
		return Handlers{MCP: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), Release: func(ctx context.Context) error {
			call := releaseCalls.Add(1)
			if call == 1 {
				return errors.New("initial release failure")
			}
			if call == 2 {
				retryEntered <- ctx
				select {
				case <-ctx.Done():
				case <-allowJoin:
				}
				<-allowJoin
			}
			return nil
		}}, nil
	})
	// Always unblock the injected callback before fixture teardown, including RED.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(allowJoin) }) }
	defer release()
	t.Cleanup(func() { release(); until(t, func() bool { return m.callbacks.Load() == 0 && !m.ops.Load() }) })
	configure(t, m)
	call := recv(t, a.calls)
	session := newTestSession(t)
	call.reply <- openResult{session: session}
	until(t, func() bool { return m.Status().State == "online" })
	session.fail <- ErrTransient
	until(t, func() bool { return m.Status().Code == ErrPrepare.Error() })
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	returned := make(chan error, 1)
	started := time.Now()
	go func() { returned <- m.Disable(ctx) }()
	retryCtx := recv(t, retryEntered)
	<-ctx.Done()
	select {
	case err := <-returned:
		if !errors.Is(err, ErrTransient) {
			t.Fatalf("deadline result=%v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("retained cleanup ignored caller deadline")
	}
	t.Logf("Disable returned after %s with cleanup still unjoined", time.Since(started))
	select {
	case <-retryCtx.Done():
	default:
		t.Fatal("cleanup retry did not receive caller cancellation")
	}
	if err := m.Configure(context.Background(), managerIntent(), Secret("new")); !errors.Is(err, ErrBusy) {
		t.Fatalf("replacement during pending release=%v", err)
	}
	if err := m.Disable(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("overlapping retry was admitted: %v", err)
	}
	if releaseCalls.Load() != 2 || prepares.Load() != 1 {
		t.Fatal("pending cleanup lost single ownership")
	}
	release()
	until(t, func() bool { return m.callbacks.Load() == 0 })
	if err := m.Disable(context.Background()); err != nil {
		t.Fatalf("cleanup did not join: %v", err)
	}
	if releaseCalls.Load() != 2 {
		t.Fatal("completed retry was invoked twice")
	}
}

func TestStatusSkipsReadinessWhenLifecycleAlreadyAdmitted(t *testing.T) {
	s := &memoryStore{}
	m, a, _ := fixture(t, s, basicHandlers)
	var readyCalls atomic.Int32
	m.cfg.MCPReady = func() bool { readyCalls.Add(1); return true }
	configure(t, m)
	call := recv(t, a.calls)
	session := newTestSession(t)
	call.reply <- openResult{session: session}
	until(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.snapshot.State == "online" })
	if err := m.beginOperation(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.ops.Store(false)
	snapshot := m.Status()
	if readyCalls.Load() != 0 || snapshot.MCPReady {
		t.Fatal("Status admitted MCPReady after lifecycle gate acquisition")
	}
}

func TestReadinessAdmissionMakesShutdownBusyUntilCallbackJoins(t *testing.T) {
	s := &memoryStore{}
	m, a, _ := fixture(t, s, basicHandlers)
	entered := make(chan struct{})
	leave := make(chan struct{})
	returned := make(chan Snapshot, 1)
	m.cfg.MCPReady = func() bool { close(entered); <-leave; return true }
	configure(t, m)
	call := recv(t, a.calls)
	session := newTestSession(t)
	call.reply <- openResult{session: session}
	until(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.snapshot.State == "online" })
	go func() { returned <- m.Status() }()
	recv(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Close(ctx); !errors.Is(err, ErrBusy) {
		t.Fatalf("Close with active readiness=%v", err)
	}
	if session.closes.Load() != 0 {
		t.Fatal("shutdown began before readiness joined")
	}
	close(leave)
	if !recv(t, returned).MCPReady {
		t.Fatal("readiness result lost")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Status().MCPReady {
		t.Fatal("successful Close admitted another readiness callback")
	}
}

type controlledCloseSession struct {
	*testSession
	closeFn func(context.Context) error
}

func (s *controlledCloseSession) Close(ctx context.Context) error { return s.closeFn(ctx) }

// Exercise pending transport cleanup, where no application callback counter is
// active: later lifecycle callers must join the same attempt, never retry it.
func TestRetainedTransportCleanupDoesNotOverlapAfterTimeout(t *testing.T) {
	store := &memoryStore{}
	m, a, _ := fixture(t, store, basicHandlers)
	entered := make(chan struct{}, 1)
	join := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(join) }) }
	defer release()
	t.Cleanup(func() { release(); until(t, func() bool { return !m.ops.Load() }) })
	base := newTestSession(t)
	var closes atomic.Int32
	session := &controlledCloseSession{testSession: base, closeFn: func(ctx context.Context) error {
		if closes.Add(1) == 1 {
			return errors.New("initial close failed")
		}
		entered <- struct{}{}
		select {
		case <-ctx.Done():
		case <-join:
		}
		<-join
		return nil
	}}
	configure(t, m)
	call := recv(t, a.calls)
	call.reply <- openResult{session: session}
	until(t, func() bool { return m.Status().State == "online" })
	base.fail <- ErrTransient
	until(t, func() bool { return m.Status().Code == ErrPrepare.Error() })
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Disable(ctx) }()
	recv(t, entered)
	if err := recv(t, done); !errors.Is(err, ErrTransient) {
		t.Fatalf("first bounded retry=%v", err)
	}
	nextCtx, nextCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer nextCancel()
	if err := m.Disable(nextCtx); !errors.Is(err, ErrTransient) {
		t.Fatalf("join existing retry=%v", err)
	}
	if closes.Load() != 2 {
		t.Fatal("timed-out transport cleanup spawned overlapping retry")
	}
	m.mu.Lock()
	pending := m.run.cleanupRun
	m.mu.Unlock()
	if pending == nil {
		t.Fatal("pending cleanup ownership lost")
	}
	release()
	recv(t, pending.done)
	if err := m.Disable(context.Background()); err != nil {
		t.Fatal(err)
	}
	if closes.Load() != 2 {
		t.Fatal("completed transport cleanup repeated")
	}
}

func TestCloudflareManagerRemainsRouteUnverified(t *testing.T) {
	s := newTestSession(t)
	a := &testAdapter{calls: make(chan openCall, 1)}
	var readyCalls atomic.Int32
	m, err := NewManager(ManagerConfig{Store: &memoryStore{}, Adapters: map[Provider]Adapter{Cloudflare: a}, MCPReady: func() bool { readyCalls.Add(1); return true }, Prepare: func(context.Context, Intent, string, uint64) (Handlers, error) {
		return Handlers{MCP: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "application_auth_required", 401) })}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	i := cloudflareIntent()
	i.SavedOrigin = s.origin
	if err = m.Configure(context.Background(), i, Secret("synthetic-test-token")); err != nil {
		t.Fatal(err)
	}
	call := recv(t, a.calls)
	call.reply <- openResult{session: s}
	until(t, func() bool { return m.Status().State == "route_unverified" })
	status := m.Status()
	if status.Code != "remote_route_unverified" || status.MCPReady || readyCalls.Load() != 0 {
		t.Fatal("false readiness", status)
	}
	client := &http.Client{Timeout: time.Second}
	for _, tc := range []struct {
		path, host, origin string
		code               int
	}{{"/mcp", "saved.ngrok-free.app", s.origin, 401}, {"/api/bootstrap", "saved.ngrok-free.app", s.origin, 404}, {"/mcp", "127.0.0.1", s.origin, 403}, {"/mcp", "saved.ngrok-free.app", "https://other.example.com", 403}} {
		req, _ := http.NewRequest("POST", "http://"+s.listener.Addr().String()+tc.path, nil)
		req.Host = tc.host
		req.Header.Set("Origin", tc.origin)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.code {
			t.Fatal("route boundary", tc.path, resp.StatusCode)
		}
	}
}
