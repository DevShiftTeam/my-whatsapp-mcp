//go:build darwin || linux

package modelstore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/workerlife"
	"golang.org/x/sys/unix"
)

// The native fixture deliberately closes its own guard copy. Only the separate
// holder can then prevent removal after the application parent receives SIGKILL.
func TestModelWorkerGuardAfterParentSIGKILL(t *testing.T) {
	switch os.Getenv("WA_MODEL_GUARD_ROLE") {
	case "holder":
		modelGuardRecordPID(t, "holder")
		time.AfterFunc(15*time.Second, func() { os.Exit(85) }) // Fixture failsafe.
		if err := workerlife.RunInherited(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(os.Getenv("WA_MODEL_GUARD_REGISTRY"), "holder.done"), []byte("normal\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return
	case "native", "descendant":
		time.AfterFunc(12*time.Second, func() { os.Exit(86) }) // Fixture failsafe.
		if os.Getenv("WA_MODEL_GUARD_ROLE") == "native" {
			modelGuardRecordPID(t, "native")
		}
		input := os.NewFile(3, "model-input")
		defer input.Close()
		var data [6]byte
		if _, err := input.ReadAt(data[:], 0); err != nil || string(data[:]) != "abcdef" {
			t.Fatalf("inherited verified model input: %q %v", data, err)
		}
		if os.Getenv("WA_MODEL_GUARD_ROLE") == "native" {
			if err := unix.Close(4); err != nil { // Appended guard; only holder stays.
				t.Fatal("native guard descriptor missing", err)
			}
			child := exec.Command(os.Args[0], "-test.run=^TestModelWorkerGuardAfterParentSIGKILL$")
			child.Env = append(os.Environ(), "WA_MODEL_GUARD_ROLE=descendant")
			child.ExtraFiles = []*os.File{input} // Keeps verified input in this PGID.
			child.Stderr = os.Stderr
			output, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
			if line := modelGuardLine(t, bufio.NewReaderSize(output, 128)); line != "descendant-ready" {
				t.Fatalf("descendant readiness: %q", line)
			}
			fmt.Fprintln(os.Stdout, "native-ready")
		} else {
			fmt.Fprintln(os.Stdout, "descendant-ready")
		}
		for {
			time.Sleep(time.Hour)
		}
	case "parent":
		runModelGuardParent(t)
		return
	}
	s := installedGuardStore(t)
	registry := t.TempDir()
	cache := filepath.Join(registry, "native-exec")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(os.Args[0], "-test.run=^TestModelWorkerGuardAfterParentSIGKILL$")
	parent.Env = append(os.Environ(), "WA_MODEL_GUARD_ROLE=parent", "WA_MODEL_GUARD_ROOT="+s.root.path, "WA_MODEL_GUARD_REGISTRY="+registry)
	// Register fixture-only cleanup authority before any child startup/readiness
	// wait. Both native and independent holder record their actual isolated PGID
	// before running; product code never obtains helper kill authority.
	defer modelGuardCleanup(parent, registry)
	output, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	parent.Stderr = os.Stderr
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReaderSize(output, 128)
	line := modelGuardLine(t, reader)
	pid, err := strconv.Atoi(line)
	if err != nil || pid <= 1 || strconv.Itoa(pid) != line {
		t.Fatalf("native PID: %q %v", line, err)
	}
	defer unix.Kill(-pid, unix.SIGKILL)
	if line := modelGuardLine(t, reader); line != "parent-ready" {
		t.Fatalf("parent readiness: %q", line)
	}
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Wait(); err == nil {
		t.Fatal("app parent survived SIGKILL")
	}
	if err := unix.Kill(pid, 0); err != nil {
		t.Fatal("native exited with the killed app parent", err)
	}
	modelGuardCacheRetained(t, cache)
	if err := s.Remove(context.Background(), "test-profile"); !errors.Is(err, ErrActiveLease) {
		t.Fatalf("app SIGKILL released live model ownership: %v", err)
	}
	if err := unix.Kill(pid, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	modelGuardLeaderGone(t, pid)
	if err := unix.Kill(-pid, 0); err != nil {
		t.Fatal("descendant did not survive the native leader", err)
	}
	if err := s.Remove(context.Background(), "test-profile"); !errors.Is(err, ErrActiveLease) {
		t.Fatalf("leader exit released the live descendant's model: %v", err)
	}
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = s.Remove(context.Background(), "test-profile")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrActiveLease) || time.Now().After(deadline) {
			t.Fatal("holder failed to release after native group exit", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	modelGuardCompletion(t, registry)
	// Remove has reacquired the existing model guard, and completion checks the
	// recorded holder's positive absence. Only now may stale copy recovery run.
	modelGuardRecoverCache(t, cache)
}

func runModelGuardParent(t *testing.T) {
	t.Helper()
	s, err := Open(os.Getenv("WA_MODEL_GUARD_ROOT"), catalogFor(component("model", []byte("abcdef"), "")), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	l, err := s.Acquire(context.Background(), "test-profile")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	input, err := l.Open("model")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	guard, err := l.WorkerGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	namespace := modelGuardNamespace(t, filepath.Join(os.Getenv("WA_MODEL_GUARD_REGISTRY"), "native-exec"))
	defer namespace.Close()
	// The fixture owns its model guard and no helper has started yet.
	if err := namespace.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	native := exec.Command(os.Args[0], "-test.run=^TestModelWorkerGuardAfterParentSIGKILL$")
	native.Env = append(os.Environ(), "WA_MODEL_GUARD_ROLE=native")
	native.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	native.ExtraFiles = []*os.File{input}
	output, err := native.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	native.Stderr = os.Stderr
	holder, err := workerlife.Start(context.Background(), workerlife.Config{Executable: os.Args[0], ExecutableProvider: namespace, Args: []string{"-test.run=^TestModelWorkerGuardAfterParentSIGKILL$"}, Environment: append(os.Environ(), "WA_MODEL_GUARD_ROLE=holder"), ReadyTimeout: 2 * time.Second}, native, []*os.File{guard})
	defer func() {
		if native.Process != nil {
			_ = unix.Kill(-native.Process.Pid, unix.SIGKILL)
			_ = native.Wait()
		}
		if holder != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = holder.AwaitCleanup(ctx)
		}
	}()
	if native.Process != nil {
		fmt.Fprintln(os.Stdout, native.Process.Pid)
	}
	if err != nil {
		t.Fatal(err)
	}
	if line := modelGuardLine(t, bufio.NewReaderSize(output, 128)); line != "native-ready" {
		t.Fatalf("native readiness: %q", line)
	}
	fmt.Fprintln(os.Stdout, "parent-ready")
	for {
		time.Sleep(time.Hour)
	}
}

func modelGuardLine(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := r.ReadSlice('\n')
		done <- result{string(line), err}
	}()
	select {
	case got := <-done:
		if got.err != nil || !strings.HasSuffix(got.line, "\n") {
			t.Fatalf("helper readiness: %q %v", got.line, got.err)
		}
		return strings.TrimSuffix(got.line, "\n")
	case <-time.After(10 * time.Second):
		t.Fatal("helper readiness timed out")
		return ""
	}
}

func modelGuardLeaderGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := unix.Kill(pid, 0)
		if errors.Is(err, unix.ESRCH) {
			return
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatal("native leader was not reaped", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func modelGuardRecordPID(t *testing.T, role string) {
	t.Helper()
	registry := os.Getenv("WA_MODEL_GUARD_REGISTRY")
	if registry == "" {
		t.Fatal("fixture process registry is missing")
	}
	pid := os.Getpid()
	group, err := unix.Getpgid(pid)
	if err != nil || group != pid {
		t.Fatalf("fixture %s is not an isolated process group: %d %v", role, group, err)
	}
	if err := os.WriteFile(filepath.Join(registry, role+".pid"), []byte(strconv.Itoa(pid)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func modelGuardCleanup(parent *exec.Cmd, registry string) {
	if parent.Process != nil {
		_ = parent.Process.Kill()
		_ = parent.Wait() // Parent cannot spawn a new group after this barrier.
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		found := 0
		for _, role := range []string{"native", "holder"} {
			b, err := os.ReadFile(filepath.Join(registry, role+".pid"))
			if err != nil {
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSuffix(string(b), "\n"))
			if err != nil || pid <= 1 {
				continue
			}
			found++
			if group, err := unix.Getpgid(pid); err == nil && group == pid {
				_ = unix.Kill(-group, unix.SIGKILL)
			}
		}
		if found == 2 || time.Now().After(deadline) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func modelGuardCompletion(t *testing.T, registry string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		b, err := os.ReadFile(filepath.Join(registry, "holder.done"))
		if err == nil && string(b) == "normal\n" {
			pidData, err := os.ReadFile(filepath.Join(registry, "holder.pid"))
			pid, parseErr := strconv.Atoi(strings.TrimSuffix(string(pidData), "\n"))
			if err != nil || parseErr != nil || pid <= 1 {
				t.Fatal("holder completion identity unavailable", err, parseErr)
			}
			modelGuardLeaderGone(t, pid)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture failsafe cannot prove normal holder cleanup", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type modelGuardGrowthLease struct{}

func (modelGuardGrowthLease) Release() error { return nil }

func modelGuardNamespace(t *testing.T, path string) *workerlife.ExecutableNamespace {
	t.Helper()
	directory, err := openDirectory(path, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.close()
	namespace, err := workerlife.NewExecutableNamespace(workerlife.NamespaceConfig{Directory: directory.file, Path: directory.path,
		Admit: func(ctx context.Context, maximum int64) (workerlife.GrowthLease, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if maximum <= 64<<10 || maximum > (64<<20)+(64<<10) {
				return nil, workerlife.ErrLimit
			}
			return modelGuardGrowthLease{}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return namespace
}

func modelGuardCacheRetained(t *testing.T, path string) {
	t.Helper()
	// Linux retains executable /dev/fd support and need not prepare a copy.
	if runtime.GOOS != "darwin" {
		return
	}
	names, err := os.ReadDir(path)
	if err != nil || len(names) == 0 {
		t.Fatal("parent death lost private executable ownership", err)
	}
}

func modelGuardRecoverCache(t *testing.T, path string) {
	t.Helper()
	namespace := modelGuardNamespace(t, path)
	defer namespace.Close()
	if err := namespace.Recover(context.Background()); err != nil {
		t.Fatal("post-death executable recovery", err)
	}
	names, err := os.ReadDir(path)
	if err != nil || len(names) != 0 {
		t.Fatal("recovered executable cache retains bytes", err)
	}
}
