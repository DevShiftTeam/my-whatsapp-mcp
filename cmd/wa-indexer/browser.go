package main

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"time"
)

// Native openers receive a single literal URL argument, never shell input.
func browserCommand(ctx context.Context, operatingSystem, ownerURL string) (*exec.Cmd, error) {
	switch operatingSystem {
	case "darwin":
		return exec.CommandContext(ctx, "/usr/bin/open", ownerURL), nil
	case "windows":
		return exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", ownerURL), nil
	case "linux":
		return exec.CommandContext(ctx, "xdg-open", ownerURL), nil
	default:
		return nil, errors.New("browser_unavailable")
	}
}

func openBrowser(ctx context.Context, ownerURL string) error {
	command, err := browserCommand(ctx, runtime.GOOS, ownerURL)
	if err != nil {
		return err
	}
	return runBrowserCommand(command)
}

func runBrowserCommand(command *exec.Cmd) error {
	// Nil output goes directly to the null device. Copy pipes would let a
	// detached browser keep Run waiting after its native opener has exited.
	command.WaitDelay = 250 * time.Millisecond
	if err := command.Run(); err != nil {
		return errors.New("browser_unavailable")
	}
	return nil
}
