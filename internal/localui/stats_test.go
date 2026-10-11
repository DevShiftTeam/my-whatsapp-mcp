package localui

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

type statsTestApplication struct {
	*fakeApplication
	query archive.StatsRequest
}

func (a *statsTestApplication) Stats(_ context.Context, query archive.StatsRequest) (archive.StatsSnapshot, error) {
	a.query = query
	return archive.StatsSnapshot{SampledAt: 1791720000, Messages: 3, Chats: 1, Sent: 1, Received: 2,
		Daily: []archive.StatsActivity{}, Kinds: []archive.StatsKind{}, TopChats: []archive.StatsChat{}}, a.called("stats")
}

func TestStatsHTTPFiltersAndOwnerAdmission(t *testing.T) {
	app := &statsTestApplication{fakeApplication: &fakeApplication{}}
	s, err := New(Config{Origin: testOrigin}, app)
	if err != nil {
		t.Fatal(err)
	}
	session := login(t, s)
	body := `{"from":1791633600,"until":1791720000,"chat_id":"opaque_chat","kind":"voice"}`
	for _, denied := range []struct {
		name    string
		method  string
		session *testSession
		headers map[string]string
	}{
		{"no session", http.MethodPost, nil, nil},
		{"no csrf", http.MethodPost, &session, map[string]string{"X-CSRF-Token": ""}},
		{"wrong origin", http.MethodPost, &session, map[string]string{"Origin": "https://other.invalid"}},
		{"GET", http.MethodGet, &session, nil},
	} {
		t.Run(denied.name, func(t *testing.T) {
			rr := request(s, denied.method, "/api/stats", body, denied.session, denied.headers)
			assertDenied(t, rr)
			if app.count("stats") != 0 {
				t.Fatal("denied stats request reached application")
			}
		})
	}
	rr := request(s, http.MethodPost, "/api/stats", body, &session, nil)
	if rr.Code != http.StatusOK || app.query.ChatID != "opaque_chat" || app.query.Kind != "voice" || app.query.From.Unix() != 1791633600 || app.query.Until.Unix() != 1791720000 {
		t.Fatalf("filters/status: %#v, %d", app.query, rr.Code)
	}
	var response map[string]json.RawMessage
	if err = json.Unmarshal(rr.Body.Bytes(), &response); err != nil || response["sampled_at"] == nil || response["messages"] == nil || response["top_chats"] == nil || response["Messages"] != nil {
		t.Fatalf("stats JSON contract: %s, %v", rr.Body.String(), err)
	}
}

func TestStatsHTTPRejectsInvalidFiltersBeforeApplication(t *testing.T) {
	app := &statsTestApplication{fakeApplication: &fakeApplication{}}
	s, err := New(Config{Origin: testOrigin}, app)
	if err != nil {
		t.Fatal(err)
	}
	session := login(t, s)
	for _, body := range []string{
		`{"from":1,"until":1}`, `{"from":2,"until":1}`, `{"until":253402300800}`, `{"from":-62135596801}`, `{"until":-62135596800}`,
		`{"from":1.5}`, `{"from":"2026-10-11"}`, `{"chat_id":"unsafe@source"}`, `{"kind":"document"}`, `{"unknown":true}`,
	} {
		rr := request(s, http.MethodPost, "/api/stats", body, &session, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid filter %s status=%d", body, rr.Code)
		}
	}
	if app.count("stats") != 0 {
		t.Fatal("invalid filters reached application")
	}
	// Optional adapters lacking Stats do not panic or weaken authentication.
	plain := newTestServer(t, &fakeApplication{})
	plainSession := login(t, plain)
	if rr := request(plain, http.MethodPost, "/api/stats", `{}`, &plainSession, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("unsupported adapter status=%d", rr.Code)
	}
}
