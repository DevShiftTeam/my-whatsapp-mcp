package localauth

import (
	"context"
	"sync"
	"time"
)

// Admission never queues or spawns a goroutine. Two 32 MiB hashes bound process
// memory even across apps, realms, setup and verification. Argon2 is synchronous;
// cancellation is checked before admission and after its bounded work completes.
type slots chan struct{}

var hashSlots = make(slots, 2)

func (s slots) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s <- struct{}{}:
		return nil
	default:
		return ErrBusy
	}
}
func (s slots) release() { <-s }

type attemptLimiter struct {
	mu               sync.Mutex
	window, cooldown time.Time
	attempts, failed int
}

var verificationAttempts attemptLimiter

func (l *attemptLimiter) admit(now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Before(l.cooldown) {
		return ErrBusy
	}
	if l.window.IsZero() || !now.Before(l.window.Add(30*time.Second)) {
		l.window = now
		l.attempts = 0
		l.failed = 0
	}
	if l.attempts >= 8 {
		return ErrBusy
	}
	l.attempts++
	return nil
}
func (l *attemptLimiter) finish(now time.Time, ok bool) {
	if ok {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failed++
	if l.failed >= 5 {
		l.cooldown = now.Add(30 * time.Second)
	}
}
