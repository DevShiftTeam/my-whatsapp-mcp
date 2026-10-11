//go:build !darwin && !linux

package workerlife

import (
	"context"
	"os"
	"os/exec"
)

func Start(context.Context, Config, *exec.Cmd, []*os.File) (*Lease, error) {
	return nil, ErrUnavailable
}
func RunInherited() error           { return ErrUnavailable }
func probeNative(int) (bool, error) { return false, ErrUnavailable }
