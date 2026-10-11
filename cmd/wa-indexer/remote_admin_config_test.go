package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteAdminOptionsRequireCompleteCanonicalRoute(t *testing.T) {
	valid := []string{"serve", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "https://archive.example.com", "--no-open"}
	if _, err := parseArgs(valid); err != nil {
		t.Fatal("canonical admin route refused")
	}
	for _, args := range [][]string{
		{"serve", "--admin-listen", "127.0.0.1:6969"},
		{"serve", "--admin-public-url", "https://archive.example.com"},
		{"serve", "--admin-env-file", "/private/admin.env"},
		{"serve", "--admin-listen", "127.0.0.1:0", "--admin-public-url", "https://archive.example.com"},
		{"serve", "--admin-listen", "localhost:6969", "--admin-public-url", "https://archive.example.com"},
		{"serve", "--admin-listen", "0.0.0.0:6969", "--admin-public-url", "https://archive.example.com"},
		{"serve", "--admin-listen", "127.0.0.1:06969", "--admin-public-url", "https://archive.example.com"},
		{"serve", "--admin-listen", "127.0.0.1:64165", "--admin-public-url", "https://archive.example.com"},
		{"serve", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "http://archive.example.com"},
		{"serve", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "https://archive.example.com/"},
		{"serve", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "https://Archive.example.com"},
		{"serve", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "https://127.0.0.1"},
		{"serve", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "https://user:PRIVATE@archive.example.com"},
		{"serve", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "https://archive.example.com?password=PRIVATE"},
		{"serve", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "https://archive.example.com", "--admin-env-file", "relative.env"},
		{"serve", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "https://archive.example.com", "--mcp-listen", "127.0.0.1:6969", "--mcp-public-url", "https://archive.example.com/mcp"},
		{"serve", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "https://archive.example.com", "--mcp-listen", "127.0.0.1:6968", "--mcp-public-url", "https://other.example.com/mcp"},
		{"doctor", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "https://archive.example.com"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("unsafe or incomplete admin route accepted")
		}
	}
}

func TestRemoteAdminMissingHashFailsBeforeOpeningProfile(t *testing.T) {
	t.Setenv("WA_INDEXER_ADMIN_PASSWORD_HASH", "")
	root := filepath.Join(t.TempDir(), "private-not-created")
	var output, diagnostic bytes.Buffer
	code := run(context.Background(), []string{"serve", "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "https://archive.example.com", "--data-dir", root, "--demo", "--no-open"}, &output, &diagnostic)
	if code != 1 {
		t.Fatal("missing backend password did not fail at startup")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing backend password touched the archive")
	}
	if output.Len() != 0 || strings.Contains(diagnostic.String(), root) {
		t.Fatal("admin startup leaked a private value")
	}
}

func validCLIAdminHash() string {
	return "$argon2id$v=19$m=19456,t=2,p=1$" + base64.RawStdEncoding.EncodeToString(make([]byte, 16)) + "$" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))
}

func privateAdminTestEnv(t *testing.T, body string) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("could not prepare canonical private test directory")
	}
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal("could not make private test directory")
	}
	path := filepath.Join(directory, "admin.env")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal("could not prepare private test env")
	}
	return path
}

func TestRemoteAdminHashFileStrictDataAndAmbientConflict(t *testing.T) {
	hash := validCLIAdminHash()
	path := privateAdminTestEnv(t, "# Private config\n"+adminPasswordHashEnv+"="+hash+"\n")
	opts := options{listen: "127.0.0.1:0", adminListen: "127.0.0.1:6969", adminPublicURL: "https://archive.example.com", adminEnvFile: path}
	t.Setenv(adminPasswordHashEnv, "")
	cfg, err := remoteAdminLaunchConfig(opts)
	if err != nil || cfg == nil || cfg.PasswordHash != hash {
		t.Fatal("private env data was not accepted")
	}
	t.Setenv(adminPasswordHashEnv, hash)
	if _, err := remoteAdminLaunchConfig(opts); err != nil {
		t.Fatal("identical env and private file conflict")
	}
	t.Setenv(adminPasswordHashEnv, "PRIVATE_AMBIENT_SENTINEL")
	if _, err := remoteAdminLaunchConfig(opts); !errors.Is(err, errAdminConfig) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("ambient conflict leaked or was accepted")
	}
	t.Setenv(adminPasswordHashEnv, "")
	for _, body := range []string{
		"", "WA_INDEXER_ADMIN_PASSWORD=PRIVATE\n", "export " + adminPasswordHashEnv + "=" + hash + "\n",
		adminPasswordHashEnv + "='" + hash + "'\n", adminPasswordHashEnv + "=" + hash + "\n" + adminPasswordHashEnv + "=" + hash + "\n",
		adminPasswordHashEnv + "=" + hash + "\nUNKNOWN=PRIVATE\n",
		adminPasswordHashEnv + "=" + hash + "\x00\n", strings.Repeat("A", maxAdminEnvBytes+1),
		adminPasswordHashEnv + "=$(PRIVATE_SHELL_SENTINEL)\n",
	} {
		invalid := opts
		invalid.adminEnvFile = privateAdminTestEnv(t, body)
		if _, err := remoteAdminLaunchConfig(invalid); !errors.Is(err, errAdminConfig) {
			t.Fatal("malformed env data was accepted")
		}
	}
}

