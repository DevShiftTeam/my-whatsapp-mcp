//go:build linux

package appdata

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func nativePathLimit() int { return 107 }

func isolateNativeThread(fd int) error {
	if unix.Unshare(unix.CLONE_FS) != nil || unix.Fchdir(fd) != nil {
		return ErrNativeTransportUnavailable
	}
	return nil
}

func nativePeerCurrentUID(fd int) bool {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	return err == nil && cred != nil && cred.Uid == uint32(os.Geteuid())
}

func nativeSocketNoACL(working *os.Root) bool {
	return relativeNativeOperation(working, func(int) error {
		size, err := unix.Lgetxattr(nativeSocketFile, "system.posix_acl_access", nil)
		if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || err == nil && size == 0 {
			return nil
		}
		return ErrUnsafeDataPath
	}) == nil
}
