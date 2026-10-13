package localui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const transcriptionStatusRoute = "/api/transcription/status"

type transcriptionTestApplication struct {
	*fakeApplication
	value TranscriptionStatus
	hook  func(context.Context)
}

func (a *transcriptionTestApplication) TranscriptionStatus(ctx context.Context) (TranscriptionStatus, error) {
	if a.hook != nil {
		a.hook(ctx)
	}
	return a.value, a.called("transcription_status")
}

func transcriptionTestServer(t *testing.T) (*Server, *transcriptionTestApplication, testSession) {
	t.Helper()
	a := &transcriptionTestApplication{fakeApplication: &fakeApplication{}, value: TranscriptionStatus{Configured: true, State: "ready", HebrewReady: true}}
	s, err := New(Config{Origin: testOrigin}, a)
	if err != nil {
		t.Fatal("transcription owner construction")
	}
	return s, a, login(t, s)
}

func TestTranscriptionStatusNativeOwnerAndNarrowInput(t *testing.T) {
	s, a, owner := transcriptionTestServer(t)
	for _, tc := range []struct {
		method, path, body string
		owner              *testSession
		headers            map[string]string
		want               int
	}{
		{"GET", transcriptionStatusRoute, "", nil, nil, 401},
		{"GET", transcriptionStatusRoute, "", nil, map[string]string{"Authorization": "Bearer SYNTHETIC_MCP_TOKEN"}, 401},
		{"GET", transcriptionStatusRoute, "", &owner, map[string]string{"X-CSRF-Token": ""}, 403},
		{"GET", transcriptionStatusRoute, "", &owner, map[string]string{"Origin": "https://other.example.com"}, 403},
		{"GET", transcriptionStatusRoute, "", &owner, map[string]string{"Host": "other.example.com"}, 403},
		{"POST", transcriptionStatusRoute, `{}`, &owner, nil, 405},
		{"PUT", transcriptionStatusRoute, `{}`, &owner, nil, 405},
		{"GET", transcriptionStatusRoute, `{"url":"PRIVATE_CANARY"}`, &owner, nil, 400},
		{"GET", transcriptionStatusRoute + "?path=PRIVATE_CANARY", "", &owner, nil, 400},
		{"GET", transcriptionStatusRoute + "?", "", &owner, nil, 400},
	} {
		rr := request(s, tc.method, tc.path, tc.body, tc.owner, tc.headers)
		if rr.Code != tc.want || strings.Contains(rr.Body.String(), "PRIVATE_CANARY") {
			t.Fatal("status input or authority boundary failed", rr.Code, tc.want)
		}
	}
	if a.total() != 0 {
		t.Fatal("denied status requests reached the application")
	}
	rr := request(s, "GET", transcriptionStatusRoute, "", &owner, nil)
	var fields map[string]json.RawMessage
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &fields) != nil || len(fields) != 3 || fields["configured"] == nil || fields["state"] == nil || fields["hebrew_ready"] == nil || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("status response exceeded the three-field private contract")
	}
}

func TestTranscriptionStatusFiniteStatesAndFixedFailures(t *testing.T) {
	for _, value := range []TranscriptionStatus{
		{State: "not_configured"}, {State: "stopped"}, {Configured: true, State: "stopped"},
		{Configured: true, State: "connecting"}, {Configured: true, State: "ready", HebrewReady: true},
		{Configured: true, State: "busy", HebrewReady: true}, {Configured: true, State: "busy"},
		{Configured: true, State: "unavailable"}, {Configured: true, State: "mismatch"},
	} {
		s, a, owner := transcriptionTestServer(t)
		a.value = value
		if rr := request(s, "GET", transcriptionStatusRoute, "", &owner, nil); rr.Code != 200 {
			t.Fatal("valid finite status refused", value.State)
		}
	}
	for _, value := range []TranscriptionStatus{
		{Configured: true, State: "PRIVATE_CANARY"}, {Configured: true, State: "not_configured"},
		{State: "ready", HebrewReady: true}, {Configured: true, State: "ready"},
		{Configured: true, State: "unavailable", HebrewReady: true}, {Configured: true, State: "stopped", HebrewReady: true},
	} {
		s, a, owner := transcriptionTestServer(t)
		a.value = value
		rr := request(s, "GET", transcriptionStatusRoute, "", &owner, nil)
		if rr.Code != 503 || strings.Contains(rr.Body.String(), "PRIVATE_CANARY") || strings.Contains(rr.Body.String(), "hebrew_ready") {
			t.Fatal("invalid app status escaped the finite response boundary")
		}
		assertFixedError(t, rr)
	}
	s, a, owner := transcriptionTestServer(t)
	a.err = errors.New("PRIVATE_TOKEN PRIVATE_ENV_PATH PRIVATE_PROVIDER_URL")
	rr := request(s, "GET", transcriptionStatusRoute, "", &owner, nil)
	if rr.Code != 503 || strings.Contains(rr.Body.String(), "PRIVATE_") {
		t.Fatal("raw status error escaped")
	}
	assertFixedError(t, rr)
	plain := newTestServer(t, &fakeApplication{})
	plainOwner := login(t, plain)
	if rr := request(plain, "GET", transcriptionStatusRoute, "", &plainOwner, nil); rr.Code != 503 {
		t.Fatal("optional status interface fabricated readiness")
	}
}

