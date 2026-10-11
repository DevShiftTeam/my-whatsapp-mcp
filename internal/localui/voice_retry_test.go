package localui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type fakeVoiceRetryApplication struct {
	*fakeApplication
	value VoiceRetrySnapshot
}

func (a *fakeVoiceRetryApplication) RetryVoiceDownloads(context.Context) (VoiceRetrySnapshot, error) {
	return a.value, a.called("voice_retry")
}

func TestVoiceRetryOwnerCapabilityOriginAndFixedEmptyRequest(t *testing.T) {
	app := &fakeVoiceRetryApplication{fakeApplication: &fakeApplication{}, value: VoiceRetrySnapshot{Queued: 3, Skipped: 1, Requested: 2}}
	s, err := New(Config{Origin: testOrigin}, app)
	if err != nil {
		t.Fatal(err)
	}
	owner := login(t, s)
	for _, denial := range []struct {
		session *testSession
		headers map[string]string
		status  int
	}{
		{nil, nil, 401},
		{&owner, map[string]string{"X-CSRF-Token": ""}, 403},
		{&owner, map[string]string{"Origin": ""}, 403},
		{&owner, map[string]string{"Origin": "https://different.example"}, 403},
	} {
		if request(s, "POST", "/api/voice/retry", `{}`, denial.session, denial.headers).Code != denial.status {
			t.Fatal("retry did not enforce owner capability and origin")
		}
	}
	for _, body := range []string{`{"account":"another"}`, `{"limit":33}`, `{"media_id":"private-id"}`, `{"protocol_reference":"private-reference"}`, `null`, `[]`, ``} {
		if request(s, "POST", "/api/voice/retry", body, &owner, nil).Code != 400 {
			t.Fatal("retry accepted caller-selected account or download input")
		}
	}
	if request(s, "GET", "/api/voice/retry", "", &owner, nil).Code != 405 || app.total() != 0 {
		t.Fatal("read request or denied retry reached application")
	}
	rr := request(s, "POST", "/api/voice/retry", `{}`, &owner, nil)
	var result VoiceRetrySnapshot
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &result) != nil || result != app.value || app.count("voice_retry") != 1 {
		t.Fatal("bounded retry result not exposed to owner")
	}
	var fields map[string]any
	if json.Unmarshal(rr.Body.Bytes(), &fields) != nil || len(fields) != 3 {
		t.Fatal("retry exposed identities or private references")
	}
}

func TestVoiceRetryErrorsAndInvalidCountsAreFixed(t *testing.T) {
	app := &fakeVoiceRetryApplication{fakeApplication: &fakeApplication{err: errors.New("PRIVATE_REFERENCE PRIVATE_PATH")}}
	s, err := New(Config{Origin: testOrigin}, app)
	if err != nil {
		t.Fatal(err)
	}
	owner := login(t, s)
	rr := request(s, "POST", "/api/voice/retry", `{}`, &owner, nil)
	if rr.Code != 503 || !strings.Contains(rr.Body.String(), "voice_retry_unavailable") || strings.Contains(rr.Body.String(), "PRIVATE") {
		t.Fatal("retry leaked private backend failure")
	}
	app.err = nil
	for _, value := range []VoiceRetrySnapshot{{Queued: -1}, {Skipped: -1}, {Requested: -1}, {Requested: 9}, {Queued: 33}, {Queued: 32, Skipped: 1}} {
		app.value = value
		if request(s, "POST", "/api/voice/retry", `{}`, &owner, nil).Code != 503 {
			t.Fatal("retry returned impossible batch count")
		}
	}
	plain := newTestServer(t, &fakeApplication{})
	plainOwner := login(t, plain)
	if request(plain, "POST", "/api/voice/retry", `{}`, &plainOwner, nil).Code != 503 {
		t.Fatal("optional retry port fabricated availability")
	}
}

func TestVoiceRetryRemoteRequiresPasswordSessionAndCapability(t *testing.T) {
	app := &fakeVoiceRetryApplication{fakeApplication: &fakeApplication{}, value: VoiceRetrySnapshot{Queued: 1}}
	s, err := NewRemote(remoteConfig(t), app)
	if err != nil {
		t.Fatal(err)
	}
	if remoteRequest(s, "POST", "/api/voice/retry", `{}`, nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer mcp-token") }).Code != 401 {
		t.Fatal("MCP bearer bypassed remote administration password")
	}
	owner := remoteLogin(t, s)
	for _, change := range []func(*http.Request){
		func(r *http.Request) { r.Header.Del("X-CSRF-Token") },
		func(r *http.Request) { r.Header.Del("Origin") },
	} {
		if remoteRequest(s, "POST", "/api/voice/retry", `{}`, &owner, change).Code != 403 {
			t.Fatal("remote retry accepted missing capability or origin")
		}
	}
	if app.total() != 0 {
		t.Fatal("denied remote retry reached application")
	}
	if remoteRequest(s, "POST", "/api/voice/retry", `{}`, &owner, nil).Code != 200 || app.count("voice_retry") != 1 {
		t.Fatal("remote owner retry unavailable")
	}
}
