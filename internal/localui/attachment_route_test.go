package localui

import (
	"encoding/base64"
	"net/http"
	"testing"
)

func TestRemoteAttachmentTicketRouteForwardsWithoutOwnerCookie(t *testing.T) {
	config := remoteConfig(t)
	called := 0
	ticket := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	path := "/mcp/attachments/" + ticket
	config.MCPHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.URL.Path != path || r.Method != http.MethodGet {
			t.Fatal("remote administration changed the opaque ticket route")
		}
		w.WriteHeader(http.StatusNotFound)
	})
	s, err := NewRemote(config, &fakeApplication{})
	if err != nil {
		t.Fatal(err)
	}
	if remoteRequest(s, "GET", path, "", nil, nil).Code != 404 || called != 1 {
		t.Fatal("remote administration required an owner cookie for an independently guarded attachment capability")
	}
	for _, invalid := range []string{"/mcp/attachments/", "/mcp/attachments/" + ticket + "/file", "/mcp/attachments/" + ticket + "?token=other", "/mcp/attachments/" + ticket[:42]} {
		if remoteRequest(s, "GET", invalid, "", nil, nil).Code == 200 {
			t.Fatal("remote administration accepted a malformed ticket route")
		}
	}
	if called != 1 {
		t.Fatal("a malformed ticket path reached the MCP attachment handler")
	}
}
