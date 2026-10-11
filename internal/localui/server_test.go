package localui

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
)

const testOrigin = "http://127.0.0.1:44771"

// fakeApplication records every application call so rejected HTTP requests can
// be checked for side effects without importing a connector or database.
type fakeApplication struct {
	mu             sync.Mutex
	calls          map[string]int
	err            error
	setup          SetupState
	pairing        PairingState
	qr             []byte
	search         archive.SearchPage
	context        archive.ContextPage
	chats          archive.DiscoveryPage
	status         archive.StatusSnapshot
	policy         archive.ArchivePolicy
	expected       archive.Generation
	searchRequest  archive.SearchRequest
	contextRequest archive.ContextRequest
	chatRequest    archive.ChatListRequest
	confirmed      bool
}

func (a *fakeApplication) called(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.calls == nil {
		a.calls = make(map[string]int)
	}
	a.calls[name]++
	return a.err
}
func (a *fakeApplication) count(name string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[name]
}
func (a *fakeApplication) total() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	var count int
	for _, n := range a.calls {
		count += n
	}
	return count
}
func (a *fakeApplication) Setup(context.Context) (SetupState, error) {
	return a.setup, a.called("setup")
}
func (a *fakeApplication) SetPolicy(_ context.Context, p archive.ArchivePolicy, expected archive.Generation) (archive.ArchivePolicy, error) {
	err := a.called("policy")
	a.policy, a.expected = p, expected
	return p, err
}
func (a *fakeApplication) StartPairing(context.Context) error { return a.called("pairing/start") }
func (a *fakeApplication) PairingState(context.Context) (PairingState, error) {
	return a.pairing, a.called("pairing")
}
func (a *fakeApplication) QRPNG(context.Context) ([]byte, error) { return a.qr, a.called("qr") }
func (a *fakeApplication) Pause(context.Context) error           { return a.called("pause") }
func (a *fakeApplication) Resume(context.Context) error          { return a.called("resume") }
func (a *fakeApplication) Disconnect(context.Context) error      { return a.called("disconnect") }
func (a *fakeApplication) DeleteLocalData(_ context.Context, confirmed bool) error {
	a.confirmed = confirmed
	return a.called("delete")
}
func (a *fakeApplication) Search(_ context.Context, q archive.SearchRequest) (archive.SearchPage, error) {
	a.searchRequest = q
	return a.search, a.called("search")
}
func (a *fakeApplication) Context(_ context.Context, q archive.ContextRequest) (archive.ContextPage, error) {
	a.contextRequest = q
	return a.context, a.called("context")
}
func (a *fakeApplication) Chats(_ context.Context, q archive.ChatListRequest) (archive.DiscoveryPage, error) {
	a.chatRequest = q
	return a.chats, a.called("chats")
}
func (a *fakeApplication) Status(context.Context) (archive.StatusSnapshot, error) {
	return a.status, a.called("status")
}

type testSession struct {
	cookie *http.Cookie
	csrf   string
}

func newTestServer(t *testing.T, app *fakeApplication, config ...Config) *Server {
	t.Helper()
	cfg := Config{Origin: testOrigin}
	if len(config) > 0 {
		cfg = config[0]
	}
	s, err := New(cfg, app)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}
func request(s *Server, method, path, body string, session *testSession, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, testOrigin+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	if session != nil {
		req.AddCookie(session.cookie)
		req.Header.Set("X-CSRF-Token", session.csrf)
	}
	for key, value := range headers {
		if key == "Host" {
			req.Host = value
		} else if value == "" {
			req.Header.Del(key)
		} else {
			req.Header.Set(key, value)
		}
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}
func bootstrapToken(t *testing.T, s *Server) string {
	t.Helper()
	u, err := url.Parse(s.BootstrapURL())
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host != testOrigin || u.Path != "/" || u.RawQuery != "" {
		t.Fatalf("bootstrap URL must use origin, root and fragment: %q", s.BootstrapURL())
	}
	values, err := url.ParseQuery(u.Fragment)
	if err != nil || len(values) != 1 || values.Get("bootstrap") == "" {
		t.Fatalf("bootstrap fragment: %q", u.Fragment)
	}
	return values.Get("bootstrap")
}
func login(t *testing.T, s *Server) testSession {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"token": bootstrapToken(t, s)})
	rr := request(s, http.MethodPost, "/api/bootstrap", string(body), nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap status=%d body=%s", rr.Code, rr.Body.String())
	}
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("bootstrap cookies=%#v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != "owner_session_44771" || cookie.Value == "" || cookie.Path != "/" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("owner cookie attributes=%#v", cookie)
	}
	var response struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil || response.CSRF == "" {
		t.Fatalf("bootstrap CSRF response=%s err=%v", rr.Body.String(), err)
	}
	if strings.Contains(rr.Body.String(), cookie.Value) {
		t.Fatal("response exposes the owner session cookie")
	}
	return testSession{cookie: cookie, csrf: response.CSRF}
}
func assertDenied(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code >= 200 && rr.Code < 300 {
		t.Fatalf("request unexpectedly accepted: %d %s", rr.Code, rr.Body.String())
	}
}
func assertFixedError(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	assertDenied(t, rr)
	var response map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("error must be JSON: %s (%v)", rr.Body.String(), err)
	}
	if len(response) != 2 {
		t.Fatalf("error must contain only code and message: %s", rr.Body.String())
	}
	for _, name := range []string{"code", "message"} {
		var value string
		if json.Unmarshal(response[name], &value) != nil || value == "" {
			t.Fatalf("missing fixed %s: %s", name, rr.Body.String())
		}
	}
}
func assertNoCalls(t *testing.T, app *fakeApplication, before int) {
	t.Helper()
	if got := app.total(); got != before {
		t.Fatalf("rejected HTTP request reached application: calls %d -> %d", before, got)
	}
}

