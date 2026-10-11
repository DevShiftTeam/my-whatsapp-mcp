//go:build darwin || linux

package workerlife

import (
	"errors"
	"io"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func namespaceNewAnchor(directory *os.File, path string) (*namespaceAnchor, error) {
	file, err := namespaceDuplicate(directory)
	if err != nil {
		return nil, ErrProtocol
	}
	identity, err := namespaceStat(file)
	if err != nil || !namespacePrivateDirectory(identity) || !namespaceNoExtendedACL(file) {
		_ = file.Close()
		return nil, ErrProtocol
	}
	anchor := &namespaceAnchor{file: file, path: path, identity: identity}
	if err := namespaceVerifyAnchor(anchor); err != nil {
		_ = file.Close()
		return nil, ErrProtocol
	}
	return anchor, nil
}

func namespacePrivateDirectory(identity namespaceIdentity) bool {
	return identity.Mode&0170000 == 0040000 && identity.Mode&07777 == 0700 && identity.Owner == uint32(os.Geteuid())
}

func namespaceVerifyAnchor(anchor *namespaceAnchor) error {
	if anchor == nil || anchor.file == nil {
		return ErrCleanup
	}
	held, err := namespaceStat(anchor.file)
	if err != nil || !namespacePrivateDirectory(held) || held.Device != anchor.identity.Device || held.Inode != anchor.identity.Inode || !namespaceNoExtendedACL(anchor.file) {
		return ErrCleanup
	}
	named, err := os.Lstat(anchor.path)
	if err != nil || !named.IsDir() || named.Mode()&os.ModeSymlink != 0 || named.Mode().Perm() != 0700 {
		return ErrCleanup
	}
	opened, err := anchor.file.Stat()
	if err != nil || !os.SameFile(opened, named) {
		return ErrCleanup
	}
	return nil
}

func namespaceDuplicate(file *os.File) (*os.File, error) {
	if file == nil {
		return nil, ErrProtocol
	}
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	runtime.KeepAlive(file)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "workerlife-owned-descriptor"), nil
}

func namespaceCreateDirectory(parent *os.File, name string) (*os.File, bool, error) {
	if _, ok := namespaceToken(name); !ok {
		return nil, false, ErrProtocol
	}
	identity, err := namespaceStat(parent)
	if err != nil || !namespacePrivateDirectory(identity) || !namespaceNoExtendedACL(parent) {
		return nil, false, ErrCleanup
	}
	if err := unix.Mkdirat(int(parent.Fd()), name, 0700); err != nil {
		runtime.KeepAlive(parent)
		return nil, false, err
	}
	file, err := namespaceOpenDirectory(parent, name)
	runtime.KeepAlive(parent)
	return file, true, err
}

func namespaceOpenDirectory(parent *os.File, name string) (*os.File, error) {
	if _, ok := namespaceToken(name); !ok {
		return nil, ErrProtocol
	}
	parentIdentity, err := namespaceStat(parent)
	if err != nil || !namespacePrivateDirectory(parentIdentity) || !namespaceNoExtendedACL(parent) {
		return nil, ErrCleanup
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	runtime.KeepAlive(parent)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "workerlife-directory")
	identity, err := namespaceStat(file)
	if err != nil || identity.Device != parentIdentity.Device || !namespacePrivateDirectory(identity) || !namespaceNoExtendedACL(file) {
		_ = file.Close()
		return nil, ErrCleanup
	}
	return file, nil
}

func namespaceOpenFile(directory *os.File, name string, create bool) (*os.File, error) {
	if name != "manifest.json" && name != "helper" {
		return nil, ErrProtocol
	}
	directoryIdentity, err := namespaceStat(directory)
	if err != nil || !namespacePrivateDirectory(directoryIdentity) || !namespaceNoExtendedACL(directory) {
		return nil, ErrCleanup
	}
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	if create {
		flags = unix.O_RDWR | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	}
	fd, err := unix.Openat(int(directory.Fd()), name, flags, 0600)
	runtime.KeepAlive(directory)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "workerlife-"+name)
	identity, err := namespaceStat(file)
	if err != nil || identity.Device != directoryIdentity.Device || !namespaceOwnedFile(identity, namespaceMaximumExecutable) || (name == "manifest.json" && identity.Mode&07777 != 0600) || !namespaceNoExtendedACL(file) {
		_ = file.Close()
		return nil, ErrCleanup
	}
	return file, nil
}

