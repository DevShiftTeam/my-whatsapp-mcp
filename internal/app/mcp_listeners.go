package app

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// MCPConfig is trusted composition, never owner HTTP input. Native transport
// always uses the protected Unix socket; remote HTTP is explicit.
type MCPConfig struct{ Remote *RemoteMCPConfig }
type RemoteMCPConfig struct {
	Listen              string
	Server              mcpserver.Config
	PublicURL           string
	HeaderAuthAvailable bool
	BearerTokenEnv      string
}

var errMCPListener = errors.New("mcp_listener_unavailable")

type mcpListeners struct {
	mu                            sync.Mutex
	servers                       []*http.Server
	listeners                     []net.Listener
	native                        *appdata.NativeSocket
	workers                       sync.WaitGroup
	localState, remoteState, code string
	localStarted, remoteStarted   bool
	remoteHandler                 http.Handler
}

func copyMCPConfig(cfg *MCPConfig) MCPConfig {
	if cfg == nil || cfg.Remote == nil {
		return MCPConfig{}
	}
	remote := *cfg.Remote
	remote.Server.Hosts = append([]string(nil), remote.Server.Hosts...)
	remote.Server.Origins = append([]string(nil), remote.Server.Origins...)
	remote.Server.Access.Identities = make(map[string]archive.CredentialID, len(cfg.Remote.Server.Access.Identities))
	for key, value := range cfg.Remote.Server.Access.Identities {
		remote.Server.Access.Identities[key] = value
	}
	return MCPConfig{Remote: &remote}
}
func literalMCPLoopback(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" || strings.Contains(host, "%") {
		return false
	}
	parsed, err := netip.ParseAddr(host)
	if err != nil || !parsed.IsLoopback() {
		return false
	}
	endpoint, err := netip.ParseAddrPort(address)
	return err == nil && endpoint.Addr() == parsed && endpoint.String() == address
}
func newMCPLoopback(ctx context.Context, address string) (net.Listener, error) {
	if !literalMCPLoopback(address) {
		return nil, errMCPListener
	}
	var cfg net.ListenConfig
	listener, err := cfg.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, errMCPListener
	}
	return listener, nil
}
func (m *mcpListeners) status() (string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.localState, m.remoteState
}
func (m *mcpListeners) serve(ctx context.Context, listener net.Listener, handler http.Handler) {
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, BaseContext: func(net.Listener) context.Context { return ctx }}
	m.servers = append(m.servers, server)
	m.listeners = append(m.listeners, listener)
	m.workers.Add(1)
	go func() { defer m.workers.Done(); _ = server.Serve(listener) }()
}
func (m *mcpListeners) close(ctx context.Context) error {
	for _, listener := range m.listeners {
		_ = listener.Close()
	}
	if m.native != nil {
		_ = m.native.Listener().Close()
	}
	for _, server := range m.servers {
		if err := server.Shutdown(ctx); err != nil {
			return errMCPListener
		}
	}
	done := make(chan struct{})
	go func() { m.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return errMCPListener
	}
	if m.native != nil {
		if err := m.native.Close(); err != nil {
			return errMCPListener
		}
		m.native = nil
	}
	m.mu.Lock()
	m.localState = "unavailable"
	m.remoteState = "unavailable"
	m.mu.Unlock()
	return nil
}

// startMCP runs under the application's lifecycle/data lock or before publication.
func (a *Application) startMCP(ctx context.Context) error {
	if !a.mcpEnabled || a.closed {
		return errMCPListener
	}
	account, err := a.store.BoundAccount(ctx)
	if err != nil || account == "" {
		return errMCPListener
	}
	if a.mcp == nil {
		a.mcp = &mcpListeners{localState: "unavailable", remoteState: "not_configured"}
	}
	localErr := a.startLocalMCP(ctx)
	remoteErr := a.startRemoteMCP(ctx)
	local, remote := a.mcp.status()
	if local == "ready" || remote == "ready" {
		return nil
	}
	if localErr != nil {
		return localErr
	}
	return remoteErr
}

