//go:build darwin || linux

package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/diskspace"
	"github.com/Bodokh/my-whatsapp-mcp/internal/workerlife"
	"golang.org/x/sys/unix"
)

// Native diagnostics are package-private, per-call and bound to an exact runtime.
// Only an explicitly armed tagged test supplies a private file. Ordinary image
// and voice extraction keeps stderr discarded and has no process-global hook.
const nativeDiagnosticMaximumBytes int64 = 32 << 10

type nativeDiagnosticContextKey struct{}

type nativeDiagnosticStatus struct {
	Stage               uint8 `json:"stage"`
	ExitCode            int   `json:"exit_code"`
	Signal              int   `json:"signal"`
	Started             bool  `json:"started"`
	Waited              bool  `json:"waited"`
	ExitKnown           bool  `json:"exit_known"`
	CleanupAcknowledged bool  `json:"cleanup_acknowledged"`
	// Legacy field name: total held diagnostic-file extent, including failed
	// stdout only when the explicit StdoutOnFailureCaptured marker is true.
	StderrBytes             int64 `json:"stderr_bytes"`
	StdoutOnFailureCaptured bool  `json:"stdout_on_failure_captured"`
	Truncated               bool  `json:"truncated"`
	WriteFailed             bool  `json:"write_failed"`
}

type nativeRuntimeDiagnostic struct {
	mu      sync.Mutex
	runtime RuntimeSpec
	file    *os.File
	status  nativeDiagnosticStatus
}

func withNativeDiagnostic(ctx context.Context, runtime RuntimeSpec, file *os.File) (context.Context, *nativeRuntimeDiagnostic, error) {
	if ctx == nil || file == nil || runtime.Path == "" || runtime.Bytes <= 0 || !validDigest(runtime.SHA256) {
		return nil, nil, ErrInvalidRequest
	}
	if err := checkPrivateFD(file, false); err != nil {
		return nil, nil, ErrUnsafeFile
	}
	if info, err := file.Stat(); err != nil || info.Size() != 0 {
		return nil, nil, ErrUnsafeFile
	}
	capture := &nativeRuntimeDiagnostic{runtime: runtime, file: file, status: nativeDiagnosticStatus{ExitCode: -1}}
	return context.WithValue(ctx, nativeDiagnosticContextKey{}, capture), capture, nil
}

func nativeDiagnosticStderr(ctx context.Context, runtime RuntimeSpec) (io.Writer, *nativeRuntimeDiagnostic) {
	capture, _ := ctx.Value(nativeDiagnosticContextKey{}).(*nativeRuntimeDiagnostic)
	if capture == nil || capture.runtime != runtime {
		return io.Discard, nil
	}
	return capture, capture
}

// Always consume excess stderr so a full diagnostic cannot block the native
// child. Stderr and failed stdout share one32KiB prefix cap; write uncertainty is
// recorded for the owning test to reject without changing native error policy.
func (d *nativeRuntimeDiagnostic) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	amount := min(int64(len(p)), nativeDiagnosticMaximumBytes-d.status.StderrBytes)
	if amount < int64(len(p)) {
		d.status.Truncated = true
	}
	if amount > 0 && !d.status.WriteFailed {
		n, err := d.file.Write(p[:int(amount)])
		d.status.StderrBytes += int64(n)
		if err != nil || n != int(amount) {
			d.status.WriteFailed = true
		}
	}
	return len(p), nil
}

