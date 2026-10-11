//go:build encoder_candidate_actual && darwin

package appdata

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// PrivateCaptureNamespace is a receipt for the restricted, empty acceptance
// namespace. Paths is always independently derived from the canonical root.
type PrivateCaptureNamespace struct {
	Paths                                                             Paths
	Device, Inode, WorkingDevice, WorkingInode, LockDevice, LockInode uint64
	UID                                                               uint32
}

// PrivateCaptureRoot owns a genuine instance lock and its same-OFD guard. It
// never accepts an application data tree, repairs a partial preparation, or
// releases its guard after an uncertain first close or namespace acknowledgment.
type PrivateCaptureRoot struct {
	*privateCaptureState
}

// Value copies share the same mutex and close checkpoints; they cannot create
// another ownership domain for the lock's original open-file description.
type privateCaptureState struct {
	mu                               sync.Mutex
	paths                            Paths
	namespace                        PrivateCaptureNamespace
	instance                         *InstanceLock
	parent                           *os.Root
	name                             string
	guard                            *os.File
	rootClose, lockClose, guardClose *privateCaptureClose
	temporary                        []*privateCaptureClose
	operationErr, ackErr             error
	ready, closing                   bool
}

// Each owned descriptor has one first-close checkpoint. A second call returns
// that result without touching a possibly reused descriptor number.
type privateCaptureClose struct {
	close     func() error
	attempted bool
	err       error
}

func (checkpoint *privateCaptureClose) first() error {
	if checkpoint == nil {
		return nil
	}
	if !checkpoint.attempted {
		checkpoint.attempted = true
		if checkpoint.close == nil || checkpoint.close() != nil {
			checkpoint.err = ErrDataUnavailable
		}
	}
	return checkpoint.err
}

func (owner *PrivateCaptureRoot) acknowledge(checkpoint *privateCaptureClose) error {
	wasAttempted := checkpoint == nil || checkpoint.attempted
	err := checkpoint.first()
	if err != nil && (!wasAttempted || owner.ackErr == nil) {
		owner.ackErr = errors.Join(owner.ackErr, err)
	}
	return err
}

func (owner *PrivateCaptureRoot) track(close func() error) *privateCaptureClose {
	// Successful checkpoints need no retained history. Failed or still-owned
	// handles remain bounded by this restricted walk and deny subsequent use.
	kept := owner.temporary[:0]
	for _, checkpoint := range owner.temporary {
		if !checkpoint.attempted || checkpoint.err != nil {
			kept = append(kept, checkpoint)
		}
	}
	owner.temporary = kept
	checkpoint := &privateCaptureClose{close: close}
	owner.temporary = append(owner.temporary, checkpoint)
	return checkpoint
}

func (owner *PrivateCaptureRoot) usable() bool {
	return owner != nil && owner.privateCaptureState != nil && owner.ready && !owner.closing && owner.operationErr == nil && owner.ackErr == nil
}

func (owner *PrivateCaptureRoot) refusal() error {
	if owner.ackErr != nil {
		return owner.ackErr
	}
	if owner.operationErr != nil {
		return owner.operationErr
	}
	return ErrLockRequired
}

func (owner *PrivateCaptureRoot) fail(err error) (*PrivateCaptureRoot, error) {
	owner.operationErr = err
	return owner, errors.Join(err, owner.ackErr)
}

func captureCanonicalRoot(root string) (string, error) {
	if root == "" || len(root) > 4096 {
		return "", ErrUnsafeDataPath
	}
	canonical, err := canonicalRoot(root)
	if err != nil {
		return "", err
	}
	if root != canonical || len(canonical) > 4096 || len(strings.Split(canonical, string(filepath.Separator))) > 256 {
		return "", ErrUnsafeDataPath
	}
	return canonical, nil
}

