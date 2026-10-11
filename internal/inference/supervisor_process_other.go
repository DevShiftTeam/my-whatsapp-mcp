//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd && !dragonfly

package inference

import "os/exec"

// Native process-tree isolation requires a supported platform implementation.
func configureProcessGroup(*exec.Cmd) error { return ErrUnavailable }
func signalProcessGroup(*exec.Cmd, bool)    {}
func processGroupExists(*exec.Cmd) bool     { return true }
