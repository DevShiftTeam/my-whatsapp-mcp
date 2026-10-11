//go:build darwin || linux

package workerlife

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestExecutableNamespaceAdmissionCancellationWritesNothing(t *testing.T) {
	var growth namespaceTestGrowth
	ns, root := namespaceTestNew(t, func(ctx context.Context, bytes int64) (GrowthLease, error) {
		return &growth, nil
	})
	source := namespaceTestSource(t, namespaceTestMachO())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lease, err := ns.Prepare(ctx, source, 64<<20)
	if lease != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("lease=%v error=%v", lease, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v error=%v", entries, err)
	}
	if growth.calls != 0 {
		t.Fatal("unadmitted cancellation released unrelated growth")
	}
}

func TestExecutableNamespaceCancellationAfterAdmissionRefundsOnlyAfterCleanup(t *testing.T) {
	var growth namespaceTestGrowth
	ctx, cancel := context.WithCancel(context.Background())
	ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) {
		cancel()
		return &growth, nil
	})
	lease, err := ns.Prepare(ctx, namespaceTestSource(t, namespaceTestMachO()), 64<<20)
	if lease != nil || !errors.Is(err, context.Canceled) || growth.calls != 1 {
		t.Fatalf("lease=%v error=%v calls=%d", lease, err, growth.calls)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v error=%v", entries, err)
	}
}

func TestExecutableNamespaceUsesFDAndExactAdmittedExtent(t *testing.T) {
	data := namespaceTestMachO()
	var growth namespaceTestGrowth
	var admitted int64
	ns, _ := namespaceTestNew(t, func(_ context.Context, bytes int64) (GrowthLease, error) { admitted = bytes; return &growth, nil })
	source := namespaceTestSource(t, data)
	if err := os.Remove(source.Name()); err != nil {
		t.Fatal(err)
	}
	lease, err := ns.Prepare(context.Background(), source, 256<<20)
	if err != nil || lease == nil {
		t.Fatalf("lease=%v error=%v", lease, err)
	}
	if admitted != int64(len(data))+(64<<10) {
		t.Fatalf("admission=%d", admitted)
	}
	copy, err := os.ReadFile(lease.Path())
	if err != nil || !bytes.Equal(copy, data) {
		t.Fatalf("copy mismatch: %v", err)
	}
	info, err := os.Lstat(lease.Path())
	if err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("final mode=%v error=%v", info, err)
	}
	if err := ns.Close(); !errors.Is(err, ErrCleanup) {
		t.Fatalf("active namespace Close=%v", err)
	}
	if err := lease.Close(); err != nil || growth.calls != 1 {
		t.Fatalf("close=%v releases=%d", err, growth.calls)
	}
	if lease.Path() != "" {
		t.Fatal("closed lease returned a runnable path")
	}
}

func TestExecutableNamespaceCopyHasReadOnlyExecutableFD(t *testing.T) {
	data := namespaceTestMachO()
	var growth namespaceTestGrowth
	var admitted int64
	ns, root := namespaceTestNew(t, func(_ context.Context, maximum int64) (GrowthLease, error) {
		admitted = maximum
		return &growth, nil
	})
	cloneCalls := 0
	ns.clone = func(*os.File, *os.File, string) error {
		cloneCalls++
		return errNamespaceCloneUnsupported
	}
	lease, err := ns.Prepare(context.Background(), namespaceTestSource(t, data), 64<<20)
	if lease != nil {
		t.Cleanup(func() {
			if err := lease.Close(); err != nil {
				t.Error("forced COPY fixture cleanup remained owned", err)
			}
		})
	}
	if err != nil || lease == nil {
		t.Fatal("forced COPY fixture preparation failed", err)
	}
	if cloneCalls != 1 || admitted != int64(len(data))+(64<<10) || growth.calls != 0 {
		t.Fatal("forced COPY admission or release ownership changed")
	}
	prepared := lease.(*namespaceExecutableLease)
	wantHash := sha256.Sum256(data)
	if prepared.record.Bytes != int64(len(data)) || prepared.record.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatal("forced COPY manifest changed its exact extent or hash")
	}
	copy, err := os.ReadFile(lease.Path())
	if err != nil || !bytes.Equal(copy, data) {
		t.Fatal("forced COPY did not preserve the tiny fixture")
	}
	info, err := prepared.executable.Stat()
	if err != nil || info.Size() != int64(len(data)) || info.Mode().Perm() != 0500 {
		t.Fatal("forced COPY executable extent or mode changed")
	}
	flags, err := unix.FcntlInt(prepared.executable.Fd(), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal("forced COPY executable access mode unavailable", err)
	}
	if err := ns.Close(); !errors.Is(err, ErrCleanup) {
		t.Fatal("active forced COPY namespace acknowledged cleanup")
	}
	if err := lease.Close(); err != nil || growth.calls != 1 || lease.Path() != "" {
		t.Fatal("forced COPY cleanup did not release exactly once")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatal("forced COPY cleanup left namespace entries")
	}
	if err := lease.Close(); err != nil || growth.calls != 1 {
		t.Fatal("forced COPY cleanup retry repeated release")
	}
	// Inspect the held descriptor, not just chmod(0500), without executing it.
	if flags&unix.O_ACCMODE != unix.O_RDONLY {
		t.Fatal("forced COPY retained a writable executable descriptor")
	}
}

