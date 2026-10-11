package modelstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func installedGuardStore(t *testing.T) *Store {
	t.Helper()
	s := testStore(t, catalogFor(component("model", []byte("abcdef"), "")), nil, Options{})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model.bin"), []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Import(context.Background(), "test-profile", dir); err != nil {
		t.Fatal(err)
	}
	return s
}

// An explicit LOCK_UN in Release would unlock every duplicate of the shared
// open-file description and let Remove destroy a still-running worker's model.
func TestWorkerGuardSurvivesReleaseUntilLastDuplicateCloses(t *testing.T) {
	s := installedGuardStore(t)
	l, err := s.Acquire(context.Background(), "test-profile")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	first, err := l.WorkerGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := l.WorkerGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	flags, err := unix.FcntlInt(first.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("worker guard inheritance must be explicit: flags=%d err=%v", flags, err)
	}
	// Status flags belong to the OFD. Reopening and re-flocking another file
	// descriptor would not update the already-held lease or its second copy.
	status, err := unix.FcntlInt(first.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(first.Fd(), unix.F_SETFL, status&^unix.O_NONBLOCK); err != nil {
		t.Fatal(err)
	}
	for _, held := range []*os.File{l.lock, second} {
		status, err := unix.FcntlInt(held.Fd(), unix.F_GETFL, 0)
		if err != nil || status&unix.O_NONBLOCK != 0 {
			t.Fatalf("worker guard reopened rather than duplicated the held OFD: flags=%d err=%v", status, err)
		}
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.WorkerGuard(); !errors.Is(err, ErrLeaseClosed) {
		t.Fatalf("released lease exported a guard: %v", err)
	}
	if err := s.Remove(context.Background(), "test-profile"); !errors.Is(err, ErrActiveLease) {
		t.Fatalf("parent release unlocked live worker guards: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(context.Background(), "test-profile"); !errors.Is(err, ErrActiveLease) {
		t.Fatalf("first duplicate close unlocked the remaining guard: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(context.Background(), "test-profile"); err != nil {
		t.Fatalf("last duplicate did not release the lease: %v", err)
	}
}

func TestWorkerGuardRejectsReplacedLeaseInode(t *testing.T) {
	s := installedGuardStore(t)
	l, err := s.Acquire(context.Background(), "test-profile")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	path := filepath.Join(s.leases.path, "test-profile.lease")
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if guard, err := l.WorkerGuard(); !errors.Is(err, ErrUnsafeFile) {
		if guard != nil {
			_ = guard.Close()
		}
		t.Fatalf("exported detached lock authority: %v", err)
	}
}

func TestWorkerGuardRetainsDirectoryAnchorAfterStoreCloses(t *testing.T) {
	s := installedGuardStore(t)
	l, err := s.Acquire(context.Background(), "test-profile")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	guard, err := l.WorkerGuard()
	if err != nil {
		t.Fatal("store close discarded the lease's directory anchor", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
}
