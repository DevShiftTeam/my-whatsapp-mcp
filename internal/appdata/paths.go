// Package appdata owns private application paths and the application-instance lease.
package appdata

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	ErrUnsafeDataPath             = errors.New("unsafe_data_path")
	ErrInstanceAlreadyRunning     = errors.New("instance_already_running")
	ErrDataUnavailable            = errors.New("data_unavailable")
	ErrLockRequired               = errors.New("instance_lock_required")
	ErrUnsupportedPlatform        = errors.New("unsupported_platform")
	ErrInvalidOwnerURL            = errors.New("invalid_owner_url")
	ErrNativeTransportUnavailable = errors.New("native_transport_unavailable")
)

// Paths is constructed by Prepare or ExistingPaths. Callers must not select
// individual child paths.
type Paths struct {
	Root         string
	ArchiveDir   string
	SessionDir   string
	MediaDir     string
	WorkingDir   string
	ModelDir     string
	ClientDir    string
	RemoteDir    string
	OwnerURLFile string
}

// InstanceLock remains held while archive/session databases and workers are open.
type InstanceLock struct {
	mu              sync.Mutex
	paths           Paths
	root            *os.Root
	file            *os.File
	closed          bool
	nativeSocket    *NativeSocket
	remoteProviders map[RemoteProvider]*RemoteProviderDirectory
}

const (
	ownerMarker        = ".wa-indexer-owner"
	ownerMarkerContent = "wa-indexer-private-data-v1\n"
	instanceLockFile   = ".instance.lock"
	ownerURLFile       = "owner-url"
	nativeSocketFile   = "native.sock"
)

var dataDirectories = []string{"archive", "session", "media", "working", "models", "clients"}

// DefaultRoot uses the OS application-data location, outside the source checkout.
func DefaultRoot() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		return "", ErrDataUnavailable
	}
	return filepath.Join(base, "WA Indexer"), nil
}

func pathsFor(root string) Paths {
	return Paths{Root: root, ArchiveDir: filepath.Join(root, "archive"), SessionDir: filepath.Join(root, "session"), MediaDir: filepath.Join(root, "media"), WorkingDir: filepath.Join(root, "working"), ModelDir: filepath.Join(root, "models"), ClientDir: filepath.Join(root, "clients"), RemoteDir: filepath.Join(root, "remote"), OwnerURLFile: filepath.Join(root, ownerURLFile)}
}

func resolveRoot(root string) (string, error) {
	if root == "" {
		var err error
		root, err = DefaultRoot()
		if err != nil {
			return "", err
		}
	}
	return canonicalRoot(root)
}

// ExistingPaths validates an existing private root and ownership marker without
// creating paths, acquiring a lease, or traversing application data. An empty
// root selects DefaultRoot. The root comes from local application configuration.
func ExistingPaths(root string) (Paths, error) {
	if !platformSupported() {
		return Paths{}, ErrUnsupportedPlatform
	}
	canonical, err := resolveRoot(root)
	if err != nil {
		return Paths{}, err
	}
	dir, err := openNoSymlinkRoot(canonical, false)
	if err != nil {
		return Paths{}, err
	}
	defer dir.Close()
	if err := verifyOwnership(dir); err != nil {
		return Paths{}, err
	}
	return pathsFor(canonical), nil
}