func TestConfigurationRejectsNonLiteralOrNonHTTPOrigins(t *testing.T) {
	for _, origin := range []string{"", "http://localhost:44771", "http://example.com:44771", "http://0.0.0.0:44771", "http://192.0.2.1:44771", "https://127.0.0.1:44771", "http://user@127.0.0.1:44771", "http://127.0.0.1:44771/path", "http://127.0.0.1:44771?token=x", "http://127.0.0.1:44771?", "http://127.0.0.1:44771#", "http://127.0.0.1:44771#token=x"} {
		t.Run(origin, func(t *testing.T) {
			if _, err := New(Config{Origin: origin}, &fakeApplication{}); err == nil {
				t.Fatalf("accepted unsafe origin %q", origin)
			}
		})
	}
}

func TestBootstrapIsOneTimeAndSecretNeverAppearsInPublicShell(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	token := bootstrapToken(t, s)
	for _, path := range []string{"/", "/assets/app.js", "/assets/app.css"} {
		rr := request(s, http.MethodGet, path, "", nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("public asset %s status=%d", path, rr.Code)
		}
		if strings.Contains(rr.Body.String(), token) {
			t.Fatalf("public asset %s exposes bootstrap token", path)
		}
		if len(rr.Result().Cookies()) != 0 {
			t.Fatalf("public asset %s grants a session", path)
		}
	}
	for _, headers := range []map[string]string{{"Origin": ""}, {"Origin": "http://evil.example"}, {"Origin": "null"}, {"Origin": testOrigin + "/"}, {"Host": "evil.example", "X-Forwarded-Host": "127.0.0.1:44771"}} {
		body, _ := json.Marshal(map[string]string{"token": token})
		rr := request(s, http.MethodPost, "/api/bootstrap", string(body), nil, headers)
		assertDenied(t, rr)
		if len(rr.Result().Cookies()) != 0 {
			t.Fatal("invalid bootstrap granted cookie")
		}
	}
	rr := request(s, http.MethodPost, "/api/bootstrap", `{"token":"wrong"}`, nil, nil)
	assertFixedError(t, rr)
	session := login(t, s)
	body, _ := json.Marshal(map[string]string{"token": token})
	rr = request(s, http.MethodPost, "/api/bootstrap", string(body), nil, nil)
	assertDenied(t, rr)
	if len(rr.Result().Cookies()) != 0 {
		t.Fatal("bootstrap replay grants cookie")
	}
	rr = request(s, http.MethodGet, "/api/session", "", &session, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), session.csrf) {
		t.Fatalf("reload session=%d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), session.cookie.Value) {
		t.Fatal("reload API exposes owner cookie")
	}
	assertNoCalls(t, app, 0)
}

func TestEveryAPIRequiresOwnerAuthentication(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	cases := []struct{ method, path, body string }{
		{"GET", "/api/session", ""}, {"GET", "/api/setup", ""}, {"GET", "/api/pairing", ""}, {"GET", "/api/pairing/qr", ""}, {"GET", "/api/status", ""}, {"GET", "/api/chats", ""},
		{"PUT", "/api/policy", `{"date_mode":"all_available","chat_mode":"all"}`}, {"POST", "/api/pairing/start", `{}`}, {"POST", "/api/search", `{"query":"needle"}`}, {"POST", "/api/context", `{"message_id":"opaque"}`}, {"POST", "/api/pause", `{}`}, {"POST", "/api/resume", `{}`}, {"POST", "/api/disconnect", `{}`}, {"POST", "/api/data/delete", `{"confirmation":"DELETE LOCAL DATA"}`},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rr := request(s, tc.method, tc.path, tc.body, nil, nil)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated status=%d body=%s", rr.Code, rr.Body.String())
			}
			assertFixedError(t, rr)
			assertNoCalls(t, app, 0)
		})
	}
}

func TestProtectedWritesRequireOriginAndCSRFBeforeApplicationCall(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	session := login(t, s)
	routes := []struct{ method, path, body string }{
		{"PUT", "/api/policy", `{"date_mode":"all_available","chat_mode":"all"}`}, {"POST", "/api/pairing/start", `{}`}, {"POST", "/api/search", `{"query":"needle"}`}, {"POST", "/api/context", `{"message_id":"opaque"}`}, {"POST", "/api/pause", `{}`}, {"POST", "/api/resume", `{}`}, {"POST", "/api/disconnect", `{}`}, {"POST", "/api/data/delete", `{"confirmation":"DELETE LOCAL DATA"}`},
	}
	attacks := []map[string]string{{"Origin": ""}, {"Origin": "http://evil.example"}, {"Origin": "null"}, {"Origin": "http://localhost:44771"}, {"X-CSRF-Token": ""}, {"X-CSRF-Token": "wrong"}, {"Host": "evil.example"}, {"Host": "127.0.0.1:44772"}, {"Host": "localhost:44771"}}
	for _, route := range routes {
		t.Run(route.path, func(t *testing.T) {
			for _, headers := range attacks {
				before := app.total()
				rr := request(s, route.method, route.path, route.body, &session, headers)
				assertDenied(t, rr)
				assertNoCalls(t, app, before)
			}
		})
	}
	forged := session
	forged.cookie = &http.Cookie{Name: session.cookie.Name, Value: "attacker-chosen-session"}
	rr := request(s, "POST", "/api/pause", `{}`, &forged, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("forged cookie status=%d", rr.Code)
	}
	assertNoCalls(t, app, 0)
}