func TestExecutableNamespaceCopyHandoffCloseFailureRetainsOwner(t *testing.T) {
	var growth namespaceTestGrowth
	ns, _ := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return &growth, nil })
	ns.clone = func(*os.File, *os.File, string) error { return errNamespaceCloneUnsupported }
	closeCalls := 0
	ns.closeFile = func(file *os.File) error {
		closeCalls++
		// Simulate an unknown first-close result after the owned writer closed.
		// A retry must not close a reused numeric descriptor or refund growth.
		if err := file.Close(); err != nil {
			t.Error("injected writer first close failed", err)
		}
		return io.ErrUnexpectedEOF
	}
	lease, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
	if lease != nil {
		partial := lease.(*namespaceExecutableLease)
		// Test teardown closes only the retained fixture descriptors. It does not
		// retry the uncertain writer, remove ownership records, or refund growth.
		t.Cleanup(func() {
			for _, file := range []*os.File{partial.handoff, partial.manifestFile, partial.directory, ns.anchor.file} {
				if file != nil {
					_ = file.Close()
				}
			}
		})
	}
	if lease == nil || !errors.Is(err, ErrCleanup) {
		t.Fatal("uncertain writer close lost its cleanup owner")
	}
	partial := lease.(*namespaceExecutableLease)
	if !partial.handoffCloseUnknown || partial.handoff == nil || lease.Path() != "" || closeCalls != 1 || growth.calls != 0 {
		t.Fatal("uncertain writer close exposed an executable or released ownership")
	}
	flags, err := unix.FcntlInt(partial.handoff.Fd(), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_ACCMODE != unix.O_RDONLY {
		t.Fatal("uncertain writer close did not retain its readonly descriptor")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := lease.Close(); !errors.Is(err, ErrCleanup) {
			t.Fatal("uncertain writer first-close result acknowledged cleanup")
		}
	}
	if closeCalls != 1 || growth.calls != 0 {
		t.Fatal("uncertain writer close was retried or growth released")
	}
	if err := ns.Close(); !errors.Is(err, ErrCleanup) {
		t.Fatal("namespace discarded an uncertain cleanup owner")
	}
}

func TestExecutableNamespaceRejectsExtentBeforeAdmission(t *testing.T) {
	calls := 0
	ns, _ := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { calls++; return &namespaceTestGrowth{}, nil })
	source := namespaceTestSource(t, namespaceTestMachO())
	for _, maximum := range []int64{0, 257 << 20, 1} {
		if lease, err := ns.Prepare(context.Background(), source, maximum); lease != nil || !errors.Is(err, ErrLimit) {
			t.Fatalf("maximum=%d lease=%v error=%v", maximum, lease, err)
		}
	}
	if calls != 0 {
		t.Fatal("invalid extent reached admission")
	}
}

