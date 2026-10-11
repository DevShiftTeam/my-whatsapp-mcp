package localui

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"testing"
)

type permissionApp struct {
	*fakeApplication
	id       string
	revision uint64
	chats    []archive.ChatID
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
