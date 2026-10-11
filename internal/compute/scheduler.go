// Package compute owns shared declared CPU admission for every native runtime.
package compute

import (
	"context"
	"errors"
	"sync"
)

// SchedulerClass declares work against the same thread budget across runtimes.
type SchedulerClass uint8

const (
	ForegroundQuery SchedulerClass = iota + 1
	BackgroundText
	HeavyMedia
)

var (
	ErrSchedulerBudget        = errors.New("scheduler_invalid_budget")
	ErrSchedulerClosed        = errors.New("scheduler_closed")
	ErrSchedulerLeaseReleased = errors.New("scheduler_lease_released")
	ErrSchedulerWorkerFailure = errors.New("scheduler_worker_failure")
)

// ThreadBudgetScheduler controls declared threads, not arbitrary native runtime
// threads. Callers must configure each runtime to the admitted thread count.
// It starts no workers; lazy loading and idle worker unloading belong to runtime
// owners. Canceling work never pretends to preempt non-yielding native calls.
type ThreadBudgetScheduler struct {
	*schedulerState
}

// All mutable authority lives behind one pointer so even a copied public handle
// cannot create an independent lock or admission counter.
type schedulerState struct {
	mu              sync.Mutex
	maxThreads      int
	threads         int
	heavyOwner      *leaseState
	closed          bool
	paused          bool
	foregroundBurst int
	heavyBurst      int
	active          map[*leaseState]struct{}
	queue           []*schedulerWaiter
}
type schedulerWaiter struct {
	lease *leaseState
	ready chan error
}

// Threads/Active count current CPU admissions. Heavy counts the lifecycle guard,
// which remains held during a checkpoint or cancellation cleanup even when the
// model is quiescent and its CPU admission has been yielded.
type SchedulerSnapshot struct {
	Threads, Heavy, Active, Queued int
	Closed                         bool
	Paused                         bool
}

// SchedulerLease must be released only after native worker stop/unload and cleanup
// have finished. Release is idempotent. Checkpoint acknowledges that native CPU
// computation has stopped at a safe slice/token boundary until readmission; it
// yields CPU threads but keeps the heavy model lifecycle guard. Foreground work
// takes priority, then the same heavy lease resumes before a different model can
// load. A Checkpoint error does not release cleanup reservations: the caller must
// finish cleanup before Release (or return from Run). No OS preemption is implied.
// Lease methods may be called concurrently, except concurrent Checkpoint calls.
type SchedulerLease struct {
	self *SchedulerLease
	*leaseState
}

// Mutable lifetime state must not be rewound by saving/restoring a public lease
// handle. Copied handles also never read mutex-protected mutable fields directly.
type leaseState struct {
	scheduler *ThreadBudgetScheduler
	class     SchedulerClass
	threads   int
	ctx       context.Context
	cancel    context.CancelFunc
	admitted  bool
	released  bool
	waiting   *schedulerWaiter
}