// Only after stdout copying, sole Wait and native/holder cleanup have joined
// may an explicitly armed test borrow the existing builder's failed output.
// Keep ordinary parsing untouched and copy at most32KiB into the same capped
// sink. Uncertain cleanup must never race a still-running builder writer.
func (d *nativeRuntimeDiagnostic) captureFailedOutput(out io.Writer) {
	if d == nil || !d.snapshot().CleanupAcknowledged {
		return
	}
	builder, ok := out.(*strings.Builder)
	if !ok {
		return
	}
	text := builder.String()
	if len(text) == 0 {
		return
	}
	amount := min(int64(len(text)), nativeDiagnosticMaximumBytes)
	d.mu.Lock()
	before := d.status.StderrBytes
	if amount < int64(len(text)) {
		d.status.Truncated = true
	}
	d.mu.Unlock()
	_, _ = d.Write([]byte(text[:int(amount)]))
	d.mu.Lock()
	if d.status.StderrBytes > before {
		d.status.StdoutOnFailureCaptured = true
	}
	d.mu.Unlock()
}

// Fixed numeric stages:1selected,2verified,3prepared,4started,5waited,6cleaned.
// No argv, source text, pathname, prompt or raw syscall error enters metadata.
func (d *nativeRuntimeDiagnostic) observe(stage uint8, state *os.ProcessState) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status.Stage = stage
	if stage == 4 {
		d.status.Started = true
	}
	if stage == 5 {
		d.status.Waited = true
		if state != nil {
			d.status.ExitCode = state.ExitCode()
			d.status.ExitKnown = true
			if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				d.status.Signal = int(status.Signal())
			}
		}
	}
	if stage == 6 {
		d.status.CleanupAcknowledged = true
	}
}

func (d *nativeRuntimeDiagnostic) snapshot() nativeDiagnosticStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

func nilExecutableAuthority(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func mediaWorkerEnvironment() []string {
	// Native task inputs use argv/FDs. Keep the native environment fixed; helper
	// fixture/composition variables belong only to WorkerLifetime.Environment.
	environment := workerlife.WithoutLoaderOverrides(nil, []string{"LANG=C.UTF-8", "LC_ALL=C.UTF-8"})
	return append(environment, "OMP_THREAD_LIMIT=2", "OMP_NUM_THREADS=2", "OPENBLAS_NUM_THREADS=2", "MKL_NUM_THREADS=2", "VECLIB_MAXIMUM_THREADS=2", "TOKENIZERS_PARALLELISM=false")
}

func executablePreparationError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, diskspace.ErrInsufficientSpace) || errors.Is(err, ErrWaitingForDiskBudget) {
		return ErrWaitingForDiskBudget
	}
	var launch workerlife.LaunchFailure
	if errors.As(err, &launch) {
		return &mediaLaunchFailure{launch: launch}
	}
	return ErrUnavailable
}

// Retain only the worker's immutable stage/errno values. The public media error
// and sentinel stay fixed, and the original syscall error is never retained.
type mediaLaunchFailure struct{ launch workerlife.LaunchFailure }

func (e *mediaLaunchFailure) Error() string        { return ErrUnavailable.Error() }
func (e *mediaLaunchFailure) Is(target error) bool { return target == ErrUnavailable }
func (e *mediaLaunchFailure) Unwrap() error        { return e.launch }

type limitedOutput struct {
	dst       io.Writer
	remaining int64
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, ErrLimit
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	return n, err
}

// A cleanup timeout transfers ownership to an explicit reaper. The compute
// guard is added first and released only after every file/model cleanup.
type extractionOwnership struct {
	mu       sync.Mutex
	cleanups []func() error
	guards   []*os.File
	pending  <-chan struct{}
	done     chan struct{}
	started  bool
	cancel   context.CancelFunc
}

