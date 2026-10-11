package main

import (
	"context"
	"errors"
	"testing"
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
