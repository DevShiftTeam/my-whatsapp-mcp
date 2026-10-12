package remoteaccess

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
)

func testDiskStore(t *testing.T) (*FileStore, appdata.Paths) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "data")
	paths, err := appdata.Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Close() })
	return NewStore(paths, lease), paths
}
func storedIntent() Intent {
	return Intent{Version: 1, Provider: Ngrok, Enabled: true, SavedOrigin: "https://saved.ngrok-free.app"}
}
func TestStoreTransaction(t *testing.T) {
	for _, stage := range []string{"before_commit", "after_commit"} {
		t.Run(stage, func(t *testing.T) {
			s, _ := testDiskStore(t)
			ctx := context.Background()
			old := storedIntent()
			if err := s.Save(ctx, old, Secret("old-secret")); err != nil {
				t.Fatal(err)
			}
			next := old
			next.ExposeBrowser = true
			s.checkpoint = func(at string) error {
				if at == stage {
					return errors.New("synthetic interrupted")
				}
				return nil
			}
			if s.Save(ctx, next, Secret("new-secret")) == nil {
				t.Fatal("interruption ignored")
			}
			s.checkpoint = nil
			got, key, err := s.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "before_commit" && (got != old || string(key) != "old-secret") {
				t.Fatal("prior commit changed")
			}
			if stage == "after_commit" && (got != next || string(key) != "new-secret") {
				t.Fatal("new commit mixed identities")
			}
		})
	}
}
func TestStoreRejectsCorruption(t *testing.T) {
	for _, kind := range []string{"version", "duplicate", "unknown", "missing_credential", "different_identity", "oversize", "symlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			s, p := testDiskStore(t)
			ctx := context.Background()
			if err := s.Save(ctx, storedIntent(), Secret("synthetic")); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(p.RemoteDir, "intent.json")
			data, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "version":
				data = []byte(strings.Replace(string(data), `"version":1`, `"version":9`, 1))
			case "duplicate":
				data = append([]byte(`{"version":1,`), data[1:]...)
			case "unknown":
				data = append([]byte(`{"alien":1,`), data[1:]...)
			case "oversize":
				data = make([]byte, maxIntentBytes+1)
			case "different_identity":
				data = []byte(strings.Replace(string(data), "saved.ngrok-free.app", "other.ngrok-free.app", 1))
			case "missing_credential":
				entries, _ := os.ReadDir(p.RemoteDir)
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), "credential-") {
						os.Remove(filepath.Join(p.RemoteDir, e.Name()))
					}
				}
			case "symlink":
				os.Remove(name)
				if err := os.Symlink(filepath.Join(p.Root, "outside"), name); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(name, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "missing_credential" && kind != "symlink" && kind != "permissions" {
				if err := os.WriteFile(name, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := s.Load(ctx); !errors.Is(err, ErrState) {
				t.Fatalf("not fixed fail-closed error: %v", err)
			}
			if _, err := appdata.Prepare(p.Root); err != nil {
				t.Fatalf("remote state prevented local startup: %v", err)
			}
		})
	}
}
func TestStoreIsolationRemoveAndBoundedFiles(t *testing.T) {
	s, p := testDiskStore(t)
	other, _ := testDiskStore(t)
	ctx := context.Background()
	for n := 0; n < 5; n++ {
		if err := s.Save(ctx, storedIntent(), Secret("synthetic")); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(p.RemoteDir)
	if len(entries) != 2 {
		t.Fatalf("orphan files: %d", len(entries))
	}
	got, key, err := other.Load(ctx)
	if err != nil || got.Enabled || len(key) != 0 {
		t.Fatal("real/demo roots shared state")
	}
	if err := s.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	got, key, err = s.Load(ctx)
	if err != nil || got.Enabled || len(key) != 0 {
		t.Fatal("state remained")
	}
}

func TestStoreSaveCannotRepairBrokenCredential(t *testing.T) {
	s, p := testDiskStore(t)
	ctx := context.Background()
	if err := s.Save(ctx, storedIntent(), Secret("original")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(p.RemoteDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "credential-") {
			if err := os.Remove(filepath.Join(p.RemoteDir, entry.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.Save(ctx, storedIntent(), Secret("replacement")); !errors.Is(err, ErrState) {
		t.Fatalf("Save silently repaired incomplete state: %v", err)
	}
}
func TestUncertainManagerSaveRetainsCommittedIdentity(t *testing.T) {
	s, _ := testDiskStore(t)
	a := &testAdapter{calls: make(chan openCall, 2)}
	m, err := newManager(ManagerConfig{Store: s, Adapters: map[Provider]Adapter{TailscalePrivate: a}, Prepare: basicHandlers}, &testClock{requests: make(chan clockRequest, 2)})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(context.Background())
	configure(t, m)
	call := recv(t, a.calls)
	s.checkpoint = func(stage string) error {
		if stage == "after_commit" {
			return errors.New("synthetic sync uncertainty")
		}
		return nil
	}
	session := newTestSession(t)
	call.reply <- openResult{session: session}
	until(t, func() bool { return m.Status().Code == ErrState.Error() && session.closes.Load() == 1 })
	s.checkpoint = nil
	replacement := managerIntent()
	replacement.SavedOrigin = "https://replacement.ngrok-free.app"
	if err := m.Configure(context.Background(), replacement, Secret("new")); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("committed authority was replaceable: %v", err)
	}
	if m.Status().SavedOrigin != session.origin {
		t.Fatal("manager forgot committed origin")
	}
}
