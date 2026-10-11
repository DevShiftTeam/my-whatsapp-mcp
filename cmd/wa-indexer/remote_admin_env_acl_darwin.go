//go:build darwin

package main

import (
	"encoding/binary"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin ACL grants bypass private mode bits. As in appdata and media, inspect
// only the fixed attrreference header and refuse any extended ACL contents.
func adminEnvNoACL(file *os.File) bool {
	attributes := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	var buffer [12]byte
	connection, err := file.SyscallConn()
	if err != nil {
		return false
	}
	var errno syscall.Errno
	err = connection.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall6(uintptr(unix.SYS_FGETATTRLIST), fd, uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 0)
	})
	runtime.KeepAlive(file)
	return err == nil && errno == 0 && binary.NativeEndian.Uint32(buffer[0:4]) == 12 && binary.NativeEndian.Uint32(buffer[4:8]) == 8 && binary.NativeEndian.Uint32(buffer[8:12]) == 0
}
