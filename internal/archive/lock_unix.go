//go:build darwin || linux

package archive

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

// flock belongs to this open file description. Opening/closing another
// descriptor for metadata checks cannot release the held writer lock.
func acquireArchiveLock(ctx context.Context, dir string) (*os.File, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	path := filepath.Join(dir, "archive.lock")
	if e := checkPrivateFile(path); e != nil {
		return nil, e
	}
	fd, e := unix.Open(path, unix.O_CLOEXEC|unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, ErrUnsafeDataPath
	}
	f := os.NewFile(uintptr(fd), path)
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, ErrUnsafeDataPath
	}
	if e = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		if errors.Is(e, unix.EWOULDBLOCK) || errors.Is(e, unix.EAGAIN) {
			return nil, ErrInstanceAlreadyRunning
		}
		return nil, ErrArchiveUnavailable
	}
	return f, nil
}
