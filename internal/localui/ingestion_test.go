package localui

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
)

const ingestionStatusRoute = "/api/ingestion/status"

type ingestionTestApplication struct {
	*fakeApplication
	value ingestion.LatencySnapshot
	hook  func(context.Context)
	at    time.Time
}

func (a *ingestionTestApplication) IngestionStatus(ctx context.Context) (ingestion.LatencySnapshot, error) {
	if a.hook != nil {
		a.hook(ctx)
	}
	return a.value, a.called("ingestion_status")
}

func (a *ingestionTestApplication) HistoricalSnapshotTime() time.Time { return a.at }

func ingestionTestServer(t *testing.T) (*Server, *ingestionTestApplication, testSession) {
	t.Helper()
	a := &ingestionTestApplication{fakeApplication: &fakeApplication{}, value: ingestion.LatencySnapshot{
		Running: true, QueuedCount: 2, QueuedBytes: 20, WaitingCount: 1, WaitingBytes: 10,
		LivePending: 1, OldestLiveAgeMS: 12, HistoryPending: 1, HistoryPrepared: true,
		Phases: []ingestion.LatencyPhaseSnapshot{{Lane: "live", Phase: "projection", Samples: 1, P50MS: 1, P95MS: 1, P99MS: 1, MaxMS: 1}},
	}}
	s, err := New(Config{Origin: testOrigin}, a)
	if err != nil {
		t.Fatal("ingestion owner fixture construction")
	}
	return s, a, login(t, s)
}

func TestIngestionStatusNativeOwnerAndNarrowInput(t *testing.T) {
	s, a, owner := ingestionTestServer(t)
	for _, tc := range []struct {
		method, path, body string
		owner              *testSession
		headers            map[string]string
		want               int
	}{
		{"GET", ingestionStatusRoute, "", nil, nil, 401},
		{"GET", ingestionStatusRoute, "", nil, map[string]string{"Authorization": "Bearer SYNTHETIC_MCP_TOKEN"}, 401},
		{"GET", ingestionStatusRoute, "", &owner, map[string]string{"X-CSRF-Token": ""}, 403},
		{"GET", ingestionStatusRoute, "", &owner, map[string]string{"Origin": "https://other.example.com"}, 403},
		{"GET", ingestionStatusRoute, "", &owner, map[string]string{"Host": "other.example.com"}, 403},
		{"POST", ingestionStatusRoute, `{}`, &owner, nil, 405},
		{"PUT", ingestionStatusRoute, `{}`, &owner, nil, 405},
		{"HEAD", ingestionStatusRoute, "", &owner, nil, 405},
		{"DELETE", ingestionStatusRoute, "", &owner, nil, 405},
		{"GET", ingestionStatusRoute, `{"body":"RawErrorPrivateCanary"}`, &owner, nil, 400},
		{"GET", ingestionStatusRoute + "?path=RawErrorPrivateCanary", "", &owner, nil, 400},
		{"GET", ingestionStatusRoute + "?", "", &owner, nil, 400},
	} {
		rr := request(s, tc.method, tc.path, tc.body, tc.owner, tc.headers)
		if rr.Code != tc.want || strings.Contains(rr.Body.String(), "RawErrorPrivateCanary") {
			t.Fatal("status input or authority boundary failed", rr.Code, tc.want)
		}
	}
	if a.total() != 0 {
		t.Fatal("denied ingestion requests reached the application")
	}
	for _, transfer := range []string{"chunked", "unknown_length", "hidden_body"} {
		r := httptest.NewRequest("GET", testOrigin+ingestionStatusRoute, nil)
		r.AddCookie(owner.cookie)
		r.Header.Set("X-CSRF-Token", owner.csrf)
		switch transfer {
		case "chunked":
			r.TransferEncoding = []string{"chunked"}
		case "unknown_length":
			r.ContentLength = -1
		case "hidden_body":
			r.Body = http.NoBody
			r.ContentLength = 1
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, r)
		if rr.Code != 400 || a.total() != 0 {
			t.Fatal("transfer input reached snapshot read")
		}
	}
	for range 2 {
		rr := request(s, "GET", ingestionStatusRoute, "", &owner, nil)
		var fields map[string]json.RawMessage
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &fields) != nil || len(fields) != 11 || len(rr.Body.Bytes()) > 16*1024 || rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("status exceeded its fixed bounded contract")
		}
		for _, name := range []string{"running", "unsafe", "queued_count", "queued_bytes", "waiting_count", "waiting_bytes", "live_pending", "oldest_live_age_ms", "history_pending", "history_prepared", "phases"} {
			if fields[name] == nil {
				t.Fatal("missing snake-case snapshot field", name)
			}
		}
		var phases []map[string]json.RawMessage
		if json.Unmarshal(fields["phases"], &phases) != nil || len(phases) != 1 || len(phases[0]) != 8 {
			t.Fatal("phase fields exceeded the body-free DTO")
		}
	}
	if a.count("ingestion_status") != 2 {
		t.Fatal("cheap snapshots were throttled or duplicated")
	}
}

