package mcpserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunWithNativeStdioCleanupFailureOverridesRunResult(t *testing.T) {
	injected := errors.New("synthetic-cleanup-token /private/synthetic/config")
	for _, fixture := range []struct {
		name               string
		runErr, cleanupErr error
	}{
		{"successful_run", nil, errNativeStdioCleanup},
		{"unavailable_run", ErrUnavailable, errNativeStdioCleanup},
		{"canceled_run", context.Canceled, errNativeStdioCleanup},
		{"successful_run_injected_error", nil, injected},
		{"unavailable_run_injected_error", ErrUnavailable, injected},
		{"canceled_run_injected_error", context.Canceled, injected},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var order []string
			err := runWithNativeStdioCleanup(func() error {
				order = append(order, "run")
				return fixture.runErr
			}, func() error {
				order = append(order, "cleanup")
				return fixture.cleanupErr
			})
			if len(order) != 2 || order[0] != "run" || order[1] != "cleanup" {
				t.Error("cleanup did not run exactly once after the session ended")
			}
			if !errors.Is(err, errNativeStdioCleanup) {
				t.Error("cleanup failure was hidden by the session result")
			}
			if err == nil || err.Error() != "mcp_stdio_cleanup_failed" {
				t.Error("cleanup failure did not use its fixed diagnostic")
			}
			if err != nil && (strings.Contains(err.Error(), "synthetic-cleanup-token") || strings.Contains(err.Error(), "/private/synthetic")) {
				t.Error("cleanup exposed the injected error text")
			}
		})
	}
}

func TestRunWithNativeStdioCleanupPreservesRunResultWhenCleanupSucceeds(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		runErr error
	}{
		{"successful_run", nil},
		{"unavailable_run", ErrUnavailable},
		{"canceled_run", context.Canceled},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			cleanupCalls := 0
			err := runWithNativeStdioCleanup(func() error { return fixture.runErr }, func() error {
				cleanupCalls++
				return nil
			})
			if err != fixture.runErr || cleanupCalls != 1 {
				t.Fatal("successful cleanup changed the session result or did not run exactly once")
			}
		})
	}
}

func requireNativeStdioCleanupError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, errNativeStdioCleanup) || err == nil || err.Error() != "mcp_stdio_cleanup_failed" {
		t.Error("owned cleanup failure did not return its fixed diagnostic")
	}
	if err != nil && (strings.Contains(err.Error(), "synthetic-") || strings.Contains(err.Error(), "/private/synthetic")) {
		t.Error("owned cleanup exposed the injected operation error")
	}
}

func TestNativeStdioCloseAttemptsBothCopiesAndMemoizesResult(t *testing.T) {
	firstFailure := errors.New("first-close-failure")
	secondFailure := errors.New("second-close-failure")
	injected := errors.New("synthetic-close-token /private/synthetic/config")
	for _, fixture := range []struct {
		name     string
		failures [2]error
		failure  bool
	}{
		{"both_succeed", [2]error{}, false},
		{"first_failure", [2]error{firstFailure, nil}, true},
		{"second_failure", [2]error{nil, secondFailure}, true},
		{"both_failures", [2]error{firstFailure, secondFailure}, true},
		{"injected_error", [2]error{injected, nil}, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var calls [2]int
			var order []int
			closeOwned := newNativeStdioClose([2]func() error{
				func() error { calls[0]++; order = append(order, 0); return fixture.failures[0] },
				func() error { calls[1]++; order = append(order, 1); return fixture.failures[1] },
			})
			var results [3]error
			for attempt := range results {
				results[attempt] = closeOwned()
				if fixture.failure {
					requireNativeStdioCleanupError(t, results[attempt])
				} else if results[attempt] != nil {
					t.Error("successful owned closes were rejected")
				}
			}
			if calls != [2]int{1, 1} || len(order) != 2 || order[0] != 0 || order[1] != 1 {
				t.Error("owned close skipped a copy, retried a close, or changed close order")
			}
			if results[0] != results[1] || results[1] != results[2] {
				t.Error("owned close did not retain the same result for later callers")
			}
		})
	}
}

