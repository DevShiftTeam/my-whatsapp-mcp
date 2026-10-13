package localui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

type savedOwnerApp struct {
	*fakeApplication
	reads, checks, releases int
	last                    ArchiveReadRequest
	value                   any
	denyAt                  int
	readErr                 error
	hook                    func(int)
}

func (a *savedOwnerApp) ReadSavedArchive(_ context.Context, q ArchiveReadRequest) (ArchiveReadResult, error) {
	a.reads++
	a.last = q
	if a.readErr != nil {
		return ArchiveReadResult{}, a.readErr
	}
	return ArchiveReadResult{Value: a.value, SavedMetadata: &archive.SavedViewMetadata{Source: "saved", Coverage: "partial", CurrentPhoneState: "unknown"}, Check: func(context.Context) error {
		a.checks++
		if a.hook != nil {
			a.hook(a.checks)
		}
		if a.denyAt == a.checks {
			return archive.ErrUnauthorized
		}
		return nil
	}, Release: func() { a.releases++ }}, nil
}
func TestSavedArchiveDispatch(t *testing.T) {
	for _, tc := range []struct{ method, path, body, kind string }{
		{"POST", "/api/archive/browse", `{"source":"saved"}`, "browse"},
		{"POST", "/api/search", `{"source":"saved","query":"canary"}`, "search"},
		{"POST", "/api/context", `{"source":"saved","message_id":"h1.0123456789abcdef.m.message"}`, "context"},
		{"GET", "/api/chats?source=saved", "", "chats"},
		{"POST", "/api/stats", `{"source":"saved"}`, "stats"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			a := &savedOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Hits": []string{"SAVED_PRIVATE_CANARY"}}}
			s, e := New(Config{Origin: testOrigin}, a)
			if e != nil {
				t.Fatal(e)
			}
			owner := login(t, s)
			rr := request(s, tc.method, tc.path, tc.body, &owner, nil)
			var data map[string]json.RawMessage
			if rr.Code != 200 || a.reads != 1 || a.last.Kind != tc.kind || json.Unmarshal(rr.Body.Bytes(), &data) != nil || data["saved_view"] == nil {
				t.Fatalf("saved dispatch status=%d reads=%d kind=%s body=%s", rr.Code, a.reads, a.last.Kind, rr.Body.String())
			}
			if a.checks != 2 || a.releases != 1 {
				t.Fatal("saved result did not retain guarded delivery lifetime")
			}
		})
	}
}
func TestSavedArchiveFinalChecks(t *testing.T) {
	for _, mode := range []string{"source", "owner", "busy"} {
		t.Run(mode, func(t *testing.T) {
			a := &savedOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Hits": []string{"SAVED_PRIVATE_CANARY"}}}
			s, e := New(Config{Origin: testOrigin}, a)
			if e != nil {
				t.Fatal(e)
			}
			owner := login(t, s)
			locked := false
			defer func() {
				if locked {
					s.mu.Unlock()
				}
			}()
			a.hook = func(n int) {
				if n == 2 {
					switch mode {
					case "source":
						a.denyAt = 2
					case "owner":
						s.RevokeSessions()
					case "busy":
						s.mu.Lock()
						locked = true
					}
				}
			}
			rr := request(s, "POST", "/api/archive/browse", `{"source":"saved"}`, &owner, nil)
			expected := 403
			if mode == "owner" {
				expected = 401
			}
			if mode == "busy" {
				expected = 503
			}
			if rr.Code != expected || strings.Contains(rr.Body.String(), "SAVED_PRIVATE_CANARY") || a.releases != 1 {
				t.Fatalf("final denial status=%d releases=%d", rr.Code, a.releases)
			}
			if mode == "busy" && !strings.Contains(rr.Body.String(), "archive_busy") {
				t.Fatal("contention misclassified")
			}
		})
	}
}
func TestSavedArchiveOwnerRealms(t *testing.T) {
	if historicalOwnerTestIsolated(t) {
		return
	}
	for _, realm := range []string{"manual", "managed"} {
		t.Run(realm, func(t *testing.T) {
			a := &savedOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Hits": []any{}}}
			var rr *httptest.ResponseRecorder
			if realm == "manual" {
				s, e := NewRemote(remoteConfig(t), a)
				if e != nil {
					t.Fatal(e)
				}
				owner := remoteLogin(t, s)
				rr = remoteRequest(s, http.MethodPost, "/api/archive/browse", `{"source":"saved"}`, &owner, nil)
			} else {
				s, _, _ := remoteFixture(t)
				s.app = a
				owner := managedRemoteLogin(t, s)
				rr = managedRemoteRequest(s, http.MethodPost, "/api/archive/browse", `{"source":"saved"}`, owner, nil)
			}
			if rr.Code != 200 || a.reads != 1 || a.releases != 1 {
				t.Fatalf("%s dispatch status=%d reads=%d", realm, rr.Code, a.reads)
			}
		})
	}
}

