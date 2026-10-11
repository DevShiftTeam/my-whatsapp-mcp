//go:build darwin || linux

package appdata

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWithClientsUsesPrivateDirectoryAndClosesBorrowedHandle(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var borrowed *os.Root
	if err := WithClients(paths, lease, func(root *os.Root) error {
		borrowed = root
		file, err := root.OpenFile("credential", os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag(), 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		if _, err := file.WriteString("synthetic private credential"); err != nil {
			return err
		}
		return file.Sync()
	}); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(paths.ClientDir, "credential"), 0600)
	if _, err := borrowed.Open("credential"); err == nil {
		t.Fatal("client handle remained usable outside callback")
	}
	if err := WithClients(paths, lease, func(root *os.Root) error {
		content, err := root.ReadFile("credential")
		if err != nil {
			return err
		}
		if string(content) != "synthetic private credential" {
			return errors.New("wrong synthetic credential")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := WithClients(paths, lease, func(root *os.Root) error {
		return root.WriteFile("../escaped-credential", []byte("synthetic denied credential"), 0600)
	}); !errors.Is(err, ErrDataUnavailable) {
		t.Fatalf("escaped callback fixed error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(paths.Root, "escaped-credential")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("client callback escaped its directory")
	}
}

func TestWithClientsResetRetainsLiveLeaseAndClearsCredentials(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := WithClients(paths, lease, func(root *os.Root) error {
		return root.WriteFile("credential", []byte("synthetic before reset"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	if err := ResetOwned(paths, lease); err != nil {
		t.Fatal(err)
	}
	if err := WithClients(paths, lease, func(root *os.Root) error {
		if _, err := root.Lstat("credential"); !errors.Is(err, os.ErrNotExist) {
			return errors.New("reset credential remains")
		}
		return root.WriteFile("new-credential", []byte("synthetic after reset"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(paths.ClientDir, "new-credential"), 0600)
	if _, err := Acquire(paths); !errors.Is(err, ErrInstanceAlreadyRunning) {
		t.Fatalf("reset lost live lease = %v", err)
	}
}

func TestWithClientsRejectsUnmatchedOrClosedLease(t *testing.T) {
	paths := syntheticPaths(t)
	called := false
	use := func(root *os.Root) error { called = true; return nil }
	if err := WithClients(paths, nil, use); !errors.Is(err, ErrLockRequired) {
		t.Fatalf("nil lease = %v", err)
	}
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	other := syntheticPaths(t)
	if err := WithClients(other, lease, use); !errors.Is(err, ErrLockRequired) {
		t.Fatalf("unmatched root = %v", err)
	}
	changed := paths
	changed.ClientDir = other.ClientDir
	if err := WithClients(changed, lease, use); !errors.Is(err, ErrLockRequired) {
		t.Fatalf("changed client path = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := WithClients(paths, lease, use); !errors.Is(err, ErrLockRequired) {
		t.Fatalf("closed lease = %v", err)
	}
	if called {
		t.Fatal("client callback ran without a live matching lease")
	}
}

func TestWithClientsRejectsReplacedRootAndSymlinks(t *testing.T) {
	for _, kind := range []string{"root-replaced", "directory-symlink", "file-symlink"} {
		t.Run(kind, func(t *testing.T) {
			paths := syntheticPaths(t)
			lease, err := Acquire(paths)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			outside := syntheticParent(t)
			outsideFile := filepath.Join(outside, "credential")
			if err := os.WriteFile(outsideFile, []byte("unrelated synthetic data"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "root-replaced":
				if err := os.Rename(paths.Root, paths.Root+"-original"); err != nil {
					t.Fatal(err)
				}
				replacement, err := Prepare(paths.Root)
				if err != nil {
					t.Fatal(err)
				}
				replacementLease, err := Acquire(replacement)
				if err != nil {
					t.Fatal(err)
				}
				defer replacementLease.Close()
			case "directory-symlink":
				if err := os.Remove(paths.ClientDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, paths.ClientDir); err != nil {
					t.Fatal(err)
				}
			case "file-symlink":
				if err := os.Symlink(outsideFile, filepath.Join(paths.ClientDir, "credential")); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			if err := WithClients(paths, lease, func(root *os.Root) error { called = true; return nil }); !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("unsafe clients fixed error = %v", err)
			}
			if called {
				t.Fatal("client callback ran against an unsafe root")
			}
			content, err := os.ReadFile(outsideFile)
			if err != nil || string(content) != "unrelated synthetic data" {
				t.Fatal("outside synthetic credential changed")
			}
		})
	}
}

type foreignUIDFileInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (info foreignUIDFileInfo) Sys() any { return &info.stat }

// A non-root test process cannot assign another UID to a filesystem fixture.
// Exercise the production ownership guards with real stat metadata except UID.
func TestPrivateClientObjectsRejectAnotherUID(t *testing.T) {
	paths := syntheticPaths(t)
	credential := filepath.Join(paths.ClientDir, "credential")
	if err := os.WriteFile(credential, []byte("synthetic credential"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.ClientDir, credential} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat := *info.Sys().(*syscall.Stat_t)
		stat.Uid ^= 1
		foreign := foreignUIDFileInfo{FileInfo: info, stat: stat}
		if privateDirectory(foreign) || privateFile(foreign) {
			t.Fatal("private client object admitted another UID")
		}
	}
}