// Prepare claims only a new/empty directory or one with this exact private marker.
// An explicit custom root does not imply that filesystem synchronization is absent.
func Prepare(root string) (Paths, error) {
	checkRoot, checkErr := resolveRoot(root)
	if checkErr != nil {
		return Paths{}, checkErr
	}
	if _, err := os.Lstat(filepath.Join(checkRoot, "archive", "historical-snapshot.json")); !errors.Is(err, os.ErrNotExist) {
		return Paths{}, ErrUnsafeDataPath
	}
	if !platformSupported() {
		return Paths{}, ErrUnsupportedPlatform
	}
	canonical, err := resolveRoot(root)
	if err != nil {
		return Paths{}, err
	}
	dir, err := openNoSymlinkRoot(canonical, true)
	if err != nil {
		return Paths{}, err
	}
	defer dir.Close()
	info, err := dir.Stat(".")
	if err != nil || !info.IsDir() || !ownedByCurrentUser(info) {
		return Paths{}, ErrUnsafeDataPath
	}
	if err := verifyNoExtendedACL(dir, ".", info); err != nil {
		return Paths{}, err
	}
	entries, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return Paths{}, ErrDataUnavailable
	}
	if len(entries) == 0 {
		rootFile, err := dir.Open(".")
		if err != nil {
			return Paths{}, ErrDataUnavailable
		}
		chmodErr := rootFile.Chmod(0700)
		rootCloseErr := rootFile.Close()
		if chmodErr != nil || rootCloseErr != nil {
			return Paths{}, ErrDataUnavailable
		}
		marker, err := dir.OpenFile(ownerMarker, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollowFlag(), 0600)
		if err != nil {
			return Paths{}, ErrDataUnavailable
		}
		if err := marker.Chmod(0600); err != nil {
			marker.Close()
			return Paths{}, ErrDataUnavailable
		}
		_, writeErr := marker.WriteString(ownerMarkerContent)
		syncErr := marker.Sync()
		closeErr := marker.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return Paths{}, ErrDataUnavailable
		}
		if err := syncDirectory(dir); err != nil {
			return Paths{}, err
		}
	}
	if err := verifyOwnership(dir); err != nil {
		return Paths{}, err
	}
	if err := verifyOwnedChildren(dir); err != nil {
		return Paths{}, err
	}
	for _, name := range dataDirectories {
		if err := dir.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return Paths{}, ErrDataUnavailable
		}
	}
	if err := verifyOwnedChildren(dir); err != nil {
		return Paths{}, err
	}
	if err := syncDirectory(dir); err != nil {
		return Paths{}, err
	}
	// Remote state is optional; an unsafe existing subtree must not block local archiving.
	if err := dir.Mkdir("remote", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return Paths{}, ErrDataUnavailable
	}
	if err := syncDirectory(dir); err != nil {
		return Paths{}, err
	}
	return pathsFor(canonical), nil
}

func canonicalRoot(root string) (string, error) {
	if strings.IndexByte(root, 0) >= 0 {
		return "", ErrUnsafeDataPath
	}
	for _, component := range strings.Split(root, string(filepath.Separator)) {
		if component == ".." {
			return "", ErrUnsafeDataPath
		}
	}
	canonical, err := filepath.Abs(root)
	if err != nil {
		return "", ErrUnsafeDataPath
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ErrDataUnavailable
	}
	canonical = filepath.Clean(canonical)
	if canonical == string(filepath.Separator) || canonical == filepath.Clean(home) {
		return "", ErrUnsafeDataPath
	}
	return canonical, nil
}

// Walk from a stable filesystem-root descriptor. Each next directory is opened
// relative to its already-verified parent, avoiding an absolute-path reopen race.
func openNoSymlinkRoot(root string, create bool) (*os.Root, error) {
	if !platformSupported() {
		return nil, ErrUnsupportedPlatform
	}
	current, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, ErrDataUnavailable
	}
	for _, component := range strings.Split(strings.TrimPrefix(root, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		info, err := current.Lstat(component)
		if errors.Is(err, os.ErrNotExist) && create {
			if err := current.Mkdir(component, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				current.Close()
				return nil, ErrDataUnavailable
			}
			info, err = current.Lstat(component)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			current.Close()
			return nil, ErrUnsafeDataPath
		}
		next, err := current.OpenRoot(component)
		if err != nil {
			current.Close()
			return nil, ErrUnsafeDataPath
		}
		opened, openErr := next.Stat(".")
		after, afterErr := current.Lstat(component)
		if openErr != nil || afterErr != nil || !after.IsDir() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, opened) || !os.SameFile(after, opened) {
			next.Close()
			current.Close()
			return nil, ErrUnsafeDataPath
		}
		current.Close()
		current = next
	}
	return current, nil
}

func privateDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm() == 0700 && ownedByCurrentUser(info)
}

func privateFile(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode().Perm() == 0600 && ownedByCurrentUser(info) && singleLink(info)
}

// Installed model components are immutable; writable metadata/partials remain
// 0600. This exception applies only inside the owned models subtree.
func privateModelFile(info os.FileInfo) bool {
	return privateFile(info) || info != nil && info.Mode().IsRegular() && info.Mode().Perm() == 0400 && ownedByCurrentUser(info) && singleLink(info)
}

