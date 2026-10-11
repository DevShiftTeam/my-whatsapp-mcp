package ingestion

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/types"
)

func canonicalSessionTestDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOpenSessionCreatesPrivateOfflineSession(t *testing.T) {
	dir := filepath.Join(canonicalSessionTestDir(t), "session space ? #")
	container, client, err := openSession(context.Background(), dir)
	if err != nil {
		t.Fatalf("open private session: %v", err)
	}
	t.Cleanup(func() { _ = container.Close() })
	if client.IsConnected() {
		t.Fatal("opening a session connected the client")
	}
	if client.EnableDecryptedEventBuffer || client.UseRetryMessageStore || client.EnableAutoReconnect {
		t.Fatal("opening enabled background reconnect or decrypted-message storage")
	}
	if !client.DisableLoginAutoReconnect {
		t.Fatal("opening allowed an automatic reconnect after login")
	}
	if !client.EmitAppStateEventsOnFullSync {
		t.Fatal("opening suppressed controls from full app-state synchronization")
	}
	if !client.ManualHistorySyncDownload || !client.DisableManualHistorySyncReceipt {
		t.Fatal("opening allowed automatic history download or receipt")
	}
	if client.Store.ID != nil {
		t.Fatal("opening a new session paired a device")
	}
	assertSessionMode(t, dir, 0700)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		assertSessionMode(t, filepath.Join(dir, "session.sqlite")+suffix, 0600)
	}
	address := url.URL{Scheme: "file", Path: filepath.Join(dir, "session.sqlite")}
	db, err := sql.Open("sqlite3", address.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("session journal mode = %q, error = %v", journal, err)
	}
}

func assertSessionMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("mode for %s = %v; want %v", filepath.Base(path), info.Mode(), want)
	}
}

