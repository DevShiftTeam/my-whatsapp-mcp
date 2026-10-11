//go:build darwin || linux

package media

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/compute"
	"github.com/Bodokh/wa-indexer/internal/diskspace"
	"github.com/Bodokh/wa-indexer/internal/modelstore"
	"github.com/Bodokh/wa-indexer/internal/workerlife"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type mediaExecutableProviderFunc func(context.Context, *os.File, int64) (workerlife.ExecutableLease, error)

func (f mediaExecutableProviderFunc) Prepare(ctx context.Context, file *os.File, limit int64) (workerlife.ExecutableLease, error) {
	return f(ctx, file, limit)
}

type mediaExecutableLease struct {
	path    string
	closeFn func() error
}

func (l *mediaExecutableLease) Path() string { return l.path }
func (l *mediaExecutableLease) Close() error { return l.closeFn() }

type mediaTestGrowthLease struct{}

func (mediaTestGrowthLease) Release() error { return nil }

func requireMediaNativePlatform(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("actual shared executable namespace qualifies thin ARM64 Mach-O only")
	}
}

func testMediaExecutableNamespace(t *testing.T, path string) *workerlife.ExecutableNamespace {
	t.Helper()
	requireMediaNativePlatform(t)
	if path == "" {
		parent, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(parent, "executable-cache")
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	directory, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	namespace, err := workerlife.NewExecutableNamespace(workerlife.NamespaceConfig{Directory: directory, Path: path,
		Admit: func(ctx context.Context, bytes int64) (workerlife.GrowthLease, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if bytes <= 64<<10 || bytes > (256<<20)+(64<<10) {
				return nil, workerlife.ErrLimit
			}
			return mediaTestGrowthLease{}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := namespace.Close(); err != nil {
			t.Error("media executable namespace still owned", err)
		}
	})
	if err := namespace.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	return namespace
}

func testMediaLifetimeConfig(t *testing.T, spec RuntimeSpec) workerlife.Config {
	t.Helper()
	return workerlife.Config{Executable: spec.Path, ExecutableProvider: testMediaExecutableNamespace(t, ""), Args: []string{"-test.run=^TestMediaLifetimeHelper$"}, Environment: []string{"WA_MEDIA_HOLDER=1"}, ReadyTimeout: 3 * time.Second}
}

func TestMediaRuntimeProviderRejectsUnavailableAuthority(t *testing.T) {
	spec := testNativeRuntime(t)
	var nilProvider mediaExecutableProviderFunc
	var nilLease *mediaExecutableLease
	for _, tc := range []struct {
		name     string
		provider workerlife.ExecutableProvider
	}{
		{"absent provider", nil},
		{"typed nil provider", nilProvider},
		{"nil lease", mediaExecutableProviderFunc(func(context.Context, *os.File, int64) (workerlife.ExecutableLease, error) { return nil, nil })},
		{"typed nil lease", mediaExecutableProviderFunc(func(context.Context, *os.File, int64) (workerlife.ExecutableLease, error) { return nilLease, nil })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			own := testWorkerOwnership(t)
			e := &Extractor{options: ExtractorOptions{Timeout: time.Second, WorkerLifetime: workerlife.Config{ExecutableProvider: tc.provider}}}
			if err := e.run(context.Background(), spec, nil, nil, io.Discard, 100, own); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("unavailable authority: %v", err)
			}
			if err := own.finish(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNewExtractorRequiresSharedExecutableProvider(t *testing.T) {
	scheduler, err := compute.NewThreadBudgetScheduler(2)
	if err != nil {
		t.Fatal(err)
	}
	options := ExtractorOptions{Store: &Store{}, Scheduler: scheduler,
		Acquire:        func(context.Context, string) (MediaModelLease, error) { return nil, ErrModelMissing },
		InstanceGuard:  mediaInstanceGuardFunc(func() (*os.File, error) { return nil, ErrUnavailable }),
		WorkerLifetime: workerlife.Config{Executable: "/trusted/private/Go-worker"},
	}
	for _, provider := range []workerlife.ExecutableProvider{nil, mediaExecutableProviderFunc(nil)} {
		options.WorkerLifetime.ExecutableProvider = provider
		if extractor, err := NewExtractor(options); extractor != nil || !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("missing shared provider accepted: %v", err)
		}
	}
	options.WorkerLifetime.ExecutableProvider = mediaExecutableProviderFunc(func(context.Context, *os.File, int64) (workerlife.ExecutableLease, error) { return nil, ErrUnavailable })
	extractor, err := NewExtractor(options)
	if err != nil {
		t.Fatal(err)
	}
	if err = extractor.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMediaRuntimeProviderFailureRetainsLeaseAndVerifiedFD(t *testing.T) {
	spec := testNativeRuntime(t)
	own := testWorkerOwnership(t)
	var allowClose atomic.Bool
	t.Cleanup(func() { allowClose.Store(true) })
	var closeCalls atomic.Int32
	var verified *os.File
	lease := &mediaExecutableLease{path: "/trusted-but-unpublished", closeFn: func() error {
		closeCalls.Add(1)
		if _, err := verified.Stat(); err != nil {
			return fmt.Errorf("runtime released before executable lease: %w", err)
		}
		for _, guard := range own.guards {
			if _, err := guard.Stat(); err != nil {
				return fmt.Errorf("guard released before executable lease: %w", err)
			}
		}
		if allowClose.Load() {
			return nil
		}
		return workerlife.ErrCleanup
	}}
	provider := mediaExecutableProviderFunc(func(_ context.Context, file *os.File, maximum int64) (workerlife.ExecutableLease, error) {
		verified = file
		if maximum != 256<<20 {
			t.Errorf("native cap: %d", maximum)
		}
		info, err := file.Stat()
		if err != nil || info.Size() != spec.Bytes {
			t.Fatalf("verified source: %v", err)
		}
		return lease, errors.Join(context.Canceled, workerlife.ErrCleanup)
	})
	e := &Extractor{options: ExtractorOptions{Timeout: time.Second, WorkerLifetime: workerlife.Config{ExecutableProvider: provider}}}
	if err := e.run(context.Background(), spec, nil, nil, io.Discard, 100, own); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if verified == nil || closeCalls.Load() != 0 {
		t.Fatal("partial provider ownership lost before caller cleanup")
	}
	if err := own.finish(); !errors.Is(err, ErrCleanup) {
		t.Fatalf("uncertain lease cleanup: %v", err)
	}
	allowClose.Store(true)
	select {
	case <-own.done:
	case <-time.After(2 * time.Second):
		t.Fatal("executable cleanup retry did not acknowledge")
	}
	if closeCalls.Load() < 2 {
		t.Fatal("partial executable cleanup was not retried")
	}
	if _, err := verified.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("runtime FD survived executable cleanup")
	}
}

func TestMediaRuntimeProviderReceivesOnlyVerifiedRuntime(t *testing.T) {
	spec := testNativeRuntime(t)
	spec.SHA256 = strings.Repeat("0", 64)
	called := false
	provider := mediaExecutableProviderFunc(func(context.Context, *os.File, int64) (workerlife.ExecutableLease, error) {
		called = true
		return nil, nil
	})
	own := testWorkerOwnership(t)
	e := &Extractor{options: ExtractorOptions{Timeout: time.Second, WorkerLifetime: workerlife.Config{ExecutableProvider: provider}}}
	if err := e.run(context.Background(), spec, nil, nil, io.Discard, 100, own); !errors.Is(err, ErrUnavailable) || called {
		t.Fatalf("verification bypass: %v called=%v", err, called)
	}
	if err := own.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestMediaRuntimeProviderPreservesDiskBudgetAndRejectsBadLeasePath(t *testing.T) {
	spec := testNativeRuntime(t)
	for _, tc := range []struct {
		name, path          string
		providerError, want error
	}{
		{"disk budget", "/private/unpublished", diskspace.ErrInsufficientSpace, ErrWaitingForDiskBudget},
		{"empty lease path", "", nil, ErrUnavailable},
		{"relative lease path", "untrusted", nil, ErrUnavailable},
		{"unclean lease path", "/private/../untrusted", nil, ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closed := 0
			lease := &mediaExecutableLease{path: tc.path, closeFn: func() error { closed++; return nil }}
			provider := mediaExecutableProviderFunc(func(context.Context, *os.File, int64) (workerlife.ExecutableLease, error) {
				return lease, tc.providerError
			})
			own := testWorkerOwnership(t)
			e := &Extractor{options: ExtractorOptions{Timeout: time.Second, WorkerLifetime: workerlife.Config{ExecutableProvider: provider}}}
			if err := e.run(context.Background(), spec, nil, nil, io.Discard, 100, own); !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
			if closed != 0 {
				t.Fatal("provider lease closed before ownership acknowledgement")
			}
			if err := own.finish(); err != nil || closed != 1 {
				t.Fatalf("lease cleanup: %v closes=%d", err, closed)
			}
		})
	}
}

func TestMediaWorkerEnvironmentIsFixedAndHasNoLoaderOverrides(t *testing.T) {
	t.Setenv("DYLD_INSERT_LIBRARIES", "/untrusted")
	t.Setenv("DYLD_LIBRARY_PATH", "/untrusted")
	t.Setenv("LD_PRELOAD", "/untrusted")
	t.Setenv("LD_LIBRARY_PATH", "/untrusted")
	t.Setenv("LD_AUDIT", "/untrusted")
	got := mediaWorkerEnvironment()
	want := []string{"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "OMP_THREAD_LIMIT=2", "OMP_NUM_THREADS=2", "OPENBLAS_NUM_THREADS=2", "MKL_NUM_THREADS=2", "VECLIB_MAXIMUM_THREADS=2", "TOKENIZERS_PARALLELISM=false"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("native environment: %v", got)
	}
}

// Structural observation keeps these real-branch regressions compilable before
// workerlife adds its immutable diagnostic value. No diagnostic is fabricated.
type mediaLaunchFailureView interface {
	error
	Stage() string
	ErrnoKnown() bool
	Errno() uint32
}

func mediaActualHolderStartFailure(t *testing.T) (error, string) {
	t.Helper()
	// Darwin executes the provider's path; Linux executes the held descriptor.
	requireMediaNativePlatform(t)
	spec := testNativeRuntime(t)
	own := testWorkerOwnership(t)
	missing := filepath.Join(t.TempDir(), "CANARY_PRIVATE_HOLDER_PATH")
	closed := 0
	provider := mediaExecutableProviderFunc(func(ctx context.Context, file *os.File, maximum int64) (workerlife.ExecutableLease, error) {
		if ctx.Err() != nil || file == nil || maximum != 64<<20 {
			return nil, errors.New("unexpected holder preparation input")
		}
		if _, err := file.Stat(); err != nil {
			return nil, err
		}
		return &mediaExecutableLease{path: missing, closeFn: func() error { closed++; return nil }}, nil
	})
	cmd := exec.Command(filepath.Join(t.TempDir(), "native-must-not-start"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{own.guards[0]}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lease, err := workerlife.Start(ctx, workerlife.Config{Executable: spec.Path, ExecutableProvider: provider}, cmd, own.guards)
	if lease != nil {
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			if err := lease.AwaitCleanup(cleanup); err != nil {
				t.Error("unexpected holder ownership remains", err)
			}
		})
		t.Fatal("successful preparation cleanup must not leave a lease")
	}
	if !errors.Is(err, workerlife.ErrUnavailable) || cmd.Process != nil || closed != 1 {
		t.Fatal("absent holder must fail before native start with acknowledged preparation cleanup")
	}
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != own.guards[0] {
		t.Fatal("failed holder launch changed native borrowed descriptors")
	}
	for _, guard := range own.guards {
		if _, err := guard.Stat(); err != nil {
			t.Fatal("failed holder launch closed a borrowed guard", err)
		}
	}
	return err, missing
}

func TestMediaLaunchFailureDiagnosticPreservesSentinel(t *testing.T) {
	launch, canary := mediaActualHolderStartFailure(t)
	mapped := executablePreparationError(context.Background(), launch)
	if !errors.Is(mapped, ErrUnavailable) || mapped.Error() != ErrUnavailable.Error() {
		t.Fatal("launch diagnostics changed the public media error")
	}
	var diagnostic mediaLaunchFailureView
	if !errors.As(mapped, &diagnostic) {
		t.Fatal("media mapping discarded the actual holder launch diagnostic")
	}
	if diagnostic.Stage() != "holder_start" || !diagnostic.ErrnoKnown() || diagnostic.Errno() != uint32(syscall.ENOENT) {
		t.Fatal("actual absent holder must retain its fixed stage and syscall errno")
	}
	if !errors.Is(mapped, workerlife.ErrUnavailable) {
		t.Fatal("media mapping discarded the worker launch sentinel")
	}
	var raw *os.PathError
	if errors.As(mapped, &raw) || strings.Contains(fmt.Sprintf("%+v", mapped), canary) {
		t.Fatal("launch diagnostics retained a private path or raw OS error")
	}
}

func TestMediaExecutableFailurePrecedence(t *testing.T) {
	launch, _ := mediaActualHolderStartFailure(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want error
	}{
		{"context-first", canceled, errors.Join(context.DeadlineExceeded, diskspace.ErrInsufficientSpace, launch), context.Canceled},
		{"canceled-before-deadline-and-budget", context.Background(), errors.Join(context.Canceled, context.DeadlineExceeded, diskspace.ErrInsufficientSpace, launch), context.Canceled},
		{"deadline-before-budget", context.Background(), errors.Join(context.DeadlineExceeded, diskspace.ErrInsufficientSpace, launch), context.DeadlineExceeded},
		{"disk-before-diagnostic", context.Background(), errors.Join(diskspace.ErrInsufficientSpace, launch), ErrWaitingForDiskBudget},
		{"media-budget-before-diagnostic", context.Background(), errors.Join(ErrWaitingForDiskBudget, launch), ErrWaitingForDiskBudget},
		{"unrelated-error-stays-unavailable", context.Background(), errors.New("CANARY_PRIVATE_UNRELATED_ERROR"), ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := executablePreparationError(tc.ctx, tc.err)
			if got != tc.want {
				t.Fatal("launch diagnostic changed context or disk-budget precedence")
			}
			var diagnostic mediaLaunchFailureView
			if errors.As(got, &diagnostic) {
				t.Fatal("higher-priority or unrelated error leaked launch diagnostic fields")
			}
		})
	}
}

func TestMediaRuntimeSnapshotWaitsForHolderCleanup(t *testing.T) {
	requireMediaNativePlatform(t)
	spec := testNativeRuntime(t)
	config := testMediaLifetimeConfig(t, spec)
	base := config.ExecutableProvider
	var holderClosed, nativeClosed atomic.Int32
	var nativePath string
	var caps []int64
	own := testWorkerOwnership(t)
	config.ExecutableProvider = mediaExecutableProviderFunc(func(ctx context.Context, fd *os.File, maximum int64) (workerlife.ExecutableLease, error) {
		caps = append(caps, maximum)
		lease, err := base.Prepare(ctx, fd, maximum)
		if lease == nil {
			return nil, err
		}
		path := lease.Path()
		if maximum == 256<<20 {
			nativePath = path
		}
		return &mediaExecutableLease{path: path, closeFn: func() error {
			if maximum == 256<<20 {
				if holderClosed.Load() != 1 {
					t.Error("native executable released before holder acknowledgement")
				}
				for _, guard := range own.guards {
					if _, err := guard.Stat(); err != nil {
						t.Error("upstream guard released before native executable", err)
					}
				}
			}
			if err := lease.Close(); err != nil {
				return err
			}
			if maximum == 64<<20 {
				holderClosed.Add(1)
			} else {
				nativeClosed.Add(1)
			}
			return nil
		}}, err
	})
	e := &Extractor{options: ExtractorOptions{Timeout: 5 * time.Second, WorkerLifetime: config}}
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err = input.WriteString("descriptor roles preserved"); err != nil {
		t.Fatal(err)
	}
	if _, err = input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err = e.run(context.Background(), spec, []string{"-test.run=^TestMediaNativeHelper$", "--", "--native-media-helper", "echo"}, []*os.File{input}, &output, 100, own); err != nil {
		t.Fatal(err)
	}
	if len(caps) != 2 || caps[0] != 256<<20 || caps[1] != 64<<20 || output.String() != "descriptor roles preserved" {
		t.Fatalf("shared provider/roles: %v %q", caps, output.String())
	}
	if nativePath == spec.Path || nativeClosed.Load() != 0 || holderClosed.Load() != 1 {
		t.Fatal("native snapshot did not retain independent ownership")
	}
	if _, err = os.Stat(nativePath); err != nil {
		t.Fatal("native executable removed before caller cleanup", err)
	}
	if err = own.finish(); err != nil {
		t.Fatal(err)
	}
	if nativeClosed.Load() != 1 {
		t.Fatal("native snapshot cleanup not acknowledged")
	}
	if _, err = os.Stat(nativePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("native snapshot survived acknowledged cleanup")
	}
}

func TestMediaHolderAdmissionPreservesDiskBudgetAndCleansNativeSnapshot(t *testing.T) {
	requireMediaNativePlatform(t)
	spec := testNativeRuntime(t)
	config := testMediaLifetimeConfig(t, spec)
	base := config.ExecutableProvider
	var caps []int64
	var nativePath string
	config.ExecutableProvider = mediaExecutableProviderFunc(func(ctx context.Context, fd *os.File, maximum int64) (workerlife.ExecutableLease, error) {
		caps = append(caps, maximum)
		if maximum == 64<<20 {
			return nil, diskspace.ErrInsufficientSpace
		}
		lease, err := base.Prepare(ctx, fd, maximum)
		if lease != nil {
			nativePath = lease.Path()
		}
		return lease, err
	})
	own := testWorkerOwnership(t)
	e := &Extractor{options: ExtractorOptions{Timeout: 5 * time.Second, WorkerLifetime: config}}
	err := e.run(context.Background(), spec, []string{"-test.run=^TestMediaNativeHelper$", "--", "--native-media-helper", "sleep"}, nil, io.Discard, 100, own)
	if !errors.Is(err, ErrWaitingForDiskBudget) || ExtractionCode(err) != "waiting_for_disk_budget" {
		t.Fatalf("holder admission classification: %v", err)
	}
	if len(caps) != 2 || caps[0] != 256<<20 || caps[1] != 64<<20 || nativePath == "" {
		t.Fatalf("both actual admissions were not reached: %v", caps)
	}
	if _, err = os.Stat(nativePath); err != nil {
		t.Fatal("native snapshot released before acknowledgement", err)
	}
	if err = own.finish(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(nativePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("native snapshot survived denied holder cleanup")
	}
}

func TestMediaLifetimeHelper(t *testing.T) {
	if os.Getenv("WA_MEDIA_HOLDER") != "1" {
		return
	}
	time.AfterFunc(45*time.Second, func() { os.Exit(93) })
	registry := os.Getenv("WA_MEDIA_HOLDER_REGISTRY")
	if path := registry; path != "" {
		if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(10)
		}
	}
	if workerlife.RunInherited() != nil {
		os.Exit(9)
	}
	if registry != "" {
		if os.WriteFile(registry+".done", []byte("clean"), 0600) != nil {
			os.Exit(94)
		}
	}
	os.Exit(0)
}

func testWorkerOwnership(t *testing.T) *extractionOwnership {
	t.Helper()
	own := newOwnership()
	t.Cleanup(func() {
		if err := own.finish(); err != nil {
			select {
			case <-own.done:
			case <-time.After(3 * time.Second):
				t.Error("test worker cleanup unacknowledged", err)
			}
		}
	})
	for i := 0; i < 3; i++ {
		f, err := os.CreateTemp(t.TempDir(), "guard")
		if err != nil {
			t.Fatal(err)
		}
		if err = unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
			f.Close()
			t.Fatal(err)
		}
		own.guards = append(own.guards, f)
		own.add(f.Close)
	}
	return own
}

func TestMediaNativeHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--native-media-helper" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "echo":
		b, _ := io.ReadAll(os.NewFile(4, "inherited-input"))
		_, _ = os.Stdout.Write(b)
	case "flood":
		for {
			if _, err := os.Stdout.Write(bytes.Repeat([]byte{'x'}, 8192)); err != nil {
				os.Exit(0)
			}
		}
	case "sleep":
		time.Sleep(time.Hour)
	case "ready-sleep":
		ready := os.NewFile(4, "ready")
		_, _ = fmt.Fprintln(ready, os.Getpid())
		_ = ready.Close()
		time.Sleep(time.Hour)

	case "tree", "unlocked-tree":
		ready := os.NewFile(4, "ready")
		if os.Args[len(os.Args)-1] == "unlocked-tree" {
			time.AfterFunc(40*time.Second, func() { _ = syscall.Kill(-os.Getpid(), syscall.SIGKILL) })
			_, _ = fmt.Fprintln(ready, os.Getpid())
			// Close before spawning: descendants can otherwise inherit extra FDs not
			// explicitly present in Cmd.ExtraFiles and conceal a missing holder.
			for fd := 5; fd <= 7; fd++ {
				if unix.Close(fd) != nil {
					os.Exit(7)
				}
			}
		}
		path, err := os.Executable()
		if err != nil {
			os.Exit(4)
		}
		child := exec.Command(path, "-test.run=TestMediaNativeHelper", "--", "--native-media-helper", "sleep")
		child.ExtraFiles = []*os.File{os.NewFile(3, "runtime")}
		if err := child.Start(); err != nil {
			os.Exit(4)
		}
		_, _ = fmt.Fprintln(ready, child.Process.Pid)
		time.Sleep(time.Hour)

	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func testNativeRuntime(t *testing.T) RuntimeSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "native-helper")
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, hash), src)
	if err != nil {
		t.Fatal(err)
	}
	if err = dst.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0500); err != nil {
		t.Fatal(err)
	}
	return RuntimeSpec{Path: path, SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: n}
}
func TestNativeVerifiedFDExecutionAndCancellation(t *testing.T) {
	requireMediaNativePlatform(t)
	spec := testNativeRuntime(t)
	e := &Extractor{options: ExtractorOptions{Timeout: 5 * time.Second, WorkerLifetime: testMediaLifetimeConfig(t, spec)}}
	t.Run("FD-only", func(t *testing.T) {
		input, err := os.CreateTemp(t.TempDir(), "input")
		if err != nil {
			t.Fatal(err)
		}
		defer input.Close()
		_, _ = input.WriteString("שלום ; $(no-command)")
		_, _ = input.Seek(0, 0)
		own := testWorkerOwnership(t)
		var output strings.Builder
		err = e.run(context.Background(), spec, []string{"-test.run=TestMediaNativeHelper", "--", "--native-media-helper", "echo"}, []*os.File{input}, &output, 1024, own)
		if cleanup := own.finish(); cleanup != nil {
			t.Fatal(cleanup)
		}
		if err != nil || output.String() != "שלום ; $(no-command)" {
			t.Fatalf("native result %q %v", output.String(), err)
		}
	})
	t.Run("oversized output", func(t *testing.T) {
		own := testWorkerOwnership(t)
		var output strings.Builder
		err := e.run(context.Background(), spec, []string{"-test.run=TestMediaNativeHelper", "--", "--native-media-helper", "flood"}, nil, &output, 10000, own)
		if cleanup := own.finish(); cleanup != nil {
			t.Fatal(cleanup)
		}
		if !errors.Is(err, ErrLimit) || output.Len() > 10000 {
			t.Fatalf("output escape len=%d err=%v", output.Len(), err)
		}
	})
	t.Run("cancelled worker reaped", func(t *testing.T) {
		ready, signal, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer ready.Close()
		defer signal.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pids := make(chan int, 1)
		go func() { var pid int; _, _ = fmt.Fscan(ready, &pid); pids <- pid; cancel() }()
		own := testWorkerOwnership(t)
		err = e.run(ctx, spec, []string{"-test.run=TestMediaNativeHelper", "--", "--native-media-helper", "ready-sleep"}, []*os.File{signal}, io.Discard, 100, own)
		_ = signal.Close()
		if cleanup := own.finish(); cleanup != nil {
			t.Fatal(cleanup)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		select {
		case pid := <-pids:
			if pid <= 0 || !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				t.Fatal("actual native cancellation/reap was not reached")
			}
		case <-time.After(time.Second):
			t.Fatal("native ready acknowledgement missing")
		}
		select {
		case <-own.done:
		default:
			t.Fatal("worker cleanup not joined")
		}
	})
	t.Run("corrupt runtime never executed", func(t *testing.T) {
		bad := spec
		bad.SHA256 = strings.Repeat("0", 64)
		f, err := openRuntime(context.Background(), bad)
		if f != nil {
			f.Close()
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	})
	t.Run("writable runtime never executed", func(t *testing.T) {
		if err := os.Chmod(spec.Path, 0700); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(spec.Path, 0500)
		f, err := openRuntime(context.Background(), spec)
		if f != nil {
			f.Close()
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Fatal("writable native executable accepted")
		}
	})
	t.Run("descendant exit precedes release", func(t *testing.T) {
		ready, signal, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer ready.Close()
		defer signal.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		pidResult := make(chan int, 1)
		go func() {
			buffer := make([]byte, 32)
			n, _ := ready.Read(buffer)
			pid, _ := strconv.Atoi(strings.TrimSpace(string(buffer[:n])))
			pidResult <- pid
			cancel()
		}()
		own := testWorkerOwnership(t)
		err = e.run(ctx, spec, []string{"-test.run=TestMediaNativeHelper", "--", "--native-media-helper", "tree"}, []*os.File{signal}, io.Discard, 128, own)
		if cleanup := own.finish(); cleanup != nil {
			t.Fatal(cleanup)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("tree cancel %v", err)
		}
		pid := <-pidResult
		if pid <= 0 {
			t.Fatal("native descendant did not start")
		}
		if !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			t.Fatal("descendant still holds inherited runtime after release")
		}
	})
}

