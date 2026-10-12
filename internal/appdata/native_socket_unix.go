//go:build darwin || linux

package appdata

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func relativeNativeOperation(working *os.Root, use func(int) error) error {
	file, err := working.Open(".")
	if err != nil {
		return ErrNativeTransportUnavailable
	}
	defer file.Close()
	info, err := file.Stat()
	current, currentErr := working.Stat(".")
	if err != nil || currentErr != nil || !privateDirectory(info) || !os.SameFile(info, current) || !noExtendedACL(file) {
		return ErrUnsafeDataPath
	}
	return runNativeOperation(file, use)
}

func newNativeDescriptor() (*os.File, error) {
	// Darwin lacks SOCK_CLOEXEC. Match Go's own socket construction, protecting
	// the socket-to-close-on-exec interval from os/exec inheritance.
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, ErrNativeTransportUnavailable
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		unix.Close(fd)
		return nil, ErrNativeTransportUnavailable
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, ErrNativeTransportUnavailable
	}
	return os.NewFile(uintptr(fd), nativeSocketFile), nil
}

func bindNativeSocket(ctx context.Context, working *os.Root) (*net.UnixListener, os.FileInfo, error) {
	var listener *net.UnixListener
	var created os.FileInfo
	err := relativeNativeOperation(working, func(parentFD int) error {
		if ctx.Err() != nil {
			return ErrNativeTransportUnavailable
		}
		socketPath, err := nativeSocketOperationPath(parentFD)
		if err != nil {
			return err
		}
		file, err := newNativeDescriptor()
		if err != nil {
			return err
		}
		defer file.Close()
		if err := unix.Bind(int(file.Fd()), &unix.SockaddrUnix{Name: socketPath}); err != nil {
			return ErrNativeTransportUnavailable
		}
		created, err = working.Lstat(nativeSocketFile)
		if err != nil || created.Mode()&os.ModeSocket == 0 || !ownedByCurrentUser(created) || !singleLink(created) {
			return ErrUnsafeDataPath
		}
		if err := unix.Fchmodat(parentFD, nativeSocketFile, 0600, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return ErrNativeTransportUnavailable
		}
		protected, err := working.Lstat(nativeSocketFile)
		if err != nil || !privateNativeSocket(protected) || !os.SameFile(created, protected) {
			return ErrUnsafeDataPath
		}
		created = protected
		if err := unix.Listen(int(file.Fd()), nativeMaxConnections); err != nil {
			return ErrNativeTransportUnavailable
		}
		converted, err := net.FileListener(file)
		if err != nil {
			return ErrNativeTransportUnavailable
		}
		var ok bool
		listener, ok = converted.(*net.UnixListener)
		if !ok {
			converted.Close()
			return ErrNativeTransportUnavailable
		}
		listener.SetUnlinkOnClose(false)
		if err := publishNativeSocketAddress(listener.Addr(), working); err != nil {
			listener.Close()
			return err
		}
		return nil
	})
	return listener, created, err
}

func connectNativeSocket(ctx context.Context, working *os.Root) (*net.UnixConn, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var conn *net.UnixConn
	err := relativeNativeOperation(working, func(parentFD int) error {
		if ctx.Err() != nil {
			return ErrNativeTransportUnavailable
		}
		socketPath, err := nativeSocketOperationPath(parentFD)
		if err != nil {
			return err
		}
		file, err := newNativeDescriptor()
		if err != nil {
			return err
		}
		defer file.Close()
		fd := int(file.Fd())
		err = unix.Connect(fd, &unix.SockaddrUnix{Name: socketPath})
		for err != nil && !errors.Is(err, unix.EISCONN) {
			if ctx.Err() != nil {
				return ErrNativeTransportUnavailable
			}
			if !errors.Is(err, unix.EINPROGRESS) && !errors.Is(err, unix.EALREADY) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
				return ErrNativeTransportUnavailable
			}
			poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
			ready, pollErr := unix.Poll(poll, 25)
			if pollErr != nil && !errors.Is(pollErr, unix.EINTR) {
				return ErrNativeTransportUnavailable
			}
			if ready == 0 || pollErr != nil {
				continue
			}
			code, sockErr := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
			if sockErr != nil || code != 0 {
				return ErrNativeTransportUnavailable
			}
			if _, peerErr := unix.Getpeername(fd); peerErr == nil {
				err = nil
				break
			}
			// Linux AF_UNIX backlog exhaustion returns EAGAIN rather than
			// initiating a connection. Poll readiness is not a retry delay:
			// an unconnected descriptor can be writable immediately.
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ErrNativeTransportUnavailable
			case <-timer.C:
			}
			err = unix.Connect(fd, &unix.SockaddrUnix{Name: socketPath})
		}
		if ctx.Err() != nil {
			return ErrNativeTransportUnavailable
		}
		converted, err := net.FileConn(file)
		if err != nil {
			return ErrNativeTransportUnavailable
		}
		var ok bool
		conn, ok = converted.(*net.UnixConn)
		if !ok {
			converted.Close()
			return ErrNativeTransportUnavailable
		}
		if err := publishNativeSocketAddress(conn.RemoteAddr(), working); err != nil {
			conn.Close()
			conn = nil
			return err
		}
		if !currentNativePeer(conn) {
			conn.Close()
			conn = nil
			return ErrNativeTransportUnavailable
		}
		return nil
	})
	return conn, err
}

func currentNativePeer(conn *net.UnixConn) bool {
	if conn == nil {
		return false
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	allowed := false
	err = raw.Control(func(fd uintptr) { allowed = nativePeerCurrentUID(int(fd)) })
	return err == nil && allowed
}
