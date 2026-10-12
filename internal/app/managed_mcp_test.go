package app

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"github.com/Bodokh/my-whatsapp-mcp/internal/remoteaccess"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func managedTestApp(t *testing.T) *Application {
	t.Helper()
	a := unixNativeTestApp(t)
	if err := a.ConfigureManagedMCP(context.Background(), &ManagedMCPConfig{PublicURL: "https://mcp.example.com/mcp", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	return a
}
func managedCall(h http.Handler, token, tool string, args any) *httptest.ResponseRecorder {
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	r := httptest.NewRequest("POST", "https://mcp.example.com/mcp", strings.NewReader(string(payload)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func managedGrant(t *testing.T, a *Application) (localui.Connection, string) {
	t.Helper()
	expiry := time.Now().Add(time.Hour)
	c, err := a.CreateConnection(context.Background(), localui.ConnectionRequest{Client: "codex_cli", Label: "managed fixture", Mode: "remote", AllChats: true, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.RevealRemoteCredential(context.Background(), localui.ConnectionRevealRequest{ID: c.ID, Revision: c.Revision})
	if err != nil {
		t.Fatal(err)
	}
	return c, s.Token
}
func TestManagedMCPAuthorizationAndGeneration(t *testing.T) {
	a := managedTestApp(t)
	h := a.ManagedMCPHandler()
	if !a.ManagedMCPReady() || len(a.mcp.listeners) != 1 || a.mcpConfig.Remote.Listen != "" {
		t.Fatal("managed route opened listener or not ready")
	}
	c, token := managedGrant(t, a)
	page, err := a.Search(context.Background(), archive.SearchRequest{Query: "פגישה", Limit: 1})
	if err != nil || len(page.Hits) != 1 {
		t.Fatal("fixture search", err)
	}
	cases := map[string]any{"search_messages": map[string]any{"query": "פגישה"}, "get_context": map[string]any{"message_id": page.Hits[0].MessageID}, "list_chats": map[string]any{}, "get_index_status": map[string]any{}}
	for tool, args := range cases {
		w := managedCall(h, token, tool, args)
		if w.Code != 200 || strings.Contains(w.Body.String(), `"isError":true`) || strings.Contains(w.Body.String(), `"error":`) {
			t.Fatalf("%s: %d %s", tool, w.Code, w.Body.String())
		}
		for _, bad := range []string{"", "wrong"} {
			if w := managedCall(h, bad, tool, args); w.Code != 401 {
				t.Fatalf("%s bad credential: %d", tool, w.Code)
			}
		}
	}
	if err = a.ClearManagedMCP(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if a.ManagedMCPReady() {
		t.Fatal("clear left readiness")
	}
	if w := managedCall(h, token, "list_chats", map[string]any{}); w.Code != 503 || w.Body.Len() != 0 {
		t.Fatal("disabled response", w.Code, w.Body.String())
	}
	if _, err = a.CreateConnection(context.Background(), localui.ConnectionRequest{Client: "codex_cli", Label: "disabled", Mode: "remote", AllChats: true, ExpiresAt: c.ExpiresAt}); err == nil {
		t.Fatal("disabled grant created")
	}
	if err = a.ConfigureManagedMCP(context.Background(), &ManagedMCPConfig{PublicURL: "https://mcp.example.com/mcp", Generation: 2}); err != nil {
		t.Fatal(err)
	}
	if err = a.ClearManagedMCP(context.Background(), 1); err != nil || !a.ManagedMCPReady() {
		t.Fatal("stale release cleared current route", err)
	}
	if err = a.RevokeConnection(context.Background(), localui.ConnectionRevokeRequest{ID: c.ID, Revision: c.Revision}); err != nil {
		t.Fatal(err)
	}
	for tool, args := range cases {
		if w := managedCall(h, token, tool, args); w.Code != 401 {
			t.Fatalf("revoked %s: %d", tool, w.Code)
		}
	}
}
func TestManagedMCPPendingAndManualExclusion(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: shortNativeRoot(t), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	if err = a.ConfigureManagedMCP(context.Background(), &ManagedMCPConfig{PublicURL: "https://mcp.example.com/mcp", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if a.ManagedMCPReady() || len(a.authState.RemoteClients) != 0 {
		t.Fatal("unpaired route granted authority")
	}
	if w := managedCall(a.ManagedMCPHandler(), "private-canary", "list_chats", map[string]any{}); w.Code != 503 || w.Body.Len() != 0 {
		t.Fatal("pending response", w.Code, w.Body.String())
	}
	for _, url := range []string{"https://mcp.example.com/other", "https://127.0.0.1/mcp", "https://mcp.example.com/mcp?x=1"} {
		if err = a.ConfigureManagedMCP(context.Background(), &ManagedMCPConfig{PublicURL: url, Generation: 2}); err == nil {
			t.Fatal("invalid URL", url)
		}
	}
	manual := remoteConnectionTestApp(t)
	if err = manual.ConfigureManagedMCP(context.Background(), &ManagedMCPConfig{PublicURL: "https://mcp.example.com/mcp", Generation: 1}); err == nil {
		t.Fatal("manual and managed coexist")
	}
}

func TestManagedMCPNarrowingNativeAndExpired(t *testing.T) {
	a := managedTestApp(t)
	h := a.ManagedMCPHandler()
	c, token := managedGrant(t, a)
	page, err := a.Search(context.Background(), archive.SearchRequest{Query: "פגישה", Limit: 1})
	if err != nil || len(page.Hits) != 1 {
		t.Fatal("fixture", err)
	}
	hit := page.Hits[0]
	tools := map[string]any{"search_messages": map[string]any{"query": "פגישה"}, "get_context": map[string]any{"message_id": hit.MessageID}, "list_chats": map[string]any{}, "get_index_status": map[string]any{}}
	_, _, err = a.IssueNativeClientConfig(context.Background(), archive.CredentialCreate{Label: "native boundary", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, "", mcpserver.NativeAdapterEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	native, _, err := a.readNativeFile(a.authState.NativeClients[0].ConfigName)
	if err != nil {
		t.Fatal(err)
	}
	for tool, args := range tools {
		if w := managedCall(h, native.Token, tool, args); w.Code != 401 {
			t.Fatalf("native %s %d", tool, w.Code)
		}
	}
	rec := a.authState.RemoteClients[0]
	_, err = a.store.SetCredentialPolicy(context.Background(), rec.CredentialID, archive.CredentialPolicy{Account: rec.Account, AllChats: true, From: time.Now().Add(24 * time.Hour)}, c.Revision)
	if err != nil {
		t.Fatal(err)
	}
	w := managedCall(h, token, "get_context", tools["get_context"])
	if strings.Contains(w.Body.String(), string(hit.MessageID)) || strings.Contains(w.Body.String(), "פגישה") {
		t.Fatal("old context escaped narrowed scope", w.Body.String())
	}
	w = managedCall(h, token, "search_messages", tools["search_messages"])
	if w.Code != 200 || strings.Contains(w.Body.String(), string(hit.MessageID)) {
		t.Fatal("search escaped narrowing", w.Code, w.Body.String())
	}
	expiry := time.Now().Add(time.Second)
	exp, err := a.CreateConnection(context.Background(), localui.ConnectionRequest{Client: "codex_cli", Label: "expiring fixture", Mode: "remote", AllChats: true, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := a.RevealRemoteCredential(context.Background(), localui.ConnectionRevealRequest{ID: exp.ID, Revision: exp.Revision})
	if err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(time.Until(expiry.Add(time.Millisecond)))
	defer timer.Stop()
	<-timer.C
	for tool, args := range tools {
		if w := managedCall(h, secret.Token, tool, args); w.Code != 401 {
			t.Fatalf("expired %s %d", tool, w.Code)
		}
	}
}
func TestManagedMCPResetDrainsAndStableHandlerUsesNewArchive(t *testing.T) {
	a := managedTestApp(t)
	h := a.ManagedMCPHandler()
	_, token := managedGrant(t, a)
	entered, canceled, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	a.managedMCP.mu.Lock()
	a.managedMCP.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(canceled)
		<-release
		if _, err := a.store.Policy(context.Background()); err != nil {
			t.Error("reader closed before handler joined", err)
		}
	})
	a.managedMCP.mu.Unlock()
	go func() { defer close(done); managedCall(h, token, "list_chats", map[string]any{}) }()
	<-entered
	reset := make(chan error, 1)
	go func() { reset <- a.DeleteLocalData(context.Background(), true) }()
	<-canceled
	if err := a.ConfigureManagedMCP(context.Background(), &ManagedMCPConfig{PublicURL: "https://mcp.example.com/mcp", Generation: 2}); !errors.Is(err, remoteaccess.ErrBusy) {
		t.Fatal("configure during reset", err)
	}
	if err := a.Close(context.Background()); !errors.Is(err, remoteaccess.ErrBusy) {
		t.Fatal("close during reset", err)
	}
	select {
	case err := <-reset:
		t.Fatal("reset did not join", err)
	default:
	}
	close(release)
	<-done
	if err := <-reset; err != nil {
		t.Fatal(err)
	}
	if w := managedCall(h, token, "list_chats", map[string]any{}); w.Code != 503 || w.Body.Len() != 0 {
		t.Fatal("old handler retained reset archive")
	}
	if err := a.ConfigureManagedMCP(context.Background(), &ManagedMCPConfig{PublicURL: "https://mcp.example.com/mcp", Generation: 2}); err != nil {
		t.Fatal(err)
	}
	policy, err := a.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.seedDemo(context.Background(), policy.Generation); err != nil {
		t.Fatal(err)
	}
	a.ensureMCP(context.Background())
	_, newToken := managedGrant(t, a)
	if w := managedCall(h, token, "list_chats", map[string]any{}); w.Code != 401 {
		t.Fatal("old token survived reset", w.Code)
	}
	if w := managedCall(h, newToken, "list_chats", map[string]any{}); w.Code != 200 {
		t.Fatal("stable handler did not select new archive", w.Code)
	}
}
func TestManagedMCPCloseDeadlineRetainsResourcesAndFencesConfiguration(t *testing.T) {
	a := managedTestApp(t)
	h := a.ManagedMCPHandler()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a.managedMCP.mu.Lock()
	a.managedMCP.handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-release })
	a.managedMCP.mu.Unlock()
	go func() { defer close(done); managedCall(h, "", "list_chats", map[string]any{}) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := a.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("close did not honor deadline", err)
	}
	if _, err := a.store.Policy(context.Background()); err != nil || a.CleanupComplete() {
		t.Fatal("pending close released resources", err)
	}
	if err := a.ConfigureManagedMCP(context.Background(), &ManagedMCPConfig{PublicURL: "https://mcp.example.com/mcp", Generation: 2}); !errors.Is(err, remoteaccess.ErrBusy) {
		t.Fatal("pending close not fenced", err)
	}
	close(release)
	<-done
	if err := a.Close(context.Background()); err != nil || !a.CleanupComplete() {
		t.Fatal("joined close failed", err)
	}
}

type appRemoteSession struct {
	listener net.Listener
	done     chan error
	fail     atomic.Bool
	closes   atomic.Int32
}

func (s *appRemoteSession) Origin() string         { return "https://mcp.example.com" }
func (s *appRemoteSession) Listener() net.Listener { return s.listener }
func (s *appRemoteSession) Done() <-chan error     { return s.done }
func (s *appRemoteSession) Close(context.Context) error {
	s.closes.Add(1)
	_ = s.listener.Close()
	if s.fail.Load() {
		return errors.New("synthetic close failure")
	}
	return nil
}

type appRemoteAdapter struct{ sessions chan *appRemoteSession }

func (a *appRemoteAdapter) Open(ctx context.Context, _ remoteaccess.Intent, _ remoteaccess.Secret, _ func(remoteaccess.Notice)) (remoteaccess.Session, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &appRemoteSession{listener: l, done: make(chan error, 1)}
	a.sessions <- s
	return s, nil
}
func appRemoteIntent(browser bool) remoteaccess.Intent {
	return remoteaccess.Intent{Version: 1, Provider: remoteaccess.Ngrok, Enabled: true, ExposeBrowser: browser, SavedOrigin: "https://mcp.example.com"}
}
func appRemoteRequest(t *testing.T, s *appRemoteSession, path string) int {
	t.Helper()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	req, _ := http.NewRequest("GET", "http://"+s.listener.Addr().String()+path, nil)
	req.Host = "mcp.example.com"
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return res.StatusCode
}
func TestManagedManagerFailureNeverReleasesArchive(t *testing.T) {
	a := unixNativeTestApp(t)
	adapter := &appRemoteAdapter{sessions: make(chan *appRemoteSession, 1)}
	manager, err := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: adapter}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Configure(context.Background(), appRemoteIntent(false), nil); err != nil {
		t.Fatal(err)
	}
	session := <-adapter.sessions
	if code := appRemoteRequest(t, session, "/mcp"); code != 405 {
		t.Fatal("manager request", code)
	}
	if !manager.Status().MCPReady {
		t.Fatal("paired ready not reflected")
	}
	session.fail.Store(true)
	if err = a.Close(context.Background()); err == nil {
		t.Fatal("failed manager close acknowledged")
	}
	if _, err = a.Search(context.Background(), archive.SearchRequest{Query: "פגישה", Limit: 1}); err != nil || a.CleanupComplete() {
		t.Fatal("provider failure stopped local data", err)
	}
	expiry := time.Now().Add(time.Hour)
	if _, err = a.CreateConnection(context.Background(), localui.ConnectionRequest{Client: "codex_cli", Label: "after failure", Mode: "remote", AllChats: true, ExpiresAt: &expiry}); err == nil {
		t.Fatal("failed shutdown admitted new grant")
	}
	session.fail.Store(false)
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestManagedOwnerFactoryOutsideLocksAndLifecycleReentry(t *testing.T) {
	a := unixNativeTestApp(t)
	adapter := &appRemoteAdapter{sessions: make(chan *appRemoteSession, 1)}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	manager, err := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: adapter}, func(ctx context.Context, i remoteaccess.Intent, origin string, g uint64) (http.Handler, error) {
		if origin != "https://mcp.example.com" || g == 0 {
			t.Error("factory authority")
		}
		if _, err := a.Setup(ctx); err != nil {
			t.Error("factory app reentry", err)
		}
		if err := a.Close(ctx); !errors.Is(err, remoteaccess.ErrBusy) {
			t.Error("factory lifecycle reentry", err)
		}
		once.Do(func() { close(entered) })
		<-release
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Configure(context.Background(), appRemoteIntent(true), nil); err != nil {
		t.Fatal(err)
	}
	session := <-adapter.sessions
	<-entered
	if err = a.DeleteLocalData(context.Background(), true); !errors.Is(err, remoteaccess.ErrBusy) {
		t.Fatal("delete during prepare", err)
	}
	close(release)
	if code := appRemoteRequest(t, session, "/"); code != 204 {
		t.Fatal("owner route", code)
	}
	if err = a.Close(context.Background()); err != nil {
		t.Fatal("close release deadlocked", err)
	}
}

func TestManagedManagerOnlineDoesNotBindAccount(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: shortNativeRoot(t), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	adapter := &appRemoteAdapter{sessions: make(chan *appRemoteSession, 1)}
	m, err := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: adapter}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Configure(context.Background(), appRemoteIntent(false), nil); err != nil {
		t.Fatal(err)
	}
	session := <-adapter.sessions
	if code := appRemoteRequest(t, session, "/mcp"); code != 503 {
		t.Fatal("unpaired route", code)
	}
	status := m.Status()
	if status.State != "online" || status.MCPReady {
		t.Fatal("provider online implied authority", status)
	}
	if account, err := a.store.BoundAccount(context.Background()); err != nil || account != "" || len(a.authState.RemoteClients) != 0 {
		t.Fatal("provider bound account", err)
	}
}
func TestManagedGenerationSurvivesManagerReconstruction(t *testing.T) {
	a := unixNativeTestApp(t)
	adapter := &appRemoteAdapter{sessions: make(chan *appRemoteSession, 1)}
	generations := make(chan uint64, 2)
	factory := func(ctx context.Context, i remoteaccess.Intent, origin string, g uint64) (http.Handler, error) {
		generations <- g
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), nil
	}
	start := func() *remoteaccess.Manager {
		m, err := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: adapter}, factory)
		if err != nil {
			t.Fatal(err)
		}
		if err = m.Configure(context.Background(), appRemoteIntent(true), nil); err != nil {
			t.Fatal(err)
		}
		if code := appRemoteRequest(t, <-adapter.sessions, "/"); code != 204 {
			t.Fatal("owner route", code)
		}
		return m
	}
	_ = start()
	old := <-generations
	staleRelease := func() error { return a.ClearManagedMCP(context.Background(), old) }
	if err := a.DeleteLocalData(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	policy, err := a.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.seedDemo(context.Background(), policy.Generation); err != nil {
		t.Fatal(err)
	}
	_ = start()
	current := <-generations
	if current <= old {
		t.Fatal("app generation was reused", old, current)
	}
	if err = staleRelease(); err != nil || !a.ManagedMCPReady() {
		t.Fatal("stale pre-reset release cleared new manager route", err)
	}
}
func waitRemoteOperation(t *testing.T, operation func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		err := operation(ctx)
		if err == nil {
			return
		}
		if !errors.Is(err, remoteaccess.ErrBusy) || ctx.Err() != nil {
			t.Fatal("remote operation", err)
		}
		runtime.Gosched()
	}
}
func TestManagedFailedPrepareReleaseCannotClearLaterRoute(t *testing.T) {
	a := unixNativeTestApp(t)
	adapter := &appRemoteAdapter{sessions: make(chan *appRemoteSession, 1)}
	generations := make(chan uint64, 2)
	var attempts atomic.Int32
	factory := func(ctx context.Context, i remoteaccess.Intent, origin string, g uint64) (http.Handler, error) {
		generations <- g
		if attempts.Add(1) == 1 {
			return nil, errors.New("synthetic owner failure")
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), nil
	}
	m, err := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: adapter}, factory)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Configure(context.Background(), appRemoteIntent(true), nil); err != nil {
		t.Fatal(err)
	}
	<-adapter.sessions
	old := <-generations
	waitRemoteOperation(t, m.Disable)
	if a.ManagedMCPReady() {
		t.Fatal("failed owner preparation published MCP")
	}
	if err = m.Configure(context.Background(), appRemoteIntent(true), nil); err != nil {
		t.Fatal(err)
	}
	if code := appRemoteRequest(t, <-adapter.sessions, "/"); code != 204 {
		t.Fatal(code)
	}
	current := <-generations
	if current <= old {
		t.Fatal("failed generation reused")
	}
	if err = a.ClearManagedMCP(context.Background(), old); err != nil || !a.ManagedMCPReady() {
		t.Fatal("failed generation release cleared current", err)
	}
}
func TestManagedRecoveryAndProviderDisablePreserveLocalSearch(t *testing.T) {
	a := unixNativeTestApp(t)
	adapter := &appRemoteAdapter{sessions: make(chan *appRemoteSession, 1)}
	m, err := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: adapter}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Configure(context.Background(), appRemoteIntent(false), nil); err != nil {
		t.Fatal(err)
	}
	if code := appRemoteRequest(t, <-adapter.sessions, "/mcp"); code != 405 {
		t.Fatal(code)
	}
	c, token := managedGrant(t, a)
	h := a.ManagedMCPHandler()
	a.ops.Lock()
	a.data.Lock()
	a.authState.RemoteInvalid = true
	_ = a.startMCP(context.Background())
	a.data.Unlock()
	a.ops.Unlock()
	if w := managedCall(h, token, "list_chats", map[string]any{}); w.Code != 503 || w.Body.Len() != 0 {
		t.Fatal("recovery response", w.Code)
	}
	if _, state := a.mcp.status(); state != "unavailable" {
		t.Fatal("recovery advertised ready transport", state)
	}
	a.ops.Lock()
	a.data.Lock()
	a.authState.RemoteInvalid = false
	_ = a.startMCP(context.Background())
	a.data.Unlock()
	a.ops.Unlock()
	if w := managedCall(h, token, "list_chats", map[string]any{}); w.Code != 200 {
		t.Fatal("recovery did not restore", w.Code)
	}
	waitRemoteOperation(t, m.Disable)
	if a.ManagedMCPReady() || len(a.authState.RemoteClients) != 1 {
		t.Fatal("disable erased retained policy or left ready")
	}
	if p, err := a.Search(context.Background(), archive.SearchRequest{Query: "פגישה", Limit: 1}); err != nil || len(p.Hits) != 1 {
		t.Fatal("disable stopped keyword", err)
	}
	if err = a.RevokeConnection(context.Background(), localui.ConnectionRevokeRequest{ID: c.ID, Revision: c.Revision}); err != nil {
		t.Fatal("disabled revoke", err)
	}
	if err = m.Configure(context.Background(), appRemoteIntent(false), nil); err != nil {
		t.Fatal(err)
	}
	if code := appRemoteRequest(t, <-adapter.sessions, "/mcp"); code != 405 {
		t.Fatal(code)
	}
	if w := managedCall(h, token, "list_chats", map[string]any{}); w.Code != 401 {
		t.Fatal("reconnect restored revoked token", w.Code)
	}
}

func TestManagedGenerationOverflowFailsClosed(t *testing.T) {
	a := unixNativeTestApp(t)
	adapter := &appRemoteAdapter{sessions: make(chan *appRemoteSession, 1)}
	a.remoteLifecycle.generation = math.MaxUint64
	var ownerCalls atomic.Int32
	m, err := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: adapter}, func(context.Context, remoteaccess.Intent, string, uint64) (http.Handler, error) {
		ownerCalls.Add(1)
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Configure(context.Background(), appRemoteIntent(true), nil); err != nil {
		t.Fatal(err)
	}
	<-adapter.sessions
	waitRemoteOperation(t, m.Disable)
	if ownerCalls.Load() != 0 || a.ManagedMCPReady() || a.remoteLifecycle.generation != math.MaxUint64 {
		t.Fatal("generation overflow admitted callback or reused identity")
	}
}

func TestManagedProviderInFlightShutdownKeepsArchiveUntilJoin(t *testing.T) {
	a := unixNativeTestApp(t)
	adapter := &appRemoteAdapter{sessions: make(chan *appRemoteSession, 1)}
	m, err := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{remoteaccess.Ngrok: adapter}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Configure(context.Background(), appRemoteIntent(false), nil); err != nil {
		t.Fatal(err)
	}
	session := <-adapter.sessions
	if code := appRemoteRequest(t, session, "/mcp"); code != 405 {
		t.Fatal(code)
	}
	entered, canceled, release, requestDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	a.managedMCP.mu.Lock()
	a.managedMCP.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(canceled)
		<-release
		if _, err := a.store.Policy(context.Background()); err != nil {
			t.Error("manager released reader before handler join", err)
		}
	})
	a.managedMCP.mu.Unlock()
	go func() { defer close(requestDone); _ = appRemoteRequest(t, session, "/mcp") }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- a.Close(ctx) }()
	<-canceled
	if err = a.DeleteLocalData(context.Background(), true); !errors.Is(err, remoteaccess.ErrBusy) {
		t.Fatal("delete raced active close", err)
	}
	if err = <-closed; err == nil {
		t.Fatal("pending provider shutdown succeeded")
	}
	if a.CleanupComplete() {
		t.Fatal("pending manager allowed teardown")
	}
	if _, err = a.Search(context.Background(), archive.SearchRequest{Query: "פגישה", Limit: 1}); err != nil {
		t.Fatal("pending manager stopped local reads", err)
	}
	if err = a.ConfigureManagedMCP(context.Background(), &ManagedMCPConfig{PublicURL: "https://mcp.example.com/mcp", Generation: 99}); !errors.Is(err, remoteaccess.ErrBusy) {
		t.Fatal("pending manager admitted configure", err)
	}
	close(release)
	<-requestDone
	waitRemoteOperation(t, a.Close)
	if !a.CleanupComplete() {
		t.Fatal("joined manager did not allow cleanup")
	}
}

