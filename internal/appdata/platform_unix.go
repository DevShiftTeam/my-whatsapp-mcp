//go:build darwin || linux

package appdata

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func platformSupported() bool { return true }
func noFollowFlag() int       { return unix.O_NOFOLLOW }
func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
func singleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func lockFile(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return ErrInstanceAlreadyRunning
		}
		return ErrDataUnavailable
	}
	return nil
}

func duplicateLockFile(file *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, ErrDataUnavailable
	}
	return os.NewFile(uintptr(fd), instanceLockFile), nil
}