func verifyOwnership(dir *os.Root) error {
	info, err := dir.Lstat(".")
	if err != nil || !privateDirectory(info) {
		return ErrUnsafeDataPath
	}
	if err := verifyNoExtendedACL(dir, ".", info); err != nil {
		return err
	}
	info, err = dir.Lstat(ownerMarker)
	if err != nil || !privateFile(info) || info.Size() != int64(len(ownerMarkerContent)) {
		return ErrUnsafeDataPath
	}
	marker, err := dir.OpenFile(ownerMarker, os.O_RDONLY|noFollowFlag(), 0)
	if err != nil {
		return ErrUnsafeDataPath
	}
	defer marker.Close()
	opened, err := marker.Stat()
	if err != nil || !os.SameFile(info, opened) || !noExtendedACL(marker) {
		return ErrUnsafeDataPath
	}
	content := make([]byte, len(ownerMarkerContent))
	if n, err := marker.Read(content); err != nil || n != len(content) || string(content) != ownerMarkerContent {
		return ErrUnsafeDataPath
	}
	return nil
}

func verifyOwnedChildren(dir *os.Root) error {
	for _, name := range []string{instanceLockFile, ownerURLFile} {
		info, err := dir.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !privateFile(info) {
			return ErrUnsafeDataPath
		}
		if err := verifyNoExtendedACL(dir, name, info); err != nil {
			return err
		}
	}
	for _, name := range dataDirectories {
		info, err := dir.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !privateDirectory(info) {
			return ErrUnsafeDataPath
		}
		if err := fs.WalkDir(dir.FS(), name, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return ErrUnsafeDataPath
			}
			info, err := dir.Lstat(path)
			allowedFile := privateFile(info)
			if name == "models" {
				allowedFile = privateModelFile(info)
			}
			if path == filepath.Join("working", nativeSocketFile) && privateNativeSocket(info) {
				working, err := openNativeWorking(dir)
				if err != nil {
					return err
				}
				defer working.Close()
				return verifyNativeSocketNode(working, info)
			}
			if err != nil || !(privateDirectory(info) || allowedFile) {
				return ErrUnsafeDataPath
			}
			if err := verifyNoExtendedACL(dir, path, info); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func verifyNoExtendedACL(dir *os.Root, name string, expected os.FileInfo) error {
	switch name {
	case "archive/archive.sqlite", "archive/archive.sqlite-wal", "archive/archive.sqlite-shm",
		"session/session.sqlite", "session/session.sqlite-wal", "session/session.sqlite-shm":
		return verifySQLiteFileACL(dir, name, expected)
	}
	file, err := dir.OpenFile(name, os.O_RDONLY|noFollowFlag(), 0)
	if err != nil {
		return ErrUnsafeDataPath
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(expected, opened) || !noExtendedACL(file) {
		return ErrUnsafeDataPath
	}
	return nil
}

// Closing any extra descriptor for a live SQLite inode releases the process's
// POSIX locks. Validate these leaves through their retained parent instead.
func verifySQLiteFileACL(dir *os.Root, name string, expected os.FileInfo) (result error) {
	if !privateFile(expected) {
		return ErrUnsafeDataPath
	}
	parentName, leaf := filepath.Dir(name), filepath.Base(name)
	beforeParent, err := dir.Lstat(parentName)
	if err != nil || !privateDirectory(beforeParent) {
		return ErrUnsafeDataPath
	}
	parent, err := dir.OpenRoot(parentName)
	if err != nil {
		return ErrUnsafeDataPath
	}
	defer func() {
		if err := parent.Close(); err != nil && result == nil {
			result = ErrDataUnavailable
		}
	}()
	file, err := parent.Open(".")
	if err != nil {
		return ErrUnsafeDataPath
	}
	defer func() {
		if err := file.Close(); err != nil && result == nil {
			result = ErrDataUnavailable
		}
	}()
	heldParent, err := file.Stat()
	namedParent, namedErr := dir.Lstat(parentName)
	if err != nil || namedErr != nil || !privateDirectory(heldParent) || !privateDirectory(namedParent) ||
		!os.SameFile(beforeParent, heldParent) || !os.SameFile(heldParent, namedParent) || !noExtendedACL(file) {
		return ErrUnsafeDataPath
	}
	before, err := parent.Lstat(leaf)
	if err != nil || !privateFile(before) || !os.SameFile(expected, before) || !noExtendedACLAt(file, leaf, before) {
		return ErrUnsafeDataPath
	}
	after, err := parent.Lstat(leaf)
	afterParent, parentErr := dir.Lstat(parentName)
	if err != nil || parentErr != nil || !privateFile(after) || !privateDirectory(afterParent) ||
		!os.SameFile(before, after) || !os.SameFile(heldParent, afterParent) || !noExtendedACL(file) {
		return ErrUnsafeDataPath
	}
	return nil
}

func openOwned(paths Paths) (*os.Root, error) {
	canonical, err := canonicalRoot(paths.Root)
	if err != nil {
		return nil, err
	}
	if paths != pathsFor(canonical) {
		return nil, ErrUnsafeDataPath
	}
	dir, err := openNoSymlinkRoot(canonical, false)
	if err != nil {
		return nil, err
	}
	if err := verifyOwnership(dir); err != nil {
		dir.Close()
		return nil, err
	}
	if err := verifyOwnedChildren(dir); err != nil {
		dir.Close()
		return nil, err
	}
	return dir, nil
}

func syncDirectory(dir *os.Root) error {
	file, err := dir.Open(".")
	if err != nil {
		return ErrDataUnavailable
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return ErrDataUnavailable
	}
	return nil
}

// Acquire takes a nonblocking advisory lock and retains its open descriptor.
func Acquire(paths Paths) (*InstanceLock, error) {
	if !platformSupported() {
		return nil, ErrUnsupportedPlatform
	}
	dir, err := openOwned(paths)
	if err != nil {
		return nil, err
	}
	file, err := dir.OpenFile(instanceLockFile, os.O_RDWR|os.O_CREATE|noFollowFlag(), 0600)
	if err != nil {
		dir.Close()
		return nil, ErrUnsafeDataPath
	}
	info, err := file.Stat()
	current, currentErr := dir.Lstat(instanceLockFile)
	if err != nil || currentErr != nil || !privateFile(info) || !os.SameFile(info, current) {
		file.Close()
		dir.Close()
		return nil, ErrUnsafeDataPath
	}
	if err := lockFile(file); err != nil {
		file.Close()
		dir.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		dir.Close()
		return nil, ErrDataUnavailable
	}
	if err := syncDirectory(dir); err != nil {
		file.Close()
		dir.Close()
		return nil, err
	}
	return &InstanceLock{paths: paths, root: dir, file: file}, nil
}

// Close releases the lock after registered providers have stopped and joined.
// The file stays in place so future owners use the same inode.
func (lease *InstanceLock) Close() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return nil
	}
	if len(lease.remoteProviders) != 0 {
		return ErrRemoteProviderActive
	}
	lease.closed = true
	if lease.file == nil || lease.root == nil {
		return nil
	}
	fileErr := lease.file.Close()
	rootErr := lease.root.Close()
	if fileErr != nil || rootErr != nil {
		return ErrDataUnavailable
	}
	return nil
}

// ResetOwned clears known local data after the coordinator has stopped workers
// and closed all databases. The root, ownership marker, and held lock survive.
// Unrelated root entries are deliberately preserved.
func ResetOwned(paths Paths, lease *InstanceLock) error {
	if _, err := os.Lstat(filepath.Join(paths.ArchiveDir, "historical-snapshot.json")); !errors.Is(err, os.ErrNotExist) {
		return ErrUnsafeDataPath
	}
	if lease == nil {
		return ErrLockRequired
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if len(lease.remoteProviders) != 0 {
		return ErrRemoteProviderActive
	}
	dir, err := openLeaseRoot(paths, lease)
	if err != nil {
		return err
	}
	defer dir.Close()
	// Older roots may not contain optional remote state.
	if _, err := dir.Lstat("remote"); !errors.Is(err, os.ErrNotExist) {
		if err := verifyRemoteTree(dir); err != nil {
			return err
		}
	}
	for _, name := range append(append([]string{}, dataDirectories...), "remote") {
		if err := dir.RemoveAll(name); err != nil {
			return ErrDataUnavailable
		}
		if err := dir.Mkdir(name, 0700); err != nil {
			return ErrDataUnavailable
		}
	}
	if err := dir.Remove(ownerURLFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrDataUnavailable
	}
	if err := verifyOwnedChildren(dir); err != nil {
		return err
	}
	return syncDirectory(dir)
}

// The caller must hold lease.mu across every use of the returned rooted handle.
func openLeaseRoot(paths Paths, lease *InstanceLock) (*os.Root, error) {
	if lease.closed || lease.file == nil || lease.root == nil || lease.paths != paths {
		return nil, ErrLockRequired
	}
	dir, err := openOwned(paths)
	if err != nil {
		return nil, err
	}
	currentRoot, err := dir.Stat(".")
	leaseRoot, leaseErr := lease.root.Stat(".")
	currentLock, lockErr := dir.Lstat(instanceLockFile)
	leaseLock, fileErr := lease.file.Stat()
	if err != nil || leaseErr != nil || lockErr != nil || fileErr != nil || !os.SameFile(currentRoot, leaseRoot) || !os.SameFile(currentLock, leaseLock) {
		dir.Close()
		return nil, ErrUnsafeDataPath
	}
	return dir, nil
}

// WithWorking runs a synchronous operation inside the private working directory
// while retaining the live instance lease. The rooted handle must not escape.
// File writers remain responsible for syncing their content before returning.
func WithWorking(paths Paths, lease *InstanceLock, use func(*os.Root) error) error {
	return withOwnedDirectory(paths, lease, "working", use)
}

// WithWorkingAcknowledged separates the callback outcome from the namespace,
// sync and descriptor-close acknowledgement. Callers must retain ownership on
// acknowledgement failure; closed descriptor numbers must never be retried.
// The callback runs once and its rooted handle must not escape.
func WithWorkingAcknowledged(paths Paths, lease *InstanceLock, use func(*os.Root) error) (callbackErr, acknowledgementErr error) {
	return withOwnedDirectoryAcknowledged(paths, lease, "working", use)
}

// WithClients runs a synchronous operation inside the private client directory
// while retaining the live instance lease. The rooted handle must not escape,
// and callbacks must not call other lease-taking helpers. File writers remain
// responsible for syncing their content before returning.
func WithClients(paths Paths, lease *InstanceLock, use func(*os.Root) error) error {
	return withOwnedDirectory(paths, lease, "clients", use)
}

func withOwnedDirectory(paths Paths, lease *InstanceLock, name string, use func(*os.Root) error) error {
	callbackErr, ackErr := withOwnedDirectoryAcknowledged(paths, lease, name, use)
	if callbackErr != nil {
		return errors.Join(ErrDataUnavailable, ackErr)
	}
	return ackErr
}

func fixedDirectoryClose(err error) error {
	if err != nil {
		return ErrDataUnavailable
	}
	return nil
}

func finishOwnedDirectory(use, validate, sync, closeOwned, closeParent func() error) (callbackErr, acknowledgementErr error) {
	callbackErr = use()
	acknowledgementErr = errors.Join(validate(), sync(), closeOwned(), closeParent())
	return
}

func withOwnedDirectoryAcknowledged(paths Paths, lease *InstanceLock, name string, use func(*os.Root) error) (callbackErr, acknowledgementErr error) {
	if lease == nil {
		return nil, ErrLockRequired
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	dir, err := openLeaseRoot(paths, lease)
	if err != nil {
		return nil, err
	}
	parentOpen := true
	defer func() {
		if parentOpen {
			acknowledgementErr = errors.Join(acknowledgementErr, fixedDirectoryClose(dir.Close()))
		}
	}()
	before, err := dir.Lstat(name)
	if err != nil || !privateDirectory(before) {
		return nil, ErrUnsafeDataPath
	}
	owned, err := dir.OpenRoot(name)
	if err != nil {
		return nil, ErrUnsafeDataPath
	}
	ownedOpen := true
	defer func() {
		if ownedOpen {
			acknowledgementErr = errors.Join(acknowledgementErr, fixedDirectoryClose(owned.Close()))
		}
	}()
	opened, openErr := owned.Stat(".")
	after, afterErr := dir.Lstat(name)
	if openErr != nil || afterErr != nil || !privateDirectory(after) || !os.SameFile(before, opened) || !os.SameFile(after, opened) {
		return nil, ErrUnsafeDataPath
	}
	if err := verifyNoExtendedACL(owned, ".", opened); err != nil {
		return nil, err
	}
	if use == nil {
		return nil, ErrDataUnavailable
	}
	return finishOwnedDirectory(func() error { return use(owned) }, func() error { return verifyOwnedChildren(dir) }, func() error { return syncDirectory(owned) }, func() error { ownedOpen = false; return fixedDirectoryClose(owned.Close()) }, func() error { parentOpen = false; return fixedDirectoryClose(dir.Close()) })
}
