//go:build darwin || linux

package archive

import (
	"os"
	"syscall"
)

func rebuildOwned(path string, directory bool) error {
	st, e := os.Lstat(path)
	if e != nil {
		return ErrUnsafeDataPath
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || st.Mode().Perm()&0077 != 0 || st.Mode()&os.ModeSymlink != 0 || directory != st.IsDir() || (!directory && (!st.Mode().IsRegular() || stat.Nlink != 1)) {
		return ErrUnsafeDataPath
	}
	return nil
}
