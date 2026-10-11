package localui

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
)

const remoteTestOrigin = "https://archive.example.com"
const remoteTestPassword = "archive-owner-test-password"

var remoteTestHashOnce sync.Once
var remoteTestHash string
var remoteTestHashError error

func remoteConfig(t *testing.T) RemoteConfig {
	t.Helper()
	remoteTestHashOnce.Do(func() { remoteTestHash, remoteTestHashError = HashAdminPassword(remoteTestPassword) })
	if remoteTestHashError != nil {
		t.Fatal(remoteTestHashError)
	}
	return RemoteConfig{Origin: remoteTestOrigin, ListenAddress: "127.0.0.1:6969", PasswordHash: remoteTestHash}
}

func remoteServer(t *testing.T, app *fakeApplication) *RemoteServer {
	t.Helper()
	s, err := NewRemote(remoteConfig(t), app)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func remoteRequest(s *RemoteServer, method, path, body string, session *testSession, change func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, remoteTestOrigin+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", remoteTestOrigin)
	if session != nil {
		req.AddCookie(session.cookie)
		req.Header.Set("X-CSRF-Token", session.csrf)
	}
	if change != nil {
		change(req)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func remoteLogin(t *testing.T, s *RemoteServer) testSession {
	t.Helper()
	rr := remoteRequest(s, "POST", "/api/auth/login", `{"password":"`+remoteTestPassword+`"}`, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("login status %d", rr.Code)
	}
	var response struct {
		CSRF string `json:"csrf_token"`
	}
	if json.Unmarshal(rr.Body.Bytes(), &response) != nil || response.CSRF == "" {
		t.Fatal("missing login csrf")
	}
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing login cookie")
	}
	cookie := cookies[0]
	if cookie.Name != "__Host-wa_owner" || cookie.Path != "/" || cookie.Domain != "" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge <= 0 {
		t.Fatal("unsafe cookie attributes")
	}
	if strings.Contains(rr.Body.String(), cookie.Value) || strings.Contains(rr.Body.String(), remoteTestPassword) {
		t.Fatal("login response exposed credentials")
	}
	return testSession{cookie: cookie, csrf: response.CSRF}
}

func TestRemoteConfigurationFailsClosed(t *testing.T) {
	valid := remoteConfig(t)
	if err := ValidateRemoteConfig(valid); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		modify func(*RemoteConfig)
	}{
		{"missing_hash", func(c *RemoteConfig) { c.PasswordHash = "" }},
		{"arbitrary_hash", func(c *RemoteConfig) { c.PasswordHash = "password" }},
		{"memory_bomb", func(c *RemoteConfig) { c.PasswordHash = strings.Replace(c.PasswordHash, "m=19456", "m=999999999", 1) }},
		{"iteration_bomb", func(c *RemoteConfig) { c.PasswordHash = strings.Replace(c.PasswordHash, "t=2", "t=999999999", 1) }},
		{"padded_hash", func(c *RemoteConfig) { c.PasswordHash += "=" }},
		{"http_origin", func(c *RemoteConfig) { c.Origin = "http://archive.example.com" }},
		{"origin_path", func(c *RemoteConfig) { c.Origin += "/" }},
		{"origin_userinfo", func(c *RemoteConfig) { c.Origin = "https://owner@archive.example.com" }},
		{"origin_uppercase", func(c *RemoteConfig) { c.Origin = "https://ARCHIVE.example.com" }},
		{"origin_empty_fragment", func(c *RemoteConfig) { c.Origin += "#" }},
		{"origin_query", func(c *RemoteConfig) { c.Origin += "?" }},
		{"wildcard_listener", func(c *RemoteConfig) { c.ListenAddress = "0.0.0.0:6969" }},
		{"hostname_listener", func(c *RemoteConfig) { c.ListenAddress = "localhost:6969" }},
		{"ephemeral_listener", func(c *RemoteConfig) { c.ListenAddress = "127.0.0.1:0" }},
		{"noncanonical_listener", func(c *RemoteConfig) { c.ListenAddress = "127.0.0.1:06969" }},
		{"ttl_too_long", func(c *RemoteConfig) { c.SessionTTL = 25 * time.Hour }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.modify(&cfg)
			if err := ValidateRemoteConfig(cfg); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
			if _, err := NewRemote(cfg, &fakeApplication{}); err == nil {
				t.Fatal("unsafe server created")
			}
		})
	}
	if _, err := NewRemote(valid, nil); err == nil {
		t.Fatal("nil application accepted")
	}
}