func TestSavedArchiveRequestValidation(t *testing.T) {
	a := &savedOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Hits": []any{}}}
	s, e := New(Config{Origin: testOrigin}, a)
	if e != nil {
		t.Fatal(e)
	}
	owner := login(t, s)
	for _, tc := range []struct{ path, body string }{
		{"/api/archive/browse", `{"source":"saved","query":"unexpected"}`},
		{"/api/archive/browse", `{"source":"saved","limit":101}`},
		{"/api/archive/browse", `{"source":"saved","cursor":"h1.0123456789abcdef.p.cursor"}`},
		{"/api/archive/browse", `{"source":"saved","chats":["h1.0123456789abcdef.c.chat"]}`},
		{"/api/search", `{"source":"saved","query":""}`},
		{"/api/search", `{"source":"live","query":"canary","cursor":"s1.cursor"}`},
		{"/api/search", `{"source":"historical","query":"canary","cursor":"s1.cursor"}`},
		{"/api/context", `{"source":"saved","message_id":"h1.0123456789abcdef.c.chat"}`},
		{"/api/context", `{"source":"saved","message_id":"s1.cursor"}`},
		{"/api/stats", `{"source":"saved","chat_id":"h1.0123456789abcdef.c.chat"}`},
	} {
		if rr := request(s, "POST", tc.path, tc.body, &owner, nil); rr.Code != 400 {
			t.Fatalf("bad saved request accepted: %s %d", tc.path, rr.Code)
		}
	}
	if a.reads != 0 {
		t.Fatal("invalid input reached saved reader")
	}
	if rr := request(s, "POST", "/api/archive/browse", `{"source":"saved"}`, nil, nil); rr.Code != 401 {
		t.Fatal("anonymous saved browse admitted")
	}
	if rr := request(s, "POST", "/api/archive/browse", `{"source":"saved"}`, &owner, map[string]string{"X-CSRF-Token": "wrong"}); rr.Code != 403 {
		t.Fatal("CSRF saved browse admitted")
	}
	for _, code := range []error{archive.ErrSavedCursorStale, archive.ErrSavedReadLimit} {
		a.readErr = code
		rr := request(s, "POST", "/api/archive/browse", `{"source":"saved"}`, &owner, nil)
		if rr.Code != 409 || strings.Contains(rr.Body.String(), "SAVED_PRIVATE_CANARY") {
			t.Fatal("saved typed error misclassified")
		}
	}
	a.readErr = nil
	before := a.reads
	for _, path := range []string{"/api/setup", "/api/status?source=live", "/api/chats?source=live"} {
		if rr := request(s, "GET", path, "", &owner, nil); rr.Code != 200 {
			t.Fatalf("operational read broken: %s", path)
		}
	}
	if a.reads != before {
		t.Fatal("operational route inherited saved source")
	}
	if rr := request(s, "GET", "/api/status?source=saved", "", &owner, nil); rr.Code != 400 {
		t.Fatal("saved selection reached operational status")
	}
}

type savedSnapshotApp struct{ *savedOwnerApp }

