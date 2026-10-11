//go:build darwin || linux

package diskspace

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Walk from a stable filesystem-root descriptor as appdata does. Compare each
// component before and after opening so a concurrent replacement is rejected.
// All later free-space queries use only the retained descriptor, not this path.
func openVerifiedRoot(path string) (*os.File, error) {
	path, err := canonicalRoot(path)
	if err != nil {
		return nil, err
	}
	current, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, ErrUnavailable
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		before, err := current.Lstat(component)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			current.Close()
			return nil, ErrUnsafeRoot
		}
		next, err := current.OpenRoot(component)
		if err != nil {
			current.Close()
			return nil, ErrUnsafeRoot
		}
		opened, openErr := next.Stat(".")
		after, afterErr := current.Lstat(component)
		if openErr != nil || afterErr != nil || !after.IsDir() || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, opened) || !os.SameFile(after, opened) {
			next.Close()
			current.Close()
			return nil, ErrUnsafeRoot
		}
		current.Close()
		current = next
	}
	file, err := current.OpenFile(".", os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	closeErr := current.Close()
	if err != nil || closeErr != nil {
		if file != nil {
			file.Close()
		}
		return nil, ErrUnavailable
	}
	if _, err := verifyRoot(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func verifyRoot(file *os.File) (uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return 0, ErrUnavailable
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0777 != 0700 || uint32(stat.Uid) != uint32(os.Geteuid()) || !noExtendedACL(file) {
		return 0, ErrUnsafeRoot
	}
	return deviceID(stat), nil
}

func queryFilesystem(file *os.File) (filesystemSpace, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &stat); err != nil {
		return filesystemSpace{}, ErrUnavailable
	}
	return filesystemSample(stat)
}
