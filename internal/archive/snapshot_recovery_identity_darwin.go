package archive

import (
	"fmt"
	"os"
	"syscall"
)

func snapshotMarkerFileIdentity(info os.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ErrUnsafeDataPath
	}
	return fmt.Sprintf("%x:%x:%x:%x", uint64(stat.Dev), stat.Ino, stat.Ctimespec.Sec, stat.Ctimespec.Nsec), nil
}
