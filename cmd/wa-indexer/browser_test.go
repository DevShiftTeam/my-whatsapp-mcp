package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBrowserCommandPassesCapabilityAsOneArgument(t *testing.T) {
	const ownerURL = "http://127.0.0.1:43210/#bootstrap=private-token-SENTINEL"
	for _, test := range []struct {
		os   string
		name string
		args []string
	}{
		{"darwin", "open", []string{ownerURL}},
		{"linux", "xdg-open", []string{ownerURL}},
		{"windows", "rundll32", []string{"url.dll,FileProtocolHandler", ownerURL}},
	} {
		command, err := browserCommand(context.Background(), test.os, ownerURL)
		if err != nil || filepath.Base(command.Args[0]) != test.name || len(command.Args) != len(test.args)+1 {
			t.Fatal("browser command did not use the native opener")
		}
		for i, arg := range test.args {
			if command.Args[i+1] != arg {
				t.Fatal("browser capability was split or interpolated")
			}
		}
	}
	if _, err := browserCommand(context.Background(), "unsupported", ownerURL); err == nil || strings.Contains(err.Error(), "SENTINEL") {
		t.Fatal("unsupported browser error exposed its capability")
	}
}

func TestOpenBrowserDoesNotWaitForDetachedBrowserOutput(t *testing.T) {
	var name string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "linux":
		name = "xdg-open"
	default:
		t.Skip("POSIX native-opener process fixture")
	}
	toolsDir := t.TempDir()
	// The launched desktop browser survives its opener. Its inherited output
	// descriptors must not keep the CLI waiting after the opener has exited.
	// Include startup above the former 200 ms test-only deadline, then hold the
	// detached child until the test explicitly releases it.
	ready := filepath.Join(toolsDir, "child-ready")
	release := filepath.Join(toolsDir, "child-release")
	exited := filepath.Join(toolsDir, "child-exited")
	writeTestExecutable(t, filepath.Join(toolsDir, name), `#!/bin/sh
sleep 0.3
(
  : > "$WA_INDEXER_BROWSER_CHILD_READY"
  attempts=0
  while [ ! -e "$WA_INDEXER_BROWSER_CHILD_RELEASE" ] && [ "$attempts" -lt 500 ]; do
    sleep 0.01
    attempts=$((attempts + 1))
  done
  : > "$WA_INDEXER_BROWSER_CHILD_EXITED"
) &
exit 0
`)

	t.Setenv("WA_INDEXER_BROWSER_CHILD_READY", ready)
	t.Setenv("WA_INDEXER_BROWSER_CHILD_RELEASE", release)
	t.Setenv("WA_INDEXER_BROWSER_CHILD_EXITED", exited)
	t.Cleanup(func() {
		if err := os.WriteFile(release, nil, 0600); err != nil {
			t.Error("could not release the detached browser fixture")
		}
		if _, err := os.Stat(ready); err == nil && !waitForBrowserFixture(exited) {
			t.Error("detached browser fixture did not exit after release")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runBrowserCommand(exec.CommandContext(ctx, filepath.Join(toolsDir, name), "http://127.0.0.1:43210/#bootstrap=private-token-SENTINEL"))
	}()
	if !waitForBrowserFixture(ready) {
		t.Fatal("detached browser fixture did not start")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("browser opener did not finish while detached output stayed open")
		}
	case <-ctx.Done():
		t.Fatal("browser opener waited for detached output to close")
	}
	if _, err := os.Stat(exited); !os.IsNotExist(err) {
		t.Fatal("detached browser exited before its output-wait check")
	}
}

func waitForBrowserFixture(path string) bool {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		select {
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
}

func TestDarwinOpenerUsesAbsoluteSystemExecutable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	command, err := browserCommand(context.Background(), "darwin", "http://127.0.0.1:1234/")
	if err != nil || command.Path != "/usr/bin/open" || command.Args[0] != "/usr/bin/open" {
		t.Fatal("macOS opener depends on ambient PATH")
	}
}
