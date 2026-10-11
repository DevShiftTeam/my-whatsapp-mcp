//go:build darwin

package workerlife

import (
	"errors"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// Fclonefileat consumes the held source FD; no source pathname is reopened.
func namespaceClone(source, directory *os.File, name string) error {
	if name != "helper" {
		return ErrProtocol
	}
	err := unix.Fclonefileat(int(source.Fd()), int(directory.Fd()), name, unix.CLONE_NOOWNERCOPY)
	runtime.KeepAlive(source)
	runtime.KeepAlive(directory)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EXDEV) {
		return errNamespaceCloneUnsupported
	}
	return err
}

func namespaceStat(file *os.File) (namespaceIdentity, error) {
	var stat unix.Stat_t
	err := unix.Fstat(int(file.Fd()), &stat)
	runtime.KeepAlive(file)
	if err != nil {
		return namespaceIdentity{}, err
	}
	return namespaceStatIdentity(stat), nil
}

func namespaceStatIdentity(stat unix.Stat_t) namespaceIdentity {
	return namespaceIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Mode: uint32(stat.Mode), Owner: stat.Uid, Links: uint64(stat.Nlink), Size: stat.Size, Allocated: namespaceAllocated(stat.Blocks),
		ModifiedSec: int64(stat.Mtim.Sec), ModifiedNano: int64(stat.Mtim.Nsec), ChangedSec: int64(stat.Ctim.Sec), ChangedNano: int64(stat.Ctim.Nsec)}
}
