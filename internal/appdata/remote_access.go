package appdata

import (
	"errors"
	"io/fs"
	"os"
	"strings"
)

// WithRemoteAccess borrows the owned remote directory under a live lease.
// The root must not escape and the callback must not call lease-taking helpers.
// Remote subtree failures are isolated from ordinary archive operations.
// Destructive provider-state operations must use WithRemoteAccessRemoval.
func WithRemoteAccess(paths Paths, lease *InstanceLock, use func(*os.Root) error) error {
	return withRemoteAccess(paths, lease, use, false)
}

// WithRemoteAccessRemoval refuses destructive mutation while any provider owns state.
func WithRemoteAccessRemoval(paths Paths, lease *InstanceLock, use func(*os.Root) error) error {
	return withRemoteAccess(paths, lease, use, true)
}
func withRemoteAccess(paths Paths, lease *InstanceLock, use func(*os.Root) error, removal bool) error {
	if lease == nil {
		return ErrLockRequired
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if removal && len(lease.remoteProviders) != 0 {
		return ErrRemoteProviderActive
	}
	dir, err := openLeaseRoot(paths, lease)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := verifyRemoteTree(dir, lease); err != nil {
		return err
	}
	before, err := dir.Lstat("remote")
	if err != nil || !privateDirectory(before) {
		return ErrUnsafeDataPath
	}
	remote, err := dir.OpenRoot("remote")
	if err != nil {
		return ErrUnsafeDataPath
	}
	defer remote.Close()
	opened, err := remote.Stat(".")
	after, afterErr := dir.Lstat("remote")
	if err != nil || afterErr != nil || !privateDirectory(opened) || !privateDirectory(after) || !os.SameFile(before, opened) || !os.SameFile(after, opened) {
		return ErrUnsafeDataPath
	}
	if err := verifyNoExtendedACL(remote, ".", opened); err != nil {
		return err
	}
	if use == nil {
		return ErrDataUnavailable
	}
	callbackErr := use(remote)
	current, currentErr := dir.Lstat("remote")
	if currentErr != nil || !os.SameFile(opened, current) {
		return ErrUnsafeDataPath
	}
	if err := verifyRemoteTree(dir, lease); err != nil {
		return err
	}
	if err := syncDirectory(remote); err != nil {
		return err
	}
	return callbackErr
}

func verifyRemoteTree(root *os.Root, leases ...*InstanceLock) error {
	info, err := root.Lstat("remote")
	if errors.Is(err, os.ErrNotExist) {
		return ErrUnsafeDataPath
	}
	if err != nil || !privateDirectory(info) {
		return ErrUnsafeDataPath
	}
	count := 0
	remainingCacheBytes := int64(tailscaleCacheWalkBytes)
	return fs.WalkDir(root.FS(), "remote", func(name string, entry fs.DirEntry, walkErr error) error {
		count++
		if walkErr != nil || count > 4096 || strings.Count(name, "/") > 16 {
			return ErrUnsafeDataPath
		}
		info, err := root.Lstat(name)
		if err != nil || !(privateDirectory(info) || privateFile(info) || tailscalePublicCache(root, name, info, &remainingCacheBytes)) {
			return ErrUnsafeDataPath
		}
		if err := verifyNoExtendedACL(root, name, info); err != nil {
			return err
		}
		// Only a registered fixed subtree may contain SDK-owned atomic writes.
		// Its current private anchor is still checked; inactive trees and direct
		// credential/manifest files retain the full walk. Only the pinned public
		// Tailscale cache schema above has a narrow inactive mode exception.
		if len(leases) > 0 {
			for provider, d := range leases[0].remoteProviders {
				if name == "remote/"+string(provider) {
					if err := d.validateLocked(); err != nil {
						return err
					}
					return fs.SkipDir
				}
			}
		}
		return nil
	})
}

// OpenRemoteAccessFile opens a single private regular file without following a
// symlink. Creation is exclusive; callers sync writes and close the descriptor.
// Use only inside WithRemoteAccess; names are direct children of the root.
func OpenRemoteAccessFile(root *os.Root, name string, create bool) (*os.File, error) {
	if root == nil || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return nil, ErrUnsafeDataPath
	}
	flags := os.O_RDONLY | noFollowFlag()
	var before os.FileInfo
	if create {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL | noFollowFlag()
	} else {
		var err error
		before, err = root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if !privateFile(before) {
			return nil, ErrUnsafeDataPath
		}
	}
	file, err := root.OpenFile(name, flags, 0600)
	if err != nil {
		return nil, ErrDataUnavailable
	}
	opened, err := file.Stat()
	current, currentErr := root.Lstat(name)
	if err != nil || currentErr != nil || !privateFile(opened) || !privateFile(current) || !os.SameFile(current, opened) || (before != nil && !os.SameFile(before, opened)) || !noExtendedACL(file) {
		file.Close()
		return nil, ErrUnsafeDataPath
	}
	return file, nil
}
