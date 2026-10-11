package appdata

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

const nativeMaxConnections = 128

// NativeSocket owns the private local endpoint and its instance-lock guard.
// Stop HTTP admission and drain HTTP requests before Close; Close terminates any
// remaining connections. Close must precede the coordinator's instance release.
type NativeSocket struct {
	mu          sync.Mutex
	path        string
	root        *os.Root
	working     *os.Root
	inode       os.FileInfo
	listener    *net.UnixListener
	guard       *os.File
	lease       *InstanceLock
	connections map[*nativeConnection]struct{}
	closed      bool
	closeErr    error
	closeDone   chan struct{}
	stopContext func() bool
}

// ListenNativeSocket binds only the fixed protected working/native.sock endpoint.
// A live matching instance lease is required to remove a stale owned socket.
func ListenNativeSocket(ctx context.Context, paths Paths, lease *InstanceLock) (*NativeSocket, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, ErrNativeTransportUnavailable
	}
	if lease == nil {
		return nil, ErrLockRequired
	}
	path := filepath.Join(paths.WorkingDir, nativeSocketFile)
	if !validNativePath(path) {
		return nil, ErrNativeTransportUnavailable
	}
	lease.mu.Lock()
	if lease.nativeSocket != nil {
		lease.mu.Unlock()
		return nil, ErrNativeTransportUnavailable
	}
	dir, err := openLeaseRoot(paths, lease)
	if err != nil {
		lease.mu.Unlock()
		return nil, err
	}
	working, err := openNativeWorking(dir)
	if err != nil {
		dir.Close()
		lease.mu.Unlock()
		return nil, err
	}
	guard, err := lease.workerGuardLocked()
	if err != nil {
		working.Close()
		dir.Close()
		lease.mu.Unlock()
		return nil, err
	}
	var listener *net.UnixListener
	var created os.FileInfo
	cleanup := func() {
		if listener != nil {
			listener.Close()
		}
		if created != nil {
			removeCreatedNativeSocket(working, created)
		}
		guard.Close()
		working.Close()
		dir.Close()
		lease.mu.Unlock()
	}
	before, statErr := working.Lstat(nativeSocketFile)
	if statErr == nil {
		if err := verifyNativeSocketNode(working, before); err != nil {
			cleanup()
			return nil, err
		}
		if err := removeNativeSocket(working, before); err != nil {
			cleanup()
			return nil, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		cleanup()
		return nil, ErrUnsafeDataPath
	}
	listener, created, err = bindNativeSocket(ctx, working)
	if err != nil {
		cleanup()
		return nil, ErrNativeTransportUnavailable
	}
	if err := verifyNativeSocketNode(working, created); err != nil {
		cleanup()
		return nil, err
	}
	if ctx.Err() != nil {
		cleanup()
		return nil, ErrNativeTransportUnavailable
	}
	if err := verifyNativeLocation(paths, dir, working); err != nil {
		cleanup()
		return nil, err
	}
	socket := &NativeSocket{path: path, root: dir, working: working, inode: created, listener: listener, guard: guard, lease: lease, connections: make(map[*nativeConnection]struct{}), closeDone: make(chan struct{})}
	lease.nativeSocket = socket
	lease.mu.Unlock()
	socket.mu.Lock()
	socket.stopContext = context.AfterFunc(ctx, func() { socket.Close() })
	socket.mu.Unlock()
	if ctx.Err() != nil {
		socket.Close()
		return nil, ErrNativeTransportUnavailable
	}
	return socket, nil
}

func (socket *NativeSocket) Path() string {
	if socket == nil {
		return ""
	}
	return socket.path
}

// Listener returns a checked listener; callers cannot bypass peer checks with
// AcceptUnix. Closing this listener stops admission but retains the socket guard
// for HTTP draining until NativeSocket.Close.
func (socket *NativeSocket) Listener() net.Listener { return nativeListener{socket: socket} }

func (socket *NativeSocket) Close() error {
	if socket == nil {
		return nil
	}
	socket.mu.Lock()
	if socket.closed {
		done := socket.closeDone
		socket.mu.Unlock()
		<-done
		socket.mu.Lock()
		err := socket.closeErr
		socket.mu.Unlock()
		return err
	}
	socket.closed = true
	if socket.stopContext != nil {
		socket.stopContext()
	}
	connections := make([]*nativeConnection, 0, len(socket.connections))
	for conn := range socket.connections {
		connections = append(connections, conn)
	}
	socket.mu.Unlock()
	// Resource shutdown is unconditional, including on namespace replacement.
	socket.listener.Close()
	for _, conn := range connections {
		conn.Close()
	}
	err := verifyNativeAnchor(socket.root, socket.working)
	if err == nil {
		err = removeNativeSocket(socket.working, socket.inode)
	}
	if socket.working.Close() != nil && err == nil {
		err = ErrNativeTransportUnavailable
	}
	if socket.root.Close() != nil && err == nil {
		err = ErrNativeTransportUnavailable
	}
	if socket.guard.Close() != nil && err == nil {
		err = ErrNativeTransportUnavailable
	}
	socket.lease.mu.Lock()
	if socket.lease.nativeSocket == socket {
		socket.lease.nativeSocket = nil
	}
	socket.lease.mu.Unlock()
	socket.mu.Lock()
	socket.closeErr = err
	close(socket.closeDone)
	socket.mu.Unlock()
	return err
}