func TestRemoteAdminHashFileRefusesUnsafeEntries(t *testing.T) {
	hash := validCLIAdminHash()
	t.Setenv(adminPasswordHashEnv, "")
	for _, mode := range []string{"public-file", "public-parent", "leaf-symlink", "hardlink", "parent-symlink"} {
		t.Run(mode, func(t *testing.T) {
			path := privateAdminTestEnv(t, adminPasswordHashEnv+"="+hash+"\n")
			switch mode {
			case "public-file":
				if os.Chmod(path, 0644) != nil {
					t.Fatal("could not prepare file permissions")
				}
			case "public-parent":
				if os.Chmod(filepath.Dir(path), 0755) != nil {
					t.Fatal("could not prepare directory permissions")
				}
			case "leaf-symlink":
				renamed := path + ".real"
				if os.Rename(path, renamed) != nil || os.Symlink(renamed, path) != nil {
					t.Fatal("could not prepare symlink")
				}
			case "hardlink":
				if os.Link(path, path+".alias") != nil {
					t.Fatal("could not prepare hardlink")
				}
			case "parent-symlink":
				alias := filepath.Join(t.TempDir(), "alias")
				if os.Symlink(filepath.Dir(path), alias) != nil {
					t.Fatal("could not prepare parent symlink")
				}
				path = filepath.Join(alias, filepath.Base(path))
			}
			if _, err := readAdminEnvFile(path); !errors.Is(err, errAdminConfig) {
				t.Fatal("unsafe private env entry was read")
			}
		})
	}
}

func TestRemoteAdminInvalidHashFailsBeforeProfileOpen(t *testing.T) {
	const sentinel = "PRIVATE_HASH_SENTINEL"
	t.Setenv(adminPasswordHashEnv, sentinel)
	root := filepath.Join(t.TempDir(), "private-not-created")
	var output, diagnostic bytes.Buffer
	code := run(context.Background(), []string{"serve", "--listen", "127.0.0.1:0", "--admin-listen", "127.0.0.1:6969", "--admin-public-url", "https://archive.example.com", "--data-dir", root, "--demo", "--no-open"}, &output, &diagnostic)
	if code != 1 {
		t.Fatal("invalid backend password did not fail at startup")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid backend password touched the archive")
	}
	if output.Len() != 0 || strings.Contains(diagnostic.String(), sentinel) || strings.Contains(diagnostic.String(), root) {
		t.Fatal("admin startup leaked private input")
	}
}

func TestRemoteAdminOccupiedListenerFailsBeforeProfileOpen(t *testing.T) {
	t.Setenv(adminPasswordHashEnv, validCLIAdminHash())
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	root := filepath.Join(t.TempDir(), "private-not-created")
	var output, diagnostic bytes.Buffer
	err = serve(context.Background(), options{listen: "127.0.0.1:0", adminListen: occupied.Addr().String(), adminPublicURL: "https://archive.example.com", dataDir: root, demo: true, noOpen: true}, &output, &diagnostic)
	if !errors.Is(err, errStartup) {
		t.Fatal("occupied second listener was not refused")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("second listener bind failure opened application data")
	}
	if output.Len() != 0 || diagnostic.Len() != 0 {
		t.Fatal("second listener failure exposed launch data")
	}
}

func TestRemoteAdminConfiguredLaunchRequiresRestartInsteadOfAttach(t *testing.T) {
	t.Setenv(adminPasswordHashEnv, validCLIAdminHash())
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	root := filepath.Join(t.TempDir(), "private-not-created")
	var output, diagnostic bytes.Buffer
	err = serve(context.Background(), options{listen: occupied.Addr().String(), adminListen: "127.0.0.1:6969", adminPublicURL: "https://archive.example.com", dataDir: root, noOpen: true}, &output, &diagnostic)
	if !errors.Is(err, errRestartRequired) {
		t.Fatal("admin launch silently attached without applying configuration")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("occupied local listener touched the profile")
	}
}

type joinedOwnerFixture struct {
	started  chan struct{}
	canceled chan struct{}
	finish   chan struct{}
	outcome  error
}

func (f *joinedOwnerFixture) Serve(ctx context.Context, _ net.Listener) error {
	close(f.started)
	select {
	case <-f.finish:
	case <-ctx.Done():
		close(f.canceled)
		<-f.finish
	}
	return f.outcome
}

func TestRemoteAdminOwnerInterfacesJoinOtherShutdownAfterFailure(t *testing.T) {
	first := &joinedOwnerFixture{started: make(chan struct{}), canceled: make(chan struct{}), finish: make(chan struct{}), outcome: errStartup}
	second := &joinedOwnerFixture{started: make(chan struct{}), canceled: make(chan struct{}), finish: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- serveOwnerInterfaces(context.Background(), []ownerInterfaceBinding{{server: first}, {server: second}})
	}()
	<-first.started
	<-second.started
	close(first.finish)
	select {
	case <-second.canceled:
	case <-time.After(time.Second):
		t.Fatal("a failing listener did not cancel its peer")
	}
	select {
	case <-done:
		t.Fatal("composition released resources before other shutdown joined")
	default:
	}
	close(second.finish)
	select {
	case err := <-done:
		if !errors.Is(err, errStartup) {
			t.Fatal("interface failure was lost after joined shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("composition did not finish after both interfaces joined")
	}
}
