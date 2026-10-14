package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

type applicationCleanup interface {
	Close(context.Context) error
	CleanupComplete() bool
}

// A bounded Close attempt is not permission to exit with a live native owner.
// Retry while this executable retains the instance lease; force process
// termination remains the operating system/user's explicit action.
func drainApplication(owner applicationCleanup) error {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := owner.Close(ctx)
		cancel()
		if owner.CleanupComplete() {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Container serving reserves two seconds inside the delivery image's ten-second
// stop envelope. Native serving is not constrained by that image envelope and
// allows more time for accepted source work, including bounded history downloads.
// Either mode uses one deadline for HTTP, provider/helper and archive joins;
// exceeding it remains an unsafe outcome, not permission to restart or recover.
const serveShutdownBudget = 8 * time.Second
const nativeServeShutdownBudget = 45 * time.Second

func serveShutdownDuration(container bool) time.Duration {
	if container {
		return serveShutdownBudget
	}
	return nativeServeShutdownBudget
}

var errServeShutdown = errors.New("serve_shutdown_incomplete")

type serveShutdown struct {
	once     sync.Once
	duration time.Duration
	ctx      context.Context
	cancel   context.CancelFunc
}

func newServeShutdown(d time.Duration) *serveShutdown { return &serveShutdown{duration: d} }
func (b *serveShutdown) begin() context.Context {
	b.once.Do(func() { b.ctx, b.cancel = context.WithTimeout(context.Background(), b.duration) })
	return b.ctx
}
func (b *serveShutdown) close() { b.begin(); b.cancel() }
func drainServeApplication(ctx context.Context, owner applicationCleanup) error {
	if ctx.Err() != nil {
		return errServeShutdown
	}
	return waitServeCleanup(ctx, startServeCleanup(ctx, owner))
}
func waitServeCleanup(ctx context.Context, done <-chan error) error {
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return errServeShutdown
	}
}
func startServeCleanup(ctx context.Context, owner applicationCleanup) <-chan error {
	// Exactly one retained worker owns handoff removal and sequential Close
	// attempts. If a callee ignores cancellation, the caller still has a total
	// bound; no second Close races it, and no lease is released by the caller.
	done := make(chan error, 1)
	go func() {
		defer close(done)
		var handoffErr error
		if handoff, ok := owner.(interface{ RemoveOwnerURL() error }); ok {
			handoffErr = handoff.RemoveOwnerURL()
		}
		for {
			if ctx.Err() != nil {
				done <- errServeShutdown
				return
			}
			closeErr := owner.Close(ctx)
			if owner.CleanupComplete() {
				if closeErr != nil || handoffErr != nil {
					done <- errServeShutdown
				} else {
					done <- nil
				}
				return
			}
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				done <- errServeShutdown
				return
			case <-timer.C:
			}
		}
	}()
	return done
}