func TestIngestionStatusOptionalInterfaceAndFixedFailures(t *testing.T) {
	plain := newTestServer(t, &fakeApplication{})
	owner := login(t, plain)
	if rr := request(plain, "GET", ingestionStatusRoute, "", &owner, nil); rr.Code != 503 {
		t.Fatal("missing optional interface fabricated metrics")
	}
	for _, change := range []string{"error", "private_lane", "private_phase", "negative_count", "too_many_phases"} {
		t.Run(change, func(t *testing.T) {
			s, a, owner := ingestionTestServer(t)
			switch change {
			case "error":
				a.err = errors.New("RawErrorPrivateCanary private path URL token epoch")
			case "private_lane":
				a.value.Phases[0].Lane = "RawErrorPrivateCanary"
			case "private_phase":
				a.value.Phases[0].Phase = "RawErrorPrivateCanary"
			case "negative_count":
				a.value.QueuedCount = -1
			case "too_many_phases":
				a.value.Phases = make([]ingestion.LatencyPhaseSnapshot, 25)
			}
			rr := request(s, "GET", ingestionStatusRoute, "", &owner, nil)
			if rr.Code != 503 || strings.Contains(rr.Body.String(), "RawErrorPrivateCanary") || strings.Contains(rr.Body.String(), "queued_count") {
				t.Fatal("invalid snapshot or provider error escaped")
			}
			assertFixedError(t, rr)
		})
	}
	s, a, owner := ingestionTestServer(t)
	a.value = ingestion.LatencySnapshot{}
	rr := request(s, "GET", ingestionStatusRoute, "", &owner, nil)
	var fields map[string]json.RawMessage
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &fields) != nil || string(fields["running"]) != "false" || string(fields["phases"]) != "[]" {
		t.Fatal("unwired snapshot was not explicitly non-running and empty")
	}
	// The existing historical gate must not turn this diagnostic into a mutation
	// rejection before the application can explain that no live source is wired.
	a.at = time.Now()
	a.err = errors.New("RawErrorPrivateCanary")
	rr = request(s, "GET", ingestionStatusRoute, "", &owner, nil)
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "ingestion_status_unavailable") {
		t.Fatal("snapshot mode did not retain the fixed diagnostic", rr.Code)
	}
}

