package localui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

type fakeRecoveryApplication struct {
	*fakeApplication
	progress *archive.RebuildProgress
	input    archive.RebuildControl
	commands map[string]bool
}

func (a *fakeRecoveryApplication) RebuildProgress(context.Context) (*archive.RebuildProgress, error) {
	return a.progress, a.called("recovery_read")
}
func (a *fakeRecoveryApplication) ControlRebuild(_ context.Context, input archive.RebuildControl) (*archive.RebuildProgress, error) {
	a.input = input
	if err := a.called("recovery_control"); err != nil {
		return nil, err
	}
	if a.commands[input.CommandID] {
		return a.progress, nil
	}
	if input.ExpectedRevision != a.progress.Revision {
		return nil, archive.ErrPolicyChanged
	}
	a.commands[input.CommandID] = true
	a.progress.Revision++
	if input.Action == "extend" {
		a.progress.CopyLimit += 10000
		a.progress.DiscoveryLimit += 512
	}
	return a.progress, nil
}
func newRecoveryServer(t *testing.T) (*Server, *fakeRecoveryApplication) {
	t.Helper()
	app := &fakeRecoveryApplication{fakeApplication: &fakeApplication{}, progress: &archive.RebuildProgress{OperationID: "operation", Revision: 7, State: "running", CopyLimit: 10000, DiscoveryLimit: 512}, commands: map[string]bool{}}
	s, err := New(Config{Origin: testOrigin}, app)
	if err != nil {
		t.Fatal(err)
	}
	return s, app
}

func TestRecoveryOwnerAuthenticationCSRFAndOrigin(t *testing.T) {
	s, app := newRecoveryServer(t)
	owner := login(t, s)
	for _, route := range []struct{ method, path, body string }{{"GET", "/api/recovery", ""}, {"POST", "/api/recovery/control", `{"expected_revision":7,"command_id":"12345678-1234-1234-1234-123456789abc","action":"pause"}`}} {
		if got := request(s, route.method, route.path, route.body, nil, nil).Code; got != 401 {
			t.Fatalf("unauthenticated recovery: %d", got)
		}
		if got := request(s, route.method, route.path, route.body, &owner, map[string]string{"X-CSRF-Token": ""}).Code; got != 403 {
			t.Fatalf("missing CSRF: %d", got)
		}
		if got := request(s, route.method, route.path, route.body, nil, map[string]string{"Authorization": "Bearer ordinary-mcp-token"}).Code; got != 401 {
			t.Fatalf("MCP bearer became owner: %d", got)
		}
	}
	for _, origin := range []string{"", "https://outside.example"} {
		if got := request(s, "POST", "/api/recovery/control", `{"expected_revision":7,"command_id":"12345678-1234-1234-1234-123456789abc","action":"pause"}`, &owner, map[string]string{"Origin": origin}).Code; got != 403 {
			t.Fatalf("untrusted origin: %d", got)
		}
	}
	if app.total() != 0 {
		t.Fatal("denied owner controls reached app")
	}
	if got := request(s, "GET", "/api/recovery", "", &owner, nil).Code; got != 200 {
		t.Fatalf("owner progress unavailable: %d", got)
	}
	if !remoteProtected["/api/recovery"] || !remoteProtected["/api/recovery/control"] {
		t.Fatal("recovery absent from managed owner allowlist")
	}
}

func TestRecoveryControlCASIdempotencyAndFixedErrors(t *testing.T) {
	s, app := newRecoveryServer(t)
	owner := login(t, s)
	body := `{"expected_revision":7,"command_id":"12345678-1234-1234-1234-123456789abc","action":"extend"}`
	for i := 0; i < 2; i++ {
		if got := request(s, "POST", "/api/recovery/control", body, &owner, nil).Code; got != 200 {
			t.Fatalf("control: %d", got)
		}
	}
	if app.progress.CopyLimit != 20000 || app.progress.DiscoveryLimit != 1024 || app.progress.Revision != 8 {
		t.Fatal("idempotent grant duplicated")
	}
	stale := strings.Replace(body, "123456789abc", "123456789def", 1)
	if got := request(s, "POST", "/api/recovery/control", stale, &owner, nil); got.Code != 409 || !strings.Contains(got.Body.String(), "recovery_changed") {
		t.Fatal("stale CAS not conflict")
	}
	app.err = errors.New("PRIVATE_ACCOUNT PRIVATE_PATH")
	if got := request(s, "GET", "/api/recovery", "", &owner, nil); got.Code != 503 || strings.Contains(got.Body.String(), "PRIVATE_") || !strings.Contains(got.Body.String(), "recovery_unavailable") {
		t.Fatal("private error disclosed")
	}
}

