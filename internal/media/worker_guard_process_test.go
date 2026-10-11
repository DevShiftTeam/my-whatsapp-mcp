//go:build darwin || linux

package media

import (
	"bufio"
	"context"
	"encoding/json"
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

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/workerlife"
	"golang.org/x/sys/unix"
)

// Files, flock OFDs and process groups are real. This fixture uses a fake
// archive repository; the seven-checkpoint crash suite covers actual SQLite.
func TestOriginalWorkerGuardsAfterParentSIGKILL(t *testing.T) {
	switch os.Getenv("WA_MEDIA_GUARD_ROLE") {
	case "holder":
		originalGuardRecordPID(t, "holder")
		time.AfterFunc(15*time.Second, func() { os.Exit(85) }) // Fixture failsafe.
		if err := workerlife.RunInherited(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(os.Getenv("WA_MEDIA_GUARD_REGISTRY"), "holder.done"), []byte("normal\n"), 0600); err != nil {
			t.Fatal(err)
		}
		return
	case "native", "descendant":
		time.AfterFunc(12*time.Second, func() { os.Exit(86) }) // Fixture failsafe.
		if os.Getenv("WA_MEDIA_GUARD_ROLE") == "native" {
			originalGuardRecordPID(t, "native")
		}
		input := os.NewFile(3, "original-input")
		defer input.Close()
		decoded := os.Getenv("WA_MEDIA_GUARD_KIND") == "decoded"
		if decoded {
			var signature [13]byte
			if _, err := input.ReadAt(signature[:], 0); err != nil || string(signature[:]) != "decoded-input" {
				t.Fatalf("inherited sealed decoded input: %q %v", signature, err)
			}
		} else {
			var signature [8]byte
			if _, err := input.ReadAt(signature[:], 0); err != nil || string(signature[:]) != "\x89PNG\r\n\x1a\n" {
				t.Fatalf("inherited verified original: %q %v", signature, err)
			}
		}
		if os.Getenv("WA_MEDIA_GUARD_ROLE") == "native" {
			if err := unix.Close(4); err != nil {
				t.Fatal("native artifact guard descriptor missing", err)
			}
			if !decoded {
				if err := unix.Close(5); err != nil {
					t.Fatal("native reader guard descriptor missing", err)
				}
			}
			child := exec.Command(os.Args[0], "-test.run=^TestOriginalWorkerGuardsAfterParentSIGKILL$")
			child.Env = append(os.Environ(), "WA_MEDIA_GUARD_ROLE=descendant")
			child.ExtraFiles = []*os.File{input}
			child.Stderr = os.Stderr
			output, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
			if line := originalGuardLine(t, bufio.NewReaderSize(output, 128)); line != "descendant-ready" {
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
		if os.Getenv("WA_MEDIA_GUARD_KIND") == "decoded" {
			runDecodedGuardParent(t)
		} else {
			runOriginalGuardParent(t)
		}
		return
	}
	s, repository, original := retainedGuardOriginal(t)
	artifact := repository.artifacts[original.Metadata().ArtifactID]
	root := filepath.Dir(s.media.path)
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	registry := t.TempDir()
	cache := filepath.Join(registry, "native-exec")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(os.Args[0], "-test.run=^TestOriginalWorkerGuardsAfterParentSIGKILL$")
	parent.Env = append(os.Environ(), "WA_MEDIA_GUARD_ROLE=parent", "WA_MEDIA_GUARD_ROOT="+root, "WA_MEDIA_GUARD_ARTIFACT="+string(encoded), "WA_MEDIA_GUARD_REGISTRY="+registry)
	defer originalGuardCleanup(parent, registry) // Before any startup wait.
	output, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	parent.Stderr = os.Stderr
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReaderSize(output, 128)
	line := originalGuardLine(t, reader)
	pid, err := strconv.Atoi(line)
	if err != nil || pid <= 1 || strconv.Itoa(pid) != line {
		t.Fatalf("native PID: %q %v", line, err)
	}
	defer unix.Kill(-pid, unix.SIGKILL)
	if line := originalGuardLine(t, reader); line != "parent-ready" {
		t.Fatalf("parent readiness: %q", line)
	}
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Wait(); err == nil {
		t.Fatal("app parent survived SIGKILL")
	}
	if err := unix.Kill(pid, 0); err != nil {
		t.Fatal("native exited with killed app parent", err)
	}
	originalGuardCacheRetained(t, cache)
	paths := appdata.Paths{Root: root, MediaDir: filepath.Join(root, "media"), WorkingDir: filepath.Join(root, "working")}
	reopened, err := Open(paths, repository, Options{Admission: &physicalAdmission{}})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Evict(context.Background(), "media-id"); !errors.Is(err, ErrActiveUse) {
		t.Fatalf("app SIGKILL released live original ownership: %v", err)
	}
	originalGuardAssertArtifactBusy(t, reopened, artifact.ID)
	if repository.charge(archive.BudgetOriginals) != artifact.Bytes {
		t.Fatal("live original lost its retained charge")
	}
	if _, err := os.Stat(filepath.Join(paths.MediaDir, artifactName(artifact.ID, ".original"))); err != nil {
		t.Fatal("live native input was removed", err)
	}
	if err := unix.Kill(pid, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	originalGuardLeaderGone(t, pid)
	if err := unix.Kill(-pid, 0); err != nil {
		t.Fatal("descendant did not survive the native leader", err)
	}
	if err := reopened.Evict(context.Background(), "media-id"); !errors.Is(err, ErrActiveUse) {
		t.Fatalf("leader exit released the live descendant's original: %v", err)
	}
	originalGuardAssertArtifactBusy(t, reopened, artifact.ID)
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = reopened.Evict(context.Background(), "media-id")
		if err == nil {
			break
		}
		if !errors.Is(err, ErrActiveUse) || time.Now().After(deadline) {
			t.Fatal("holder failed to release after native group exit", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if repository.charge(archive.BudgetOriginals) != 0 {
		t.Fatal("stopped original cleanup retained its charge")
	}
	guard, err := artifactLock(context.Background(), reopened.media, artifactName(artifact.ID, ".lock"), true, false)
	if err != nil {
		t.Fatal("stopped artifact guard remained locked", err)
	}
	if err := releaseLock(guard); err != nil {
		t.Fatal(err)
	}
	originalGuardCompletion(t, registry)
	originalGuardRecoverCache(t, cache)
}

func runOriginalGuardParent(t *testing.T) {
	t.Helper()
	root := os.Getenv("WA_MEDIA_GUARD_ROOT")
	var artifact archive.MediaArtifact
	if err := json.Unmarshal([]byte(os.Getenv("WA_MEDIA_GUARD_ARTIFACT")), &artifact); err != nil {
		t.Fatal(err)
	}
	repository := newRepository()
	repository.artifacts[artifact.ID] = artifact
	repository.media[artifact.MediaID] = artifact.ID
	paths := appdata.Paths{Root: root, MediaDir: filepath.Join(root, "media"), WorkingDir: filepath.Join(root, "working")}
	s, err := Open(paths, repository, Options{Admission: &physicalAdmission{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	original, err := s.OpenOriginal(context.Background(), archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: 1}, artifact.MediaID)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	input, err := original.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	guards, err := original.WorkerGuards()
	if err != nil {
		t.Fatal(err)
	}
	defer closeGuardFiles(guards)
	startOriginalGuardParentWorker(t, input, guards)
}

func startOriginalGuardParentWorker(t *testing.T, input *os.File, guards []*os.File) {
	t.Helper()
	namespace := originalGuardNamespace(t, filepath.Join(os.Getenv("WA_MEDIA_GUARD_REGISTRY"), "native-exec"))
	defer namespace.Close()
	// The existing Original/Decoded and store guards are held; there is no
	// previously started helper in this fresh fixture namespace.
	if err := namespace.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	native := exec.Command(os.Args[0], "-test.run=^TestOriginalWorkerGuardsAfterParentSIGKILL$")
	native.Env = append(os.Environ(), "WA_MEDIA_GUARD_ROLE=native")
	native.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	native.ExtraFiles = []*os.File{input}
	output, err := native.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	native.Stderr = os.Stderr
	holder, err := workerlife.Start(context.Background(), workerlife.Config{Executable: os.Args[0], ExecutableProvider: namespace, Args: []string{"-test.run=^TestOriginalWorkerGuardsAfterParentSIGKILL$"}, Environment: append(os.Environ(), "WA_MEDIA_GUARD_ROLE=holder"), ReadyTimeout: 2 * time.Second}, native, guards)
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
	if line := originalGuardLine(t, bufio.NewReaderSize(output, 128)); line != "native-ready" {
		t.Fatalf("native readiness: %q", line)
	}
	fmt.Fprintln(os.Stdout, "parent-ready")
	for {
		time.Sleep(time.Hour)
	}
}

func originalGuardLine(t *testing.T, r *bufio.Reader) string {
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

func originalGuardLeaderGone(t *testing.T, pid int) {
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

func originalGuardAssertArtifactBusy(t *testing.T, s *Store, id archive.ArtifactID) {
	t.Helper()
	guard, err := artifactLock(context.Background(), s.media, artifactName(id, ".lock"), true, false)
	if !errors.Is(err, ErrActiveUse) {
		_ = releaseLock(guard)
		t.Fatal("live cross-process artifact guard was released", err)
	}
}

func originalGuardRecordPID(t *testing.T, role string) {
	t.Helper()
	registry := os.Getenv("WA_MEDIA_GUARD_REGISTRY")
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

func originalGuardCleanup(parent *exec.Cmd, registry string) {
	if parent.Process != nil {
		_ = parent.Process.Kill()
		_ = parent.Wait()
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

func originalGuardCompletion(t *testing.T, registry string) {
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
			originalGuardLeaderGone(t, pid)
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture failsafe cannot prove normal holder cleanup", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The same real-OS fixture now protects a sealed temporary input and its
// outstanding decoded reservation. Reconcile has only the artifact EX fence.
func TestDecodedWorkerGuardAfterParentSIGKILL(t *testing.T) {
	repository := newRepository()
	s, paths, _ := testStore(t, repository, Options{})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	registry := t.TempDir()
	cache := filepath.Join(registry, "native-exec")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(os.Args[0], "-test.run=^TestOriginalWorkerGuardsAfterParentSIGKILL$")
	parent.Env = append(os.Environ(), "WA_MEDIA_GUARD_ROLE=parent", "WA_MEDIA_GUARD_KIND=decoded", "WA_MEDIA_GUARD_ROOT="+paths.Root, "WA_MEDIA_GUARD_REGISTRY="+registry)
	defer originalGuardCleanup(parent, registry)
	output, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	parent.Stderr = os.Stderr
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReaderSize(output, 128)
	line := originalGuardLine(t, reader)
	pid, err := strconv.Atoi(line)
	if err != nil || pid <= 1 || strconv.Itoa(pid) != line {
		t.Fatalf("native PID: %q %v", line, err)
	}
	defer unix.Kill(-pid, unix.SIGKILL)
	if line := originalGuardLine(t, reader); line != "parent-ready" {
		t.Fatalf("parent readiness: %q", line)
	}
	b, err := os.ReadFile(filepath.Join(registry, "decoded.json"))
	if err != nil {
		t.Fatal(err)
	}
	var artifact archive.MediaArtifact
	if err := json.Unmarshal(b, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.Budget != archive.BudgetDecoded || artifact.ReservedBytes != 64 {
		t.Fatalf("decoded reservation fixture: %+v", artifact)
	}
	repository.artifacts[artifact.ID] = artifact
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Wait(); err == nil {
		t.Fatal("app parent survived SIGKILL")
	}
	if err := unix.Kill(pid, 0); err != nil {
		t.Fatal("native exited with killed app parent", err)
	}
	originalGuardCacheRetained(t, cache)
	reopened, err := Open(paths, repository, Options{Admission: &physicalAdmission{}})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertBusy := func() {
		t.Helper()
		originalGuardAssertArtifactBusy(t, reopened, artifact.ID)
		if report, err := reopened.Reconcile(context.Background(), 256); err != nil || report.Cleaned != 0 || report.Active != 1 {
			t.Fatalf("reconcile bypassed live decoded holder: %+v %v", report, err)
		}
		if repository.charge(archive.BudgetDecoded) != 64 {
			t.Fatal("live decoded holder lost reservation charge")
		}
		if _, err := os.Stat(filepath.Join(paths.WorkingDir, artifactName(artifact.ID, ".decoded"))); err != nil {
			t.Fatal("live sealed native input was removed", err)
		}
	}
	assertBusy()
	if err := unix.Kill(pid, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	originalGuardLeaderGone(t, pid)
	if err := unix.Kill(-pid, 0); err != nil {
		t.Fatal("descendant did not survive the native leader", err)
	}
	assertBusy()
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		report, err := reopened.Reconcile(context.Background(), 256)
		if err != nil {
			t.Fatal(err)
		}
		if report.Cleaned == 1 {
			break
		}
		if report.Active != 1 || time.Now().After(deadline) {
			t.Fatalf("holder did not release stopped decoded input: %+v", report)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if repository.charge(archive.BudgetDecoded) != 0 {
		t.Fatal("decoded cleanup retained its reservation")
	}
	if _, err := os.Stat(filepath.Join(paths.WorkingDir, artifactName(artifact.ID, ".decoded"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("decoded cleanup did not remove the stopped input", err)
	}
	originalGuardCompletion(t, registry)
	originalGuardRecoverCache(t, cache)
}

func runDecodedGuardParent(t *testing.T) {
	t.Helper()
	root := os.Getenv("WA_MEDIA_GUARD_ROOT")
	repository := newRepository()
	paths := appdata.Paths{Root: root, MediaDir: filepath.Join(root, "media"), WorkingDir: filepath.Join(root, "working")}
	s, err := Open(paths, repository, Options{Admission: &physicalAdmission{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err := s.ReserveDecoded(context.Background(), repository.source, "media-id", 64)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.File().Write([]byte("decoded-input")); err != nil {
		t.Fatal(err)
	}
	if err := d.Seal(context.Background()); err != nil {
		t.Fatal(err)
	}
	artifact := repository.artifacts[d.reservation.ArtifactID]
	b, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("WA_MEDIA_GUARD_REGISTRY"), "decoded.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	input, err := d.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	guard, err := d.WorkerGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	startOriginalGuardParentWorker(t, input, []*os.File{guard})
}

type originalGuardGrowthLease struct{}

func (originalGuardGrowthLease) Release() error { return nil }

func originalGuardNamespace(t *testing.T, path string) *workerlife.ExecutableNamespace {
	t.Helper()
	directory, err := openDirectory(path)
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
			return originalGuardGrowthLease{}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return namespace
}

func originalGuardCacheRetained(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return
	}
	names, err := os.ReadDir(path)
	if err != nil || len(names) == 0 {
		t.Fatal("parent death lost private executable ownership", err)
	}
}

func originalGuardRecoverCache(t *testing.T, path string) {
	t.Helper()
	namespace := originalGuardNamespace(t, path)
	defer namespace.Close()
	// Evict/Reconcile already reacquired the existing lifetime artifact guard;
	// the normal completion marker requires positive recorded holder absence.
	if err := namespace.Recover(context.Background()); err != nil {
		t.Fatal("post-death executable recovery", err)
	}
	names, err := os.ReadDir(path)
	if err != nil || len(names) != 0 {
		t.Fatal("recovered executable cache retains bytes", err)
	}
}
