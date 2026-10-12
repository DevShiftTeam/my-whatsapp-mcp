package localui

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLegacyAndUnknownDispatchNeverGainNativeProviderAuthority(t *testing.T) {
	app := &fakeRemoteAccessApplication{fakeApplication: &fakeApplication{}}
	legacy, err := NewRemote(remoteConfig(t), app)
	if err != nil {
		t.Fatal(err)
	}
	owner := remoteLogin(t, legacy)
	for path, method := range routeMethods {
		if !nativeOnlyRoute(path) {
			continue
		}
		t.Run(path, func(t *testing.T) {
			for _, auth := range []*testSession{nil, &owner} {
				if rr := remoteRequest(legacy, method, path, `{}`, auth, nil); rr.Code != http.StatusNotFound {
					t.Fatalf("legacy outer boundary allowed native route: %d", rr.Code)
				}
			}
			// Prove the inner dispatcher remains safe even if an outer route is
			// added accidentally. A nil managed pointer grants no native authority.
			for _, dispatch := range []*Server{legacy.dispatch, {app: app}} {
				rr := httptest.NewRecorder()
				req := httptest.NewRequest(method, remoteTestOrigin+path, nil)
				dispatch.api(rr, req, "synthetic-csrf")
				if rr.Code != http.StatusNotFound {
					t.Fatalf("inner boundary allowed native route: %d", rr.Code)
				}
			}
		})
	}
	if app.total() != 0 {
		t.Fatal("native provider operation reached application from another realm")
	}
}

func TestManagedOwnerPreservesEverydayOperationsWithSessionAndCSRF(t *testing.T) {
	s, _, _ := remoteFixture(t)
	owner := managedPreservationSession(t, s)
	stats := &fakeApplication{}
	assertManagedEverydayOperation(t, s, owner, "/api/stats", `{}`, "stats", &statsTestApplication{fakeApplication: stats}, stats)
	voice := &fakeApplication{}
	assertManagedEverydayOperation(t, s, owner, "/api/voice/retry", `{}`, "voice_retry", &fakeVoiceRetryApplication{fakeApplication: voice}, voice)
	document := &fakeApplication{}
	assertManagedEverydayOperation(t, s, owner, "/api/documents/retry", `{"message_ids":["opaque-message"]}`, "document_retry", &fakeDocumentRetryApplication{fakeApplication: document}, document)
	permission := &fakeApplication{}
	app := &permissionApp{fakeApplication: permission}
	assertManagedEverydayOperation(t, s, owner, "/api/connections/send-permission", `{"id":"client_1","revision":1,"mode":"selected","chats":["chat_1"],"expected_send_revision":3}`, "send_permission_mode", app, permission)
	if app.id != "client_1" || app.expected != 3 || app.permission.Mode != "selected" {
		t.Fatal("managed permission edit lost the existing CAS or send scope")
	}
}

func assertManagedEverydayOperation(t *testing.T, s *Server, owner *testSession, path, body, call string, app Application, base *fakeApplication) {
	t.Helper()
	s.app = app
	for _, auth := range []*testSession{nil, owner} {
		rr := managedRemoteRequest(s, http.MethodPost, path, body, auth, func(r *http.Request) { r.Header.Del("X-CSRF-Token") })
		if rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
			t.Fatalf("%s accepted missing session/CSRF: %d", path, rr.Code)
		}
	}
	if rr := managedRemoteRequest(s, http.MethodPost, path, body, owner, func(r *http.Request) { r.Header.Del("Origin") }); rr.Code != http.StatusForbidden {
		t.Fatalf("%s accepted a mutation without origin: %d", path, rr.Code)
	}
	if base.total() != 0 {
		t.Fatal("denied managed everyday request reached application")
	}
	if rr := managedRemoteRequest(s, http.MethodPost, path, body, owner, nil); rr.Code != http.StatusOK || base.count(call) != 1 {
		t.Fatalf("%s lost existing everyday behavior: %d %s", path, rr.Code, rr.Body.String())
	}
}

