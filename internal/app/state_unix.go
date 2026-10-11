//go:build darwin || linux

package app

import (
	"os"
	"syscall"
)

func stateNoFollow() int { return syscall.O_NOFOLLOW }

func namespaceDeviceMatches(info os.FileInfo, expected uint64) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && expected != 0 && namespaceStatDevice(stat) == expected
}

func privateStateFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && info.Mode().Perm() == 0600 && stat.Uid == uint32(os.Getuid()) && stat.Nlink == 1
}