// walk acknowledges every intermediate root close and retains the final parent
// so the root's named identity can be checked without reopening an absolute path.
func (owner *PrivateCaptureRoot) walk(canonical string, create bool) error {
	current, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return ErrDataUnavailable
	}
	currentClose := owner.track(current.Close)
	components := strings.Split(strings.TrimPrefix(canonical, string(filepath.Separator)), string(filepath.Separator))
	for i, component := range components {
		if create && i == len(components)-1 {
			// C0's final leaf must be born in this operation. A caller's earlier
			// absence observation cannot authorize adopting a peer-created leaf.
			// Refuse EEXIST before any Lstat/OpenRoot/flock of that final leaf.
			if err := current.Mkdir(component, 0700); err != nil {
				if errors.Is(err, os.ErrExist) {
					return ErrUnsafeDataPath
				}
				return ErrDataUnavailable
			}
			if err := owner.syncDirectory(current); err != nil {
				return err
			}
		}
		// Ancestors must already exist; C0 never creates or repairs a parent.
		before, statErr := current.Lstat(component)
		if statErr != nil || before == nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeDataPath
		}
		next, openErr := current.OpenRoot(component)
		if openErr != nil {
			return ErrUnsafeDataPath
		}
		if i == len(components)-1 {
			owner.parent, owner.name = current, component
			owner.instance = &InstanceLock{paths: owner.paths, root: next}
			owner.rootClose = &privateCaptureClose{close: next.Close}
		} else {
			// Register before any following inspection can fail.
			owner.track(next.Close)
		}
		opened, heldErr := next.Stat(".")
		after, afterErr := current.Lstat(component)
		if heldErr != nil || afterErr != nil || after == nil || !after.IsDir() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(after, opened) {
			return ErrUnsafeDataPath
		}
		if i == len(components)-1 {
			return nil
		}
		if err := owner.acknowledge(currentClose); err != nil {
			return err
		}
		current = next
		currentClose = owner.temporary[len(owner.temporary)-1]
	}
	return ErrUnsafeDataPath
}

func captureIdentity(info os.FileInfo) (device, inode uint64, uid uint32, ok bool) {
	if info == nil {
		return 0, 0, 0, false
	}
	stat, valid := info.Sys().(*syscall.Stat_t)
	if !valid || stat.Ino == 0 {
		return 0, 0, 0, false
	}
	return uint64(uint32(stat.Dev)), stat.Ino, stat.Uid, true
}

// Check the canonical name from a fresh filesystem-root anchor as well as the
// retained parent. A moved ancestor must not turn a receipt's path into another
// namespace merely because the original parent descriptor still works.
func (owner *PrivateCaptureRoot) verifyNamedRoot() error {
	current, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return ErrDataUnavailable
	}
	currentClose := owner.track(current.Close)
	for _, component := range strings.Split(strings.TrimPrefix(owner.paths.Root, string(filepath.Separator)), string(filepath.Separator)) {
		before, statErr := current.Lstat(component)
		if statErr != nil || before == nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			owner.acknowledge(currentClose)
			return ErrUnsafeDataPath
		}
		next, openErr := current.OpenRoot(component)
		if openErr != nil {
			owner.acknowledge(currentClose)
			return ErrUnsafeDataPath
		}
		nextClose := owner.track(next.Close)
		held, heldErr := next.Stat(".")
		after, afterErr := current.Lstat(component)
		valid := heldErr == nil && afterErr == nil && after != nil && after.IsDir() && after.Mode()&os.ModeSymlink == 0 && os.SameFile(before, held) && os.SameFile(after, held)
		closeErr := owner.acknowledge(currentClose)
		if !valid || closeErr != nil {
			owner.acknowledge(nextClose)
			if closeErr != nil {
				return closeErr
			}
			return ErrUnsafeDataPath
		}
		current, currentClose = next, nextClose
	}
	fresh, freshErr := current.Stat(".")
	held, heldErr := owner.instance.root.Stat(".")
	closeErr := owner.acknowledge(currentClose)
	if closeErr != nil {
		return closeErr
	}
	if freshErr != nil || heldErr != nil || !privateDirectory(fresh) || !os.SameFile(fresh, held) {
		return ErrUnsafeDataPath
	}
	return nil
}

func (owner *PrivateCaptureRoot) syncDirectory(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		owner.ackErr = ErrDataUnavailable
		return owner.ackErr
	}
	checkpoint := owner.track(file.Close)
	syncErr := file.Sync()
	closeErr := owner.acknowledge(checkpoint)
	if syncErr != nil && owner.ackErr == nil {
		owner.ackErr = ErrDataUnavailable
	}
	if syncErr != nil || closeErr != nil {
		return owner.ackErr
	}
	return nil
}

