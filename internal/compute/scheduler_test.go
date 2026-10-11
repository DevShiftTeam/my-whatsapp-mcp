package compute

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func schedulerForTest(t *testing.T) *ThreadBudgetScheduler {
	t.Helper()
	s, err := NewThreadBudgetScheduler(2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
func awaitSchedulerQueue(t *testing.T, s *ThreadBudgetScheduler, count int) {
	t.Helper()
	deadline := time.After(time.Second)
	for s.Snapshot().Queued != count {
		select {
		case <-deadline:
			t.Fatalf("queue did not reach %d: %+v", count, s.Snapshot())
		default:
			time.Sleep(time.Millisecond)
		}
	}
}
func awaitLease(t *testing.T, ch <-chan *SchedulerLease) *SchedulerLease {
	t.Helper()
	select {
	case l := <-ch:
		if l == nil {
			t.Fatal("nil lease")
		}
		return l
	case <-time.After(time.Second):
		t.Fatal("lease timed out")
		return nil
	}
}

func TestSchedulerThreadBudgetAndOneHeavy(t *testing.T) {
	s := schedulerForTest(t)
	if _, err := s.Acquire(context.Background(), HeavyMedia, 3); !errors.Is(err, ErrSchedulerBudget) {
		t.Fatalf("oversized: %v", err)
	}
	heavy, err := s.Acquire(context.Background(), HeavyMedia, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := make(chan *SchedulerLease, 1)
	go func() { l, _ := s.Acquire(ctx, HeavyMedia, 1); second <- l }()
	awaitSchedulerQueue(t, s, 1)
	fg, err := s.Acquire(context.Background(), ForegroundQuery, 1)
	if err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if snap.Threads != 2 || snap.Heavy != 1 || snap.Active != 2 {
		t.Fatalf("budget: %+v", snap)
	}
	select {
	case <-second:
		t.Fatal("second heavy admitted")
	default:
	}
	fg.Release()
	heavy.Release()
	next := awaitLease(t, second)
	next.Release()
	next.Release()
	if snap := s.Snapshot(); snap.Threads != 0 || snap.Active != 0 || snap.Queued != 0 {
		t.Fatalf("leak: %+v", snap)
	}
}

func TestSchedulerForegroundAtCheckpointAndBackfillPause(t *testing.T) {
	s := schedulerForTest(t)
	heavy, _ := s.Acquire(context.Background(), HeavyMedia, 2)
	bg := make(chan *SchedulerLease, 1)
	go func() { l, _ := s.Acquire(context.Background(), BackgroundText, 1); bg <- l }()
	awaitSchedulerQueue(t, s, 1)
	fg := make(chan *SchedulerLease, 1)
	go func() { l, _ := s.Acquire(context.Background(), ForegroundQuery, 2); fg <- l }()
	awaitSchedulerQueue(t, s, 2)
	yielded := make(chan error, 1)
	go func() { yielded <- heavy.Checkpoint() }()
	query := awaitLease(t, fg)
	select {
	case <-bg:
		t.Fatal("backfill passed foreground/heavy")
	default:
	}
	select {
	case <-yielded:
		t.Fatal("heavy resumed before query release")
	default:
	}
	query.Release()
	select {
	case err := <-yielded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("heavy failed to resume")
	}
	select {
	case <-bg:
		t.Fatal("backfill ran during heavy work")
	default:
	}
	heavy.Release()
	awaitLease(t, bg).Release()
}

func TestSchedulerFIFOWithinClass(t *testing.T) {
	s := schedulerForTest(t)
	blocker, _ := s.Acquire(context.Background(), ForegroundQuery, 2)
	one, two := make(chan *SchedulerLease, 1), make(chan *SchedulerLease, 1)
	go func() { l, _ := s.Acquire(context.Background(), ForegroundQuery, 2); one <- l }()
	awaitSchedulerQueue(t, s, 1)
	go func() { l, _ := s.Acquire(context.Background(), ForegroundQuery, 1); two <- l }()
	awaitSchedulerQueue(t, s, 2)
	blocker.Release()
	first := awaitLease(t, one)
	select {
	case <-two:
		t.Fatal("FIFO overtaken")
	default:
	}
	first.Release()
	awaitLease(t, two).Release()
}

func TestSchedulerCanceledPendingActiveAndWorkerFailure(t *testing.T) {
	s := schedulerForTest(t)
	blocker, _ := s.Acquire(context.Background(), HeavyMedia, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Acquire(ctx, ForegroundQuery, 1); done <- err }()
	awaitSchedulerQueue(t, s, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s.Snapshot().Queued != 0 {
		t.Fatal("pending cancellation leaked")
	}
	blocker.Release()
	ctx, cancel = context.WithCancel(context.Background())
	started := make(chan struct{})
	ack := make(chan struct{})
	go func() {
		done <- s.Run(ctx, HeavyMedia, 2, func(ctx context.Context, l *SchedulerLease) error {
			close(started)
			<-ctx.Done()
			<-ack
			return ctx.Err()
		})
	}()
	<-started
	cancel()
	if s.Snapshot().Threads != 2 {
		t.Fatal("cancellation falsely preempted non-yielding work")
	}
	close(ack)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s.Snapshot().Threads != 0 {
		t.Fatal("active cancellation leaked")
	}
	err := s.Run(context.Background(), HeavyMedia, 2, func(context.Context, *SchedulerLease) error { return ErrSchedulerWorkerFailure })
	if !errors.Is(err, ErrSchedulerWorkerFailure) || s.Snapshot().Threads != 0 {
		t.Fatalf("returned failure cleanup: %v %+v", err, s.Snapshot())
	}
	already, c := context.WithCancel(context.Background())
	c()
	if _, err := s.Acquire(already, ForegroundQuery, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("stale context: %v", err)
	}
}

func TestSchedulerPanicRetainsAdmissionUntilCleanupAcknowledgement(t *testing.T) {
	s := schedulerForTest(t)
	var owned *SchedulerLease
	err := s.Run(context.Background(), HeavyMedia, 2, func(_ context.Context, l *SchedulerLease) error { owned = l; panic("private failure") })
	if !errors.Is(err, ErrSchedulerWorkerFailure) {
		t.Fatal(err)
	}
	if owned.Context().Err() != context.Canceled {
		t.Fatal("panic failed to request stop")
	}
	second := make(chan *SchedulerLease, 1)
	go func() { l, _ := s.Acquire(context.Background(), HeavyMedia, 1); second <- l }()
	awaitSchedulerQueue(t, s, 1)
	if snap := s.Snapshot(); snap.Heavy != 1 || snap.Threads != 2 {
		t.Fatalf("panic released unverified native cleanup: %+v", snap)
	}
	select {
	case <-second:
		t.Fatal("next model admitted before panic cleanup")
	default:
	}
	owned.Release() // owner acknowledges stopped worker and completed cleanup
	awaitLease(t, second).Release()
}

func TestSchedulerGoexitRetainsUnacknowledgedCleanup(t *testing.T) {
	s := schedulerForTest(t)
	owned := make(chan *SchedulerLease, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = s.Run(context.Background(), HeavyMedia, 2, func(_ context.Context, l *SchedulerLease) error { owned <- l; runtime.Goexit(); return nil })
	}()
	l := awaitLease(t, owned)
	<-finished
	if snap := s.Snapshot(); snap.Heavy != 1 || snap.Threads != 2 {
		t.Fatalf("Goexit acknowledged cleanup: %+v", snap)
	}
	if l.Context().Err() != context.Canceled {
		t.Fatal("Goexit failed to request stop")
	}
	l.Release()
}

func TestSchedulerNilPanicReturnsFixedFailureAndRetainsGuard(t *testing.T) {
	s := schedulerForTest(t)
	var owned *SchedulerLease
	err := s.Run(context.Background(), HeavyMedia, 2, func(_ context.Context, l *SchedulerLease) error { owned = l; panic(nil) })
	if !errors.Is(err, ErrSchedulerWorkerFailure) {
		t.Fatalf("nil panic falsely succeeded: %v", err)
	}
	if snap := s.Snapshot(); snap.Heavy != 1 || snap.Threads != 2 {
		t.Fatalf("nil panic released guard: %+v", snap)
	}
	owned.Release()
}

func TestSchedulerLeaseCopyCannotReleaseOrYieldOriginalAdmission(t *testing.T) {
	s := schedulerForTest(t)
	heavy, _ := s.Acquire(context.Background(), HeavyMedia, 2)
	copied := *heavy
	copied.Release()
	if snap := s.Snapshot(); snap.Threads != 2 || snap.Heavy != 1 || snap.Active != 1 {
		t.Fatalf("copied lease released live owner: %+v", snap)
	}
	if err := copied.Checkpoint(); !errors.Is(err, ErrSchedulerLeaseReleased) {
		t.Fatalf("copied lease yielded live owner: %v", err)
	}
	heavy.Release()
	if snap := s.Snapshot(); snap.Threads != 0 || snap.Heavy != 0 || snap.Active != 0 {
		t.Fatalf("original release corrupted accounting: %+v", snap)
	}
}

func TestSchedulerCopyCannotCreateAnotherAdmissionAuthority(t *testing.T) {
	s := schedulerForTest(t)
	copied := *s
	heavy, _ := s.Acquire(context.Background(), HeavyMedia, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if l, err := copied.Acquire(ctx, ForegroundQuery, 1); err == nil {
		l.Release()
		t.Fatal("scheduler copy exceeded the shared two-thread admission")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if snap := copied.Snapshot(); snap.Threads != 2 || snap.Heavy != 1 {
		t.Fatalf("copy detached authority: %+v", snap)
	}
	heavy.Release()
}

func TestSchedulerLeaseRetainsAcquiredAuthorityAfterHandleReplacement(t *testing.T) {
	s := schedulerForTest(t)
	heavy, _ := s.Acquire(context.Background(), HeavyMedia, 2)
	original := *s
	t.Cleanup(original.Close)
	other := schedulerForTest(t)
	query, _ := other.Acquire(context.Background(), ForegroundQuery, 2)
	*s = *other // replacement of the public handle must not rebind an old lease
	heavy.Release()
	if snap := original.Snapshot(); snap.Threads != 0 || snap.Heavy != 0 {
		t.Fatalf("old authority leaked: %+v", snap)
	}
	if snap := other.Snapshot(); snap.Threads != 2 || snap.Active != 1 {
		t.Fatalf("old lease mutated replacement authority: %+v", snap)
	}
	query.Release()
}

func TestSchedulerSavedLeaseHandleCannotRewindReleasedAccounting(t *testing.T) {
	s := schedulerForTest(t)
	l, _ := s.Acquire(context.Background(), HeavyMedia, 2)
	saved := *l
	l.Release()
	*l = saved
	l.Release()
	if snap := s.Snapshot(); snap.Threads != 0 || snap.Heavy != 0 || snap.Active != 0 {
		t.Fatalf("saved lease rewound release accounting: %+v", snap)
	}
}

func TestSchedulerCloseRetainsAuthorityAfterPublicLeaseOverwrite(t *testing.T) {
	s := schedulerForTest(t)
	l, _ := s.Acquire(context.Background(), HeavyMedia, 2)
	saved := *l
	*l = SchedulerLease{}
	s.Close() // private scheduler ownership must not dereference the public handle
	if saved.Context().Err() != context.Canceled {
		t.Fatal("overwritten handle prevented shutdown cancellation")
	}
	if snap := s.Snapshot(); snap.Threads != 2 || snap.Heavy != 1 {
		t.Fatalf("overwrite released live ownership: %+v", snap)
	}
	*l = saved
	l.Release()
}

func TestSchedulerHeavySequenceDoesNotStarveText(t *testing.T) {
	s := schedulerForTest(t)
	blocker, _ := s.Acquire(context.Background(), HeavyMedia, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admitted := make(chan *SchedulerLease, 16)
	go func() {
		l, _ := s.Acquire(ctx, BackgroundText, 2)
		if l != nil {
			admitted <- l
		}
	}()
	awaitSchedulerQueue(t, s, 1)
	for i := 0; i < 10; i++ {
		go func() {
			l, _ := s.Acquire(ctx, HeavyMedia, 2)
			if l != nil {
				admitted <- l
			}
		}()
		awaitSchedulerQueue(t, s, i+2)
	}
	blocker.Release()
	backgroundAt := -1
	for i := 0; i < 11; i++ {
		l := awaitLease(t, admitted)
		if l.class == BackgroundText {
			backgroundAt = i
		}
		l.Release()
	}
	if backgroundAt < 0 || backgroundAt > 1 {
		t.Fatalf("text starved behind %d heavy models", backgroundAt)
	}
}

func TestSchedulerCloseCancelsWorkAndWaiters(t *testing.T) {
	s := schedulerForTest(t)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Run(context.Background(), HeavyMedia, 2, func(ctx context.Context, _ *SchedulerLease) error { close(started); <-ctx.Done(); return ctx.Err() })
	}()
	<-started
	pending := make(chan error, 1)
	go func() { _, err := s.Acquire(context.Background(), ForegroundQuery, 1); pending <- err }()
	awaitSchedulerQueue(t, s, 1)
	s.Close()
	s.Close()
	if err := <-pending; !errors.Is(err, ErrSchedulerClosed) {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.Acquire(context.Background(), ForegroundQuery, 1); !errors.Is(err, ErrSchedulerClosed) {
		t.Fatal(err)
	}
	if s.Snapshot().Threads != 0 {
		t.Fatal("close cleanup leaked")
	}
}

func TestSchedulerHeavyWaitsForBackfillCheckpoint(t *testing.T) {
	s := schedulerForTest(t)
	background, err := s.Acquire(context.Background(), BackgroundText, 1)
	if err != nil {
		t.Fatal(err)
	}
	heavy := make(chan *SchedulerLease, 1)
	go func() { l, _ := s.Acquire(context.Background(), HeavyMedia, 1); heavy <- l }()
	awaitSchedulerQueue(t, s, 1)
	select {
	case <-heavy:
		t.Fatal("heavy overlapped existing backfill")
	default:
	}
	yielded := make(chan error, 1)
	go func() { yielded <- background.Checkpoint() }()
	media := awaitLease(t, heavy)
	select {
	case <-yielded:
		t.Fatal("backfill resumed during heavy")
	default:
	}
	media.Release()
	select {
	case err := <-yielded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("backfill did not resume")
	}
	background.Release()
}

func TestSchedulerCanceledBeforeCheckpointRetainsCleanupReservation(t *testing.T) {
	s := schedulerForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	checkpointReturned := make(chan error, 1)
	cleanup := make(chan struct{})
	done := make(chan error, 1)
	defer close(cleanup)
	go func() {
		done <- s.Run(ctx, HeavyMedia, 2, func(ctx context.Context, l *SchedulerLease) error {
			close(started)
			<-ctx.Done()
			err := l.Checkpoint()
			checkpointReturned <- err
			<-cleanup
			return err
		})
	}()
	<-started
	second := make(chan *SchedulerLease, 1)
	go func() { l, _ := s.Acquire(context.Background(), HeavyMedia, 1); second <- l }()
	awaitSchedulerQueue(t, s, 1)
	cancel()
	if err := <-checkpointReturned; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if snap.Threads != 2 || snap.Heavy != 1 || snap.Queued != 1 {
		t.Fatalf("canceled checkpoint released live cleanup reservation: %+v", snap)
	}
	select {
	case l := <-second:
		if l != nil {
			l.Release()
		}
		t.Fatal("second heavy admitted while canceled worker cleanup blocked")
	default:
	}
	// A successful explicit acknowledgement must happen before the deferred release.
	cleanup <- struct{}{}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	awaitLease(t, second).Release()
	if snap := s.Snapshot(); snap.Threads != 0 || snap.Heavy != 0 || snap.Queued != 0 {
		t.Fatalf("cleanup leak: %+v", snap)
	}
}

func TestSchedulerHeavyLifecycleHeldAcrossCheckpointAndReadmissionCancellation(t *testing.T) {
	s := schedulerForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	yield := make(chan struct{})
	checkpointReturned := make(chan error, 1)
	cleanup := make(chan struct{})
	done := make(chan error, 1)
	defer close(cleanup)
	go func() {
		done <- s.Run(ctx, HeavyMedia, 2, func(_ context.Context, l *SchedulerLease) error {
			close(started)
			<-yield
			err := l.Checkpoint()
			checkpointReturned <- err
			<-cleanup
			return err
		})
	}()
	<-started
	// Queue the next heavy before yielding: it must not load while the old model
	// stays alive, and it must not prevent the old lease from resuming later.
	second := make(chan *SchedulerLease, 1)
	go func() { l, _ := s.Acquire(context.Background(), HeavyMedia, 1); second <- l }()
	awaitSchedulerQueue(t, s, 1)
	foreground := make(chan *SchedulerLease, 1)
	go func() { l, _ := s.Acquire(context.Background(), ForegroundQuery, 2); foreground <- l }()
	awaitSchedulerQueue(t, s, 2)
	close(yield)
	query := awaitLease(t, foreground)
	if snap := s.Snapshot(); snap.Threads != 2 || snap.Heavy != 1 {
		t.Fatalf("yield dropped heavy lifecycle guard: %+v", snap)
	}
	cancel()
	if err := <-checkpointReturned; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	query.Release()
	select {
	case l := <-second:
		if l != nil {
			l.Release()
		}
		t.Fatal("readmission cancellation admitted another heavy before cleanup")
	default:
	}
	if snap := s.Snapshot(); snap.Heavy != 1 || snap.Threads != 0 || snap.Queued != 1 {
		t.Fatalf("cleanup lifecycle lost: %+v", snap)
	}
	cleanup <- struct{}{}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	awaitLease(t, second).Release()
	if snap := s.Snapshot(); snap.Threads != 0 || snap.Heavy != 0 || snap.Queued != 0 {
		t.Fatalf("cleanup leak: %+v", snap)
	}
}

func TestSchedulerHealthyCheckpointResumesSameHeavyBeforeNextModel(t *testing.T) {
	s := schedulerForTest(t)
	heavy, _ := s.Acquire(context.Background(), HeavyMedia, 2)
	second := make(chan *SchedulerLease, 1)
	go func() { l, _ := s.Acquire(context.Background(), HeavyMedia, 1); second <- l }()
	awaitSchedulerQueue(t, s, 1)
	foreground := make(chan *SchedulerLease, 1)
	go func() { l, _ := s.Acquire(context.Background(), ForegroundQuery, 2); foreground <- l }()
	awaitSchedulerQueue(t, s, 2)
	resumed := make(chan error, 1)
	go func() { resumed <- heavy.Checkpoint() }()
	query := awaitLease(t, foreground)
	if snap := s.Snapshot(); snap.Heavy != 1 {
		t.Fatalf("resident heavy model lost guard: %+v", snap)
	}
	query.Release()
	select {
	case err := <-resumed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("existing heavy continuation blocked by next model")
	}
	select {
	case l := <-second:
		if l != nil {
			l.Release()
		}
		t.Fatal("next heavy loaded before existing model stopped")
	default:
	}
	heavy.Release()
	awaitLease(t, second).Release()
}

// Removing bounded foreground bursts would let new queries continually overtake
// the background item, even though every query itself releases promptly.
func TestSchedulerForegroundPriorityDoesNotStarveBackground(t *testing.T) {
	s := schedulerForTest(t)
	blocker, _ := s.Acquire(context.Background(), ForegroundQuery, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admitted := make(chan *SchedulerLease, 16)
	go func() {
		l, _ := s.Acquire(ctx, BackgroundText, 2)
		if l != nil {
			admitted <- l
		}
	}()
	awaitSchedulerQueue(t, s, 1)
	for i := 0; i < 12; i++ {
		go func() {
			l, _ := s.Acquire(ctx, ForegroundQuery, 2)
			if l != nil {
				admitted <- l
			}
		}()
		awaitSchedulerQueue(t, s, i+2)
	}
	blocker.Release()
	backgroundAt := -1
	for i := 0; i < 13; i++ {
		l := awaitLease(t, admitted)
		if i == 0 && l.class != ForegroundQuery {
			t.Fatal("foreground lost initial priority")
		}
		if l.class == BackgroundText {
			backgroundAt = i
		}
		l.Release()
	}
	if backgroundAt < 1 || backgroundAt >= 8 {
		t.Fatalf("background starved behind %d admissions", backgroundAt)
	}
}

func TestSchedulerForegroundBurstAllowsNewHeavy(t *testing.T) {
	s := schedulerForTest(t)
	blocker, _ := s.Acquire(context.Background(), ForegroundQuery, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admitted := make(chan *SchedulerLease, 16)
	go func() {
		l, _ := s.Acquire(ctx, HeavyMedia, 2)
		if l != nil {
			admitted <- l
		}
	}()
	awaitSchedulerQueue(t, s, 1)
	for i := 0; i < 12; i++ {
		go func() {
			l, _ := s.Acquire(ctx, ForegroundQuery, 2)
			if l != nil {
				admitted <- l
			}
		}()
		awaitSchedulerQueue(t, s, i+2)
	}
	blocker.Release()
	heavyAt := -1
	for i := 0; i < 13; i++ {
		l := awaitLease(t, admitted)
		if i == 0 && l.class != ForegroundQuery {
			t.Fatal("foreground lost initial priority")
		}
		if l.class == HeavyMedia {
			heavyAt = i
		}
		l.Release()
	}
	if heavyAt < 1 || heavyAt >= 8 {
		t.Fatalf("model load starved behind %d queries", heavyAt)
	}
}

func TestSchedulerForegroundBurstAllowsHeavyContinuation(t *testing.T) {
	s := schedulerForTest(t)
	heavy, _ := s.Acquire(context.Background(), HeavyMedia, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queries := make(chan *SchedulerLease, 16)
	for i := 0; i < 12; i++ {
		go func() {
			l, _ := s.Acquire(ctx, ForegroundQuery, 2)
			if l != nil {
				queries <- l
			}
		}()
		awaitSchedulerQueue(t, s, i+1)
	}
	resumed := make(chan error, 1)
	go func() { resumed <- heavy.Checkpoint() }()
	beforeResume := 0
	for {
		select {
		case err := <-resumed:
			if err != nil {
				t.Fatal(err)
			}
			if beforeResume < 1 || beforeResume > 8 {
				t.Fatalf("heavy continuation starved behind %d queries", beforeResume)
			}
			heavy.Release()
			for i := beforeResume; i < 12; i++ {
				awaitLease(t, queries).Release()
			}
			return
		case l := <-queries:
			beforeResume++
			l.Release()
		case <-time.After(time.Second):
			t.Fatal("heavy continuation deadlocked")
		}
	}
}

func TestSchedulerFairTurnReservesRequiredThreadCapacity(t *testing.T) {
	s := schedulerForTest(t)
	sibling, _ := s.Acquire(context.Background(), ForegroundQuery, 1)
	current, _ := s.Acquire(context.Background(), ForegroundQuery, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	background := make(chan *SchedulerLease, 1)
	go func() { l, _ := s.Acquire(ctx, BackgroundText, 2); background <- l }()
	awaitSchedulerQueue(t, s, 1)
	queries := make(chan *SchedulerLease, 16)
	for i := 0; i < 10; i++ {
		go func() {
			l, _ := s.Acquire(ctx, ForegroundQuery, 1)
			if l != nil {
				queries <- l
			}
		}()
		awaitSchedulerQueue(t, s, i+2)
	}
	for i := 0; i < 6; i++ {
		current.Release()
		current = awaitLease(t, queries)
	}
	current.Release()
	if snap := s.Snapshot(); snap.Threads != 1 {
		t.Fatalf("new query consumed capacity reserved for fair turn: %+v", snap)
	}
	sibling.Release()
	awaitLease(t, background).Release()
	for i := 0; i < 4; i++ {
		awaitLease(t, queries).Release()
	}
}

func TestSchedulerPausedIndexingLeavesForegroundUsable(t *testing.T) {
	s := schedulerForTest(t)
	s.SetPaused(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	background := make(chan *SchedulerLease, 1)
	go func() { l, _ := s.Acquire(ctx, BackgroundText, 2); background <- l }()
	awaitSchedulerQueue(t, s, 1)
	query, err := s.Acquire(context.Background(), ForegroundQuery, 2)
	if err != nil {
		t.Fatal(err)
	}
	query.Release()
	if snap := s.Snapshot(); !snap.Paused || snap.Threads != 0 || snap.Queued != 1 {
		t.Fatalf("pause: %+v", snap)
	}
	s.SetPaused(false)
	awaitLease(t, background).Release()
}

func TestSchedulerPauseAtHeavyCheckpointRetainsLifecycle(t *testing.T) {
	s := schedulerForTest(t)
	heavy, _ := s.Acquire(context.Background(), HeavyMedia, 2)
	s.SetPaused(true)
	checkpoint := make(chan error, 1)
	go func() { checkpoint <- heavy.Checkpoint() }()
	awaitSchedulerQueue(t, s, 1)
	query, err := s.Acquire(context.Background(), ForegroundQuery, 2)
	if err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); snap.Heavy != 1 || snap.Threads != 2 {
		t.Fatalf("paused heavy guard: %+v", snap)
	}
	select {
	case <-checkpoint:
		t.Fatal("paused media resumed")
	default:
	}
	query.Release()
	s.SetPaused(false)
	select {
	case err := <-checkpoint:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("media did not resume")
	}
	heavy.Release()
}