func TestExecutableNamespaceCloneErrorDoesNotSilentlyCopy(t *testing.T) {
	var growth namespaceTestGrowth
	ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return &growth, nil })
	ns.clone = func(*os.File, *os.File, string) error { return io.ErrUnexpectedEOF }
	ns.copy = func(context.Context, *os.File, *os.File, int64) error {
		t.Fatal("unexpected copy fallback")
		return nil
	}
	lease, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
	if lease != nil || err == nil || growth.calls != 1 {
		t.Fatalf("lease=%v error=%v releases=%d", lease, err, growth.calls)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v error=%v", entries, err)
	}
}

func TestExecutableNamespaceFailedCopyRetainsFailedReleaseForRetry(t *testing.T) {
	growth := &namespaceTestGrowth{failures: 1}
	ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return growth, nil })
	ns.clone = func(*os.File, *os.File, string) error { return errNamespaceCloneUnsupported }
	ns.copy = func(_ context.Context, _ *os.File, output *os.File, _ int64) error {
		_, _ = output.Write([]byte("partial"))
		return io.ErrUnexpectedEOF
	}
	lease, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
	if lease == nil || !errors.Is(err, ErrCleanup) || growth.calls != 1 {
		t.Fatalf("lease=%v error=%v releases=%d", lease, err, growth.calls)
	}
	if lease.Path() != "" {
		t.Fatal("failed preparation returned executable path")
	}
	if err := ns.Close(); !errors.Is(err, ErrCleanup) {
		t.Fatalf("pending Close=%v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("physical cleanup incomplete: %v %v", entries, err)
	}
	if err := lease.Close(); err != nil || growth.calls != 2 {
		t.Fatalf("retry=%v releases=%d", err, growth.calls)
	}
}

func TestExecutableNamespacePublishedCleanupReleaseIsRetryable(t *testing.T) {
	growth := &namespaceTestGrowth{failures: 1}
	ns, _ := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return growth, nil })
	lease, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); !errors.Is(err, ErrCleanup) {
		t.Fatalf("first Close=%v", err)
	}
	if err := ns.Close(); !errors.Is(err, ErrCleanup) {
		t.Fatalf("namespace Close=%v", err)
	}
	if err := lease.Close(); err != nil || growth.calls != 2 {
		t.Fatalf("retry=%v releases=%d", err, growth.calls)
	}
	if err := lease.Close(); err != nil || growth.calls != 2 {
		t.Fatalf("idempotent Close=%v releases=%d", err, growth.calls)
	}
}

func TestExecutableNamespaceRefusesReplacedPublishedDirectory(t *testing.T) {
	var growth namespaceTestGrowth
	ns, _ := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return &growth, nil })
	lease, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(lease.Path())
	saved := dir + ".saved"
	if err := os.Rename(dir, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(dir, "keep")
	if err := os.WriteFile(unknown, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if lease.Path() != "" {
		t.Fatal("replacement path was accepted")
	}
	if err := lease.Close(); !errors.Is(err, ErrCleanup) || growth.calls != 0 {
		t.Fatalf("close=%v releases=%d", err, growth.calls)
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatalf("foreign entry removed: %v", err)
	}
	if err := os.Remove(unknown); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, dir); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil || growth.calls != 1 {
		t.Fatalf("restored exact inode retry=%v releases=%d", err, growth.calls)
	}
}