type nativeListener struct{ socket *NativeSocket }

func (listener nativeListener) Accept() (net.Conn, error) {
	if listener.socket == nil {
		return nil, ErrNativeTransportUnavailable
	}
	for {
		conn, err := listener.socket.listener.AcceptUnix()
		if err != nil {
			return nil, ErrNativeTransportUnavailable
		}
		if !currentNativePeer(conn) {
			conn.Close()
			continue
		}
		socket := listener.socket
		socket.mu.Lock()
		if socket.closed {
			socket.mu.Unlock()
			conn.Close()
			return nil, ErrNativeTransportUnavailable
		}
		if len(socket.connections) >= nativeMaxConnections {
			socket.mu.Unlock()
			conn.Close()
			continue
		}
		tracked := &nativeConnection{UnixConn: conn, owner: socket}
		socket.connections[tracked] = struct{}{}
		socket.mu.Unlock()
		return tracked, nil
	}
}

func (listener nativeListener) Close() error {
	if listener.socket == nil {
		return nil
	}
	if err := listener.socket.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return ErrNativeTransportUnavailable
	}
	return nil
}

func (listener nativeListener) Addr() net.Addr {
	if listener.socket == nil {
		return &net.UnixAddr{Name: nativeSocketFile, Net: "unix"}
	}
	return listener.socket.listener.Addr()
}

type nativeConnection struct {
	*net.UnixConn
	owner    *NativeSocket
	once     sync.Once
	closeErr error
}

func (conn *nativeConnection) Close() error {
	conn.once.Do(func() {
		conn.closeErr = conn.UnixConn.Close()
		conn.owner.mu.Lock()
		delete(conn.owner.connections, conn)
		conn.owner.mu.Unlock()
	})
	return conn.closeErr
}

