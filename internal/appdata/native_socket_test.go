//go:build darwin || linux

package appdata

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func nativeSyntheticPaths(t *testing.T) Paths {
	t.Helper()
	base := "/tmp"
	if runtime.GOOS == "darwin" {
		base = "/private/tmp"
	}
	root, err := os.MkdirTemp(base, "wa-native-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	paths, err := Prepare(root)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestNativeSocketRoundTripAndProtectedWorking(t *testing.T) {
	paths := nativeSyntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	socket, err := ListenNativeSocket(ctx, paths, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if socket.Path() != filepath.Join(paths.WorkingDir, "native.sock") {
		t.Fatal("socket path escaped fixed working endpoint")
	}
	assertMode(t, socket.Path(), 0600)
	if _, raw := socket.Listener().(*net.UnixListener); raw {
		t.Fatal("listener exposed unchecked Unix accept")
	}
	if err := WithWorking(paths, lease, func(root *os.Root) error {
		f, err := root.OpenFile("native-test-state", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString("synthetic")
		return err
	}); err != nil {
		t.Fatalf("working socket refused: %v", err)
	}
	if _, err := Prepare(paths.Root); err != nil {
		t.Fatalf("socket restart preflight=%v", err)
	}
	if other, err := Acquire(paths); !errors.Is(err, ErrInstanceAlreadyRunning) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("socket launch contention=%v", err)
	}
	accepted := make(chan error, 1)
	go func() {
		conn, err := socket.Listener().Accept()
		if err != nil {
			accepted <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		var data [1]byte
		if _, err := io.ReadFull(conn, data[:]); err != nil {
			accepted <- err
			return
		}
		_, err = conn.Write(data[:])
		accepted <- err
	}()
	conn, err := DialNativeSocket(ctx, socket.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte{'N'}); err != nil {
		t.Fatal(err)
	}
	var reply [1]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil || reply[0] != 'N' {
		t.Fatal("native Unix round trip failed")
	}
	if err := <-accepted; err != nil {
		t.Fatalf("accepted current-UID peer=%v", err)
	}
	if err := socket.Close(); err != nil {
		t.Fatal(err)
	}
	if err := socket.Close(); err != nil {
		t.Fatal("close was not idempotent")
	}
	if _, err := os.Lstat(socket.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned socket survived close")
	}
}

func TestNativeSocketStaleOwnedRestartAndOtherSocketRefusal(t *testing.T) {
	for _, kind := range []string{"stale-native", "other-working", "session-native"} {
		t.Run(kind, func(t *testing.T) {
			paths := nativeSyntheticPaths(t)
			path := filepath.Join(paths.WorkingDir, "native.sock")
			if kind == "other-working" {
				path = filepath.Join(paths.WorkingDir, "other.sock")
			}
			if kind == "session-native" {
				path = filepath.Join(paths.SessionDir, "native.sock")
			}
			fixture, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			fixture.SetUnlinkOnClose(false)
			if err := os.Chmod(path, 0600); err != nil {
				fixture.Close()
				t.Fatal(err)
			}
			fixture.Close()
			lease, err := Acquire(paths)
			if kind != "stale-native" {
				if lease != nil {
					lease.Close()
				}
				if !errors.Is(err, ErrUnsafeDataPath) {
					t.Fatalf("unexpected socket accepted=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			socket, err := ListenNativeSocket(context.Background(), paths, lease)
			if err != nil {
				t.Fatalf("stale owned restart=%v", err)
			}
			defer socket.Close()
			assertMode(t, path, 0600)
		})
	}
}

func TestNativeSocketRefusesUnsafeParentAndEndpoint(t *testing.T) {
	for _, kind := range []string{"no-lease", "closed-lease", "working-mode", "working-symlink", "socket-symlink", "regular-file", "tampered-path"} {
		t.Run(kind, func(t *testing.T) {
			paths := nativeSyntheticPaths(t)
			lease, err := Acquire(paths)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			path := filepath.Join(paths.WorkingDir, "native.sock")
			switch kind {
			case "no-lease":
				lease.Close()
				lease = nil
			case "closed-lease":
				lease.Close()
			case "working-mode":
				if err := os.Chmod(paths.WorkingDir, 0755); err != nil {
					t.Fatal(err)
				}
			case "working-symlink":
				outside := filepath.Join(syntheticParent(t), "outside-working")
				if err := os.Mkdir(outside, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(paths.WorkingDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, paths.WorkingDir); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
						t.Error("native bind wrote outside owned working directory")
					}
				})
			case "socket-symlink":
				if err := os.Symlink(filepath.Join(syntheticParent(t), "missing"), path); err != nil {
					t.Fatal(err)
				}
			case "regular-file":
				if err := os.WriteFile(path, []byte("preserve synthetic file"), 0600); err != nil {
					t.Fatal(err)
				}
			case "tampered-path":
				paths.WorkingDir = filepath.Join(syntheticParent(t), "outside")
			}
			socket, err := ListenNativeSocket(context.Background(), paths, lease)
			if socket != nil {
				socket.Close()
				t.Fatal("unsafe endpoint produced a listener")
			}
			if err == nil {
				t.Fatal("unsafe endpoint accepted")
			}
			if kind == "regular-file" {
				if content, err := os.ReadFile(path); err != nil || string(content) != "preserve synthetic file" {
					t.Fatal("regular endpoint file was removed")
				}
			}
		})
	}
}

func TestNativeSocketClosePreservesReplacement(t *testing.T) {
	paths := nativeSyntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	socket, err := ListenNativeSocket(context.Background(), paths, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if err := os.Remove(socket.Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket.Path(), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := socket.Close(); err == nil {
		t.Fatal("replacement cleanup was not refused")
	}
	if content, err := os.ReadFile(socket.Path()); err != nil || string(content) != "replacement" {
		t.Fatal("close removed a replacement inode")
	}
}

func TestNativeSocketCancellationAndLongPath(t *testing.T) {
	paths := nativeSyntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if socket, err := ListenNativeSocket(ctx, paths, lease); socket != nil || err == nil {
		if socket != nil {
			socket.Close()
		}
		t.Fatal("canceled listen succeeded")
	}
	if _, err := os.Lstat(filepath.Join(paths.WorkingDir, "native.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled listen created a socket")
	}
	if conn, err := DialNativeSocket(ctx, filepath.Join(paths.WorkingDir, "native.sock")); conn != nil || err == nil {
		if conn != nil {
			conn.Close()
		}
		t.Fatal("canceled dial succeeded")
	}
	longRoot := filepath.Join(paths.Root, strings.Repeat("long", 30))
	longPaths, err := Prepare(longRoot)
	if err != nil {
		t.Fatal(err)
	}
	longLease, err := Acquire(longPaths)
	if err != nil {
		t.Fatal(err)
	}
	defer longLease.Close()
	if socket, err := ListenNativeSocket(context.Background(), longPaths, longLease); socket != nil || !errors.Is(err, ErrNativeTransportUnavailable) {
		if socket != nil {
			socket.Close()
		}
		t.Fatalf("long path fixed error=%v", err)
	}
}

func TestNativeDialRefusesUnownedPathWithoutCreation(t *testing.T) {
	root := filepath.Join(syntheticParent(t), "unowned")
	path := filepath.Join(root, "working", "native.sock")
	if conn, err := DialNativeSocket(context.Background(), path); conn != nil || err == nil {
		if conn != nil {
			conn.Close()
		}
		t.Fatal("unowned native path accepted")
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dial created or claimed a root")
	}
}

func TestNativeSocketRetainsInstanceUntilClose(t *testing.T) {
	paths := nativeSyntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	socket, err := ListenNativeSocket(context.Background(), paths, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := Acquire(paths); !errors.Is(err, ErrInstanceAlreadyRunning) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("listener released instance exclusion=%v", err)
	}
	if err := socket.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Acquire(paths)
	if err != nil {
		t.Fatalf("closed listener retained instance=%v", err)
	}
	again.Close()
}

func TestNativeSocketConcurrentOperationsPreserveProcessCWD(t *testing.T) {
	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	stopObserving := make(chan struct{})
	observation := make(chan error, 1)
	var group sync.WaitGroup
	defer func() {
		// Test defers precede fixture cleanup, including when setup calls Fatal.
		group.Wait()
		close(stopObserving)
		if err := <-observation; err != nil {
			t.Error(err)
		}
	}()
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopObserving:
				observation <- nil
				return
			case <-ticker.C:
				current, err := os.Getwd()
				if err != nil || current != before {
					observation <- errors.New("native operation changed another goroutine's cwd")
					return
				}
			}
		}
	}()
	errorsSeen := make(chan error, 3)
	for i := 0; i < 3; i++ {
		paths := nativeSyntheticPaths(t)
		lease, err := Acquire(paths)
		if err != nil {
			t.Fatal(err)
		}
		group.Add(1)
		go func() {
			defer group.Done()
			defer lease.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			socket, err := ListenNativeSocket(ctx, paths, lease)
			if err != nil {
				errorsSeen <- err
				return
			}
			defer socket.Close()
			conn, err := DialNativeSocket(ctx, socket.Path())
			if err != nil {
				errorsSeen <- err
				return
			}
			conn.Close()
			canceled, stop := context.WithCancel(ctx)
			stop()
			if other, err := ListenNativeSocket(canceled, paths, lease); err == nil || other != nil {
				if other != nil {
					other.Close()
				}
				errorsSeen <- errors.New("canceled listener accepted")
				return
			}
			if other, err := DialNativeSocket(canceled, socket.Path()); err == nil || other != nil {
				if other != nil {
					other.Close()
				}
				errorsSeen <- errors.New("canceled dial accepted")
			}
		}()
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent native operation=%v", err)
	}
	after, err := os.Getwd()
	if err != nil || after != before {
		t.Fatal("native operations changed process cwd")
	}
}

func TestNativeSocketListenerCloseDrainsBeforeGuardRelease(t *testing.T) {
	paths := nativeSyntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	socket, err := ListenNativeSocket(context.Background(), paths, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	client, err := DialNativeSocket(context.Background(), socket.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := socket.Listener().Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	if err := socket.Listener().Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := Acquire(paths); !errors.Is(err, ErrInstanceAlreadyRunning) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("admission close released drain guard=%v", err)
	}
	accepted.SetWriteDeadline(time.Now().Add(time.Second))
	client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := accepted.Write([]byte{'D'}); err != nil {
		t.Fatalf("listener close killed draining request=%v", err)
	}
	var reply [1]byte
	if _, err := io.ReadFull(client, reply[:]); err != nil || reply[0] != 'D' {
		t.Fatal("draining connection failed after admission close")
	}
	if err := socket.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Acquire(paths)
	if err != nil {
		t.Fatalf("drained close retained instance=%v", err)
	}
	again.Close()
}

func TestNativeSocketAdmissionCapAndReuse(t *testing.T) {
	paths := nativeSyntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socket, err := ListenNativeSocket(ctx, paths, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	accepted := make([]net.Conn, 0, 128)
	defer func() {
		for _, conn := range accepted {
			conn.Close()
		}
	}()
	for i := 0; i < 128; i++ {
		client, err := DialNativeSocket(ctx, socket.Path())
		if err != nil {
			t.Fatalf("within-cap dial %d=%v", i, err)
		}
		conn, err := socket.Listener().Accept()
		client.Close() // Retain server admission without doubling fixture FDs.
		if err != nil {
			t.Fatalf("within-cap accept %d=%v", i, err)
		}
		accepted = append(accepted, conn)
	}
	result := make(chan net.Conn, 1)
	go func() {
		conn, _ := socket.Listener().Accept()
		result <- conn
	}()
	overflow, err := DialNativeSocket(ctx, socket.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer overflow.Close()
	overflow.SetReadDeadline(time.Now().Add(time.Second))
	var data [1]byte
	if _, err := overflow.Read(data[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("over-cap peer was not closed before admission=%v", err)
	}
	select {
	case conn := <-result:
		if conn != nil {
			conn.Close()
		}
		t.Fatal("over-cap peer escaped listener")
	default:
	}
	accepted[0].Close()
	reused, err := DialNativeSocket(ctx, socket.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer reused.Close()
	select {
	case conn := <-result:
		if conn == nil {
			t.Fatal("freed admission slot did not accept")
		}
		conn.Close()
	case <-ctx.Done():
		t.Fatal("freed admission slot did not become reusable")
	}
}

func TestNativeSocketRootAndWorkingReplacementPreserveNewData(t *testing.T) {
	for _, kind := range []string{"root", "working"} {
		t.Run(kind, func(t *testing.T) {
			paths := nativeSyntheticPaths(t)
			lease, err := Acquire(paths)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			socket, err := ListenNativeSocket(context.Background(), paths, lease)
			if err != nil {
				t.Fatal(err)
			}
			defer socket.Close()
			if kind == "root" {
				if err := os.Rename(paths.Root, paths.Root+"-moved"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(paths.Root + "-moved") })
				if _, err := Prepare(paths.Root); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(paths.WorkingDir, paths.WorkingDir+"-moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(paths.WorkingDir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(socket.Path(), []byte("new namespace"), 0600); err != nil {
				t.Fatal(err)
			}
			if conn, err := DialNativeSocket(context.Background(), socket.Path()); conn != nil || err == nil {
				if conn != nil {
					conn.Close()
				}
				t.Fatal("replacement node accepted for dial")
			}
			socket.Close()
			if content, err := os.ReadFile(socket.Path()); err != nil || string(content) != "new namespace" {
				t.Fatal("close changed replacement namespace")
			}
		})
	}
}

func TestNativeSocketCloseTerminatesAcceptedConnections(t *testing.T) {
	paths := nativeSyntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	socket, err := ListenNativeSocket(context.Background(), paths, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	client, err := DialNativeSocket(context.Background(), socket.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := socket.Listener().Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	if err := socket.Close(); err != nil {
		t.Fatal(err)
	}
	client.SetReadDeadline(time.Now().Add(time.Second))
	var data [1]byte
	if _, err := client.Read(data[:]); err == nil {
		t.Fatal("closed socket retained accepted connection")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("close failed to terminate accepted connection")
	}
}
