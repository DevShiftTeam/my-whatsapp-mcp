package localui

import (
	"context"
	"encoding/json"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"testing"
)

type permissionApp struct {
	*fakeApplication
	id         string
	revision   uint64
	chats      []archive.ChatID
	permission archive.SendPermission
	expected   uint64
	modeError  error
}

func (a *permissionApp) SetSendPermissionMode(_ context.Context, id string, revision uint64, permission archive.SendPermission, expected uint64) error {
	a.id, a.revision, a.permission, a.expected = id, revision, permission, expected
	if err := a.called("send_permission_mode"); err != nil {
		return err
	}
	return a.modeError
}

func TestOwnerSendModeRequiresOwnCASAndRejectsAmbiguousPermissions(t *testing.T) {
	app := &permissionApp{fakeApplication: &fakeApplication{}}
	s, err := New(Config{Origin: testOrigin}, app)
	if err != nil {
		t.Fatal(err)
	}
	owner := login(t, s)
	path := "/api/connections/send-permission"
	for _, body := range []string{
		`{"id":"client_1","revision":1,"mode":"all","chats":[]}`,
		`{"id":"client_1","revision":1,"mode":"everything","chats":[],"expected_send_revision":0}`,
		`{"id":"client_1","revision":1,"mode":"all","chats":["chat_1"],"expected_send_revision":0}`,
		`{"id":"client_1","revision":1,"mode":"none","chats":["chat_1"],"expected_send_revision":0}`,
		`{"id":"client_1","revision":1,"mode":"selected","chats":[],"expected_send_revision":0}`,
		`{"id":"client_1","revision":1,"mode":"selected","chats":["chat_1","chat_1"],"expected_send_revision":0}`,
		`{"id":"client_1","revision":1,"mode":"all","chats":null,"expected_send_revision":0}`,
		`{"id":"client_1","revision":1,"mode":null,"chats":[],"expected_send_revision":0}`,
		`{"id":"client_1","revision":1,"mode":"all","chats":[],"expected_send_revision":null}`,
		`{"id":"client_1","revision":1,"chats":[],"expected_send_revision":0}`,
	} {
		if response := request(s, "POST", path, body, &owner, nil); response.Code != 400 {
			t.Fatalf("invalid permission accepted: %s", response.Body)
		}
	}
	if app.total() != 0 {
		t.Fatal("invalid permissions reached application")
	}
	body := `{"id":"client_1","revision":1,"mode":"all","chats":[],"expected_send_revision":3}`
	if request(s, "POST", path, body, nil, nil).Code != 401 || request(s, "POST", path, body, &owner, map[string]string{"Origin": ""}).Code != 403 || request(s, "POST", path, body, &owner, map[string]string{"X-CSRF-Token": ""}).Code != 403 {
		t.Fatal("mode permission accepted without owner, origin or CSRF")
	}
	response := request(s, "POST", path, body, &owner, nil)
	if response.Code != 200 || app.permission.Mode != "all" || app.expected != 3 || app.revision != 1 {
		t.Fatalf("mode request failed: %s", response.Body)
	}
	var result struct {
		Permission archive.SendPermission `json:"send_permission"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Permission.Mode != "all" || result.Permission.Revision != 4 || result.Permission.Chats == nil {
		t.Fatal("mode response omitted permission revision or array")
	}
	app.modeError = archive.ErrSendConflict
	if response = request(s, "POST", path, body, &owner, nil); response.Code != 409 {
		t.Fatalf("stale permission did not return conflict: %s", response.Body)
	}
}

func (a *permissionApp) SetSendPermission(_ context.Context, id string, revision uint64, chats []archive.ChatID) error {
	a.id = id
	a.revision = revision
	a.chats = chats
	return a.called("send_permission")
}
func TestSendPermissionRequiresOwnerOriginAndCSRF(t *testing.T) {
	app := &permissionApp{fakeApplication: &fakeApplication{}}
	s, err := New(Config{Origin: testOrigin}, app)
	if err != nil {
		t.Fatal(err)
	}
	owner := login(t, s)
	path := "/api/connections/send-permission"
	body := `{"id":"client_1","revision":1,"chats":["chat_1"]}`
	if request(s, "POST", path, body, nil, nil).Code != 401 {
		t.Fatal("missing owner accepted")
	}
	if request(s, "POST", path, body, &owner, map[string]string{"X-CSRF-Token": ""}).Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if request(s, "POST", path, body, &owner, map[string]string{"Origin": ""}).Code != 403 {
		t.Fatal("missing exact origin accepted")
	}
	if app.total() != 0 {
		t.Fatal("denied requests reached app")
	}
	if response := request(s, "POST", path, body, &owner, nil); response.Code != 200 || app.id != "client_1" || app.revision != 1 || len(app.chats) != 1 {
		t.Fatalf("owner permission failed: %s", response.Body)
	}
	if request(s, "POST", path, `{"id":"client_1","revision":1,"chats":[],"account":"other"}`, &owner, nil).Code != 400 {
		t.Fatal("authority injection accepted")
	}
}
