//go:build darwin || linux

package appdata

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResetDeletesOnlyOwnedDataAndKeepsHeldLock(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	lockBefore, err := os.Stat(filepath.Join(paths.Root, ".instance.lock"))
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(paths.Root), "outside.txt")
	unrelated := filepath.Join(paths.Root, "unrelated.txt")
	for _, path := range []string{outside, unrelated} {
		if err := os.WriteFile(path, []byte("preserve me"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, child := range []string{paths.ArchiveDir, paths.SessionDir, paths.MediaDir, paths.WorkingDir, paths.ModelDir, paths.ClientDir} {
		nested := filepath.Join(child, "nested")
		if err := os.Mkdir(nested, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(nested, "private-data"), []byte("synthetic only"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(paths.OwnerURLFile, []byte("private bootstrap material"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ResetOwned(paths, lease); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, unrelated} {
		content, err := os.ReadFile(path)
		if err != nil || string(content) != "preserve me" {
			t.Fatal("unrelated file was removed or changed")
		}
	}
	for _, child := range []string{paths.ArchiveDir, paths.SessionDir, paths.MediaDir, paths.WorkingDir, paths.ModelDir, paths.ClientDir} {
		assertMode(t, child, 0700)
		entries, err := os.ReadDir(child)
		if err != nil || len(entries) != 0 {
			t.Fatal("owned data remains")
		}
	}
	if _, err := os.Lstat(paths.OwnerURLFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("bootstrap material remains")
	}
	lockAfter, err := os.Stat(filepath.Join(paths.Root, ".instance.lock"))
	if err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("reset replaced the lock inode")
	}
	if _, err := Acquire(paths); !errors.Is(err, ErrInstanceAlreadyRunning) {
		t.Fatalf("reset released lock: %v", err)
	}
	if _, err := Prepare(paths.Root); err != nil {
		t.Fatalf("owned root was not retained: %v", err)
	}
}

func TestResetRequiresMatchingLiveLease(t *testing.T) {
	paths := syntheticPaths(t)
	if err := ResetOwned(paths, nil); !errors.Is(err, ErrLockRequired) {
		t.Fatalf("nil lease = %v", err)
	}
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	other := syntheticPaths(t)
	if err := ResetOwned(other, lease); !errors.Is(err, ErrLockRequired) {
		t.Fatalf("different root = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ResetOwned(paths, lease); !errors.Is(err, ErrLockRequired) {
		t.Fatalf("closed lease = %v", err)
	}
}

func TestResetValidatesAllOwnedPathsBeforeDeleting(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	body := filepath.Join(paths.ArchiveDir, "synthetic-content")
	if err := os.WriteFile(body, []byte("must survive refusal"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(syntheticParent(t), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(paths.MediaDir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := ResetOwned(paths, lease); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("error = %v", err)
	}
	content, err := os.ReadFile(body)
	if err != nil || string(content) != "must survive refusal" {
		t.Fatal("partial reset preceded validation")
	}
	content, err = os.ReadFile(outside)
	if err != nil || string(content) != "outside" {
		t.Fatal("symlink target changed")
	}
}

func TestResetRechecksOwnershipAndRootIdentity(t *testing.T) {
	for _, kind := range []string{"marker", "root-replaced", "lock-replaced"} {
		t.Run(kind, func(t *testing.T) {
			paths := syntheticPaths(t)
			lease, err := Acquire(paths)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			switch kind {
			case "marker":
				if err := os.WriteFile(filepath.Join(paths.Root, ".wa-indexer-owner"), []byte("unowned"), 0600); err != nil {
					t.Fatal(err)
				}
			case "root-replaced":
				if err := os.Rename(paths.Root, paths.Root+"-moved"); err != nil {
					t.Fatal(err)
				}
				if _, err := Prepare(paths.Root); err != nil {
					t.Fatal(err)
				}
			case "lock-replaced":
				if err := os.Remove(filepath.Join(paths.Root, ".instance.lock")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(paths.Root, ".instance.lock"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ResetOwned(paths, lease); !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
