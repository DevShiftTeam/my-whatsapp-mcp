//go:build darwin || linux

package appdata

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestRemoteAccessPrivateAndLocalIsolation(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if paths.RemoteDir != filepath.Join(paths.Root, "remote") {
		t.Fatal("wrong remote root")
	}
	var borrowed *os.Root
	if err := WithRemoteAccess(paths, lease, func(root *os.Root) error { borrowed = root; return root.WriteFile("state", []byte("synthetic"), 0600) }); err != nil {
		t.Fatal(err)
	}
	if _, err := borrowed.Open("state"); err == nil {
		t.Fatal("root escaped callback")
	}
	if err := os.Chmod(filepath.Join(paths.RemoteDir, "state"), 0644); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := WithRemoteAccess(paths, lease, func(*os.Root) error { called = true; return nil }); !errors.Is(err, ErrUnsafeDataPath) || called {
		t.Fatalf("unsafe remote allowed: %v", err)
	}
	if _, err := Prepare(paths.Root); err != nil {
		t.Fatalf("remote state blocked archive startup: %v", err)
	}
	if err := WithClients(paths, lease, func(*os.Root) error { return nil }); err != nil {
		t.Fatalf("remote state blocked archive operation: %v", err)
	}
	if err := ResetOwned(paths, lease); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatal("unsafe reset allowed")
	}
}
func TestRemoteAccessResetAndLease(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := WithRemoteAccess(paths, nil, func(*os.Root) error { return nil }); !errors.Is(err, ErrLockRequired) {
		t.Fatal(err)
	}
	if err := WithRemoteAccess(paths, lease, func(root *os.Root) error { return root.WriteFile("credential", []byte("synthetic"), 0600) }); err != nil {
		t.Fatal(err)
	}
	if err := ResetOwned(paths, lease); err != nil {
		t.Fatal(err)
	}
	if err := WithRemoteAccess(paths, lease, func(root *os.Root) error {
		_, err := root.Lstat("credential")
		if !errors.Is(err, os.ErrNotExist) {
			return ErrDataUnavailable
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestRemoteAccessRejectsSymlink(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	outside := filepath.Join(syntheticParent(t), "secret")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(paths.RemoteDir, "credential")); err != nil {
		t.Fatal(err)
	}
	if err := WithRemoteAccess(paths, lease, func(*os.Root) error { t.Fatal("symlink admitted"); return nil }); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatal(err)
	}
}

func TestRemoteAccessRejectsForeignOwnership(t *testing.T) {
	paths := syntheticPaths(t)
	name := filepath.Join(paths.RemoteDir, "synthetic")
	if err := os.WriteFile(name, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.RemoteDir, name} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat := *info.Sys().(*syscall.Stat_t)
		stat.Uid ^= 1
		foreign := foreignUIDFileInfo{FileInfo: info, stat: stat}
		if privateDirectory(foreign) || privateFile(foreign) {
			t.Fatal("foreign remote ownership accepted")
		}
	}
}
func TestRemoteAccessRejectsACL(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS ACL fixture")
	}
	for _, directory := range []bool{false, true} {
		paths := syntheticPaths(t)
		lease, err := Acquire(paths)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		name := paths.RemoteDir
		if !directory {
			name = filepath.Join(paths.RemoteDir, "synthetic")
			if err := os.WriteFile(name, []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := exec.Command("/bin/chmod", "+a", "everyone allow read,write,execute", name).Run(); err != nil {
			t.Fatal(err)
		}
		if err := WithRemoteAccess(paths, lease, func(*os.Root) error { t.Fatal("ACL admitted"); return nil }); !errors.Is(err, ErrUnsafeDataPath) {
			t.Fatal(err)
		}
		if _, err := Prepare(paths.Root); err != nil {
			t.Fatal("remote ACL blocked local startup")
		}
	}
}

func TestResetOlderRootWithoutRemoteDirectory(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := os.Remove(paths.RemoteDir); err != nil {
		t.Fatal(err)
	}
	if err := ResetOwned(paths, lease); err != nil {
		t.Fatal(err)
	}
	if err := WithRemoteAccess(paths, lease, func(*os.Root) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestExistingPathsLeavesLegacyOptionalRemoteDirectoryAbsent(t *testing.T) {
	paths := syntheticPaths(t)
	if err := os.Remove(paths.RemoteDir); err != nil {
		t.Fatal(err)
	}
	existing, err := ExistingPaths(paths.Root)
	if err != nil || existing != paths {
		t.Fatalf("legacy paths changed: %v", err)
	}
	if _, err := os.Lstat(paths.RemoteDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only path inspection created remote state: %v", err)
	}
	if _, err := Prepare(paths.Root); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(paths.RemoteDir)
	if err != nil || !privateDirectory(info) {
		t.Fatalf("prepare did not add private remote directory: %v", err)
	}
}

func TestUnsafeOptionalRemoteRootNeverRepairsOrBlocksLocalArchive(t *testing.T) {
	paths := syntheticPaths(t)
	if err := os.Remove(paths.RemoteDir); err != nil {
		t.Fatal(err)
	}
	outside := syntheticParent(t)
	if err := os.Symlink(outside, paths.RemoteDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(paths.Root); err != nil {
		t.Fatalf("optional remote root blocked local prepare: %v", err)
	}
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := WithWorking(paths, lease, func(*os.Root) error { return nil }); err != nil {
		t.Fatalf("optional remote root blocked local working state: %v", err)
	}
	if err := WithRemoteAccess(paths, lease, func(*os.Root) error { t.Fatal("unsafe remote root admitted"); return nil }); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("wrong remote error: %v", err)
	}
	if target, err := os.Readlink(paths.RemoteDir); err != nil || target != outside {
		t.Fatal("optional path was repaired or replaced")
	}
}