func TestHTTPInputsAreStrictAndBounded(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	session := login(t, s)
	invalid := []struct {
		name, method, path, body string
		headers                  map[string]string
	}{
		{"empty_command", "POST", "/api/pause", "", nil},
		{"null_command", "POST", "/api/pause", "null", nil},
		{"array_command", "POST", "/api/pause", "[]", nil},
		{"unknown_command_key", "POST", "/api/pause", `{"account":"injected"}`, nil},
		{"concatenated_json", "POST", "/api/pause", `{} {}`, nil},
		{"malformed_json", "POST", "/api/pause", `{`, nil},
		{"non_json", "POST", "/api/pause", `{}`, map[string]string{"Content-Type": "text/plain"}},
		{"missing_content_type", "POST", "/api/pause", `{}`, map[string]string{"Content-Type": ""}},
		{"oversized_body", "POST", "/api/pause", strings.Repeat(" ", 32769) + `{}`, nil},
		{"search_scope_injection", "POST", "/api/search", `{"query":"needle","account":"victim"}`, nil},
		{"search_all_chats_grant", "POST", "/api/search", `{"query":"needle","all_chats":true}`, nil},
		{"search_unicode_field_alias", "POST", "/api/search", `{"query":"needle","chat\u017f":["opaque-chat"]}`, nil},
		{"search_empty", "POST", "/api/search", `{"query":""}`, nil},
		{"search_negative_limit", "POST", "/api/search", `{"query":"needle","limit":-1}`, nil},
		{"search_large_limit", "POST", "/api/search", `{"query":"needle","limit":101}`, nil},
		{"search_invalid_time", "POST", "/api/search", `{"query":"needle","from":"yesterday"}`, nil},
		{"search_reversed_window", "POST", "/api/search", `{"query":"needle","from":"2026-10-06T12:00:00Z","until":"2026-10-05T12:00:00Z"}`, nil},
		{"search_large_query", "POST", "/api/search", `{"query":"` + strings.Repeat("x", 4097) + `"}`, nil},
		{"search_large_cursor", "POST", "/api/search", `{"query":"needle","cursor":"` + strings.Repeat("x", 8193) + `"}`, nil},
		{"context_scope_injection", "POST", "/api/context", `{"message_id":"opaque","account":"victim"}`, nil},
		{"context_empty_id", "POST", "/api/context", `{"message_id":""}`, nil},
		{"context_negative_before", "POST", "/api/context", `{"message_id":"opaque","before":-1}`, nil},
		{"context_large_after", "POST", "/api/context", `{"message_id":"opaque","after":51}`, nil},
		{"context_large_id", "POST", "/api/context", `{"message_id":"` + strings.Repeat("x", 129) + `"}`, nil},
		{"policy_unknown_key", "PUT", "/api/policy", `{"date_mode":"all_available","chat_mode":"all","account":"victim"}`, nil},
		{"policy_invalid_date_mode", "PUT", "/api/policy", `{"date_mode":"unknown","chat_mode":"all"}`, nil},
		{"policy_since_missing_date", "PUT", "/api/policy", `{"date_mode":"since","chat_mode":"all"}`, nil},
		{"policy_invalid_chat_mode", "PUT", "/api/policy", `{"date_mode":"all_available","chat_mode":"unknown"}`, nil},
		{"policy_negative_retention", "PUT", "/api/policy", `{"date_mode":"all_available","chat_mode":"all","rolling_retention_days":-1}`, nil},
		{"policy_retention_overflow", "PUT", "/api/policy", `{"date_mode":"all_available","chat_mode":"all","rolling_retention_days":999999999}`, nil},
		{"chats_negative_limit", "GET", "/api/chats?limit=-1", "", nil},
		{"chats_large_limit", "GET", "/api/chats?limit=201", "", nil},
		{"chats_non_integer_limit", "GET", "/api/chats?limit=NaN", "", nil},
		{"chats_unknown_scope", "GET", "/api/chats?account=victim", "", nil},
		{"chats_large_cursor", "GET", "/api/chats?cursor=" + strings.Repeat("x", 8193), "", nil},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			before := app.total()
			rr := request(s, tc.method, tc.path, tc.body, &session, tc.headers)
			assertFixedError(t, rr)
			assertNoCalls(t, app, before)
		})
	}
}

