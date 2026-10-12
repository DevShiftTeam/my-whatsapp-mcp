package remoteaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
)

func TestCloudflareChild(t *testing.T) {
	if os.Getenv("WA_TEST_CLOUDFLARE") == "" {
		return
	}
	cfg, e := os.ReadFile(cloudflareFDPath(3))
	if e != nil || string(cfg) != "{}\n" {
		os.Exit(11)
	}
	key, e := os.ReadFile(cloudflareFDPath(4))
	if e != nil || string(key) != "synthetic-test-token" {
		os.Exit(12)
	}
	// Child output must never escape into application errors or logs.
	fmt.Fprintln(os.Stderr, strings.Repeat("synthetic-secret", 10000))
	if os.Getenv("WA_TEST_EXIT") != "" {
		os.Exit(1)
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	if ready := os.Getenv("WA_TEST_READY"); ready != "" {
		_ = os.WriteFile(ready, []byte("ready"), 0600)
	}
	if os.Getenv("WA_TEST_IGNORE_TERM") != "" {
		for {
			time.Sleep(time.Hour)
		}
	}
	<-ch
	os.Exit(0)
}

func cloudflareFixture(t *testing.T, mode string) (*cloudflareAdapter, *FileStore, appdata.Paths) {
	t.Helper()
	store, paths := testDiskStore(t)
	a := NewCloudflareAdapter(paths, store.lease).(*cloudflareAdapter)
	a.verify = func(context.Context, string) error { return nil }
	a.command = func(path string, args []string, env []string) *exec.Cmd {
		want := []string{"tunnel", "--no-autoupdate", "--config", cloudflareFDPath(3), "--metrics", "127.0.0.1:8125", "run", "--token-file", cloudflareFDPath(4)}
		if !reflect.DeepEqual(args, want) {
			t.Errorf("unsafe argv: %q", args)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestCloudflareChild$")
		cmd.Env = append(env, "WA_TEST_CLOUDFLARE=1", mode+"=1")
		return cmd
	}
	a.grace = 100 * time.Millisecond
	return a, store, paths
}
func cloudflareIntent() Intent {
	return Intent{Version: 1, Provider: Cloudflare, Enabled: true, SavedOrigin: "https://mcp.example.com"}
}

func TestCloudflareLifecycle(t *testing.T) {
	a, store, paths := cloudflareFixture(t, "WA_TEST_IGNORE_TERM")
	for round := 0; round < 2; round++ {
		s, err := a.Open(context.Background(), cloudflareIntent(), Secret("synthetic-test-token"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.Listener().Addr().String() != "127.0.0.1:8124" || s.Origin() != "https://mcp.example.com" {
			t.Fatal("wrong route")
		}
		if err := store.Remove(context.Background()); err == nil {
			t.Fatal("removed active provider")
		}
		if err := store.Save(context.Background(), cloudflareIntent(), Secret("synthetic-test-token")); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"config.yml", "token"} {
			st, err := os.Stat(filepath.Join(paths.RemoteDir, "cloudflare", name))
			if err != nil || st.Mode().Perm() != 0600 {
				t.Fatal("unsafe snapshot", err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err = s.Close(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestCloudflareExitIsGenericAndReaped(t *testing.T) {
	a, _, _ := cloudflareFixture(t, "WA_TEST_EXIT")
	s, err := a.Open(context.Background(), cloudflareIntent(), Secret("synthetic-test-token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-s.Done():
		if err != ErrTransient {
			t.Fatal("unsafe classification", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child not monitored")
	}
	if err = s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestCloudflareRejectsLocallyBeforeLaunch(t *testing.T) {
	a, _, _ := cloudflareFixture(t, "")
	a.verify = func(context.Context, string) error { return ErrState }
	for _, key := range []Secret{nil, Secret("bad\nkey")} {
		if s, err := a.Open(context.Background(), cloudflareIntent(), key, nil); s != nil || err != ErrCredentialsRejected {
			t.Fatal("invalid credential accepted")
		}
	}
	if s, err := a.Open(context.Background(), cloudflareIntent(), Secret("synthetic-test-token"), nil); s != nil || err != ErrState {
		t.Fatal("unverified executable accepted")
	}
}
func TestCloudflareEnvironmentAndBoundedOutput(t *testing.T) {
	t.Setenv("TUNNEL_TOKEN", "secret")
	t.Setenv("HTTPS_PROXY", "secret")
	t.Setenv("CLOUDFLARED_CONFIG", "secret")
	env := cloudflareEnvironment("/private/home")
	if !reflect.DeepEqual(env, []string{"HOME=/private/home", "LANG=C", "LC_ALL=C"}) {
		t.Fatal("ambient environment inherited", env)
	}
	var b cloudflareOutput
	data := []byte(strings.Repeat("secret", 10000))
	n, err := b.Write(data)
	if n != len(data) || err != nil || len(b.bytes) > 1024 {
		t.Fatal("unbounded output")
	}
	b.discard()
	if len(b.bytes) != 0 {
		t.Fatal("retained raw output")
	}
}

func TestCloudflareRetainsCleanupAfterCallerTimeout(t *testing.T) {
	a, store, _ := cloudflareFixture(t, "WA_TEST_IGNORE_TERM")
	ready := filepath.Join(t.TempDir(), "ready")
	old := a.command
	a.command = func(path string, args, env []string) *exec.Cmd {
		cmd := old(path, args, env)
		cmd.Env = append(cmd.Env, "WA_TEST_READY="+ready)
		return cmd
	}
	a.grace = 300 * time.Millisecond
	s, err := a.Open(context.Background(), cloudflareIntent(), Secret("synthetic-test-token"), nil)
	if err != nil {
		t.Fatal(err)
	}
	until(t, func() bool { _, err := os.Stat(ready); return err == nil })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = s.Close(ctx); err != ErrTransient {
		t.Fatal("caller deadline not bounded", err)
	}
	if err = store.Remove(context.Background()); err == nil {
		t.Fatal("registration released before reap")
	}
	start := time.Now()
	if err = s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("ignored SIGTERM did not exercise grace/kill")
	}
	if err = store.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestCloudflareOccupiedIngressNeverSubstitutes(t *testing.T) {
	a, _, _ := cloudflareFixture(t, "")
	l, err := net.Listen("tcp4", "127.0.0.1:8124")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	a.command = func(string, []string, []string) *exec.Cmd { t.Fatal("launched on occupied origin"); return nil }
	if s, err := a.Open(context.Background(), cloudflareIntent(), Secret("synthetic-test-token"), nil); s != nil || err != ErrState {
		t.Fatal("port substitution")
	}
}
func TestCloudflareRejectsSnapshotLinkAndPermissions(t *testing.T) {
	a, _, paths := cloudflareFixture(t, "")
	dir, err := appdata.OpenRemoteProviderDirectory(paths, a.lease, appdata.CloudflareProvider)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := dir.Path()
	dir.Close()
	target := filepath.Join(t.TempDir(), "foreign")
	os.WriteFile(target, []byte("foreign"), 0600)
	token := filepath.Join(home, "token")
	if err = os.Symlink(target, token); err != nil {
		t.Fatal(err)
	}
	if s, err := a.Open(context.Background(), cloudflareIntent(), Secret("synthetic-test-token"), nil); s != nil || err != ErrState {
		t.Fatal("symlink accepted")
	}
	os.Remove(token)
	os.WriteFile(token, []byte("old"), 0644)
	if s, err := a.Open(context.Background(), cloudflareIntent(), Secret("synthetic-test-token"), nil); s != nil || err != ErrState {
		t.Fatal("public token accepted")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "foreign" {
		t.Fatal("foreign file changed")
	}
}
func TestCloudflarePinnedManifestAndOptionalNativeProbe(t *testing.T) {
	raw, err := os.ReadFile("../../packaging/cloudflared-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version string `json:"version"`
		Assets  []struct {
			Platform string `json:"platform"`
			Size     int64  `json:"executable_size"`
			Hash     string `json:"executable_sha256"`
		} `json:"assets"`
	}
	if json.Unmarshal(raw, &manifest) != nil || manifest.Version != CloudflareVersion {
		t.Fatal("manifest drift")
	}
	pin, ok := cloudflarePlatformPin()
	if !ok {
		t.Skip("unsupported platform")
	}
	found := false
	for _, asset := range manifest.Assets {
		if asset.Platform == runtime.GOOS+"/"+runtime.GOARCH {
			found = true
			if asset.Size != pin.size || asset.Hash != pin.hash {
				t.Fatal("pin drift")
			}
		}
	}
	if !found {
		t.Fatal("missing pin")
	}
	if path := os.Getenv("WA_TEST_CLOUDFLARED_BINARY"); path != "" {
		if err := verifyCloudflare(context.Background(), path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCloudflareExecutableTrustBoundary(t *testing.T) {
	home, _ := os.UserHomeDir()
	dir, err := os.MkdirTemp(home, ".wa-cloudflare-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "cloudflared")
	if err = os.WriteFile(path, []byte("not-the-pinned-helper"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err = cloudflarePath(path); err != nil {
		t.Fatal("owned clean executable rejected", err)
	}
	if err = verifyCloudflare(context.Background(), path); err != ErrState {
		t.Fatal("wrong hash/version accepted", err)
	}
	os.Chmod(dir, 0777)
	if _, err = cloudflarePath(path); err != ErrState {
		t.Fatal("writable parent accepted")
	}
	os.Chmod(dir, 0700)
	link := filepath.Join(dir, "link")
	os.Symlink(path, link)
	if _, err = cloudflarePath(link); err != ErrState {
		t.Fatal("symlink executable accepted")
	}
	if runtime.GOOS == "darwin" {
		if err = exec.Command("/bin/chmod", "+a", "everyone allow write", path).Run(); err != nil {
			t.Fatal(err)
		}
		if _, err = cloudflarePath(path); err != ErrState {
			t.Fatal("ACL writer accepted")
		}
	}
}
