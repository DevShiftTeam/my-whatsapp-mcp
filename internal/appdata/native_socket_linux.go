//go:build linux

package appdata

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

func nativePathLimit() int { return 107 }

// Linux AF_UNIX has no bindat/connectat. Its proc descriptor alias keeps the
// socket lookup anchored to the retained directory without changing cwd or
// requiring unshare, which ordinary container seccomp deliberately denies.
func runNativeOperation(file *os.File, use func(int) error) error {
	return use(int(file.Fd()))
}

func nativeSocketOperationPath(fd int) (string, error) {
	if fd < 0 {
		return "", ErrNativeTransportUnavailable
	}
	anchor := "/proc/self/fd/" + strconv.Itoa(fd)
	var held unix.Stat_t
	if unix.Fstat(fd, &held) != nil || held.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", ErrNativeTransportUnavailable
	}
	var throughProc unix.Stat_t
	if unix.Stat(anchor, &throughProc) != nil || held.Dev != throughProc.Dev || held.Ino != throughProc.Ino || throughProc.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", ErrNativeTransportUnavailable
	}
	path := anchor + "/" + nativeSocketFile
	if len(path) > nativePathLimit() {
		return "", ErrNativeTransportUnavailable
	}
	return path, nil
}

// FD aliases are transient syscall spellings, never public connection paths.
// net.FileListener/FileConn otherwise expose the kernel's proc alias as Addr.
func publishNativeSocketAddress(address net.Addr, working *os.Root) error {
	addr, ok := address.(*net.UnixAddr)
	if !ok || addr == nil || working == nil {
		return ErrNativeTransportUnavailable
	}
	path := filepath.Join(working.Name(), nativeSocketFile)
	if !validNativePath(path) {
		return ErrNativeTransportUnavailable
	}
	addr.Name = path
	return nil
}

func nativePeerCurrentUID(fd int) bool {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	return err == nil && cred != nil && cred.Uid == uint32(os.Geteuid())
}

func nativeSocketNoACL(working *os.Root) bool {
	return relativeNativeOperation(working, func(fd int) error {
		path, err := nativeSocketOperationPath(fd)
		if err != nil {
			return err
		}
		size, err := unix.Lgetxattr(path, "system.posix_acl_access", nil)
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || err == nil && size == 0 {
			return nil
		}
		return ErrUnsafeDataPath
	}) == nil
}