func TestIngestionStatusBufferedOwnerAndContextFences(t *testing.T) {
	for _, change := range []string{"revoke", "expire", "cancel", "error_cancel", "query_deadline", "final_revoke", "final_cancel", "final_deny", "final_deny_cancel", "admission_busy"} {
		t.Run(change, func(t *testing.T) {
			s, a, owner := ingestionTestServer(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a.hook = func(call context.Context) {
				deadline, ok := call.Deadline()
				if !ok || time.Until(deadline) > 3*time.Second {
					t.Error("snapshot call has no three-second deadline")
				}
				switch change {
				case "revoke":
					s.RevokeSessions()
				case "expire":
					s.mu.Lock()
					s.owner.expires = time.Now().Add(-time.Hour)
					s.mu.Unlock()
				case "cancel":
					cancel()
				case "error_cancel":
					a.err = errors.New("RawErrorPrivateCanary")
					cancel()
				case "query_deadline":
					<-call.Done()
				}
			}
			if strings.HasPrefix(change, "final_") {
				ctx = context.WithValue(ctx, ownerReadCheckKey{}, func() bool {
					if change == "final_revoke" {
						s.RevokeSessions()
					} else if change == "final_cancel" || change == "final_deny_cancel" {
						cancel()
					}
					return change != "final_deny" && change != "final_deny_cancel"
				})
			}
			if change == "admission_busy" {
				ctx = context.WithValue(ctx, ownerReadAdmissionErrorKey{}, func() error { return errOwnerAdmissionBusy })
			}
			if change == "query_deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 10*time.Millisecond)
				defer stop()
			}
			r := httptest.NewRequest("GET", testOrigin+ingestionStatusRoute, nil).WithContext(ctx)
			r.AddCookie(owner.cookie)
			r.Header.Set("X-CSRF-Token", owner.csrf)
			rr := httptest.NewRecorder()
			s.Handler().ServeHTTP(rr, r)
			want := 401
			if change == "cancel" || change == "error_cancel" || change == "final_cancel" || change == "final_deny_cancel" || change == "query_deadline" || change == "admission_busy" {
				want = 503
			}
			if rr.Code != want || strings.Contains(rr.Body.String(), "live_pending") {
				t.Fatal("buffered metrics escaped a revoked or interrupted request", rr.Code, want)
			}
			assertFixedError(t, rr)
		})
	}
}

func TestIngestionStatusDirectHandlerAuthorityAndMethods(t *testing.T) {
	s, a, owner := ingestionTestServer(t)
	for _, tc := range []struct {
		realm  ownerRealm
		method string
		owner  bool
		want   int
	}{
		{realmUnknown, "GET", true, 404},
		{realmNative, "POST", true, 405},
		{realmNative, "GET", false, 401},
	} {
		s.realm = tc.realm
		r := httptest.NewRequest(tc.method, testOrigin+ingestionStatusRoute, nil)
		if tc.owner {
			r.AddCookie(owner.cookie)
			r.Header.Set("X-CSRF-Token", owner.csrf)
		}
		rr := httptest.NewRecorder()
		s.ingestionStatus(rr, r, r.Context())
		if rr.Code != tc.want || a.total() != 0 {
			t.Fatal("direct handler bypassed native owner or method authority")
		}
	}
	s.realm = realmNative
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", testOrigin+ingestionStatusRoute, nil).WithContext(ctx)
	r.AddCookie(owner.cookie)
	r.Header.Set("X-CSRF-Token", owner.csrf)
	rr := httptest.NewRecorder()
	s.ingestionStatus(rr, r, ctx)
	if rr.Code != 503 || a.total() != 0 {
		t.Fatal("initially cancelled read reached the optional application")
	}
}

func TestIngestionStatusAdmissionDirectCancellationChecks(t *testing.T) {
	for _, returnError := range []bool{false, true} {
		s, _, owner := ingestionTestServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx = context.WithValue(ctx, ownerReadAdmissionErrorKey{}, func() error {
			cancel()
			if returnError {
				return errOwnerAdmissionBusy
			}
			return nil
		})
		r := httptest.NewRequest("GET", testOrigin+ingestionStatusRoute, nil).WithContext(ctx)
		r.AddCookie(owner.cookie)
		r.Header.Set("X-CSRF-Token", owner.csrf)
		rr := httptest.NewRecorder()
		if s.ingestionStatusAdmission(rr, r, ctx) || rr.Code != 503 || !strings.Contains(rr.Body.String(), "request_interrupted") {
			t.Fatal("owner admission did not directly recheck caller cancellation")
		}
	}
}