func TestRoutingNeverTurnsGETIntoMutation(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	session := login(t, s)
	for _, path := range []string{"/api/bootstrap", "/api/policy", "/api/pairing/start", "/api/search", "/api/context", "/api/pause", "/api/resume", "/api/disconnect", "/api/data/delete"} {
		rr := request(s, "GET", path, "", &session, nil)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
	for _, path := range []string{"/api/setup", "/api/pairing", "/api/status", "/api/chats", "/api/session"} {
		rr := request(s, "POST", path, `{}`, &session, nil)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
	assertNoCalls(t, app, 0)
}

func TestPolicySearchAndContextInputsReachApplicationWithoutOwnerScope(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	session := login(t, s)
	policy := `{"expected_generation":7,"date_mode":"since","import_start":"2026-01-02T03:04:05Z","chat_mode":"selective","included_chats":["opaque-chat"],"rolling_retention_days":30}`
	rr := request(s, "PUT", "/api/policy", policy, &session, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("policy=%d %s", rr.Code, rr.Body.String())
	}
	if app.expected != 7 || app.policy.DateMode != archive.DateSince || app.policy.ImportStart.Format(time.RFC3339) != "2026-01-02T03:04:05Z" || app.policy.ChatMode != archive.ChatsSelective || len(app.policy.IncludedChats) != 1 || app.policy.IncludedChats[0] != "opaque-chat" || app.policy.RollingRetention != 30*24*time.Hour {
		t.Fatalf("policy mapping=%#v expected=%d", app.policy, app.expected)
	}
	rr = request(s, "POST", "/api/search", `{"query":"שלום needle","chats":["opaque-chat"],"from":"2026-01-02T03:04:05Z","until":"2026-02-02T03:04:05Z","limit":12,"cursor":"opaque-cursor"}`, &session, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("search=%d %s", rr.Code, rr.Body.String())
	}
	if q := app.searchRequest; q.Query != "שלום needle" || len(q.Chats) != 1 || q.Chats[0] != "opaque-chat" || q.Limit != 12 || q.Cursor != "opaque-cursor" || q.From.Format(time.RFC3339) != "2026-01-02T03:04:05Z" || q.Until.Format(time.RFC3339) != "2026-02-02T03:04:05Z" {
		t.Fatalf("search mapping=%#v", q)
	}
	rr = request(s, "POST", "/api/context", `{"message_id":"opaque-message","before":4,"after":6}`, &session, nil)
	if rr.Code != http.StatusOK || app.contextRequest.MessageID != "opaque-message" || app.contextRequest.Before != 4 || app.contextRequest.After != 6 {
		t.Fatalf("context=%d %s request=%#v", rr.Code, rr.Body.String(), app.contextRequest)
	}
	rr = request(s, "GET", "/api/chats?limit=12&cursor=opaque-cursor", "", &session, nil)
	if rr.Code != http.StatusOK || app.chatRequest.Limit != 12 || app.chatRequest.Cursor != "opaque-cursor" {
		t.Fatalf("chats=%d %s request=%#v", rr.Code, rr.Body.String(), app.chatRequest)
	}
}

func TestCommandsAndDeleteRequireExplicitConfirmation(t *testing.T) {
	app := &fakeApplication{setup: SetupState{Configured: true}}
	s := newTestServer(t, app)
	session := login(t, s)
	for _, path := range []string{"/api/pairing/start", "/api/pause", "/api/resume", "/api/disconnect"} {
		rr := request(s, "POST", path, `{}`, &session, nil)
		if rr.Code < 200 || rr.Code >= 300 {
			t.Fatalf("%s=%d %s", path, rr.Code, rr.Body.String())
		}
	}
	for _, body := range []string{`{}`, `{"confirmation":"delete"}`, `{"confirmation":"DELETE LOCAL DATA "}`, `{"confirmation":"DELETE LOCAL DATA","path":"/"}`} {
		before := app.total()
		rr := request(s, "POST", "/api/data/delete", body, &session, nil)
		assertFixedError(t, rr)
		assertNoCalls(t, app, before)
	}
	rr := request(s, "POST", "/api/data/delete", `{"confirmation":"DELETE LOCAL DATA"}`, &session, nil)
	if rr.Code < 200 || rr.Code >= 300 || app.count("delete") != 1 || !app.confirmed {
		t.Fatalf("confirmed deletion=%d %s calls=%d confirmed=%v", rr.Code, rr.Body.String(), app.count("delete"), app.confirmed)
	}
}

func TestRevocationInvalidatesSessionAndItsCSRF(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	session := login(t, s)
	s.RevokeSessions()
	for _, tc := range []struct{ method, path, body string }{{"GET", "/api/session", ""}, {"GET", "/api/setup", ""}, {"POST", "/api/pause", `{}`}} {
		rr := request(s, tc.method, tc.path, tc.body, &session, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("revoked %s=%d %s", tc.path, rr.Code, rr.Body.String())
		}
	}
	assertNoCalls(t, app, 0)
}

func TestSessionAndBootstrapExpiry(t *testing.T) {
	t.Run("session", func(t *testing.T) {
		app := &fakeApplication{}
		s := newTestServer(t, app, Config{Origin: testOrigin, SessionTTL: 20 * time.Millisecond})
		session := login(t, s)
		time.Sleep(40 * time.Millisecond)
		rr := request(s, "GET", "/api/setup", "", &session, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("expired session=%d %s", rr.Code, rr.Body.String())
		}
		assertNoCalls(t, app, 0)
	})
	t.Run("bootstrap", func(t *testing.T) {
		s := newTestServer(t, &fakeApplication{}, Config{Origin: testOrigin, BootstrapTTL: 20 * time.Millisecond})
		token := bootstrapToken(t, s)
		time.Sleep(40 * time.Millisecond)
		body, _ := json.Marshal(map[string]string{"token": token})
		rr := request(s, "POST", "/api/bootstrap", string(body), nil, nil)
		assertDenied(t, rr)
		if len(rr.Result().Cookies()) != 0 {
			t.Fatal("expired bootstrap grants cookie")
		}
	})
}

func TestResponsesProtectLocalDataAndNeverEchoInternalErrors(t *testing.T) {
	sentinel := "SECRET_SOURCE_JID_123@s.whatsapp.net /home/private/archive.sqlite SELECT secret"
	app := &fakeApplication{err: errors.New(sentinel)}
	s := newTestServer(t, app)
	session := login(t, s)
	for _, tc := range []struct{ method, path, body string }{{"GET", "/api/setup", ""}, {"GET", "/api/pairing", ""}, {"GET", "/api/status", ""}, {"GET", "/api/chats", ""}, {"POST", "/api/search", `{"query":"needle"}`}, {"POST", "/api/context", `{"message_id":"opaque"}`}, {"POST", "/api/pause", `{}`}} {
		rr := request(s, tc.method, tc.path, tc.body, &session, nil)
		assertFixedError(t, rr)
		var response struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil || response.Code != "archive_unavailable" || response.Message != "The archive is unavailable. Try again or restart the app." {
			t.Fatalf("%s must return the fixed public unavailable error: %s", tc.path, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), sentinel) || strings.Contains(rr.Body.String(), "whatsapp.net") || strings.Contains(rr.Body.String(), "archive.sqlite") {
			t.Fatalf("%s echoes internal error: %s", tc.path, rr.Body.String())
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s caches private/error response: %s", tc.path, rr.Header().Get("Cache-Control"))
		}
		if rr.Header().Get("X-Content-Type-Options") != "nosniff" || rr.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s missing data protection headers: %#v", tc.path, rr.Header())
		}
	}
	rr := request(s, "GET", "/", "", nil, nil)
	csp := rr.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src", "frame-ancestors", "base-uri"} {
		if !strings.Contains(csp, directive) {
			t.Fatalf("shell CSP lacks %s: %q", directive, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("shell allows executable inline content: %q", csp)
	}
}

func TestQRIsOwnerOnlyPNGAndAbsentFromStatus(t *testing.T) {
	qr := []byte("\x89PNG\r\n\x1a\nQR_SECRET_BYTES")
	app := &fakeApplication{qr: qr, pairing: PairingState{State: "awaiting_scan", QRAvailable: true, ExpiresAt: time.Now().Add(time.Minute)}}
	s := newTestServer(t, app)
	session := login(t, s)
	rr := request(s, "GET", "/api/pairing/qr", "", &session, nil)
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rr.Body.Bytes(), qr) {
		t.Fatalf("QR=%d type=%q bytes=%q", rr.Code, rr.Header().Get("Content-Type"), rr.Body.Bytes())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("QR response must never be cached")
	}
	for _, path := range []string{"/api/setup", "/api/status", "/api/pairing"} {
		rr = request(s, "GET", path, "", &session, nil)
		if rr.Code != http.StatusOK || bytes.Contains(rr.Body.Bytes(), []byte("QR_SECRET_BYTES")) || strings.Contains(rr.Body.String(), base64.StdEncoding.EncodeToString(qr)) {
			t.Fatalf("%s contains QR data or failed: %d %s", path, rr.Code, rr.Body.String())
		}
	}
	app.qr = nil
	rr = request(s, "GET", "/api/pairing/qr", "", &session, nil)
	assertDenied(t, rr)
	app.qr, app.err = qr, archive.ErrSourceUnavailable
	rr = request(s, "GET", "/api/pairing/qr", "", &session, nil)
	assertDenied(t, rr)
	if bytes.Contains(rr.Body.Bytes(), []byte("QR_SECRET_BYTES")) {
		t.Fatal("failed QR request leaks image bytes")
	}
}

func TestDisconnectedArchiveRemainsSearchable(t *testing.T) {
	app := &fakeApplication{status: archive.StatusSnapshot{Connection: "disconnected"}, search: archive.SearchPage{Hits: []archive.SearchHit{{MessageID: "saved-message", Snippet: "saved archive result"}}}}
	s := newTestServer(t, app)
	session := login(t, s)
	rr := request(s, "GET", "/api/status", "", &session, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "disconnected") {
		t.Fatalf("status=%d %s", rr.Code, rr.Body.String())
	}
	rr = request(s, "POST", "/api/search", `{"query":"saved"}`, &session, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "saved archive result") || app.count("search") != 1 {
		t.Fatalf("offline search=%d %s", rr.Code, rr.Body.String())
	}
}

type addressOnlyListener struct{ addr net.Addr }

func (l addressOnlyListener) Addr() net.Addr          { return l.addr }
func (addressOnlyListener) Accept() (net.Conn, error) { return nil, errors.New("unexpected Accept") }
func (addressOnlyListener) Close() error              { return nil }

func TestServeRejectsUnsafeOrMismatchedListenersBeforeAccept(t *testing.T) {
	for _, addr := range []*net.TCPAddr{{IP: net.ParseIP("0.0.0.0"), Port: 44771}, {IP: net.ParseIP("192.0.2.1"), Port: 44771}, {IP: net.ParseIP("127.0.0.1"), Port: 44772}, {IP: net.ParseIP("::1"), Port: 44771}} {
		s := newTestServer(t, &fakeApplication{})
		if err := s.Serve(context.Background(), addressOnlyListener{addr: addr}); err == nil || err.Error() == "unexpected Accept" {
			t.Fatalf("unsafe listener reached HTTP serving: addr=%s err=%v", addr, err)
		}
	}
}

func TestPairingCannotStartBeforeExplicitSetup(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	session := login(t, s)
	rr := request(s, "POST", "/api/pairing/start", `{}`, &session, nil)
	if rr.Code != http.StatusConflict || app.count("pairing/start") != 0 {
		t.Fatalf("unconfigured pairing=%d %s starts=%d", rr.Code, rr.Body.String(), app.count("pairing/start"))
	}
	assertFixedError(t, rr)
}

func TestConcurrentBootstrapGrantsExactlyOneOwnerSession(t *testing.T) {
	s := newTestServer(t, &fakeApplication{})
	body, _ := json.Marshal(map[string]string{"token": bootstrapToken(t, s)})
	const attempts = 12
	replies := make(chan *httptest.ResponseRecorder, attempts)
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() { defer wg.Done(); replies <- request(s, "POST", "/api/bootstrap", string(body), nil, nil) }()
	}
	wg.Wait()
	close(replies)
	granted := 0
	for rr := range replies {
		if rr.Code == http.StatusOK {
			granted++
			if len(rr.Result().Cookies()) != 1 {
				t.Fatal("successful concurrent bootstrap has no owner cookie")
			}
		} else {
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("concurrent bootstrap=%d %s", rr.Code, rr.Body.String())
			}
			if len(rr.Result().Cookies()) != 0 {
				t.Fatal("failed concurrent bootstrap sets a cookie")
			}
		}
	}
	if granted != 1 {
		t.Fatalf("bootstrap grants %d owner sessions, want exactly one", granted)
	}
}

