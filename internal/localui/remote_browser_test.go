package localui

import (
	"context"
	"encoding/json"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localauth"
	"github.com/skip2/go-qrcode"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Explicit opt-in synthetic TLS fixture for real Chromium qualification. It never
// opens an archive, account or provider; its test-only control is a local file.
func TestRemoteBrowserFixture(t *testing.T) {
	dir := os.Getenv("WA_REMOTE_BROWSER_FIXTURE")
	if dir == "" {
		t.Skip("browser fixture opt-in only")
	}
	paths, e := appdata.Prepare(filepath.Join(t.TempDir(), "data"))
	if e != nil {
		t.Fatal(e)
	}
	lease, e := appdata.Acquire(paths)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	auth, e := localauth.NewAuthenticator(paths, lease)
	if e != nil {
		t.Fatal(e)
	}
	defer auth.Close()
	if e = auth.SetPassword(context.Background(), []byte("correct passphrase")); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewUnstartedServer(nil)
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	origin := "https://archive.example.com"
	qr, _ := qrcode.Encode("synthetic pairing fixture only", qrcode.Medium, 256)
	expires := time.Now().Add(time.Hour)
	message := archive.MessageView{ID: "message_1", ChatID: "chat_1", Body: "Synthetic private browser message", SenderName: "Fixture sender", SentAt: time.Now()}
	fake := &fakeConnectionApplication{fakeApplication: &fakeApplication{setup: SetupState{Configured: true, Demo: false}, search: archive.SearchPage{Hits: []archive.SearchHit{{MessageID: "message_1", ChatID: "chat_1", ChatName: "Synthetic private chat", Snippet: message.Body, SentAt: message.SentAt}}}, context: archive.ContextPage{Messages: []archive.MessageView{message}}, pairing: PairingState{State: "waiting", QRAvailable: true, ExpiresAt: expires}, qr: qr}, snapshot: ConnectionSnapshot{Items: []Connection{{ID: "remote_1", Client: "codex_cli", Label: "Synthetic private connection", Mode: "remote", Revision: 1, State: "ready", ExpiresAt: &expires, Scope: ConnectionScopeSummary{AllChats: true}}}, LocalState: "ready", RemoteState: "ready", RecoveryState: "ready"}}
	s, e := NewManagedRemote(ManagedRemoteConfig{Context: ctx, Origin: origin, Generation: 1, Authenticator: auth}, fake)
	if e != nil {
		t.Fatal(e)
	}
	server.Config.Handler = s.Handler()
	server.StartTLS()
	nativeServer := httptest.NewUnstartedServer(nil)
	defer nativeServer.Close()
	native, e := New(Config{Origin: "http://" + nativeServer.Listener.Addr().String()}, fake)
	if e != nil {
		t.Fatal(e)
	}
	nativeServer.Config.Handler = native.Handler()
	nativeServer.Start()
	data, _ := json.Marshal(map[string]string{"origin": origin, "port": port, "native_url": native.BootstrapURL()})
	defer os.Remove(filepath.Join(dir, "fixture.json"))
	if e = os.WriteFile(filepath.Join(dir, "fixture.json"), data, 0600); e != nil {
		t.Fatal(e)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("browser fixture timeout")
		case <-ticker.C:
			command, e := os.ReadFile(filepath.Join(dir, "control"))
			if e != nil {
				continue
			}
			os.Remove(filepath.Join(dir, "control"))
			switch string(command) {
			case "stop":
				return
			case "expire":
				s.mu.Lock()
				future := s.clock.nowLocked().Add(25 * time.Hour)
				s.clock.wall = func() time.Time { return future }
				s.mu.Unlock()
				os.WriteFile(filepath.Join(dir, "expired"), []byte("ok"), 0600)
			}
		}
	}
}
