//go:build darwin || linux

package appdata

import (
	"golang.org/x/sys/unix"
	"os"
)

func duplicateRebuildLockFD(fd int) (*os.File, error) {
	duplicate, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, ErrLockRequired
	}
	return os.NewFile(uintptr(duplicate), instanceLockFile), nil
}
