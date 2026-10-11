//go:build darwin || linux

package workerlife

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/diskspace"
)

type lifecycleProviderFunc func(context.Context, *os.File, int64) (ExecutableLease, error)

func (f lifecycleProviderFunc) Prepare(ctx context.Context, file *os.File, maximum int64) (ExecutableLease, error) {
	return f(ctx, file, maximum)
}

type lifecycleCopyLease struct {
	mu     sync.Mutex
	inner  ExecutableLease
	path   string
	fail   bool
	closed bool
}

func (l *lifecycleCopyLease) Path() string {
	if l.inner != nil {
		return l.inner.Path()
	}
	return l.path
}
func (l *lifecycleCopyLease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail {
		return errors.New("private-copy-cleanup-canary")
	}
	if l.inner != nil {
		if err := l.inner.Close(); err != nil {
			return err
		}
	}
	l.closed = true
	return nil
}
func (l *lifecycleCopyLease) allowCleanup() { l.mu.Lock(); l.fail = false; l.mu.Unlock() }

func TestDarwinStartRequiresExecutableProvider(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin executable namespace contract")
	}
	s := newLifecycleFixture(t, 1, "native")
	config := s.config(1)
	config.ExecutableProvider = nil
	lease, err := Start(context.Background(), config, s.native, s.guards)
	if lease != nil || err != ErrUnavailable || s.native.Process != nil {
		t.Fatal("missing executable provider started ownership", err)
	}
}

func TestDarwinTypedNilExecutableAuthorityIsFixed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin executable namespace contract")
	}
	for _, name := range []string{"provider", "lease"} {
		t.Run(name, func(t *testing.T) {
			s := newLifecycleFixture(t, 1, "native")
			config := s.config(1)
			if name == "provider" {
				var provider lifecycleProviderFunc
				config.ExecutableProvider = provider
			} else {
				config.ExecutableProvider = lifecycleProviderFunc(func(context.Context, *os.File, int64) (ExecutableLease, error) {
					var copy *lifecycleCopyLease
					return copy, nil
				})
			}
			lease, err := Start(context.Background(), config, s.native, s.guards)
			if lease != nil || err != ErrUnavailable || s.native.Process != nil {
				t.Fatal("typednil executable authority accepted", err)
			}
		})
	}
}

func TestDarwinPartialExecutablePreparationKeepsCleanupOwner(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin executable namespace contract")
	}
	s := newLifecycleFixture(t, 1, "native")
	copy := &lifecycleCopyLease{path: filepath.Join(s.root, "unused-copy"), fail: true}
	config := s.config(1)
	config.ExecutableProvider = lifecycleProviderFunc(func(ctx context.Context, file *os.File, maximum int64) (ExecutableLease, error) {
		if maximum != 64<<20 {
			t.Error("holder executable cap changed", maximum)
		}
		return copy, errors.New("private-provider-path-canary")
	})
	var err error
	s.lease, err = Start(context.Background(), config, s.native, s.guards)
	if s.lease == nil || !errors.Is(err, ErrUnavailable) || err.Error() != ErrUnavailable.Error() || s.native.Process != nil {
		t.Fatal("partial provider ownership lost or raw error escaped", err)
	}
	s.closeBorrowedGuards(t)
	if err := s.lease.AwaitCleanup(context.Background()); err != ErrCleanup {
		t.Fatal("failed copy cleanup acknowledged release", err)
	}
	lifecycleAssertLocked(t, s.guardPaths[0])
	copy.allowCleanup()
	if err := s.lease.AwaitCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	lifecycleAssertUnlocked(t, s.guardPaths[0])
}

func TestDarwinHolderStartFailureRetainsCleanupOwner(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin executable namespace contract")
	}
	for _, cleanupFails := range []bool{false, true} {
		name := "cleanup-succeeds"
		if cleanupFails {
			name = "cleanup-retry"
		}
		t.Run(name, func(t *testing.T) {
			s := newLifecycleFixture(t, 1, "native")
			copy := &lifecycleCopyLease{path: filepath.Join(s.root, "missing-holder-private-path-canary"), fail: cleanupFails}
			// A failed diagnostic assertion must still let fixture cleanup retry.
			t.Cleanup(copy.allowCleanup)
			config := s.config(1)
			calls := 0
			config.ExecutableProvider = lifecycleProviderFunc(func(ctx context.Context, file *os.File, maximum int64) (ExecutableLease, error) {
				calls++
				if ctx == nil || file == nil || maximum != 64<<20 {
					t.Error("holder preparation authority or cap changed")
				}
				return copy, nil
			})
			var err error
			s.lease, err = Start(context.Background(), config, s.native, s.guards)
			lifecycleAssertLaunchFailure(t, err, "holder_start", true, uint32(syscall.ENOENT))
			if calls != 1 || s.native.Process != nil {
				t.Fatal("successful holder preparation did not stop at failed holder launch")
			}
			for _, path := range []string{s.holderReport, s.marker} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed holder launch executed a holder or native")
				}
			}
			// Start owns duplicates, while every borrowed guard remains open.
			s.closeBorrowedGuards(t)
			if !cleanupFails {
				if s.lease != nil || !copy.closed {
					t.Fatal("successful failed-launch cleanup retained an owner")
				}
				lifecycleAssertUnlocked(t, s.guardPaths[0])
				return
			}
			if s.lease == nil || copy.closed {
				t.Fatal("failed copy cleanup lost its never-started owner")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := s.lease.AwaitCleanup(ctx); !errors.Is(err, ErrCleanup) {
				t.Fatal("failed copy cleanup acknowledged release")
			}
			lifecycleAssertLocked(t, s.guardPaths[0])
			copy.allowCleanup()
			if err := s.lease.AwaitCleanup(ctx); err != nil || !copy.closed {
				t.Fatal("never-started cleanup retry did not finish")
			}
			lifecycleAssertUnlocked(t, s.guardPaths[0])
		})
	}
}

