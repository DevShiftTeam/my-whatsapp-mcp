//go:build linux

package media

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func absentACL(file *os.File, name string) bool {
	size, err := unix.Fgetxattr(int(file.Fd()), name, nil)
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || (err == nil && size == 0)
}

// Access ACL masks alone cannot detect inherited defaults or all extended ACLs.
// Refuse both access and default ACLs even when Unix mode bits look private.
func noExtendedACL(file *os.File) bool {
	return absentACL(file, "system.posix_acl_access") && absentACL(file, "system.posix_acl_default")
}

func safeAncestorACL(file *os.File) bool { return noExtendedACL(file) }

func renameNoReplace(fd int, old, new string) error {
	return unix.Renameat2(fd, old, fd, new, unix.RENAME_NOREPLACE)
}