func TestRemotePublicShellHasNoCapabilitiesAndAnonymousAPIsNeverCallApplication(t *testing.T) {
	app := &fakeApplication{}
	s := remoteServer(t, app)
	for _, path := range []string{"/", "/assets/app.js", "/assets/app.css"} {
		rr := remoteRequest(s, "GET", path, "", nil, nil)
		if rr.Code != http.StatusOK || len(rr.Result().Cookies()) != 0 {
			t.Fatalf("asset rejected or granted cookie: %s", path)
		}
		if strings.Contains(rr.Body.String(), s.passwordHash) {
			t.Fatal("public asset exposed password hash")
		}
		if path == "/" && !strings.Contains(rr.Body.String(), `data-remote-admin="true"`) {
			t.Fatal("remote shell mode missing")
		}
	}
	for path, method := range routeMethods {
		rr := remoteRequest(s, method, path, `{}`, nil, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s status %d", path, rr.Code)
		}
		assertFixedError(t, rr)
		assertNoCalls(t, app, 0)
	}
	for _, path := range []string{"/api/bootstrap", "/api/owner/attach", "/unknown", "/api/unknown", "/api/auth/register"} {
		for _, method := range []string{"GET", "POST"} {
			rr := remoteRequest(s, method, path, `{}`, nil, nil)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("unavailable route %s status %d", path, rr.Code)
			}
		}
	}
	assertNoCalls(t, app, 0)
}

func TestRemotePasswordSessionGatesReadWriteAndReload(t *testing.T) {
	app := &fakeApplication{}
	s := remoteServer(t, app)
	owner := remoteLogin(t, s)
	for _, tc := range []struct{ method, path, body string }{{"GET", "/api/status", ""}, {"POST", "/api/search", `{"query":"שלום"}`}, {"POST", "/api/pause", `{}`}} {
		rr := remoteRequest(s, tc.method, tc.path, tc.body, &owner, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("authenticated route %s rejected: %d", tc.path, rr.Code)
		}
	}
	if app.count("status") != 1 || app.count("search") != 1 || app.count("pause") != 1 {
		t.Fatal("authenticated requests did not dispatch")
	}
	before := app.total()
	rr := remoteRequest(s, "POST", "/api/auth/session", `{}`, &owner, func(r *http.Request) { r.Header.Del("X-CSRF-Token") })
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), owner.csrf) || len(rr.Result().Cookies()) != 0 {
		t.Fatal("session reload failed or extended cookie")
	}
	assertNoCalls(t, app, before)
}

func TestRemoteAuthorityCSRFAndDuplicateCapabilitiesFailBeforeDispatch(t *testing.T) {
	app := &fakeApplication{}
	s := remoteServer(t, app)
	owner := remoteLogin(t, s)
	changes := []struct {
		name   string
		change func(*http.Request)
	}{
		{"wrong_host", func(r *http.Request) {
			r.Host = "evil.example"
			r.Header.Set("X-Forwarded-Host", "archive.example.com")
		}},
		{"wrong_origin", func(r *http.Request) {
			r.Header.Set("Origin", "https://evil.example")
			r.Header.Set("X-Forwarded-Proto", "https")
		}},
		{"missing_origin", func(r *http.Request) { r.Header.Del("Origin") }},
		{"duplicate_origin", func(r *http.Request) { r.Header.Add("Origin", remoteTestOrigin) }},
		{"missing_csrf", func(r *http.Request) { r.Header.Del("X-CSRF-Token") }},
		{"wrong_csrf", func(r *http.Request) { r.Header.Set("X-CSRF-Token", "wrong") }},
		{"duplicate_csrf", func(r *http.Request) { r.Header.Add("X-CSRF-Token", owner.csrf) }},
		{"duplicate_cookie", func(r *http.Request) { r.AddCookie(owner.cookie) }},
		{"duplicate_cookie_header", func(r *http.Request) { r.Header.Add("Cookie", owner.cookie.String()) }},
		{"url_host_mismatch", func(r *http.Request) { r.URL.Host = "evil.example" }},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			rr := remoteRequest(s, "POST", "/api/pause", `{}`, &owner, tc.change)
			assertFixedError(t, rr)
			assertNoCalls(t, app, 0)
		})
	}
	for _, change := range []func(*http.Request){func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, func(r *http.Request) { r.Header.Add("X-CSRF-Token", owner.csrf) }} {
		rr := remoteRequest(s, "GET", "/api/status", "", &owner, change)
		assertFixedError(t, rr)
		assertNoCalls(t, app, 0)
	}
	if rr := remoteRequest(s, "GET", "/api/status", "", &owner, func(r *http.Request) { r.Header.Del("Origin") }); rr.Code != http.StatusOK {
		t.Fatal("normal browser GET denied")
	}
}

