package remoteaccess

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type clock interface {
	After(time.Duration) <-chan time.Time
}
type realClock struct{}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type generation struct {
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	cleanup    func(context.Context) error
	cleanupRun *cleanupAttempt
}

type cleanupAttempt struct {
	done chan struct{}
	err  error
}

// Manager uses a nonblocking operation gate, never a held operation mutex.
// Application callbacks run without mutexes; lifecycle reentry returns ErrBusy
// immediately. Status/OwnerNotice may be called by Prepare/Release.
type Manager struct {
	admission    sync.Mutex // protects callback admission against lifecycle acquisition
	ops          atomic.Bool
	callbacks    atomic.Int32
	stateBlocked bool
	mu           sync.Mutex
	cfg          ManagerConfig
	clock        clock
	intent       Intent
	secret       Secret
	snapshot     Snapshot
	notice       Notice
	generation   uint64
	run          *generation
	closed       bool
}

func NewManager(cfg ManagerConfig) (*Manager, error) { return newManager(cfg, realClock{}) }
func newManager(cfg ManagerConfig, c clock) (*Manager, error) {
	if cfg.Store == nil || cfg.Prepare == nil || c == nil {
		return nil, ErrInvalidIntent
	}
	adapters := make(map[Provider]Adapter, len(cfg.Adapters))
	for p, a := range cfg.Adapters {
		adapters[p] = a
	}
	cfg.Adapters = adapters
	m := &Manager{cfg: cfg, clock: c, snapshot: Snapshot{State: "disabled", Code: "remote_disabled"}}
	intent, secret, err := cfg.Store.Load(context.Background())
	if err != nil || intent.Version != 0 && validateIntent(intent) != nil || len(secret) > maxSecretBytes {
		m.stateBlocked = true
		m.snapshot.State = "needs_setup"
		m.snapshot.Code = ErrState.Error()
		return m, nil
	}
	if intent.Version == 0 {
		return m, nil
	}
	m.intent = intent
	clear(m.secret)
	m.secret = append(Secret{}, secret...)
	m.snapshot = baseSnapshot(intent)
	if intent.Enabled {
		m.start()
	}
	return m, nil
}
func baseSnapshot(i Intent) Snapshot {
	return Snapshot{Provider: i.Provider, State: "disabled", Code: "remote_disabled", SavedOrigin: i.SavedOrigin, BrowserEnabled: i.Enabled && i.ExposeBrowser}
}
func (m *Manager) Configure(ctx context.Context, intent Intent, secret Secret) error {
	if validateIntent(intent) != nil || len(secret) > maxSecretBytes {
		return ErrInvalidIntent
	}
	if err := m.beginOperation(ctx); err != nil {
		return err
	}
	defer m.ops.Store(false)
	m.mu.Lock()
	closed := m.closed
	old := m.intent
	blocked := m.stateBlocked
	m.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if blocked {
		return ErrState
	}
	if err := compatibleIntent(old, intent); err != nil {
		return err
	}
	if err := m.stop(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	old = m.intent
	m.mu.Unlock()
	if err := compatibleIntent(old, intent); err != nil {
		return err
	}
	if old.SavedOrigin != "" {
		intent.SavedOrigin = old.SavedOrigin
	}
	if ctx.Err() != nil {
		return ErrTransient
	}
	if err := m.cfg.Store.Save(ctx, intent, secret); err != nil {
		m.recoverSaveFailure()
		return ErrState
	}
	m.mu.Lock()
	m.intent = intent
	clear(m.secret)
	m.secret = append(Secret{}, secret...)
	m.snapshot = baseSnapshot(intent)
	m.notice = Notice{}
	m.mu.Unlock()
	if intent.Enabled {
		m.start()
	}
	return nil
}
func compatibleIntent(old, next Intent) error {
	if old.Version != 0 && (old.Provider != next.Provider || old.NodeName != next.NodeName) {
		return ErrIdentityChanged
	}
	if old.SavedOrigin != "" && next.SavedOrigin != "" && old.SavedOrigin != next.SavedOrigin {
		return ErrIdentityChanged
	}
	return nil
}
func (m *Manager) Disable(ctx context.Context) error {
	if err := m.beginOperation(ctx); err != nil {
		return err
	}
	defer m.ops.Store(false)
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if err := m.stop(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	i := m.intent
	secret := append(Secret{}, m.secret...)
	m.mu.Unlock()
	if i.Version != 0 {
		i.Enabled = false
		if ctx.Err() != nil {
			return ErrTransient
		}
		if m.cfg.Store.Save(ctx, i, secret) != nil {
			m.recoverSaveFailure()
			return ErrState
		}
	}
	m.mu.Lock()
	m.intent = i
	m.snapshot = baseSnapshot(i)
	m.notice = Notice{}
	m.mu.Unlock()
	return nil
}
func (m *Manager) Remove(ctx context.Context) error {
	if err := m.beginOperation(ctx); err != nil {
		return err
	}
	defer m.ops.Store(false)
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if err := m.stop(ctx); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ErrTransient
	}
	if m.cfg.Store.Remove(ctx) != nil {
		m.fixedFailure(ErrState)
		return ErrState
	}
	m.mu.Lock()
	m.intent = Intent{}
	m.stateBlocked = false
	clear(m.secret)
	m.secret = nil
	m.snapshot = baseSnapshot(Intent{})
	m.notice = Notice{}
	m.mu.Unlock()
	return nil
}
func (m *Manager) Close(ctx context.Context) error {
	if err := m.beginOperation(ctx); err != nil {
		return err
	}
	defer m.ops.Store(false)
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	if err := m.stop(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.snapshot.State = "disabled"
	m.snapshot.Code = "remote_closed"
	m.snapshot.MCPReady = false
	m.notice = Notice{}
	clear(m.secret)
	m.secret = nil
	m.mu.Unlock()
	return nil
}
func (m *Manager) Status() Snapshot {
	// Admit the readiness callback and acquire lifecycle ownership under the same
	// short lock. No app callback runs while admission or state is locked.
	m.admission.Lock()
	m.mu.Lock()
	snapshot := m.snapshot
	generation := m.generation
	ready := m.cfg.MCPReady
	admitted := snapshot.State == "online" && ready != nil && !m.closed && !m.ops.Load() && m.callbacks.Load() == 0
	if admitted {
		m.callbacks.Add(1)
	}
	m.mu.Unlock()
	m.admission.Unlock()
	if admitted {
		func() { defer m.callbacks.Add(-1); snapshot.MCPReady = ready() }()
	}
	m.mu.Lock()
	if generation != m.generation || m.snapshot.State != "online" {
		snapshot = m.snapshot
		snapshot.MCPReady = false
	}
	m.mu.Unlock()
	return snapshot
}
func (m *Manager) OwnerNotice() Notice { m.mu.Lock(); defer m.mu.Unlock(); return m.notice }
func (m *Manager) start() {
	m.mu.Lock()
	m.generation++
	ctx, cancel := context.WithCancel(context.Background())
	g := &generation{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	m.run = g
	i := m.intent
	key := append(Secret{}, m.secret...)
	m.snapshot.State = "connecting"
	m.snapshot.Code = "remote_connecting"
	m.mu.Unlock()
	go func() { defer close(g.done); defer g.cancel(); defer clear(key); m.worker(g, i, key) }()
}
func (m *Manager) stop(ctx context.Context) error {
	m.mu.Lock()
	g := m.run
	if g != nil {
		g.cancel()
		m.snapshot.State = "disabled"
		m.snapshot.Code = "remote_stopping"
		m.snapshot.MCPReady = false
		m.notice = Notice{}
	}
	m.mu.Unlock()
	if g == nil {
		return nil
	}
	select {
	case <-g.done:
	case <-ctx.Done():
		return ErrTransient
	}
	if g.cleanup != nil {
		if ctx.Err() != nil {
			return ErrTransient
		}
		// Only the operation owner touches cleanupRun. A caller timeout leaves the
		// old worker/attempt attached, so a later caller joins it instead of starting
		// another cleanup. The retry receives caller cancellation but retains actual
		// ownership even if a provider/app callback delays its return.
		if g.cleanupRun == nil {
			attempt := &cleanupAttempt{done: make(chan struct{})}
			g.cleanupRun = attempt
			cleanup := g.cleanup
			go func() { defer close(attempt.done); attempt.err = cleanup(ctx) }()
		}
		attempt := g.cleanupRun
		select {
		case <-attempt.done:
		case <-ctx.Done():
			return ErrTransient
		}
		g.cleanupRun = nil
		if attempt.err != nil {
			m.fixedFailure(ErrPrepare)
			return ErrPrepare
		}
		g.cleanup = nil
	}
	m.mu.Lock()
	if m.run == g {
		m.run = nil
		m.generation++
	}
	m.mu.Unlock()
	return nil
}
func (m *Manager) fixedFailure(err error) {
	state, code, _ := diagnostic(err)
	m.mu.Lock()
	m.snapshot.State = state
	m.snapshot.Code = code
	m.snapshot.MCPReady = false
	m.notice = Notice{}
	m.mu.Unlock()
}
func (m *Manager) update(g *generation, state, code string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.run != g || g.ctx.Err() != nil {
		return
	}
	m.snapshot.State = state
	m.snapshot.Code = code
	m.snapshot.MCPReady = false
	authURL := ""
	if state == "permission_required" || state == "needs_setup" {
		authURL = m.notice.AuthURL
	}
	m.notice = Notice{State: state, Code: code, AuthURL: authURL}
}
func (m *Manager) notify(g *generation, attempt uint64, n Notice) {
	state, code := "connecting", "remote_connecting"
	switch n.State {
	case "needs_setup":
		state, code = "needs_setup", "remote_needs_setup"
	case "permission_required":
		state, code = "permission_required", ErrPermissionRequired.Error()
	}
	authURL := ""
	if validAuthURL(n.AuthURL) {
		authURL = n.AuthURL
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.run != g || m.generation != attempt || g.ctx.Err() != nil {
		return
	}
	m.notice = Notice{State: state, Code: code, AuthURL: authURL}
	// Provider notices never declare the managed application online or ready.
	if m.snapshot.State != "online" {
		m.snapshot.State = state
		m.snapshot.Code = code
	}
}
func validAuthURL(value string) bool {
	u, err := url.Parse(value)
	return value != "" && len(value) <= 2048 && err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == "" && !strings.ContainsAny(value, "\r\n\x00")
}
func diagnostic(err error) (string, string, bool) {
	switch {
	case errors.Is(err, ErrCredentialsRejected):
		return "credentials_rejected", ErrCredentialsRejected.Error(), false
	case errors.Is(err, ErrPermissionRequired):
		return "permission_required", ErrPermissionRequired.Error(), false
	case errors.Is(err, ErrServiceLimit):
		return "service_limit_reached", ErrServiceLimit.Error(), false
	case errors.Is(err, ErrIdentityChanged):
		return "needs_setup", ErrIdentityChanged.Error(), false
	case errors.Is(err, ErrInvalidOrigin):
		return "needs_setup", ErrInvalidOrigin.Error(), false
	case errors.Is(err, ErrState):
		return "needs_setup", ErrState.Error(), false
	case errors.Is(err, ErrPrepare):
		return "needs_setup", ErrPrepare.Error(), false
	default:
		return "reconnecting", ErrTransient.Error(), true
	}
}
func (m *Manager) worker(g *generation, intent Intent, key Secret) {
	adapter := m.cfg.Adapters[intent.Provider]
	if adapter == nil {
		m.update(g, "needs_setup", "remote_adapter_unavailable")
		return
	}
	delay := time.Second
	for g.ctx.Err() == nil {
		m.mu.Lock()
		m.generation++
		attempt := m.generation
		m.notice = Notice{}
		m.mu.Unlock()
		session, err := adapter.Open(g.ctx, intent, append(Secret{}, key...), func(n Notice) { m.notify(g, attempt, n) })
		if err == nil && session == nil {
			err = ErrTransient
		}
		if err == nil && g.ctx.Err() == nil {
			origin, pinErr := PinOrigin(intent.SavedOrigin, session.Origin())
			err = pinErr
			if err == nil {
				intent.SavedOrigin = origin
				if m.cfg.Store.Save(g.ctx, intent, key) != nil {
					err = ErrState
					m.recoverSaveFailure()
				} else {
					m.mu.Lock()
					if m.run == g {
						m.intent = intent
						m.snapshot.SavedOrigin = origin
					}
					m.mu.Unlock()
				}
			}
			if err == nil && g.ctx.Err() == nil {
				err = m.serve(g, attempt, intent, session)
				session = nil
			}
		}
		if session != nil {
			if closeSession(session) != nil {
				failed := session
				g.cleanup = func(ctx context.Context) error { return closeSessionWithContext(ctx, failed) }
				err = ErrPrepare
			}
		}
		m.mu.Lock()
		if m.generation == attempt {
			m.generation++
		}
		m.mu.Unlock()
		if g.ctx.Err() != nil {
			return
		}
		state, code, retry := diagnostic(err)
		m.update(g, state, code)
		if !retry {
			return
		}
		select {
		case <-g.ctx.Done():
			return
		case <-m.clock.After(jitter(delay)):
		}
		if delay < time.Minute {
			delay *= 2
			if delay > time.Minute {
				delay = time.Minute
			}
		}
	}
}
func jitter(base time.Duration) time.Duration {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		base += time.Duration(binary.LittleEndian.Uint64(b[:]) % uint64(base/4+1))
	}
	if base > time.Minute {
		return time.Minute
	}
	return base
}

const shutdownTimeout = 5 * time.Second

func closeSession(s Session) error { return closeSessionWithContext(context.Background(), s) }
func closeSessionWithContext(parent context.Context, s Session) error {
	if l := s.Listener(); l != nil {
		_ = l.Close()
	}
	ctx, cancel := context.WithTimeout(parent, shutdownTimeout)
	defer cancel()
	if s.Close(ctx) != nil {
		return ErrPrepare
	}
	return nil
}
func (m *Manager) releaseHandlers(h Handlers) error {
	return m.releaseHandlersWithContext(context.Background(), h)
}
func (m *Manager) releaseHandlersWithContext(parent context.Context, h Handlers) error {
	if h.Release == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, shutdownTimeout)
	defer cancel()
	m.admitAppCallback()
	defer m.callbacks.Add(-1)
	if h.Release(ctx) != nil {
		return ErrPrepare
	}
	return nil
}
func (m *Manager) serve(g *generation, attempt uint64, intent Intent, session Session) error {
	serveCtx, cancelServe := context.WithCancel(g.ctx)
	defer cancelServe()
	listener := session.Listener()
	if listener == nil {
		if closeSession(session) != nil {
			g.cleanup = func(ctx context.Context) error { return closeSessionWithContext(ctx, session) }
		}
		return ErrPrepare
	}
	m.admitAppCallback()
	h, err := m.cfg.Prepare(serveCtx, intent, intent.SavedOrigin, attempt)
	m.callbacks.Add(-1)
	if err != nil || g.ctx.Err() != nil {
		if closeSession(session) != nil {
			g.cleanup = func(ctx context.Context) error { return closeSessionWithContext(ctx, session) }
		}
		if m.releaseHandlers(h) != nil {
			g.cleanup = func(ctx context.Context) error {
				if closeSessionWithContext(ctx, session) != nil {
					return ErrPrepare
				}
				return m.releaseHandlersWithContext(ctx, h)
			}
		}
		return ErrPrepare
	}
	router, err := newRouter(intent.SavedOrigin, intent.ExposeBrowser, h)
	if err != nil {
		if closeSession(session) != nil {
			g.cleanup = func(ctx context.Context) error { return closeSessionWithContext(ctx, session) }
		}
		if m.releaseHandlers(h) != nil {
			g.cleanup = func(ctx context.Context) error {
				if closeSessionWithContext(ctx, session) != nil {
					return ErrPrepare
				}
				return m.releaseHandlersWithContext(ctx, h)
			}
		}
		return ErrPrepare
	}
	active := &activeHandler{handler: router}
	server := &http.Server{Handler: active, BaseContext: func(net.Listener) context.Context { return serveCtx }, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384, ErrorLog: log.New(io.Discard, "", 0)}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	if intent.Provider == Cloudflare {
		m.update(g, "route_unverified", "remote_route_unverified")
	} else {
		m.update(g, "online", "remote_online")
	}
	joined := false
	select {
	case <-g.ctx.Done():
		err = g.ctx.Err()
	case failure, ok := <-session.Done():
		err = failure
		if !ok || err == nil {
			err = ErrTransient
		}
	case err = <-served:
		joined = true
	}
	cancelServe()
	active.stop()
	closeErr := closeSession(session)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	if server.Shutdown(shutdownCtx) != nil {
		_ = server.Close()
	}
	cancel()
	if !joined {
		<-served
	}
	// Even a misbehaving app handler cannot cause generation release before it
	// exits. Lifecycle callers may time out; replacement never starts early.
	active.wait.Wait()
	releaseErr := m.releaseHandlers(h)
	if closeErr != nil || releaseErr != nil {
		g.cleanup = func(ctx context.Context) error {
			if closeSessionWithContext(ctx, session) != nil {
				return ErrPrepare
			}
			return m.releaseHandlersWithContext(ctx, h)
		}
		return ErrPrepare
	}
	return err
}

type activeHandler struct {
	mu       sync.Mutex
	wait     sync.WaitGroup
	stopping bool
	handler  http.Handler
}

func (h *activeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.stopping {
		h.mu.Unlock()
		http.Error(w, "remote_stopping", 503)
		return
	}
	h.wait.Add(1)
	h.mu.Unlock()
	defer h.wait.Done()
	h.handler.ServeHTTP(w, r)
}
func (h *activeHandler) stop() { h.mu.Lock(); h.stopping = true; h.mu.Unlock() }

func (m *Manager) beginOperation(ctx context.Context) error {
	if ctx.Err() != nil {
		return ErrTransient
	}
	m.admission.Lock()
	defer m.admission.Unlock()
	if m.callbacks.Load() != 0 || !m.ops.CompareAndSwap(false, true) {
		return ErrBusy
	}
	return nil
}

// Prepare and Release belong to the current worker (or its retained cleanup),
// which lifecycle stop joins. Their admission makes callback reentry ErrBusy.
func (m *Manager) admitAppCallback() {
	m.admission.Lock()
	m.callbacks.Add(1)
	m.admission.Unlock()
}

// An error after the atomic rename can mean the new identity was committed.
// Refresh that complete identity without ever starting it. Corrupt state needs
// explicit Remove; Configure cannot silently replace unknown saved authority.
func (m *Manager) recoverSaveFailure() {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	intent, key, err := m.cfg.Store.Load(ctx)
	m.mu.Lock()
	if err != nil || intent.Version != 0 && validateIntent(intent) != nil || len(key) > maxSecretBytes {
		m.stateBlocked = true
	} else {
		m.intent = intent
		clear(m.secret)
		m.secret = append(Secret{}, key...)
		m.snapshot = baseSnapshot(intent)
	}
	m.mu.Unlock()
	m.fixedFailure(ErrState)
}