func (a *Application) startLocalMCP(ctx context.Context) error {
	m := a.mcp
	if m.localStarted && !a.nativeRecoveryPending && !a.authState.NativeInvalid {
		return nil
	}
	m.mu.Lock()
	m.localState = "unavailable"
	m.mu.Unlock()
	if m.native == nil {
		socket, err := appdata.ListenNativeSocket(a.queryContext, a.paths, a.lease)
		if err != nil {
			m.code = "native_transport_unavailable"
			return errMCPListener
		}
		m.native = socket
		a.nativeEndpoint = mcpserver.NativeEndpoint{SocketPath: socket.Path()}
	}
	if err := a.reconcileNativeClients(ctx); err != nil {
		return err
	}
	if !m.localStarted {
		cfg := mcpserver.Config{Outbound: a, Mode: mcpserver.LocalAdapter, Hosts: []string{"localhost"}, NativeClients: &a.nativeRegistry}
		handler, err := mcpserver.NewHandler(cfg, a.resources.retrieval, a.store)
		if err != nil {
			return errMCPListener
		}
		m.serve(a.queryContext, m.native.Listener(), handler)
		m.localStarted = true
		m.mu.Lock()
		m.localState = "ready"
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.localState = "ready"
	m.mu.Unlock()
	return nil
}

func (a *Application) startRemoteMCP(ctx context.Context) error {
	m := a.mcp
	if remote := a.mcpConfig.Remote; remote != nil {
		if m.remoteStarted && !a.remoteRecoveryPending && !a.authState.RemoteInvalid {
			return nil
		}
		m.mu.Lock()
		m.remoteState = "unavailable"
		m.mu.Unlock()
		if err := a.reconcileRemoteClients(ctx); err != nil {
			return err
		}
		if m.remoteStarted {
			m.mu.Lock()
			m.remoteState = "ready"
			m.mu.Unlock()
			return nil
		}
		if remote.Server.Mode != mcpserver.Bearer && remote.Server.Mode != mcpserver.Access {
			return errMCPListener
		}
		cfg := remote.Server
		cfg.Outbound = a
		cfg.NativeClients = &a.nativeRegistry
		cfg.RemoteClients = &a.remoteRegistry
		cfg.LocalCredentialID = ""
		handler, err := mcpserver.NewHandler(cfg, a.resources.retrieval, a.store)
		if err != nil {
			return errMCPListener
		}
		listener, err := newMCPLoopback(a.queryContext, remote.Listen)
		if err != nil {
			return err
		}
		m.serve(a.queryContext, listener, handler)
		m.remoteHandler = handler
		m.remoteStarted = true
		m.mu.Lock()
		m.remoteState = "ready"
		m.mu.Unlock()
	}
	return nil
}
func (a *Application) ensureMCP(ctx context.Context) {
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed || !a.mcpEnabled {
		return
	}
	if a.mcp != nil && a.mcp.localStarted && !a.nativeRecoveryPending {
		if a.mcpConfig.Remote == nil || (a.mcp.remoteStarted && !a.remoteRecoveryPending) {
			return
		}
	}
	_ = a.startMCP(ctx)
}
func (a *Application) closeMCP(ctx context.Context) error {
	a.nativeRegistry.replace(nil)
	a.remoteRegistry.replace(nil)
	if a.mcp == nil {
		return nil
	}
	// Reset and close revoke borrowed HTTP publication before teardown. Even a
	// failed shutdown must not leave a handler for the old archive reachable.
	a.mcp.remoteHandler = nil
	if err := a.mcp.close(ctx); err != nil {
		return err
	}
	a.mcp = nil
	a.nativeEndpoint = mcpserver.NativeEndpoint{}
	return nil
}
func nativeTransportPending(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("{\"error\":\"native_transport_pending\"}"))
}