func TestRemoteLoginInputsAreBoundedAndStrict(t *testing.T) {
	app := &fakeApplication{}
	s := remoteServer(t, app)
	cases := []struct {
		name, body string
		change     func(*http.Request)
		status     int
	}{
		{"unknown_field", `{"password":"` + remoteTestPassword + `","account":"other"}`, nil, 400},
		{"duplicate_password", `{"password":"` + remoteTestPassword + `","Password":"wrong"}`, nil, 400},
		{"null", `null`, nil, 400},
		{"large", strings.Repeat(" ", 4097) + `{}`, nil, 413},
		{"long_password", `{"password":"` + strings.Repeat("a", 1025) + `"}`, nil, 400},
		{"bad_content_type", `{}`, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"duplicate_content_type", `{}`, func(r *http.Request) { r.Header.Add("Content-Type", "application/json") }, 415},
		{"missing_origin", `{}`, func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"duplicate_origin", `{}`, func(r *http.Request) { r.Header.Add("Origin", remoteTestOrigin) }, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := remoteRequest(s, "POST", "/api/auth/login", tc.body, nil, tc.change)
			if rr.Code != tc.status {
				t.Fatalf("status %d expected %d", rr.Code, tc.status)
			}
			assertFixedError(t, rr)
			if len(rr.Result().Cookies()) != 0 || strings.Contains(rr.Body.String(), remoteTestPassword) {
				t.Fatal("rejected login grants session or leaks password")
			}
		})
	}
	assertNoCalls(t, app, 0)
}

func TestRemoteLoginThrottleIsGlobalAndKDFConcurrencyBounded(t *testing.T) {
	s := remoteServer(t, &fakeApplication{})
	for i := 0; i < remoteLoginBurst; i++ {
		rr := remoteRequest(s, "POST", "/api/auth/login", `{"password":"wrong-password"}`, nil, func(r *http.Request) { r.RemoteAddr = "192.0.2.1:1"; r.Header.Set("CF-Connecting-IP", "192.0.2.1") })
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status %d", i, rr.Code)
		}
	}
	rr := remoteRequest(s, "POST", "/api/auth/login", `{"password":"`+remoteTestPassword+`"}`, nil, func(r *http.Request) {
		r.RemoteAddr = "198.51.100.1:1"
		r.Header.Set("CF-Connecting-IP", "198.51.100.1")
	})
	if rr.Code != http.StatusTooManyRequests || rr.Header().Get("Retry-After") == "" {
		t.Fatal("spoofed address bypassed throttle")
	}
	s.mu.Lock()
	now := s.clock.nowLocked()
	s.loginWindowStart = now.Add(-remoteLoginWindow)
	s.mu.Unlock()
	s.kdfSlot <- struct{}{}
	rr = remoteRequest(s, "POST", "/api/auth/login", `{"password":"`+remoteTestPassword+`"}`, nil, nil)
	<-s.kdfSlot
	if rr.Code != http.StatusTooManyRequests {
		t.Fatal("parallel KDF accepted")
	}
}

