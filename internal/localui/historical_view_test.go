package localui

import (
	"context"
	"encoding/json"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHistoricalViewExplicitLiveOwnerRoutes(t *testing.T) {
	a := &statsTestApplication{fakeApplication: &fakeApplication{}}
	s, err := New(Config{Origin: testOrigin}, a)
	if err != nil {
		t.Fatal(err)
	}
	session := login(t, s)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/status?source=live", ""},
		{http.MethodGet, "/api/chats?source=live", ""},
		{http.MethodPost, "/api/search", `{"source":"live","query":"canary"}`},
		{http.MethodPost, "/api/context", `{"source":"live","message_id":"opaque_message"}`},
		{http.MethodPost, "/api/stats", `{"source":"live"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rr := request(s, tc.method, tc.path, tc.body, &session, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("explicit live read status=%d", rr.Code)
			}
		})
	}
}

func TestHistoricalViewUnavailableNeverFallsBackToLive(t *testing.T) {
	a := &statsTestApplication{fakeApplication: &fakeApplication{}}
	s, err := New(Config{Origin: testOrigin}, a)
	if err != nil {
		t.Fatal(err)
	}
	session := login(t, s)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/status?source=historical", ""},
		{http.MethodGet, "/api/chats?source=historical", ""},
		{http.MethodPost, "/api/search", `{"source":"historical","query":"canary"}`},
		{http.MethodPost, "/api/context", `{"source":"historical","message_id":"h1.0123456789abcdef.m.opaque_message"}`},
		{http.MethodPost, "/api/stats", `{"source":"historical"}`},
	} {
		rr := request(s, tc.method, tc.path, tc.body, &session, nil)
		if rr.Code < 400 {
			t.Fatalf("disabled historical read fell back: %s", tc.path)
		}
	}
	for _, kind := range []string{"status", "chats", "search", "context", "stats"} {
		if a.count(kind) != 0 {
			t.Fatalf("historical request reached live %s", kind)
		}
	}
}

type historicalOwnerApp struct {
	*fakeApplication
	value                   any
	checks, releases, reads int
	denyAt                  int
	checkHook               func(int)
}

func (a *historicalOwnerApp) ReadArchive(_ context.Context, q ArchiveReadRequest) (ArchiveReadResult, error) {
	a.reads++
	return ArchiveReadResult{Value: a.value, Metadata: archive.HistoricalViewMetadata{Source: "historical", SnapshotID: "0123456789abcdef", CapturedAt: time.Unix(1, 0), ReadOnly: true, Coverage: "partial", CurrentPhoneState: "unknown"}, Check: func(context.Context) error {
		a.checks++
		if a.checkHook != nil {
			a.checkHook(a.checks)
		}
		if a.denyAt == a.checks {
			return archive.ErrUnauthorized
		}
		return nil
	}, Release: func() { a.releases++ }}, nil
}

type historicalMarshalHook struct{ hook func() }

func (v historicalMarshalHook) MarshalJSON() ([]byte, error) {
	v.hook()
	return []byte(`{"Hits":[{"Snippet":"HISTORICAL_PRIVATE_CANARY"}]}`), nil
}
func TestHistoricalViewFinalOwnerAndSelectedReaderChecks(t *testing.T) {
	for _, mode := range []string{"success", "first", "final", "owner_expiry"} {
		t.Run(mode, func(t *testing.T) {
			a := &historicalOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Hits": []string{"HISTORICAL_PRIVATE_CANARY"}}}
			s, err := New(Config{Origin: testOrigin}, a)
			if err != nil {
				t.Fatal(err)
			}
			owner := login(t, s)
			switch mode {
			case "first":
				a.denyAt = 1
			case "final":
				a.denyAt = 2
			case "owner_expiry":
				a.value = historicalMarshalHook{hook: s.RevokeSessions}
			}
			rr := request(s, "POST", "/api/search", `{"source":"historical","query":"canary"}`, &owner, nil)
			if mode == "success" {
				var value map[string]json.RawMessage
				if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &value) != nil || value["archive_view"] == nil || a.checks != 2 {
					t.Fatalf("historical envelope/checks: %d %d", rr.Code, a.checks)
				}
			} else if rr.Code < 400 || strings.Contains(rr.Body.String(), "HISTORICAL_PRIVATE_CANARY") {
				t.Fatal("final denial leaked response")
			}
			if a.releases != 1 {
				t.Fatalf("query lifetime release=%d", a.releases)
			}
		})
	}
}
func TestHistoricalViewOwnerRealmsAndNamespace(t *testing.T) {
	if historicalOwnerTestIsolated(t) {
		return
	}
	a := &historicalOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Chats": []any{}}}
	t.Run("manual", func(t *testing.T) {
		s, e := NewRemote(remoteConfig(t), a)
		if e != nil {
			t.Fatal(e)
		}
		owner := remoteLogin(t, s)
		if r := remoteRequest(s, "GET", "/api/chats?source=historical", "", &owner, nil); r.Code != 200 {
			t.Fatalf("manual source read %d", r.Code)
		}
		a.value = historicalMarshalHook{hook: s.RevokeSessions}
		r := remoteRequest(s, "GET", "/api/chats?source=historical", "", &owner, nil)
		if r.Code < 400 || strings.Contains(r.Body.String(), "CANARY") {
			t.Fatal("manual final expiry leaked")
		}
	})
	t.Run("managed", func(t *testing.T) {
		s, _, _ := remoteFixture(t)
		s.app = a
		owner := managedRemoteLogin(t, s)
		a.value = map[string]any{"Chats": []any{}}
		if r := managedRemoteRequest(s, "GET", "/api/chats?source=historical", "", owner, nil); r.Code != 200 {
			t.Fatalf("managed source read %d", r.Code)
		}
		a.value = historicalMarshalHook{hook: s.RevokeSessions}
		r := managedRemoteRequest(s, "GET", "/api/chats?source=historical", "", owner, nil)
		if r.Code < 400 || strings.Contains(r.Body.String(), "CANARY") {
			t.Fatal("managed final expiry leaked")
		}
	})
	s, e := New(Config{Origin: testOrigin}, a)
	if e != nil {
		t.Fatal(e)
	}
	owner := login(t, s)
	before := a.reads
	for _, body := range []string{`{"source":"live","message_id":"h1.0123456789abcdef.m.id"}`, `{"source":"other","message_id":"id"}`, `{"source":"historical","message_id":"unwrapped"}`, `{"source":"historical","message_id":"h1.0123456789abcdef.c.id"}`} {
		if r := request(s, "POST", "/api/context", body, &owner, nil); r.Code != 400 {
			t.Fatalf("invalid source handle accepted %d", r.Code)
		}
	}
	if a.reads != before {
		t.Fatal("invalid source routed")
	}
	if r := request(s, "POST", "/api/search", `{"source":"historical","query":"canary"}`, nil, map[string]string{"Authorization": "Bearer mcp-token"}); r.Code != 401 {
		t.Fatal("bearer became owner")
	}
}

func TestHistoricalViewOwnerDeliveryOrdering(t *testing.T) {
	for _, mode := range []string{"source_invalidated_during_owner_check", "cancelled_during_owner_check", "owner_revoked_during_source_check"} {
		t.Run(mode, func(t *testing.T) {
			a := &historicalOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Hits": []string{"HISTORICAL_PRIVATE_CANARY"}}}
			s, err := New(Config{Origin: testOrigin}, a)
			if err != nil {
				t.Fatal(err)
			}
			owner := login(t, s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "owner_revoked_during_source_check" {
				a.checkHook = func(n int) {
					if n == 2 {
						s.RevokeSessions()
					}
				}
			}
			req := httptest.NewRequest("GET", testOrigin+"/api/chats?source=historical", nil).WithContext(ctx)
			req.AddCookie(owner.cookie)
			req.Header.Set("X-CSRF-Token", owner.csrf)
			if mode != "owner_revoked_during_source_check" {
				req = req.WithContext(context.WithValue(req.Context(), ownerReadCheckKey{}, func() bool {
					if mode == "source_invalidated_during_owner_check" {
						a.denyAt = a.checks + 1
					} else {
						cancel()
					}
					return true
				}))
			}
			rr := httptest.NewRecorder()
			s.historicalRead(rr, req, ctx, ArchiveReadRequest{Kind: "chats"})
			if rr.Code < 400 || strings.Contains(rr.Body.String(), "HISTORICAL_PRIVATE_CANARY") {
				t.Fatal("authority changed between final checks but historical content escaped")
			}
			if a.releases != 1 {
				t.Fatal("reader not released")
			}
		})
	}
}

func TestHistoricalViewFinalAdmissionAcrossOwnerRealms(t *testing.T) {
	if historicalOwnerTestIsolated(t) {
		return
	}
	for _, realm := range []string{"native", "manual", "managed"} {
		for _, mode := range []string{"revoked", "contended"} {
			t.Run(realm+"/"+mode, func(t *testing.T) {
				a := &historicalOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Hits": []string{"HISTORICAL_PRIVATE_CANARY"}}}
				var run func() *httptest.ResponseRecorder
				var revoke func()
				var mu *sync.Mutex
				switch realm {
				case "native":
					s, e := New(Config{Origin: testOrigin}, a)
					if e != nil {
						t.Fatal(e)
					}
					owner := login(t, s)
					mu = &s.mu
					revoke = s.RevokeSessions
					run = func() *httptest.ResponseRecorder {
						return request(s, "GET", "/api/chats?source=historical", "", &owner, nil)
					}
				case "manual":
					s, e := NewRemote(remoteConfig(t), a)
					if e != nil {
						t.Fatal(e)
					}
					owner := remoteLogin(t, s)
					mu = &s.mu
					revoke = s.RevokeSessions
					run = func() *httptest.ResponseRecorder {
						return remoteRequest(s, "GET", "/api/chats?source=historical", "", &owner, nil)
					}
				case "managed":
					s, _, _ := remoteFixture(t)
					s.app = a
					owner := managedRemoteLogin(t, s)
					mu = &s.mu
					revoke = s.remote.auth.Close
					run = func() *httptest.ResponseRecorder {
						return managedRemoteRequest(s, "GET", "/api/chats?source=historical", "", owner, nil)
					}
				}
				locked := false
				defer func() {
					if locked {
						mu.Unlock()
					}
				}()
				a.checkHook = func(n int) {
					if n == 2 {
						if mode == "revoked" {
							revoke()
						} else {
							mu.Lock()
							locked = true
						}
					}
				}
				rr := run()
				if rr.Code < 400 || strings.Contains(rr.Body.String(), "HISTORICAL_PRIVATE_CANARY") {
					t.Fatal("final owner admission leaked historical response")
				}
				if a.releases != 1 {
					t.Fatal("reader not released")
				}
			})
		}
	}
}

// Password verification admission is intentionally process-wide. Keep new
// managed-owner fixtures from consuming another test's finite auth window.
func historicalOwnerTestIsolated(t *testing.T) bool {
	t.Helper()
	const key = "WA_INDEXER_HISTORICAL_OWNER_TEST"
	if os.Getenv(key) == t.Name() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), key+"="+t.Name())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated owner assertions failed: %v\n%s", err, output)
	}
	return true
}