func TestNativeLaunchRejectsMissingLifetimeGuards(t *testing.T) {
	spec := testNativeRuntime(t)
	provider := mediaExecutableProviderFunc(func(context.Context, *os.File, int64) (workerlife.ExecutableLease, error) {
		t.Fatal("missing lifetime guards reached preparation")
		return nil, ErrUnavailable
	})
	e := &Extractor{options: ExtractorOptions{Timeout: time.Second, WorkerLifetime: workerlife.Config{Executable: spec.Path, ExecutableProvider: provider}}}
	own := newOwnership()
	if err := e.run(context.Background(), spec, []string{"-test.run=^TestMediaNativeHelper$", "--", "--native-media-helper", "sleep"}, nil, io.Discard, 100, own); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing authority: %v", err)
	}
	if err := own.finish(); err != nil {
		t.Fatal(err)
	}
}

// This fixture closes every native guard and gives none to its descendant.
// Only the independent holder can keep the three lock classes busy after kill.
func TestMediaGuardParentHelper(t *testing.T) {
	if os.Getenv("WA_MEDIA_PARENT") != "1" {
		return
	}
	size, _ := strconv.ParseInt(os.Getenv("WA_MEDIA_RUNTIME_BYTES"), 10, 64)
	spec := RuntimeSpec{Path: os.Getenv("WA_MEDIA_RUNTIME"), SHA256: os.Getenv("WA_MEDIA_RUNTIME_SHA"), Bytes: size}
	e := &Extractor{options: ExtractorOptions{Timeout: 20 * time.Second, WorkerLifetime: workerlife.Config{Executable: spec.Path, ExecutableProvider: testMediaExecutableNamespace(t, os.Getenv("WA_MEDIA_NAMESPACE")), Args: []string{"-test.run=^TestMediaLifetimeHelper$"}, Environment: []string{"WA_MEDIA_HOLDER=1", "WA_MEDIA_HOLDER_REGISTRY=" + os.Getenv("WA_MEDIA_HOLDER_REGISTRY")}, ReadyTimeout: 3 * time.Second}}}
	own := newOwnership()
	for fd := 4; fd <= 6; fd++ {
		file := os.NewFile(uintptr(fd), "role-guard")
		own.guards = append(own.guards, file)
		own.add(file.Close)
	}
	err := e.run(context.Background(), spec, []string{"-test.run=^TestMediaNativeHelper$", "--", "--native-media-helper", "unlocked-tree"}, []*os.File{os.NewFile(3, "ready")}, io.Discard, 128, own)
	if own.finish() != nil || err != nil {
		os.Exit(8)
	}
	os.Exit(0)
}