func TestBootstrapRejectsMalformedBodiesWithoutConsumingCapability(t *testing.T) {
	s := newTestServer(t, &fakeApplication{})
	token := bootstrapToken(t, s)
	for _, body := range []string{`{"token":"` + token + `","account":"victim"}`, `{"token":"` + token + `"} {}`, `{"token":7}`, strings.Repeat(" ", 32769) + `{}`} {
		rr := request(s, "POST", "/api/bootstrap", body, nil, nil)
		assertFixedError(t, rr)
		if len(rr.Result().Cookies()) != 0 {
			t.Fatal("malformed bootstrap grants a cookie")
		}
		if strings.Contains(rr.Body.String(), token) {
			t.Fatal("malformed bootstrap echoes the private capability")
		}
	}
	login(t, s)
}

func TestReadEndpointsRejectWrongHostAndCrossOriginOwnerRequests(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	session := login(t, s)
	for _, path := range []string{"/api/session", "/api/setup", "/api/pairing", "/api/pairing/qr", "/api/status", "/api/chats"} {
		for _, headers := range []map[string]string{{"Origin": "http://evil.example"}, {"Host": "evil.example"}, {"Host": "localhost:44771"}} {
			before := app.total()
			rr := request(s, "GET", path, "", &session, headers)
			assertDenied(t, rr)
			assertNoCalls(t, app, before)
			if value := rr.Header().Get("Access-Control-Allow-Origin"); value != "" {
				t.Fatalf("%s grants CORS to rejected read: %q", path, value)
			}
		}
	}
}

