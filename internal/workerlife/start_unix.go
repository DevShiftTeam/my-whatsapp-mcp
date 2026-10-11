//go:build darwin || linux

package workerlife

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func newLaunchFailure(stage launchStage, cause error) LaunchFailure {
	failure := LaunchFailure{stage: stage}
	var errno syscall.Errno
	if errors.As(cause, &errno) && errno != 0 && uint64(errno) <= uint64(^uint32(0)) {
		failure.errnoKnown = true
		failure.errno = uint32(errno)
	}
	return failure
}

// Start starts and acknowledges the independent holder before native.Start.
// Guards are borrowed and copied with dup, retaining the original flock OFD.
// A nonnil lease returned with an error must be retained and cleaned up.
func Start(ctx context.Context, config Config, native *exec.Cmd, guards []*os.File) (*Lease, error) {
	if ctx == nil || native == nil || native.Process != nil || native.Cancel != nil || native.SysProcAttr == nil || !native.SysProcAttr.Setpgid || native.SysProcAttr.Pgid != 0 || native.SysProcAttr.Setsid {
		return nil, ErrProtocol
	}
	if len(guards) > maxHolderGuards {
		return nil, ErrLimit
	}
	if config.ReadyTimeout == 0 {
		config.ReadyTimeout = time.Second
	}
	if config.ReadyTimeout <= 0 || config.ReadyTimeout > 10*time.Second {
		return nil, ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	executable, err := executableFile(config)
	if err != nil {
		return nil, err
	}
	defer executable.Close()
	copies := make([]*os.File, 0, len(guards))
	closeCopies := func() {
		for _, file := range copies {
			_ = file.Close()
		}
	}
	for _, guard := range guards {
		file, err := duplicateRegular(guard)
		if err != nil {
			closeCopies()
			return nil, err
		}
		copies = append(copies, file)
	}
	helperPath, preparedExecutable, err := helperExecutable(ctx, config, executable, 5+len(copies))
	closePrepared := func(cause error) (*Lease, error) {
		if preparedExecutable != nil {
			if preparedExecutable.Close() != nil {
				// No helper or native start was attempted, or os/exec positively
				// failed helper.Start. Keep private-copy and guard cleanup owned.
				return neverStartedLease(copies, preparedExecutable), cause
			}
		}
		closeCopies()
		return nil, cause
	}
	if err != nil {
		return closePrepared(err)
	}
	if err := ctx.Err(); err != nil {
		return closePrepared(err)
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		return closePrepared(ErrUnavailable)
	}
	replyRead, replyWrite, err := os.Pipe()
	if err != nil {
		controlRead.Close()
		controlWrite.Close()
		return closePrepared(ErrUnavailable)
	}
	args := append([]string(nil), config.Args...)
	if config.Args == nil {
		args = []string{HolderModeArgument}
	}
	helper := exec.Command(helperPath, args...)
	helper.Env = helperEnvironment(os.Environ(), config.Environment)
	helper.Stdin = nil
	helper.Stdout, helper.Stderr = io.Discard, io.Discard
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	helper.ExtraFiles = append([]*os.File{controlRead, replyWrite}, copies...)
	helper.ExtraFiles = append(helper.ExtraFiles, executable)
	if err := helper.Start(); err != nil {
		controlRead.Close()
		controlWrite.Close()
		replyRead.Close()
		replyWrite.Close()
		return closePrepared(newLaunchFailure(holderLaunchStage, err))
	}
	controlRead.Close()
	replyWrite.Close()
	lease := &Lease{guards: copies, executable: preparedExecutable, control: controlWrite, reply: replyRead, helperExited: make(chan struct{}), protocolDone: make(chan struct{}), decision: make(chan int, 1), ready: make(chan error, 1)}
	go func() { _ = helper.Wait(); close(lease.helperExited) }()
	// Protocol ownership outlives ctx. Each frame is tiny and finite; an
	// unresponsive helper retains guards instead of authorizing early cleanup.
	go lease.runProtocol(len(copies))
	readyContext, cancel := context.WithTimeout(ctx, config.ReadyTimeout)
	defer cancel()
	select {
	case err = <-lease.ready:
		if err != nil {
			lease.decision <- 0
			return lease, err
		}
	case <-readyContext.Done():
		lease.decision <- 0
		return lease, readyContext.Err()
	}
	if err := ctx.Err(); err != nil {
		lease.decision <- 0
		return lease, err
	}
	originalFiles := native.ExtraFiles
	native.ExtraFiles = append(append([]*os.File(nil), originalFiles...), copies...)
	err = native.Start()
	native.ExtraFiles = originalFiles
	if err != nil {
		lease.decision <- 0
		return lease, newLaunchFailure(nativeLaunchStage, err)
	}
	lease.mu.Lock()
	lease.pid = native.Process.Pid
	lease.mu.Unlock()
	lease.decision <- native.Process.Pid
	select {
	case <-lease.protocolDone:
		lease.mu.Lock()
		acknowledged := lease.acknowledged
		lease.mu.Unlock()
		if !acknowledged {
			return lease, ErrProtocol
		}
		if err := ctx.Err(); err != nil {
			return lease, err
		}
		return lease, nil
	case <-readyContext.Done():
		return lease, readyContext.Err()
	}
}

func neverStartedLease(guards []*os.File, executable ExecutableLease) *Lease {
	lease := &Lease{guards: guards, executable: executable, helperExited: make(chan struct{}), protocolDone: make(chan struct{}), acknowledged: true}
	close(lease.helperExited)
	close(lease.protocolDone)
	return lease
}

func (lease *Lease) runProtocol(count int) {
	defer close(lease.protocolDone)
	defer lease.control.Close()
	if writeHolderFrame(lease.control, "INIT "+strconv.Itoa(count)) != nil {
		lease.ready <- ErrProtocol
		return
	}
	frame, err := readHolderFrame(lease.reply)
	if err != nil || frame != "READY" {
		lease.ready <- ErrProtocol
		return
	}
	lease.ready <- nil
	pid := <-lease.decision
	registration := "NOOP"
	if pid > 0 {
		registration = "GROUP " + strconv.Itoa(pid)
	}
	if writeHolderFrame(lease.control, registration) != nil {
		return
	}
	frame, err = readHolderFrame(lease.reply)
	if err != nil || frame != "ACK" {
		return
	}
	lease.mu.Lock()
	lease.acknowledged = true
	lease.mu.Unlock()
	// EOF terminates exactly one registration; it never signifies no ownership.
	_ = lease.control.Close()
}