func (*savedSnapshotApp) HistoricalSnapshotTime() time.Time { return time.Unix(1, 0) }
func TestSavedArchiveSnapshotBrowseReadOnly(t *testing.T) {
	a := &savedSnapshotApp{&savedOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Hits": []any{}}}}
	s, e := New(Config{Origin: testOrigin}, a)
	if e != nil {
		t.Fatal(e)
	}
	owner := login(t, s)
	if rr := request(s, "POST", "/api/archive/browse", `{"source":"saved"}`, &owner, nil); rr.Code != 200 {
		t.Fatalf("snapshot saved read %d", rr.Code)
	}
	if rr := request(s, "POST", "/api/pause", `{}`, &owner, nil); rr.Code != 403 {
		t.Fatal("snapshot mutation admitted")
	}
}
func TestSavedArchiveSerializationRevocation(t *testing.T) {
	a := &savedOwnerApp{fakeApplication: &fakeApplication{}}
	s, e := New(Config{Origin: testOrigin}, a)
	if e != nil {
		t.Fatal(e)
	}
	owner := login(t, s)
	a.value = historicalMarshalHook{hook: s.RevokeSessions}
	rr := request(s, "POST", "/api/archive/browse", `{"source":"saved"}`, &owner, nil)
	if rr.Code != 401 || strings.Contains(rr.Body.String(), "CANARY") || a.releases != 1 {
		t.Fatal("serialization revocation escaped or leaked lifetime")
	}
}

func TestSavedArchiveSerializationCancellation(t *testing.T) {
	for _, mode := range []string{"cancelled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			a := &savedOwnerApp{fakeApplication: &fakeApplication{}}
			s, e := New(Config{Origin: testOrigin}, a)
			if e != nil {
				t.Fatal(e)
			}
			owner := login(t, s)
			ctx, cancel := context.WithCancel(context.Background())
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
			}
			defer cancel()
			a.value = historicalMarshalHook{hook: func() {
				if mode == "cancelled" {
					cancel()
				} else {
					<-ctx.Done()
				}
			}}
			r := httptest.NewRequest("POST", testOrigin+"/api/archive/browse", nil).WithContext(ctx)
			r.AddCookie(owner.cookie)
			r.Header.Set("X-CSRF-Token", owner.csrf)
			rr := httptest.NewRecorder()
			s.savedRead(rr, r, ctx, ArchiveReadRequest{Kind: "browse"})
			if rr.Code != 503 || !strings.Contains(rr.Body.String(), "request_interrupted") || strings.Contains(rr.Body.String(), "CANARY") || a.releases != 1 {
				t.Fatalf("interrupted serialization status=%d releases=%d", rr.Code, a.releases)
			}
		})
	}
}
func TestSavedArchiveFinalOwnerRealms(t *testing.T) {
	if historicalOwnerTestIsolated(t) {
		return
	}
	for _, realm := range []string{"manual", "managed"} {
		for _, mode := range []string{"busy", "revoked"} {
			t.Run(realm+"/"+mode, func(t *testing.T) {
				a := &savedOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Hits": []string{"SAVED_PRIVATE_CANARY"}}}
				var run func() *httptest.ResponseRecorder
				var revoke func()
				var mu *sync.Mutex
				if realm == "manual" {
					s, e := NewRemote(remoteConfig(t), a)
					if e != nil {
						t.Fatal(e)
					}
					owner := remoteLogin(t, s)
					mu = &s.mu
					revoke = s.RevokeSessions
					run = func() *httptest.ResponseRecorder {
						return remoteRequest(s, "POST", "/api/archive/browse", `{"source":"saved"}`, &owner, nil)
					}
				} else {
					s, _, _ := remoteFixture(t)
					s.app = a
					owner := managedRemoteLogin(t, s)
					mu = &s.mu
					revoke = s.RevokeSessions
					run = func() *httptest.ResponseRecorder {
						return managedRemoteRequest(s, "POST", "/api/archive/browse", `{"source":"saved"}`, owner, nil)
					}
				}
				locked := false
				defer func() {
					if locked {
						mu.Unlock()
					}
				}()
				a.hook = func(n int) {
					if n == 2 {
						if mode == "busy" {
							mu.Lock()
							locked = true
						} else {
							revoke()
						}
					}
				}
				rr := run()
				expected := 401
				if mode == "busy" {
					expected = 503
				}
				if rr.Code != expected || strings.Contains(rr.Body.String(), "SAVED_PRIVATE_CANARY") || a.releases != 1 {
					t.Fatalf("%s %s final status=%d releases=%d", realm, mode, rr.Code, a.releases)
				}
			})
		}
	}
}

