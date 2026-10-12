//go:build linux

package appdata

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Denying unshare must not disable the real native transport. Run the filter in
// a subprocess so it cannot alter the parent suite's syscall policy.
func TestNativeSocketWithUnshareDenied(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestNativeSocketUnshareDeniedChild$", "-test.v")
	command.Env = append(os.Environ(), "WA_TEST_UNSHARE_DENIED=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native transport failed with unshare denied: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "restricted native transport ready") {
		t.Fatalf("child did not exercise restricted transport: %s", output)
	}
}

func TestNativeSocketUnshareDeniedChild(t *testing.T) {
	if os.Getenv("WA_TEST_UNSHARE_DENIED") != "1" {
		t.Skip("subprocess-only seccomp fixture")
	}
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: uint32(unix.SYS_UNSHARE)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	result, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)))
	if errno != 0 || result != 0 {
		t.Fatalf("failed to deny unshare across process threads: result=%d errno=%v", result, errno)
	}
	if err := unix.Unshare(unix.CLONE_FS); !errors.Is(err, unix.EPERM) {
		t.Fatalf("fixture did not deny unshare: %v", err)
	}
	TestNativeSocketRoundTripAndProtectedWorking(t)
	TestNativeSocketClosePreservesReplacement(t)
	TestNativeSocketRootAndWorkingReplacementPreserveNewData(t)
	TestNativeSocketConcurrentOperationsPreserveProcessCWD(t)
	t.Log("restricted native transport ready")
}

func TestNativeSocketPublishesCanonicalLinuxAddress(t *testing.T) {
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
	expected := filepath.Join(paths.WorkingDir, "native.sock")
	if socket.Listener().Addr().String() != expected {
		t.Fatalf("listener address leaked relative/descriptor path: %q", socket.Listener().Addr().String())
	}
	conn, err := DialNativeSocket(ctx, expected)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.RemoteAddr().String() != expected {
		t.Fatalf("client address leaked relative/descriptor path: %q", conn.RemoteAddr().String())
	}
}

func TestNativeSocketOperationsUseRetainedLinuxDirectory(t *testing.T) {
	paths := nativeSyntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	dir, err := os.OpenRoot(paths.WorkingDir)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	moved := paths.WorkingDir + "-retained"
	if err := os.Rename(paths.WorkingDir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths.WorkingDir, 0700); err != nil {
		t.Fatal(err)
	}
	listener, inode, err := bindNativeSocket(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if inode == nil {
		t.Fatal("socket inode not captured")
	}
	if _, err := os.Lstat(filepath.Join(paths.WorkingDir, "native.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("bind touched replacement working namespace")
	}
	conn, err := connectNativeSocket(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	accepted, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	if !currentNativePeer(accepted) {
		t.Fatal("same-UID peer denied")
	}
	if err := removeNativeSocket(dir, inode); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(moved, "native.sock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retained directory socket survived rooted retirement")
	}
	// FileListener must never unlink the descriptor spelling after its FD retires.
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}
