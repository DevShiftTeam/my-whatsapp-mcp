package app

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"github.com/Bodokh/my-whatsapp-mcp/internal/remoteaccess"
)

// ManagedMCPConfig is trusted provider composition, never request input.
type ManagedMCPConfig struct {
	PublicURL           string
	HeaderAuthAvailable bool
	Generation          uint64
}

// Admission owns all references to a generation's reader. Retiring a route
// cancels requests and joins them before any archive resource may be released.
type managedMCP struct {
	mu               sync.Mutex
	generation       uint64
	active, draining bool
	handler          http.Handler
	ctx              context.Context
	cancel           context.CancelFunc
	requests         int
	drained          chan struct{}
}

func (a *Application) ConfigureManagedMCP(ctx context.Context, cfg *ManagedMCPConfig) error {
	if err := a.beginRemoteOperation(false); err != nil {
		return err
	}
	defer a.endRemoteOperation(false)
	return a.configureManagedMCP(ctx, cfg)
}
func (a *Application) configureManagedMCP(ctx context.Context, cfg *ManagedMCPConfig) error {
	if a.snapshot != nil {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	if cfg == nil || cfg.Generation == 0 || !strings.HasSuffix(cfg.PublicURL, "/mcp") {
		return remoteaccess.ErrInvalidOrigin
	}
	origin := strings.TrimSuffix(cfg.PublicURL, "/mcp")
	if _, err := remoteaccess.PinOrigin("", origin); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed || !a.mcpEnabled {
		return remoteaccess.ErrClosed
	}
	for _, reserved := range a.reservedRemoteOrigins {
		if reserved == origin {
			return remoteaccess.ErrInvalidOrigin
		}
	}
	if old := a.mcpConfig.Remote; old != nil && !old.Managed {
		return errMCPListener
	}
	a.remoteLifecycle.mu.Lock()
	if cfg.Generation > a.remoteLifecycle.generation {
		a.remoteLifecycle.generation = cfg.Generation
	}
	a.remoteLifecycle.mu.Unlock()
	m := &a.managedMCP
	m.mu.Lock()
	if m.draining || cfg.Generation <= m.generation {
		m.mu.Unlock()
		return remoteaccess.ErrBusy
	}
	if m.active {
		m.mu.Unlock()
		return remoteaccess.ErrBusy
	}
	m.generation = cfg.Generation
	m.active = true
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.mu.Unlock()
	a.mcpConfig.Remote = &RemoteMCPConfig{Managed: true, PublicURL: cfg.PublicURL, HeaderAuthAvailable: cfg.HeaderAuthAvailable, Server: mcpserver.Config{Mode: mcpserver.Bearer, Hosts: []string{strings.TrimPrefix(origin, "https://")}, Origins: []string{origin}}}
	if a.mcp == nil {
		root, cancel := context.WithCancel(context.Background())
		a.mcp = &mcpListeners{root: root, cancel: cancel, localState: "unavailable", remoteState: "unavailable"}
	}
	if a.mcp.attachments != nil {
		if err := a.mcp.attachments.setPublicURL(cfg.PublicURL); err != nil {
			return err
		}
	}
	// A configured transport is allowed before pairing. Readiness remains false
	// until startMCP can prove account binding and recover credential journals.
	_ = a.startMCP(ctx)
	return nil
}

func (a *Application) ManagedMCPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.remoteShutdownPending() {
			managedMCPPending(w)
			return
		}
		// Recovery eligibility and generation admission form one read frame.
		// Mutations hold data before changing recovery/journal state; never keep
		// either lock through serving, which owns the request count instead.
		a.data.RLock()
		m := &a.managedMCP
		m.mu.Lock()
		if !a.managedMCPReadyLocked() {
			m.mu.Unlock()
			a.data.RUnlock()
			managedMCPPending(w)
			return
		}
		h, routeCtx := m.handler, m.ctx
		if m.requests == 0 {
			m.drained = make(chan struct{})
		}
		m.requests++
		m.mu.Unlock()
		a.data.RUnlock()
		defer func() {
			m.mu.Lock()
			m.requests--
			if m.requests == 0 {
				close(m.drained)
			}
			m.mu.Unlock()
		}()
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(routeCtx, cancel)
		defer func() { stop(); cancel() }()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}
