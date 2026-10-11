package localui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/clientsetup"
)

type fakeConnectionApplication struct {
	*fakeApplication
	snapshot       ConnectionSnapshot
	created        Connection
	guide          clientsetup.Guide
	input          ConnectionRequest
	guideInput     ConnectionGuideRequest
	revoked        ConnectionRevokeRequest
	createOverride *Connection
}

func (a *fakeConnectionApplication) Connections(context.Context) (ConnectionSnapshot, error) {
	return a.snapshot, a.called("clients_list")
}
func (a *fakeConnectionApplication) CreateConnection(_ context.Context, request ConnectionRequest) (Connection, error) {
	a.input = request
	value := a.created
	value.Client, value.Mode, value.Label, value.ExpiresAt = request.Client, request.Mode, request.Label, request.ExpiresAt
	value.Scope = ConnectionScopeSummary{AllChats: request.AllChats, ChatCount: len(request.Chats), StartDate: request.StartDate, EndDate: request.EndDate}
	if a.createOverride != nil {
		value = *a.createOverride
	}
	return value, a.called("clients_create")
}
func (a *fakeConnectionApplication) ConnectionGuide(_ context.Context, request ConnectionGuideRequest) (clientsetup.Guide, error) {
	a.guideInput = request
	return a.guide, a.called("clients_guide")
}
func (a *fakeConnectionApplication) RevokeConnection(_ context.Context, request ConnectionRevokeRequest) error {
	a.revoked = request
	return a.called("clients_revoke")
}

func newConnectionTestServer(t *testing.T) (*Server, *fakeConnectionApplication) {
	t.Helper()
	expires := time.Now().Add(time.Hour)
	connection := Connection{ID: "grant_1", Client: "codex_cli", Label: "My local client", Mode: "local", Revision: 1, State: "ready", ExpiresAt: &expires, Scope: ConnectionScopeSummary{AllChats: true}}
	a := &fakeConnectionApplication{fakeApplication: &fakeApplication{}, snapshot: ConnectionSnapshot{Items: []Connection{connection}, LocalState: "ready", RemoteState: "not_configured", RecoveryState: "ready"}, created: connection,
		guide: clientsetup.Guide{Client: clientsetup.CodexCLI, Mode: clientsetup.Local, Status: clientsetup.NotYetVerified, Configuration: "[mcp_servers.wa_indexer]\ncommand = \"/private/app\"", Command: "'/private/app' mcp stdio --config '/private/client.json'", Explanation: "The exact client still needs a successful call."}}
	s, err := New(Config{Origin: testOrigin}, a)
	if err != nil {
		t.Fatal("connection owner construction failed")
	}
	return s, a
}

func connectionBody(t *testing.T, changes map[string]any) string {
	t.Helper()
	value := map[string]any{"client": "codex_cli", "label": "My client", "mode": "local", "all_chats": true, "chats": []string{}, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339Nano)}
	for key, changed := range changes {
		value[key] = changed
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal("connection fixture encode failed")
	}
	return string(data)
}

func TestConnectionsRequireOwnerCapabilityAndExactOrigin(t *testing.T) {
	s, app := newConnectionTestServer(t)
	owner := login(t, s)
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/connections", ""}, {"POST", "/api/connections/create", connectionBody(t, nil)},
		{"POST", "/api/connections/guide", `{"id":"grant_1","client":"codex_cli","mode":"local"}`},
		{"POST", "/api/connections/revoke", `{"id":"grant_1","revision":1}`},
	} {
		if request(s, route.method, route.path, route.body, nil, nil).Code != 401 {
			t.Fatal("unauthenticated connection request accepted")
		}
		if request(s, route.method, route.path, route.body, &owner, map[string]string{"X-CSRF-Token": ""}).Code != 403 {
			t.Fatal("connection request accepted without origin capability")
		}
		if route.method != "GET" && request(s, route.method, route.path, route.body, &owner, map[string]string{"Origin": ""}).Code != 403 {
			t.Fatal("connection action accepted without exact origin")
		}
	}
	if app.total() != 0 {
		t.Fatal("denied connections reached the app")
	}
}