func namespaceSameDirectory(parent *os.File, name string, expected namespaceIdentity) error {
	parentIdentity, err := namespaceStat(parent)
	if err != nil || !namespacePrivateDirectory(parentIdentity) || expected.Device != parentIdentity.Device || !namespaceNoExtendedACL(parent) {
		return ErrCleanup
	}
	var named unix.Stat_t
	err = unix.Fstatat(int(parent.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(parent)
	if err != nil {
		return ErrCleanup
	}
	identity := namespaceStatIdentity(named)
	if !namespacePrivateDirectory(identity) || identity.Device != expected.Device || identity.Inode != expected.Inode {
		return ErrCleanup
	}
	opened, err := namespaceOpenDirectory(parent, name)
	if err != nil {
		return ErrCleanup
	}
	defer opened.Close()
	held, err := namespaceStat(opened)
	if err != nil || held.Device != expected.Device || held.Inode != expected.Inode {
		return ErrCleanup
	}
	return nil
}

func namespaceSameFile(directory *os.File, name string, expected namespaceIdentity) error {
	if name != "manifest.json" && name != "helper" {
		return ErrProtocol
	}
	directoryIdentity, err := namespaceStat(directory)
	if err != nil || !namespacePrivateDirectory(directoryIdentity) || expected.Device != directoryIdentity.Device || !namespaceNoExtendedACL(directory) {
		return ErrCleanup
	}
	var named unix.Stat_t
	err = unix.Fstatat(int(directory.Fd()), name, &named, unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(directory)
	if err != nil {
		return ErrCleanup
	}
	identity := namespaceStatIdentity(named)
	if !namespaceOwnedFile(identity, namespaceMaximumExecutable) || !expected.stable(identity) {
		return ErrCleanup
	}
	opened, err := namespaceOpenFile(directory, name, false)
	if err != nil {
		return ErrCleanup
	}
	defer opened.Close()
	held, err := namespaceStat(opened)
	if err != nil || !expected.stable(held) {
		return ErrCleanup
	}
	return nil
}

func namespaceRenameDirectory(parent *os.File, from, to string) error {
	if _, ok := namespaceToken(from); !ok {
		return ErrProtocol
	}
	if _, ok := namespaceToken(to); !ok {
		return ErrProtocol
	}
	if !namespaceNoExtendedACL(parent) {
		return ErrCleanup
	}
	var existing unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), to, &existing, unix.AT_SYMLINK_NOFOLLOW)
	if !errors.Is(err, unix.ENOENT) {
		runtime.KeepAlive(parent)
		return ErrCleanup
	}
	err = unix.Renameat(int(parent.Fd()), from, int(parent.Fd()), to)
	runtime.KeepAlive(parent)
	return err
}

func namespaceUnlinkFile(directory *os.File, name string) error {
	if name != "manifest.json" && name != "helper" {
		return ErrProtocol
	}
	if !namespaceNoExtendedACL(directory) {
		return ErrCleanup
	}
	err := unix.Unlinkat(int(directory.Fd()), name, 0)
	runtime.KeepAlive(directory)
	return err
}

func namespaceRemoveDirectory(parent *os.File, name string) error {
	if _, ok := namespaceToken(name); !ok {
		return ErrProtocol
	}
	if !namespaceNoExtendedACL(parent) {
		return ErrCleanup
	}
	err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
	runtime.KeepAlive(parent)
	return err
}

func namespaceNames(directory *os.File, maximum int) ([]string, error) {
	fd, err := unix.Openat(int(directory.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	runtime.KeepAlive(directory)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "workerlife-list")
	defer file.Close()
	names, err := file.Readdirnames(maximum + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(names) > maximum {
		return nil, ErrLimit
	}
	return names, nil
}
