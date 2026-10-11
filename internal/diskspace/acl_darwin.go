//go:build darwin

package diskspace

import (
	"encoding/binary"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin ACLs can independently grant access despite 0700. This matches the
// fail-closed appdata directory check without retaining any ACL contents.
func noExtendedACL(file *os.File) bool {
	attributes := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	var buffer [12]byte
	_, _, errno := syscall.Syscall6(uintptr(unix.SYS_FGETATTRLIST), file.Fd(), uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 0)
	runtime.KeepAlive(file)
	return errno == 0 && binary.NativeEndian.Uint32(buffer[0:4]) == 12 && binary.NativeEndian.Uint32(buffer[4:8]) == 8 && binary.NativeEndian.Uint32(buffer[8:12]) == 0
}