func TestNativeStdioCleanupFailurePriorityAndMemoizedResult(t *testing.T) {
	firstFailure := errors.New("first-close-failure")
	secondFailure := errors.New("second-close-failure")
	injected := errors.New("synthetic-cleanup-token /private/synthetic/config")
	for _, fixture := range []struct {
		name                  string
		runErr                error
		closeErrors           [2]error
		restoreErr            error
		failure, closeFailure bool
	}{
		{"successful_cleanup_and_run", nil, [2]error{}, nil, false, false},
		{"first_close_failure_run_success", nil, [2]error{firstFailure, nil}, nil, true, true},
		{"second_close_failure_run_configuration", ErrConfiguration, [2]error{nil, secondFailure}, nil, true, true},
		{"first_close_failure_run_unavailable", ErrUnavailable, [2]error{firstFailure, nil}, nil, true, true},
		{"first_close_failure_run_canceled", context.Canceled, [2]error{firstFailure, nil}, nil, true, true},
		{"injected_close_error_run_success", nil, [2]error{injected, nil}, nil, true, true},
		{"restore_failure_after_successful_close", nil, [2]error{}, errNativeStdioCleanup, true, false},
		{"both_close_and_restore_fail", nil, [2]error{firstFailure, secondFailure}, injected, true, true},
		{"injected_restore_error_run_unavailable", ErrUnavailable, [2]error{}, injected, true, false},
		{"successful_cleanup_unavailable_run", ErrUnavailable, [2]error{}, nil, false, false},
		{"successful_cleanup_canceled_run", context.Canceled, [2]error{}, nil, false, false},
		{"successful_cleanup_configuration_run", ErrConfiguration, [2]error{}, nil, false, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			stopCalls, restoreCalls := 0, 0
			var closeCalls [2]int
			var order []string
			closeOwned := newNativeStdioClose([2]func() error{
				func() error { closeCalls[0]++; order = append(order, "close0"); return fixture.closeErrors[0] },
				func() error { closeCalls[1]++; order = append(order, "close1"); return fixture.closeErrors[1] },
			})
			cleanup := newNativeStdioCleanup(func() bool {
				stopCalls++
				order = append(order, "stop")
				return false
			}, closeOwned, func() error {
				restoreCalls++
				order = append(order, "restore")
				if closeCalls != [2]int{1, 1} {
					t.Error("restoration began before both owned closes completed")
				}
				return fixture.restoreErr
			})
			result := runWithNativeStdioCleanup(func() error {
				order = append(order, "run")
				return fixture.runErr
			}, cleanup)
			if fixture.failure {
				requireNativeStdioCleanupError(t, result)
			} else if result != fixture.runErr {
				t.Error("successful cleanup changed the session result")
			}
			var cleanupResults, closeResults [2]error
			for attempt := range cleanupResults {
				cleanupResults[attempt] = cleanup()
				closeResults[attempt] = closeOwned()
				if fixture.failure {
					requireNativeStdioCleanupError(t, cleanupResults[attempt])
				} else if cleanupResults[attempt] != nil {
					t.Error("successful cleanup acquired an error on a later call")
				}
				if fixture.closeFailure {
					requireNativeStdioCleanupError(t, closeResults[attempt])
				} else if closeResults[attempt] != nil {
					t.Error("successful owned close acquired an error on a later call")
				}
			}
			if stopCalls != 1 || restoreCalls != 1 || closeCalls != [2]int{1, 1} || strings.Join(order, ",") != "run,stop,close0,close1,restore" {
				t.Error("cleanup changed operation order or repeated a stop, close, or restoration")
			}
			if cleanupResults[0] != cleanupResults[1] || closeResults[0] != closeResults[1] {
				t.Error("later cleanup callers did not receive the memoized result")
			}
		})
	}
}

func TestNativeStdioCleanupWaitsForConcurrentClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseClose := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseClose()
	var copiesClosed atomic.Bool
	var restores atomic.Int32
	closeOwned := newNativeStdioClose([2]func() error{
		func() error { close(started); <-release; return nil },
		func() error { copiesClosed.Store(true); return nil },
	})
	closeDone, cleanupDone := make(chan error, 1), make(chan error, 1)
	go func() { closeDone <- closeOwned() }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("controlled owned close did not start")
	}
	cleanup := newNativeStdioCleanup(func() bool { close(stopped); return false }, closeOwned, func() error {
		if !copiesClosed.Load() {
			t.Error("cleanup restored flags while an owned close was still running")
		}
		restores.Add(1)
		return nil
	})
	go func() { cleanupDone <- cleanup() }()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("cleanup did not enter its close barrier")
	}
	cleanupReturned := false
	select {
	case <-cleanupDone:
		cleanupReturned = true
		t.Error("cleanup returned before the running owned close completed")
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal("controlled close barrier exceeded its deadline")
	}
	releaseClose()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Error("successful controlled closes were rejected")
		}
	case <-ctx.Done():
		t.Fatal("controlled owned close did not finish")
	}
	if !cleanupReturned {
		select {
		case err := <-cleanupDone:
			if err != nil {
				t.Error("successful controlled cleanup was rejected")
			}
		case <-ctx.Done():
			t.Fatal("cleanup did not finish after the close barrier released")
		}
	}
	if cleanup() != nil || !copiesClosed.Load() || restores.Load() != 1 {
		t.Error("controlled cleanup failed or repeated restoration")
	}
}