func TestIngestionStatusFinalOwnerLockContentionDoesNotBlock(t *testing.T) {
	s, a, owner := ingestionTestServer(t)
	a.hook = func(context.Context) { s.mu.Lock() }
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request(s, "GET", ingestionStatusRoute, "", &owner, nil) }()
	select {
	case rr := <-done:
		s.mu.Unlock()
		if rr.Code != 503 || a.count("ingestion_status") != 1 || strings.Contains(rr.Body.String(), "queued_count") {
			t.Fatal("final owner lock contention admitted a buffered snapshot")
		}
	case <-time.After(time.Second):
		s.mu.Unlock()
		<-done
		t.Fatal("final owner admission acquired a blocking lock")
	}
}

func TestIngestionStatusDeniedInBothRemoteRealmsAndDirectDispatch(t *testing.T) {
	a := &ingestionTestApplication{fakeApplication: &fakeApplication{}}
	legacy, err := NewRemote(remoteConfig(t), a)
	if err != nil {
		t.Fatal("legacy ingestion fixture")
	}
	owner := remoteLogin(t, legacy)
	for _, who := range []*testSession{nil, &owner} {
		if rr := remoteRequest(legacy, "GET", ingestionStatusRoute, "", who, nil); rr.Code != 404 {
			t.Fatal("legacy admin gained native ingestion authority")
		}
	}
	managed, _, _ := remoteFixture(t)
	managed.app = a
	managedOwner := managedPreservationSession(t, managed)
	if rr := managedRemoteRequest(managed, "GET", ingestionStatusRoute, "", managedOwner, nil); rr.Code != 404 {
		t.Fatal("managed admin gained native ingestion authority", rr.Code)
	}
	for _, dispatch := range []*Server{legacy.dispatch, managed, {app: a}} {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("GET", remoteTestOrigin+ingestionStatusRoute, nil)
		dispatch.api(rr, r, "synthetic-csrf")
		if rr.Code != 404 {
			t.Fatal("inner dispatcher gained native ingestion authority")
		}
	}
	if a.total() != 0 {
		t.Fatal("remote ingestion request reached the application")
	}
}

func TestIngestionStatusDoesNotChangeRegularStatus(t *testing.T) {
	s, a, owner := ingestionTestServer(t)
	rr := request(s, "GET", "/api/status", "", &owner, nil)
	if rr.Code != 200 || a.count("status") != 1 || a.count("ingestion_status") != 0 || strings.Contains(rr.Body.String(), "queued_count") || strings.Contains(rr.Body.String(), "phases") {
		t.Fatal("ordinary status borrowed owner-wide latency metrics")
	}
}

func TestIngestionStatusMaximumFixedSnapshotFitsResponseBudget(t *testing.T) {
	s, a, owner := ingestionTestServer(t)
	a.value = ingestion.LatencySnapshot{
		Running: true, Unsafe: true, QueuedCount: 256, QueuedBytes: 64 << 20,
		WaitingCount: 256, WaitingBytes: 64 << 20, LivePending: 128,
		OldestLiveAgeMS: math.MaxInt64, HistoryPending: 128, HistoryPrepared: true,
	}
	for _, lane := range []string{"live", "history", "source"} {
		for _, phase := range []string{"admission_wait", "queue_wait", "projection", "source_commit", "callback_to_commit", "history_download", "history_decode", "history_preparation"} {
			a.value.Phases = append(a.value.Phases, ingestion.LatencyPhaseSnapshot{
				Lane: lane, Phase: phase, Samples: math.MaxUint64, Saturated: true,
				P50MS: math.MaxInt64, P95MS: math.MaxInt64, P99MS: math.MaxInt64, MaxMS: math.MaxInt64,
			})
		}
	}
	rr := request(s, "GET", ingestionStatusRoute, "", &owner, nil)
	var got ingestion.LatencySnapshot
	if rr.Code != 200 || len(rr.Body.Bytes()) > 16*1024 || json.Unmarshal(rr.Body.Bytes(), &got) != nil || len(got.Phases) != 24 || !got.Valid() || !got.Phases[0].Saturated {
		t.Fatal("maximum valid fixed snapshot exceeded its response budget")
	}
}
