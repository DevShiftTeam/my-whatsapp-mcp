package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
)

func borrowedRemoteMCPTestApp(handler http.Handler) *Application {
	root, cancel := context.WithCancel(context.Background())
	return &Application{mcpEnabled: true, queryContext: context.Background(),
		mcpConfig: MCPConfig{Remote: &RemoteMCPConfig{Server: mcpserver.Config{Mode: mcpserver.Bearer}}},
		mcp:       &mcpListeners{root: root, cancel: cancel, remoteState: "ready", remoteStarted: true, remoteHandler: handler}}
}

func borrowedRemoteMCPRequest(token, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "https://mcp.example.com/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	return r
}

func TestRemoteMCPHandlerDeniesUnpublishedAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Application)
	}{
		{"closed", func(a *Application) { a.closed = true }},
		{"disabled", func(a *Application) { a.mcpEnabled = false }},
		{"missing_owner", func(a *Application) { a.mcp = nil }},
		{"not_ready", func(a *Application) { a.mcp.remoteState = "unavailable" }},
		{"not_configured", func(a *Application) { a.mcpConfig.Remote = nil }},
		{"wrong_auth_mode", func(a *Application) { a.mcpConfig.Remote.Server.Mode = mcpserver.LocalAdapter }},
		{"pending_recovery", func(a *Application) { a.remoteRecoveryPending = true }},
		{"invalid_journal", func(a *Application) { a.authState.RemoteInvalid = true }},
		{"missing_handler", func(a *Application) { a.mcp.remoteHandler = nil }},
		{"missing_lifetime", func(a *Application) { a.mcp.root = nil }},
		{"ended_lifetime", func(a *Application) {
			root, cancel := context.WithCancel(context.Background())
			cancel()
			a.mcp.root = root
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			a := borrowedRemoteMCPTestApp(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			test.mutate(a)
			response := httptest.NewRecorder()
			a.RemoteMCPHandler().ServeHTTP(response, borrowedRemoteMCPRequest("private-token-SENTINEL", "private-body-SENTINEL"))
			if response.Code != http.StatusServiceUnavailable || calls.Load() != 0 || strings.Contains(response.Body.String(), "SENTINEL") || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("unpublished remote transport reached private authority")
			}
		})
	}
}

func TestRemoteMCPHandlerSharesCurrentOwnedHandlerAndRechecksReset(t *testing.T) {
	var first, second atomic.Int32
	a := borrowedRemoteMCPTestApp(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	handler := a.RemoteMCPHandler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, borrowedRemoteMCPRequest("bounded", "{}"))
	if response.Code != http.StatusNoContent || first.Load() != 1 {
		t.Fatal("ready borrowed transport did not use the owned handler")
	}
	a.data.Lock()
	a.mcp.remoteHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second.Add(1)
		w.WriteHeader(http.StatusAccepted)
	})
	a.data.Unlock()
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, borrowedRemoteMCPRequest("bounded", "{}"))
	if response.Code != http.StatusAccepted || first.Load() != 1 || second.Load() != 1 {
		t.Fatal("borrowed transport cached a replaced resource handler")
	}
	a.data.Lock()
	if err := a.closeMCP(context.Background()); err != nil {
		a.data.Unlock()
		t.Fatal("bounded remote unpublication failed")
	}
	a.data.Unlock()
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, borrowedRemoteMCPRequest("bounded", "{}"))
	if response.Code != http.StatusServiceUnavailable || first.Load() != 1 || second.Load() != 1 {
		t.Fatal("reset kept a stale borrowed resource handler reachable")
	}
}

func TestRemoteMCPHandlerRejectsLifecycleContentionBeforeAdmission(t *testing.T) {
	var calls atomic.Int32
	a := borrowedRemoteMCPTestApp(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	a.data.Lock()
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.RemoteMCPHandler().ServeHTTP(response, borrowedRemoteMCPRequest("anonymous-SENTINEL", "private-body-SENTINEL"))
	}()
	select {
	case <-done:
		a.data.Unlock()
	case <-time.After(time.Second):
		a.data.Unlock()
		<-done
		t.Fatal("lifecycle contention queued public request before bounded admission")
	}
	if response.Code != http.StatusServiceUnavailable || calls.Load() != 0 || strings.Contains(response.Body.String(), "SENTINEL") || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("lifecycle contention reached archive authority")
	}
}

func TestMCPRequestUsesCurrentQueryAndRetainsTransportLifetime(t *testing.T) {
	for _, transport := range []string{"dedicated", "borrowed"} {
		t.Run(transport, func(t *testing.T) {
			entered, cancelled := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			owned := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(entered)
					<-r.Context().Done()
					close(cancelled)
				} else if r.Context().Err() != nil {
					t.Error("fresh query inherited an ended query context")
				}
				w.WriteHeader(http.StatusNoContent)
			})
			a := borrowedRemoteMCPTestApp(owned)
			defer a.mcp.cancel()
			query, cancelQuery := context.WithCancel(context.Background())
			defer cancelQuery()
			a.queryContext = query
			var handler http.Handler
			if transport == "dedicated" {
				handler = a.mcpRequestHandler(a.mcp, owned)
			} else {
				handler = a.RemoteMCPHandler()
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				handler.ServeHTTP(httptest.NewRecorder(), borrowedRemoteMCPRequest("bounded", "{}"))
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				cancelQuery()
				t.Fatal("request did not enter its captured handler")
			}
			a.data.Lock()
			cancelQuery()
			a.queryContext = context.Background()
			a.data.Unlock()
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("query cancellation did not stop in-flight MCP work")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("cancelled MCP work did not return")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, borrowedRemoteMCPRequest("bounded", "{}"))
			if response.Code != http.StatusNoContent || calls.Load() != 2 || a.mcp.root.Err() != nil {
				t.Fatal("query replacement ended the transport lifetime")
			}
			a.mcp.cancel()
			response = httptest.NewRecorder()
			handler.ServeHTTP(response, borrowedRemoteMCPRequest("bounded", "{}"))
			if response.Code != http.StatusServiceUnavailable || calls.Load() != 2 {
				t.Fatal("cancelled MCP lifetime retained handler authority")
			}
		})
	}
}