// DialNativeSocket reads no configuration or archive state. Its canonical path
// comes only from private native client configuration or a typed app endpoint,
// never HTTP input. Peer identity is checked before returning a connection.
func DialNativeSocket(ctx context.Context, path string) (net.Conn, error) {
	if ctx == nil || ctx.Err() != nil || !validNativePath(path) {
		return nil, ErrNativeTransportUnavailable
	}
	root := filepath.Dir(filepath.Dir(path))
	paths, err := ExistingPaths(root)
	if err != nil {
		return nil, err
	}
	if path != filepath.Join(paths.WorkingDir, nativeSocketFile) {
		return nil, ErrUnsafeDataPath
	}
	dir, err := openNoSymlinkRoot(paths.Root, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if err := verifyOwnership(dir); err != nil {
		return nil, err
	}
	working, err := openNativeWorking(dir)
	if err != nil {
		return nil, err
	}
	defer working.Close()
	before, err := working.Lstat(nativeSocketFile)
	if err != nil {
		return nil, ErrNativeTransportUnavailable
	}
	if err := verifyNativeSocketNode(working, before); err != nil {
		return nil, err
	}
	conn, err := connectNativeSocket(ctx, working)
	if err != nil {
		return nil, ErrNativeTransportUnavailable
	}
	after, err := working.Lstat(nativeSocketFile)
	if err != nil || !os.SameFile(before, after) {
		conn.Close()
		return nil, ErrUnsafeDataPath
	}
	if err := verifyNativeSocketNode(working, after); err != nil {
		conn.Close()
		return nil, err
	}
	if err := verifyNativeAnchor(dir, working); err != nil {
		conn.Close()
		return nil, err
	}
	if err := verifyNativeLocation(paths, dir, working); err != nil {
		conn.Close()
		return nil, err
	}
	if !currentNativePeer(conn) || ctx.Err() != nil {
		conn.Close()
		return nil, ErrNativeTransportUnavailable
	}
	return conn, nil
}

func validNativePath(path string) bool {
	return nativePathLimit() > 0 && utf8.ValidString(path) && len(path) <= nativePathLimit() && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

func privateNativeSocket(info os.FileInfo) bool {
	return info != nil && info.Mode()&os.ModeSocket != 0 && info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 && info.Mode().Perm() == 0600 && ownedByCurrentUser(info) && singleLink(info)
}

// VerifyNativeSocketEntry validates only the fixed native.sock metadata against
// a caller's retained working-directory identity. It neither connects to nor
// removes the node; stale cleanup still requires ListenNativeSocket's lease.
func VerifyNativeSocketEntry(workingPath string, expectedDirectory os.FileInfo) error {
	if !filepath.IsAbs(workingPath) || filepath.Clean(workingPath) != workingPath || !privateDirectory(expectedDirectory) {
		return ErrUnsafeDataPath
	}
	working, err := openNoSymlinkRoot(workingPath, false)
	if err != nil {
		return ErrUnsafeDataPath
	}
	defer working.Close()
	info, err := working.Stat(".")
	if err != nil || !privateDirectory(info) || !os.SameFile(expectedDirectory, info) || verifyNoExtendedACL(working, ".", info) != nil {
		return ErrUnsafeDataPath
	}
	node, err := working.Lstat(nativeSocketFile)
	if err != nil || verifyNativeSocketNode(working, node) != nil {
		return ErrUnsafeDataPath
	}
	// Reject a replaced pathname rather than validating a different namespace
	// from the caller's retained descriptor. Both roots are metadata-only.
	current, err := openNoSymlinkRoot(workingPath, false)
	if err != nil {
		return ErrUnsafeDataPath
	}
	defer current.Close()
	after, err := current.Stat(".")
	if err != nil || !privateDirectory(after) || !os.SameFile(expectedDirectory, after) || verifyNoExtendedACL(current, ".", after) != nil || verifyNativeSocketNode(current, node) != nil {
		return ErrUnsafeDataPath
	}
	return nil
}

func openNativeWorking(dir *os.Root) (*os.Root, error) {
	before, err := dir.Lstat("working")
	if err != nil || !privateDirectory(before) {
		return nil, ErrUnsafeDataPath
	}
	working, err := dir.OpenRoot("working")
	if err != nil {
		return nil, ErrUnsafeDataPath
	}
	opened, openErr := working.Stat(".")
	after, afterErr := dir.Lstat("working")
	if openErr != nil || afterErr != nil || !privateDirectory(after) || !os.SameFile(before, opened) || !os.SameFile(after, opened) {
		working.Close()
		return nil, ErrUnsafeDataPath
	}
	if err := verifyNoExtendedACL(working, ".", opened); err != nil {
		working.Close()
		return nil, err
	}
	return working, nil
}

func verifyNativeAnchor(root, working *os.Root) error {
	if err := verifyOwnership(root); err != nil {
		return err
	}
	before, err := root.Lstat("working")
	opened, openErr := working.Stat(".")
	if err != nil || openErr != nil || !privateDirectory(before) || !os.SameFile(before, opened) {
		return ErrUnsafeDataPath
	}
	return verifyNoExtendedACL(working, ".", opened)
}

func verifyNativeLocation(paths Paths, root, working *os.Root) error {
	current, err := openNoSymlinkRoot(paths.Root, false)
	if err != nil {
		return err
	}
	defer current.Close()
	if err := verifyOwnership(current); err != nil {
		return err
	}
	before, err := root.Stat(".")
	after, afterErr := current.Stat(".")
	if err != nil || afterErr != nil || !os.SameFile(before, after) {
		return ErrUnsafeDataPath
	}
	return verifyNativeAnchor(current, working)
}

func verifyNativeSocketNode(working *os.Root, expected os.FileInfo) error {
	before, err := working.Lstat(nativeSocketFile)
	if err != nil || !privateNativeSocket(before) || !privateNativeSocket(expected) || !os.SameFile(before, expected) || !nativeSocketNoACL(working) {
		return ErrUnsafeDataPath
	}
	after, err := working.Lstat(nativeSocketFile)
	if err != nil || !privateNativeSocket(after) || !os.SameFile(before, after) {
		return ErrUnsafeDataPath
	}
	return nil
}

func removeNativeSocket(working *os.Root, expected os.FileInfo) error {
	if err := verifyNativeSocketNode(working, expected); err != nil {
		return err
	}
	if err := working.Remove(nativeSocketFile); err != nil {
		return ErrNativeTransportUnavailable
	}
	return syncDirectory(working)
}

// Failed bind setup may not yet have chmodded its new socket. Cleanup is limited
// to that observed newly created inode, never an existing stale/replacement node.
func removeCreatedNativeSocket(working *os.Root, expected os.FileInfo) error {
	info, err := working.Lstat(nativeSocketFile)
	if err != nil || expected == nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) || !singleLink(info) || !os.SameFile(info, expected) {
		return ErrUnsafeDataPath
	}
	if err := working.Remove(nativeSocketFile); err != nil {
		return ErrNativeTransportUnavailable
	}
	return syncDirectory(working)
}
