//go:build darwin || linux

package appdata

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWorkerGuardRetainsSameLockAfterLeaseClose(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	guard, err := lease.WorkerGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	original, err := lease.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	duplicated, err := guard.Stat()
	if err != nil || !os.SameFile(original, duplicated) {
		t.Fatal("guard did not duplicate held lock inode")
	}
	flags, err := unix.FcntlInt(guard.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("guard descriptor was not close-on-exec")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := Acquire(paths); !errors.Is(err, ErrInstanceAlreadyRunning) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("duplicate failed to retain flock: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Acquire(paths)
	if err != nil {
		t.Fatalf("guard close retained lock: %v", err)
	}
	again.Close()
}

func TestWorkerGuardAllowsRetainedPrivateExecutableChild(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	first, err := lease.WorkerGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	// A verified namespace keeps its sealed executable through the worker's
	// lifetime. Requesting the next worker guard must not reject that live copy.
	namespace := filepath.Join(paths.WorkingDir, "native-exec")
	if err := os.Mkdir(namespace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(namespace, "sealed-executable"), []byte("fixture"), 0500); err != nil {
		t.Fatal(err)
	}
	second, err := lease.WorkerGuard()
	if err != nil {
		t.Fatal("retained executable blocked the next worker guard", err)
	}
	defer second.Close()
	original, err := lease.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	duplicated, err := second.Stat()
	if err != nil || !os.SameFile(original, duplicated) {
		t.Fatal("next guard did not duplicate the held lock inode")
	}
	// This change does not widen initial namespace admission or file modes.
	if _, err := Prepare(paths.Root); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatal("Prepare accepted an executable outside its initial tree policy", err)
	}
	if other, err := Acquire(paths); !errors.Is(err, ErrUnsafeDataPath) {
		if other != nil {
			other.Close()
		}
		t.Fatal("Acquire widened its initial tree policy", err)
	}
}

func TestWorkerGuardRefusesMissingClosedAndReplacedLease(t *testing.T) {
	var missing *InstanceLock
	if guard, err := missing.WorkerGuard(); guard != nil || !errors.Is(err, ErrLockRequired) {
		t.Fatalf("nil guard fixed error=%v", err)
	}
	for _, kind := range []string{"closed", "lock-replaced", "root-replaced", "marker-changed", "marker-symlink"} {
		t.Run(kind, func(t *testing.T) {
			paths := syntheticPaths(t)
			lease, err := Acquire(paths)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			expected := ErrUnsafeDataPath
			switch kind {
			case "closed":
				expected = ErrLockRequired
				lease.Close()
			case "lock-replaced":
				if err := os.Remove(filepath.Join(paths.Root, instanceLockFile)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(paths.Root, instanceLockFile), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "root-replaced":
				if err := os.Rename(paths.Root, paths.Root+"-original"); err != nil {
					t.Fatal(err)
				}
				replacement, err := Prepare(paths.Root)
				if err != nil {
					t.Fatal(err)
				}
				other, err := Acquire(replacement)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
			case "marker-changed":
				if err := os.WriteFile(filepath.Join(paths.Root, ownerMarker), []byte("another-owner"), 0600); err != nil {
					t.Fatal(err)
				}
			case "marker-symlink":
				marker := filepath.Join(paths.Root, ownerMarker)
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(paths.Root, "missing"), marker); err != nil {
					t.Fatal(err)
				}
			}
			guard, err := lease.WorkerGuard()
			if guard != nil {
				guard.Close()
				t.Fatal("invalid lease produced a guard")
			}
			if !errors.Is(err, expected) {
				t.Fatalf("guard fixed error=%v", err)
			}
		})
	}
}

func TestWorkerGuardSurvivesParentSIGKILL(t *testing.T) {
	paths := syntheticPaths(t)
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlRead.Close()
	defer controlWrite.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	parent := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWorkerGuardProcessHelper$")
	parent.Env = append(os.Environ(), "WA_INDEXER_GUARD_TEST_PARENT="+paths.Root)
	parent.ExtraFiles = []*os.File{controlRead}
	stdout, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	defer stdoutWrite.Close()
	parent.Stdout = stdoutWrite
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	parentWaited := false
	defer func() {
		if !parentWaited {
			parent.Process.Kill()
			parent.Wait()
		}
	}()
	stdoutWrite.Close()
	controlRead.Close()
	reader := bufio.NewReader(stdout)
	readLine := func(expected string) {
		t.Helper()
		result := make(chan string, 1)
		go func() { line, _ := reader.ReadString('\n'); result <- line }()
		select {
		case line := <-result:
			if line != expected {
				t.Fatal("guard helper did not complete expected stage")
			}
		case <-ctx.Done():
			t.Fatal("guard helper timed out")
		}
	}
	readLine("guard-ready\n")
	if other, err := Acquire(paths); !errors.Is(err, ErrInstanceAlreadyRunning) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("parent guard lock = %v", err)
	}
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitErr := parent.Wait()
	parentWaited = true
	if waitErr == nil {
		t.Fatal("parent was not killed")
	}
	if other, err := Acquire(paths); !errors.Is(err, ErrInstanceAlreadyRunning) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("parent death released descendant guard: %v", err)
	}
	controlWrite.Close()
	readLine("guard-released\n")
	again, err := Acquire(paths)
	if err != nil {
		t.Fatalf("descendant guard release = %v", err)
	}
	again.Close()
}

func TestWorkerGuardProcessHelper(t *testing.T) {
	if os.Getenv("WA_INDEXER_GUARD_TEST_DESCENDANT") == "1" {
		time.AfterFunc(12*time.Second, func() { os.Exit(79) })
		guard := os.NewFile(3, "inherited-instance-guard")
		control := os.NewFile(4, "guard-release-control")
		if guard == nil || control == nil {
			os.Exit(78)
		}
		fmt.Fprintln(os.Stdout, "guard-ready")
		io.Copy(io.Discard, control)
		guard.Close()
		fmt.Fprintln(os.Stdout, "guard-released")
		os.Exit(0)
	}
	root := os.Getenv("WA_INDEXER_GUARD_TEST_PARENT")
	if root == "" {
		return
	}
	paths, err := Prepare(root)
	if err != nil {
		os.Exit(78)
	}
	lease, err := Acquire(paths)
	if err != nil {
		os.Exit(78)
	}
	guard, err := lease.WorkerGuard()
	if err != nil {
		os.Exit(78)
	}
	control := os.NewFile(3, "guard-release-control")
	child := exec.Command(os.Args[0], "-test.run=^TestWorkerGuardProcessHelper$")
	child.Env = append(os.Environ(), "WA_INDEXER_GUARD_TEST_DESCENDANT=1")
	child.ExtraFiles = []*os.File{guard, control}
	child.Stdout = os.Stdout
	if err := child.Start(); err != nil {
		os.Exit(78)
	}
	guard.Close()
	control.Close()
	child.Wait()
	lease.Close()
	os.Exit(0)
}
