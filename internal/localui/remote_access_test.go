package localui

import (
	"context"
	"errors"
	"fmt"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localauth"
	"github.com/Bodokh/my-whatsapp-mcp/internal/remoteaccess"
	"strings"
	"testing"
)

type fakeRemoteAccessApplication struct {
	*fakeApplication
	snapshot RemoteAccessSnapshot
	input    RemoteAccessSubmission
	signIn   string
}

func (a *fakeRemoteAccessApplication) RemoteAccessStatus(context.Context) (RemoteAccessSnapshot, error) {
	return a.snapshot, a.called("remote_status")
}
func (a *fakeRemoteAccessApplication) ConfigureRemoteAccess(_ context.Context, v RemoteAccessSubmission) error {
	a.input = v
	a.input.Secret = append(remoteaccess.Secret{}, v.Secret...)
	return a.called("remote_configure")
}
func (a *fakeRemoteAccessApplication) DisableRemoteAccess(context.Context) error {
	return a.called("remote_disable")
}
func (a *fakeRemoteAccessApplication) RemoveRemoteAccess(context.Context) error {
	return a.called("remote_remove")
}
func (a *fakeRemoteAccessApplication) SetRemoteOwnerPassword(context.Context, []byte) error {
	return a.called("remote_password")
}
func (a *fakeRemoteAccessApplication) RemoteAccessSignIn(context.Context) (string, error) {
	return a.signIn, a.called("remote_signin")
}
func remoteWizardServer(t *testing.T) (*Server, *fakeRemoteAccessApplication) {
	t.Helper()
	a := &fakeRemoteAccessApplication{fakeApplication: &fakeApplication{}, snapshot: RemoteAccessSnapshot{State: "disabled", Code: "remote_disabled"}}
	s, e := New(Config{Origin: testOrigin}, a)
	if e != nil {
		t.Fatal(e)
	}
	return s, a
}

const wizardSubmission = `{"provider":"ngrok","origin":"https://archive.example.com","secret":"synthetic-secret","expose_browser":false}`

func TestRemoteWizardOwnerDenials(t *testing.T) {
	s, a := remoteWizardServer(t)
	owner := login(t, s)
	for _, path := range []string{"/api/remote/configure", "/api/remote/disable", "/api/remote/remove", "/api/remote/sign-in"} {
		for _, tc := range []struct {
			auth    bool
			headers map[string]string
			want    int
		}{{false, nil, 401}, {true, map[string]string{"Origin": ""}, 403}, {true, map[string]string{"X-CSRF-Token": ""}, 403}, {true, map[string]string{"Origin": "https://evil.example.com"}, 403}} {
			var who *testSession
			if tc.auth {
				who = &owner
			}
			if w := request(s, "POST", path, wizardSubmission, who, tc.headers); w.Code != tc.want {
				t.Fatalf("%s %d want %d", path, w.Code, tc.want)
			}
		}
	}
	if a.total() != 0 {
		t.Fatal("denial reached app")
	}
}
func TestRemoteWizardNarrowWireAndRedaction(t *testing.T) {
	s, a := remoteWizardServer(t)
	owner := login(t, s)
	for _, body := range []string{`{"provider":"unknown"}`, `{"provider":"ngrok","origin":"https://user:password@archive.example.com"}`, `{"provider":"ngrok","origin":"https://127.0.0.1"}`, `{"provider":"ngrok","origin":"https://archive.example.com/mcp"}`, `{"provider":"ngrok","origin":"https://archive.example.com","path":"/private"}`, `{"provider":"ngrok","origin":"https://archive.example.com","helper_version":"x"}`, `{"Provider":"ngrok","origin":"https://archive.example.com"}`, `{"provider":"ngrok","origin":"https://archive.example.com","expose_browser":null}`, `{"provider":"tailscale_private","node_name":"bad name"}`, `{"provider":"ngrok","origin":"https://archive.example.com","secret":"` + strings.Repeat("x", 8193) + `"}`} {
		if w := request(s, "POST", "/api/remote/configure", body, &owner, nil); w.Code != 400 {
			t.Fatalf("invalid input HTTP %d", w.Code)
		}
	}
	if a.total() != 0 {
		t.Fatal("invalid input reached app")
	}
	w := request(s, "POST", "/api/remote/configure", wizardSubmission, &owner, nil)
	if w.Code != 200 || string(a.input.Secret) != "synthetic-secret" || a.input.Intent.SavedOrigin != "https://archive.example.com" {
		t.Fatal("valid input rejected")
	}
	a.err = remoteaccess.ErrBusy
	w = request(s, "POST", "/api/remote/configure", wizardSubmission, &owner, nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "remote_operation_busy") {
		t.Fatal("busy not retryable")
	}
	a.err = errors.New("SYNTHETIC_SECRET_PATH")
	w = request(s, "POST", "/api/remote/configure", wizardSubmission, &owner, nil)
	if w.Code != 503 || strings.Contains(w.Body.String(), "SYNTHETIC") {
		t.Fatal("raw error leaked")
	}
	a.err = nil
	a.signIn = "https://login.tailscale.com/a/synthetic-owner-only"
	w = request(s, "GET", "/api/remote/status", "", &owner, nil)
	if strings.Contains(w.Body.String(), "synthetic") {
		t.Fatal("status leaked capability")
	}
	w = request(s, "POST", "/api/remote/sign-in", `{}`, &owner, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "login.tailscale.com") {
		t.Fatal("separate sign-in unavailable")
	}
}

func TestRemoteWizardConfirmationsAndSignInAuthority(t *testing.T) {
	s, a := remoteWizardServer(t)
	owner := login(t, s)
	for _, action := range []string{"disable", "remove"} {
		for _, body := range []string{`{}`, `{"confirm":true}`, `{"confirm":"other"}`, `{"confirm":"` + action + `","path":"private"}`} {
			if w := request(s, "POST", "/api/remote/"+action, body, &owner, nil); w.Code != 400 {
				t.Fatal("missing explicit confirmation accepted")
			}
		}
		if a.total() != 0 {
			t.Fatal("invalid confirmation mutated app")
		}
	}
	for _, url := range []string{"https://evil.example.com/secret", "http://login.tailscale.com/secret", "https://user:secret@login.tailscale.com/a", "https://login.tailscale.com.evil.example.com/a"} {
		a.signIn = url
		w := request(s, "POST", "/api/remote/sign-in", `{}`, &owner, nil)
		if w.Code != 503 || strings.Contains(w.Body.String(), url) {
			t.Fatal("untrusted sign-in authority exposed")
		}
	}
}

func TestRemoteWizardPasswordFixedErrors(t *testing.T) {
	s, a := remoteWizardServer(t)
	owner := login(t, s)
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("SYNTHETIC_SECRET: %w", localauth.ErrBusy), 409, "owner_auth_busy"},
		{fmt.Errorf("SYNTHETIC_SECRET: %w", remoteaccess.ErrBusy), 409, "owner_auth_busy"},
		{localauth.ErrPassword, 400, "invalid_request"},
		{errors.New("SYNTHETIC_SECRET_PATH"), 503, "owner_auth_unavailable"},
	} {
		a.err = tc.err
		w := request(s, "PUT", "/api/remote/password", `{"password":"synthetic-valid-password"}`, &owner, nil)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) || strings.Contains(w.Body.String(), "SYNTHETIC") {
			t.Fatalf("password error HTTP %d %s", w.Code, w.Body.String())
		}
	}
}
