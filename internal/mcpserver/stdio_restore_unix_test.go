//go:build unix

package mcpserver

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// These probes replace only the syscall boundary. Their descriptor numbers are
// synthetic; no test reads, changes, closes, or duplicates the host's stdio.
type nativeRestoreProbe struct {
	t                    *testing.T
	fds                  [2]uintptr
	flags, readback      [2]int
	setErrors, getErrors [2][]error
	setCalls, getCalls   [2]int
}

func newNativeRestoreProbe(t *testing.T) *nativeRestoreProbe {
	t.Helper()
	return &nativeRestoreProbe{
		t:        t,
		fds:      [2]uintptr{101, 202},
		flags:    [2]int{unix.O_RDONLY, unix.O_WRONLY},
		readback: [2]int{unix.O_RDONLY, unix.O_WRONLY},
	}
}

func (p *nativeRestoreProbe) fcntl(fd uintptr, command, argument int) (int, error) {
	index := -1
	for i, candidate := range p.fds {
		if candidate == fd {
			index = i
			break
		}
	}
	if index == -1 {
		p.t.Fatal("restoration addressed a descriptor outside its supplied pair")
		return 0, unix.EBADF
	}
	var attempts *int
	var failures []error
	switch command {
	case unix.F_SETFL:
		if p.getCalls[index] != 0 {
			p.t.Error("restoration retried a setter after its readback began")
		}
		if argument != p.flags[index] {
			p.t.Error("restoration changed the original setter flags")
		}
		attempts, failures = &p.setCalls[index], p.setErrors[index]
	case unix.F_GETFL:
		if argument != 0 || p.setCalls[index] == 0 {
			p.t.Error("readback did not follow a restoration attempt")
		}
		attempts, failures = &p.getCalls[index], p.getErrors[index]
	default:
		p.t.Fatal("restoration used an unexpected syscall command")
		return 0, unix.EINVAL
	}
	(*attempts)++
	if *attempts > 3 {
		p.t.Fatal("restoration exceeded three attempts for one syscall")
		return 0, unix.EINTR
	}
	if *attempts <= len(failures) && failures[*attempts-1] != nil {
		return 0, failures[*attempts-1]
	}
	if command == unix.F_GETFL {
		return p.readback[index], nil
	}
	return 0, nil
}

func requireNativeRestoreResult(t *testing.T, err error, failure bool) {
	t.Helper()
	if !failure {
		if err != nil {
			t.Error("verified restoration was rejected")
		}
		return
	}
	if !errors.Is(err, errNativeStdioCleanup) || err == nil || err.Error() != "mcp_stdio_cleanup_failed" {
		t.Error("restoration failure did not return its fixed cleanup diagnostic")
	}
	if err != nil && (strings.Contains(err.Error(), "synthetic-restore-token") || strings.Contains(err.Error(), "/private/synthetic")) {
		t.Error("restoration exposed the injected syscall error text")
	}
}

func TestNativeStdioRestoreRetriesInterruptedSyscalls(t *testing.T) {
	for _, fixture := range []struct {
		name                 string
		setErrors, getErrors [2][]error
		sets, gets           [2]int
		failure              bool
	}{
		{"set_interrupted_once", [2][]error{{unix.EINTR}, nil}, [2][]error{}, [2]int{2, 1}, [2]int{1, 1}, false},
		{"set_interrupted_twice", [2][]error{nil, {unix.EINTR, unix.EINTR}}, [2][]error{}, [2]int{1, 3}, [2]int{1, 1}, false},
		{"get_interrupted_once", [2][]error{}, [2][]error{{unix.EINTR}, nil}, [2]int{1, 1}, [2]int{2, 1}, false},
		{"get_interrupted_twice", [2][]error{}, [2][]error{nil, {unix.EINTR, unix.EINTR}}, [2]int{1, 1}, [2]int{1, 3}, false},
		{"set_retry_exhausted", [2][]error{{unix.EINTR, unix.EINTR, unix.EINTR}, nil}, [2][]error{}, [2]int{3, 1}, [2]int{1, 1}, true},
		{"get_retry_exhausted", [2][]error{}, [2][]error{{unix.EINTR, unix.EINTR, unix.EINTR}, nil}, [2]int{1, 1}, [2]int{3, 1}, true},
		{"both_set_retries_exhausted", [2][]error{{unix.EINTR, unix.EINTR, unix.EINTR}, {unix.EINTR, unix.EINTR, unix.EINTR}}, [2][]error{}, [2]int{3, 3}, [2]int{1, 1}, true},
		{"both_get_retries_exhausted", [2][]error{}, [2][]error{{unix.EINTR, unix.EINTR, unix.EINTR}, {unix.EINTR, unix.EINTR, unix.EINTR}}, [2]int{1, 1}, [2]int{3, 3}, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			probe := newNativeRestoreProbe(t)
			probe.setErrors, probe.getErrors = fixture.setErrors, fixture.getErrors
			err := restoreNativeStdioFlags(probe.fds, probe.flags, probe.fcntl)
			requireNativeRestoreResult(t, err, fixture.failure)
			if probe.setCalls != fixture.sets || probe.getCalls != fixture.gets {
				t.Error("restoration skipped a descriptor, retry, or verified readback")
			}
		})
	}
}