func TestDuplicateOwnerCookiesCannotSelectAnAuthenticatedIdentity(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	session := login(t, s)
	for _, duplicate := range []string{"attacker-session", ""} {
		for _, ownerFirst := range []bool{false, true} {
			req := httptest.NewRequest("GET", testOrigin+"/api/setup", nil)
			owner := session.cookie
			extra := &http.Cookie{Name: session.cookie.Name, Value: duplicate}
			if ownerFirst {
				req.AddCookie(owner)
				req.AddCookie(extra)
			} else {
				req.AddCookie(extra)
				req.AddCookie(owner)
			}
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("ambiguous cookie accepted (extra=%q ownerFirst=%v): %d %s", duplicate, ownerFirst, rr.Code, rr.Body.String())
			}
		}
	}
	assertNoCalls(t, app, 0)
}

func TestQRGateDoesNotReadImageWhenPairingIsUnavailable(t *testing.T) {
	app := &fakeApplication{pairing: PairingState{State: "disconnected", QRAvailable: false}, qr: []byte("\x89PNG\r\n\x1a\nsecret")}
	s := newTestServer(t, app)
	session := login(t, s)
	rr := request(s, "GET", "/api/pairing/qr", "", &session, nil)
	assertFixedError(t, rr)
	if app.count("qr") != 0 {
		t.Fatal("QR image was accessed despite unavailable pairing")
	}
}

func TestDeletionRevokesTheOwnerSessionAndClearsCookie(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	session := login(t, s)
	rr := request(s, "POST", "/api/data/delete", `{"confirmation":"DELETE LOCAL DATA"}`, &session, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete=%d %s", rr.Code, rr.Body.String())
	}
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != session.cookie.Name || cookies[0].Value != "" || cookies[0].MaxAge >= 0 {
		t.Fatalf("delete does not clear owner cookie: %#v", cookies)
	}
	rr = request(s, "GET", "/api/session", "", &session, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("deleted session remains live: %d %s", rr.Code, rr.Body.String())
	}
}

