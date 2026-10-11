//go:build darwin || linux

package archive

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openSnapshotMarker(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrUnsafeDataPath
	}
	info, err := f.Stat()
	stat, ok := func() (*syscall.Stat_t, bool) {
		if info == nil {
			return nil, false
		}
		s, ok := info.Sys().(*syscall.Stat_t)
		return s, ok
	}()
	if err != nil || !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || info.Size() > 33 {
		f.Close()
		return nil, ErrUnsafeDataPath
	}
	return f, nil
}
