//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package inference

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil
}
func signalProcessGroup(cmd *exec.Cmd, kill bool) {
	signal := syscall.SIGTERM
	if kill {
		signal = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, signal)
}
func processGroupExists(cmd *exec.Cmd) bool {
	return !errors.Is(syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH)
}
