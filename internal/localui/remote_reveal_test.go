package localui

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeRemoteApplication struct {
	*fakeConnectionApplication
	secret   RemoteCredentialSecret
	revealed ConnectionRevealRequest
}

func (a *fakeRemoteApplication) RevealRemoteCredential(_ context.Context, input ConnectionRevealRequest) (RemoteCredentialSecret, error) {
	a.revealed = input
	return a.secret, a.called("remote_reveal")
}

func newRemoteTestServer(t *testing.T) (*Server, *fakeRemoteApplication) {
	t.Helper()
	_, connections := newConnectionTestServer(t)
	token := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("R", 32)))
	a := &fakeRemoteApplication{fakeConnectionApplication: connections, secret: RemoteCredentialSecret{ID: "remote_1", Revision: 4, PublicURL: "https://archive.example.com/mcp", Token: token, ExpiresAt: time.Now().Add(time.Hour)}}
	s, err := New(Config{Origin: testOrigin}, a)
	if err != nil {
		t.Fatal("remote owner construction failed")
	}
	return s, a
}

func TestRemoteRevealRequiresExplicitOwnerPostAndStrictAuthority(t *testing.T) {
	s, app := newRemoteTestServer(t)
	owner := login(t, s)
	body := `{"id":"remote_1","revision":4}`
	if request(s, "POST", "/api/connections/reveal", body, nil, nil).Code != 401 {
		t.Fatal("unauthenticated secret reveal accepted")
	}
	for _, headers := range []map[string]string{{"X-CSRF-Token": ""}, {"X-CSRF-Token": "wrong"}, {"Origin": ""}, {"Origin": "http://evil.example"}, {"Host": "127.0.0.1:44772"}} {
		if request(s, "POST", "/api/connections/reveal", body, &owner, headers).Code != 403 {
			t.Fatal("remote reveal accepted wrong authority")
		}
	}
	if request(s, "GET", "/api/connections/reveal", "", &owner, nil).Code != 405 {
		t.Fatal("GET revealed remote secret")
	}
	for _, invalid := range []string{`{"id":"/private/file","revision":4}`, `{"id":"remote_1","revision":0}`, `{"id":"remote_1","revision":4,"public_url":"https://evil.example/mcp"}`, `{"id":"remote_1","revision":4,"token":"supplied"}`, `{"id":"remote_1","revision":4,"REVISION":5}`} {
		if request(s, "POST", "/api/connections/reveal", invalid, &owner, nil).Code != 400 {
			t.Fatal("reveal accepted caller authority or ambiguous revision")
		}
	}
	if app.total() != 0 {
		t.Fatal("denied secret requests reached app")
	}
	rr := request(s, "POST", "/api/connections/reveal", body, &owner, nil)
	if rr.Code != 200 || rr.Header().Get("Cache-Control") != "no-store" || len(rr.Body.Bytes()) > 4096 || app.revealed.ID != "remote_1" || app.revealed.Revision != 4 {
		t.Fatal("valid explicit reveal failed")
	}
	var secret RemoteCredentialSecret
	if json.Unmarshal(rr.Body.Bytes(), &secret) != nil || secret.Token != app.secret.Token || secret.PublicURL != app.secret.PublicURL {
		t.Fatal("explicit secret response lost trusted fields")
	}
}

func TestRemoteSecretDoesNotEnterOrdinaryOwnerResponsesOrDiagnostics(t *testing.T) {
	s, app := newRemoteTestServer(t)
	owner := login(t, s)
	for _, route := range []struct{ method, path, body string }{{"GET", "/api/connections", ""}, {"POST", "/api/connections/create", connectionBody(t, nil)}, {"POST", "/api/connections/guide", `{"id":"grant_1","client":"codex_cli","mode":"local"}`}} {
		rr := request(s, route.method, route.path, route.body, &owner, nil)
		if rr.Code != 200 || strings.Contains(rr.Body.String(), app.secret.Token) || strings.Contains(rr.Body.String(), `"token"`) {
			t.Fatal("ordinary connection response exposed secret")
		}
	}
	for _, rendered := range []string{fmt.Sprint(app.secret), fmt.Sprintf("%#v", app.secret)} {
		if strings.Contains(rendered, app.secret.Token) || strings.Contains(rendered, app.secret.PublicURL) {
			t.Fatal("secret diagnostic exposed private fields")
		}
	}
}

