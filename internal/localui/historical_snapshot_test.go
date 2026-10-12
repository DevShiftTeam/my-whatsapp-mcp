package localui

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

type snapshotUIFixture struct{ *fakeApplication }

func (*snapshotUIFixture) HistoricalSnapshotTime() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}

func TestHistoricalSnapshotOwnerReadAndAuthenticationBoundaries(t *testing.T) {
	app := &snapshotUIFixture{&fakeApplication{}}
	local, err := New(Config{Origin: testOrigin}, app)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := NewRemote(remoteConfig(t), app)
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []string{request(local, "GET", "/", "", nil, nil).Body.String(), remoteRequest(remote, "GET", "/", "", nil, nil).Body.String()} {
		if !strings.Contains(page, "Historical snapshot — captured 2026-01-02T03:04:05Z") || !strings.Contains(page, "Sending and syncing disabled") || strings.Index(page, "historical-snapshot-banner") > strings.Index(page, `id="auth-screen"`) {
			t.Fatal("persistent signed-out banner missing")
		}
	}
	// Real owner login/bootstrap and dispatch, with mutation tripwires at the app boundary.
	localSession := login(t, local)
	remoteSession := remoteLogin(t, remote)
	for _, path := range []string{"/api/resume", "/api/pause", "/api/disconnect", "/api/pairing/start", "/api/data/delete", "/api/connections/create", "/api/models/operations", "/api/voice/retry", "/api/documents/retry"} {
		localResponse := request(local, "POST", path, `{}`, &localSession, nil)
		remoteResponse := remoteRequest(remote, "POST", path, `{}`, &remoteSession, nil)
		for _, rr := range []struct {
			code int
			body string
		}{{localResponse.Code, localResponse.Body.String()}, {remoteResponse.Code, remoteResponse.Body.String()}} {
			if rr.code != 403 || !strings.Contains(rr.body, "historical_snapshot_read_only") {
				t.Fatalf("mutation %s: %d %s", path, rr.code, rr.body)
			}
		}
	}
	// Managed setup routes are native-only even on an ordinary archive. The
	// snapshot adds a read-only fence locally; legacy remote retains its 404.
	for _, path := range []string{"/api/remote/configure", "/api/remote/disable", "/api/remote/remove", "/api/remote/password"} {
		method := http.MethodPost
		if path == "/api/remote/password" {
			method = http.MethodPut
		}
		if rr := request(local, method, path, `{}`, &localSession, nil); rr.Code != 403 || !strings.Contains(rr.Body.String(), "historical_snapshot_read_only") {
			t.Fatalf("native managed mutation %s: %d", path, rr.Code)
		}
		if rr := remoteRequest(remote, method, path, `{}`, &remoteSession, nil); rr.Code != 404 {
			t.Fatalf("legacy remote exposed native-only route %s: %d", path, rr.Code)
		}
	}
	if app.total() != 0 {
		t.Fatal("denied route reached application")
	}
	if rr := request(local, "POST", "/api/resume", `{}`, nil, nil); rr.Code != 401 {
		t.Fatal("snapshot bypassed auth")
	}
	if rr := request(local, "POST", "/api/search", `{"query":"canary"}`, &localSession, map[string]string{"X-CSRF-Token": "bad"}); rr.Code != 403 || !strings.Contains(rr.Body.String(), "csrf") {
		t.Fatal("snapshot bypassed CSRF")
	}
	if rr := remoteRequest(remote, "POST", "/api/search", `{"query":"canary"}`, &remoteSession, func(r *http.Request) { r.Header.Set("Origin", "https://wrong.example") }); rr.Code != 403 {
		t.Fatal("snapshot bypassed exact origin")
	}
	if rr := request(local, "POST", "/api/search", `{"query":"canary"}`, &localSession, nil); rr.Code != 200 {
		t.Fatalf("POST read denied: %d %s", rr.Code, rr.Body.String())
	}
	if rr := remoteRequest(remote, "GET", "/api/status", "", &remoteSession, nil); rr.Code != 200 {
		t.Fatal("remote status denied")
	}
	if rr := remoteRequest(remote, "POST", "/api/auth/logout", `{}`, &remoteSession, nil); rr.Code != 200 {
		t.Fatal("logout denied")
	}
}
