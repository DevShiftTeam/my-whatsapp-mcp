package media

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Bodokh/wa-indexer/internal/appdata"
	"github.com/Bodokh/wa-indexer/internal/archive"
)

func retainTestOriginal(t *testing.T, s *Store, f *fakeRepository) DownloadRequest {
	t.Helper()
	data := pngData(t)
	q := request(f, data)
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.File().Write(data); err != nil {
		t.Fatal(err)
	}
	if err := d.Complete(context.Background(), receipt(q, data)); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	return q
}
func ownerScope() archive.ReadScope {
	return archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: 1}
}

func TestActiveOriginalAndDuplicateBlockEviction(t *testing.T) {
	f := newRepository()
	s, _, _ := testStore(t, f, Options{})
	q := retainTestOriginal(t, s, f)
	f.processed = true
	o, err := s.OpenOriginal(context.Background(), ownerScope(), q.MediaID)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := o.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); !errors.Is(err, ErrActiveUse) {
		t.Fatal(err)
	}
	if err := s.Evict(context.Background(), q.MediaID); !errors.Is(err, ErrActiveUse) {
		t.Fatal(err)
	}
	f.mu.Lock()
	state := f.artifacts[f.media[q.MediaID]].State
	f.mu.Unlock()
	if state != "retained" {
		t.Fatal("busy reader revoked retained association")
	}
	_ = fd.Close()
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Evict(context.Background(), q.MediaID); err != nil {
		t.Fatal(err)
	}
	if f.charge(archive.BudgetOriginals) != 0 {
		t.Fatal("eviction charge leaked")
	}
}
func TestOriginalIndependentReadOffsets(t *testing.T) {
	f := newRepository()
	s, _, _ := testStore(t, f, Options{})
	q := retainTestOriginal(t, s, f)
	o, err := s.OpenOriginal(context.Background(), ownerScope(), q.MediaID)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	a, _ := o.Open()
	b, _ := o.Open()
	defer a.Close()
	defer b.Close()
	one := make([]byte, 8)
	two := make([]byte, 8)
	_, _ = io.ReadFull(a, one)
	_, _ = io.ReadFull(b, two)
	if string(one) != string(two) {
		t.Fatal("reader offsets shared")
	}
}
func TestDecodedSealAndChildLifetime(t *testing.T) {
	f := newRepository()
	s, paths, p := testStore(t, f, Options{})
	d, err := s.ReserveDecoded(context.Background(), f.source, "media-id", 32)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Open(); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	_, _ = d.File().Write([]byte("native input"))
	if err := d.Seal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.File().WriteAt([]byte("mutation"), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatal(err)
	}
	fd, err := d.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fd.Write([]byte("bypass")); err == nil {
		t.Fatal("native descriptor writable")
	}
	if err := d.Close(); !errors.Is(err, ErrActiveUse) {
		t.Fatal(err)
	}
	if f.charge(archive.BudgetDecoded) == 0 || p.held == 0 {
		t.Fatal("active child charge released")
	}
	_ = fd.Close()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(paths.WorkingDir)
	if len(entries) != 0 || p.held != 0 {
		t.Fatal("decoded cleanup leaked")
	}
}
func TestReconcileSkipsActiveAndPreservesUnknown(t *testing.T) {
	f := newRepository()
	s, paths, _ := testStore(t, f, Options{})
	d, err := s.AdmitDownload(context.Background(), request(f, pngData(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	unknown := filepath.Join(paths.MediaDir, "unknown-original")
	if err := os.WriteFile(unknown, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := s.Reconcile(context.Background(), 256)
	if err != nil {
		t.Fatal(err)
	}
	if report.Active != 1 || report.Cleaned != 0 {
		t.Fatalf("wrong report %+v", report)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatal("unknown file guessed as orphan")
	}
}
func TestReconcileRejectsUnsafeUnknowns(t *testing.T) {
	for _, attack := range []string{"symlink", "hardlink"} {
		t.Run(attack, func(t *testing.T) {
			f := newRepository()
			s, paths, _ := testStore(t, f, Options{})
			outside := filepath.Join(filepath.Dir(paths.Root), "outside")
			_ = os.WriteFile(outside, []byte("preserve"), 0600)
			target := filepath.Join(paths.MediaDir, "unsafe")
			var err error
			if attack == "symlink" {
				err = os.Symlink(outside, target)
			} else {
				err = os.Link(outside, target)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reconcile(context.Background(), 256); !errors.Is(err, ErrUnsafeFile) {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(outside)
			if string(got) != "preserve" {
				t.Fatal("unsafe target touched")
			}
			_ = os.Remove(target)
		})
	}
}
func TestUncertainPublicationRetainsOwnershipUntilInspection(t *testing.T) {
	f := newRepository()
	f.publishErr = context.DeadlineExceeded
	f.inspectErr = archive.ErrArchiveUnavailable
	s, paths, p := testStore(t, f, Options{})
	data := pngData(t)
	q := request(f, data)
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.File().Write(data)
	if err := d.Complete(context.Background(), receipt(q, data)); !errors.Is(err, ErrPublicationUncertain) {
		t.Fatal(err)
	}
	if err := d.Close(); !errors.Is(err, ErrPublicationUncertain) {
		t.Fatal(err)
	}
	if p.held == 0 || f.charge(archive.BudgetOriginals) == 0 {
		t.Fatal("uncertain ownership released")
	}
	if _, err := os.Stat(filepath.Join(paths.MediaDir, artifactName(d.reservation.ArtifactID, ".original"))); err != nil {
		t.Fatal("uncertain file removed")
	}
	f.inspectErr = nil
	f.denied[q.MediaID] = true
	f.media["shared-source"] = d.reservation.ArtifactID
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	o, err := s.OpenOriginal(context.Background(), ownerScope(), "shared-source")
	if err != nil {
		t.Fatal("other authorized association lost", err)
	}
	_ = o.Close()
}
func TestUncertainCompleteRetryCannotClaimRetention(t *testing.T) {
	f := newRepository()
	f.publishErr = context.DeadlineExceeded
	f.rejectPublish = true
	f.inspectErr = archive.ErrArchiveUnavailable
	s, _, _ := testStore(t, f, Options{})
	data := pngData(t)
	q := request(f, data)
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, _ = d.File().Write(data)
	if err := d.Complete(context.Background(), receipt(q, data)); !errors.Is(err, ErrPublicationUncertain) {
		t.Fatal(err)
	}
	f.inspectErr = nil
	if err := d.Complete(context.Background(), receipt(q, data)); !errors.Is(err, ErrSourceChanged) {
		t.Fatal("nonretained retry falsely succeeded", err)
	}
}
func TestCleanupFailureRetainsChargeAndRetry(t *testing.T) {
	f := newRepository()
	s, paths, p := testStore(t, f, Options{})
	d, err := s.AdmitDownload(context.Background(), request(f, pngData(t)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.File().Write([]byte("partial"))
	f.cleanupErr = archive.ErrArchiveUnavailable
	if err := d.Close(); !errors.Is(err, archive.ErrArchiveUnavailable) {
		t.Fatal(err)
	}
	if p.held == 0 || f.charge(archive.BudgetOriginals) == 0 {
		t.Fatal("quota prematurely released")
	}
	if _, err := os.Stat(filepath.Join(paths.MediaDir, artifactName(d.reservation.ArtifactID, ".part"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stopped partial remains")
	}
	f.cleanupErr = nil
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if p.held != 0 || f.charge(archive.BudgetOriginals) != 0 {
		t.Fatal("retry leaked")
	}
}
func TestBoundedFileExtentAllowsCryptoOverwrite(t *testing.T) {
	f := newRepository()
	s, _, _ := testStore(t, f, Options{ImageMaximumBytes: 16})
	q := request(f, []byte("plaintext"))
	q.DeclaredBytes = 0
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	wire := wireMaximum(16)
	if _, err := d.File().Write(make([]byte, wire)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.File().WriteAt(make([]byte, wire), 0); err != nil {
		t.Fatal("bounded crypto overwrite rejected", err)
	}
	if _, err := d.File().Seek(1<<62, io.SeekCurrent); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if err := d.File().Truncate(wire + 1); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}
func TestEvictionRetryPreservesRequestedMediaID(t *testing.T) {
	f := newRepository()
	s, _, _ := testStore(t, f, Options{})
	retainTestOriginal(t, s, f)
	f.processed = true
	data := pngData(t)
	f.quota[archive.BudgetOriginals] = wireMaximum(int64(len(data)))
	q := request(f, data)
	q.MediaID = "original-request-after-victim"
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.reservation.MediaID != q.MediaID {
		t.Fatal("victim identity replaced requested source")
	}
}
func TestPendingAdmissionRecoveryAfterClose(t *testing.T) {
	f := newRepository()
	s, paths, p := testStore(t, f, Options{})
	f.cleanupErr = archive.ErrArchiveUnavailable
	s.options.OnTransition = func(v Transition) {
		if v.Phase == "reserved" {
			if err := os.WriteFile(filepath.Join(paths.MediaDir, artifactName(v.ArtifactID, ".part")), []byte("crash fixture"), 0600); err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := s.AdmitDownload(context.Background(), request(f, pngData(t))); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	if p.held == 0 {
		t.Fatal("failed admission lost physical ownership")
	}
	if err := s.Close(); !errors.Is(err, ErrActiveUse) {
		t.Fatal(err)
	}
	f.cleanupErr = nil
	report, err := s.Reconcile(context.Background(), 256)
	if err != nil || report.Cleaned != 1 {
		t.Fatalf("cannot recover after drain %+v %v", report, err)
	}
	if p.held != 0 || f.charge(archive.BudgetOriginals) != 0 {
		t.Fatal("pending admission leaked")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestFailedPhysicalReleaseRetriesAfterSQLCleanup(t *testing.T) {
	f := newRepository()
	s, paths, p := testStore(t, f, Options{})
	p.releaseErr = archive.ErrArchiveUnavailable
	s.options.OnTransition = func(v Transition) {
		if v.Phase == "reserved" {
			_ = os.WriteFile(filepath.Join(paths.MediaDir, artifactName(v.ArtifactID, ".part")), []byte("fixture"), 0600)
		}
	}
	if _, err := s.AdmitDownload(context.Background(), request(f, pngData(t))); !errors.Is(err, archive.ErrArchiveUnavailable) {
		t.Fatal(err)
	}
	if f.charge(archive.BudgetOriginals) != 0 || p.held == 0 {
		t.Fatal("release failure not retained after SQL acknowledgement")
	}
	if err := s.Close(); !errors.Is(err, ErrActiveUse) {
		t.Fatal(err)
	}
	p.releaseErr = nil
	if _, err := s.Reconcile(context.Background(), 256); err != nil {
		t.Fatal(err)
	}
	if p.held != 0 {
		t.Fatal("cleaned physical lease not retried")
	}
}
func TestReconcileOwnsDiscardedStoppedHandle(t *testing.T) {
	f := newRepository()
	s, _, p := testStore(t, f, Options{})
	d, err := s.AdmitDownload(context.Background(), request(f, pngData(t)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.File().Write([]byte("partial"))
	f.cleanupErr = archive.ErrArchiveUnavailable
	if err := d.Close(); !errors.Is(err, archive.ErrArchiveUnavailable) {
		t.Fatal(err)
	}
	d = nil
	f.cleanupErr = nil
	if _, err := s.Reconcile(context.Background(), 256); err != nil {
		t.Fatal(err)
	}
	if p.held != 0 || f.charge(archive.BudgetOriginals) != 0 {
		t.Fatal("discarded stopped owner leaked")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestNoProcessedVictimPauses(t *testing.T) {
	f := newRepository()
	s, _, p := testStore(t, f, Options{})
	q := retainTestOriginal(t, s, f)
	data := pngData(t)
	f.quota[archive.BudgetOriginals] = int64(len(data)) + wireMaximum(int64(len(data))) - 1
	next := request(f, data)
	next.MediaID = "next-unprocessed"
	if _, err := s.AdmitDownload(context.Background(), next); !errors.Is(err, ErrWaitingForDiskBudget) {
		t.Fatal(err)
	}
	if f.charge(archive.BudgetOriginals) != int64(len(data)) || p.held != 0 {
		t.Fatal("pending original evicted or physical reservation leaked")
	}
	o, err := s.OpenOriginal(context.Background(), ownerScope(), q.MediaID)
	if err != nil {
		t.Fatal(err)
	}
	_ = o.Close()
}
func TestSharedBlobAuthorizesEachSource(t *testing.T) {
	f := newRepository()
	s, _, _ := testStore(t, f, Options{})
	q := retainTestOriginal(t, s, f)
	id := f.media[q.MediaID]
	f.media["allowed-shared"] = id
	f.denied[q.MediaID] = true
	if _, err := s.OpenOriginal(context.Background(), ownerScope(), q.MediaID); !errors.Is(err, archive.ErrSourceUnavailable) {
		t.Fatal("denied association crossedsharedhash", err)
	}
	o, err := s.OpenOriginal(context.Background(), ownerScope(), "allowed-shared")
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if o.Metadata().ArtifactID != id {
		t.Fatal("sharedartifact identitychanged")
	}
	got := make([]byte, o.Metadata().Bytes)
	if _, err := o.Reader().ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(pngData(t)) {
		t.Fatal("allowedsharedbytes corrupted")
	}
}

func TestReconcilePreservesPrivateNativeExecutableNamespace(t *testing.T) {
	f := newRepository()
	s, paths, _ := testStore(t, f, Options{})
	namespace := filepath.Join(paths.WorkingDir, "native-exec")
	if err := os.Mkdir(namespace, 0700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(namespace)
	if err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(paths.WorkingDir, "unknown-decoded")
	if err := os.WriteFile(unknown, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := s.Reconcile(context.Background(), 256)
	if err != nil {
		t.Fatalf("private native executable namespace rejected: %v", err)
	}
	if report.Cleaned != 0 || report.Active != 0 {
		t.Fatalf("unknown namespace manufactured media ownership: %+v", report)
	}
	after, err := os.Stat(namespace)
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0700 {
		t.Fatal("native executable namespace changed", err)
	}
	entries, err := os.ReadDir(namespace)
	if err != nil || len(entries) != 0 {
		t.Fatal("empty native executable namespace changed", err)
	}
	if got, err := os.ReadFile(unknown); err != nil || string(got) != "preserve" {
		t.Fatal("unknown working file changed", err)
	}
}

func TestReconcileRejectsUnsafeNativeExecutableNamespace(t *testing.T) {
	for _, attack := range []string{"media-directory", "unexpected-directory", "public-directory", "regular-file", "symlink"} {
		t.Run(attack, func(t *testing.T) {
			f := newRepository()
			s, paths, p := testStore(t, f, Options{})
			d, err := s.ReserveDecoded(context.Background(), f.source, "media-id", 32)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			charged := f.charge(archive.BudgetDecoded)
			held := p.held
			target := filepath.Join(paths.WorkingDir, "native-exec")
			outside := filepath.Join(filepath.Dir(paths.Root), "outside-namespace")
			if err := os.Mkdir(outside, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "preserved"), []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			switch attack {
			case "media-directory":
				target = filepath.Join(paths.MediaDir, "native-exec")
				err = os.Mkdir(target, 0700)
			case "unexpected-directory":
				target = filepath.Join(paths.WorkingDir, "other-exec")
				err = os.Mkdir(target, 0700)
			case "public-directory":
				err = os.Mkdir(target, 0755)
			case "regular-file":
				err = os.WriteFile(target, []byte("preserve"), 0600)
			case "symlink":
				err = os.Symlink(outside, target)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reconcile(context.Background(), 256); !errors.Is(err, ErrUnsafeFile) {
				t.Fatalf("unsafe namespace accepted: %v", err)
			}
			if f.charge(archive.BudgetDecoded) != charged || p.held != held || charged == 0 || held == 0 {
				t.Fatal("unsafe namespace released active decoded ownership")
			}
			if _, err := os.Lstat(target); err != nil {
				t.Fatal("unsafe namespace removed", err)
			}
			if got, err := os.ReadFile(filepath.Join(outside, "preserved")); err != nil || string(got) != "preserve" {
				t.Fatal("symlink target changed", err)
			}
		})
	}
}

// A process crash leaves this application-owned node before the native
// transport's next startup can validate and remove it.
func nativeSocketReconcileStore(t *testing.T) (*Store, appdata.Paths) {
	t.Helper()
	base := "/tmp"
	if runtime.GOOS == "darwin" {
		base = "/private/tmp"
	}
	root, err := os.MkdirTemp(base, "wa-media-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	paths, err := appdata.Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(paths, newRepository(), Options{Admission: &physicalAdmission{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, paths
}

func staleReconcileSocket(t *testing.T, path string) {
	t.Helper()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcilePreservesPrivateNativeSocket(t *testing.T) {
	s, paths := nativeSocketReconcileStore(t)
	path := filepath.Join(paths.WorkingDir, "native.sock")
	staleReconcileSocket(t, path)
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.Reconcile(context.Background(), 256)
	if err != nil {
		t.Fatalf("crash-left private native socket blocks reconciliation: %v", err)
	}
	if report.Cleaned != 0 || report.Active != 0 {
		t.Fatalf("native socket manufactured media ownership: %+v", report)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || after.Mode()&os.ModeSocket == 0 || after.Mode().Perm() != 0600 {
		t.Fatal("native socket modified or removed", err)
	}
}

func TestReconcileRejectsUnsafeNativeSocket(t *testing.T) {
	for _, kind := range []string{"media-socket", "other-socket", "public-socket", "special-mode-socket", "linked-socket", "regular-file", "directory", "symlink", "parent-replacement"} {
		t.Run(kind, func(t *testing.T) {
			s, paths := nativeSocketReconcileStore(t)
			path := filepath.Join(paths.WorkingDir, "native.sock")
			switch kind {
			case "media-socket":
				path = filepath.Join(paths.MediaDir, "native.sock")
				staleReconcileSocket(t, path)
			case "other-socket":
				path = filepath.Join(paths.WorkingDir, "other.sock")
				staleReconcileSocket(t, path)
			case "public-socket":
				staleReconcileSocket(t, path)
				if err := os.Chmod(path, 0666); err != nil {
					t.Fatal(err)
				}
			case "special-mode-socket":
				staleReconcileSocket(t, path)
				if err := os.Chmod(path, 0600|os.ModeSticky); err != nil {
					t.Fatal(err)
				}
			case "linked-socket":
				staleReconcileSocket(t, path)
				if err := os.Link(path, filepath.Join(paths.WorkingDir, "linked.sock")); err != nil {
					t.Fatal(err)
				}
			case "regular-file":
				if err := os.WriteFile(path, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(paths.WorkingDir, "preserved")
				if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "parent-replacement":
				staleReconcileSocket(t, path)
				original, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				oldWorking := filepath.Join(paths.Root, "retained-working")
				if err := os.Rename(paths.WorkingDir, oldWorking); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(paths.WorkingDir, 0700); err != nil {
					t.Fatal(err)
				}
				staleReconcileSocket(t, path)
				t.Cleanup(func() {
					after, err := os.Lstat(filepath.Join(oldWorking, "native.sock"))
					if err != nil || !os.SameFile(original, after) {
						t.Error("retained socket changed or removed", err)
					}
				})
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Reconcile(context.Background(), 256); err == nil {
				t.Fatal("unsafe native socket namespace accepted")
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("unsafe entry changed or removed", err)
			}
		})
	}
}
