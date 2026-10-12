package appdata

import (
	"errors"
	"os"
)

var ErrLiveRebuildSource = errors.New("live_rebuild_source_disabled")

// LiveRebuildSourceGuardPath is outside the preserved profile. An operator
// installs it before taking the coherent stopped backup. Never remove it as
// an implicit rollback: the copied device evolves after its first connection.
func LiveRebuildSourceGuardPath(root string) string { return root + ".live-rebuild-guard.json" }

func RefuseLiveRebuildSource(root string) error {
	canonical, err := resolveRoot(root)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(LiveRebuildSourceGuardPath(canonical)); !errors.Is(err, os.ErrNotExist) {
		return ErrLiveRebuildSource
	}
	return nil
}

// AcquireExisting obtains the original instance lock without creating or
// rewriting any source file. It also waits for inherited worker guards through
// the same nonblocking flock used by ordinary launches.
func AcquireExisting(paths Paths) (*InstanceLock, error) {
	if !platformSupported() {
		return nil, ErrUnsupportedPlatform
	}
	dir, err := openOwned(paths)
	if err != nil {
		return nil, err
	}
	file, err := dir.OpenFile(instanceLockFile, os.O_RDWR|noFollowFlag(), 0)
	if err != nil {
		dir.Close()
		return nil, ErrUnsafeDataPath
	}
	info, err := file.Stat()
	current, currentErr := dir.Lstat(instanceLockFile)
	if err != nil || currentErr != nil || !privateFile(info) || !os.SameFile(info, current) || !noExtendedACL(file) {
		file.Close()
		dir.Close()
		return nil, ErrUnsafeDataPath
	}
	if err := lockFile(file); err != nil {
		file.Close()
		dir.Close()
		return nil, err
	}
	return &InstanceLock{paths: paths, root: dir, file: file}, nil
}

// AcquireInheritedExisting adopts a duplicate of an operator-supplied source
// guard. The descriptor must already name the original private lock inode. Its
// shared open-file-description may also protect a historical read-only child;
// duplicating that authority never creates a second independent device owner.
func AcquireInheritedExisting(paths Paths, fd int) (*InstanceLock, error) {
	if fd < 3 {
		return nil, ErrLockRequired
	}
	dir, err := openOwned(paths)
	if err != nil {
		return nil, err
	}
	guard, err := duplicateRebuildLockFD(fd)
	if err != nil {
		dir.Close()
		return nil, err
	}
	info, err := guard.Stat()
	named, namedErr := dir.Lstat(instanceLockFile)
	if err != nil || namedErr != nil || !privateFile(info) || !privateFile(named) || !os.SameFile(info, named) || !noExtendedACL(guard) {
		guard.Close()
		dir.Close()
		return nil, ErrUnsafeDataPath
	}
	// A separate open description must already be excluded. Otherwise this
	// would silently turn an unlocked arbitrary descriptor into authority.
	probe, err := dir.OpenFile(instanceLockFile, os.O_RDWR|noFollowFlag(), 0)
	if err != nil {
		guard.Close()
		dir.Close()
		return nil, ErrLockRequired
	}
	probeErr := lockFile(probe)
	probe.Close()
	if !errors.Is(probeErr, ErrInstanceAlreadyRunning) {
		guard.Close()
		dir.Close()
		return nil, ErrLockRequired
	}
	if err := lockFile(guard); err != nil {
		guard.Close()
		dir.Close()
		return nil, err
	}
	return &InstanceLock{paths: paths, root: dir, file: guard}, nil
}
