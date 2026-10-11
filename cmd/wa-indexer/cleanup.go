package main

import (
	"context"
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
