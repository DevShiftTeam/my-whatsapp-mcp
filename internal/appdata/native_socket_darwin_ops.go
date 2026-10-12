//go:build darwin

package appdata

import (
	"net"
	"os"
	"runtime"
)

func runNativeOperation(file *os.File, use func(int) error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Deliberately never unlock: Go terminates this isolated thread on exit.
		err := isolateNativeThread(int(file.Fd()))
		if err == nil {
			err = use(int(file.Fd()))
		}
		done <- err
	}()
	return <-done
}

func nativeSocketOperationPath(int) (string, error)       { return nativeSocketFile, nil }
func publishNativeSocketAddress(net.Addr, *os.Root) error { return nil }
