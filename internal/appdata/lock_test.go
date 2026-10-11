//go:build darwin || linux

package appdata

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestLockRejectsSecondProcessAndReopensAfterRelease(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	assertMode(t, filepath.Join(paths.Root, ".instance.lock"), 0600)
	_, err = Acquire(paths)
	if !errors.Is(err, ErrInstanceAlreadyRunning) {
		t.Fatalf("second acquisition = %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockProcessHelper$")
	cmd.Env = append(os.Environ(), "WA_INDEXER_LOCK_TEST_ROOT="+paths.Root)
	if err := cmd.Run(); err == nil {
		t.Fatal("second process acquired held lock")
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 77 {
			t.Fatalf("helper = %v", err)
		}
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Acquire(paths)
	if err != nil {
		t.Fatalf("after release = %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal("Close was not idempotent")
	}
}

func TestLockProcessHelper(t *testing.T) {
	root := os.Getenv("WA_INDEXER_LOCK_TEST_ROOT")
	if root == "" {
		return
	}
	paths, err := Prepare(root)
	if err != nil {
		os.Exit(78)
	}
	lease, err := Acquire(paths)
	if errors.Is(err, ErrInstanceAlreadyRunning) {
		os.Exit(77)
	}
	if err != nil {
		os.Exit(78)
	}
	if os.Getenv("WA_INDEXER_LOCK_TEST_HOLD") == "1" {
		fmt.Fprintln(os.Stdout, "ready")
		io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	lease.Close()
	os.Exit(0)
}

func TestProcessTerminationReleasesDescriptorLock(t *testing.T) {
	paths := syntheticPaths(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockProcessHelper$")
	cmd.Env = append(os.Environ(), "WA_INDEXER_LOCK_TEST_ROOT="+paths.Root, "WA_INDEXER_LOCK_TEST_HOLD=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatal("helper did not acquire lock")
	}
	if _, err := Acquire(paths); !errors.Is(err, ErrInstanceAlreadyRunning) {
		t.Fatalf("helper lock = %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("helper was not terminated")
	}
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatalf("terminated process retained lock: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentResetAndClosePreserveLeaseLifecycle(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	group.Add(2)
	results := make(chan error, 2)
	go func() { defer group.Done(); results <- ResetOwned(paths, lease) }()
	go func() { defer group.Done(); results <- lease.Close() }()
	group.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, ErrLockRequired) {
			t.Fatalf("concurrent lifecycle = %v", err)
		}
	}
	again, err := Acquire(paths)
	if err != nil {
		t.Fatalf("closed lock remains held: %v", err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireRejectsTamperedChildPath(t *testing.T) {
	paths := syntheticPaths(t)
	paths.ArchiveDir = filepath.Join(syntheticParent(t), "outside")
	if _, err := Acquire(paths); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("error = %v", err)
	}
}

func TestAcquireRejectsLockSymlinkAndUnsafePermissions(t *testing.T) {
	for _, kind := range []string{"symlink", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			paths := syntheticPaths(t)
			lock := filepath.Join(paths.Root, ".instance.lock")
			if kind == "symlink" {
				if err := os.Symlink(filepath.Join(syntheticParent(t), "missing"), lock); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(lock, nil, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Acquire(paths); !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPrivateOwnershipRefusesHardlinks(t *testing.T) {
	for _, kind := range []string{"marker", "lock", "owner-url", "archive-data"} {
		t.Run(kind, func(t *testing.T) {
			paths := syntheticPaths(t)
			outside := filepath.Join(filepath.Dir(paths.Root), "outside-hardlink")
			content := "outside synthetic data"
			var target string
			switch kind {
			case "marker":
				target = filepath.Join(paths.Root, ".wa-indexer-owner")
				content = "wa-indexer-private-data-v1\n"
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
			case "lock":
				target = filepath.Join(paths.Root, ".instance.lock")
			case "owner-url":
				target = paths.OwnerURLFile
			case "archive-data":
				target = filepath.Join(paths.ArchiveDir, "synthetic-data")
			}
			if err := os.WriteFile(outside, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(outside, target); err != nil {
				t.Fatal(err)
			}
			if _, err := Acquire(paths); !errors.Is(err, ErrUnsafeDataPath) {
				t.Fatalf("hardlink acquisition = %v", err)
			}
			got, err := os.ReadFile(outside)
			if err != nil || string(got) != content {
				t.Fatal("outside hardlink content changed")
			}
		})
	}
}
