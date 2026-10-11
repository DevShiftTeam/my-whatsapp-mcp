//go:build darwin

package appdata

import (
	"encoding/binary"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Darwin ACL entries can grant access independently of mode bits. Query only
// the fixed attrreference header; ACL contents are unnecessary for refusal.
// fgetattrlist and ATTR_CMN_EXTENDED_SECURITY are defined by the macOS SDK.
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

// Read ACL and leaf identity in one no-follow query without opening the leaf.
func noExtendedACLAt(parent *os.File, name string, expected os.FileInfo) bool {
	stat, ok := expected.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	leaf, err := unix.BytePtrFromString(name)
	if err != nil {
		return false
	}
	attributes := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_DEVID | unix.ATTR_CMN_EXTENDED_SECURITY | unix.ATTR_CMN_FILEID}
	var buffer [24]byte
	_, _, errno := syscall.Syscall6(uintptr(unix.SYS_GETATTRLISTAT), parent.Fd(), uintptr(unsafe.Pointer(leaf)), uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), uintptr(unix.FSOPT_NOFOLLOW))
	runtime.KeepAlive(parent)
	runtime.KeepAlive(leaf)
	read := binary.NativeEndian.Uint32
	return errno == 0 && read(buffer[0:4]) == 24 && read(buffer[4:8]) == uint32(stat.Dev) &&
		read(buffer[8:12]) == 16 && read(buffer[12:16]) == 0 && binary.NativeEndian.Uint64(buffer[16:24]) == stat.Ino
}