func TestRecoveryRejectsMalformedControlAndKeepsOrdinaryProfileEmpty(t *testing.T) {
	s, app := newRecoveryServer(t)
	owner := login(t, s)
	for _, body := range []string{`{}`, `{"expected_revision":0,"command_id":"id","action":"pause"}`, `{"expected_revision":7,"command_id":"12345678-1234-1234-1234-123456789abc","action":"run_forever"}`, `{"expected_revision":7,"command_id":"12345678-1234-1234-1234-123456789abc","action":"extend","copy_limit":999999}`, `{"expected_revision":7,"command_id":"12345678-1234-1234-1234-123456789abc","action":"pause","ACTION":"resume"}`} {
		if got := request(s, "POST", "/api/recovery/control", body, &owner, nil).Code; got != http.StatusBadRequest {
			t.Fatalf("malformed recovery control: %d", got)
		}
	}
	if app.total() != 0 {
		t.Fatal("invalid control reached writer")
	}
	app.progress = nil
	got := request(s, "GET", "/api/recovery", "", &owner, nil)
	if got.Code != 200 || strings.TrimSpace(got.Body.String()) != "null" {
		t.Fatal("normal profile invented recovery")
	}
	plain := newTestServer(t, &fakeApplication{})
	session := login(t, plain)
	if got := request(plain, "GET", "/api/recovery", "", &session, nil); got.Code != 200 || strings.TrimSpace(got.Body.String()) != "null" {
		t.Fatal("optional recovery changed normal profile")
	}
}

func TestRecoveryUsesExistingManualAndManagedOwnerRealms(t *testing.T) {
	_, app := newRecoveryServer(t)
	body := `{"expected_revision":7,"command_id":"12345678-1234-1234-1234-123456789abc","action":"pause"}`
	t.Run("manual", func(t *testing.T) {
		s, err := NewRemote(remoteConfig(t), app)
		if err != nil {
			t.Fatal(err)
		}
		if w := remoteRequest(s, "GET", "/api/recovery", "", nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer ordinary-mcp-token") }); w.Code != 401 {
			t.Fatal("manual bearer gained owner progress")
		}
		owner := remoteLogin(t, s)
		if w := remoteRequest(s, "GET", "/api/recovery", "", &owner, nil); w.Code != 200 {
			t.Fatalf("manual owner denied: %d", w.Code)
		}
		if w := remoteRequest(s, "POST", "/api/recovery/control", body, &owner, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }); w.Code != 403 {
			t.Fatal("manual control bypassed csrf")
		}
		if w := remoteRequest(s, "POST", "/api/recovery/control", body, &owner, func(r *http.Request) { r.Header.Del("Origin") }); w.Code != 403 {
			t.Fatal("manual control bypassed origin")
		}
	})
	t.Run("managed", func(t *testing.T) {
		s, _, _ := remoteFixture(t)
		s.app = app
		if w := managedRemoteRequest(s, "GET", "/api/recovery", "", nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer ordinary-mcp-token") }); w.Code != 401 {
			t.Fatal("managed bearer gained owner progress")
		}
		owner := managedRemoteLogin(t, s)
		if w := managedRemoteRequest(s, "GET", "/api/recovery", "", owner, nil); w.Code != 200 {
			t.Fatalf("managed owner denied: %d", w.Code)
		}
		if w := managedRemoteRequest(s, "POST", "/api/recovery/control", body, owner, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }); w.Code != 403 {
			t.Fatal("managed control bypassed csrf")
		}
		if w := managedRemoteRequest(s, "POST", "/api/recovery/control", body, owner, func(r *http.Request) { r.Header.Del("Origin") }); w.Code != 403 {
			t.Fatal("managed control bypassed origin")
		}
	})
	if app.count("recovery_control") != 0 {
		t.Fatal("denied realm requests reached control")
	}
}