func TestDarwinHelperPreparationPreservesBudgetCauseWithoutPrivateDetails(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin executable namespace contract")
	}
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			s := newLifecycleFixture(t, 1, "native")
			copy := &lifecycleCopyLease{path: filepath.Join(s.root, "unused-copy"), fail: true}
			config := s.config(1)
			called := false
			config.ExecutableProvider = lifecycleProviderFunc(func(ctx context.Context, file *os.File, maximum int64) (ExecutableLease, error) {
				called = true
				if ctx == nil || file == nil || maximum != 64<<20 {
					t.Error("holder preparation authority or cap changed")
				}
				cause := fmt.Errorf("private-budget-path-canary: %w", diskspace.ErrInsufficientSpace)
				if partial {
					return copy, cause
				}
				return nil, cause
			})
			var err error
			s.lease, err = Start(context.Background(), config, s.native, s.guards)
			if !called || !errors.Is(err, diskspace.ErrInsufficientSpace) || !errors.Is(err, ErrUnavailable) || err.Error() != ErrUnavailable.Error() {
				t.Fatal("budget identity lost or provider detail escaped", err)
			}
			if s.native.Process != nil {
				t.Fatal("budget denial started native process")
			}
			if _, err := os.Stat(s.holderReport); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("budget denial started holder")
			}
			s.closeBorrowedGuards(t)
			if partial {
				if s.lease == nil || s.lease.AwaitCleanup(context.Background()) != ErrCleanup {
					t.Fatal("partial budget failure lost cleanup owner")
				}
				lifecycleAssertLocked(t, s.guardPaths[0])
				copy.allowCleanup()
				if err := s.lease.AwaitCleanup(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else if s.lease != nil {
				t.Fatal("nil preparation returned unexpected cleanup owner")
			}
			lifecycleAssertUnlocked(t, s.guardPaths[0])
		})
	}
}

func TestDarwinLateExecutablePreparationDoesNotLaunchAfterCancellation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin executable namespace contract")
	}
	s := newLifecycleFixture(t, 1, "native")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	copy := &lifecycleCopyLease{path: filepath.Join(s.root, "never-executed-copy")}
	config := s.config(1)
	config.ExecutableProvider = lifecycleProviderFunc(func(context.Context, *os.File, int64) (ExecutableLease, error) {
		cancel()
		return copy, nil
	})
	lease, err := Start(ctx, config, s.native, s.guards)
	if lease != nil || !errors.Is(err, context.Canceled) || !copy.closed || s.native.Process != nil {
		t.Fatal("late preparation launched or abandoned copy cleanup", err)
	}
	if _, err := os.Stat(s.holderReport); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("holder ran after cancellation", err)
	}
}

func TestDarwinPrivateHelperCopyLivesUntilNativeAndHolderCleanup(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin executable namespace contract")
	}
	s := newLifecycleFixture(t, 1, "native")
	config := s.config(1)
	var wrapped *lifecycleCopyLease
	config.Environment = append(config.Environment, "DYLD_LIBRARY_PATH="+s.root, "LD_PRELOAD="+s.root)
	config.ExecutableProvider = lifecycleProviderFunc(func(ctx context.Context, file *os.File, maximum int64) (ExecutableLease, error) {
		copy, err := s.namespace.Prepare(ctx, file, maximum)
		if copy != nil {
			wrapped = &lifecycleCopyLease{inner: copy, fail: true}
		}
		return wrapped, err
	})
	var err error
	s.lease, err = Start(context.Background(), config, s.native, s.guards)
	if err != nil || s.lease == nil || wrapped == nil {
		t.Fatal("private helper launch", err)
	}
	copyPath := wrapped.Path()
	holder := lifecycleWaitReport(t, s.holderReport)
	if len(holder.LoaderOverrides) != 0 {
		t.Fatal("copied helper retained loader overrides", holder.LoaderOverrides)
	}
	copyInfo, copyErr := os.Stat(copyPath)
	holderInfo, holderErr := os.Stat(holder.Executable)
	sourceInfo, sourceErr := os.Stat(os.Args[0])
	if copyErr != nil || holderErr != nil || sourceErr != nil || !os.SameFile(copyInfo, holderInfo) || os.SameFile(sourceInfo, holderInfo) {
		t.Fatal("helper did not execute its private prepared copy", holder.Executable, copyErr, holderErr, sourceErr)
	}
	if err := s.namespace.Close(); err == nil {
		t.Fatal("namespace closed with an active holder copy")
	}
	_ = lifecycleReadEvent(t, s.eventsRead)
	s.closeBorrowedGuards(t)
	_ = s.stopWrite.Close()
	if err := s.native.Wait(); err != nil {
		t.Fatal(err)
	}
	s.waited = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.lease.AwaitCleanup(ctx); err != ErrCleanup {
		t.Fatal("copy cleanup failure lost owner", err)
	}
	lifecycleAssertLocked(t, s.guardPaths[0])
	wrapped.allowCleanup()
	if err := s.lease.AwaitCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(copyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("copy not removed after cleanup", err)
	}
	lifecycleAssertUnlocked(t, s.guardPaths[0])
}
