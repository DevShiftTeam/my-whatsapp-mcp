package localui

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localauth"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const remoteOrigin = "https://archive.example.com"

func remoteFixture(t *testing.T) (*Server, *localauth.Authenticator, context.CancelFunc) {
	t.Helper()
	p, e := appdata.Prepare(filepath.Join(t.TempDir(), "data"))
	if e != nil {
		t.Fatal(e)
	}
	l, e := appdata.Acquire(p)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { l.Close() })
	a, e := localauth.NewAuthenticator(p, l)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.SetPassword(context.Background(), []byte("correct passphrase")); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, e := NewManagedRemote(ManagedRemoteConfig{Context: ctx, Origin: remoteOrigin, Generation: 1, Authenticator: a}, &fakeApplication{})
	if e != nil {
		t.Fatal(e)
	}
	return s, a, cancel
}
func managedRemoteRequest(s *Server, method, path, body string, session *testSession, edit func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, remoteOrigin+path, strings.NewReader(body))
	r.Header.Set("Origin", remoteOrigin)
	r.Header.Set("Content-Type", "application/json")
	if session != nil {
		r.AddCookie(session.cookie)
		r.Header.Set("X-CSRF-Token", session.csrf)
	}
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func remoteChallenge(t *testing.T, s *Server) (*http.Cookie, string) {
	t.Helper()
	w := managedRemoteRequest(s, "GET", "/api/remote/login", "", nil, nil)
	if w.Code != 200 {
		t.Fatalf("challenge: %d %s", w.Code, w.Body.String())
	}
	var v map[string]string
	json.Unmarshal(w.Body.Bytes(), &v)
	return w.Result().Cookies()[0], v["challenge"]
}
func managedRemoteLogin(t *testing.T, s *Server) *testSession {
	t.Helper()
	cookie, challenge := remoteChallenge(t, s)
	body, _ := json.Marshal(map[string]string{"password": "correct passphrase", "challenge": challenge})
	w := managedRemoteRequest(s, "POST", "/api/remote/login", string(body), nil, func(r *http.Request) { r.AddCookie(cookie) })
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	var v map[string]string
	json.Unmarshal(w.Body.Bytes(), &v)
	var c *http.Cookie
	for _, x := range w.Result().Cookies() {
		if x.Name == "__Host-wa_managed_owner" {
			c = x
		}
	}
	if c == nil || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
		t.Fatal("cookie policy")
	}
	return &testSession{cookie: c, csrf: v["csrf_token"]}
}
func TestRemoteRealmRouteMatrixAndReset(t *testing.T) {
	s, a, cancel := remoteFixture(t)
	if s.AttachURL() != "" || s.BootstrapURL() != "" {
		t.Fatal("minted native capability")
	}
	session := managedRemoteLogin(t, s)
	for _, path := range []string{"/api/setup", "/api/status", "/api/chats", "/api/pairing", "/api/session"} {
		if w := managedRemoteRequest(s, "GET", path, "", session, nil); w.Code != 200 {
			t.Fatalf("protected %s: %d", path, w.Code)
		}
		if w := managedRemoteRequest(s, "GET", path, "", session, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }); w.Code != 403 {
			t.Fatal("read without CSRF", path, w.Code)
		}
	}
	for _, path := range []string{"/api/bootstrap", "/api/owner/attach", "/api/data/delete", "/api/connections/reveal", "/api/remote/password", "/api/remote/status", "/api/remote/configure", "/api/remote/disable", "/api/remote/remove", "/api/remote/sign-in", "/healthz"} {
		for _, method := range []string{"GET", "POST"} {
			if w := managedRemoteRequest(s, method, path, "{}", session, nil); w.Code != 404 {
				t.Fatalf("local authority leaked: %s %s %d", method, path, w.Code)
			}
		}
	}
	if w := managedRemoteRequest(s, "GET", "/api/status", "", nil, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: "owner_session_44771", Value: session.cookie.Value})
		r.Header.Set("Authorization", "Bearer provider-or-mcp")
	}); w.Code != 401 {
		t.Fatal("cross realm auth")
	}
	challengeCookie, challenge := remoteChallenge(t, s)
	if e := a.SetPassword(context.Background(), []byte("correct passphrase")); e != nil {
		t.Fatal(e)
	}
	if w := managedRemoteRequest(s, "GET", "/api/status", "", session, nil); w.Code != 401 {
		t.Fatal("reset session survived")
	}
	body, _ := json.Marshal(map[string]string{"password": "correct passphrase", "challenge": challenge})
	if w := managedRemoteRequest(s, "POST", "/api/remote/login", string(body), nil, func(r *http.Request) { r.AddCookie(challengeCookie) }); w.Code != 401 {
		t.Fatal("reset challenge survived")
	}
	session = managedRemoteLogin(t, s)
	cancel()
	if w := managedRemoteRequest(s, "GET", "/api/status", "", session, nil); w.Code != 401 {
		t.Fatal("canceled realm survived")
	}
}
func TestRemoteLoginRetryLogoutExpiryAndAuthority(t *testing.T) {
	s, _, _ := remoteFixture(t)
	cookie, challenge := remoteChallenge(t, s)
	body, _ := json.Marshal(map[string]string{"password": "wrong passphrase", "challenge": challenge})
	w := managedRemoteRequest(s, "POST", "/api/remote/login", string(body), nil, func(r *http.Request) { r.AddCookie(cookie) })
	if w.Code != 401 {
		t.Fatal("bad password accepted")
	}
	session := managedRemoteLogin(t, s)
	for _, edit := range []func(*http.Request){func(r *http.Request) {
		r.Host = "evil.example"
		r.Header.Set("X-Forwarded-Host", "archive.example.com")
	}, func(r *http.Request) { r.Header.Add("Origin", remoteOrigin) }, func(r *http.Request) { r.Header.Set("Origin", "null") }, func(r *http.Request) { r.URL.User = url.User("alias") }, func(r *http.Request) { r.Header.Set("Forwarded", "host=evil.example") }, func(r *http.Request) { r.Header.Add("X-CSRF-Token", session.csrf) }, func(r *http.Request) { r.AddCookie(session.cookie) }} {
		if w := managedRemoteRequest(s, "GET", "/api/status", "", session, edit); w.Code < 400 {
			t.Fatal("hostile authority admitted")
		}
	}
	if w := managedRemoteRequest(s, "POST", "/api/remote/logout", "{}", session, nil); w.Code != 200 {
		t.Fatal("logout", w.Code)
	}
	if w := managedRemoteRequest(s, "GET", "/api/status", "", session, nil); w.Code != 401 {
		t.Fatal("logout survived")
	}
	session = managedRemoteLogin(t, s)
	s.mu.Lock()
	now := s.clock.nowLocked()
	s.clock.wall = func() time.Time { return now.Add(25 * time.Hour) }
	s.mu.Unlock()
	if w := managedRemoteRequest(s, "GET", "/api/status", "", session, nil); w.Code != 401 {
		t.Fatal("expiry")
	}
	s.mu.Lock()
	s.clock.wall = func() time.Time { return now }
	s.mu.Unlock()
	if w := managedRemoteRequest(s, "GET", "/api/status", "", session, nil); w.Code != 401 {
		t.Fatal("expired revived")
	}
	managedRemoteLogin(t, s)
}