func TestConnectionRequestsAcceptExplicitScopeAndRejectAuthorityInputs(t *testing.T) {
	s, app := newConnectionTestServer(t)
	owner := login(t, s)
	for _, change := range []map[string]any{
		{"client": "other"}, {"mode": "socket"}, {"label": "bad\nlabel"}, {"all_chats": nil}, {"expires_at": nil}, {"expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339Nano)},
		{"all_chats": false, "chats": []string{}}, {"chats": []string{"chat_1"}}, {"all_chats": false, "chats": []string{"../../private"}},
		{"all_chats": false, "chats": []string{"chat_1", "chat_1"}}, {"account": "other"}, {"principal": "owner"}, {"endpoint": "http://127.0.0.1:1234"}, {"config_path": "/private/path"}, {"token": "PRIVATE_TOKEN"},
		{"start_date": "2026-10-07T00:00:00Z", "end_date": "2026-10-06T00:00:00Z"},
	} {
		if request(s, "POST", "/api/connections/create", connectionBody(t, change), &owner, nil).Code != 400 {
			t.Fatal("invalid connection input accepted")
		}
	}
	valid := connectionBody(t, nil)
	if request(s, "POST", "/api/connections/create", strings.TrimSuffix(valid, "}")+`,"CLIENT":"claude_code"}`, &owner, nil).Code != 400 {
		t.Fatal("case-alias duplicate connection accepted")
	}
	if request(s, "POST", "/api/connections/create", strings.Repeat(" ", maxBodyBytes)+valid, &owner, nil).Code != 413 {
		t.Fatal("valid oversized connection escaped body cap")
	}
	if app.total() != 0 {
		t.Fatal("invalid connection input reached app")
	}
	expiry := time.Now().Add(time.Hour).UTC()
	rr := request(s, "POST", "/api/connections/create", connectionBody(t, map[string]any{"all_chats": false, "chats": []string{"chat_1"}, "start_date": "2026-10-01T00:00:00Z", "end_date": "2026-10-07T00:00:00Z", "expires_at": expiry.Format(time.RFC3339Nano)}), &owner, nil)
	if rr.Code != 200 || app.input.AllChats || len(app.input.Chats) != 1 || app.input.StartDate == nil || !app.input.StartDate.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || app.input.EndDate == nil || !app.input.EndDate.Equal(time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)) || app.input.ExpiresAt == nil || !app.input.ExpiresAt.Equal(expiry) {
		t.Fatal("explicit finite connection scope lost")
	}
	var created Connection
	if json.Unmarshal(rr.Body.Bytes(), &created) != nil || created.Scope.AllChats || created.Scope.ChatCount != 1 || created.Scope.StartDate == nil || !created.Scope.StartDate.Equal(*app.input.StartDate) || created.Scope.EndDate == nil || !created.Scope.EndDate.Equal(*app.input.EndDate) || created.ExpiresAt == nil || !created.ExpiresAt.Equal(expiry) {
		t.Fatal("public created scope broadened or lost bounds")
	}
	if request(s, "POST", "/api/connections/revoke", `{"id":"grant_1","revision":1}`, &owner, nil).Code != 200 || app.revoked.Revision != 1 {
		t.Fatal("connection revoke revision lost")
	}
}

func TestCreatedConnectionCannotBroadenRequestedScopeDatesOrExpiry(t *testing.T) {
	s, app := newConnectionTestServer(t)
	owner := login(t, s)
	start, end, expiry := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), time.Now().Add(time.Hour).UTC()
	body := connectionBody(t, map[string]any{"all_chats": false, "chats": []string{"chat_1"}, "start_date": start, "end_date": end, "expires_at": expiry})
	valid := app.created
	valid.Scope = ConnectionScopeSummary{AllChats: false, ChatCount: 1, StartDate: &start, EndDate: &end}
	valid.ExpiresAt = &expiry
	app.createOverride = &valid
	if request(s, "POST", "/api/connections/create", body, &owner, nil).Code != 200 {
		t.Fatal("exact created-scope control was rejected")
	}
	for _, change := range []func(*Connection){
		func(value *Connection) { value.Scope.AllChats = true }, func(value *Connection) { value.Scope.ChatCount = 2 }, func(value *Connection) { value.Scope.StartDate = nil }, func(value *Connection) { value.Scope.EndDate = nil },
		func(value *Connection) { later := expiry.Add(time.Minute); value.ExpiresAt = &later },
	} {
		bad := valid
		change(&bad)
		app.createOverride = &bad
		if request(s, "POST", "/api/connections/create", body, &owner, nil).Code != 503 {
			t.Fatal("created connection broadened explicit request")
		}
	}
}