func TestExecutableNamespaceRequiresRecoveryAndMatchingBorrowedAnchor(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	admit := func(context.Context, int64) (GrowthLease, error) { return &namespaceTestGrowth{}, nil }
	if ns, err := NewExecutableNamespace(NamespaceConfig{Directory: directory, Path: t.TempDir(), Admit: admit}); ns != nil || err == nil {
		t.Fatalf("mismatched anchor namespace=%v error=%v", ns, err)
	}
	ns, err := NewExecutableNamespace(NamespaceConfig{Directory: directory, Path: root, Admit: admit})
	if err != nil {
		t.Fatal(err)
	}
	defer ns.Close()
	if lease, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20); lease != nil || !errors.Is(err, ErrCleanup) {
		t.Fatalf("unrecovered lease=%v error=%v", lease, err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ns.Recover(context.Background()); err != nil {
		t.Fatalf("borrowed FD was not duplicated: %v", err)
	}
}

func TestExecutableNamespaceRecoveryIgnoresForeignRootButRetainsUnknownOwnedEntry(t *testing.T) {
	ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) {
		t.Fatal("Recover must not admit")
		return nil, ErrProtocol
	})
	foreign := filepath.Join(root, "other-cache-file")
	if err := os.WriteFile(foreign, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ns.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	owned := filepath.Join(root, "workerlife-stage-0123456789abcdef0123456789abcdef")
	if err := os.Mkdir(owned, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, "unexpected"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ns.Recover(context.Background()); !errors.Is(err, ErrCleanup) {
		t.Fatalf("unknown owned entry=%v", err)
	}
	if _, err := os.Stat(filepath.Join(owned, "unexpected")); err != nil {
		t.Fatalf("unknown entry removed: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign root entry removed: %v", err)
	}
}

func TestExecutableNamespaceRecoveryDeletesKnownOrphanWithoutAdmission(t *testing.T) {
	var originalGrowth namespaceTestGrowth
	ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return &originalGrowth, nil })
	prepared, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	lease := prepared.(*namespaceExecutableLease)
	// Model process death: no Close/Release transaction survives; persisted
	// bytes remain. The caller has independently observed guard recovery EOF.
	_ = lease.executable.Close()
	_ = lease.manifestFile.Close()
	_ = lease.directory.Close()
	ns.leases = make(map[*namespaceExecutableLease]struct{})
	if err := ns.Close(); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewExecutableNamespace(NamespaceConfig{Directory: directory, Path: root,
		Admit: func(context.Context, int64) (GrowthLease, error) {
			t.Fatal("Recover admitted growth")
			return nil, ErrProtocol
		}})
	_ = directory.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("orphan remains: %v %v", entries, err)
	}
	if originalGrowth.calls != 0 {
		t.Fatal("recovery refunded an old process's reservation object")
	}
}

func TestExecutableNamespaceRecoveryRetainsPostUnlinkCheckpointsForRetry(t *testing.T) {
	var originalGrowth namespaceTestGrowth
	ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return &originalGrowth, nil })
	prepared, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	lease := prepared.(*namespaceExecutableLease)
	name := lease.name
	_ = lease.executable.Close()
	_ = lease.manifestFile.Close()
	_ = lease.directory.Close()
	ns.leases = make(map[*namespaceExecutableLease]struct{})
	if err := ns.Close(); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewExecutableNamespace(NamespaceConfig{Directory: directory, Path: root,
		Admit: func(context.Context, int64) (GrowthLease, error) {
			t.Fatal("Recover admitted growth")
			return nil, ErrProtocol
		}})
	_ = directory.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	removeCalls := 0
	restarted.removeDirectory = func(parent *os.File, child string) error {
		removeCalls++
		if removeCalls == 1 {
			return io.ErrUnexpectedEOF
		}
		return namespaceRemoveDirectory(parent, child)
	}
	if err := restarted.Recover(context.Background()); !errors.Is(err, ErrCleanup) {
		t.Fatalf("first Recover=%v", err)
	}
	pending := restarted.pendingRecovery
	if pending == nil || !pending.executableRemoved || !pending.manifestRemoved || !pending.executableClosed || !pending.manifestClosed || pending.directoryClosed {
		t.Fatal("post-unlink checkpoints or held directory lost")
	}
	if entries, err := os.ReadDir(filepath.Join(root, name)); err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v error=%v", entries, err)
	}
	if err := restarted.Close(); !errors.Is(err, ErrCleanup) {
		t.Fatalf("pending Close=%v", err)
	}
	if err := restarted.Recover(context.Background()); err != nil {
		t.Fatalf("retry Recover=%v", err)
	}
	if restarted.pendingRecovery != nil || !pending.closed || !pending.directoryClosed || removeCalls != 2 {
		t.Fatal("retry did not finish the retained cleanup transaction")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("orphan remains: %v %v", entries, err)
	}
	if originalGrowth.calls != 0 {
		t.Fatal("recovery refunded an old process's reservation object")
	}
}

