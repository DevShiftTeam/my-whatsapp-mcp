//go:build encoder_candidate_actual && darwin

package appdata

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// These fixtures exercise only new synthetic roots when separately admitted.
// Authoring this file does not execute preparation, locks or capture.
func captureFixture(t *testing.T) (*PrivateCaptureRoot, PrivateCaptureNamespace) {
	t.Helper()
	owner, err := PreparePrivateCaptureRoot(filepath.Join(syntheticParent(t), "capture"))
	if err != nil {
		if owner != nil {
			captureFixtureCleanup(t, owner)
		}
		t.Fatal(err)
	}
	captureFixtureCleanup(t, owner)
	namespace, err := owner.Namespace()
	if err != nil {
		t.Fatal(err)
	}
	return owner, namespace
}

// An intentionally injected close error remains sticky in production. The
// fixture alone disposes still-unattempted synthetic handles after assertions.
func captureFixtureCleanup(t *testing.T, owner *PrivateCaptureRoot) {
	t.Helper()
	t.Cleanup(func() {
		owner.mu.Lock()
		defer owner.mu.Unlock()
		for _, checkpoint := range owner.temporary {
			if !checkpoint.attempted {
				_ = checkpoint.first()
			}
		}
		for _, checkpoint := range []*privateCaptureClose{owner.lockClose, owner.rootClose, owner.guardClose} {
			if checkpoint != nil && !checkpoint.attempted {
				_ = checkpoint.first()
			}
		}
	})
}

