//go:build darwin || linux

package workerlife

import (
	"errors"
	"io"
	"os"
	"runtime"
	"time"
)

// RunInherited serves the private holder protocol on fd3/fd4. Guard descriptors
// fd5..fd12 are inherited before Go initialization and remain kernel-owned even
// if INIT never arrives. Uncertain state intentionally keeps this process alive.
func RunInherited() error {
	control := os.NewFile(3, "workerlife-control")
	reply := os.NewFile(4, "workerlife-reply")
	if control != nil {
		defer control.Close()
	}
	if reply != nil {
		defer reply.Close()
	}
	wait := func() { time.Sleep(100 * time.Millisecond) }
	if control == nil || reply == nil {
		return retainHolder(nil, wait)
	}
	return runHolder(control, reply, openInheritedGuards, probeNative, wait)
}

func openInheritedGuards(count int) ([]*os.File, error) {
	if count < 0 || count > maxHolderGuards {
		return nil, ErrLimit
	}
	guards := make([]*os.File, 0, count)
	for index := 0; index < count; index++ {
		file := os.NewFile(uintptr(5+index), "workerlife-guard")
		if file == nil {
			return guards, ErrProtocol
		}
		guards = append(guards, file)
	}
	return guards, nil
}

func runHolder(control io.Reader, reply io.Writer,
	openGuards func(int) ([]*os.File, error), probe func(int) (bool, error), wait func(),
) error {
	frame, err := readHolderFrame(control)
	if err != nil {
		return retainHolder(nil, wait)
	}
	count, err := parseInit(frame)
	if err != nil {
		return retainHolder(nil, wait)
	}
	guards, err := openGuards(count)
	if err != nil || len(guards) != count {
		return retainHolder(guards, wait)
	}
	for _, file := range guards {
		if file == nil {
			return retainHolder(guards, wait)
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return retainHolder(guards, wait)
		}
	}
	if err := writeHolderFrame(reply, "READY"); err != nil {
		return retainHolder(guards, wait)
	}
	frame, err = readHolderFrame(control)
	if err != nil {
		return retainHolder(guards, wait)
	}
	pid, noop, err := parseRegistration(frame)
	if err != nil {
		return retainHolder(guards, wait)
	}
	// ACK delivery can fail when the owner dies after supplying a complete
	// registration. Preserve that known PID/NOOP instead of losing ownership.
	_ = writeHolderFrame(reply, "ACK")
	// The parent closes its writer after ACK. Require clean EOF before release,
	// so a queued duplicate/trailing byte cannot race NOOP or an absent PID.
	var trailing [1]byte
	n, err := control.Read(trailing[:])
	if n != 0 || err != io.EOF {
		return retainHolder(guards, wait)
	}
	if noop {
		return closeHolderGuards(guards)
	}
	for {
		absent, err := probe(pid)
		if err == nil && absent {
			return closeHolderGuards(guards)
		}
		wait()
		runtime.KeepAlive(guards)
	}
}

func retainHolder(guards []*os.File, wait func()) error {
	for {
		wait()
		// Keep wrappers alive so uncertain ownership cannot invoke finalizers.
		runtime.KeepAlive(guards)
	}
}

func closeHolderGuards(guards []*os.File) error {
	var failures []error
	for _, file := range guards {
		if err := file.Close(); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		return errors.Join(append([]error{ErrCleanup}, failures...)...)
	}
	return nil
}