func TestExecutableNamespaceCleanupDurableBoundariesAreRetryable(t *testing.T) {
	want := []string{"unlink-helper", "sync-helper", "unlink-manifest", "sync-manifest", "close-helper", "close-manifest", "rmdir", "sync-parent", "close-directory"}
	for _, owner := range []string{"live", "recovery"} {
		for _, failure := range want {
			t.Run(owner+"/"+failure, func(t *testing.T) {
				var growth namespaceTestGrowth
				ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return &growth, nil })
				prepared, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
				if err != nil {
					t.Fatal(err)
				}
				lease := prepared.(*namespaceExecutableLease)
				if owner == "recovery" {
					ns = namespaceTestRestart(t, ns, root)
				}
				transaction := lease
				checkpoint := func() *namespaceExecutableLease {
					if ns.pendingRecovery != nil {
						transaction = ns.pendingRecovery
					}
					return transaction
				}
				var completed []string
				failed := false
				operation := func(name string, call func() error) error {
					if name == failure && !failed {
						failed = true
						return io.ErrUnexpectedEOF
					}
					if err := call(); err != nil {
						return err
					}
					completed = append(completed, name)
					return nil
				}
				ns.unlinkFile = func(directory *os.File, name string) error {
					step := "unlink-helper"
					if name == "manifest.json" {
						step = "unlink-manifest"
					}
					return operation(step, func() error { return namespaceUnlinkFile(directory, name) })
				}
				ns.syncDirectory = func(directory *os.File) error {
					step := "sync-helper"
					current := checkpoint()
					if directory == ns.anchor.file {
						if current.closed {
							return directory.Sync()
						}
						step = "sync-parent"
					} else if current.helperSynced {
						step = "sync-manifest"
					}
					return operation(step, directory.Sync)
				}
				ns.removeDirectory = func(directory *os.File, name string) error {
					return operation("rmdir", func() error { return namespaceRemoveDirectory(directory, name) })
				}
				ns.closeFile = func(file *os.File) error {
					step := "close-directory"
					current := checkpoint()
					if file == current.executable {
						step = "close-helper"
					} else if file == current.manifestFile {
						step = "close-manifest"
					}
					return operation(step, file.Close)
				}
				run := lease.Close
				wantReleases := 1
				if owner == "recovery" {
					run = func() error { return ns.Recover(context.Background()) }
					wantReleases = 0
				}
				if err := run(); !errors.Is(err, ErrCleanup) || !failed || growth.calls != 0 {
					t.Fatalf("first cleanup=%v failed=%v releases=%d", err, failed, growth.calls)
				}
				if lease.Path() != "" {
					t.Fatal("uncertain cleanup retained runnable path")
				}
				if err := ns.Close(); !errors.Is(err, ErrCleanup) {
					t.Fatalf("uncertain namespace Close=%v", err)
				}
				if err := run(); err != nil || growth.calls != wantReleases {
					t.Fatalf("retry cleanup=%v releases=%d", err, growth.calls)
				}
				if !reflect.DeepEqual(completed, want) {
					t.Fatalf("completed=%v want=%v", completed, want)
				}
				if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
					t.Fatalf("entries=%v error=%v", entries, err)
				}
			})
		}
	}
}

