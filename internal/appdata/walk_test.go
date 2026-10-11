//go:build darwin || linux

package appdata

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A pathname reopen after component checks could create the lock in the
// independently owned outside root while an ancestor is replaced by a symlink.
func TestConcurrentAncestorSymlinkCannotRedirectLockCreation(t *testing.T) {
	parent := syntheticParent(t)
	live := filepath.Join(parent, "live")
	hidden := filepath.Join(parent, "hidden")
	outside := filepath.Join(parent, "outside")
	paths, err := Prepare(filepath.Join(live, "app"))
	if err != nil {
		t.Fatal(err)
	}
	outsidePaths, err := Prepare(filepath.Join(outside, "app"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	failure := make(chan error, 1)
	go func() {
		defer close(done)
		for attempt := 0; attempt < 300; attempt++ {
			if err := os.Rename(live, hidden); err != nil {
				failure <- err
				return
			}
			if err := os.Symlink(outside, live); err != nil {
				failure <- err
				return
			}
			runtime.Gosched()
			if err := os.Remove(live); err != nil {
				failure <- err
				return
			}
			if err := os.Rename(hidden, live); err != nil {
				failure <- err
				return
			}
			runtime.Gosched()
		}
	}()
	for attempt := 0; attempt < 300; attempt++ {
		lease, err := Acquire(paths)
		if err == nil {
			if err := lease.Close(); err != nil {
				t.Error(err)
			}
		} else if !errors.Is(err, ErrUnsafeDataPath) && !errors.Is(err, ErrDataUnavailable) {
			t.Errorf("unexpected fixed error = %v", err)
		}
	}
	<-done
	select {
	case err := <-failure:
		t.Fatal(err)
	default:
	}
	if _, err := os.Lstat(filepath.Join(outsidePaths.Root, ".instance.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ancestor symlink redirected lock creation")
	}
}