func managedMCPPending(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
}
func (a *Application) ManagedMCPReady() bool {
	a.data.RLock()
	defer a.data.RUnlock()
	m := &a.managedMCP
	m.mu.Lock()
	defer m.mu.Unlock()
	return a.managedMCPReadyLocked()
}

// Caller holds data (read or write) followed by managedMCP.mu. Sharing this
// predicate keeps readiness and request admission aligned for every recovery
// entry point, including direct credential issuance and revocation.
func (a *Application) managedMCPReadyLocked() bool {
	m := &a.managedMCP
	return !a.closed && !a.remoteShutdownPending() && m.active && m.handler != nil && !a.remoteRecoveryPending && !a.authState.RemoteInvalid
}

// Clear is the controlled manager Release callback. It deliberately bypasses
// lifecycle admission: shutdown owns that gate while waiting for this callback.
// No application lock is held while waiting for admitted requests to return.
func (a *Application) ClearManagedMCP(ctx context.Context, generation uint64) error {
	a.ops.Lock()
	a.data.Lock()
	m := &a.managedMCP
	m.mu.Lock()
	if m.generation != generation || generation == 0 {
		m.mu.Unlock()
		a.data.Unlock()
		a.ops.Unlock()
		return nil
	}
	m.active = false
	m.handler = nil
	m.draining = true
	if m.cancel != nil {
		m.cancel()
	}
	drained := m.drained
	pending := m.requests != 0
	a.remoteRegistry.replace(nil)
	if a.mcp != nil {
		a.mcp.remoteHandler = nil
		if a.mcp.attachments != nil {
			_ = a.mcp.attachments.setPublicURL("")
		}
		a.mcp.mu.Lock()
		a.mcp.remoteStarted = false
		a.mcp.remoteState = "unavailable"
		a.mcp.mu.Unlock()
	}
	m.mu.Unlock()
	a.data.Unlock()
	a.ops.Unlock()
	if pending {
		select {
		case <-drained:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.mu.Lock()
	if m.generation == generation {
		m.draining = false
	}
	m.mu.Unlock()
	return nil
}

// Called only with ops/data held. The managed mode never opens a TCP listener.
func (a *Application) startManagedMCP(ctx context.Context) error {
	a.mcp.mu.Lock()
	a.mcp.remoteState = "unavailable"
	a.mcp.mu.Unlock()
	if a.remoteShutdownPending() {
		return errMCPListener
	}
	m := &a.managedMCP
	m.mu.Lock()
	active := m.active
	m.mu.Unlock()
	if !active {
		return errMCPListener
	}
	if err := a.reconcileRemoteClients(ctx); err != nil {
		m.mu.Lock()
		m.handler = nil
		m.mu.Unlock()
		return err
	}
	cfg := a.mcpConfig.Remote.Server
	cfg.Outbound = a
	cfg.Attachments = a.mcp.attachments
	cfg.LocalCredentialID = ""
	cfg.NativeClients = &a.nativeRegistry
	cfg.RemoteClients = &a.remoteRegistry
	handler, err := mcpserver.NewHandler(cfg, a.resources.retrieval, a.store)
	if err != nil {
		return errMCPListener
	}
	handler = a.mcpRequestHandler(a.mcp, handler)
	m.mu.Lock()
	m.handler = handler
	m.mu.Unlock()
	a.mcp.remoteHandler = a.ManagedMCPHandler()
	a.mcp.mu.Lock()
	a.mcp.remoteStarted = true
	a.mcp.remoteState = "ready"
	a.mcp.mu.Unlock()
	return nil
}