func TestTranscriptionStatusBufferedDeliveryRechecksOwnerAndCancellation(t *testing.T) {
	for _, change := range []string{"revoke", "expire", "cancel"} {
		t.Run(change, func(t *testing.T) {
			s, a, owner := transcriptionTestServer(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a.hook = func(context.Context) {
				switch change {
				case "revoke":
					s.RevokeSessions()
				case "expire":
					s.mu.Lock()
					s.owner.expires = time.Now().Add(-time.Hour)
					s.mu.Unlock()
				case "cancel":
					cancel()
				}
			}
			r := httptest.NewRequest("GET", testOrigin+transcriptionStatusRoute, nil).WithContext(ctx)
			r.AddCookie(owner.cookie)
			r.Header.Set("X-CSRF-Token", owner.csrf)
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, r)
			want := 401
			if change == "cancel" {
				want = 503
			}
			if rr.Code != want || strings.Contains(rr.Body.String(), "hebrew_ready") || strings.Contains(rr.Body.String(), "configured") {
				t.Fatal("revoked/cancelled status was delivered", rr.Code)
			}
		})
	}
}

func TestTranscriptionStatusFinalAdmissionFencesBufferedResponse(t *testing.T) {
	for _, change := range []string{"revoke", "cancel"} {
		t.Run(change, func(t *testing.T) {
			s, _, owner := transcriptionTestServer(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = context.WithValue(ctx, ownerReadCheckKey{}, func() bool {
				if change == "revoke" {
					s.RevokeSessions()
				} else {
					cancel()
				}
				return true
			})
			r := httptest.NewRequest("GET", testOrigin+transcriptionStatusRoute, nil).WithContext(ctx)
			r.AddCookie(owner.cookie)
			r.Header.Set("X-CSRF-Token", owner.csrf)
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, r)
			want := 401
			if change == "cancel" {
				want = 503
			}
			if rr.Code != want || strings.Contains(rr.Body.String(), "hebrew_ready") {
				t.Fatal("final admission delivered buffered status after invalidation", rr.Code)
			}
		})
	}
}

func TestTranscriptionStatusRejectsTransferInputAndRapidChecks(t *testing.T) {
	s, a, owner := transcriptionTestServer(t)
	r := httptest.NewRequest("GET", testOrigin+transcriptionStatusRoute, nil)
	r.AddCookie(owner.cookie)
	r.Header.Set("X-CSRF-Token", owner.csrf)
	r.TransferEncoding = []string{"chunked"}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != 400 || a.total() != 0 {
		t.Fatal("unexpected streaming input reached status probe")
	}
	if rr := request(s, "GET", transcriptionStatusRoute, "", &owner, nil); rr.Code != 200 {
		t.Fatal("first check refused")
	}
	rr = request(s, "GET", transcriptionStatusRoute, "", &owner, nil)
	if rr.Code != 429 || rr.Header().Get("Retry-After") != "1" || a.count("transcription_status") != 1 {
		t.Fatal("rapid checks bypassed bounded admission")
	}
}

func TestTranscriptionStatusBoundsAndRateLimitsActualChecks(t *testing.T) {
	s, a, owner := transcriptionTestServer(t)
	entered := make(chan struct{})
	a.hook = func(ctx context.Context) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 3*time.Second {
			t.Error("status application call has no short deadline")
		}
		close(entered)
		<-ctx.Done()
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request(s, "GET", transcriptionStatusRoute, "", &owner, nil) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("status check did not start")
	}
	rr := request(s, "GET", transcriptionStatusRoute, "", &owner, nil)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatal("overlapping owner checks were not bounded", rr.Code)
	}
	select {
	case rr := <-done:
		if rr.Code != 503 || strings.Contains(rr.Body.String(), "hebrew_ready") || a.count("transcription_status") != 1 {
			t.Fatal("timed-out owner check fabricated readiness or duplicated probes")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("owner status handler exceeded its finite budget")
	}
}

func TestTranscriptionStatusDeniedInBothRemoteRealms(t *testing.T) {
	a := &transcriptionTestApplication{fakeApplication: &fakeApplication{}}
	legacy, err := NewRemote(remoteConfig(t), a)
	if err != nil {
		t.Fatal("legacy status fixture")
	}
	owner := remoteLogin(t, legacy)
	for _, who := range []*testSession{nil, &owner} {
		if rr := remoteRequest(legacy, "GET", transcriptionStatusRoute, "", who, nil); rr.Code != 404 {
			t.Fatal("legacy admin gained native status authority")
		}
	}
	managed, _, _ := remoteFixture(t)
	managed.app = a
	managedOwner := managedPreservationSession(t, managed)
	if rr := managedRemoteRequest(managed, "GET", transcriptionStatusRoute, "", managedOwner, nil); rr.Code != 404 {
		t.Fatal("managed admin gained native status authority", rr.Code)
	}
	for _, dispatch := range []*Server{legacy.dispatch, managed, {app: a}} {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("GET", remoteTestOrigin+transcriptionStatusRoute, nil)
		dispatch.api(rr, r, "synthetic-csrf")
		if rr.Code != 404 {
			t.Fatal("inner dispatcher gained native status authority")
		}
	}
	if a.total() != 0 {
		t.Fatal("remote status request reached the application")
	}
}