func NewThreadBudgetScheduler(maxThreads int) (*ThreadBudgetScheduler, error) {
	if maxThreads < 1 || maxThreads > 2 {
		return nil, ErrSchedulerBudget
	}
	return &ThreadBudgetScheduler{schedulerState: &schedulerState{maxThreads: maxThreads, active: make(map[*leaseState]struct{})}}, nil
}
func (s *ThreadBudgetScheduler) Snapshot() SchedulerSnapshot {
	if s == nil || s.schedulerState == nil {
		return SchedulerSnapshot{Closed: true}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	heavy := 0
	if s.heavyOwner != nil {
		heavy = 1
	}
	return SchedulerSnapshot{Threads: s.threads, Heavy: heavy, Active: len(s.active), Queued: len(s.queue), Closed: s.closed, Paused: s.paused}
}

// SetPaused applies the app's persisted indexing setting. It does not persist a
// competing setting itself. Foreground queries remain available. Running work
// pauses only after acknowledging a safe Checkpoint, retaining cleanup guards.
func (s *ThreadBudgetScheduler) SetPaused(paused bool) {
	if s == nil || s.schedulerState == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = paused
	s.dispatchLocked()
}
func (s *ThreadBudgetScheduler) Acquire(ctx context.Context, class SchedulerClass, threads int) (*SchedulerLease, error) {
	if s == nil || s.schedulerState == nil || ctx == nil || threads < 1 || threads > s.maxThreads || class < ForegroundQuery || class > HeavyMedia {
		return nil, ErrSchedulerBudget
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	leaseCtx, cancel := context.WithCancel(ctx)
	// Capture an internal immutable handle: overwriting the caller's public
	// scheduler value cannot rebind a live lease to another authority.
	l := &SchedulerLease{leaseState: &leaseState{scheduler: &ThreadBudgetScheduler{schedulerState: s.schedulerState}, class: class, threads: threads, ctx: leaseCtx, cancel: cancel}}
	l.self = l
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return nil, ErrSchedulerClosed
	}
	w := s.enqueueLocked(l.leaseState)
	s.dispatchLocked()
	s.mu.Unlock()
	if err := l.leaseState.await(w); err != nil {
		l.Release()
		return nil, err
	}
	return l, nil
}
func (s *ThreadBudgetScheduler) enqueueLocked(l *leaseState) *schedulerWaiter {
	w := &schedulerWaiter{lease: l, ready: make(chan error, 1)}
	l.waiting = w
	s.queue = append(s.queue, w)
	return w
}
func (l *leaseState) await(w *schedulerWaiter) error {
	select {
	case err := <-w.ready:
		if err != nil {
			return err
		}
		if err := l.ctx.Err(); err != nil {
			return err
		}
		return nil
	case <-l.ctx.Done():
		l.scheduler.mu.Lock()
		closed := l.scheduler.closed
		l.scheduler.mu.Unlock()
		if closed {
			return ErrSchedulerClosed
		}
		return l.ctx.Err()
	}
}
func (l *SchedulerLease) Context() context.Context { return l.ctx }
func (s *ThreadBudgetScheduler) removeActiveLocked(l *leaseState) {
	if !l.admitted {
		return
	}
	delete(s.active, l)
	s.threads -= l.threads
	l.admitted = false
}
func (s *ThreadBudgetScheduler) removeWaiterLocked(w *schedulerWaiter, err error) {
	for i, item := range s.queue {
		if item == w {
			s.dropWaiterLocked(i)
			w.lease.waiting = nil
			w.ready <- err
			return
		}
	}
}

func (s *ThreadBudgetScheduler) dropWaiterLocked(i int) {
	copy(s.queue[i:], s.queue[i+1:])
	s.queue[len(s.queue)-1] = nil
	s.queue = s.queue[:len(s.queue)-1]
}

// The admission bound applies at cooperative boundaries: it does not promise
// wall-clock preemption of a native call. Eight consecutive query admissions
// drain before eligible background work reserves its required thread capacity.
const maxForegroundBurst = 8

func (s *ThreadBudgetScheduler) firstClassLocked(class SchedulerClass) *schedulerWaiter {
	for _, w := range s.queue {
		if w.lease.class == class {
			return w
		}
	}
	return nil
}

func (s *ThreadBudgetScheduler) fairBackgroundLocked() *schedulerWaiter {
	if s.paused {
		return nil
	}
	if s.heavyOwner != nil {
		return s.heavyOwner.waiting
	}
	if s.heavyBurst > 0 {
		if w := s.firstClassLocked(BackgroundText); w != nil {
			return w
		}
	}
	if w := s.firstClassLocked(HeavyMedia); w != nil {
		// An active text batch must acknowledge a checkpoint before model load.
		// It cannot make foreground work wait for an unsafe forced preemption.
		for l := range s.active {
			if l.class == BackgroundText {
				return nil
			}
		}
		return w
	}
	return s.firstClassLocked(BackgroundText)
}

// dispatchLocked admits new jobs FIFO within each class. A large head request
// cannot be overtaken by a smaller new job. A yielded heavy owner is a continuation
// of an admitted job, so it resumes before any different heavy model can load.
// Backfill pauses while a heavy lifecycle is held. Between released heavy
// lifecycles, a waiting text batch gets one admission before another model load.
// Foreground work can share the remaining CPU budget, subject to bounded bursts.
func (s *ThreadBudgetScheduler) dispatchLocked() {
	if s.closed {
		return
	}
	for i := 0; i < len(s.queue); {
		w := s.queue[i]
		if err := w.lease.ctx.Err(); err != nil {
			s.removeWaiterLocked(w, err)
			continue
		}
		i++
	}
	for {
		var chosen *schedulerWaiter
		if s.foregroundBurst >= maxForegroundBurst {
			chosen = s.fairBackgroundLocked()
		}
		for _, class := range []SchedulerClass{ForegroundQuery, HeavyMedia, BackgroundText} {
			if chosen != nil {
				break
			}
			if s.paused && class != ForegroundQuery {
				continue
			}
			if class == HeavyMedia && s.heavyOwner == nil && s.heavyBurst > 0 {
				if w := s.firstClassLocked(BackgroundText); w != nil {
					chosen = w
					break
				}
			}
			if class == HeavyMedia && s.heavyOwner != nil && s.heavyOwner.waiting != nil {
				chosen = s.heavyOwner.waiting
				break
			}
			for _, w := range s.queue {
				if w.lease.class == class {
					chosen = w
					break
				}
			}
			if chosen != nil {
				break
			}
		}
		if chosen == nil {
			return
		}
		l := chosen.lease
		backfillActive := false
		for active := range s.active {
			if active.class == BackgroundText {
				backfillActive = true
				break
			}
		}
		if l.threads+s.threads > s.maxThreads || (l.class == HeavyMedia && ((s.heavyOwner != nil && s.heavyOwner != l) || backfillActive)) || (l.class == BackgroundText && s.heavyOwner != nil) {
			return
		}
		for i, w := range s.queue {
			if w == chosen {
				s.dropWaiterLocked(i)
				break
			}
		}
		l.waiting = nil
		l.admitted = true
		s.active[l] = struct{}{}
		s.threads += l.threads
		if l.class == HeavyMedia {
			s.heavyOwner = l
			s.heavyBurst = 1
		} else if l.class == BackgroundText {
			s.heavyBurst = 0
		}
		if l.class == ForegroundQuery {
			if s.foregroundBurst < maxForegroundBurst {
				s.foregroundBurst++
			}
		} else {
			s.foregroundBurst = 0
		}
		chosen.ready <- nil
	}
}
func (l *SchedulerLease) Release() {
	if l == nil || l.leaseState == nil || l.self != l {
		return
	}
	l.leaseState.release()
}

func (l *leaseState) release() {
	s := l.scheduler
	s.mu.Lock()
	defer s.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	l.cancel()
	s.removeActiveLocked(l)
	if s.heavyOwner == l {
		s.heavyOwner = nil
	}
	if l.waiting != nil {
		s.removeWaiterLocked(l.waiting, ErrSchedulerLeaseReleased)
	}
	s.dispatchLocked()
}
func (l *SchedulerLease) Checkpoint() error {
	if l == nil || l.leaseState == nil || l.self != l {
		return ErrSchedulerLeaseReleased
	}
	return l.leaseState.checkpoint()
}

func (l *leaseState) checkpoint() error {
	s := l.scheduler
	s.mu.Lock()
	if l.released {
		s.mu.Unlock()
		return ErrSchedulerLeaseReleased
	}
	if s.closed {
		s.mu.Unlock()
		return ErrSchedulerClosed
	}
	if err := l.ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	if !l.admitted {
		s.mu.Unlock()
		return ErrSchedulerLeaseReleased
	}
	s.removeActiveLocked(l)
	w := s.enqueueLocked(l)
	s.dispatchLocked()
	s.mu.Unlock()
	if err := l.await(w); err != nil {
		// CPU work was acknowledged stopped before yielding. Remove only the
		// pending CPU readmission; keep any concurrently granted CPU admission
		// and the heavy lifecycle guard until the caller finishes cleanup.
		s.mu.Lock()
		if l.waiting == w {
			s.removeWaiterLocked(w, err)
		}
		s.dispatchLocked()
		s.mu.Unlock()
		return err
	}
	return nil
}

// Run treats a normal callback return as acknowledgement that its worker and
// temporary resources have stopped. A panic requests cancellation and returns a
// fixed error without releasing admission: the callback owner must retain the
// supplied lease and acknowledge cleanup with Release. Native supervisors should
// use Acquire directly so their actor retains explicit lifecycle ownership.
func (s *ThreadBudgetScheduler) Run(ctx context.Context, class SchedulerClass, threads int, work func(context.Context, *SchedulerLease) error) (err error) {
	if work == nil {
		return ErrSchedulerWorkerFailure
	}
	l, err := s.Acquire(ctx, class, threads)
	if err != nil {
		return err
	}
	state := l.leaseState
	acknowledged := false
	defer func() {
		if acknowledged {
			state.release()
		} else {
			state.cancel()
			err = ErrSchedulerWorkerFailure
		}
	}()
	defer func() { _ = recover() }()
	err = work(state.ctx, l)
	acknowledged = true
	if err != nil {
		return err
	}
	return state.ctx.Err()
}

// Close rejects admissions and signals running workers to stop. Reservations
// remain held until actual cleanup/Release; Close cannot stop native code itself.
func (s *ThreadBudgetScheduler) Close() {
	if s == nil || s.schedulerState == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for l := range s.active {
		l.cancel()
	}
	if s.heavyOwner != nil {
		s.heavyOwner.cancel()
	}
	for len(s.queue) > 0 {
		w := s.queue[0]
		w.lease.cancel()
		s.removeWaiterLocked(w, ErrSchedulerClosed)
	}
}