func TestRemoteMCPHandlerJoinsRequestCancellationBeforeResourceMutation(t *testing.T) {
	entered := make(chan struct{})
	released := make(chan struct{})
	cancelled := make(chan struct{})
	query, cancelQuery := context.WithCancel(context.Background())
	defer cancelQuery()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(released) }) }
	defer release()
	a := borrowedRemoteMCPTestApp(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(cancelled)
		<-released
		w.WriteHeader(http.StatusNoContent)
	}))
	a.queryContext = query
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		a.RemoteMCPHandler().ServeHTTP(httptest.NewRecorder(), borrowedRemoteMCPRequest("bounded", "{}"))
	}()
	t.Cleanup(func() {
		cancelQuery()
		release()
		select {
		case <-requestDone:
		case <-time.After(3 * time.Second):
			t.Error("borrowed handler did not join during cleanup")
		}
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("borrowed transport never entered the owned handler")
	}
	if !a.data.TryLock() {
		t.Fatal("borrowed provider work retained the resource read lock")
	}
	a.data.Unlock()
	closed := make(chan error, 1)
	closeJoined := make(chan struct{})
	closeContext, cancelClose := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelClose()
	go func() {
		defer close(closeJoined)
		a.data.Lock()
		defer a.data.Unlock()
		closed <- a.closeMCP(closeContext)
	}()
	t.Cleanup(func() {
		cancelQuery()
		cancelClose()
		release()
		select {
		case <-closeJoined:
		case <-time.After(3 * time.Second):
			t.Error("borrowed resource teardown did not join during cleanup")
		}
	})
	cancelQuery()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("archive query cancellation did not reach borrowed request")
	}
	select {
	case <-closed:
		t.Fatal("resource teardown skipped the borrowed handler acknowledgement")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("borrowed request did not return after acknowledgement")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal("resource teardown failed after handler acknowledgement")
		}
	case <-time.After(time.Second):
		t.Fatal("resource teardown did not join the borrowed request")
	}
	if !a.data.TryLock() {
		t.Fatal("finished teardown retained the resource lock")
	}
	a.data.Unlock()
}

func TestRemoteMCPHandlerPublishesOnlyRemoteScopedCredentials(t *testing.T) {
	a := remoteConnectionTestApp(t)
	ownerHits, err := a.Search(context.Background(), archive.SearchRequest{Query: "Atlas", Limit: 10})
	if err != nil || len(ownerHits.Hits) == 0 {
		t.Fatal("synthetic scoped fixture unavailable")
	}
	expiry := time.Now().Add(time.Hour)
	connection, err := a.CreateConnection(context.Background(), localui.ConnectionRequest{Client: "codex_cli", Label: "borrowed remote scoped", Mode: "remote", Chats: []archive.ChatID{ownerHits.Hits[0].ChatID}, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal("synthetic scoped credential creation failed")
	}
	secret, err := a.RevealRemoteCredential(context.Background(), localui.ConnectionRevealRequest{ID: connection.ID, Revision: connection.Revision})
	if err != nil {
		t.Fatal("synthetic scoped credential reveal failed")
	}
	_, _, err = a.IssueNativeClientConfig(context.Background(), archive.CredentialCreate{Label: "borrowed native separate", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: expiry}, "", mcpserver.NativeAdapterEndpoint)
	if err != nil {
		t.Fatal("synthetic native credential creation failed")
	}
	native, _, err := a.readNativeFile(a.authState.NativeClients[0].ConfigName)
	if err != nil {
		t.Fatal("synthetic native fixture unavailable")
	}
	handler := a.RemoteMCPHandler()
	call := func(token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, borrowedRemoteMCPRequest(token, body))
		return r
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"bounded-borrowed-test","version":"1"}}}`
	if status := call(secret.Token, initialize).Code; status != http.StatusOK {
		t.Fatalf("published remote credential did not initialize: status %d", status)
	}
	if status := call(native.Token, initialize).Code; status != http.StatusUnauthorized {
		t.Fatalf("native credential crossed borrowed remote transport: status %d", status)
	}
	allowed := call(secret.Token, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_messages","arguments":{"query":"Atlas","limit":10}}}`)
	if allowed.Code != http.StatusOK || !strings.Contains(allowed.Body.String(), "Project Atlas") {
		t.Fatal("scoped borrowed remote search lost permitted fixture")
	}
	excluded := call(secret.Token, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_messages","arguments":{"query":"Travel","limit":10}}}`)
	if excluded.Code != http.StatusOK || strings.Contains(excluded.Body.String(), "Travel planning") {
		t.Fatal("scoped borrowed remote search exposed excluded fixture")
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(excluded.Body.Bytes(), &envelope) != nil || envelope["error"] != nil {
		t.Fatal("excluded search failed instead of enforcing a readable empty scope")
	}
	if err = a.RevokeConnection(context.Background(), localui.ConnectionRevokeRequest{ID: connection.ID, Revision: connection.Revision}); err != nil {
		t.Fatal("bounded remote credential revocation failed")
	}
	if status := call(secret.Token, initialize).Code; status != http.StatusUnauthorized {
		t.Fatalf("revoked remote credential remained accepted: status %d", status)
	}
}
