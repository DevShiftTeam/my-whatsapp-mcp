//go:build darwin || linux

package appdata

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWorkingAcknowledgementPreservesCallbackAndAllCleanupFailures(t *testing.T) {
	callbackFailure := errors.New("callback outcome")
	cleanupFailure := errors.New("cleanup uncertainty")
	var order []string
	step := func(name string, err error) func() error {
		return func() error { order = append(order, name); return err }
	}
	callbackErr, ackErr := finishOwnedDirectory(step("callback", callbackFailure), step("validate", cleanupFailure), step("sync", cleanupFailure), step("owned-close", cleanupFailure), step("parent-close", cleanupFailure))
	if callbackErr != callbackFailure || !errors.Is(ackErr, cleanupFailure) {
		t.Fatalf("callback=%v acknowledgement=%v", callbackErr, ackErr)
	}
	if !reflect.DeepEqual(order, []string{"callback", "validate", "sync", "owned-close", "parent-close"}) {
		t.Fatalf("cleanup order/count = %v", order)
	}
}

func TestWorkingAcknowledgedCallbackFailureStillClosesHandle(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	want := errors.New("callback outcome")
	var borrowed *os.Root
	callbackErr, ackErr := WithWorkingAcknowledged(paths, lease, func(root *os.Root) error { borrowed = root; return want })
	if callbackErr != want || ackErr != nil {
		t.Fatalf("callback=%v acknowledgement=%v", callbackErr, ackErr)
	}
	if _, err := borrowed.Stat("."); err == nil {
		t.Fatal("borrowed root survived acknowledgement")
	}
}

func TestWithWorkingUsesPrivateLeasedDirectoryAndClosesHandle(t *testing.T) {
	paths := syntheticPaths(t)
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var borrowed *os.Root
	if err := WithWorking(paths, lease, func(root *os.Root) error {
		borrowed = root
		file, err := root.OpenFile("state", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = file.WriteString("synthetic private state")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertMode(t, filepath.Join(paths.WorkingDir, "state"), 0600)
	if _, err := borrowed.Open("state"); err == nil {
		t.Fatal("working handle escaped callback lifetime")
	}
	if err := WithWorking(paths, lease, func(root *os.Root) error {
		content, err := root.ReadFile("state")
		if err != nil {
			return err
		}
		if string(content) != "synthetic private state" {
			return errors.New("wrong synthetic state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := WithWorking(paths, lease, func(root *os.Root) error { return errors.New("private path must never escape") }); !errors.Is(err, ErrDataUnavailable) {
		t.Fatalf("callback error = %v", err)
	}
}

func TestWithWorkingRequiresLiveMatchingRoot(t *testing.T) {
	paths := syntheticPaths(t)
	called := false
	use := func(root *os.Root) error { called = true; return nil }
	if err := WithWorking(paths, nil, use); !errors.Is(err, ErrLockRequired) {
		t.Fatalf("nil lease = %v", err)
	}
	lease, err := Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := os.Rename(paths.Root, paths.Root+"-original"); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(paths.Root); err != nil {
		t.Fatal(err)
	}
	if err := WithWorking(paths, lease, use); !errors.Is(err, ErrUnsafeDataPath) {
		t.Fatalf("stale lease = %v", err)
	}
	if called {
		t.Fatal("callback ran without matching authority")
	}
}