func (owner *PrivateCaptureRoot) inspectDirectory(root *os.Root, maximum int) ([]os.DirEntry, os.FileInfo, error) {
	file, err := root.Open(".")
	if err != nil {
		return nil, nil, ErrDataUnavailable
	}
	checkpoint := owner.track(file.Close)
	held, statErr := file.Stat()
	entries, readErr := file.ReadDir(maximum + 1)
	after, afterErr := root.Lstat(".")
	valid := statErr == nil && afterErr == nil && privateDirectory(held) && privateDirectory(after) && os.SameFile(held, after) && noExtendedACL(file)
	closeErr := owner.acknowledge(checkpoint)
	if closeErr != nil {
		return nil, nil, closeErr
	}
	if !valid || len(entries) > maximum {
		return nil, nil, ErrUnsafeDataPath
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, nil, ErrDataUnavailable
	}
	return entries, held, nil
}

func (owner *PrivateCaptureRoot) marker() error {
	root := owner.instance.root
	before, err := root.Lstat(ownerMarker)
	if err != nil || !privateFile(before) || before.Size() != int64(len(ownerMarkerContent)) {
		return ErrUnsafeDataPath
	}
	return owner.markerAfterNamed(before)
}

// The named check is preliminary. Open nonblocking, then prove the actual held
// descriptor is the same private regular marker before reading any bytes. A
// FIFO substituted after Lstat must neither block open nor reach a body read.
func (owner *PrivateCaptureRoot) markerAfterNamed(before os.FileInfo) error {
	if !privateFile(before) || before.Size() != int64(len(ownerMarkerContent)) {
		return ErrUnsafeDataPath
	}
	root := owner.instance.root
	file, err := root.OpenFile(ownerMarker, os.O_RDONLY|noFollowFlag()|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ErrUnsafeDataPath
	}
	checkpoint := owner.track(file.Close)
	held, statErr := file.Stat()
	named, namedErr := root.Lstat(ownerMarker)
	validHeld := statErr == nil && namedErr == nil && privateFile(held) && privateFile(named) && held.Size() == int64(len(ownerMarkerContent)) && named.Size() == held.Size() && os.SameFile(before, held) && os.SameFile(named, held) && noExtendedACL(file)
	if !validHeld {
		if err := owner.acknowledge(checkpoint); err != nil {
			return err
		}
		return ErrUnsafeDataPath
	}
	body := make([]byte, len(ownerMarkerContent)+1)
	n, readErr := io.ReadFull(file, body)
	after, afterErr := root.Lstat(ownerMarker)
	heldAfter, heldAfterErr := file.Stat()
	valid := heldAfterErr == nil && afterErr == nil && privateFile(heldAfter) && privateFile(after) && heldAfter.Size() == int64(len(ownerMarkerContent)) && after.Size() == heldAfter.Size() && os.SameFile(before, heldAfter) && os.SameFile(after, heldAfter) && noExtendedACL(file) && n == len(ownerMarkerContent) && errors.Is(readErr, io.ErrUnexpectedEOF) && string(body[:n]) == ownerMarkerContent
	if err := owner.acknowledge(checkpoint); err != nil {
		return err
	}
	if !valid {
		return ErrUnsafeDataPath
	}
	return nil
}

func (owner *PrivateCaptureRoot) acquireLock(create bool, expected *PrivateCaptureNamespace) error {
	flags := os.O_RDWR | noFollowFlag()
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err := owner.instance.root.OpenFile(instanceLockFile, flags, 0600)
	if err != nil {
		return ErrUnsafeDataPath
	}
	owner.instance.file = file
	owner.lockClose = &privateCaptureClose{close: file.Close}
	held, statErr := file.Stat()
	named, nameErr := owner.instance.root.Lstat(instanceLockFile)
	if statErr != nil || nameErr != nil || !privateFile(held) || !privateFile(named) || !os.SameFile(held, named) || held.Size() != 0 || !noExtendedACL(file) {
		return ErrUnsafeDataPath
	}
	if expected != nil {
		// The named precheck cannot authorize an inode reopened after a swap.
		// Bind the actual held descriptor to the sealed receipt before flock.
		device, inode, uid, valid := captureIdentity(held)
		if !valid || device != expected.LockDevice || inode != expected.LockInode || uid != expected.UID {
			return ErrUnsafeDataPath
		}
		if err := owner.checkExpectedNames(*expected); err != nil {
			return err
		}
	}
	if err := lockFile(file); err != nil {
		return err
	}
	guard, err := duplicateLockFile(file)
	if err != nil {
		return err
	}
	owner.guard = guard
	owner.guardClose = &privateCaptureClose{close: guard.Close}
	duplicated, duplicateErr := guard.Stat()
	if duplicateErr != nil || !privateFile(duplicated) || !os.SameFile(held, duplicated) || !noExtendedACL(guard) {
		return ErrUnsafeDataPath
	}
	if err := file.Sync(); err != nil {
		owner.ackErr = ErrDataUnavailable
		return owner.ackErr
	}
	return owner.syncDirectory(owner.instance.root)
}