func TestNativeStdioRestoreRejectsTerminalSyscallFailures(t *testing.T) {
	injected := errors.New("synthetic-restore-token /private/synthetic/config")
	for _, fixture := range []struct {
		name                 string
		setErrors, getErrors [2][]error
	}{
		{"stdin_set_failure_matching_readback", [2][]error{{unix.EIO}, nil}, [2][]error{}},
		{"stdout_set_failure_matching_readback", [2][]error{nil, {unix.EBADF}}, [2][]error{}},
		{"stdin_get_failure", [2][]error{}, [2][]error{{unix.EIO}, nil}},
		{"stdout_get_failure", [2][]error{}, [2][]error{nil, {unix.EBADF}}},
		{"setter_injected_error", [2][]error{{injected}, nil}, [2][]error{}},
		{"getter_injected_error", [2][]error{}, [2][]error{nil, {injected}}},
		{"both_set_failures", [2][]error{{unix.EIO}, {unix.EBADF}}, [2][]error{}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			probe := newNativeRestoreProbe(t)
			probe.setErrors, probe.getErrors = fixture.setErrors, fixture.getErrors
			err := restoreNativeStdioFlags(probe.fds, probe.flags, probe.fcntl)
			requireNativeRestoreResult(t, err, true)
			if probe.setCalls != [2]int{1, 1} || probe.getCalls != [2]int{1, 1} {
				t.Error("terminal failure was retried or prevented the other descriptor being restored and checked")
			}
		})
	}
}

func TestNativeStdioRestoreChecksAllReadbackFlags(t *testing.T) {
	const darwinWasWritten = 0x10000
	// These literal Darwin async/dsync bits also exercise strict preservation of
	// bits without portable x/sys constants on every Unix target.
	const darwinAsync, darwinDataSync = 0x40, 0x400000
	for _, fixture := range []struct {
		name          string
		before, after [2]int
		failure       bool
	}{
		{"unchanged", [2]int{0, 1}, [2]int{0, 1}, false},
		{"original_nonblocking_unchanged", [2]int{unix.O_NONBLOCK, 1 | unix.O_NONBLOCK}, [2]int{unix.O_NONBLOCK, 1 | unix.O_NONBLOCK}, false},
		{"stdout_written", [2]int{0, 1}, [2]int{0, 1 | darwinWasWritten}, runtime.GOOS != "darwin"},
		{"stdout_already_written_unchanged", [2]int{0, 1 | darwinWasWritten}, [2]int{0, 1 | darwinWasWritten}, false},
		{"original_nonblocking_stdout_written", [2]int{unix.O_NONBLOCK, 1 | unix.O_NONBLOCK}, [2]int{unix.O_NONBLOCK, 1 | unix.O_NONBLOCK | darwinWasWritten}, runtime.GOOS != "darwin"},
		{"stdout_write_history_cleared", [2]int{0, 1 | darwinWasWritten}, [2]int{0, 1}, true},
		{"stdin_write_history_added", [2]int{0, 1}, [2]int{darwinWasWritten, 1}, true},
		{"stdin_nonblocking_added", [2]int{0, 1}, [2]int{unix.O_NONBLOCK, 1}, true},
		{"stdin_nonblocking_removed", [2]int{unix.O_NONBLOCK, 1}, [2]int{0, 1}, true},
		{"stdout_nonblocking_added", [2]int{0, 1}, [2]int{0, 1 | unix.O_NONBLOCK}, true},
		{"stdout_nonblocking_removed", [2]int{0, 1 | unix.O_NONBLOCK}, [2]int{0, 1}, true},
		{"stdout_written_and_nonblocking_added", [2]int{0, 1}, [2]int{0, 1 | darwinWasWritten | unix.O_NONBLOCK}, true},
		{"stdout_append_added", [2]int{0, 1}, [2]int{0, 1 | unix.O_APPEND}, true},
		{"stdout_append_removed", [2]int{0, 1 | unix.O_APPEND}, [2]int{0, 1}, true},
		{"stdout_async_bit_added", [2]int{0, 1}, [2]int{0, 1 | darwinAsync}, true},
		{"stdout_sync_added", [2]int{0, 1}, [2]int{0, 1 | unix.O_SYNC}, true},
		{"stdout_dsync_bit_added", [2]int{0, 1}, [2]int{0, 1 | darwinDataSync}, true},
		{"stdin_access_writeonly", [2]int{0, 1}, [2]int{unix.O_WRONLY, 1}, true},
		{"stdin_access_readwrite", [2]int{0, 1}, [2]int{unix.O_RDWR, 1}, true},
		{"stdout_access_readonly", [2]int{0, 1}, [2]int{0, unix.O_RDONLY}, true},
		{"stdout_access_readwrite", [2]int{0, 1}, [2]int{0, unix.O_RDWR}, true},
		{"stdout_unknown_bit_added", [2]int{0, 1}, [2]int{0, 0x10000001}, true},
		{"stdout_unknown_bit_removed", [2]int{0, 0x10000001}, [2]int{0, 1}, true},
		{"stdin_unknown_bit_added", [2]int{0, 1}, [2]int{0x10000000, 1}, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			probe := newNativeRestoreProbe(t)
			probe.flags, probe.readback = fixture.before, fixture.after
			err := restoreNativeStdioFlags(probe.fds, probe.flags, probe.fcntl)
			requireNativeRestoreResult(t, err, fixture.failure)
			if probe.setCalls != [2]int{1, 1} || probe.getCalls != [2]int{1, 1} {
				t.Error("readback skipped a descriptor or retried a non-interrupted operation")
			}
		})
	}
}