func TestRemoteSessionsExpireLogoutAndDeletionRevoke(t *testing.T) {
	app := &fakeApplication{}
	s := remoteServer(t, app)
	owner := remoteLogin(t, s)
	rr := remoteRequest(s, "POST", "/api/auth/logout", `{}`, &owner, nil)
	if rr.Code != 200 {
		t.Fatal("logout failed")
	}
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || cookies[0].MaxAge != -1 {
		t.Fatal("logout cookie unsafe")
	}
	if rr := remoteRequest(s, "GET", "/api/status", "", &owner, nil); rr.Code != 401 {
		t.Fatal("logged out session still valid")
	}
	// A successful password login must not invalidate another owner's browser.
	s.mu.Lock()
	s.loginWindowStart = s.clock.nowLocked().Add(-remoteLoginWindow)
	s.mu.Unlock()
	owner = remoteLogin(t, s)
	s.mu.Lock()
	s.loginWindowStart = s.clock.nowLocked().Add(-remoteLoginWindow)
	s.mu.Unlock()
	other := remoteLogin(t, s)
	if rr := remoteRequest(s, "GET", "/api/status", "", &owner, nil); rr.Code != 200 {
		t.Fatal("independent login invalidated session")
	}
	s.mu.Lock()
	for key, sess := range s.sessions {
		sess.expires = s.clock.nowLocked().Add(-time.Second)
		s.sessions[key] = sess
	}
	s.mu.Unlock()
	if rr := remoteRequest(s, "POST", "/api/auth/session", `{}`, &other, nil); rr.Code != 401 {
		t.Fatal("expired session reload accepted")
	}
	s.mu.Lock()
	s.loginWindowStart = s.clock.nowLocked().Add(-remoteLoginWindow)
	s.mu.Unlock()
	owner = remoteLogin(t, s)
	rr = remoteRequest(s, "POST", "/api/data/delete", `{"confirmation":"DELETE LOCAL DATA"}`, &owner, nil)
	if rr.Code != 200 || app.count("delete") != 1 {
		t.Fatal("delete did not dispatch")
	}
	if rr := remoteRequest(s, "GET", "/api/status", "", &owner, nil); rr.Code != 401 {
		t.Fatal("deletion left remote session valid")
	}
}

func TestRemoteSessionLimitAndPermanentShutdownRevocation(t *testing.T) {
	s := remoteServer(t, &fakeApplication{})
	owner := remoteLogin(t, s)
	for i := 1; i < remoteSessionLimit; i++ {
		s.mu.Lock()
		s.loginWindowStart = s.clock.nowLocked().Add(-remoteLoginWindow)
		s.mu.Unlock()
		remoteLogin(t, s)
	}
	s.mu.Lock()
	s.loginWindowStart = s.clock.nowLocked().Add(-remoteLoginWindow)
	s.mu.Unlock()
	rr := remoteRequest(s, "POST", "/api/auth/login", `{"password":"`+remoteTestPassword+`"}`, nil, nil)
	if rr.Code != 429 || len(rr.Result().Cookies()) != 0 {
		t.Fatal("session limit not enforced")
	}
	s.RevokeSessions()
	if rr := remoteRequest(s, "GET", "/api/status", "", &owner, nil); rr.Code != 401 {
		t.Fatal("revocation failed")
	}
	if rr := remoteRequest(s, "POST", "/api/auth/login", `{"password":"`+remoteTestPassword+`"}`, nil, nil); rr.Code != 503 {
		t.Fatal("closed remote server accepted login")
	}
}

func TestRemoteAndLocalCredentialsAreIsolated(t *testing.T) {
	app := &fakeApplication{}
	local := newTestServer(t, app)
	localOwner := login(t, local)
	remote := remoteServer(t, app)
	remoteOwner := remoteLogin(t, remote)
	if rr := remoteRequest(remote, "GET", "/api/status", "", &localOwner, nil); rr.Code != 401 {
		t.Fatal("local cookie accepted remotely")
	}
	if rr := request(local, "GET", "/api/status", "", &remoteOwner, nil); rr.Code != 401 {
		t.Fatal("remote cookie accepted locally")
	}
	if rr := remoteRequest(remote, "GET", "/api/status", "", nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer client-token") }); rr.Code != 401 {
		t.Fatal("MCP bearer accepted as admin")
	}
	assertNoCalls(t, app, 0)
}

func TestRemoteMCPUsesDedicatedHandlerWithoutAdminAdmission(t *testing.T) {
	app := &fakeApplication{}
	cfg := remoteConfig(t)
	called := 0
	cfg.MCPHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Header.Get("Authorization") != "Bearer scoped-client" {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(204)
	})
	s, err := NewRemote(cfg, app)
	if err != nil {
		t.Fatal(err)
	}
	if rr := remoteRequest(s, "POST", "/mcp", `{}`, nil, nil); rr.Code != 401 {
		t.Fatal("anonymous MCP accepted")
	}
	if rr := remoteRequest(s, "POST", "/mcp", `{}`, nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer scoped-client") }); rr.Code != 204 {
		t.Fatal("scoped MCP not delegated")
	}
	if called != 2 {
		t.Fatal("dedicated MCP handler not used")
	}
	if rr := remoteRequest(s, "POST", "/mcp/extra", `{}`, nil, nil); rr.Code != 404 {
		t.Fatal("prefix MCP route exposed")
	}
	assertNoCalls(t, app, 0)
}