func TestRemoteMalformedLoginAndMissingPasswordFailClosed(t *testing.T) {
	s, a, _ := remoteFixture(t)
	for _, body := range []string{`{"password":"correct passphrase","challenge":"` + strings.Repeat("x", 43) + `","extra":1}`, `{"password":"correct passphrase","Password":"other passphrase"}`, `{"password":"correct passphrase","challenge":null}`, `{"password":"abcdefghijk\\ud800","challenge":"` + strings.Repeat("x", 43) + `"}`, strings.Repeat("x", 2049)} {
		if w := managedRemoteRequest(s, "POST", "/api/remote/login", body, nil, nil); w.Code < 400 {
			t.Fatal("invalid body accepted")
		}
	}
	for _, path := range []string{"/api/remote/login?x=1", "/api/remote/login?", "/api/interface?x=1", "/%61pi/remote/login"} {
		if w := managedRemoteRequest(s, "GET", path, "", nil, nil); w.Code < 400 {
			t.Fatal("alias/query", path)
		}
	}
	for _, path := range []string{"/", "/api/interface", "/api/remote/login"} {
		if w := managedRemoteRequest(s, "GET", path, "body", nil, nil); w.Code != 400 {
			t.Fatal("body accepted", path)
		}
	}
	cookie, challenge := remoteChallenge(t, s)
	body, _ := json.Marshal(map[string]string{"password": "correct passphrase", "challenge": challenge})
	for _, edit := range []func(*http.Request){func(r *http.Request) { r.Header.Del("Origin"); r.AddCookie(cookie) }, func(r *http.Request) { r.Header.Add("Origin", remoteOrigin); r.AddCookie(cookie) }, func(r *http.Request) { r.Header.Set("Origin", "https://hostile.example"); r.AddCookie(cookie) }, func(r *http.Request) { r.AddCookie(cookie); r.AddCookie(cookie) }, func(r *http.Request) { r.Header.Set("X-Extra", strings.Repeat("x", 16385)); r.AddCookie(cookie) }} {
		if w := managedRemoteRequest(s, "POST", "/api/remote/login", string(body), nil, edit); w.Code < 400 {
			t.Fatal("invalid challenge authority")
		}
	}
	w := httptest.NewRecorder()
	s.revealRemote(w, httptest.NewRequest("POST", "/api/connections/reveal", strings.NewReader("{}")), context.Background())
	if w.Code != 404 {
		t.Fatal("direct reveal bypass")
	}
	s.mu.Lock()
	for i := 0; i < 64; i++ {
		key := [32]byte{byte(i)}
		s.remote.challenges[key] = loginChallenge{generation: a.Generation(), expires: s.clock.nowLocked().Add(time.Minute)}
	}
	s.mu.Unlock()
	if w := managedRemoteRequest(s, "GET", "/api/remote/login", "", nil, nil); w.Code != 429 {
		t.Fatal("unbounded challenge admission")
	}
	a.Close()
	if _, e := NewManagedRemote(ManagedRemoteConfig{Context: context.Background(), Origin: remoteOrigin, Generation: 2, Authenticator: a}, &fakeApplication{}); e == nil {
		t.Fatal("missing password allowed")
	}
	for _, origin := range []string{testOrigin, "https://127.0.0.1", "https://archive.example.com/", "https://archive.example.com?x=1"} {
		if _, e := NewManagedRemote(ManagedRemoteConfig{Context: context.Background(), Origin: origin, Generation: 1, Authenticator: a}, &fakeApplication{}); e == nil {
			t.Fatal("invalid remote origin")
		}
	}
}