// Exercise public mutations that reconcile directly, without routing their
// failures through startManagedMCP or changing eligibility flags in the test.
func TestManagedRevokeRecoveryAdmissionAndRestoration(t *testing.T) {
	a := managedTestApp(t)
	h := a.ManagedMCPHandler()
	revoked, revokedToken := managedGrant(t, a)
	_, survivingToken := managedGrant(t, a)
	record := a.authState.RemoteClients[1]
	var payload []byte
	a.ops.Lock()
	a.data.Lock()
	err := appdata.WithClients(a.paths, a.lease, func(root *os.Root) error { var err error; payload, err = root.ReadFile(record.ConfigName); return err })
	if err == nil {
		err = a.removeRemoteFile(record)
	}
	a.data.Unlock()
	a.ops.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.RevokeConnection(context.Background(), localui.ConnectionRevokeRequest{ID: revoked.ID, Revision: revoked.Revision}); !errors.Is(err, localui.ErrConnectionRecovery) {
		t.Fatal("missing surviving journal must fail revoke recovery", err)
	}
	calls := managedRecoveryToolCalls(t, a)
	assertManagedRecoveryPending(t, a, h, survivingToken, calls)
	// Restore the exact owned credential artifact; successful normal app recovery
	// must preserve the surviving grant and keep the first grant revoked.
	a.ops.Lock()
	a.data.Lock()
	err = appdata.WithClients(a.paths, a.lease, func(root *os.Root) error {
		file, err := root.OpenFile(record.ConfigName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(payload)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
		return syncNativeDirectory(root)
	})
	a.data.Unlock()
	a.ops.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	a.ensureMCP(context.Background())
	assertManagedRecoveryRestored(t, a, h, survivingToken, calls)
	for tool, args := range calls {
		if w := managedCall(h, revokedToken, tool, args); w.Code != 401 {
			t.Fatalf("recovery revived revoked %s: status=%d", tool, w.Code)
		}
	}
}
func TestManagedIssuanceRecoveryAdmissionAndRestoration(t *testing.T) {
	a := managedTestApp(t)
	h := a.ManagedMCPHandler()
	_, token := managedGrant(t, a)
	expiry := time.Now().Add(time.Hour)
	_, err := a.CreateConnection(context.Background(), localui.ConnectionRequest{Client: "codex_cli", Label: "unknown chat issuance", Mode: "remote", Chats: []archive.ChatID{"missing-chat-fixture"}, ExpiresAt: &expiry})
	if !errors.Is(err, localui.ErrConnectionRecovery) {
		t.Fatal("unknown chat must fail credential issuance after journal intent", err)
	}
	calls := managedRecoveryToolCalls(t, a)
	assertManagedRecoveryPending(t, a, h, token, calls)
	a.ensureMCP(context.Background())
	assertManagedRecoveryRestored(t, a, h, token, calls)
	snapshot, err := a.Connections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Items) != 1 {
		t.Fatal("failed issuance survived recovery", len(snapshot.Items))
	}
}
func managedRecoveryToolCalls(t *testing.T, a *Application) map[string]any {
	t.Helper()
	page, err := a.Search(context.Background(), archive.SearchRequest{Query: "פגישה", Limit: 1})
	if err != nil || len(page.Hits) != 1 {
		t.Fatal("recovery stopped local search", err)
	}
	return map[string]any{"search_messages": map[string]any{"query": "פגישה"}, "get_context": map[string]any{"message_id": page.Hits[0].MessageID}, "list_chats": map[string]any{}, "get_index_status": map[string]any{}}
}
func assertManagedRecoveryPending(t *testing.T, a *Application, h http.Handler, token string, calls map[string]any) {
	t.Helper()
	if a.ManagedMCPReady() {
		t.Fatal("failed credential mutation left readiness")
	}
	for tool, args := range calls {
		w := managedCall(h, token, tool, args)
		if w.Code != 503 || w.Body.Len() != 0 {
			t.Errorf("recovery %s must return empty503: status=%d bytes=%d", tool, w.Code, w.Body.Len())
		}
	}
}
func assertManagedRecoveryRestored(t *testing.T, a *Application, h http.Handler, token string, calls map[string]any) {
	t.Helper()
	if !a.ManagedMCPReady() {
		t.Fatal("successful reconciliation did not restore readiness")
	}
	for tool, args := range calls {
		w := managedCall(h, token, tool, args)
		if w.Code != 200 || strings.Contains(w.Body.String(), `"isError":true`) || strings.Contains(w.Body.String(), `"error":`) {
			t.Errorf("recovered %s failed: status=%d", tool, w.Code)
		}
	}
}
