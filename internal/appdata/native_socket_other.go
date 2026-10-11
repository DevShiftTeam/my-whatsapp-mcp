//go:build !darwin && !linux

package appdata

import (
	"context"
	"net"
	"os"
)

func nativePathLimit() int                 { return 0 }
func nativeSocketNoACL(*os.Root) bool      { return false }
func currentNativePeer(*net.UnixConn) bool { return false }
func bindNativeSocket(context.Context, *os.Root) (*net.UnixListener, os.FileInfo, error) {
	return nil, nil, ErrNativeTransportUnavailable
}
func connectNativeSocket(context.Context, *os.Root) (*net.UnixConn, error) {
	return nil, ErrNativeTransportUnavailable
}