type passwordApplication struct {
	*fakeApplication
	passwordCalls int
	password      []byte
}

func (a *passwordApplication) SetRemoteOwnerPassword(_ context.Context, p []byte) error {
	a.passwordCalls++
	a.password = append([]byte(nil), p...)
	return nil
}
func TestPasswordSetupNativeOwnerAndCSRFAuthority(t *testing.T) {
	app := &passwordApplication{fakeApplication: &fakeApplication{}}
	s, e := New(Config{Origin: testOrigin}, app)
	if e != nil {
		t.Fatal(e)
	}
	body := `{"password":"  preserve these spaces  "}`
	if w := request(s, "PUT", "/api/remote/password", body, nil, nil); w.Code != 401 {
		t.Fatal("unauthenticated setup")
	}
	session := login(t, s)
	if w := request(s, "PUT", "/api/remote/password", body, &session, map[string]string{"X-CSRF-Token": ""}); w.Code != 403 {
		t.Fatal("setup without CSRF")
	}
	if w := request(s, "PUT", "/api/remote/password", body, &session, map[string]string{"Origin": ""}); w.Code != 403 {
		t.Fatal("setup without origin")
	}
	if app.passwordCalls != 0 {
		t.Fatal("denied setup reached app")
	}
	w := request(s, "PUT", "/api/remote/password", body, &session, nil)
	if w.Code != 200 || app.passwordCalls != 1 || string(app.password) != "  preserve these spaces  " {
		t.Fatal("local setup", w.Code)
	}
	if strings.Contains(w.Body.String(), "preserve") {
		t.Fatal("password echoed")
	}
	clear(app.password)
}