func newOwnership() *extractionOwnership           { return &extractionOwnership{done: make(chan struct{})} }
func (o *extractionOwnership) add(fn func() error) { o.cleanups = append(o.cleanups, fn) }
func (o *extractionOwnership) clean() error {
	for len(o.cleanups) > 0 {
		i := len(o.cleanups) - 1
		if err := o.cleanups[i](); err != nil {
			return ErrCleanup
		}
		o.cleanups = o.cleanups[:i]
	}
	return nil
}
func (o *extractionOwnership) finish() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.started {
		select {
		case <-o.done:
			return nil
		default:
			return ErrCleanup
		}
	}
	o.started = true
	if o.pending == nil {
		if err := o.clean(); err == nil {
			close(o.done)
			return nil
		}
	}
	go func() {
		if o.pending != nil {
			<-o.pending
		}
		for {
			if o.clean() == nil {
				close(o.done)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return ErrCleanup
}

// addWorkerGuards borrows no lock authority from file paths: every descriptor is
// a close-only duplicate of an existing instance, original, or model lease.
func (e *Extractor) addWorkerGuards(own *extractionOwnership, original *Original, model MediaModelLease, decoded *Decoded) error {
	if e.options.InstanceGuard == nil || original == nil || len(own.guards) != 0 {
		return ErrUnavailable
	}
	add := func(f *os.File) {
		if f != nil {
			own.guards = append(own.guards, f)
			own.add(f.Close)
		}
	}
	instance, err := e.options.InstanceGuard.WorkerGuard()
	add(instance)
	if err != nil || instance == nil {
		return ErrUnavailable
	}
	source, err := original.WorkerGuards()
	for _, file := range source {
		add(file)
	}
	if err != nil || len(source) != 2 || source[0] == nil || source[1] == nil {
		return ErrUnavailable
	}
	if model != nil {
		guard, err := model.WorkerGuard()
		add(guard)
		if err != nil || guard == nil {
			return ErrUnavailable
		}
	}
	if decoded != nil {
		guard, err := decoded.WorkerGuard()
		add(guard)
		if err != nil || guard == nil {
			return ErrUnavailable
		}
	}
	return nil
}

func openRuntime(ctx context.Context, r RuntimeSpec) (*os.File, error) {
	if !filepath.IsAbs(r.Path) || len(r.SHA256) != 64 || r.Bytes <= 0 || r.Bytes > 256<<20 {
		return nil, ErrUnavailable
	}
	dir, err := openDirectory(filepath.Dir(r.Path))
	if err != nil {
		return nil, ErrUnavailable
	}
	defer dir.close()
	name := filepath.Base(r.Path)
	if !safeFilename(name) {
		return nil, ErrUnavailable
	}
	fd, err := unix.Openat(int(dir.file.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	f := os.NewFile(uintptr(fd), "native-runtime")
	fail := func() (*os.File, error) { _ = f.Close(); return nil, ErrUnavailable }
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || uint32(st.Uid) != uint32(os.Geteuid()) || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || st.Mode&07777 != 0500 || st.Size != r.Bytes || !noExtendedACL(f) {
		return fail()
	}
	before, err := f.Stat()
	if err != nil {
		return fail()
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	reader := io.NewSectionReader(f, 0, r.Bytes)
	var readBytes int64
	for {
		if ctx.Err() != nil {
			_ = f.Close()
			return nil, ctx.Err()
		}
		n, e := reader.Read(buffer)
		readBytes += int64(n)
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return fail()
		}
	}
	var after unix.Stat_t
	info, infoErr := f.Stat()
	if readBytes != r.Bytes || unix.Fstat(fd, &after) != nil || infoErr != nil || after.Dev != st.Dev || after.Ino != st.Ino || after.Mode != st.Mode || after.Nlink != 1 || after.Uid != st.Uid || after.Size != r.Bytes || !info.ModTime().Equal(before.ModTime()) || !noExtendedACL(f) || hex.EncodeToString(hash.Sum(nil)) != r.SHA256 {
		return fail()
	}
	if _, err = f.Seek(0, 0); err != nil {
		return fail()
	}
	return f, nil
}

func (e *Extractor) run(ctx context.Context, r RuntimeSpec, args []string, files []*os.File, out io.Writer, limit int64, own *extractionOwnership) (runErr error) {
	stderr, diagnostic := nativeDiagnosticStderr(ctx, r)
	defer func() {
		if runErr != nil {
			diagnostic.captureFailedOutput(out)
		}
	}()
	diagnostic.observe(1, nil)
	ctx, cancel := context.WithTimeout(ctx, e.options.Timeout)
	defer cancel()
	if len(own.guards) < 3 || len(own.guards) > 8 {
		return ErrUnavailable
	}
	if nilExecutableAuthority(e.options.WorkerLifetime.ExecutableProvider) {
		return ErrUnavailable
	}
	runtime, err := openRuntime(ctx, r)
	if err != nil {
		return err
	}
	diagnostic.observe(2, nil)
	own.add(runtime.Close)
	// The shared provider audits the actual system-only executable closure and
	// creates its private immutable launch path from this verified descriptor.
	// A partial lease stays owned even when preparation itself fails.
	executable, prepareErr := e.options.WorkerLifetime.ExecutableProvider.Prepare(ctx, runtime, 256<<20)
	if !nilExecutableAuthority(executable) {
		own.add(executable.Close)
	} else {
		executable = nil
	}
	if prepareErr != nil {
		return executablePreparationError(ctx, prepareErr)
	}
	if executable == nil {
		return ErrUnavailable
	}
	path := executable.Path() // Capture once; Close invalidates the lease's Path.
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.IndexByte(path, 0) >= 0 {
		return ErrUnavailable
	}
	diagnostic.observe(3, nil)
	read, write, err := os.Pipe()
	if err != nil {
		return ErrFailed
	}
	cmd := exec.Command(path, args...)
	// FD3 remains the original verified runtime; every data/model role stays put.
	cmd.ExtraFiles = append([]*os.File{runtime}, files...)
	cmd.Dir = "/"
	cmd.Stdin = nil
	cmd.Stdout = write
	cmd.Stderr = stderr
	cmd.Env = mediaWorkerEnvironment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	lifetime, startErr := workerlife.Start(ctx, e.options.WorkerLifetime, cmd, own.guards)
	if cmd.Process == nil {
		_ = write.Close()
		_ = read.Close()
		if lifetime != nil {
			// A holder with uncertain registration must retain every upstream owner.
			own.add(func() error {
				cleanup, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				if lifetime.AwaitCleanup(cleanup) != nil {
					return ErrCleanup
				}
				return nil
			})
		}
		return executablePreparationError(ctx, startErr)
	}

	diagnostic.observe(4, nil)
	_ = write.Close()
	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		diagnostic.observe(5, cmd.ProcessState)
		close(exited)
	}()
	copied := make(chan error, 1)
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		_, err := io.Copy(&limitedOutput{dst: out, remaining: limit}, read)
		copied <- err
	}()
	var result error
	if startErr != nil {
		result = executablePreparationError(ctx, startErr)
	} else {
		select {
		case result = <-copied:
		case <-ctx.Done():
			result = ctx.Err()
		}
	}

	if result == nil {
		select {
		case <-exited:
		case <-ctx.Done():
			result = ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	gone := func() bool {
		select {
		case <-exited:
			return errors.Is(syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH)
		default:
			return false
		}
	}
	cleanupDone := make(chan struct{})
	go func() {
		deadline := time.NewTimer(100 * time.Millisecond)
		defer deadline.Stop()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		kill := false
		for {
			if gone() {
				_ = read.Close()
				<-outputDone
				// Decoder cleanup includes the independent holder before any ASR launch.
				// Failure remains owned and charged; parent cancellation is not an ack.
				if lifetime == nil {
					return
				}
				for lifetime.AwaitCleanup(context.Background()) != nil {
					time.Sleep(100 * time.Millisecond)
				}
				close(cleanupDone)
				return
			}
			select {
			case <-deadline.C:
				kill = true
			case <-tick.C:
			}
			if kill {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		}
	}()
	select {
	case <-cleanupDone:
		diagnostic.observe(6, nil)
	case <-time.After(2 * time.Second):
		own.pending = cleanupDone
		_ = read.Close()
		return ErrCleanup
	}
	if result != nil {
		return result
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		return ErrFailed
	}
	return nil
}
