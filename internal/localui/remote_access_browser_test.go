package localui

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/remoteaccess"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type wizardBrowserApplication struct {
	*fakeApplication
	lock                 sync.Mutex
	snapshot             RemoteAccessSnapshot
	failure              bool
	mutations, passwords int
}

func (a *wizardBrowserApplication) RemoteAccessStatus(context.Context) (RemoteAccessSnapshot, error) {
	a.lock.Lock()
	defer a.lock.Unlock()
	return a.snapshot, nil
}
func (a *wizardBrowserApplication) ConfigureRemoteAccess(_ context.Context, v RemoteAccessSubmission) error {
	a.lock.Lock()
	defer a.lock.Unlock()
	if a.failure {
		a.failure = false
		return errors.New("SYNTHETIC_PROVIDER_SECRET private/path")
	}
	if v.Intent.ExposeBrowser && !a.snapshot.PasswordConfigured {
		return ErrRemotePasswordRequired
	}
	a.mutations++
	origin := v.Intent.SavedOrigin
	if origin == "" {
		origin = "https://wa-archive.synthetic.ts.net"
	}
	state := "online"
	if v.Intent.Provider == remoteaccess.Cloudflare {
		state = "route_unverified"
	}
	a.snapshot = RemoteAccessSnapshot{Provider: v.Intent.Provider, State: state, Code: "remote_online", SavedOrigin: origin, NodeName: v.Intent.NodeName, BrowserEnabled: v.Intent.ExposeBrowser, PasswordConfigured: a.snapshot.PasswordConfigured}
	return nil
}
func (a *wizardBrowserApplication) DisableRemoteAccess(context.Context) error {
	a.lock.Lock()
	defer a.lock.Unlock()
	a.mutations++
	a.snapshot.State = "disabled"
	a.snapshot.BrowserEnabled = false
	return nil
}
func (a *wizardBrowserApplication) RemoveRemoteAccess(context.Context) error {
	a.lock.Lock()
	defer a.lock.Unlock()
	a.mutations++
	a.snapshot = RemoteAccessSnapshot{State: "disabled", Code: "remote_disabled", PasswordConfigured: a.snapshot.PasswordConfigured}
	return nil
}
func (a *wizardBrowserApplication) SetRemoteOwnerPassword(_ context.Context, p []byte) error {
	a.lock.Lock()
	defer a.lock.Unlock()
	if len(p) < 12 || len(p) > 128 {
		return errors.New("invalid password")
	}
	a.passwords++
	a.snapshot.PasswordConfigured = true
	return nil
}
func (a *wizardBrowserApplication) RemoteAccessSignIn(context.Context) (string, error) {
	return "https://login.tailscale.com/a/SYNTHETIC_OWNER_SIGNIN", nil
}

// Explicit opt-in only: in-memory synthetic data and provider responses; no
// provider/account, session directory or real archive is opened by this fixture.
func TestRemoteWizardBrowserFixture(t *testing.T) {
	dir := os.Getenv("WA_WIZARD_BROWSER_FIXTURE")
	if dir == "" {
		t.Skip("synthetic browser opt-in")
	}
	a := &wizardBrowserApplication{fakeApplication: &fakeApplication{status: archive.StatusSnapshot{Messages: 1}, setup: SetupState{Configured: true, Demo: true}, search: archive.SearchPage{Hits: []archive.SearchHit{{MessageID: "synthetic_1", ChatID: "synthetic_chat", Snippet: "Synthetic local result during outage", SentAt: time.Now()}}}}, snapshot: RemoteAccessSnapshot{State: "disabled", Code: "remote_disabled"}}
	h := httptest.NewUnstartedServer(nil)
	defer h.Close()
	s, e := New(Config{Origin: "http://" + h.Listener.Addr().String()}, a)
	if e != nil {
		t.Fatal(e)
	}
	h.Config.Handler = s.Handler()
	h.Start()
	data, _ := json.Marshal(map[string]string{"native_url": s.BootstrapURL()})
	if e = os.WriteFile(filepath.Join(dir, "fixture.json"), data, 0600); e != nil {
		t.Fatal(e)
	}
	defer os.Remove(filepath.Join(dir, "fixture.json"))
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-timer.C:
			t.Fatal("browser fixture timeout")
		case <-tick.C:
			command, e := os.ReadFile(filepath.Join(dir, "control"))
			if e != nil {
				continue
			}
			os.Remove(filepath.Join(dir, "control"))
			if string(command) == "stop" {
				a.lock.Lock()
				report, _ := json.Marshal(map[string]int{"mutations": a.mutations, "passwords": a.passwords})
				a.lock.Unlock()
				os.WriteFile(filepath.Join(dir, "server-result.json"), report, 0600)
				return
			}
			a.lock.Lock()
			switch string(command) {
			case "error":
				a.failure = true
			case "cloudflare_unconnected":
				a.snapshot.Provider = remoteaccess.Cloudflare
				a.snapshot.State = "route_unverified"
				a.snapshot.Code = "remote_route_unverified"
				a.snapshot.MCPReady = false
				a.snapshot.AuthorizationReady = false
			case "authorization_ready":
				a.snapshot.AuthorizationReady = true
			default:
				a.snapshot.State = string(command)
			}
			a.lock.Unlock()
			os.WriteFile(filepath.Join(dir, "ack"), command, 0600)
		}
	}
}