func TestNativeStdioConstructorRollbackChecksRestoration(t *testing.T) {
	injected := errors.New("synthetic-restore-token /private/synthetic/config")
	for _, fixture := range []struct {
		name                  string
		before, after         [2]int
		closeErrors           [2]error
		setErrors, getErrors  [2][]error
		restoreErr            error
		failure, closeFailure bool
	}{
		{"successful_restore_preserves_configuration_error", [2]int{0, 1}, [2]int{0, 1}, [2]error{}, [2][]error{}, [2][]error{}, nil, false, false},
		{"successful_nonblocking_restore_preserves_configuration_error", [2]int{unix.O_NONBLOCK, 1 | unix.O_NONBLOCK}, [2]int{unix.O_NONBLOCK, 1 | unix.O_NONBLOCK}, [2]error{}, [2][]error{}, [2][]error{}, nil, false, false},
		{"stdin_terminal_set_failure_matching_readback", [2]int{0, 1}, [2]int{0, 1}, [2]error{}, [2][]error{{unix.EIO}, nil}, [2][]error{}, nil, true, false},
		{"stdout_terminal_set_failure_matching_readback", [2]int{0, 1}, [2]int{0, 1}, [2]error{}, [2][]error{nil, {unix.EBADF}}, [2][]error{}, nil, true, false},
		{"stdin_get_failure", [2]int{0, 1}, [2]int{0, 1}, [2]error{}, [2][]error{}, [2][]error{{unix.EIO}, nil}, nil, true, false},
		{"stdout_get_failure", [2]int{0, 1}, [2]int{0, 1}, [2]error{}, [2][]error{}, [2][]error{nil, {unix.EBADF}}, nil, true, false},
		{"stdout_nonblocking_mismatch", [2]int{0, 1}, [2]int{0, 1 | unix.O_NONBLOCK}, [2]error{}, [2][]error{}, [2][]error{}, nil, true, false},
		{"stdout_access_mismatch", [2]int{0, 1}, [2]int{0, unix.O_RDWR}, [2]error{}, [2][]error{}, [2][]error{}, nil, true, false},
		{"stdout_unknown_bit_mismatch", [2]int{0, 1}, [2]int{0, 0x10000001}, [2]error{}, [2][]error{}, [2][]error{}, nil, true, false},
		{"stdin_flags_mismatch", [2]int{0, 1}, [2]int{unix.O_NONBLOCK, 1}, [2]error{}, [2][]error{}, [2][]error{}, nil, true, false},
		{"syscall_injected_error", [2]int{0, 1}, [2]int{0, 1}, [2]error{}, [2][]error{{injected}, nil}, [2][]error{}, nil, true, false},
		{"restore_injected_error_priority", [2]int{0, 1}, [2]int{0, 1}, [2]error{}, [2][]error{}, [2][]error{}, injected, true, false},
		{"restore_fixed_error_priority", [2]int{0, 1}, [2]int{0, 1}, [2]error{}, [2][]error{}, [2][]error{}, errNativeStdioCleanup, true, false},
		{"first_close_failure_matching_readback", [2]int{0, 1}, [2]int{0, 1}, [2]error{unix.EIO, nil}, [2][]error{}, [2][]error{}, nil, true, true},
		{"second_close_failure_matching_readback", [2]int{0, 1}, [2]int{0, 1}, [2]error{nil, unix.EBADF}, [2][]error{}, [2][]error{}, nil, true, true},
		{"both_close_failures_matching_readback", [2]int{0, 1}, [2]int{0, 1}, [2]error{unix.EIO, unix.EBADF}, [2][]error{}, [2][]error{}, nil, true, true},
		{"close_injected_error_priority", [2]int{0, 1}, [2]int{0, 1}, [2]error{injected, nil}, [2][]error{}, [2][]error{}, nil, true, true},
		{"close_and_restore_failure_priority", [2]int{0, 1}, [2]int{0, 1}, [2]error{injected, nil}, [2][]error{}, [2][]error{}, injected, true, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			probe := newNativeRestoreProbe(t)
			probe.flags, probe.readback = fixture.before, fixture.after
			probe.setErrors, probe.getErrors = fixture.setErrors, fixture.getErrors
			var closeCalls [2]int
			restoreCalls := 0
			closeCopy := func(index int) error {
				closeCalls[index]++
				if probe.setCalls != [2]int{} || probe.getCalls != [2]int{} {
					t.Error("constructor rollback closed its owned copies after restoration began")
				}
				return fixture.closeErrors[index]
			}
			closeOwned := newNativeStdioClose([2]func() error{
				func() error { return closeCopy(0) },
				func() error { return closeCopy(1) },
			})
			err := rollbackNativeStdio(closeOwned, func() error {
				restoreCalls++
				result := restoreNativeStdioFlags(probe.fds, probe.flags, func(fd uintptr, command, argument int) (int, error) {
					if closeCalls != [2]int{1, 1} {
						t.Error("constructor rollback attempted a syscall before closing its owned copies exactly once")
					}
					return probe.fcntl(fd, command, argument)
				})
				if fixture.restoreErr != nil {
					return fixture.restoreErr
				}
				return result
			})
			if closeCalls != [2]int{1, 1} || restoreCalls != 1 || probe.setCalls != [2]int{1, 1} {
				t.Error("constructor rollback skipped a close, restoration, or original descriptor")
			}
			if fixture.failure {
				requireNativeRestoreResult(t, err, true)
				if probe.getCalls != [2]int{1, 1} {
					t.Error("constructor rollback did not check both original descriptors")
				}
			} else {
				if err != ErrConfiguration {
					t.Error("successful constructor rollback changed the original configuration failure")
				}
				if probe.getCalls[0] > 1 || probe.getCalls[1] > 1 {
					t.Error("constructor rollback retried an uninterrupted readback")
				}
			}
			var closeResults [2]error
			for attempt := range closeResults {
				closeResults[attempt] = closeOwned()
				if fixture.closeFailure {
					requireNativeStdioCleanupError(t, closeResults[attempt])
				} else if closeResults[attempt] != nil {
					t.Error("successful rollback close acquired an error on a later call")
				}
			}
			if closeCalls != [2]int{1, 1} || closeResults[0] != closeResults[1] {
				t.Error("constructor rollback retried a close or lost its memoized result")
			}
		})
	}
}

