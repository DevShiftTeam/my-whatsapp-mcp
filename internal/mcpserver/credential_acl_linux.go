//go:build linux

package mcpserver

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Refuse both explicit access ACLs and inherited defaults, matching media's
// private-artifact boundary rather than relying on the current ACL mask alone.
func credentialNoExtendedACL(file *os.File) bool {
	connection, err := file.SyscallConn()
	if err != nil {
		return false
	}
	allowed := true
	err = connection.Control(func(fd uintptr) {
		for _, name := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
			size, err := unix.Fgetxattr(int(fd), name, nil)
			if !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) && !(err == nil && size == 0) {
				allowed = false
				return
			}
		}
	})
	return err == nil && allowed
}