func TestRemoteRevocationCannotClearNewRealm(t *testing.T) {
	old, a, cancel := remoteFixture(t)
	next, e := NewManagedRemote(ManagedRemoteConfig{Context: context.Background(), Origin: remoteOrigin, Generation: 2, Authenticator: a}, &fakeApplication{})
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	old.RevokeSessions()
	if w := managedRemoteRequest(old, "GET", "/api/remote/login", "", nil, nil); w.Code < 400 {
		t.Fatal("old realm revived")
	}
	if w := managedRemoteRequest(next, "GET", "/api/remote/login", "", nil, nil); w.Code != 200 {
		t.Fatal("stale realm revocation cleared new generation")
	}
}
func TestRemoteAllProtectedPathsRequireSessionAndCSRF(t *testing.T) {
	s, _, _ := remoteFixture(t)
	for path := range remoteProtected {
		method := routeMethods[path]
		if path == "/api/remote/logout" {
			method = "POST"
		}
		if w := managedRemoteRequest(s, method, path, "{}", nil, nil); w.Code != 401 {
			t.Fatalf("%s unauthenticated code %d", path, w.Code)
		}
	}
}

func TestInterfaceMetadataIsPublicBodyFreeAndNonsecret(t *testing.T) {
	s, _, _ := remoteFixture(t)
	for _, server := range []*Server{s, newTestServer(t, &fakeApplication{})} {
		var w *httptest.ResponseRecorder
		want := "local"
		if server.remote != nil {
			want = "remote"
			w = managedRemoteRequest(server, "GET", "/api/interface", "", nil, nil)
		} else {
			w = request(server, "GET", "/api/interface", "", nil, nil)
		}
		if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"realm":"`+want+`"}` {
			t.Fatal("unexpected discovery content")
		}
	}
}

// A fresh process keeps this genuine password-verification scenario independent
// of the process-global attempt budget used by the rest of the auth suite. Do
// not reset/bypass the production limiter or add wall-clock sleeps to tests.
func TestRemoteSessionCapacity(t *testing.T) {
	const childGuard = "WA_TEST_REMOTE_SESSION_CAPACITY"
	if os.Getenv(childGuard) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRemoteSessionCapacity$", "-test.count=1", "-test.timeout=15s")
		command.Env = append(os.Environ(), childGuard+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("isolated session-capacity regression: %v\n%s", err, output)
		}
		return
	}
	s, a, _ := remoteFixture(t)
	owners := make([]testSession, 16)
	snapshot := make(map[[32]byte]remoteSession, 16)
	s.mu.Lock()
	expires := s.clock.nowLocked().Add(time.Hour)
	generation := a.Generation()
	for i := range owners {
		token, err := randomToken()
		if err != nil {
			s.mu.Unlock()
			t.Fatal("fixture entropy unavailable")
		}
		csrf, err := randomToken()
		if err != nil {
			s.mu.Unlock()
			t.Fatal("fixture entropy unavailable")
		}
		owners[i] = testSession{cookie: &http.Cookie{Name: remoteCookie, Value: token}, csrf: csrf}
		digest := sha256.Sum256([]byte(token))
		value := remoteSession{session: session{digest: digest, csrf: csrf, expires: expires}, generation: generation}
		s.remote.sessions[digest] = value
		snapshot[digest] = value
	}
	s.mu.Unlock()
	assertPreserved := func(except [32]byte) {
		t.Helper()
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.remote.sessions) != 16 {
			t.Fatalf("session count %d, want 16", len(s.remote.sessions))
		}
		for key, want := range snapshot {
			if key == except {
				continue
			}
			if got, ok := s.remote.sessions[key]; !ok || got != want {
				t.Fatal("unrelated or prior session changed")
			}
		}
	}
	attempt := func(password string, owner *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		cookie, challenge := remoteChallenge(t, s)
		body, err := json.Marshal(map[string]string{"password": password, "challenge": challenge})
		if err != nil {
			t.Fatal(err)
		}
		return managedRemoteRequest(s, "POST", "/api/remote/login", string(body), nil, func(r *http.Request) {
			r.AddCookie(cookie)
			if owner != nil {
				r.AddCookie(owner)
			}
		})
	}
	// All seeded entries are live, not merely arbitrary cookies counted as slots.
	for i := range owners {
		if w := managedRemoteRequest(s, "GET", "/api/status", "", &owners[i], nil); w.Code != 200 {
			t.Fatalf("live fixture entry denied: %d", w.Code)
		}
	}
	if w := attempt("incorrect passphrase", owners[0].cookie); w.Code != 401 {
		t.Fatalf("failed password code %d, want 401", w.Code)
	}
	assertPreserved([32]byte{})
	if w := attempt("correct passphrase", nil); w.Code != 401 {
		t.Fatalf("new seventeenth session code %d, want 401", w.Code)
	}
	assertPreserved([32]byte{})
	forged := &http.Cookie{Name: remoteCookie, Value: strings.Repeat("F", 43)}
	if _, exists := snapshot[sha256.Sum256([]byte(forged.Value))]; exists {
		t.Fatal("forged cookie collides with fixture")
	}
	if w := attempt("correct passphrase", forged); w.Code != 401 {
		t.Fatalf("forged-cookie session code %d, want 401", w.Code)
	}
	assertPreserved([32]byte{})
	w := attempt("correct passphrase", owners[0].cookie)
	if w.Code != 200 {
		t.Fatalf("existing owner cannot replace its session at capacity: HTTP %d (want 200)", w.Code)
	}
	oldKey := sha256.Sum256([]byte(owners[0].cookie.Value))
	assertPreserved(oldKey)
	var response map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal("invalid login response")
	}
	var replacement *http.Cookie
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == remoteCookie {
			replacement = cookie
		}
	}
	if replacement == nil || replacement.Value == owners[0].cookie.Value || response["csrf_token"] == "" || response["csrf_token"] == owners[0].csrf {
		t.Fatal("replacement did not issue independent credentials")
	}
	next := &testSession{cookie: replacement, csrf: response["csrf_token"]}
	if result := managedRemoteRequest(s, "GET", "/api/status", "", next, nil); result.Code != 200 {
		t.Fatalf("replacement session denied: %d", result.Code)
	}
	if result := managedRemoteRequest(s, "GET", "/api/status", "", &owners[0], nil); result.Code != 401 {
		t.Fatal("replaced cookie and CSRF remain authorized")
	}
	if result := managedRemoteRequest(s, "GET", "/api/status", "", next, func(r *http.Request) { r.Header.Set("X-CSRF-Token", owners[0].csrf) }); result.Code != 403 {
		t.Fatal("old CSRF authorizes replacement cookie")
	}
	for i := 1; i < len(owners); i++ {
		if result := managedRemoteRequest(s, "GET", "/api/status", "", &owners[i], nil); result.Code != 200 {
			t.Fatalf("unrelated session %d denied: %d", i, result.Code)
		}
	}
	s.mu.Lock()
	_, oldExists := s.remote.sessions[oldKey]
	s.mu.Unlock()
	if oldExists {
		t.Fatal("replaced session still retained")
	}
}