func (owner *PrivateCaptureRoot) checkExpectedNames(expected PrivateCaptureNamespace) error {
	root, rootErr := owner.instance.root.Stat(".")
	working, workingErr := owner.instance.root.Lstat("working")
	lock, lockErr := owner.instance.root.Lstat(instanceLockFile)
	rootDevice, rootInode, rootUID, rootValid := captureIdentity(root)
	workingDevice, workingInode, workingUID, workingValid := captureIdentity(working)
	lockDevice, lockInode, lockUID, lockValid := captureIdentity(lock)
	if rootErr != nil || workingErr != nil || lockErr != nil || !privateDirectory(root) || !privateDirectory(working) || !privateFile(lock) || !rootValid || !workingValid || !lockValid || rootDevice != expected.Device || rootInode != expected.Inode || workingDevice != expected.WorkingDevice || workingInode != expected.WorkingInode || lockDevice != expected.LockDevice || lockInode != expected.LockInode || rootUID != expected.UID || workingUID != expected.UID || lockUID != expected.UID {
		return ErrUnsafeDataPath
	}
	return nil
}

// validate reads only the marker, lock metadata and eight bounded root entries.
// It never opens a data body or traverses a populated application subtree.
func (owner *PrivateCaptureRoot) validate() (PrivateCaptureNamespace, error) {
	if owner.instance == nil || owner.instance.root == nil || owner.instance.file == nil || owner.guard == nil || owner.parent == nil {
		return PrivateCaptureNamespace{}, ErrLockRequired
	}
	if err := owner.verifyNamedRoot(); err != nil {
		return PrivateCaptureNamespace{}, err
	}
	root := owner.instance.root
	entries, rootInfo, err := owner.inspectDirectory(root, 8)
	if err != nil {
		return PrivateCaptureNamespace{}, err
	}
	namedRoot, rootErr := owner.parent.Lstat(owner.name)
	if rootErr != nil || !privateDirectory(namedRoot) || !os.SameFile(rootInfo, namedRoot) || len(entries) != 8 {
		return PrivateCaptureNamespace{}, ErrUnsafeDataPath
	}
	allowed := map[string]bool{ownerMarker: true, instanceLockFile: true}
	for _, name := range dataDirectories {
		allowed[name] = true
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return PrivateCaptureNamespace{}, ErrUnsafeDataPath
		}
		delete(allowed, entry.Name())
	}
	if len(allowed) != 0 {
		return PrivateCaptureNamespace{}, ErrUnsafeDataPath
	}
	if err := owner.marker(); err != nil {
		return PrivateCaptureNamespace{}, err
	}
	lockInfo, lockErr := owner.instance.file.Stat()
	namedLock, namedErr := root.Lstat(instanceLockFile)
	guardInfo, guardErr := owner.guard.Stat()
	if lockErr != nil || namedErr != nil || guardErr != nil || !privateFile(lockInfo) || !privateFile(namedLock) || !privateFile(guardInfo) || lockInfo.Size() != 0 || !os.SameFile(lockInfo, namedLock) || !os.SameFile(lockInfo, guardInfo) || !noExtendedACL(owner.instance.file) || !noExtendedACL(owner.guard) {
		return PrivateCaptureNamespace{}, ErrUnsafeDataPath
	}
	device, inode, uid, valid := captureIdentity(rootInfo)
	lockDevice, lockInode, lockUID, lockValid := captureIdentity(lockInfo)
	if !valid || !lockValid || device == 0 || device != lockDevice || uid != uint32(os.Geteuid()) || lockUID != uid {
		return PrivateCaptureNamespace{}, ErrUnsafeDataPath
	}
	namespace := PrivateCaptureNamespace{Paths: owner.paths, Device: device, Inode: inode, LockDevice: lockDevice, LockInode: lockInode, UID: uid}
	for _, name := range dataDirectories {
		before, beforeErr := root.Lstat(name)
		if beforeErr != nil || !privateDirectory(before) {
			return PrivateCaptureNamespace{}, ErrUnsafeDataPath
		}
		child, openErr := root.OpenRoot(name)
		if openErr != nil {
			return PrivateCaptureNamespace{}, ErrUnsafeDataPath
		}
		checkpoint := owner.track(child.Close)
		children, info, inspectErr := owner.inspectDirectory(child, 0)
		after, afterErr := root.Lstat(name)
		closeErr := owner.acknowledge(checkpoint)
		if closeErr != nil {
			return PrivateCaptureNamespace{}, closeErr
		}
		if inspectErr != nil {
			return PrivateCaptureNamespace{}, inspectErr
		}
		childDevice, childInode, childUID, childValid := captureIdentity(info)
		if afterErr != nil || !privateDirectory(after) || !os.SameFile(before, info) || !os.SameFile(after, info) || len(children) != 0 || !childValid || childDevice != device || childUID != uid {
			return PrivateCaptureNamespace{}, ErrUnsafeDataPath
		}
		if name == "working" {
			namespace.WorkingDevice, namespace.WorkingInode = childDevice, childInode
		}
	}
	return namespace, nil
}