func TestMediaHolderSurvivesParentSIGKILLAndNativeGuardClosure(t *testing.T) {
	requireMediaNativePlatform(t)
	spec := testNativeRuntime(t)
	ready, signal, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	defer signal.Close()
	var guards []*os.File
	var paths []string
	for _, role := range []string{"instance", "model", "original"} {
		f, err := os.CreateTemp(t.TempDir(), role)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		mode := unix.LOCK_SH
		if role == "instance" {
			mode = unix.LOCK_EX
		}
		if err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		guards = append(guards, f)
		paths = append(paths, f.Name())
	}
	registry := filepath.Join(t.TempDir(), "holder-pid")
	cacheParent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(cacheParent, "executable-cache")
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(spec.Path, "-test.run=^TestMediaGuardParentHelper$")
	parent.Env = []string{"WA_MEDIA_PARENT=1", "WA_MEDIA_NAMESPACE=" + cache, "WA_MEDIA_HOLDER_REGISTRY=" + registry, "WA_MEDIA_RUNTIME=" + spec.Path, "WA_MEDIA_RUNTIME_SHA=" + spec.SHA256, "WA_MEDIA_RUNTIME_BYTES=" + strconv.FormatInt(spec.Bytes, 10)}
	parent.ExtraFiles = append([]*os.File{signal}, guards...)
	if err = parent.Start(); err != nil {
		t.Fatal(err)
	}

	parentDone := make(chan struct{})
	go func() { _ = parent.Wait(); close(parentDone) }()
	var group int
	completed := false
	t.Cleanup(func() {
		if group > 0 {
			_ = syscall.Kill(-group, syscall.SIGKILL)
		}
		// An unknown native PID is still owned by the parent bounded reaper. Do not
		// kill that reaper on readiness timeout; allow its timeout and joins first.
		select {
		case <-parentDone:
		case <-time.After(25 * time.Second):
			_ = parent.Process.Kill()
			<-parentDone
		}
		// Test-only authority: this exact fixture writes its own PID before READY.
		// Product code never kills holders to force cleanup acknowledgement.
		if data, err := os.ReadFile(registry); !completed && err == nil {
			if pid, err := strconv.Atoi(string(data)); err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	_ = signal.Close()
	for _, file := range guards {
		_ = file.Close()
	}
	pids := make(chan int, 2)
	go func() {
		input := bufio.NewReader(ready)
		for i := 0; i < 2; i++ {
			var pid int
			_, _ = fmt.Fscan(input, &pid)
			pids <- pid
		}
	}()
	select {
	case group = <-pids:
		if group <= 0 {
			t.Fatal("native group did not become ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native group startup timed out")
	}
	select {
	case child := <-pids:
		if child <= 0 {
			t.Fatal("native descendant did not become ready")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native descendant startup timed out")
	}
	if err = parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-parentDone
	locked := func(path string) bool {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return false
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			t.Fatal(err)
		}
		return true
	}
	for _, path := range paths {
		if !locked(path) {
			t.Fatal("parent death released a live native role lock")
		}
	}
	if err = syscall.Kill(-group, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		held := false
		for _, path := range paths {
			held = locked(path) || held
		}
		if !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native group exit did not release holder locks")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
		t.Fatal("locks released before native group absence")
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		data, markerErr := os.ReadFile(registry + ".done")
		pidData, pidErr := os.ReadFile(registry)
		pid, parseErr := strconv.Atoi(string(pidData))
		if markerErr == nil && string(data) == "clean" && pidErr == nil && parseErr == nil && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("holder did not acknowledge clean completion and exit")
		}
		time.Sleep(20 * time.Millisecond)
	}
	completed = true
}

type mediaInstanceGuardFunc func() (*os.File, error)

func (f mediaInstanceGuardFunc) WorkerGuard() (*os.File, error) { return f() }

func TestWorkerGuardExportErrorKeepsReturnedDescriptorOwned(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "partial-guard")
	if err != nil {
		t.Fatal(err)
	}
	e := &Extractor{options: ExtractorOptions{InstanceGuard: mediaInstanceGuardFunc(func() (*os.File, error) { return file, ErrUnavailable })}}
	own := newOwnership()
	if err = e.addWorkerGuards(own, &Original{}, nil, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err = file.Stat(); err != nil {
		t.Fatal("exported descriptor was dropped before ownership cleanup")
	}
	if err = own.finish(); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("partial guard export leaked its descriptor")
	}
}

// TestMain recognizes the fixed decoder/ASR entry argument prefixes used by the
// production launcher. These fixtures are CPU Go children, not ASR evidence.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "-nostdin" {
		data, err := io.ReadAll(io.LimitReader(os.NewFile(4, "original"), 1<<20))
		if err != nil || len(data) < 4 {
			os.Exit(20)
		}
		switch data[len(data)-4] {
		case 0:
		case 1:
			_, _ = os.Stdout.Write(make([]byte, 64))
		case 2:
			_, _ = os.Stdout.Write([]byte{1, 0})
		case 3:
			data := make([]byte, slicePCMBytes+2)
			data[0] = 1
			data[len(data)-2] = 1
			_, _ = os.Stdout.Write(data)
		default:
			os.Exit(21)
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == "--model" {
		model, err := io.ReadAll(io.LimitReader(os.NewFile(5, "model"), 32))
		if err != nil {
			os.Exit(22)
		}
		if string(model) == "speech" {
			_, _ = os.Stdout.Write([]byte("שלום\n"))
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type primaryAudioModel struct {
	model, guard   *os.File
	opened, guards []*os.File
	closes         int
}

func (m *primaryAudioModel) Profile() modelstore.InstalledProfile {
	return modelstore.InstalledProfile{ID: "ivrit-whisper-large-v3-turbo", Identity: strings.Repeat("a", 64)}
}
func (m *primaryAudioModel) Open(string) (*os.File, error) {
	f, err := os.Open(m.model.Name())
	if f != nil {
		m.opened = append(m.opened, f)
	}
	return f, err
}
func (m *primaryAudioModel) WorkerGuard() (*os.File, error) {
	fd, err := unix.FcntlInt(m.guard.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "model-guard")
	m.guards = append(m.guards, f)
	return f, nil
}
func (m *primaryAudioModel) Close() error {
	if m.closes > 0 {
		return nil
	}
	m.closes++
	return errors.Join(m.model.Close(), m.guard.Close())
}

func TestPrimaryAudioActualPathOutcomesAndCheckpointOwnership(t *testing.T) {
	requireMediaNativePlatform(t)
	spec := testNativeRuntime(t)
	for _, tc := range []struct {
		name    string
		tag     byte
		outcome archive.ExtractionOutcome
		failure error
		opens   int
	}{
		{"zero decoded PCM", 0, "", ErrCorrupt, 0},
		{"positive digital silence", 1, archive.ExtractionSilent, nil, 0},
		{"nonzero blank ASR", 2, "", ErrFailed, 1},
		{"two complete speech slices", 3, archive.ExtractionText, nil, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepository()
			store, _, _ := testStore(t, repo, Options{})
			data := waveFixture()
			data[len(data)-4] = tc.tag
			q := request(repo, data)
			q.Kind = archive.MessageVoice
			download, err := store.AdmitDownload(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := download.Close(); err != nil {
					t.Error(err)
				}
			})
			if _, err = download.File().Write(data); err != nil {
				t.Fatal(err)
			}
			if err = download.Complete(context.Background(), receipt(q, data)); err != nil {
				t.Fatal(err)
			}
			if err = download.Close(); err != nil {
				t.Fatal(err)
			}
			original, err := store.OpenOriginal(context.Background(), archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: 1}, q.MediaID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := original.Close(); err != nil {
					t.Error(err)
				}
			})
			model, err := os.CreateTemp(t.TempDir(), "model")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = model.Close() })
			modelText := "speech"
			if tc.tag == 2 {
				modelText = "blank"
			}
			if _, err = model.WriteString(modelText); err != nil {
				t.Fatal(err)
			}
			modelGuard, err := os.CreateTemp(t.TempDir(), "model-lock")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = modelGuard.Close() })
			if err = unix.Flock(int(modelGuard.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			instance, err := os.CreateTemp(t.TempDir(), "instance-lock")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = instance.Close() })
			if err = unix.Flock(int(instance.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			lease := &primaryAudioModel{model: model, guard: modelGuard}
			scheduler, err := compute.NewThreadBudgetScheduler(2)
			if err != nil {
				t.Fatal(err)
			}
			runtimes := RuntimePaths{Speech: spec, Decoder: spec}
			fingerprint, err := RuntimeFingerprint(archive.JobTranscription, runtimes)
			if err != nil {
				t.Fatal(err)
			}
			version := archive.ExtractionVersion{ID: "primary-audio", Kind: archive.JobTranscription, ProfileID: lease.Profile().ID, ProfileIdentity: lease.Profile().Identity, RuntimeSHA256: fingerprint, ExtractorVersion: ExtractorVersion, PromptVersion: "forced-he-v1", Language: "he"}
			beforeCalls, acquires := 0, 0
			e, err := NewExtractor(ExtractorOptions{Store: store, Scheduler: scheduler, Runtimes: runtimes, Language: SpeechHebrew, ExtractionVersions: []archive.ExtractionVersion{version}, Timeout: 3 * time.Second,
				WorkerLifetime: testMediaLifetimeConfig(t, spec),
				InstanceGuard: mediaInstanceGuardFunc(func() (*os.File, error) {
					fd, err := unix.FcntlInt(instance.Fd(), unix.F_DUPFD_CLOEXEC, 0)
					if err != nil {
						return nil, err
					}
					return os.NewFile(uintptr(fd), "instance-guard"), nil
				}),
				Acquire: func(context.Context, string) (MediaModelLease, error) { acquires++; return lease, nil },
				BeforeHeavy: func(context.Context) error {
					beforeCalls++
					if beforeCalls == 1 {
						return nil
					}
					if lease.closes != 0 {
						t.Error("installed profile pin released between slices")
					}
					for _, fd := range append(append([]*os.File(nil), lease.opened...), lease.guards...) {
						if _, err := fd.Stat(); !errors.Is(err, os.ErrClosed) {
							t.Error("slice model descriptor survived checkpoint")
						}
					}
					challenger, err := os.OpenFile(modelGuard.Name(), os.O_RDWR, 0)
					if err != nil {
						return err
					}
					defer challenger.Close()
					if err = unix.Flock(int(challenger.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
						t.Error("whole-profile pin absent at checkpoint")
					}
					repo.mu.Lock()
					activeDecoded := 0
					for _, a := range repo.artifacts {
						if a.Budget == archive.BudgetDecoded && a.ReservedBytes > 0 {
							activeDecoded++
						}
					}
					repo.mu.Unlock()
					if activeDecoded != 1 {
						t.Error("WAV decoded charge survived checkpoint")
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				closing, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				if err := e.Close(closing); err != nil {
					t.Error(err)
				}
			})
			meta := original.Metadata()
			work := archive.ExtractionWork{ID: "audio-work", Lease: archive.JobLease{JobID: "audio-work", LeaseID: "lease"}, LeaseUntil: time.Now().Add(time.Minute), Version: version, Source: meta.Source, MediaID: meta.MediaID, ArtifactID: meta.ArtifactID, OriginalSHA256: meta.SHA256, ContentHash: strings.Repeat("c", 64)}
			result, err := e.ExtractPrimary(context.Background(), work, original)
			if !errors.Is(err, tc.failure) || result.Outcome != tc.outcome {
				t.Fatalf("outcome %q err=%v", result.Outcome, err)
			}
			if acquires != 1 || lease.closes != 1 || len(lease.opened) != tc.opens {
				t.Fatalf("model ownership acquisitions=%d closes=%d opens=%d", acquires, lease.closes, len(lease.opened))
			}
			if tc.tag == 3 && (len(result.Documents) != 2 || beforeCalls != 3 || result.Documents[1].Provenance.StartMS != 30000 || result.Documents[1].Provenance.EndMS != 30001) {
				t.Fatal("speech slice coverage/checkpoint changed")
			}
			if scheduler.Snapshot().Threads != 0 {
				t.Fatal("compute lease retained after acknowledged extraction")
			}
			closing, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err = e.Close(closing); err != nil {
				t.Fatal(err)
			}
			if err = original.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