func TestHTTPBoundsAcceptTheirMaximumAndNormalizeDefaults(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	session := login(t, s)
	rr := request(s, "POST", "/api/pause", strings.Repeat(" ", 32766)+`{}`, &session, nil)
	if rr.Code != http.StatusOK || app.count("pause") != 1 {
		t.Fatalf("maximum JSON body rejected: %d %s", rr.Code, rr.Body.String())
	}
	query := strings.Repeat("א", 2048) // 4096 UTF-8 bytes, not 4096 code points.
	body, _ := json.Marshal(map[string]any{"query": query, "limit": 100, "cursor": strings.Repeat("c", 8192)})
	rr = request(s, "POST", "/api/search", string(body), &session, nil)
	if rr.Code != http.StatusOK || app.searchRequest.Limit != 100 || len(app.searchRequest.Query) != 4096 {
		t.Fatalf("maximum search input rejected: %d %s", rr.Code, rr.Body.String())
	}
	rr = request(s, "POST", "/api/search", `{"query":"needle"}`, &session, nil)
	if rr.Code != http.StatusOK || app.searchRequest.Limit != 30 {
		t.Fatalf("search default limit=%d status=%d", app.searchRequest.Limit, rr.Code)
	}
	rr = request(s, "POST", "/api/context", `{"message_id":"`+strings.Repeat("m", 128)+`","before":50,"after":50}`, &session, nil)
	if rr.Code != http.StatusOK || app.contextRequest.Before != 50 || app.contextRequest.After != 50 {
		t.Fatalf("maximum context input rejected: %d %s", rr.Code, rr.Body.String())
	}
	rr = request(s, "GET", "/api/chats?limit=100&cursor="+strings.Repeat("c", 8192), "", &session, nil)
	if rr.Code != http.StatusOK || app.chatRequest.Limit != 100 {
		t.Fatalf("maximum chats input rejected: %d %s", rr.Code, rr.Body.String())
	}
	rr = request(s, "GET", "/api/chats", "", &session, nil)
	if rr.Code != http.StatusOK || app.chatRequest.Limit != 100 {
		t.Fatalf("chat default limit=%d status=%d", app.chatRequest.Limit, rr.Code)
	}
}

func TestChatDiscoveryRejectsAboveArchivePageLimit(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	session := login(t, s)
	for _, limit := range []string{"101", "200"} {
		before := app.total()
		rr := request(s, "GET", "/api/chats?limit="+limit, "", &session, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("discovery above archive page limit accepted: limit=%s status=%d", limit, rr.Code)
		}
		assertNoCalls(t, app, before)
	}
}

// A cookie captured by another local service on 127.0.0.1 is not sufficient to
// read this service: cookies are host scoped, while the second capability is
// retained only by the owner page for this exact origin.
func TestCapturedCookieCannotReadOrRecoverOwnerCapability(t *testing.T) {
	qr := []byte("\x89PNG\r\n\x1a\nPRIVATE_QR_PAYLOAD")
	app := &fakeApplication{
		setup:   SetupState{Configured: true},
		pairing: PairingState{State: "awaiting_scan", QRAvailable: true},
		qr:      qr,
		status:  archive.StatusSnapshot{Connection: "connected", Messages: 987654},
		chats:   archive.DiscoveryPage{Chats: []archive.DiscoveryChatView{{ID: "private-chat", Name: "PRIVATE_CHAT_NAME"}}},
	}
	s := newTestServer(t, app)
	session := login(t, s)
	for _, path := range []string{"/api/session", "/api/setup", "/api/pairing", "/api/pairing/qr", "/api/status", "/api/chats"} {
		t.Run(path, func(t *testing.T) {
			for _, origin := range []string{"", testOrigin} {
				for _, supplied := range []string{"", "attacker-guessed-capability"} {
					before := app.total()
					rr := request(s, "GET", path, "", &session, map[string]string{"Origin": origin, "X-CSRF-Token": supplied})
					if rr.Code != http.StatusForbidden {
						t.Fatalf("captured cookie with CSRF %q reads %s: %d %s", supplied, path, rr.Code, rr.Body.String())
					}
					assertFixedError(t, rr)
					var failure struct {
						Code string `json:"code"`
					}
					if err := json.Unmarshal(rr.Body.Bytes(), &failure); err != nil || failure.Code != "csrf_denied" {
						t.Fatalf("missing capability should produce csrf_denied: %s", rr.Body.String())
					}
					assertNoCalls(t, app, before)
					for _, private := range []string{session.csrf, session.cookie.Value, "PRIVATE_QR_PAYLOAD", base64.StdEncoding.EncodeToString(qr), "PRIVATE_CHAT_NAME", "987654"} {
						if strings.Contains(rr.Body.String(), private) {
							t.Fatalf("cookie-only %s discloses owner data/capability %q: %s", path, private, rr.Body.String())
						}
					}
				}
			}
			// Normal browser GETs need no Origin header when both capabilities
			// are present; this also covers authenticated page reloads.
			rr := request(s, "GET", path, "", &session, map[string]string{"Origin": ""})
			if rr.Code != http.StatusOK {
				t.Fatalf("owner GET %s rejected with both capabilities: %d %s", path, rr.Code, rr.Body.String())
			}
		})
	}
}