func TestExecutableNamespaceRestartRecoversMissingHelperAndEmptyOwnedDirectory(t *testing.T) {
	for _, kind := range []string{"manifest-preclone", "manifest-stage", "manifest-exec", "empty-stage", "empty-exec"} {
		t.Run(kind, func(t *testing.T) {
			var growth namespaceTestGrowth
			ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return &growth, nil })
			if kind == "manifest-preclone" || kind == "manifest-stage" || kind == "manifest-exec" {
				prepared, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
				if err != nil {
					t.Fatal(err)
				}
				lease := prepared.(*namespaceExecutableLease)
				if err := os.Remove(filepath.Join(root, lease.name, "helper")); err != nil {
					t.Fatal(err)
				}
				if kind == "manifest-preclone" || kind == "manifest-stage" {
					name := "workerlife-stage-" + lease.record.Token
					if err := os.Rename(filepath.Join(root, lease.name), filepath.Join(root, name)); err != nil {
						t.Fatal(err)
					}
					lease.name = name
					if kind == "manifest-preclone" {
						record := lease.record
						record.FileDevice, record.FileInode = 0, 0
						data, err := json.Marshal(record)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(root, name, "manifest.json"), data, 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
			} else {
				prefix := "workerlife-stage-"
				if kind == "empty-exec" {
					prefix = "workerlife-exec-"
				}
				if err := os.Mkdir(filepath.Join(root, prefix+"0123456789abcdef0123456789abcdef"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			restarted := namespaceTestRestart(t, ns, root)
			if err := restarted.Recover(context.Background()); err != nil {
				t.Fatalf("restart Recover=%v", err)
			}
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
				t.Fatalf("entries=%v error=%v", entries, err)
			}
			if growth.calls != 0 {
				t.Fatal("restart released the old admission object")
			}
		})
	}
}

func TestExecutableNamespaceRestartRetainsUnknownOrMismatchedOwnedObjects(t *testing.T) {
	for _, kind := range []string{"malformed-manifest", "directory-inode", "file-inode", "file-device", "helper-digest", "manifestless-nonempty", "foreign-entry", "unsafe-empty-mode"} {
		t.Run(kind, func(t *testing.T) {
			var growth namespaceTestGrowth
			ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return &growth, nil })
			prepared, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			lease := prepared.(*namespaceExecutableLease)
			directory := filepath.Join(root, lease.name)
			manifest := filepath.Join(directory, "manifest.json")
			switch kind {
			case "malformed-manifest":
				err = os.WriteFile(manifest, []byte("{"), 0600)
			case "directory-inode", "file-inode", "file-device":
				record := lease.record
				if kind == "directory-inode" {
					record.DirectoryInode++
				} else if kind == "file-inode" {
					record.FileInode++
				} else {
					record.FileDevice ^= 1
				}
				data, encodeErr := json.Marshal(record)
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				err = os.WriteFile(manifest, data, 0600)
			case "helper-digest":
				if err := os.Chmod(filepath.Join(directory, "helper"), 0700); err != nil {
					t.Fatal(err)
				}
				data := namespaceTestMachO()
				data[len(data)-1] ^= 1
				if err := os.WriteFile(filepath.Join(directory, "helper"), data, 0500); err != nil {
					t.Fatal(err)
				}
				err = os.Chmod(filepath.Join(directory, "helper"), 0500)
			case "manifestless-nonempty":
				err = os.Remove(manifest)
			case "foreign-entry":
				err = os.WriteFile(filepath.Join(directory, "foreign"), []byte("keep"), 0600)
			case "unsafe-empty-mode":
				if err := os.Remove(filepath.Join(directory, "helper")); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(manifest); err != nil {
					t.Fatal(err)
				}
				err = os.Chmod(directory, 0777)
			}
			if err != nil {
				t.Fatal(err)
			}
			restarted := namespaceTestRestart(t, ns, root)
			if err := restarted.Recover(context.Background()); !errors.Is(err, ErrCleanup) {
				t.Fatalf("restart Recover=%v", err)
			}
			if _, err := os.Lstat(directory); err != nil {
				t.Fatalf("uncertain directory removed: %v", err)
			}
			if growth.calls != 0 {
				t.Fatal("uncertain restart released old admission object")
			}
		})
	}
}

func TestExecutableNamespaceRetainedDeviceMismatchRefusesPathAndCleanup(t *testing.T) {
	var growth namespaceTestGrowth
	ns, _ := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) { return &growth, nil })
	prepared, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	lease := prepared.(*namespaceExecutableLease)
	path := lease.Path()
	for _, identity := range []*namespaceIdentity{&lease.directoryIdentity, &lease.manifestIdentity, &lease.executableIdentity} {
		device := identity.Device
		identity.Device ^= 1
		if lease.Path() != "" {
			t.Fatal("mismatched retained device returned executable path")
		}
		if err := lease.Close(); !errors.Is(err, ErrCleanup) || growth.calls != 0 {
			t.Fatalf("device mismatch Close=%v releases=%d", err, growth.calls)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("device mismatch removed helper: %v", err)
		}
		identity.Device = device
		lease.ready = true
	}
	if err := lease.Close(); err != nil || growth.calls != 1 {
		t.Fatalf("restored device Close=%v releases=%d", err, growth.calls)
	}
}

