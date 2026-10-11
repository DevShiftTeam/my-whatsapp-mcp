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

func nativePathLimit() int { return 103 }

func isolateNativeThread(fd int) error {
	if unix.PthreadFchdir(fd) != nil {
		return ErrNativeTransportUnavailable
	}
	return nil
}

func nativePeerCurrentUID(fd int) bool {
	// Darwin's sys/ucred.h defines XUCRED_VERSION as 0. x/sys deliberately
	// exposes the returned struct but does not export this SDK constant.
	const xucredVersion = 0
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	return err == nil && cred != nil && cred.Version == xucredVersion && cred.Uid == uint32(os.Geteuid())
}

// A socket descriptor is not its filesystem vnode, and opening a socket node
// fails. Query that node relative to the retained working-directory descriptor.
func nativeSocketNoACL(working *os.Root) bool {
	file, err := working.Open(".")
	if err != nil {
		return false
	}
	defer file.Close()
	name, err := unix.BytePtrFromString(nativeSocketFile)
	if err != nil {
		return false
	}
	attributes := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	var buffer [12]byte
	_, _, errno := syscall.Syscall6(uintptr(unix.SYS_GETATTRLISTAT), file.Fd(), uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), uintptr(unix.FSOPT_NOFOLLOW))
	runtime.KeepAlive(file)
	runtime.KeepAlive(name)
	return errno == 0 && binary.NativeEndian.Uint32(buffer[0:4]) == 12 && binary.NativeEndian.Uint32(buffer[4:8]) == 8 && binary.NativeEndian.Uint32(buffer[8:12]) == 0
}