func TestPrivateCaptureC0CreatesOnlyKnownEmptyNamespace(t *testing.T) {
	owner, namespace := captureFixture(t)
	if namespace.Device == 0 || namespace.Inode == 0 || namespace.WorkingDevice != namespace.Device || namespace.LockDevice != namespace.Device || namespace.WorkingInode == 0 || namespace.LockInode == 0 || namespace.UID != uint32(os.Geteuid()) {
		t.Fatal("preparation did not bind the actual private namespace")
	}
	entries, err := os.ReadDir(namespace.Paths.Root)
	if err != nil || len(entries) != 8 {
		t.Fatalf("root has unexpected entries: %d, %v", len(entries), err)
	}
	for _, child := range []string{"archive", "session", "media", "working", "models", "clients"} {
		path := filepath.Join(namespace.Paths.Root, child)
		assertMode(t, path, 0700)
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s is not an empty private directory", child)
		}
	}
	assertMode(t, filepath.Join(namespace.Paths.Root, ".wa-indexer-owner"), 0600)
	assertMode(t, filepath.Join(namespace.Paths.Root, ".instance.lock"), 0600)
	if _, err := owner.Guard(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateCaptureC1AcquiresExactRecordedNamespace(t *testing.T) {
	owner, namespace := captureFixture(t)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := AcquirePrivateCaptureRoot(namespace)
	if reopened != nil {
		captureFixtureCleanup(t, reopened)
	}
	if err != nil || reopened == nil {
		t.Fatalf("exact acquisition failed: %v", err)
	}
	actual, err := reopened.Namespace()
	if err != nil || actual != namespace {
		t.Fatal("acquisition changed the recorded namespace")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateCaptureC1RefusesChangedIdentityAndPathTuple(t *testing.T) {
	owner, namespace := captureFixture(t)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*PrivateCaptureNamespace){
		func(n *PrivateCaptureNamespace) { n.Inode++ },
		func(n *PrivateCaptureNamespace) { n.WorkingInode++ },
		func(n *PrivateCaptureNamespace) { n.LockInode++ },
		func(n *PrivateCaptureNamespace) { n.UID++ },
		func(n *PrivateCaptureNamespace) { n.Paths.WorkingDir = n.Paths.ClientDir },
	} {
		changed := namespace
		change(&changed)
		partial, err := AcquirePrivateCaptureRoot(changed)
		if partial != nil {
			captureFixtureCleanup(t, partial)
			_ = partial.Close()
		}
		if !errors.Is(err, ErrUnsafeDataPath) {
			t.Fatalf("changed namespace accepted: %v", err)
		}
	}
}

func TestPrivateCaptureC1RefusesAnotherLiveOwner(t *testing.T) {
	owner, namespace := captureFixture(t)
	partial, err := AcquirePrivateCaptureRoot(namespace)
	if partial != nil {
		captureFixtureCleanup(t, partial)
		if closeErr := partial.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	if !errors.Is(err, ErrInstanceAlreadyRunning) {
		t.Fatalf("second writer was not refused: %v", err)
	}
	if _, err := owner.Guard(); err != nil {
		t.Fatal("failed contender changed the original owner")
	}
}

func TestPrivateCaptureC0RefusesSymlinkAndNonemptyRoot(t *testing.T) {
	parent := syntheticParent(t)
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(outside, "canary")
	if err := os.WriteFile(canary, []byte("owned-fixture-only"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, filepath.Join(link, "capture"), outside} {
		partial, err := PreparePrivateCaptureRoot(path)
		if partial != nil {
			captureFixtureCleanup(t, partial)
			_ = partial.Close()
		}
		if !errors.Is(err, ErrUnsafeDataPath) {
			t.Fatalf("unsafe preparation accepted: %v", err)
		}
	}
	body, err := os.ReadFile(canary)
	if err != nil || string(body) != "owned-fixture-only" {
		t.Fatal("refused preparation modified existing data")
	}
}

func TestPrivateCaptureC1RefusesPopulatedOrExtraEntries(t *testing.T) {
	for _, relative := range []string{"working/canary", "models/canary", "owner-url", "foreign"} {
		t.Run(relative, func(t *testing.T) {
			owner, namespace := captureFixture(t)
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(namespace.Paths.Root, relative)
			if err := os.WriteFile(path, []byte("preserved"), 0600); err != nil {
				t.Fatal(err)
			}
			partial, err := AcquirePrivateCaptureRoot(namespace)
			if partial != nil {
				captureFixtureCleanup(t, partial)
				_ = partial.Close()
			}
			if !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("populated capture namespace accepted: %v", err)
			}
			body, err := os.ReadFile(path)
			if err != nil || string(body) != "preserved" {
				t.Fatal("acquisition modified the refused entry")
			}
		})
	}
}

func TestPrivateCaptureCloseKeepsFirstFailureAndGuard(t *testing.T) {
	owner, _ := captureFixture(t)
	actualClose := owner.lockClose.close
	calls := 0
	owner.lockClose.close = func() error {
		calls++
		if err := actualClose(); err != nil {
			return err
		}
		return errors.New("synthetic-first-close-failure")
	}
	if !errors.Is(owner.Close(), ErrDataUnavailable) || !errors.Is(owner.Close(), ErrDataUnavailable) || calls != 1 {
		t.Fatal("later close erased or retried the first failure")
	}
	if owner.guardClose.attempted {
		t.Fatal("guard was released before instance close acknowledgment")
	}
	if _, err := owner.Guard(); err == nil {
		t.Fatal("uncertain owner authorized a new borrower")
	}
}

func TestPrivateCaptureGuardClosesAfterLockAndRoot(t *testing.T) {
	owner, _ := captureFixture(t)
	var order []string
	for _, step := range []struct {
		name       string
		checkpoint *privateCaptureClose
	}{{"lock", owner.lockClose}, {"root", owner.rootClose}, {"guard", owner.guardClose}} {
		name, checkpoint, original := step.name, step.checkpoint, step.checkpoint.close
		checkpoint.close = func() error { order = append(order, name); return original() }
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[0] != "lock" || order[1] != "root" || order[2] != "guard" {
		t.Fatalf("incorrect authority-close order: %v", order)
	}
}

func TestPrivateCaptureWorkingPreservesCallbackOutcome(t *testing.T) {
	owner, _ := captureFixture(t)
	callbackFailure := errors.New("synthetic-callback-denial")
	calls := 0
	callbackErr, ackErr := owner.WithWorkingAcknowledged(func(working *os.Root) error {
		calls++
		info, err := working.Stat(".")
		if err != nil || !privateDirectory(info) {
			t.Fatal("callback did not receive the acquired private working directory")
		}
		return callbackFailure
	})
	if !errors.Is(callbackErr, callbackFailure) || ackErr != nil || calls != 1 {
		t.Fatalf("callback/acknowledgment conflated: %v %v", callbackErr, ackErr)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateCaptureWorkingDetectsMutationWithoutRemovingIt(t *testing.T) {
	owner, namespace := captureFixture(t)
	callbackErr, ackErr := owner.WithWorkingAcknowledged(func(working *os.Root) error {
		file, err := working.OpenFile("canary", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		return file.Close()
	})
	if callbackErr != nil || !errors.Is(ackErr, ErrUnsafeDataPath) {
		t.Fatalf("populated callback result accepted: %v %v", callbackErr, ackErr)
	}
	if _, err := os.Lstat(filepath.Join(namespace.Paths.WorkingDir, "canary")); err != nil {
		t.Fatal("producer repaired a dirty working directory")
	}
	if !errors.Is(owner.Close(), ErrUnsafeDataPath) || owner.guardClose.attempted {
		t.Fatal("uncertain namespace released authority")
	}
}

func TestPrivateCaptureFirstCloseFailureFencesPreparation(t *testing.T) {
	checkpoint := &privateCaptureClose{close: func() error { return errors.New("synthetic-close-error") }}
	owner := &PrivateCaptureRoot{privateCaptureState: &privateCaptureState{}}
	if !errors.Is(owner.acknowledge(checkpoint), ErrDataUnavailable) || !errors.Is(owner.acknowledge(checkpoint), ErrDataUnavailable) {
		t.Fatal("failed first close was not sticky")
	}
	if owner.ackErr == nil || owner.usable() || owner.ready {
		t.Fatal("unknown close allowed further preparation")
	}
}

func TestPrivateCaptureC0ReturnsPartialOwnerWithoutRepair(t *testing.T) {
	root := filepath.Join(syntheticParent(t), "partial")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, ownerMarker)
	if err := os.WriteFile(marker, []byte(ownerMarkerContent), 0600); err != nil {
		t.Fatal(err)
	}
	partial, err := PreparePrivateCaptureRoot(root)
	if partial == nil || !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("partial preparation was accepted or lost its owner: %v", err)
	}
	if partial.instance != nil || partial.rootClose != nil || partial.guard != nil {
		t.Fatal("refused preparation opened the preexisting final root")
	}
	captureFixtureCleanup(t, partial)
	if _, err := partial.Namespace(); err == nil {
		t.Fatal("refused partial namespace supplied an authoritative receipt")
	}
	if err := partial.Close(); err != nil {
		t.Fatal("known refusal did not acknowledge its temporary handles")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != ownerMarker {
		t.Fatal("preparation repaired a prior partial namespace")
	}
}

func TestPrivateCaptureAncestorReplacementRetainsGuard(t *testing.T) {
	parent := syntheticParent(t)
	ancestor := filepath.Join(parent, "ancestor")
	if err := os.Mkdir(ancestor, 0700); err != nil {
		t.Fatal(err)
	}
	owner, err := PreparePrivateCaptureRoot(filepath.Join(ancestor, "capture"))
	if err != nil {
		if owner != nil {
			captureFixtureCleanup(t, owner)
		}
		t.Fatal(err)
	}
	captureFixtureCleanup(t, owner)
	if err := os.Rename(ancestor, filepath.Join(parent, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ancestor, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Namespace(); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("moved ancestor retained canonical authority: %v", err)
	}
	if !errors.Is(owner.Close(), ErrUnsafeDataPath) || owner.guardClose.attempted {
		t.Fatal("replacement uncertainty released the original guard")
	}
}

func TestPrivateCaptureWorkingCloseFailurePreservesCallbackAndFirstError(t *testing.T) {
	owner, _ := captureFixture(t)
	callbackFailure := errors.New("synthetic-callback-failure")
	calls := 0
	callbackErr, ackErr := owner.WithWorkingAcknowledged(func(working *os.Root) error {
		// The callback-owned rooted handle has a tracked producer close before
		// use begins. Inject a first-close error without reopening its number.
		checkpoint := owner.temporary[len(owner.temporary)-1]
		actualClose := checkpoint.close
		checkpoint.close = func() error {
			calls++
			if err := actualClose(); err != nil {
				return err
			}
			return errors.New("synthetic-working-close-failure")
		}
		return callbackFailure
	})
	if !errors.Is(callbackErr, callbackFailure) || !errors.Is(ackErr, ErrDataUnavailable) || calls != 1 {
		t.Fatalf("working first-close acknowledgment lost: %v %v", callbackErr, ackErr)
	}
	if !errors.Is(owner.Close(), ErrDataUnavailable) || calls != 1 || owner.guardClose.attempted {
		t.Fatal("cleanup retried the working close or released its guard")
	}
}

func TestPrivateCaptureValueCopySharesFirstCloseOwnership(t *testing.T) {
	owner, _ := captureFixture(t)
	copy := *owner
	calls := 0
	actualClose := owner.lockClose.close
	owner.lockClose.close = func() error {
		calls++
		return actualClose()
	}
	if err := copy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil || calls != 1 {
		t.Fatal("value copy repeated the original descriptor close")
	}
	if _, err := owner.Guard(); err == nil {
		t.Fatal("value copy created another guard ownership domain")
	}
}

func TestPrivateCaptureC1NeverLocksReplacementAfterExpectedNameCheck(t *testing.T) {
	owner, namespace := captureFixture(t)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	partial := &PrivateCaptureRoot{privateCaptureState: &privateCaptureState{paths: namespace.Paths}}
	captureFixtureCleanup(t, partial)
	if err := partial.walk(namespace.Paths.Root, false); err != nil {
		t.Fatal(err)
	}
	if err := partial.checkExpectedNames(namespace); err != nil {
		t.Fatal(err)
	}
	// The expected-name check passes, then another otherwise-valid lock inode
	// replaces it before the acquisition helper opens the name. Keep the old
	// inode alive so the filesystem cannot recycle its number for this control.
	original, err := partial.instance.root.OpenFile(instanceLockFile, os.O_RDONLY|noFollowFlag(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = original.Close() })
	if err := partial.instance.root.Remove(instanceLockFile); err != nil {
		t.Fatal(err)
	}
	replacement, err := partial.instance.root.OpenFile(instanceLockFile, os.O_RDWR|os.O_CREATE|os.O_EXCL|noFollowFlag(), 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = replacement.Close() })
	info, err := replacement.Stat()
	device, inode, _, valid := captureIdentity(info)
	if err != nil || !valid || device != namespace.LockDevice || inode == namespace.LockInode {
		t.Fatal("replacement control did not supply a different lock inode")
	}
	if err := partial.acquireLock(false, &namespace); !errors.Is(err, ErrUnsafeDataPath) || partial.guard != nil {
		t.Fatalf("replacement inode supplied lock authority: %v", err)
	}
	if err := lockFile(replacement); err != nil {
		t.Fatal("denied acquisition flocked the replacement inode")
	}
	if err := partial.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateCaptureC0RefusesLeafCreatedAfterAbsenceCheck(t *testing.T) {
	root := filepath.Join(syntheticParent(t), "raced")
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("freshness control was already populated")
	}
	// A peer wins creation after the caller's preliminary absence check.
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	peer, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	before, err := peer.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	partial, err := PreparePrivateCaptureRoot(root)
	if partial == nil || !errors.Is(err, ErrUnsafeDataPath) || partial.instance != nil || partial.rootClose != nil || partial.guard != nil {
		t.Fatalf("prechecked peer leaf was adopted or opened: %v", err)
	}
	captureFixtureCleanup(t, partial)
	if err := partial.Close(); err != nil {
		t.Fatal("refused ancestor-only owner did not acknowledge its handles")
	}
	after, err := peer.Stat(".")
	entries, readErr := os.ReadDir(root)
	if err != nil || readErr != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || len(entries) != 0 {
		t.Fatal("refusal changed or closed the peer's empty root")
	}
}

func TestPrivateCaptureC0RefusesPreexistingEmptyLeafWithoutMutation(t *testing.T) {
	for _, mode := range []os.FileMode{0700, 0755} {
		root := filepath.Join(syntheticParent(t), "existing")
		if err := os.Mkdir(root, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		partial, err := PreparePrivateCaptureRoot(root)
		if partial == nil || !errors.Is(err, ErrUnsafeDataPath) || partial.instance != nil || partial.rootClose != nil || partial.guard != nil {
			t.Fatalf("preexisting empty leaf supplied fresh authority: %v", err)
		}
		captureFixtureCleanup(t, partial)
		if err := partial.Close(); err != nil {
			t.Fatal(err)
		}
		after, err := os.Lstat(root)
		entries, readErr := os.ReadDir(root)
		if err != nil || readErr != nil || !os.SameFile(before, after) || after.Mode().Perm() != mode || len(entries) != 0 {
			t.Fatal("fresh-only refusal modified a preexisting leaf")
		}
	}
}

func TestPrivateCaptureC0RefusesMissingAncestorWithoutCreatingIt(t *testing.T) {
	ancestor := filepath.Join(syntheticParent(t), "missing")
	partial, err := PreparePrivateCaptureRoot(filepath.Join(ancestor, "capture"))
	if !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("missing fixed parent accepted: %v", err)
	}
	if partial != nil {
		captureFixtureCleanup(t, partial)
		if err := partial.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(ancestor); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preparation silently created an unadmitted ancestor")
	}
}

func TestPrivateCaptureC0RefusalDoesNotReleaseExistingPeerLock(t *testing.T) {
	peer, namespace := captureFixture(t)
	partial, err := PreparePrivateCaptureRoot(namespace.Paths.Root)
	if partial == nil || !errors.Is(err, ErrUnsafeDataPath) || partial.instance != nil || partial.rootClose != nil || partial.guard != nil {
		t.Fatalf("existing peer root was opened: %v", err)
	}
	captureFixtureCleanup(t, partial)
	if err := partial.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Guard(); err != nil {
		t.Fatal("refusal closed the existing peer's authority")
	}
	contender, err := AcquirePrivateCaptureRoot(namespace)
	if contender != nil {
		captureFixtureCleanup(t, contender)
		if err := contender.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if !errors.Is(err, ErrInstanceAlreadyRunning) {
		t.Fatal("C0 refusal released the peer's original flock")
	}
}

func TestPrivateCaptureMarkerFIFOAfterNamedCheckDoesNotBlockOpen(t *testing.T) {
	owner, namespace := captureFixture(t)
	before, err := owner.instance.root.Lstat(ownerMarker)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.instance.root.Remove(ownerMarker); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(namespace.Paths.Root, ownerMarker)
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	// No writer exists. A blocking read-only open would wait for one.
	done := make(chan error, 1)
	go func() {
		owner.mu.Lock()
		defer owner.mu.Unlock()
		done <- owner.markerAfterNamed(before)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnsafeDataPath) {
			t.Fatalf("FIFO substitution was not denied: %v", err)
		}
	case <-time.After(5 * time.Second):
		// A regression gets a bounded fixture-only writer to unblock the
		// owned reader. Unknown join still retains its mutex/handles until
		// the separately owned test-process controller terminates the test.
		fd, openErr := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK|noFollowFlag(), 0)
		if openErr == nil {
			checkpoint := &privateCaptureClose{close: func() error { return syscall.Close(fd) }}
			if err := checkpoint.first(); err != nil {
				t.Fatal("fixture writer first close failed")
			}
		}
		select {
		case <-done:
			t.Fatal("marker open blocked on a substituted FIFO")
		case <-time.After(5 * time.Second):
			t.Fatal("substituted marker reader did not acknowledge its join")
		}
	}
}

func TestPrivateCaptureMarkerFIFOIsRejectedBeforeReadingCanary(t *testing.T) {
	owner, namespace := captureFixture(t)
	before, err := owner.instance.root.Lstat(ownerMarker)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.instance.root.Remove(ownerMarker); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(namespace.Paths.Root, ownerMarker)
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	// Keep a raw nonblocking fixture endpoint so no os.File polling wrapper
	// can turn the canary observation into a wait for another writer.
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|noFollowFlag(), 0)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := &privateCaptureClose{close: func() error { return syscall.Close(fd) }}
	t.Cleanup(func() { _ = checkpoint.first() })
	canary := []byte("unvalidated-fixture-canary-data")
	if len(canary) < len(ownerMarkerContent)+1 {
		t.Fatal("fixture canary does not cover a complete erroneous marker read")
	}
	if n, err := syscall.Write(fd, canary); err != nil || n != len(canary) {
		t.Fatal("fixture canary write failed")
	}
	if err := owner.markerAfterNamed(before); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("FIFO supplied marker authority: %v", err)
	}
	body := make([]byte, len(canary)+1)
	n, err := syscall.Read(fd, body)
	if err != nil || n != len(canary) || string(body[:n]) != string(canary) {
		t.Fatal("marker reader consumed an unvalidated FIFO body")
	}
}

func TestPrivateCaptureMarkerRefusesRegularReplacementAfterNamedCheck(t *testing.T) {
	owner, _ := captureFixture(t)
	before, err := owner.instance.root.Lstat(ownerMarker)
	if err != nil {
		t.Fatal(err)
	}
	original, err := owner.instance.root.OpenFile(ownerMarker, os.O_RDONLY|noFollowFlag(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = original.Close() })
	if err := owner.instance.root.Remove(ownerMarker); err != nil {
		t.Fatal(err)
	}
	replacement, err := owner.instance.root.OpenFile(ownerMarker, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag(), 0600)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := &privateCaptureClose{close: replacement.Close}
	t.Cleanup(func() { _ = checkpoint.first() })
	if n, err := replacement.WriteString(ownerMarkerContent); err != nil || n != len(ownerMarkerContent) {
		t.Fatal("replacement control write failed")
	}
	info, err := replacement.Stat()
	if err != nil || !privateFile(info) || info.Size() != before.Size() || os.SameFile(before, info) {
		t.Fatal("replacement control did not preserve shape with a new inode")
	}
	if err := owner.markerAfterNamed(before); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("different regular inode supplied marker authority: %v", err)
	}
}
