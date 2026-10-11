package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"golang.org/x/sys/unix"
)

func retainedGuardOriginal(t *testing.T) (*Store, *fakeRepository, *Original) {
	t.Helper()
	f := newRepository()
	s, _, _ := testStore(t, f, Options{})
	data := pngData(t)
	q := request(f, data)
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.File().Write(data); err != nil {
		t.Fatal(err)
	}
	if err := d.Complete(context.Background(), receipt(q, data)); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := s.OpenOriginal(context.Background(), archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: 1}, q.MediaID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = original.Close() })
	f.processed = true
	return s, f, original
}

// Parent Close must not LOCK_UN the shared OFDs held by the independent worker
// holder. Both global eviction exclusion and per-artifact cleanup stay live.
func TestOriginalWorkerGuardsSurviveCloseUntilLastDuplicate(t *testing.T) {
	s, f, original := retainedGuardOriginal(t)
	first, err := original.WorkerGuards()
	if err != nil {
		t.Fatal(err)
	}
	defer closeGuardFiles(first)
	second, err := original.WorkerGuards()
	if err != nil {
		t.Fatal(err)
	}
	defer closeGuardFiles(second)
	if len(first) != 2 || len(second) != 2 {
		t.Fatal("both artifact and reader guards must be exported")
	}
	for i, guard := range first {
		flags, err := unix.FcntlInt(guard.Fd(), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Fatalf("worker guard inheritance must be explicit: flags=%d err=%v", flags, err)
		}
		status, err := unix.FcntlInt(guard.Fd(), unix.F_GETFL, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := unix.FcntlInt(guard.Fd(), unix.F_SETFL, status&^unix.O_NONBLOCK); err != nil {
			t.Fatal(err)
		}
		parents := []*os.File{original.guard, original.readers}
		for _, held := range []*os.File{parents[i], second[i]} {
			status, err := unix.FcntlInt(held.Fd(), unix.F_GETFL, 0)
			if err != nil || status&unix.O_NONBLOCK != 0 {
				t.Fatalf("worker guard reopened instead of duplicating the held OFD: flags=%d err=%v", status, err)
			}
		}
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if guards, err := original.WorkerGuards(); !errors.Is(err, os.ErrClosed) {
		closeGuardFiles(guards)
		t.Fatal("closed Original exported new guards", err)
	}
	if err := s.Evict(context.Background(), "media-id"); !errors.Is(err, ErrActiveUse) {
		t.Fatalf("Original close released exported reader guards: %v", err)
	}
	if f.charge(archive.BudgetOriginals) == 0 {
		t.Fatal("active worker's quota was released")
	}
	closeGuardFiles(first)
	if err := s.Evict(context.Background(), "media-id"); !errors.Is(err, ErrActiveUse) {
		t.Fatalf("first duplicate set close released final holder: %v", err)
	}
	guard, err := artifactLock(context.Background(), s.media, artifactName(original.Metadata().ArtifactID, ".lock"), true, false)
	if !errors.Is(err, ErrActiveUse) {
		_ = releaseLock(guard)
		t.Fatalf("exported artifact guard did not block physical cleanup: %v", err)
	}
	closeGuardFiles(second)
	if err := s.Evict(context.Background(), "media-id"); err != nil {
		t.Fatalf("last guard did not release cleanup authority: %v", err)
	}
	if f.charge(archive.BudgetOriginals) != 0 {
		t.Fatal("completed cleanup retained its charge")
	}
}

func TestOriginalWorkerGuardsRejectReplacedArtifactAnchor(t *testing.T) {
	s, _, original := retainedGuardOriginal(t)
	path := filepath.Join(s.media.path, artifactName(original.Metadata().ArtifactID, ".lock"))
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if guards, err := original.WorkerGuards(); !errors.Is(err, ErrUnsafeFile) {
		closeGuardFiles(guards)
		t.Fatalf("exported detached artifact lock authority: %v", err)
	}
}

func closeGuardFiles(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

func TestDecodedWorkerGuardSealedSameOFDAndLastDuplicate(t *testing.T) {
	repository := newRepository()
	s, paths, physical := testStore(t, repository, Options{})
	d, err := s.ReserveDecoded(context.Background(), repository.source, "media-id", 64)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if guard, err := d.WorkerGuard(); !errors.Is(err, ErrClosed) {
		if guard != nil {
			_ = guard.Close()
		}
		t.Fatal("unsealed decoded extent exported a worker guard", err)
	}
	if _, err := d.File().Write([]byte("decoded-input")); err != nil {
		t.Fatal(err)
	}
	if err := d.Seal(context.Background()); err != nil {
		t.Fatal(err)
	}
	guard, err := d.WorkerGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	flags, err := unix.FcntlInt(guard.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("decoded inheritance must be explicit: flags=%d err=%v", flags, err)
	}
	status, err := unix.FcntlInt(guard.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(guard.Fd(), unix.F_SETFL, status&^unix.O_NONBLOCK); err != nil {
		t.Fatal(err)
	}
	status, err = unix.FcntlInt(d.guard.Fd(), unix.F_GETFL, 0)
	if err != nil || status&unix.O_NONBLOCK != 0 {
		t.Fatalf("decoded export reopened its guard: flags=%d err=%v", status, err)
	}
	// Simulate the independent holder's inherited duplicate, which is not the
	// caller-owned *os.File tracked by Decoded. Closing local copies is not proof
	// that the native group and independent holder have finished.
	fd, err := unix.FcntlInt(guard.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	holder := os.NewFile(uintptr(fd), "decoded-independent-holder")
	defer holder.Close()
	if err := d.Close(); !errors.Is(err, ErrActiveUse) {
		t.Fatal("decoded Close ignored its live exported descriptor", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); !errors.Is(err, ErrActiveUse) {
		t.Fatal("decoded Close removed input while external holder stayed alive", err)
	}
	if repository.charge(archive.BudgetDecoded) != 64 || physical.held == 0 {
		t.Fatal("active decoded holder lost accounting")
	}
	path := filepath.Join(paths.WorkingDir, artifactName(d.reservation.ArtifactID, ".decoded"))
	if _, err := os.Stat(path); err != nil {
		t.Fatal("active decoded input was removed", err)
	}
	if report, err := s.Reconcile(context.Background(), 256); err != nil || report.Active != 1 || report.Cleaned != 0 {
		t.Fatalf("reconcile bypassed independent decoded guard: %+v %v", report, err)
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal("last decoded duplicate did not permit cleanup", err)
	}
	if repository.charge(archive.BudgetDecoded) != 0 || physical.held != 0 {
		t.Fatal("finished decoded cleanup retained accounting")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("finished decoded input still present", err)
	}
}