func TestExecutableNamespaceRejectsNilAdmissionLease(t *testing.T) {
	ns, root := namespaceTestNew(t, func(context.Context, int64) (GrowthLease, error) {
		var growth *namespaceTestGrowth
		return growth, nil
	})
	if lease, err := ns.Prepare(context.Background(), namespaceTestSource(t, namespaceTestMachO()), 64<<20); lease != nil || !errors.Is(err, ErrProtocol) {
		t.Fatalf("typed nil lease=%v error=%v", lease, err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v error=%v", entries, err)
	}
}

func TestExecutableNamespaceManifestDecoderRejectsDuplicatesAndOverflow(t *testing.T) {
	for _, data := range []string{
		`{"version":1,"version":1}`,
		`{"version":1,"Version":1}`,
		`{"token":"a","Token":"b"}`,
		`{"VERSION":1}`,
		`{"version":1,"unknown":0}`,
		`{"version":1} {"version":1}`,
		string(bytes.Repeat([]byte(" "), int(namespaceManifestMaximum)+1)),
	} {
		if _, err := decodeNamespaceManifest(bytes.NewBufferString(data)); err == nil {
			t.Fatalf("invalid manifest accepted: %q", data)
		}
	}
}

func TestExecutableNamespaceManifestDecoderAcceptsExactSchemaRoundTrip(t *testing.T) {
	want := namespaceManifest{Version: 1, Token: "0123456789abcdef0123456789abcdef", Bytes: 32,
		SHA256:          "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		DirectoryDevice: 1, DirectoryInode: 2, FileDevice: 1, FileInode: 3}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeNamespaceManifest(bytes.NewReader(data))
	if err != nil || got != want {
		t.Fatalf("record=%+v error=%v", got, err)
	}
}

func namespaceTestNew(t *testing.T, admit func(context.Context, int64) (GrowthLease, error)) (*ExecutableNamespace, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := NewExecutableNamespace(NamespaceConfig{Directory: directory, Path: root, Admit: admit})
	_ = directory.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Close() })
	if err := ns.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	return ns, root
}

func namespaceTestSource(t *testing.T, data []byte) *os.File {
	t.Helper()
	name := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(name, data, 0500); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func namespaceTestRestart(t *testing.T, old *ExecutableNamespace, root string) *ExecutableNamespace {
	t.Helper()
	// Simulate only the namespace process state loss; guard recovery EOF is the
	// app caller's independent precondition, exercised by lifecycle integration.
	for lease := range old.leases {
		_ = lease.executable.Close()
		_ = lease.manifestFile.Close()
		_ = lease.directory.Close()
	}
	old.leases = make(map[*namespaceExecutableLease]struct{})
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewExecutableNamespace(NamespaceConfig{Directory: directory, Path: root,
		Admit: func(context.Context, int64) (GrowthLease, error) {
			t.Fatal("Recover admitted growth")
			return nil, ErrProtocol
		}})
	_ = directory.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	return restarted
}

type namespaceTestGrowth struct{ calls, failures int }

func (growth *namespaceTestGrowth) Release() error {
	growth.calls++
	if growth.failures > 0 {
		growth.failures--
		return io.ErrUnexpectedEOF
	}
	return nil
}

func namespaceTestMachO(extra ...[]byte) []byte {
	commands := [][]byte{namespaceTestLoad(0xe, "/usr/lib/dyld"), namespaceTestLoad(0xc, "/usr/lib/libSystem.B.dylib")}
	commands = append(commands, extra...)
	var payload []byte
	for _, command := range commands {
		payload = append(payload, command...)
	}
	header := make([]byte, 32)
	binary.LittleEndian.PutUint32(header[0:], 0xfeedfacf)
	binary.LittleEndian.PutUint32(header[4:], 0x0100000c)
	binary.LittleEndian.PutUint32(header[12:], 2)
	binary.LittleEndian.PutUint32(header[16:], uint32(len(commands)))
	binary.LittleEndian.PutUint32(header[20:], uint32(len(payload)))
	return append(header, payload...)
}

func namespaceTestLoad(command uint32, name string) []byte {
	header := 24
	if command == 0xe || command == 0x8000001c || command == 0x27 {
		header = 12
	}
	length := (header + len(name) + 1 + 7) &^ 7
	data := make([]byte, length)
	binary.LittleEndian.PutUint32(data, command)
	binary.LittleEndian.PutUint32(data[4:], uint32(length))
	binary.LittleEndian.PutUint32(data[8:], uint32(header))
	copy(data[header:], name)
	return data
}
