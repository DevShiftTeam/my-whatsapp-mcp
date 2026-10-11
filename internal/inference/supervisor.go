package inference

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/workerlife"
)

type State string

const (
	Unloaded State = "unloaded"
	Starting State = "starting"
	Ready    State = "ready"
	Running  State = "running"
	Stopping State = "stopping"
	Failed   State = "failed"
	Closed   State = "closed"
)

type Snapshot struct {
	State           State
	PID             int
	ProfileID, Code string
	Queued          int
}
type Config struct {
	Executable                                                    string
	Args, Environment                                             []string
	Profile                                                       Profile
	AllowExperimental                                             bool
	Acquire                                                       LeaseProvider
	Admit                                                         AdmissionProvider
	WorkerLifetime                                                workerlife.Config
	InstanceGuard                                                 func() (*os.File, error)
	RequestTimeout, IdleTimeout, TerminationGrace, CleanupTimeout time.Duration
}
type responseResult struct {
	response Response
	err      error
}
type call struct {
	ctx     context.Context
	request Request
	reply   chan responseResult
}
type unloadCall struct{ reply chan error }
type acquisitionCleanup struct {
	lease ComponentLease
	err   error
}
type admittedCompute struct {
	lease ComputeLease
	err   error
}

var errAdmissionYield = errors.New("encoder_admission_yield")

type workerProcess struct {
	cmd           *exec.Cmd
	input, output *os.File
	files         []*os.File
	lease         ComponentLease
	life          *workerlife.Lease
	exited        chan struct{}
	waitErr       error
	reaped        bool
}
type Supervisor struct {
	config           Config
	mu               sync.Mutex
	snapshot         Snapshot
	requests         chan call
	admissions       chan struct{}
	unloads          chan unloadCall
	shutdown         chan struct{}
	done             chan struct{}
	cleanupDone      chan struct{}
	cleanupOnce      sync.Once
	closeOnce        sync.Once
	closeErr         error
	closing          bool
	worker           *workerProcess
	nextID           uint64
	requestsUsed     int
	acquirePending   <-chan acquisitionCleanup
	pendingLease     ComponentLease
	pendingLife      *workerlife.Lease
	computeGuard     ComputeLease
	computeCancel    context.CancelFunc
	admissionPending <-chan admittedCompute
	deferred         []call
}

