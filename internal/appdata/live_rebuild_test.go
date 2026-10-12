package appdata

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireExistingPreservesSourceAndRequiresStoppedOwner(t *testing.T) {
	paths := syntheticPaths(t)
	if _, err := AcquireExisting(paths); err == nil {
		t.Fatal("created missing source lock")
	}
	owner, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireExisting(paths); !errors.Is(err, ErrInstanceAlreadyRunning) {
		t.Fatalf("running owner accepted: %v", err)
	}
	owner.Close()
	before, _ := os.Stat(filepath.Join(paths.Root, instanceLockFile))
	held, err := AcquireExisting(paths)
	if err != nil {
		t.Fatal(err)
	}
	held.Close()
	after, _ := os.Stat(filepath.Join(paths.Root, instanceLockFile))
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("source lock changed")
	}
}

func TestLiveRebuildSourceGuardFailsClosedForAnyEntry(t *testing.T) {
	paths := syntheticPaths(t)
	if err := RefuseLiveRebuildSource(paths.Root); err != nil {
		t.Fatal(err)
	}
	guard := LiveRebuildSourceGuardPath(paths.Root)
	if err := os.Symlink("missing", guard); err != nil {
		t.Fatal(err)
	}
	if err := RefuseLiveRebuildSource(paths.Root); !errors.Is(err, ErrLiveRebuildSource) {
		t.Fatalf("guard ignored: %v", err)
	}
}

func TestAcquireInheritedExistingRetainsSharedAuthorityWithoutClosingCaller(t *testing.T) {
	paths := syntheticPaths(t)
	original, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	fd, err := original.WorkerGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	adopted, err := AcquireInheritedExisting(paths, int(fd.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := adopted.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fd.Stat(); err != nil {
		t.Fatal("closed supplied source guard")
	}
	if l, err := AcquireExisting(paths); err == nil {
		l.Close()
		t.Fatal("released original shared lock")
	}
	other := syntheticPaths(t)
	otherLease, err := Acquire(other)
	if err != nil {
		t.Fatal(err)
	}
	defer otherLease.Close()
	if l, err := AcquireInheritedExisting(other, int(fd.Fd())); err == nil {
		l.Close()
		t.Fatal("accepted unrelated lock inode")
	}
}