func TestRemoteRevealRejectsUnboundExpiredNoncanonicalAndOversizeSecrets(t *testing.T) {
	s, app := newRemoteTestServer(t)
	owner := login(t, s)
	valid := app.secret
	body := `{"id":"remote_1","revision":4}`
	for _, change := range []func(*RemoteCredentialSecret){
		func(value *RemoteCredentialSecret) { value.ID = "another" }, func(value *RemoteCredentialSecret) { value.Revision = 5 }, func(value *RemoteCredentialSecret) { value.ExpiresAt = time.Now().Add(-time.Second) },
		func(value *RemoteCredentialSecret) { value.Token = "bad" }, func(value *RemoteCredentialSecret) { value.PublicURL = "http://archive.example.com/mcp" }, func(value *RemoteCredentialSecret) { value.PublicURL = "https://127.0.0.1/mcp" }, func(value *RemoteCredentialSecret) { value.PublicURL = "https://archive.example.com/mcp?token=secret" },
	} {
		bad := valid
		change(&bad)
		app.secret = bad
		rr := request(s, "POST", "/api/connections/reveal", body, &owner, nil)
		if rr.Code != 503 || strings.Contains(rr.Body.String(), valid.Token) {
			t.Fatal("invalid secret response accepted or echoed")
		}
	}
	app.secret = valid
	// Exercise serialization size independently of semantic URL validation.
	base, err := json.Marshal(valid)
	if err != nil {
		t.Fatal("secret cap control failed")
	}
	boundary := valid
	boundary.PublicURL = strings.Repeat("x", maxRemoteSecretResponse-len(base)+len(valid.PublicURL))
	encoded, ok := encodeRemoteSecret(boundary)
	if !ok || len(encoded) != maxRemoteSecretResponse {
		t.Fatal("exact secret response cap control rejected")
	}
	clear(encoded)
	boundary.PublicURL += "x"
	if encoded, ok := encodeRemoteSecret(boundary); ok || len(encoded) != 0 {
		t.Fatal("secret response encoder exceeded cap")
	}
	app.secret.PublicURL = boundary.PublicURL
	rr := request(s, "POST", "/api/connections/reveal", body, &owner, nil)
	if rr.Code != 503 || strings.Contains(rr.Body.String(), valid.Token) || len(rr.Body.Bytes()) > 4096 {
		t.Fatal("oversized secret response accepted or echoed")
	}
	app.secret = valid
	app.err = ErrConnectionConflict
	if request(s, "POST", "/api/connections/reveal", body, &owner, nil).Code != 409 {
		t.Fatal("stale secret revision was not rejected")
	}
	plain, _ := newConnectionTestServer(t)
	plainOwner := login(t, plain)
	if request(plain, "POST", "/api/connections/reveal", body, &plainOwner, nil).Code != 503 {
		t.Fatal("missing optional secret capability fabricated reveal")
	}
}

func TestConnectionRecoveryFamiliesRemainIndependentWithLegacyFallback(t *testing.T) {
	s, app := newRemoteTestServer(t)
	owner := login(t, s)
	app.snapshot.RecoveryState = "invalid"
	app.snapshot.LocalRecoveryState = "ready"
	app.snapshot.RemoteRecoveryState = "invalid"
	rr := request(s, "GET", "/api/connections", "", &owner, nil)
	var snapshot ConnectionSnapshot
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &snapshot) != nil || snapshot.LocalRecoveryState != "ready" || snapshot.RemoteRecoveryState != "invalid" {
		t.Fatal("remote recovery disabled healthy native family")
	}
	app.snapshot.RecoveryState = "ready"
	app.snapshot.LocalRecoveryState = ""
	app.snapshot.RemoteRecoveryState = "PRIVATE_ERROR"
	rr = request(s, "GET", "/api/connections", "", &owner, nil)
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &snapshot) != nil || snapshot.LocalRecoveryState != "ready" || snapshot.RemoteRecoveryState != "recovery_pending" || strings.Contains(rr.Body.String(), "PRIVATE_ERROR") {
		t.Fatal("family recovery fallback fabricated readiness")
	}
}