func TestSavedArchiveIndeterminateAuthenticator(t *testing.T) {
	if historicalOwnerTestIsolated(t) {
		return
	}
	a := &savedOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Hits": []string{"SAVED_PRIVATE_CANARY"}}}
	s, _, _ := remoteFixture(t)
	s.app = a
	owner := managedRemoteLogin(t, s)
	a.hook = func(n int) {
		if n == 2 {
			s.remote.auth.Close()
		}
	}
	rr := managedRemoteRequest(s, "POST", "/api/archive/browse", `{"source":"saved"}`, owner, nil)
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "archive_unavailable") || strings.Contains(rr.Body.String(), "archive_busy") || strings.Contains(rr.Body.String(), "SAVED_PRIVATE_CANARY") || a.releases != 1 {
		t.Fatalf("indeterminate admission status=%d releases=%d body=%s", rr.Code, a.releases, rr.Body.String())
	}
}

func TestSavedArchiveCancellationDuringOwnerPreflight(t *testing.T) {
	a := &savedOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Hits": []string{"SAVED_PRIVATE_CANARY"}}}
	s, e := New(Config{Origin: testOrigin}, a)
	if e != nil {
		t.Fatal(e)
	}
	owner := login(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", testOrigin+"/api/archive/browse", nil).WithContext(ctx)
	r.AddCookie(owner.cookie)
	r.Header.Set("X-CSRF-Token", owner.csrf)
	r = r.WithContext(context.WithValue(r.Context(), ownerReadCheckKey{}, func() bool { cancel(); return false }))
	rr := httptest.NewRecorder()
	s.savedRead(rr, r, ctx, ArchiveReadRequest{Kind: "browse"})
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "request_interrupted") || strings.Contains(rr.Body.String(), "SAVED_PRIVATE_CANARY") || a.releases != 1 {
		t.Fatalf("preflight cancellation status=%d releases=%d", rr.Code, a.releases)
	}
}

type savedFailWriter struct {
	header http.Header
	writes int
}

func (w *savedFailWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *savedFailWriter) WriteHeader(int) {}
func (w *savedFailWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("synthetic write failure")
}
func TestSavedArchiveWriteFailureReleases(t *testing.T) {
	a := &savedOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Hits": []any{}}}
	s, e := New(Config{Origin: testOrigin}, a)
	if e != nil {
		t.Fatal(e)
	}
	owner := login(t, s)
	r := httptest.NewRequest("POST", testOrigin+"/api/archive/browse", nil)
	r.AddCookie(owner.cookie)
	r.Header.Set("X-CSRF-Token", owner.csrf)
	w := &savedFailWriter{}
	s.savedRead(w, r, r.Context(), ArchiveReadRequest{Kind: "browse"})
	if w.writes != 1 || a.releases != 1 || a.checks != 2 {
		t.Fatal("write failure retained saved lifetime")
	}
}
func TestSavedArchiveMutationNamespaceRejected(t *testing.T) {
	for _, id := range []string{"s1.cursor", "h1.0123456789abcdef.m.message", "h1.0123456789abcdef.c.chat"} {
		if validOpaque(id) || ValidDocumentRetryIDs([]archive.MessageID{archive.MessageID(id)}) {
			t.Fatalf("read handle accepted by current action: %s", id)
		}
	}
}

func TestSavedArchiveResponseCapWithholdsAndReleases(t *testing.T) {
	a := &savedOwnerApp{fakeApplication: &fakeApplication{}, value: map[string]any{"Hits": []string{"SAVED_PRIVATE_CANARY" + strings.Repeat("x", maxArchiveResponseBytes)}}}
	s, e := New(Config{Origin: testOrigin}, a)
	if e != nil {
		t.Fatal(e)
	}
	owner := login(t, s)
	rr := request(s, "POST", "/api/archive/browse", `{"source":"saved"}`, &owner, nil)
	if rr.Code != 503 || strings.Contains(rr.Body.String(), "SAVED_PRIVATE_CANARY") || a.releases != 1 || a.checks != 1 {
		t.Fatalf("overcap response status=%d releases=%d checks=%d", rr.Code, a.releases, a.checks)
	}
}