func TestManagedOwnerAppearanceAssetsAndLegacyCookiesRemainSeparate(t *testing.T) {
	s, _, _ := remoteFixture(t)
	for _, path := range []string{"/assets/app.css", "/assets/app.js", "/assets/theme.js", "/assets/selects.js", "/assets/logo.png"} {
		rr := managedRemoteRequest(s, http.MethodGet, path, "", nil, nil)
		if rr.Code != http.StatusOK || rr.Body.Len() == 0 || len(rr.Result().Cookies()) != 0 {
			t.Fatalf("managed appearance asset unavailable or gained authority: %s", path)
		}
		if path == "/assets/logo.png" {
			assertPublicPNG(t, rr)
		}
	}
	owner := managedPreservationSession(t, s)
	legacy := remoteServer(t, &fakeApplication{})
	legacyOwner := remoteLogin(t, legacy)
	if owner.cookie.Name == legacyOwner.cookie.Name {
		t.Fatal("managed and legacy realms share an owner cookie")
	}
	if rr := managedRemoteRequest(s, http.MethodGet, "/api/status", "", &legacyOwner, nil); rr.Code != http.StatusUnauthorized {
		t.Fatal("legacy cookie authorized managed realm")
	}
	if rr := remoteRequest(legacy, http.MethodGet, "/api/status", "", owner, nil); rr.Code != http.StatusUnauthorized {
		t.Fatal("managed cookie authorized legacy realm")
	}
	// Native-only interfaces remain optional; an everyday implementation does
	// not have to expose provider operations to build a managed owner realm.
	var _ Application = &fakeApplication{}
}

// Route tests seed an ordinary live managed session without consuming the
// process-wide login attempt budget. Genuine challenge/password tests live in
// remote_login_test.go; no production limiter is reset or bypassed.
func managedPreservationSession(t *testing.T, s *Server) *testSession {
	t.Helper()
	token, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	csrf, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(token))
	s.mu.Lock()
	s.remote.sessions[digest] = remoteSession{session: session{digest: digest, csrf: csrf, expires: s.clock.nowLocked().Add(time.Hour)}, generation: s.remote.auth.Generation()}
	s.mu.Unlock()
	return &testSession{cookie: &http.Cookie{Name: remoteCookie, Value: token}, csrf: csrf}
}

func TestLegacyOwnerShutdownUsesSharedBudgetAndReportsUnjoinedRequest(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	app := &remoteBlockingApplication{fakeApplication: &fakeApplication{}, entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	defer close(app.release)
	cfg := remoteConfig(t)
	cfg.ListenAddress = listener.Addr().String()
	s, err := NewRemote(cfg, app)
	if err != nil {
		t.Fatal(err)
	}
	owner := remoteLogin(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budget, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- s.ServeWithShutdown(ctx, listener, func() context.Context { return budget }) }()
	go func() {
		req, err := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+"/api/status", nil)
		if err != nil {
			return
		}
		req.Host = "archive.example.com"
		req.AddCookie(owner.cookie)
		req.Header.Set("X-CSRF-Token", owner.csrf)
		client := &http.Client{Timeout: time.Second}
		reply, err := client.Do(req)
		if err == nil {
			reply.Body.Close()
		}
	}()
	select {
	case <-app.entered:
	case <-time.After(time.Second):
		t.Fatal("legacy request was not admitted")
	}
	cancel()
	select {
	case err := <-served:
		if !errors.Is(err, ErrShutdownIncomplete) {
			t.Fatalf("unjoined legacy request treated as clean shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("legacy shutdown replaced the shared budget or waited without bound")
	}
	if rr := remoteRequest(s, http.MethodGet, "/api/status", "", &owner, nil); rr.Code != http.StatusUnauthorized {
		t.Fatal("legacy owner admission reopened after incomplete shutdown")
	}
}
