// Package workerlife retains existing flock ownership across app death until a
// trusted native process group and its leader have exited.
package workerlife

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
)

var (
	ErrUnavailable = errors.New("worker_lifetime_unavailable")
	ErrProtocol    = errors.New("worker_lifetime_protocol_invalid")
	ErrCleanup     = errors.New("worker_lifetime_cleanup_incomplete")
	ErrLimit       = errors.New("worker_lifetime_limit_exceeded")
)

type launchStage uint8

const (
	holderLaunchStage launchStage = iota
	nativeLaunchStage
)

// LaunchFailure exposes only the fixed launch role and a numeric syscall errno.
// It is returned by value, so errors.As gives callers an independent copy.
// It never retains the original error, executable path, argv, or stderr.
type LaunchFailure struct {
	stage      launchStage
	errnoKnown bool
	errno      uint32
}

func (failure LaunchFailure) Stage() string {
	if failure.stage == nativeLaunchStage {
		return "native_start"
	}
	return "holder_start"
}

func (failure LaunchFailure) ErrnoKnown() bool { return failure.errnoKnown }
func (failure LaunchFailure) Errno() uint32    { return failure.errno }
func (failure LaunchFailure) Error() string    { return ErrUnavailable.Error() }
func (failure LaunchFailure) Unwrap() error    { return ErrUnavailable }

// Config is trusted composition input, never a native request or user option.
// ExecutableFile is a borrowed, package-verified executable descriptor. Without
// it, Executable names a trusted stable absolute path opened without following
// its final symlink. Darwin requires ExecutableProvider to prepare a private
// immutable copy from that descriptor. Args is the complete helper argv; nil
// selects hidden mode.
type Config struct {
	Executable         string
	ExecutableFile     *os.File
	ExecutableProvider ExecutableProvider
	Args, Environment  []string
	ReadyTimeout       time.Duration
}

// Lease owns parent guard duplicates, control pipes, and the helper wait. A
// timeout never drops ownership. The native caller remains its sole Wait owner.
type Lease struct {
	mu                         sync.Mutex
	guards                     []*os.File
	control, reply             *os.File
	helperExited, protocolDone chan struct{}
	decision                   chan int
	ready                      chan error
	pid                        int
	acknowledged               bool
	cleaned                    bool
	executable                 ExecutableLease
}

// AwaitCleanup succeeds only after the holder is reaped and the registered
// native group AND leader are absent. A never-started native requires an
// explicit acknowledged NOOP. The caller must separately reap its native cmd.
func (lease *Lease) AwaitCleanup(ctx context.Context) error {
	if lease == nil || ctx == nil {
		return ErrProtocol
	}
	for _, done := range []<-chan struct{}{lease.protocolDone, lease.helperExited} {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for {
		lease.mu.Lock()
		if lease.cleaned {
			lease.mu.Unlock()
			return nil
		}
		pid, acknowledged := lease.pid, lease.acknowledged
		lease.mu.Unlock()
		absent := pid == 0 && acknowledged
		if pid > 0 {
			var err error
			absent, err = probeNative(pid)
			absent = absent && err == nil
		}
		if absent {
			lease.mu.Lock()
			defer lease.mu.Unlock()
			if lease.cleaned {
				return nil
			}
			if lease.executable != nil {
				if err := lease.executable.Close(); err != nil {
					return ErrCleanup
				}
				lease.executable = nil
			}
			for _, guard := range lease.guards {
				if err := guard.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
					return ErrCleanup
				}
			}
			lease.guards = nil
			if lease.control != nil {
				_ = lease.control.Close()
			}
			if lease.reply != nil {
				_ = lease.reply.Close()
			}
			lease.cleaned = true
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