func TestNativeStdioConstructorRollbackClosesTemporaryDuplicate(t *testing.T) {
	injected := errors.New("synthetic-temporary-close-token /private/synthetic/config")
	for _, fixture := range []struct {
		name                  string
		temporary             bool
		temporaryErr          error
		owned                 [2]bool
		ownedErrors           [2]error
		restoreErr            error
		failure, ownedFailure bool
	}{
		{"no_temporary_no_owned_copies", false, nil, [2]bool{}, [2]error{}, nil, false, false},
		{"no_temporary_first_owned_copy", false, nil, [2]bool{true, false}, [2]error{}, nil, false, false},
		{"temporary_first_duplicate_success", true, nil, [2]bool{}, [2]error{}, nil, false, false},
		{"temporary_second_duplicate_success", true, nil, [2]bool{true, false}, [2]error{}, nil, false, false},
		{"temporary_close_failure_without_owned_copy", true, unix.EIO, [2]bool{}, [2]error{}, nil, true, false},
		{"temporary_close_failure_with_first_owned_copy", true, unix.EBADF, [2]bool{true, false}, [2]error{}, nil, true, false},
		{"temporary_close_injected_error", true, injected, [2]bool{true, false}, [2]error{}, nil, true, false},
		{"temporary_and_owned_close_failures", true, injected, [2]bool{true, false}, [2]error{unix.EIO, nil}, nil, true, true},
		{"temporary_success_owned_close_failure", true, nil, [2]bool{true, false}, [2]error{injected, nil}, nil, true, true},
		{"temporary_and_restore_failure", true, injected, [2]bool{true, false}, [2]error{}, injected, true, false},
		{"temporary_success_restore_failure", true, nil, [2]bool{true, false}, [2]error{}, injected, true, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			probe := newNativeRestoreProbe(t)
			temporaryCalls, restoreCalls := 0, 0
			var ownedAttempts, ownedCloses [2]int
			var order []string
			closeCopy := func(index int) error {
				ownedAttempts[index]++
				if index == 0 {
					order = append(order, "owned0")
				} else {
					order = append(order, "owned1")
				}
				if probe.setCalls != [2]int{} || probe.getCalls != [2]int{} {
					t.Error("constructor closed an owned copy after restoration began")
				}
				if fixture.owned[index] {
					ownedCloses[index]++
					return fixture.ownedErrors[index]
				}
				return nil
			}
			closeOwned := newNativeStdioClose([2]func() error{
				func() error { return closeCopy(0) },
				func() error { return closeCopy(1) },
			})
			var closeTemporary func() error
			if fixture.temporary {
				closeTemporary = func() error {
					temporaryCalls++
					order = append(order, "temporary")
					if ownedAttempts != [2]int{} || probe.setCalls != [2]int{} || probe.getCalls != [2]int{} {
						t.Error("temporary duplicate close did not precede owned closes and restoration")
					}
					return fixture.temporaryErr
				}
			}
			closeConstructorCopies := newNativeStdioClose([2]func() error{closeTemporary, closeOwned})
			err := rollbackNativeStdio(closeConstructorCopies, func() error {
				restoreCalls++
				order = append(order, "restore")
				result := restoreNativeStdioFlags(probe.fds, probe.flags, func(fd uintptr, command, argument int) (int, error) {
					if ownedAttempts != [2]int{1, 1} || (fixture.temporary && temporaryCalls != 1) {
						t.Error("constructor attempted restoration before every duplicate close completed")
					}
					return probe.fcntl(fd, command, argument)
				})
				if fixture.restoreErr != nil {
					return fixture.restoreErr
				}
				return result
			})
			if fixture.failure {
				requireNativeStdioCleanupError(t, err)
				if probe.getCalls != [2]int{1, 1} {
					t.Error("constructor failure did not check both original descriptors")
				}
			} else {
				if err != ErrConfiguration {
					t.Error("successful temporary rollback changed the configuration failure")
				}
				if probe.getCalls[0] > 1 || probe.getCalls[1] > 1 {
					t.Error("constructor retried an uninterrupted readback")
				}
			}
			var constructorResults, ownedResults [3]error
			for attempt := range constructorResults {
				constructorResults[attempt] = closeConstructorCopies()
				ownedResults[attempt] = closeOwned()
				if fixture.temporaryErr != nil || fixture.ownedFailure {
					requireNativeStdioCleanupError(t, constructorResults[attempt])
				} else if constructorResults[attempt] != nil {
					t.Error("successful constructor closes acquired an error on a later call")
				}
				if fixture.ownedFailure {
					requireNativeStdioCleanupError(t, ownedResults[attempt])
				} else if ownedResults[attempt] != nil {
					t.Error("successful owned closes acquired an error on a later call")
				}
			}
			wantTemporaryCalls := 0
			wantOrder := "owned0,owned1,restore"
			if fixture.temporary {
				wantTemporaryCalls = 1
				wantOrder = "temporary," + wantOrder
			}
			var wantOwnedCloses [2]int
			for index, present := range fixture.owned {
				if present {
					wantOwnedCloses[index] = 1
				}
			}
			if temporaryCalls != wantTemporaryCalls || ownedAttempts != [2]int{1, 1} || ownedCloses != wantOwnedCloses || restoreCalls != 1 || probe.setCalls != [2]int{1, 1} || strings.Join(order, ",") != wantOrder {
				t.Error("constructor skipped, reordered, or retried a temporary close, owned close, or restoration")
			}
			if constructorResults[0] != constructorResults[1] || constructorResults[1] != constructorResults[2] || ownedResults[0] != ownedResults[1] || ownedResults[1] != ownedResults[2] {
				t.Error("constructor or owned close lost its memoized result")
			}
		})
	}
}
