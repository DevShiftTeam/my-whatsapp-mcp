package main

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/remoteaccess"
	"testing"
	"time"
)

type delayedApplicationCleanup struct {
	calls int
	done  bool
}

func (c *delayedApplicationCleanup) Close(context.Context) error {
	c.calls++
	if c.calls == 1 {
		return errors.New("cleanup_pending")
	}
	c.done = true
	return nil
}
func (c *delayedApplicationCleanup) CleanupComplete() bool { return c.done }
func TestExecutableRetainsOwnershipUntilCleanupAcknowledged(t *testing.T) {
	owner := &delayedApplicationCleanup{}
	if err := drainApplication(owner); err != nil {
		t.Fatal(err)
	}
	if owner.calls != 2 || !owner.done {
		t.Fatal("executable exited before cleanup acknowledgement")
	}
}

type serveCleanupProbe struct {
	calls         int
	completeAfter int
	contexts      []context.Context
}

func (c *serveCleanupProbe) Close(ctx context.Context) error {
	c.calls++
	c.contexts = append(c.contexts, ctx)
	if c.CleanupComplete() {
		return nil
	}
	return remoteaccess.ErrBusy
}
func (c *serveCleanupProbe) CleanupComplete() bool {
	return c.completeAfter > 0 && c.calls >= c.completeAfter
}
func TestServeDrainOneBudgetAndAcknowledgment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	p := &serveCleanupProbe{completeAfter: 3}
	if e := drainServeApplication(ctx, p); e != nil {
		t.Fatal(e)
	}
	for _, c := range p.contexts {
		if c != ctx {
			t.Fatal("renewed cleanup context")
		}
	}
	ctx2, stop := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer stop()
	pending := &serveCleanupProbe{}
	start := time.Now()
	worker := startServeCleanup(ctx2, pending)
	if e := waitServeCleanup(ctx2, worker); e == nil {
		t.Fatal("pending acknowledged")
	}
	<-worker // Timeout retains the worker; join before reading its fake counters.
	if pending.CleanupComplete() {
		t.Fatal("pending acknowledged")
	}
	if time.Since(start) > 300*time.Millisecond || pending.calls > 5 {
		t.Fatal("unbounded drain or spin")
	}
}
func TestServeBudgetStartsOnceAtStopIntent(t *testing.T) {
	b := newServeShutdown(80 * time.Millisecond)
	defer b.close()
	first := b.begin()
	deadline, _ := first.Deadline()
	time.Sleep(20 * time.Millisecond)
	second := b.begin()
	again, _ := second.Deadline()
	if first != second || deadline != again {
		t.Fatal("shutdown budget refreshed")
	}
	<-first.Done()
	if b.begin().Err() == nil {
		t.Fatal("expired budget revived")
	}
}

func TestServeShutdownBudgetPreservesContainerAndAllowsNativeDrain(t *testing.T) {
	for _, tc := range []struct {
		name      string
		container bool
		want      time.Duration
	}{
		{"native", false, 45 * time.Second},
		{"container", true, 8 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serveShutdownDuration(tc.container); got != tc.want {
				t.Fatalf("shutdown budget = %s, want %s", got, tc.want)
			}
			budget := newServeShutdown(serveShutdownDuration(tc.container))
			defer budget.close()
			started := time.Now()
			ctx := budget.begin()
			deadline, ok := ctx.Deadline()
			if !ok || deadline.Before(started.Add(tc.want)) || budget.begin() != ctx {
				t.Fatal("selected shutdown budget was shortened or replaced")
			}
		})
	}
}

type blockedServeCleanup struct {
	release chan struct{}
	entered chan struct{}
	remove  bool
	closed  bool
}

func (c *blockedServeCleanup) RemoveOwnerURL() error {
	if c.remove {
		close(c.entered)
		<-c.release
	}
	return nil
}
func (c *blockedServeCleanup) Close(context.Context) error {
	if !c.remove {
		close(c.entered)
		<-c.release
	}
	c.closed = true
	return nil
}
func (c *blockedServeCleanup) CleanupComplete() bool { return c.closed }
func TestServeBudgetBoundsUncooperativeCleanupAndHandoff(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "handoff"}[remove], func(t *testing.T) {
			owner := &blockedServeCleanup{release: make(chan struct{}), entered: make(chan struct{}), remove: remove}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			worker := startServeCleanup(ctx, owner)
			result := make(chan error, 1)
			go func() { result <- waitServeCleanup(ctx, worker) }()
			select {
			case <-owner.entered:
			case <-time.After(time.Second):
				close(owner.release)
				t.Fatal("stage not entered")
			}
			select {
			case e := <-result:
				if e == nil {
					t.Fatal("blocked cleanup acknowledged")
				}
				close(owner.release)
			case <-time.After(150 * time.Millisecond):
				close(owner.release)
				<-result
				t.Fatal("serve caller exceeded its total deadline")
			}
			select {
			case <-worker:
			case <-time.After(time.Second):
				t.Fatal("retained cleanup worker did not join after fake released")
			}
		})
	}
}

// Ownership acknowledgement does not imply that the terminal drain succeeded.
type terminalServeCleanup struct {
	closeErr, handoffErr error
	complete             bool
	calls, removals      int
}

func (c *terminalServeCleanup) RemoveOwnerURL() error { c.removals++; return c.handoffErr }
func (c *terminalServeCleanup) Close(context.Context) error {
	c.calls++
	c.complete = true
	return c.closeErr
}
func (c *terminalServeCleanup) CleanupComplete() bool { return c.complete }
func TestServeDrainTerminalFailuresRemainFixedErrors(t *testing.T) {
	secretError := errors.New("SYNTHETIC_SECRET /private/owner-state")
	for _, tc := range []struct {
		name                 string
		closeErr, handoffErr error
	}{
		{"terminal close", secretError, nil},
		{"handoff", nil, secretError},
		{"both", secretError, secretError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			owner := &terminalServeCleanup{closeErr: tc.closeErr, handoffErr: tc.handoffErr}
			err := drainServeApplication(ctx, owner)
			if err != errServeShutdown {
				t.Fatalf("terminal failure must return the fixed serve error; got %v", err)
			}
			if !owner.complete || owner.calls != 1 || owner.removals != 1 {
				t.Fatal("terminal cleanup was not acknowledged exactly once")
			}
			if ctx.Err() != nil {
				t.Fatal("terminal error waited for the deadline")
			}
		})
	}
}
