package inference

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/workerlife"
)

type testLease struct {
	mu         sync.Mutex
	closed     bool
	files      []*os.File
	closeErr   error
	openErr    error
	guardErr   error
	closeCalls int
}

func testRegularGuard() (*os.File, error) {
	file, err := os.CreateTemp("", "wa-encoder-test-guard-")
	if err == nil {
		_ = os.Remove(file.Name())
	}
	return file, err
}
func (l *testLease) WorkerGuard() (*os.File, error) {
	if l.guardErr != nil {
		return nil, l.guardErr
	}
	return testRegularGuard()
}

type testGrowthLease struct{}

func (testGrowthLease) Release() error { return nil }
func testExecutableNamespace(t *testing.T) *workerlife.ExecutableNamespace {
	t.Helper()
	path := filepath.Join(t.TempDir(), "native-exec")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	namespace, err := workerlife.NewExecutableNamespace(workerlife.NamespaceConfig{Directory: directory, Path: path,
		Admit: func(ctx context.Context, growth int64) (workerlife.GrowthLease, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if growth <= 64<<10 || growth > (64<<20)+(64<<10) {
				return nil, workerlife.ErrLimit
			}
			return testGrowthLease{}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := namespace.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := namespace.Close(); err != nil {
			t.Error("test executable namespace still owned", err)
		}
	})
	return namespace
}
func newTestSupervisor(t *testing.T, config Config) (*Supervisor, error) {
	config.InstanceGuard = testRegularGuard
	config.WorkerLifetime = workerlife.Config{Executable: os.Args[0], ExecutableProvider: testExecutableNamespace(t), Args: []string{"-test.run=TestSupervisorHolderProcess"}, Environment: []string{"WA_LIFETIME_HELPER=1", "GORACE=atexit_sleep_ms=0"}, ReadyTimeout: 5 * time.Second}
	return NewSupervisor(config)
}
func TestSupervisorHolderProcess(t *testing.T) {
	if os.Getenv("WA_LIFETIME_HELPER") != "1" {
		return
	}
	if workerlife.RunInherited() != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestSupervisorRequiresParentDeathGuards(t *testing.T) {
	base := Config{Executable: os.Args[0], Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return &testLease{}, nil }, WorkerLifetime: workerlife.Config{Executable: os.Args[0], ExecutableProvider: testExecutableNamespace(t)}, InstanceGuard: testRegularGuard}
	withoutInstance := base
	withoutInstance.InstanceGuard = nil
	if _, err := NewSupervisor(withoutInstance); err != ErrUnavailable {
		t.Fatalf("missing instance guard accepted: %v", err)
	}
	withoutHolder := base
	withoutHolder.WorkerLifetime = workerlife.Config{}
	if _, err := NewSupervisor(withoutHolder); err != ErrUnavailable {
		t.Fatalf("missing holder accepted: %v", err)
	}
}

func TestSupervisorGuardFailureReturnsFixedError(t *testing.T) {
	lease := &testLease{guardErr: errors.New("private-model-guard-path")}
	s := testSupervisor(t, "normal", lease)
	if _, err := s.EncodeQuery(context.Background(), "private-text"); err != ErrArtifact {
		t.Fatalf("guard error escaped: %v", err)
	}
	if !lease.isClosed() || s.Snapshot().PID != 0 {
		t.Fatal("failed guard left model ownership or native process")
	}
}

func (l *testLease) Open(string) (*os.File, error) {
	if l.openErr != nil {
		return nil, l.openErr
	}
	f, e := os.Open(os.DevNull)
	if e == nil {
		l.mu.Lock()
		l.files = append(l.files, f)
		l.mu.Unlock()
	}
	return f, e
}
func (l *testLease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	l.closeCalls++
	return l.closeErr
}
func (l *testLease) isClosed() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.closed }
func testSupervisor(t *testing.T, mode string, lease *testLease) *Supervisor {
	t.Helper()
	s, err := newTestSupervisor(t, Config{Executable: os.Args[0], Args: []string{"-test.run=TestSupervisorHelperProcess"}, Environment: []string{"WA_ENCODER_HELPER=" + mode}, Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return lease, nil }, IdleTimeout: 50 * time.Millisecond, RequestTimeout: time.Second, TerminationGrace: 20 * time.Millisecond, CleanupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func TestWorkerExitFailsInFlight(t *testing.T) {
	l := &testLease{}
	s := testSupervisor(t, "exit", l)
	_, err := s.EncodeQuery(context.Background(), "secret-canary")
	if !errors.Is(err, ErrWorkerFailed) {
		t.Fatalf("exit: %v", err)
	}
	if !l.isClosed() {
		t.Fatal("lease retained after verified exit")
	}
	if strings.Contains(s.Snapshot().Code, "secret") {
		t.Fatal("input diagnostic")
	}
}
func TestTimeoutDiscardsLateResponse(t *testing.T) {
	l := &testLease{}
	marker := filepath.Join(t.TempDir(), "query-inflight")
	s, err := newTestSupervisor(t, Config{
		Executable: os.Args[0], Args: []string{"-test.run=TestSupervisorHelperProcess"},
		Environment: []string{"WA_ENCODER_HELPER=late", "WA_ENCODER_INFLIGHT_FILE=" + marker},
		Profile:     ReferenceProfile(), AllowExperimental: true,
		Acquire:     func(context.Context, Profile) (ComponentLease, error) { return l, nil },
		IdleTimeout: time.Second, RequestTimeout: time.Second,
		TerminationGrace: 20 * time.Millisecond, CleanupTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.EncodeQuery(context.Background(), "warm fixture"); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().PID == 0 {
		t.Fatal("warm late-response worker is not running")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal("warm query did not reach the worker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = s.EncodeQuery(ctx, "sensitive")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline %v", err)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "query\n" {
		t.Fatal("timed query did not enter the late-response worker")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err = s.Unload(ctx2); err != nil {
		t.Fatal(err)
	}
	if !l.isClosed() {
		t.Fatal("lease release before cleanup")
	}
	if s.Snapshot().PID != 0 {
		t.Fatal("late worker retained")
	}
}
func TestWorkerNonfiniteWrongDimension(t *testing.T) {
	for _, mode := range []string{"dimension", "nonfinite"} {
		t.Run(mode, func(t *testing.T) {
			s := testSupervisor(t, mode, &testLease{})
			if _, err := s.EncodeQuery(context.Background(), "private"); !errors.Is(err, ErrProtocol) {
				t.Fatalf("bad response %v", err)
			}
		})
	}
}
func TestModelMissingLeavesKeywordAvailable(t *testing.T) {
	s, err := newTestSupervisor(t, Config{Executable: os.Args[0], Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return nil, ErrUnavailable }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Snapshot().PID != 0 {
		t.Fatal("eager launch")
	}
	if _, err = s.EncodeQuery(context.Background(), "text"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing model %v", err)
	}
}
func TestSupervisorQueueBound(t *testing.T) {
	s := testSupervisor(t, "late", &testLease{})
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = s.EncodeQuery(context.Background(), "private") }()
	}
	time.Sleep(80 * time.Millisecond)
	if n := s.Snapshot().Queued; n > 16 {
		t.Errorf("unbounded pending queue %d", n)
	}
	_ = s.Close()
	wg.Wait()
}
func TestWorkerFailureCodes(t *testing.T) {
	for _, test := range []struct {
		code string
		want error
	}{{CodeUnavailable, ErrUnavailable}, {CodeLimit, ErrLimit}, {CodeDeadline, context.DeadlineExceeded}, {CodeInvalid, ErrProtocol}} {
		if !errors.Is(responseError(test.code), test.want) {
			t.Errorf("code %s wrongly classified", test.code)
		}
	}
}
func TestShutdownCancelsModelAcquire(t *testing.T) {
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := newTestSupervisor(t, Config{Executable: os.Args[0], Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(ctx context.Context, _ Profile) (ComponentLease, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = s.EncodeQuery(ctx, "private") }()
	<-entered
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Error("close failed to cancel model acquisition")
		cancel()
		<-done
	}
}
func TestUnloadReportsAcquireReleaseFailure(t *testing.T) {
	entered := make(chan struct{})
	l := &testLease{closeErr: errors.New("private-close-detail")}
	s, err := newTestSupervisor(t, Config{Executable: os.Args[0], Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(ctx context.Context, _ Profile) (ComponentLease, error) {
		close(entered)
		<-ctx.Done()
		return l, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	queryDone := make(chan error, 1)
	go func() { _, e := s.EncodeQuery(context.Background(), "private"); queryDone <- e }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = s.Unload(ctx); !errors.Is(err, ErrCleanup) {
		t.Errorf("release failure hidden %v", err)
	}
	if err = <-queryDone; !errors.Is(err, ErrCleanup) {
		t.Errorf("lifecycle failure hidden %v", err)
	}
	if err = s.Close(); !errors.Is(err, ErrCleanup) {
		t.Errorf("close failure hidden %v", err)
	}
}
func TestLaunchReportsReleaseFailure(t *testing.T) {
	for _, openFail := range []bool{true, false} {
		t.Run(map[bool]string{true: "open", false: "start"}[openFail], func(t *testing.T) {
			l := &testLease{closeErr: errors.New("private-release")}
			if openFail {
				l.openErr = ErrArtifact
			}
			s, e := newTestSupervisor(t, Config{Executable: "/no-such-encoder-worker", Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return l, nil }})
			if e != nil {
				t.Fatal(e)
			}
			_, e = s.EncodeQuery(context.Background(), "private")
			if !errors.Is(e, ErrCleanup) {
				t.Errorf("launch cleanup hidden %v", e)
			}
			if e = s.Close(); !errors.Is(e, ErrCleanup) {
				t.Errorf("close cleanup hidden %v", e)
			}
		})
	}
}
func TestCleanupRequiresReapWindow(t *testing.T) {
	_, err := newTestSupervisor(t, Config{Executable: os.Args[0], Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return &testLease{}, nil }, TerminationGrace: 20 * time.Millisecond, CleanupTimeout: 20*time.Millisecond + time.Nanosecond})
	if !errors.Is(err, ErrLimit) {
		t.Fatal("insufficient kill/reap interval accepted")
	}
}

type testComputeGuard struct {
	release func()
	ctx     context.Context
}

func (g *testComputeGuard) Release() { g.release() }
func (g *testComputeGuard) Context() context.Context {
	if g.ctx != nil {
		return g.ctx
	}
	return context.Background()
}
func TestNativeExitPrecedesComputeRelease(t *testing.T) {
	var pid atomic.Int64
	released := make(chan bool, 1)
	s, e := newTestSupervisor(t, Config{Executable: os.Args[0], Args: []string{"-test.run=TestSupervisorHelperProcess"}, Environment: []string{"WA_ENCODER_HELPER=late"}, Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return &testLease{}, nil }, Admit: func(context.Context, AdmissionClass, int) (ComputeLease, error) {
		return &testComputeGuard{release: func() { released <- errors.Is(syscall.Kill(-int(pid.Load()), 0), syscall.ESRCH) }}, nil
	}, RequestTimeout: time.Second, TerminationGrace: 20 * time.Millisecond, CleanupTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queryDone := make(chan error, 1)
	go func() { _, e := s.EncodeQuery(ctx, "private"); queryDone <- e }()
	deadline := time.Now().Add(time.Second)
	for {
		snapshot := s.Snapshot()
		if snapshot.State == Running && snapshot.PID != 0 {
			pid.Store(int64(snapshot.PID))
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not run")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if e = <-queryDone; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	select {
	case absent := <-released:
		if !absent {
			t.Fatal("compute released before process group exit")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("compute guard not released")
	}
}
func TestComputeAdmissionClassAndRelease(t *testing.T) {
	classes := make(chan AdmissionClass, 2)
	released := make(chan struct{}, 2)
	s, e := newTestSupervisor(t, Config{Executable: os.Args[0], Args: []string{"-test.run=TestSupervisorHelperProcess"}, Environment: []string{"WA_ENCODER_HELPER=normal"}, Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return &testLease{}, nil }, Admit: func(_ context.Context, c AdmissionClass, threads int) (ComputeLease, error) {
		if threads != 2 {
			t.Error("compute thread contract")
		}
		classes <- c
		return &testComputeGuard{release: func() { released <- struct{}{} }}, nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.EncodeQuery(context.Background(), "query"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.EncodePassages(context.Background(), []string{"passage"}); e != nil {
		t.Fatal(e)
	}
	if <-classes != ForegroundQuery || <-classes != BackgroundText {
		t.Fatal("incorrect class")
	}
	if len(released) != 2 {
		t.Fatal("compute not released at completed native boundaries")
	}
}
func TestComputeAdmissionContextLivesUntilNativeBoundary(t *testing.T) {
	s, e := newTestSupervisor(t, Config{Executable: os.Args[0], Args: []string{"-test.run=TestSupervisorHelperProcess"}, Environment: []string{"WA_ENCODER_HELPER=normal"}, Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return &testLease{}, nil }, Admit: func(ctx context.Context, _ AdmissionClass, _ int) (ComputeLease, error) {
		return &testComputeGuard{ctx: ctx, release: func() {}}, nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.EncodeQuery(context.Background(), "private"); e != nil {
		t.Fatalf("live compute context cancelled early %v", e)
	}
}
func TestComputeGuardSurvivesWorkerRollover(t *testing.T) {
	s, e := newTestSupervisor(t, Config{Executable: os.Args[0], Args: []string{"-test.run=TestSupervisorHelperProcess"}, Environment: []string{"WA_ENCODER_HELPER=normal"}, Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return &testLease{}, nil }, Admit: func(ctx context.Context, _ AdmissionClass, _ int) (ComputeLease, error) {
		ctx, cancel := context.WithCancel(ctx)
		return &testComputeGuard{ctx: ctx, release: cancel}, nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	for i := 0; i < MaxWorkerRequests+1; i++ {
		if _, e = s.EncodeQuery(context.Background(), "private"); e != nil {
			t.Fatalf("rollover request %d: %v", i, e)
		}
	}
}
func TestAlreadyCancelledComputeGuardNeverLoadsModel(t *testing.T) {
	guardctx, cancel := context.WithCancel(context.Background())
	cancel()
	var loads atomic.Int64
	s, e := newTestSupervisor(t, Config{Executable: os.Args[0], Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { loads.Add(1); return &testLease{}, nil }, Admit: func(context.Context, AdmissionClass, int) (ComputeLease, error) {
		return &testComputeGuard{ctx: guardctx, release: func() {}}, nil
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.EncodeQuery(context.Background(), "private"); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancelled guard %v", e)
	}
	if loads.Load() != 0 {
		t.Fatal("model loaded under cancelled guard")
	}
}
func TestComputeProviderErrorIsFixed(t *testing.T) {
	s, e := newTestSupervisor(t, Config{Executable: os.Args[0], Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return &testLease{}, nil }, Admit: func(context.Context, AdmissionClass, int) (ComputeLease, error) {
		return nil, errors.New("private-admission-detail")
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.EncodeQuery(context.Background(), "private"); e != ErrUnavailable {
		t.Fatalf("provider detail escaped %v", e)
	}
}
func TestWrappedModelCleanupErrorIsFixed(t *testing.T) {
	s, e := newTestSupervisor(t, Config{Executable: os.Args[0], Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) {
		return nil, fmt.Errorf("private-component: %w", ErrCleanup)
	}})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.EncodeQuery(context.Background(), "private"); e != ErrCleanup {
		t.Fatalf("wrapped cleanup detail escaped %v", e)
	}
}
func TestLateComputeAdmissionReleasedAfterClose(t *testing.T) {
	entered, allowReturn, released := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s, e := newTestSupervisor(t, Config{Executable: os.Args[0], Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return &testLease{}, nil }, Admit: func(context.Context, AdmissionClass, int) (ComputeLease, error) {
		close(entered)
		<-allowReturn
		return &testComputeGuard{release: func() { close(released) }}, nil
	}, TerminationGrace: time.Millisecond, CleanupTimeout: 60 * time.Millisecond, RequestTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	queryDone := make(chan struct{})
	go func() { _, _ = s.EncodeQuery(ctx, "private"); close(queryDone) }()
	<-entered
	cancel()
	<-queryDone
	if e = s.Close(); !errors.Is(e, ErrCleanup) {
		t.Fatalf("expected pending cleanup %v", e)
	}
	time.Sleep(140 * time.Millisecond)
	close(allowReturn)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("late compute guard abandoned")
	}
}
func TestForegroundBypassesWaitingBackgroundAdmission(t *testing.T) {
	entered := make(chan struct{}, 1)
	s, e := newTestSupervisor(t, Config{Executable: os.Args[0], Args: []string{"-test.run=TestSupervisorHelperProcess"}, Environment: []string{"WA_ENCODER_HELPER=normal"}, Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return &testLease{}, nil }, Admit: func(ctx context.Context, c AdmissionClass, _ int) (ComputeLease, error) {
		if c == BackgroundText {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &testComputeGuard{ctx: ctx, release: func() {}}, nil
	}, RequestTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	bgctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bgDone := make(chan struct{})
	go func() { _, _ = s.EncodePassages(bgctx, []string{"background"}); close(bgDone) }()
	<-entered
	ctx, stop := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer stop()
	if _, e = s.EncodeQuery(ctx, "foreground"); e != nil {
		t.Fatalf("foreground blocked by nonnative admission %v", e)
	}
	cancel()
	<-bgDone
}
func TestCloseKeepsCleanupOwnerUntilComponentRelease(t *testing.T) {
	l := &testLease{openErr: ErrArtifact, closeErr: errors.New("private-close")}
	released := make(chan struct{}, 1)
	s, e := newTestSupervisor(t, Config{Executable: os.Args[0], Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return l, nil }, Admit: func(ctx context.Context, _ AdmissionClass, _ int) (ComputeLease, error) {
		return &testComputeGuard{ctx: ctx, release: func() { released <- struct{}{} }}, nil
	}, TerminationGrace: time.Millisecond, CleanupTimeout: 60 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.EncodeQuery(context.Background(), "private"); !errors.Is(e, ErrCleanup) {
		t.Fatal(e)
	}
	if e = s.Close(); !errors.Is(e, ErrCleanup) {
		t.Fatal(e)
	}
	if len(released) != 0 {
		t.Fatal("compute guard released with failed component cleanup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if e = s.AwaitCleanup(ctx); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("incomplete cleanup acknowledged: %v", e)
	}
	l.mu.Lock()
	l.closeErr = nil
	l.mu.Unlock()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("component cleanup owner abandoned after Close")
	}
	if e = s.AwaitCleanup(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestAwaitCleanupRequiresClosedOwner(t *testing.T) {
	s := testSupervisor(t, "normal", &testLease{})
	if e := s.AwaitCleanup(nil); e != ErrProtocol {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if e := s.AwaitCleanup(ctx); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("open supervisor acknowledged shutdown: %v", e)
	}
	if e := s.Close(); e != nil {
		t.Fatal(e)
	}
	if e := s.AwaitCleanup(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func TestLateAdmissionCleanupRetriesArePaced(t *testing.T) {
	l := &testLease{}
	entered, allowReturn, released := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var allowOnce sync.Once
	s, e := newTestSupervisor(t, Config{Executable: os.Args[0], Args: []string{"-test.run=TestSupervisorHelperProcess"}, Environment: []string{"WA_ENCODER_HELPER=normal"}, Profile: ReferenceProfile(), AllowExperimental: true, Acquire: func(context.Context, Profile) (ComponentLease, error) { return l, nil }, Admit: func(ctx context.Context, class AdmissionClass, _ int) (ComputeLease, error) {
		if class == ForegroundQuery {
			return &testComputeGuard{ctx: ctx, release: func() {}}, nil
		}
		close(entered)
		<-allowReturn
		return &testComputeGuard{release: func() { close(released) }}, nil
	}, TerminationGrace: time.Millisecond, CleanupTimeout: 60 * time.Millisecond, RequestTimeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		l.mu.Lock()
		l.closeErr = nil
		l.mu.Unlock()
		allowOnce.Do(func() { close(allowReturn) })
		_ = s.Close()
	})
	if _, e = s.EncodeQuery(context.Background(), "warm-worker"); e != nil {
		t.Fatal(e)
	}
	l.mu.Lock()
	l.closeErr = errors.New("private-release")
	l.mu.Unlock()
	backgroundDone := make(chan struct{})
	go func() { _, _ = s.EncodePassages(context.Background(), []string{"private"}); close(backgroundDone) }()
	<-entered
	if e = s.Close(); !errors.Is(e, ErrCleanup) {
		t.Fatalf("expected pending cleanup %v", e)
	}
	<-backgroundDone
	l.mu.Lock()
	before := l.closeCalls
	l.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	l.mu.Lock()
	retries := l.closeCalls - before
	l.closeErr = nil
	l.mu.Unlock()
	if retries > 3 {
		t.Fatalf("cleanup retry loop is unpaced: %d retries", retries)
	}
	allowOnce.Do(func() { close(allowReturn) })
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("late compute guard abandoned after component cleanup recovered")
	}
}
func TestIdleUnloadReleasesLease(t *testing.T) {
	l := &testLease{}
	s := testSupervisor(t, "normal", l)
	if _, err := s.EncodeQuery(context.Background(), "text"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.Snapshot().PID != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !l.isClosed() || s.Snapshot().PID != 0 {
		t.Fatal("idle process not reaped")
	}
}
func TestUnloadReapsProcessTree(t *testing.T) {
	l := &testLease{}
	s := testSupervisor(t, "tree", l)
	if _, err := s.EncodeQuery(context.Background(), "text"); err != nil {
		t.Fatal(err)
	}
	pid := s.Snapshot().PID
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Unload(ctx); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(-pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process group exists %v", err)
	}
	if !l.isClosed() {
		t.Fatal("lease not released after group exit")
	}
}
func TestSupervisorHelperProcess(t *testing.T) {
	mode := os.Getenv("WA_ENCODER_HELPER")
	if mode == "" {
		return
	}
	if mode == "environment" && !supervisorFixtureEnvironmentValid() {
		os.Exit(9)
	}
	in := os.NewFile(3, "private-in")
	out := os.NewFile(4, "private-out")
	defer in.Close()
	defer out.Close()
	r, err := ReadRequest(in)
	if err != nil {
		os.Exit(4)
	}
	if err = WriteResponse(out, Response{ID: r.ID, Op: r.Op, Code: "ok"}); err != nil {
		os.Exit(5)
	}
	if mode == "tree" {
		readyRead, readyWrite, e := os.Pipe()
		if e != nil {
			os.Exit(6)
		}
		cmd := exec.Command(os.Args[0], "-test.run=TestSupervisorDescendant")
		cmd.Env = append(os.Environ(), "WA_ENCODER_DESCENDANT=1")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		cmd.ExtraFiles = []*os.File{readyWrite}
		if cmd.Start() != nil {
			os.Exit(6)
		}
		readyWrite.Close()
		if _, e = io.ReadFull(readyRead, make([]byte, 1)); e != nil {
			os.Exit(6)
		}
		readyRead.Close()
	}
	for {
		r, err = ReadRequest(in)
		if err != nil {
			os.Exit(0)
		}
		if mode == "exit" {
			os.Exit(7)
		}
		if mode == "late" {
			if marker := os.Getenv("WA_ENCODER_INFLIGHT_FILE"); marker != "" {
				if os.WriteFile(marker, []byte("query\n"), 0600) != nil {
					os.Exit(10)
				}
			}
			time.Sleep(300 * time.Millisecond)
		}
		v := make([]float32, 384)
		v[0] = 1
		if mode == "dimension" {
			v = v[:3]
		}
		if mode == "nonfinite" {
			body := []byte(`{"id":2,"op":"query","code":"ok","vectors":[[1e1000]]}`)
			header := make([]byte, 4)
			binary.BigEndian.PutUint32(header, uint32(len(body)))
			_, _ = out.Write(header)
			_, _ = out.Write(body)
			os.Exit(0)
		}
		resp := Response{ID: r.ID, Op: r.Op, Code: "ok", Vectors: [][]float32{v}}
		if mode == "dimension" {
			data := `{"id":` + "2" + `,"op":"query","code":"ok","vectors":[[1,0,0]]}`
			header := []byte{0, 0, 0, byte(len(data))}
			out.Write(header)
			out.Write([]byte(data))
			continue
		}
		if err = WriteResponse(out, resp); err != nil {
			os.Exit(8)
		}
	}
}
func TestSupervisorDescendant(t *testing.T) {
	if os.Getenv("WA_ENCODER_DESCENDANT") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	ready := os.NewFile(3, "ready")
	_, _ = ready.Write([]byte{1})
	ready.Close()
	for {
		time.Sleep(time.Second)
	}
}