func TestRemoteServeRejectsWrongListener(t *testing.T) {
	s := remoteServer(t, &fakeApplication{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := s.Serve(context.Background(), listener); err == nil {
		t.Fatal("mismatched listener accepted")
	}
}

func TestRemoteDeletionRevokesBothOwnerInterfaces(t *testing.T) {
	for _, deletingInterface := range []string{"local", "remote"} {
		t.Run(deletingInterface, func(t *testing.T) {
			app := &fakeApplication{}
			local := newTestServer(t, app)
			remote := remoteServer(t, app)
			if err := LinkOwnerRevocations(local, remote); err != nil {
				t.Fatal(err)
			}
			localOwner := login(t, local)
			remoteOwner := remoteLogin(t, remote)
			var rr *httptest.ResponseRecorder
			if deletingInterface == "local" {
				rr = request(local, "POST", "/api/data/delete", `{"confirmation":"DELETE LOCAL DATA"}`, &localOwner, nil)
			} else {
				rr = remoteRequest(remote, "POST", "/api/data/delete", `{"confirmation":"DELETE LOCAL DATA"}`, &remoteOwner, nil)
			}
			if rr.Code != 200 || app.count("delete") != 1 {
				t.Fatal("delete failed")
			}
			before := app.total()
			if rr := request(local, "GET", "/api/status", "", &localOwner, nil); rr.Code != 401 {
				t.Fatal("local session survived deletion")
			}
			if rr := remoteRequest(remote, "GET", "/api/status", "", &remoteOwner, nil); rr.Code != 401 {
				t.Fatal("remote session survived deletion")
			}
			if rr := remoteRequest(remote, "POST", "/api/auth/login", `{"password":"`+remoteTestPassword+`"}`, nil, nil); rr.Code != 503 {
				t.Fatal("remote password login reopened deleted archive")
			}
			local.mu.Lock()
			revoked := local.attachRevoked
			pending := len(local.bootstraps)
			local.mu.Unlock()
			if !revoked || pending != 0 {
				t.Fatal("local owner capability survived deletion")
			}
			assertNoCalls(t, app, before)
		})
	}
}

type remoteBlockingApplication struct {
	*fakeApplication
	entered, canceled, release chan struct{}
}

func (a *remoteBlockingApplication) Status(ctx context.Context) (archive.StatusSnapshot, error) {
	close(a.entered)
	<-ctx.Done()
	close(a.canceled)
	<-a.release
	return a.fakeApplication.Status(ctx)
}

func TestRemoteServeJoinsAcceptedHandlerAfterForcedHTTPClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	cfg := remoteConfig(t)
	cfg.ListenAddress = listener.Addr().String()
	app := &remoteBlockingApplication{fakeApplication: &fakeApplication{}, entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	s, err := NewRemote(cfg, app)
	if err != nil {
		t.Fatal(err)
	}
	owner := remoteLogin(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(app.release) }) }
	defer release()
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, listener) }()
	requested := make(chan error, 1)
	go func() {
		req, err := http.NewRequest("GET", "http://"+listener.Addr().String()+"/api/status", nil)
		if err != nil {
			requested <- err
			return
		}
		req.Host = "archive.example.com"
		req.AddCookie(owner.cookie)
		req.Header.Set("X-CSRF-Token", owner.csrf)
		client := &http.Client{Timeout: 10 * time.Second}
		response, err := client.Do(req)
		if response != nil {
			response.Body.Close()
		}
		requested <- err
	}()
	select {
	case <-app.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler was not admitted")
	}
	cancel()
	// The application observes request cancellation after the bounded graceful
	// HTTP shutdown expires and Close cancels the accepted request context.
	select {
	case <-app.canceled:
	case <-time.After(7 * time.Second):
		t.Fatal("accepted request was not canceled")
	}
	select {
	case <-served:
		t.Fatal("Serve returned before accepted handler completed")
	default:
	}
	release()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not join accepted handler")
	}
	select {
	case <-requested:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request did not exit")
	}
}