func TestOpenSessionReopensExistingDevice(t *testing.T) {
	dir := filepath.Join(canonicalSessionTestDir(t), "session")
	container, client, err := openSession(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	jid := types.NewADJID("15550000001", 0, 1)
	client.Store.ID = &jid
	client.Store.Account = &waAdv.ADVSignedDeviceIdentity{
		Details: []byte{}, AccountSignature: make([]byte, 64),
		AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64),
	}
	if err := client.Store.Save(context.Background()); err != nil {
		_ = container.Close()
		t.Fatal(err)
	}
	if err := container.Close(); err != nil {
		t.Fatal(err)
	}
	container, client, err = openSession(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer container.Close()
	if client.Store.ID == nil || *client.Store.ID != jid {
		t.Fatal("session device was not preserved on reopen")
	}
	if client.IsConnected() || client.EnableDecryptedEventBuffer || client.UseRetryMessageStore || client.EnableAutoReconnect {
		t.Fatal("reopen connected or enabled a plaintext storage option")
	}
	if !client.DisableLoginAutoReconnect {
		t.Fatal("reopen allowed an automatic reconnect after login")
	}
	if !client.EmitAppStateEventsOnFullSync {
		t.Fatal("reopen suppressed controls from full app-state synchronization")
	}
}

func TestPrivateDirectoryRejectsUnsafePaths(t *testing.T) {
	base := canonicalSessionTestDir(t)
	for _, tc := range []struct {
		name string
		path func(t *testing.T) string
	}{
		{"empty", func(t *testing.T) string { return "" }},
		{"relative", func(t *testing.T) string { return "relative-session" }},
		{"unclean", func(t *testing.T) string { return base + "/../session" }},
		{"public final", func(t *testing.T) string {
			path := filepath.Join(base, "public")
			if err := os.Mkdir(path, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0755); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{"writable parent", func(t *testing.T) string {
			path := filepath.Join(base, "writable")
			if err := os.Mkdir(path, 0777); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0777); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(path, "session")
		}},
		{"symlink final", func(t *testing.T) string {
			path := filepath.Join(base, "final-link")
			if err := os.Symlink(base, path); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{"symlink parent", func(t *testing.T) string {
			path := filepath.Join(base, "parent-link")
			if err := os.Symlink(base, path); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(path, "session")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path(t)
			if err := privateDirectory(path, true); !errors.Is(err, archive.ErrUnsafeDataPath) {
				t.Fatalf("unsafe directory error = %v", err)
			}
		})
	}
}

func TestPrivateDirectoryAllowsOrdinaryAncestorWithoutChangingIt(t *testing.T) {
	base := canonicalSessionTestDir(t)
	ancestor := filepath.Join(base, "ordinary")
	if err := os.Mkdir(ancestor, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ancestor, 0755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(ancestor, "private", "temp")
	if err := privateDirectory(dir, true); err != nil {
		t.Fatal(err)
	}
	assertSessionMode(t, ancestor, 0755)
	assertSessionMode(t, filepath.Dir(dir), 0700)
	assertSessionMode(t, dir, 0700)
	if err := privateDirectory(dir, false); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(base, "not-created")
	if err := privateDirectory(missing, false); !errors.Is(err, archive.ErrUnsafeDataPath) {
		t.Fatalf("missing directory error = %v", err)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatal("validation created a directory")
	}
}

func TestOpenSessionRejectsUnsafeDatabaseAndSidecars(t *testing.T) {
	for _, name := range []string{"session.sqlite", "session.sqlite-wal", "session.sqlite-shm"} {
		for _, kind := range []string{"public", "symlink", "hardlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				dir := canonicalSessionTestDir(t)
				path := filepath.Join(dir, name)
				switch kind {
				case "public":
					if err := os.WriteFile(path, nil, 0644); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				case "symlink", "hardlink":
					target := filepath.Join(dir, "target")
					if err := os.WriteFile(target, nil, 0600); err != nil {
						t.Fatal(err)
					}
					var err error
					if kind == "symlink" {
						err = os.Symlink(target, path)
					} else {
						err = os.Link(target, path)
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				container, client, err := openSession(context.Background(), dir)
				if container != nil {
					_ = container.Close()
				}
				if !errors.Is(err, archive.ErrUnsafeDataPath) || client != nil || container != nil {
					t.Fatalf("unsafe file returned container=%v client=%v error=%v", container != nil, client != nil, err)
				}
			})
		}
	}
}

func grantingSessionACL(t *testing.T, path string, inherit bool) {
	t.Helper()
	grant := "everyone allow read,write,execute"
	if inherit {
		grant += ",file_inherit,directory_inherit"
	}
	if err := exec.Command("/bin/chmod", "+a", grant, path).Run(); err != nil {
		t.Fatal("could not create synthetic granting ACL")
	}
}

func TestPrivateDirectoryRejectsGrantingACL(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS granting ACLs can bypass Unix private mode bits")
	}
	dir := canonicalSessionTestDir(t)
	grantingSessionACL(t, dir, false)
	if err := privateDirectory(dir, false); !errors.Is(err, archive.ErrUnsafeDataPath) {
		t.Fatalf("granting ACL directory error = %v", err)
	}
}

func TestPrivateDirectoryRejectsInheritedGrantingACL(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS granting ACLs can bypass Unix private mode bits")
	}
	parent := canonicalSessionTestDir(t)
	grantingSessionACL(t, parent, true)
	dir := filepath.Join(parent, "inherited-session")
	if err := privateDirectory(dir, true); !errors.Is(err, archive.ErrUnsafeDataPath) {
		t.Fatalf("inherited ACL directory error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "session.sqlite")); !os.IsNotExist(err) {
		t.Fatal("an inherited ACL directory acquired a session database")
	}
}

func TestPrivateDirectoryRejectsGrantingACLAncestor(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS granting ACLs can bypass Unix mode bits")
	}
	for _, right := range []string{"write", "append", "delete", "delete_child", "writeattr", "writeextattr", "writesecurity", "chown"} {
		t.Run(right, func(t *testing.T) {
			parent := canonicalSessionTestDir(t)
			if err := os.Chmod(parent, 0755); err != nil {
				t.Fatal(err)
			}
			if err := exec.Command("/bin/chmod", "+a", "everyone allow "+right, parent).Run(); err != nil {
				t.Fatal("could not create a writable ancestor ACL")
			}
			dir := filepath.Join(parent, "private-session")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := privateDirectory(dir, false); !errors.Is(err, archive.ErrUnsafeDataPath) {
				t.Fatalf("granting ancestor ACL error = %v", err)
			}
		})
	}
}

func TestPrivateDirectoryAllowsDenyOnlyACLAncestor(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS home directories have standard deny-delete ACLs")
	}
	parent := canonicalSessionTestDir(t)
	if err := exec.Command("/bin/chmod", "+a", "everyone deny delete", parent).Run(); err != nil {
		t.Fatal("could not create a harmless ancestor ACL")
	}
	t.Cleanup(func() { _ = exec.Command("/bin/chmod", "-N", parent).Run() })
	dir := filepath.Join(parent, "private-session")
	if err := privateDirectory(dir, true); err != nil {
		t.Fatalf("harmless deny-only ancestor rejected: %v", err)
	}
	assertSessionMode(t, dir, 0700)
}

func TestOpenSessionRejectsGrantingACLDatabaseAndSidecars(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS granting ACLs can bypass Unix private mode bits")
	}
	for _, name := range []string{"session.sqlite", "session.sqlite-wal", "session.sqlite-shm"} {
		for _, inherited := range []bool{false, true} {
			kind := "direct"
			if inherited {
				kind = "inherited"
			}
			t.Run(name+"/"+kind, func(t *testing.T) {
				dir := canonicalSessionTestDir(t)
				path := filepath.Join(dir, name)
				if inherited {
					grantingSessionACL(t, dir, true)
				}
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
				if inherited {
					// Keep the inherited file ACL while removing the directory ACL,
					// so refusal must come from checking the sensitive file itself.
					if err := exec.Command("/bin/chmod", "-N", dir).Run(); err != nil {
						t.Fatal("could not remove the parent fixture ACL")
					}
				} else {
					grantingSessionACL(t, path, false)
				}
				container, client, err := openSession(context.Background(), dir)
				if container != nil {
					_ = container.Close()
				}
				if !errors.Is(err, archive.ErrUnsafeDataPath) || container != nil || client != nil {
					t.Fatalf("granting ACL file returned container=%v client=%v error=%v", container != nil, client != nil, err)
				}
			})
		}
	}
}