// PreparePrivateCaptureRoot is C0: atomically create only a fresh private leaf
// beneath existing ancestors. An already existing final leaf is never opened.
// A partial owner accompanies every post-open error and must remain owned until
// its first-close acknowledgment succeeds. No repair or removal is performed.
func PreparePrivateCaptureRoot(root string) (*PrivateCaptureRoot, error) {
	canonical, err := captureCanonicalRoot(root)
	if err != nil {
		return nil, err
	}
	owner := &PrivateCaptureRoot{privateCaptureState: &privateCaptureState{paths: pathsFor(canonical)}}
	if err := owner.walk(canonical, true); err != nil {
		return owner.fail(err)
	}
	entries, _, err := owner.inspectDirectory(owner.instance.root, 0)
	if err != nil || len(entries) != 0 {
		if err == nil {
			err = ErrUnsafeDataPath
		}
		return owner.fail(err)
	}
	// Acquire and duplicate the flock before publishing the ownership marker.
	if err := owner.acquireLock(true, nil); err != nil {
		return owner.fail(err)
	}
	for _, name := range dataDirectories {
		if err := owner.instance.root.Mkdir(name, 0700); err != nil {
			return owner.fail(ErrDataUnavailable)
		}
		if err := owner.syncDirectory(owner.instance.root); err != nil {
			return owner.fail(err)
		}
	}
	marker, err := owner.instance.root.OpenFile(ownerMarker, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag(), 0600)
	if err != nil {
		return owner.fail(ErrDataUnavailable)
	}
	checkpoint := owner.track(marker.Close)
	n, writeErr := marker.WriteString(ownerMarkerContent)
	syncErr := marker.Sync()
	closeErr := owner.acknowledge(checkpoint)
	if writeErr != nil || n != len(ownerMarkerContent) || syncErr != nil || closeErr != nil {
		if owner.ackErr == nil {
			owner.ackErr = ErrDataUnavailable
		}
		return owner.fail(ErrDataUnavailable)
	}
	if err := owner.syncDirectory(owner.instance.root); err != nil {
		return owner.fail(err)
	}
	namespace, err := owner.validate()
	if err != nil {
		return owner.fail(err)
	}
	owner.namespace, owner.ready = namespace, true
	return owner, nil
}

// AcquirePrivateCaptureRoot is C1: acquire only an exact, already prepared empty
// namespace. The caller's receipt is expected metadata, never path authority.
func AcquirePrivateCaptureRoot(expected PrivateCaptureNamespace) (*PrivateCaptureRoot, error) {
	canonical, err := captureCanonicalRoot(expected.Paths.Root)
	if err != nil {
		return nil, err
	}
	if expected.Paths != pathsFor(canonical) || expected.Device == 0 || expected.Inode == 0 || expected.WorkingDevice != expected.Device || expected.WorkingInode == 0 || expected.LockDevice != expected.Device || expected.LockInode == 0 || expected.UID != uint32(os.Geteuid()) {
		return nil, ErrUnsafeDataPath
	}
	owner := &PrivateCaptureRoot{privateCaptureState: &privateCaptureState{paths: pathsFor(canonical)}}
	if err := owner.walk(canonical, false); err != nil {
		return owner.fail(err)
	}
	// Exact identity is required before even attempting the named root's lock.
	if err := owner.checkExpectedNames(expected); err != nil {
		return owner.fail(err)
	}
	if err := owner.acquireLock(false, &expected); err != nil {
		return owner.fail(err)
	}
	actual, err := owner.validate()
	if err != nil {
		return owner.fail(err)
	}
	if actual != expected {
		return owner.fail(ErrUnsafeDataPath)
	}
	owner.namespace, owner.ready = actual, true
	return owner, nil
}

