//go:build darwin

package remoteaccess

import (
	"encoding/binary"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Ordinary macOS ancestors can have deny-delete ACLs. Accept deny entries only;
// no permit ACE may widen ancestor access or inherit a grant into a child.
func cloudflareSafeACL(file *os.File) bool {
	// Fixed kauth_filesec/kauth_ace sizes and KAUTH_ACL_MAX_ENTRIES bound reads.
	var buffer [12 + 44 + 24*128]byte
	attributes := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	_, _, errno := syscall.Syscall6(uintptr(unix.SYS_FGETATTRLIST), file.Fd(), uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 0)
	runtime.KeepAlive(file)
	if errno != 0 {
		return false
	}
	read := binary.NativeEndian.Uint32
	length, offset, aclLength := read(buffer[0:4]), read(buffer[4:8]), read(buffer[8:12])
	if offset != 8 || length < 12 || uint64(length) > uint64(len(buffer)) || uint64(aclLength)+12 != uint64(length) {
		return false
	}
	if aclLength == 0 {
		return true
	}
	data := buffer[12:length]
	if len(data) < 44 || read(data[0:4]) != 0x012cc16d {
		return false
	}
	entries := read(data[36:40])
	if entries == 0xffffffff {
		return len(data) == 44
	}
	if entries > 128 || uint64(len(data)) != 44+24*uint64(entries) {
		return false
	}
	for entry := uint32(0); entry < entries; entry++ {
		start := 44 + 24*entry
		if read(data[start+16:start+20])&0xf != 2 { // KAUTH_ACE_DENY
			return false
		}
	}
	return true
}
