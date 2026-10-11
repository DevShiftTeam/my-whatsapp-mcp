//go:build darwin

package workerlife

import (
	"encoding/binary"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ACL grants are independent of Darwin mode bits. Query only the fixed
// attrreference header and reject every extended-security payload or error.
func namespaceNoExtendedACL(file *os.File) bool {
	if file == nil {
		return false
	}
	attributes := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	var buffer [12]byte
	_, _, errno := syscall.Syscall6(uintptr(unix.SYS_FGETATTRLIST), file.Fd(), uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 0)
	runtime.KeepAlive(file)
	if errno != 0 {
		return false
	}
	return binary.NativeEndian.Uint32(buffer[0:4]) == 12 && binary.NativeEndian.Uint32(buffer[4:8]) == 8 && binary.NativeEndian.Uint32(buffer[8:12]) == 0
}