// Namespace validates the held and named empty scope before returning a receipt.
func (owner *PrivateCaptureRoot) Namespace() (PrivateCaptureNamespace, error) {
	if owner == nil || owner.privateCaptureState == nil {
		return PrivateCaptureNamespace{}, ErrLockRequired
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if !owner.usable() {
		return PrivateCaptureNamespace{}, owner.refusal()
	}
	actual, err := owner.validate()
	if err == nil && actual != owner.namespace {
		err = ErrUnsafeDataPath
	}
	if err != nil {
		if owner.ackErr == nil {
			owner.ackErr = err
		}
		return PrivateCaptureNamespace{}, owner.ackErr
	}
	return actual, nil
}

// Guard returns a borrowed same-OFD descriptor. The caller must never close it;
// the producer owns the final close after all borrowers and acknowledgments.
func (owner *PrivateCaptureRoot) Guard() (*os.File, error) {
	if owner == nil || owner.privateCaptureState == nil {
		return nil, ErrLockRequired
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if !owner.usable() {
		return nil, owner.refusal()
	}
	actual, err := owner.validate()
	if err == nil && actual != owner.namespace {
		err = ErrUnsafeDataPath
	}
	if err != nil {
		if owner.ackErr == nil {
			owner.ackErr = err
		}
		return nil, owner.ackErr
	}
	return owner.guard, nil
}

// WithWorkingAcknowledged calls use once while retaining the genuine lease.
// The rooted handle must not escape and use must not call lease-taking methods.
// Pre-callback producer refusal is ackErr; callback denial remains callbackErr.
func (owner *PrivateCaptureRoot) WithWorkingAcknowledged(use func(*os.Root) error) (callbackErr, ackErr error) {
	if owner == nil || owner.privateCaptureState == nil {
		return nil, ErrLockRequired
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if !owner.usable() {
		return nil, owner.refusal()
	}
	owner.instance.mu.Lock()
	defer owner.instance.mu.Unlock()
	before, err := owner.validate()
	if err == nil && before != owner.namespace {
		err = ErrUnsafeDataPath
	}
	if err != nil || use == nil {
		if err == nil {
			err = ErrDataUnavailable
		}
		if owner.ackErr == nil {
			owner.ackErr = err
		}
		return nil, owner.ackErr
	}
	working, err := owner.instance.root.OpenRoot("working")
	if err != nil {
		owner.ackErr = ErrUnsafeDataPath
		return nil, owner.ackErr
	}
	checkpoint := owner.track(working.Close)
	info, statErr := working.Stat(".")
	device, inode, uid, valid := captureIdentity(info)
	if statErr != nil || !privateDirectory(info) || !valid || device != owner.namespace.WorkingDevice || inode != owner.namespace.WorkingInode || uid != owner.namespace.UID {
		owner.ackErr = ErrUnsafeDataPath
		owner.acknowledge(checkpoint)
		return nil, owner.ackErr
	}
	callbackErr = use(working)
	after, validationErr := owner.validate()
	if validationErr == nil && after != owner.namespace {
		validationErr = ErrUnsafeDataPath
	}
	if validationErr != nil && owner.ackErr == nil {
		owner.ackErr = validationErr
	}
	// Do not open another sync handle after an uncertain namespace/close. The
	// existing working handle still receives its first close acknowledgment.
	if owner.ackErr == nil {
		owner.syncDirectory(working)
	}
	owner.acknowledge(checkpoint)
	return callbackErr, owner.ackErr
}

// Close attempts each owned first close once. The guard is last and remains
// held after any earlier uncertainty. A later call cannot erase a first error.
func (owner *PrivateCaptureRoot) Close() error {
	if owner == nil || owner.privateCaptureState == nil {
		return nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	owner.closing = true
	for _, checkpoint := range owner.temporary {
		owner.acknowledge(checkpoint)
	}
	if owner.ackErr != nil {
		return owner.ackErr
	}
	if owner.instance != nil {
		owner.instance.mu.Lock()
		defer owner.instance.mu.Unlock()
		owner.instance.closed = true
	}
	owner.acknowledge(owner.lockClose)
	owner.acknowledge(owner.rootClose)
	if owner.ackErr != nil {
		return owner.ackErr
	}
	owner.acknowledge(owner.guardClose)
	return owner.ackErr
}
