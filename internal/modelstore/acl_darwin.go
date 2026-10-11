//go:build darwin

package modelstore

import (
	"encoding/binary"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin ACLs can grant access independently of 0700/0600 modes. Refuse any
// extended ACL without retaining its contents; the fixed attrreference header
// reports whether ATTR_CMN_EXTENDED_SECURITY has data.
func noExtendedACL(file *os.File) bool {
	attributes := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	var buffer [12]byte
	_, _, errno := syscall.Syscall6(uintptr(unix.SYS_FGETATTRLIST), file.Fd(), uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 0)
	runtime.KeepAlive(file)
	if errno != 0 {
		return false
	}
	return binary.NativeEndian.Uint32(buffer[0:4]) == 12 && binary.NativeEndian.Uint32(buffer[4:8]) == 8 && binary.NativeEndian.Uint32(buffer[8:12]) == 0
}

// Ordinary macOS ancestors such as the home directory have deny-delete ACLs.
// Preserve those while refusing grants that permit replacing a private child,
// changing ancestor permissions, or inheriting access into created children.
func safeAncestorACL(file *os.File) bool {
	// The SDK's kauth_filesec header is 44 bytes and each kauth_ace is 24 bytes;
	// KAUTH_ACL_MAX_ENTRIES is 128. This bounds metadata reads independently of
	// attacker-controlled ACL length, and no ACL contents escape this helper.
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
	if len(data) < 44 || read(data[0:4]) != 0x012cc16d { // KAUTH_FILESEC_MAGIC
		return false
	}
	entries := read(data[36:40])
	if entries == 0xffffffff { // KAUTH_FILESEC_NOACL
		return len(data) == 44
	}
	if entries > 128 || uint64(len(data)) != 44+24*uint64(entries) {
		return false
	}
	// KAUTH_VNODE_* rights that mutate the inode, permissions, or children;
	// include the generic-all/write aliases used by ACL interfaces.
	const mutating uint32 = 1<<2 | 1<<4 | 1<<5 | 1<<6 | 1<<8 | 1<<10 | 1<<12 | 1<<13 | 1<<21 | 1<<23 | 1<<25 | 1<<26
	for entry := uint32(0); entry < entries; entry++ {
		start := 44 + 24*entry
		flags, rights := read(data[start+16:start+20]), read(data[start+20:start+24])
		kind := flags & 0xf
		if kind != 1 && kind != 2 { // KAUTH_ACE_PERMIT / KAUTH_ACE_DENY
			return false
		}
		if kind == 1 && rights&mutating != 0 {
			return false
		}
	}
	return true
}
