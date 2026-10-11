//go:build darwin || linux

package app

import (
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

func privateBundleEntry(info os.FileInfo, directory, executable bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	mode := os.FileMode(0400)
	if directory {
		mode = 0700
	} else if executable {
		mode = 0500
	}
	return ok && stat.Uid == uint32(os.Getuid()) && info.Mode().Perm() == mode && ((directory && info.IsDir()) || (!directory && info.Mode().IsRegular() && stat.Nlink == 1))
}

func duplicateBundleFile(file *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, errBundledAI
	}
	return os.NewFile(uintptr(fd), "verified-bundle-component"), nil
}