func TestDuplicateJSONFieldsAreRejectedBeforeApplicationCalls(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	session := login(t, s)
	cases := []struct{ name, method, path, body string }{
		{"search_repeated_query", "POST", "/api/search", `{"query":"first","query":"second"}`},
		{"search_repeated_limit", "POST", "/api/search", `{"query":"needle","limit":1,"limit":2}`},
		{"search_escaped_duplicate", "POST", "/api/search", `{"query":"first","\u0071uery":"second"}`},
		{"search_case_alias_duplicate", "POST", "/api/search", `{"query":"first","Query":"second"}`},
		{"search_unicode_alias_duplicate", "POST", "/api/search", `{"query":"needle","chats":["first"],"chat\u017f":["second"]}`},
		{"context_repeated_id", "POST", "/api/context", `{"message_id":"first","message_id":"second"}`},
		{"policy_repeated_generation", "PUT", "/api/policy", `{"expected_generation":7,"expected_generation":8,"date_mode":"all_available","chat_mode":"all"}`},
		{"policy_repeated_mode", "PUT", "/api/policy", `{"date_mode":"all_available","date_mode":"all_available","chat_mode":"all"}`},
		{"policy_case_alias_duplicate", "PUT", "/api/policy", `{"date_mode":"all_available","DATE_MODE":"all_available","chat_mode":"all"}`},
		{"delete_repeated_confirmation", "POST", "/api/data/delete", `{"confirmation":"DELETE LOCAL DATA","confirmation":"DELETE LOCAL DATA"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := app.total()
			rr := request(s, tc.method, tc.path, tc.body, &session, nil)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("duplicate JSON field accepted: %d %s", rr.Code, rr.Body.String())
			}
			assertFixedError(t, rr)
			assertNoCalls(t, app, before)
		})
	}
}

func TestDuplicateBootstrapTokenDoesNotConsumeCapability(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	token := bootstrapToken(t, s)
	for _, body := range []string{`{"token":"` + token + `","token":"` + token + `"}`, `{"token":"` + token + `","Token":"` + token + `"}`, `{"token":"` + token + `","\u0074oken":"` + token + `"}`, `{"to\u212Aen":"` + token + `"}`, `{"token":"` + token + `","to\u212Aen":"` + token + `"}`} {
		rr := request(s, "POST", "/api/bootstrap", body, nil, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("duplicate bootstrap token accepted: %d %s", rr.Code, rr.Body.String())
		}
		assertFixedError(t, rr)
		assertNoCalls(t, app, 0)
		if len(rr.Result().Cookies()) != 0 {
			t.Fatal("duplicate bootstrap token granted owner cookie")
		}
		if strings.Contains(rr.Body.String(), token) {
			t.Fatal("duplicate bootstrap token was echoed")
		}
	}
	login(t, s)
}

func requestWithDuplicateHeader(s *Server, method, path, body string, session *testSession, header string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, testOrigin+path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Content-Type", "application/json")
	if session != nil {
		req.AddCookie(session.cookie)
		req.Header.Set("X-CSRF-Token", session.csrf)
	}
	req.Header.Add(header, req.Header.Get(header))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestDuplicateAuthorityAndCapabilityHeadersAreDenied(t *testing.T) {
	app := &fakeApplication{setup: SetupState{Configured: true}}
	s := newTestServer(t, app)
	session := login(t, s)
	routes := []struct{ method, path, body string }{
		{"GET", "/api/session", ""}, {"GET", "/api/setup", ""}, {"GET", "/api/pairing", ""}, {"GET", "/api/pairing/qr", ""}, {"GET", "/api/status", ""}, {"GET", "/api/chats", ""},
		{"PUT", "/api/policy", `{"date_mode":"all_available","chat_mode":"all"}`}, {"POST", "/api/pairing/start", `{}`}, {"POST", "/api/search", `{"query":"needle"}`}, {"POST", "/api/context", `{"message_id":"opaque"}`}, {"POST", "/api/pause", `{}`}, {"POST", "/api/resume", `{}`}, {"POST", "/api/disconnect", `{}`}, {"POST", "/api/data/delete", `{"confirmation":"DELETE LOCAL DATA"}`},
	}
	for _, route := range routes {
		t.Run(route.path, func(t *testing.T) {
			for _, header := range []string{"Origin", "X-CSRF-Token"} {
				before := app.total()
				rr := requestWithDuplicateHeader(s, route.method, route.path, route.body, &session, header)
				if rr.Code != http.StatusForbidden {
					t.Fatalf("duplicate %s accepted by %s: %d %s", header, route.path, rr.Code, rr.Body.String())
				}
				assertFixedError(t, rr)
				assertNoCalls(t, app, before)
			}
		})
	}
}

func TestDuplicateContentTypeHeadersAreRejectedBeforeApplicationCalls(t *testing.T) {
	app := &fakeApplication{setup: SetupState{Configured: true}}
	s := newTestServer(t, app)
	session := login(t, s)
	routes := []struct{ method, path, body string }{
		{"PUT", "/api/policy", `{"date_mode":"all_available","chat_mode":"all"}`}, {"POST", "/api/pairing/start", `{}`}, {"POST", "/api/search", `{"query":"needle"}`}, {"POST", "/api/context", `{"message_id":"opaque"}`}, {"POST", "/api/pause", `{}`}, {"POST", "/api/resume", `{}`}, {"POST", "/api/disconnect", `{}`}, {"POST", "/api/data/delete", `{"confirmation":"DELETE LOCAL DATA"}`},
	}
	for _, route := range routes {
		t.Run(route.path, func(t *testing.T) {
			before := app.total()
			rr := requestWithDuplicateHeader(s, route.method, route.path, route.body, &session, "Content-Type")
			if rr.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("duplicate Content-Type accepted by %s: %d %s", route.path, rr.Code, rr.Body.String())
			}
			assertFixedError(t, rr)
			assertNoCalls(t, app, before)
		})
	}
}

func TestDuplicateBootstrapHeadersDoNotConsumeCapability(t *testing.T) {
	app := &fakeApplication{}
	s := newTestServer(t, app)
	body, _ := json.Marshal(map[string]string{"token": bootstrapToken(t, s)})
	for _, tc := range []struct {
		header string
		status int
	}{{"Origin", http.StatusForbidden}, {"Content-Type", http.StatusUnsupportedMediaType}} {
		rr := requestWithDuplicateHeader(s, "POST", "/api/bootstrap", string(body), nil, tc.header)
		if rr.Code != tc.status {
			t.Fatalf("duplicate bootstrap %s accepted: %d %s", tc.header, rr.Code, rr.Body.String())
		}
		assertFixedError(t, rr)
		assertNoCalls(t, app, 0)
		if len(rr.Result().Cookies()) != 0 {
			t.Fatal("duplicate bootstrap header granted owner cookie")
		}
	}
	login(t, s)
}
