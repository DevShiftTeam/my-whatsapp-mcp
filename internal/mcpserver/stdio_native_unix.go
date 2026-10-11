//go:build unix

package mcpserver

import (
	"context"
	"errors"
	"os"
	"runtime"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/unix"
)

func currentNativeStdioFlags() ([2]int, error) {
	var flags [2]int
	for fd := range flags {
		value, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil {
			return flags, ErrConfiguration
		}
		flags[fd] = value
	}
	return flags, nil
}

// Restore and verify both original descriptors after all owned copies close.
// The per-call syscall port never replaces a process-global function.
func restoreNativeStdioFlags(fds [2]uintptr, flags [2]int, fcntl func(uintptr, int, int) (int, error)) error {
	call := func(fd uintptr, command, argument int) (int, error) {
		var value int
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			value, err = fcntl(fd, command, argument)
			if !errors.Is(err, unix.EINTR) {
				break
			}
		}
		return value, err
	}
	var result error
	for index, fd := range fds {
		_, setErr := call(fd, unix.F_SETFL, flags[index])
		readback, getErr := call(fd, unix.F_GETFL, 0)
		matches := readback == flags[index]
		// Darwin can gain irreversible FWASWRITTEN bookkeeping on stdout.
		// Every other flag, and an existing write-history bit, stays exact.
		if runtime.GOOS == "darwin" && index == 1 && readback == flags[index]|0x10000 {
			matches = true
		}
		if setErr != nil || getErr != nil || !matches {
			result = errNativeStdioCleanup
		}
	}
	return result
}

// Native inherited pipes are normally blocking, so the SDK's StdioTransport
// cannot interrupt a full stdout pipe. Duplicate them and register nonblocking
// copies with Go's poller. Dup shares file-status flags: exclusive stdio use is
// required until both copies close and the original flags have been restored.
func nativeStdioTransport(ctx context.Context) (mcp.Transport, func() error, error) {
	flags, err := currentNativeStdioFlags()
	if err != nil {
		return nil, nil, ErrConfiguration
	}
	var files [2]*os.File
	closeOwned := newNativeStdioClose([2]func() error{
		func() error {
			if files[0] != nil {
				return files[0].Close()
			}
			return nil
		},
		func() error {
			if files[1] != nil {
				return files[1].Close()
			}
			return nil
		},
	})
	restoreFlags := func() error {
		return restoreNativeStdioFlags([2]uintptr{0, 1}, flags, unix.FcntlInt)
	}
	fail := func(closeTemporary func() error) (mcp.Transport, func() error, error) {
		closeConstructorCopies := newNativeStdioClose([2]func() error{closeTemporary, closeOwned})
		return nil, nil, rollbackNativeStdio(closeConstructorCopies, restoreFlags)
	}
	for fd := range files {
		duplicate, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			return fail(nil)
		}
		if err := unix.SetNonblock(duplicate, true); err != nil {
			return fail(func() error { return unix.Close(duplicate) })
		}
		files[fd] = os.NewFile(uintptr(duplicate), "mcp-native-pipe")
		if files[fd] == nil {
			return fail(func() error { return unix.Close(duplicate) })
		}
	}
	// os.NewFile can silently fall back when polling is unsupported. Do not
	// promise cancellable stdio for regular files or unsupported descriptors.
	if files[0].SetReadDeadline(time.Time{}) != nil || files[1].SetWriteDeadline(time.Time{}) != nil {
		return fail(nil)
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = closeOwned() })
	// The shared close Once waits for poll-aware Close and interrupted I/O before
	// cleanup restores the original shared file-status flags.
	cleanup := newNativeStdioCleanup(stopCancellation, closeOwned, restoreFlags)
	return &mcp.IOTransport{
		Reader:        &nativeStdioReader{file: files[0], close: closeOwned},
		Writer:        &nativeStdioWriter{ctx: ctx, file: files[1], close: closeOwned},
		MaxLineLength: int(defaultRequestBytes) + 2048,
	}, cleanup, nil
}

type nativeStdioReader struct {
	file  *os.File
	close func() error
}

func (r *nativeStdioReader) Read(data []byte) (int, error) { return r.file.Read(data) }
func (r *nativeStdioReader) Close() error {
	if r.close() != nil {
		return errNativeStdioCleanup
	}
	return nil
}

type nativeStdioWriter struct {
	ctx   context.Context
	file  *os.File
	close func() error
}

func (w *nativeStdioWriter) Write(data []byte) (int, error) {
	if len(data) > defaultResponseBytes+2048 {
		return 0, ErrResponseTooLarge
	}
	if w.ctx.Err() != nil {
		return 0, ErrUnavailable
	}
	deadline := time.Now().Add(adapterRequestTimeout)
	if callerDeadline, ok := w.ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	if err := w.file.SetWriteDeadline(deadline); err != nil {
		return 0, ErrUnavailable
	}
	n, err := w.file.Write(data)
	if err != nil {
		return n, ErrUnavailable
	}
	return n, nil
}

func (w *nativeStdioWriter) Close() error {
	if w.close() != nil {
		return errNativeStdioCleanup
	}
	return nil
}
