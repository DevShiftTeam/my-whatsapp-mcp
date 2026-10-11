package localui

import (
	"net/http"
	"testing"
)

func TestRemoteOutboundRoutesRequireBearerIndependentlyOfAdmin(t *testing.T) {
	app := &fakeApplication{}
	cfg := remoteConfig(t)
	calls := 0
	cfg.MCPHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer scoped-client" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	s, err := NewRemote(cfg, app)
	if err != nil {
		t.Fatal(err)
	}
	owner := remoteLogin(t, s)
	for _, path := range []string{"/api/send/message", "/api/send/media", "/api/send/voice", "/api/send/status"} {
		if rr := remoteRequest(s, http.MethodPost, path, `{}`, nil, nil); rr.Code != http.StatusUnauthorized {
			t.Fatal("anonymous send route bypassed bearer admission")
		}
		if rr := remoteRequest(s, http.MethodPost, path, `{}`, &owner, nil); rr.Code != http.StatusUnauthorized {
			t.Fatal("admin cookie substituted for send bearer")
		}
		if rr := remoteRequest(s, http.MethodPost, path, `{}`, nil, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer scoped-client")
		}); rr.Code != http.StatusNoContent {
			t.Fatal("exact send route was not delegated")
		}
	}
	if calls != 12 {
		t.Fatal("send route admission was not delegated exactly once")
	}
	for _, path := range []string{"/api/send", "/api/send/message/extra", "/api/send/unknown", "/api/send/%6dessage", "/api/send/message?token=private"} {
		rr := remoteRequest(s, http.MethodPost, path, `{}`, &owner, func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer scoped-client")
		})
		if rr.Code == http.StatusNoContent {
			t.Fatal("noncanonical send path reached bearer handler")
		}
	}
	if calls != 12 || app.total() != 0 {
		t.Fatal("invalid route or send request reached admin authority")
	}
}

func TestRemoteOutboundRouteUnavailableWithoutMCP(t *testing.T) {
	s := remoteServer(t, &fakeApplication{})
	for _, path := range []string{"/api/send/message", "/api/send/media", "/api/send/voice", "/api/send/status"} {
		if rr := remoteRequest(s, http.MethodPost, path, `{}`, nil, nil); rr.Code != http.StatusNotFound {
			t.Fatal("unconfigured send route was exposed")
		}
	}
}