func NewSupervisor(config Config) (*Supervisor, error) {
	if config.Profile.Validate() != nil || (!config.AllowExperimental && !config.Profile.ReleaseSelected) || config.Acquire == nil || config.InstanceGuard == nil || !workerlife.ValidateExecutableConfig(config.WorkerLifetime) || (config.WorkerLifetime.ExecutableFile == nil && !filepath.IsAbs(config.WorkerLifetime.Executable)) || !filepath.IsAbs(config.Executable) {
		return nil, ErrUnavailable
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 30 * time.Second
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = 5 * time.Minute
	}
	if config.TerminationGrace == 0 {
		config.TerminationGrace = 100 * time.Millisecond
	}
	if config.CleanupTimeout == 0 {
		config.CleanupTimeout = 3 * time.Second
	}
	if config.RequestTimeout <= 0 || config.RequestTimeout > MaxRequestDuration || config.IdleTimeout <= 0 || config.IdleTimeout > time.Hour || config.TerminationGrace <= 0 || config.TerminationGrace > time.Second || config.CleanupTimeout < config.TerminationGrace+50*time.Millisecond || config.CleanupTimeout > 10*time.Second {
		return nil, ErrLimit
	}
	s := &Supervisor{config: config, snapshot: Snapshot{State: Unloaded, ProfileID: config.Profile.ID}, requests: make(chan call, 16), admissions: make(chan struct{}, 16), unloads: make(chan unloadCall, 4), shutdown: make(chan struct{}), done: make(chan struct{}), cleanupDone: make(chan struct{})}
	go s.loop()
	return s, nil
}
func (s *Supervisor) Snapshot() Snapshot { s.mu.Lock(); defer s.mu.Unlock(); return s.snapshot }
func (s *Supervisor) setState(state State, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot.State = state
	s.snapshot.Code = code
	s.snapshot.PID = 0
	if s.worker != nil {
		s.snapshot.PID = s.worker.cmd.Process.Pid
	}
}
func (s *Supervisor) TokenizerIdentity() TokenizerIdentity {
	return TokenizerIdentity{ProfileID: s.config.Profile.ID, TokenizerSHA256: s.config.Profile.TokenizerSHA256, Implementation: "daulet-tokenizers-v1.27.0-hf0.22", PrefixVersion: "e5-passage-special-v1"}
}
func (s *Supervisor) EncodeQuery(ctx context.Context, text string) ([]float32, error) {
	r, e := s.submit(ctx, Request{Op: OpQuery, Text: text})
	if e != nil {
		return nil, e
	}
	return r.Vectors[0], nil
}
func (s *Supervisor) EncodePassages(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	if len(texts) > MaxBatchItems {
		return nil, ErrLimit
	}
	r, e := s.submit(ctx, Request{Op: OpPassages, Texts: append([]string(nil), texts...)})
	return r.Vectors, e
}
func (s *Supervisor) CountPassage(ctx context.Context, text string) (int, error) {
	r, e := s.submit(ctx, Request{Op: OpCount, Text: text})
	return r.Count, e
}
func (s *Supervisor) SplitPassage(ctx context.Context, text string, maxTokens int) ([]TextSpan, error) {
	r, e := s.submit(ctx, Request{Op: OpSplit, Text: text, MaxTokens: maxTokens})
	if e != nil {
		return nil, e
	}
	if strings.Join(r.Chunks, "") != text || len(r.Chunks) != len(r.Counts) {
		return nil, ErrProtocol
	}
	spans := make([]TextSpan, len(r.Chunks))
	offset := 0
	for i, chunk := range r.Chunks {
		if r.Counts[i] > maxTokens {
			return nil, ErrProtocol
		}
		spans[i] = TextSpan{Start: offset, End: offset + len(chunk), Text: chunk, Tokens: r.Counts[i]}
		offset += len(chunk)
	}
	return spans, nil
}
func (s *Supervisor) submit(ctx context.Context, request Request) (Response, error) {
	if ctx == nil {
		return Response{}, ErrProtocol
	}
	ctx, cancel := context.WithTimeout(ctx, s.config.RequestTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	request.DeadlineUnixNano = deadline.UnixNano()
	request.ID = 1
	if err := validateRequest(request); err != nil {
		return Response{}, err
	}
	request.ID = 0
	c := call{ctx: ctx, request: request, reply: make(chan responseResult, 1)}
	select {
	case <-s.shutdown:
		return Response{}, ErrClosed
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case s.admissions <- struct{}{}:
	}
	s.mu.Lock()
	if s.closing || ctx.Err() != nil {
		s.mu.Unlock()
		<-s.admissions
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, ErrClosed
	}
	s.requests <- c
	s.mu.Unlock()
	select {
	case r := <-c.reply:
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return r.response, r.err
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case <-s.done:
		return Response{}, ErrClosed
	}
}
func (s *Supervisor) Unload(ctx context.Context) error {
	if ctx == nil {
		return ErrProtocol
	}
	u := unloadCall{reply: make(chan error, 1)}
	select {
	case <-s.done:
		return s.closeErr
	case <-ctx.Done():
		return ctx.Err()
	case s.unloads <- u:
	}
	select {
	case e := <-u.reply:
		return e
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.closeErr
	}
}
func (s *Supervisor) Close() error {
	s.closeOnce.Do(func() { s.mu.Lock(); s.closing = true; close(s.shutdown); s.mu.Unlock() })
	<-s.done
	return s.closeErr
}

// AwaitCleanup acknowledges shutdown only after every provider result, process,
// model component and compute guard has been released. Close may return
// ErrCleanup earlier while an actor-owned retry remains in progress.
func (s *Supervisor) AwaitCleanup(ctx context.Context) error {
	if ctx == nil {
		return ErrProtocol
	}
	select {
	case <-s.cleanupDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Supervisor) finish(c call, r responseResult) { c.reply <- r; <-s.admissions }
func (s *Supervisor) finishShutdown(pending []call) {
	pending = append(pending, s.deferred...)
	s.deferred = nil
	for i, c := range pending {
		s.finish(c, responseResult{err: ErrClosed})
		pending[i] = call{}
	}
	for {
		select {
		case c := <-s.requests:
			s.finish(c, responseResult{err: ErrClosed})
		default:
			s.mu.Lock()
			s.snapshot.Queued = 0
			s.mu.Unlock()
			return
		}
	}
}

func (s *Supervisor) loop() {
	defer close(s.done)
	defer s.finalizeLateCleanup()
	pending := []call{}
	idle := time.NewTimer(s.config.IdleTimeout)
	defer idle.Stop()
	resetIdle := func() {
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(s.config.IdleTimeout)
	}
	for {
		pending = append(pending, s.deferred...)
		s.deferred = nil
		// Drain arrivals at each native boundary so a queued query leads background.
		draining := true
		for draining {
			select {
			case c := <-s.requests:
				pending = append(pending, c)
			default:
				draining = false
			}
		}
		s.mu.Lock()
		s.snapshot.Queued = len(pending)
		s.mu.Unlock()
		select {
		case <-s.shutdown:
			s.closeErr = s.stop()
			s.finishShutdown(pending)
			s.setState(Closed, "closed")
			return
		case u := <-s.unloads:
			u.reply <- s.stop()
			resetIdle()
			continue
		default:
		}
		if len(pending) > 0 {
			index := 0
			for i, c := range pending {
				if c.request.Op == OpQuery {
					index = i
					break
				}
			}
			c := pending[index]
			copy(pending[index:], pending[index+1:])
			pending[len(pending)-1] = call{}
			pending = pending[:len(pending)-1]
			if c.ctx.Err() != nil {
				s.finish(c, responseResult{err: c.ctx.Err()})
				continue
			}
			r := s.execute(c)
			if errors.Is(r.err, errAdmissionYield) {
				pending = append(pending, c)
				c = call{}
				resetIdle()
				continue
			}
			s.finish(c, r)
			c = call{}
			resetIdle()
			continue
		}
		var exited <-chan struct{}
		if s.worker != nil {
			exited = s.worker.exited
		}
		select {
		case <-s.shutdown:
			s.closeErr = s.stop()
			s.finishShutdown(pending)
			s.setState(Closed, "closed")
			return
		case u := <-s.unloads:
			u.reply <- s.stop()
			resetIdle()
		case c := <-s.requests:
			pending = append(pending, c)
		case <-idle.C:
			_ = s.stop()
			resetIdle()
		case <-exited:
			_ = s.stop()
			if s.worker == nil {
				s.setState(Unloaded, "worker_exited")
			}
			resetIdle()
		}
	}
}
func (s *Supervisor) launch(ctx context.Context) error {
	if s.pendingLease != nil || s.pendingLife != nil {
		return ErrCleanup
	}
	if s.acquirePending != nil {
		select {
		case r := <-s.acquirePending:
			s.acquirePending = nil
			if r.err != nil {
				s.pendingLease = r.lease
				return ErrCleanup
			}
		default:
			return ErrCleanup
		}
	}
	s.setState(Starting, "loading")
	lease, err := s.acquire(ctx)
	if err != nil {
		if errors.Is(err, ErrCleanup) {
			s.setState(Failed, "cleanup_incomplete")
			return ErrCleanup
		}
		if errors.Is(err, ErrClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			s.setState(Unloaded, "cancelled")
			return fixedProviderError(err)
		}
		s.setState(Unloaded, "model_unavailable")
		return ErrUnavailable
	}
	if lease == nil {
		s.setState(Unloaded, "model_unavailable")
		return ErrUnavailable
	}
	files := []*os.File{}
	cleanup := func(cause error, code string) error {
		for _, f := range files {
			f.Close()
		}
		if lease.Close() != nil {
			s.pendingLease = lease
			s.setState(Failed, "lease_release_failed")
			return ErrCleanup
		}
		s.setState(Unloaded, code)
		return cause
	}
	for _, name := range []string{"model", "tokenizer", "runtime"} {
		f, e := lease.Open(name)
		if e != nil || f == nil {
			return cleanup(ErrArtifact, "artifact_invalid")
		}
		files = append(files, f)
	}
	instanceGuard, guardErr := s.config.InstanceGuard()
	if guardErr != nil || instanceGuard == nil {
		if instanceGuard != nil {
			instanceGuard.Close()
		}
		return cleanup(ErrArtifact, "guard_invalid")
	}
	defer instanceGuard.Close()
	modelGuard, guardErr := lease.WorkerGuard()
	if guardErr != nil || modelGuard == nil {
		if modelGuard != nil {
			modelGuard.Close()
		}
		return cleanup(ErrArtifact, "guard_invalid")
	}
	defer modelGuard.Close()
	if ctx.Err() != nil {
		return cleanup(ctx.Err(), "cancelled")
	}
	childInput, input, err := os.Pipe()
	if err != nil {
		return cleanup(ErrWorkerFailed, "worker_start_failed")
	}
	output, childOutput, err := os.Pipe()
	if err != nil {
		childInput.Close()
		input.Close()
		return cleanup(ErrWorkerFailed, "worker_start_failed")
	}
	cmd := exec.Command(s.config.Executable, s.config.Args...)
	cmd.Env = workerlife.WithoutLoaderOverrides(os.Environ(), s.config.Environment)
	cmd.Env = append(cmd.Env, "GOMAXPROCS=2", "TOKENIZERS_PARALLELISM=false", "OMP_NUM_THREADS=2", "OPENBLAS_NUM_THREADS=2", "MKL_NUM_THREADS=2", "VECLIB_MAXIMUM_THREADS=2")
	cmd.ExtraFiles = append([]*os.File{childInput, childOutput}, files...)
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if configureProcessGroup(cmd) != nil {
		childInput.Close()
		input.Close()
		output.Close()
		childOutput.Close()
		return cleanup(ErrUnavailable, "worker_unavailable")
	}
	life, startErr := workerlife.Start(ctx, s.config.WorkerLifetime, cmd, []*os.File{instanceGuard, modelGuard})
	childInput.Close()
	childOutput.Close()
	if cmd.Process == nil {
		input.Close()
		output.Close()
		if life == nil {
			return cleanup(ErrWorkerFailed, "worker_start_failed")
		}
		for _, file := range files {
			file.Close()
		}
		s.pendingLife, s.pendingLease = life, lease
		s.setState(Failed, "cleanup_incomplete")
		if s.stop() != nil {
			return ErrCleanup
		}
		return ErrWorkerFailed
	}
	// Start may return a lease and error after native.Start. Install both before
	// handling that error so this actor remains the sole native Wait/reap owner.
	w := &workerProcess{cmd: cmd, input: input, output: output, files: files, lease: lease, life: life, exited: make(chan struct{})}
	s.worker = w
	s.requestsUsed = 0
	go func() { w.waitErr = cmd.Wait(); close(w.exited) }()
	if startErr != nil {
		if s.stop() != nil {
			return ErrCleanup
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrWorkerFailed
	}
	s.setState(Starting, "loading")
	s.nextID++
	deadline, _ := ctx.Deadline()
	request := Request{ID: s.nextID, Op: OpInit, DeadlineUnixNano: deadline.UnixNano(), Init: &WorkerInit{Profile: s.config.Profile, Paths: ComponentPaths{Model: "/dev/fd/5", Tokenizer: "/dev/fd/6", Runtime: "/dev/fd/7"}}}
	result := s.exchange(ctx, request)
	if result.err != nil {
		_ = s.stop()
		return result.err
	}
	if result.response.Code != "ok" {
		_ = s.stop()
		return responseError(result.response.Code)
	}
	s.setState(Ready, "ready")
	return nil
}
func (s *Supervisor) acquire(ctx context.Context) (ComponentLease, error) {
	type acquired struct {
		lease ComponentLease
		err   error
	}
	workctx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan acquired, 1)
	go func() { lease, err := s.config.Acquire(workctx, s.config.Profile); result <- acquired{lease, err} }()
	var reason error
	var unload *unloadCall
	select {
	case r := <-result:
		if ctx.Err() != nil {
			if r.lease != nil {
				if r.lease.Close() != nil {
					s.pendingLease = r.lease
					return nil, ErrCleanup
				}
			}
			return nil, ctx.Err()
		}
		if r.err != nil && r.lease != nil {
			if r.lease.Close() != nil {
				s.pendingLease = r.lease
				return nil, ErrCleanup
			}
			return nil, r.err
		}
		return r.lease, r.err
	case <-ctx.Done():
		reason = ctx.Err()
	case <-s.shutdown:
		reason = ErrClosed
	case u := <-s.unloads:
		unload = &u
		reason = ErrUnavailable
	}
	cancel()
	timer := time.NewTimer(s.config.CleanupTimeout)
	defer timer.Stop()
	select {
	case r := <-result:
		var cleanupErr error
		if r.lease != nil {
			if r.lease.Close() != nil {
				s.pendingLease = r.lease
				cleanupErr = ErrCleanup
			}
		}
		if unload != nil {
			unload.reply <- cleanupErr
		}
		if cleanupErr != nil {
			return nil, cleanupErr
		}
		return nil, reason
	case <-timer.C:
		done := make(chan acquisitionCleanup, 1)
		s.acquirePending = done
		go func() {
			r := <-result
			var err error
			if r.lease != nil {
				err = r.lease.Close()
			}
			done <- acquisitionCleanup{r.lease, err}
		}()
		if unload != nil {
			unload.reply <- ErrCleanup
		}
		return nil, ErrCleanup
	}
}
func (s *Supervisor) execute(c call) responseResult {
	if s.computeGuard != nil || s.admissionPending != nil || s.pendingLife != nil || s.pendingLease != nil || s.acquirePending != nil {
		if e := s.stop(); e != nil {
			return responseResult{err: e}
		}
	}
	if s.worker != nil && s.requestsUsed >= MaxWorkerRequests {
		if e := s.stop(); e != nil {
			return responseResult{err: e}
		}
	}
	if s.config.Admit != nil {
		class := BackgroundText
		if c.request.Op == OpQuery {
			class = ForegroundQuery
		}
		guard, e := s.admit(c.ctx, class)
		if e != nil {
			return responseResult{err: e}
		}
		s.computeGuard = guard
		ctx, cancel := context.WithCancel(c.ctx)
		stopCancellation := context.AfterFunc(guard.Context(), cancel)
		if guard.Context().Err() != nil {
			cancel()
		}
		c.ctx = ctx
		defer func() {
			stopCancellation()
			cancel()
			if s.Snapshot().State != Failed {
				s.releaseCompute()
			}
		}()
	}
	r := s.executeAdmitted(c)
	if s.computeGuard != nil && s.computeGuard.Context().Err() != nil && r.err == nil {
		reason := s.computeGuard.Context().Err()
		if e := s.stop(); e != nil {
			return responseResult{err: e}
		}
		return responseResult{err: reason}
	}
	return r
}
func (s *Supervisor) executeAdmitted(c call) responseResult {
	if s.worker == nil {
		if e := s.launch(c.ctx); e != nil {
			return responseResult{err: e}
		}
	} else {
		select {
		case <-s.worker.exited:
			_ = s.stop()
			return responseResult{err: ErrWorkerFailed}
		default:
		}
	}
	s.nextID++
	c.request.ID = s.nextID
	s.requestsUsed++
	s.setState(Running, "running")
	r := s.exchange(c.ctx, c.request)
	if r.err != nil {
		_ = s.stop()
		return r
	}
	if r.response.Code != "ok" {
		s.setState(Ready, r.response.Code)
		return responseResult{err: responseError(r.response.Code)}
	}
	if r.response.ID != c.request.ID || r.response.Op != c.request.Op || (c.request.Op == OpPassages && len(r.response.Vectors) != len(c.request.Texts)) || (c.request.Op == OpSplit && (strings.Join(r.response.Chunks, "") != c.request.Text)) {
		_ = s.stop()
		return responseResult{err: ErrProtocol}
	}
	s.setState(Ready, "ready")
	return r
}
func (s *Supervisor) releaseCompute() {
	guard := s.computeGuard
	s.computeGuard = nil
	if s.computeCancel != nil {
		s.computeCancel()
		s.computeCancel = nil
	}
	if guard != nil {
		guard.Release()
	}
}
func (s *Supervisor) admit(ctx context.Context, class AdmissionClass) (ComputeLease, error) {
	workctx, cancel := context.WithCancel(ctx)
	retained := false
	defer func() {
		if !retained {
			cancel()
		}
	}()
	result := make(chan admittedCompute, 1)
	go func() { lease, err := s.config.Admit(workctx, class, 2); result <- admittedCompute{lease, err} }()
	var reason error
	var unload *unloadCall
	for {
		select {
		case r := <-result:
			var leaseErr error
			if r.lease != nil && r.lease.Context() != nil {
				leaseErr = r.lease.Context().Err()
			}
			if ctx.Err() == nil && r.err == nil && r.lease != nil && r.lease.Context() != nil && leaseErr == nil {
				retained = true
				s.computeCancel = cancel
				return r.lease, nil
			}
			s.computeGuard = r.lease
			if e := s.stop(); e != nil {
				return nil, e
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if r.err != nil {
				return nil, fixedProviderError(r.err)
			}
			if leaseErr != nil {
				return nil, fixedProviderError(leaseErr)
			}
			return nil, ErrUnavailable
		case <-ctx.Done():
			reason = ctx.Err()
		case <-s.shutdown:
			reason = ErrClosed
		case u := <-s.unloads:
			unload = &u
			reason = ErrUnavailable
		case c := <-s.requests:
			s.deferred = append(s.deferred, c)
			if class != ForegroundQuery && c.request.Op == OpQuery {
				reason = errAdmissionYield
			} else {
				continue
			}
		}
		break
	}
	cancel()
	timer := time.NewTimer(s.config.CleanupTimeout)
	defer timer.Stop()
	select {
	case r := <-result:
		s.computeGuard = r.lease
		if errors.Is(reason, errAdmissionYield) {
			s.releaseCompute()
			return nil, reason
		}
		e := s.stop()
		if unload != nil {
			unload.reply <- e
		}
		if e != nil {
			return nil, e
		}
		return nil, reason
	case <-timer.C:
		s.admissionPending = result
		s.setState(Failed, "cleanup_incomplete")
		if unload != nil {
			unload.reply <- ErrCleanup
		}
		return nil, ErrCleanup
	}
}
func fixedProviderError(err error) error {
	for _, fixed := range []error{context.Canceled, context.DeadlineExceeded, ErrClosed, ErrCleanup, ErrUnavailable, ErrLimit, ErrArtifact, ErrProtocol, ErrWorkerFailed} {
		if errors.Is(err, fixed) {
			return fixed
		}
	}
	return ErrUnavailable
}

// A provider that ignores cancellation still has an owner after public Close
// returns ErrCleanup. No actor remains when this cleanup goroutine takes over.
func (s *Supervisor) finalizeLateCleanup() {
	if s.admissionPending == nil && s.acquirePending == nil && s.pendingLease == nil && s.pendingLife == nil && s.worker == nil && s.computeGuard == nil {
		s.cleanupOnce.Do(func() { close(s.cleanupDone) })
		return
	}
	go func() {
		<-s.done
		for {
			if s.stop() == nil {
				s.setState(Closed, "closed")
				s.cleanupOnce.Do(func() { close(s.cleanupDone) })
				return
			}
			time.Sleep(s.config.CleanupTimeout)
		}
	}()
}
func responseError(code string) error {
	switch code {
	case CodeUnavailable:
		return ErrUnavailable
	case "artifact_invalid":
		return ErrArtifact
	case CodeLimit:
		return ErrLimit
	case "cancelled":
		return context.Canceled
	case CodeDeadline:
		return context.DeadlineExceeded
	case CodeInvalid:
		return ErrProtocol
	default:
		return ErrWorkerFailed
	}
}
func (s *Supervisor) exchange(ctx context.Context, request Request) responseResult {
	w := s.worker
	done := make(chan responseResult, 1)
	go func() {
		if err := WriteRequest(w.input, request); err != nil {
			done <- responseResult{err: ErrWorkerFailed}
			return
		}
		r, err := ReadResponse(w.output)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				err = ErrWorkerFailed
			} else {
				err = ErrProtocol
			}
		}
		done <- responseResult{response: r, err: err}
	}()
	select {
	case r := <-done:
		if ctx.Err() != nil {
			return responseResult{err: ctx.Err()}
		}
		if r.err == nil && (r.response.ID != request.ID || r.response.Op != request.Op) {
			r.err = ErrProtocol
		}
		return r
	case <-ctx.Done():
		return responseResult{err: ctx.Err()}
	case <-s.shutdown:
		return responseResult{err: ErrClosed}
	case <-w.exited:
		return responseResult{err: ErrWorkerFailed}
	case u := <-s.unloads:
		e := s.stop()
		u.reply <- e
		if e != nil {
			return responseResult{err: e}
		}
		return responseResult{err: ErrUnavailable}
	}
}
func (s *Supervisor) stop() error {
	if e := s.stopWorker(); e != nil {
		return e
	}
	if s.admissionPending != nil {
		select {
		case r := <-s.admissionPending:
			s.admissionPending = nil
			s.computeGuard = r.lease
		case <-time.After(s.config.CleanupTimeout):
			s.setState(Failed, "cleanup_incomplete")
			return ErrCleanup
		}
	}
	if s.acquirePending != nil {
		select {
		case r := <-s.acquirePending:
			s.acquirePending = nil
			if r.err != nil {
				s.pendingLease = r.lease
			}
		case <-time.After(s.config.CleanupTimeout):
			s.setState(Failed, "cleanup_incomplete")
			return ErrCleanup
		}
	}
	if s.pendingLife != nil {
		ctx, cancel := context.WithTimeout(context.Background(), s.config.CleanupTimeout)
		err := s.pendingLife.AwaitCleanup(ctx)
		cancel()
		if err != nil {
			s.setState(Failed, "cleanup_incomplete")
			return ErrCleanup
		}
		s.pendingLife = nil
	}
	if s.pendingLease != nil {
		if s.pendingLease.Close() != nil {
			s.setState(Failed, "lease_release_failed")
			return ErrCleanup
		}
		s.pendingLease = nil
	}
	s.releaseCompute()
	s.setState(Unloaded, "unloaded")
	return nil
}
func (s *Supervisor) stopWorker() error {
	w := s.worker
	if w == nil {
		return nil
	}
	if w.reaped {
		if w.life != nil {
			ctx, cancel := context.WithTimeout(context.Background(), s.config.CleanupTimeout)
			err := w.life.AwaitCleanup(ctx)
			cancel()
			if err != nil {
				s.setState(Failed, "cleanup_incomplete")
				return ErrCleanup
			}
			w.life = nil
		}
		if w.lease.Close() != nil {
			s.setState(Failed, "lease_release_failed")
			return ErrCleanup
		}
		s.worker = nil
		return nil
	}
	s.setState(Stopping, "stopping")
	w.input.Close()
	w.output.Close()
	signalProcessGroup(w.cmd, false)
	deadline := time.NewTimer(s.config.CleanupTimeout)
	defer deadline.Stop()
	grace := time.NewTimer(s.config.TerminationGrace)
	defer grace.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	killed := false
	for {
		select {
		case <-w.exited:
			if !processGroupExists(w.cmd) {
				w.reaped = true
				for _, f := range w.files {
					f.Close()
				}
				return s.stopWorker()
			}
		default:
		}
		select {
		case <-grace.C:
			signalProcessGroup(w.cmd, true)
			killed = true
		case <-tick.C:
			if killed {
				signalProcessGroup(w.cmd, true)
			}
		case <-deadline.C:
			signalProcessGroup(w.cmd, true)
			s.setState(Failed, "cleanup_incomplete")
			return ErrCleanup
		}
	}
}