func TestConnectionListIsBodyFreeAndDoesNotInventRecoveryReadiness(t *testing.T) {
	s, app := newConnectionTestServer(t)
	owner := login(t, s)
	app.snapshot.LocalState = "PRIVATE_PATH"
	app.snapshot.RemoteState = "PRIVATE_ENDPOINT"
	app.snapshot.RecoveryState = "PRIVATE_ERROR"
	app.snapshot.Items = append(app.snapshot.Items, Connection{ID: "/PRIVATE_CONFIG", Client: "PRIVATE_CLIENT", State: "PRIVATE_STATE"})
	zero := time.Time{}
	app.snapshot.Items = append(app.snapshot.Items, Connection{ID: "grant_bad", Client: "PRIVATE_CLIENT", Label: "bad\nlabel", Mode: "PRIVATE_MODE", State: "PRIVATE_STATE", ExpiresAt: &zero, Scope: ConnectionScopeSummary{ChatCount: -1, StartDate: &zero, EndDate: &zero}})
	rr := request(s, "GET", "/api/connections", "", &owner, nil)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "PRIVATE_") || strings.Contains(rr.Body.String(), "/private/") {
		t.Fatal("connection list exposed private material")
	}
	var value ConnectionSnapshot
	if json.Unmarshal(rr.Body.Bytes(), &value) != nil || value.LocalState != "unavailable" || value.RecoveryState != "recovery_pending" || len(value.Items) != 2 || value.Items[1].Client != "unknown" || value.Items[1].Mode != "" || value.Items[1].State != "recovery_pending" || value.Items[1].Scope.ChatCount != 0 || value.Items[1].ExpiresAt != nil || value.Items[1].Scope.StartDate != nil || value.Items[1].Scope.EndDate != nil {
		t.Fatal("connection projection fabricated availability")
	}
	if app.snapshot.LocalState != "PRIVATE_PATH" {
		t.Fatal("projection changed app snapshot")
	}
}

func TestConnectionGuidesAreInertBoundAndCannotClaimVerification(t *testing.T) {
	s, app := newConnectionTestServer(t)
	owner := login(t, s)
	body := `{"id":"grant_1","client":"codex_cli","mode":"local"}`
	rr := request(s, "POST", "/api/connections/guide", body, &owner, nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "/private/client.json") || !strings.Contains(rr.Body.String(), "not_yet_verified") {
		t.Fatal("inert local guide lost server-owned setup path")
	}
	for _, input := range []string{`{"id":"/private/config","client":"codex_cli","mode":"local"}`, `{"id":"grant_1","client":"codex_cli","mode":"local","endpoint":"https://example.invalid"}`} {
		if request(s, "POST", "/api/connections/guide", input, &owner, nil).Code != 400 {
			t.Fatal("guide accepted caller authority")
		}
	}
	app.guide.Status = clientsetup.SupportStatus("verified")
	if request(s, "POST", "/api/connections/guide", body, &owner, nil).Code != 503 {
		t.Fatal("guide fabricated client verification")
	}
	app.guide.Status = clientsetup.NotYetVerified
	app.guide.Client = clientsetup.ClaudeCode
	if request(s, "POST", "/api/connections/guide", body, &owner, nil).Code != 503 {
		t.Fatal("guide returned another client binding")
	}
	app.guide.Client = clientsetup.CodexCLI
	app.guide.Status = clientsetup.Unsupported
	app.guide.Command = ""
	app.guide.Configuration = ""
	app.err = clientsetup.ErrUnsupported
	if request(s, "POST", "/api/connections/guide", body, &owner, nil).Code != 200 {
		t.Fatal("unsupported guide explanation lost")
	}
}

func TestConnectionErrorsAreFixedAndOptionalCapabilityFailsClosed(t *testing.T) {
	s, app := newConnectionTestServer(t)
	owner := login(t, s)
	for _, check := range []struct {
		err  error
		code int
		text string
	}{
		{ErrConnectionRecovery, 409, "client_recovery_pending"}, {ErrConnectionConflict, 409, "client_connection_changed"}, {ErrConnectionNotFound, 404, "client_connection_not_found"}, {errors.New("PRIVATE_TOKEN /private/config"), 503, "client_connections_unavailable"},
	} {
		app.err = check.err
		rr := request(s, "GET", "/api/connections", "", &owner, nil)
		if rr.Code != check.code || !strings.Contains(rr.Body.String(), check.text) || strings.Contains(rr.Body.String(), "PRIVATE_") {
			t.Fatal("connection error was not fixed")
		}
	}
	plain := newTestServer(t, &fakeApplication{})
	plainOwner := login(t, plain)
	if request(plain, "GET", "/api/connections", "", &plainOwner, nil).Code != 503 {
		t.Fatal("optional connections fabricated readiness")
	}
}
