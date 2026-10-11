//go:build linux

package diskspace

import (
	"os"

	"golang.org/x/sys/unix"
)

// POSIX access ACL grants obey the checked 0700 group-class mask. Richer ACL
// filesystems can expose named grants independently of mode bits, so unknown
// filesystem types fail closed instead of assuming POSIX semantics.
func noExtendedACL(file *os.File) bool {
	var stat unix.Statfs_t
	return unix.Fstatfs(int(file.Fd()), &stat) == nil && posixMaskFilesystem(uint64(uint32(stat.Type)))
}
