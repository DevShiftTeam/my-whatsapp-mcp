//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func privateTestDir(t *testing.T) string {
	t.Helper()
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return dir
}
func TestControlDeadlinePersistsDenialAndRefusesObservationCleanup(t *testing.T) {
	dir := privateTestDir(t)
	s, e := Open(context.Background(), dir, Options{})
	if e != nil {
		t.Fatal(e)
	}
	p := testPolicy(t, s)
	apply(t, s, event("m", "before delete", 1, EventOriginal), p.Generation)
	id, e := s.BeginObservation(context.Background(), "account")
	if e != nil {
		t.Fatal(e)
	}
	conn, e := s.writeDB.Conn(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, e = s.ApplySourceEvent(ctx, event("m", "", 2, EventDelete), p.Generation)
	conn.Close()
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("control deadline %v", e)
	}
	if _, e = s.readSafety("account"); !errors.Is(e, ErrReconciliationRequired) {
		t.Fatal(e)
	}
	if e = s.EndObservation(context.Background(), id); !errors.Is(e, ErrReconciliationRequired) {
		t.Fatalf("cleared unsafe observation: %v", e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(context.Background(), dir, Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.readSafety("account"); !errors.Is(e, ErrReconciliationRequired) {
		t.Fatalf("reopen safety %v", e)
	}
	for _, name := range []string{"observation.marker", "unsafe.marker"} {
		if _, e = os.Stat(filepath.Join(dir, name)); e != nil {
			t.Fatalf("marker removed: %s %v", name, e)
		}
	}
}
func TestCanceledAcceptedBodyCannotCleanObservation(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	id, e := s.BeginObservation(context.Background(), "account")
	if e != nil {
		t.Fatal(e)
	}
	conn, e := s.writeDB.Conn(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	baseline := s.writeDB.Stats().WaitCount
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.ApplySourceEvent(ctx, event("lost", "accepted source", 1, EventOriginal), p.Generation)
		done <- err
	}()
	waitArchiveCondition(t, func() bool { return s.writeDB.Stats().WaitCount > baseline })
	cancel()
	if e = <-done; !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	conn.Close()
	if e = s.Drain(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = s.EndObservation(context.Background(), id); !errors.Is(e, ErrReconciliationRequired) {
		t.Fatalf("lost accepted body cleared marker: %v", e)
	}
	if _, e = s.readSafety("account"); !errors.Is(e, ErrReconciliationRequired) {
		t.Fatal(e)
	}
}
func TestBodyAdmissionReservesCountAndBytesIncludingInFlight(t *testing.T) {
	s, e := Open(context.Background(), privateTestDir(t), Options{BodyQueueCount: 2, BodyQueueBytes: 2048})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	p := testPolicy(t, s)
	conn, e := s.writeDB.Conn(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	baseline := s.writeDB.Stats().WaitCount
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := s.ApplySourceEvent(context.Background(), event("a", "one", 1, EventOriginal), p.Generation)
		first <- err
	}()
	waitArchiveCondition(t, func() bool { return s.writeDB.Stats().WaitCount > baseline })
	go func() {
		_, err := s.ApplySourceEvent(context.Background(), event("b", "two", 1, EventOriginal), p.Generation)
		second <- err
	}()
	waitArchiveCondition(t, func() bool { return len(s.bodies) == 1 })
	if s.queuedCount.Load() != 2 || s.queuedBytes.Load() > 2048 {
		t.Fatalf("count=%d bytes=%d", s.queuedCount.Load(), s.queuedBytes.Load())
	}
	if _, e = s.ApplySourceEvent(context.Background(), event("overflow", "three", 1, EventOriginal), p.Generation); !errors.Is(e, ErrQueueLimitExceeded) {
		t.Fatalf("unbounded admission %v", e)
	}
	conn.Close()
	<-first
	<-second
	if _, e = s.readSafety("account"); !errors.Is(e, ErrReconciliationRequired) {
		t.Fatalf("overflow not denied %v", e)
	}
}

func TestExpiryAndRetentionSweepAtomicallyRemovesPlaintextAndVersions(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Millisecond)
	var clock atomic.Int64
	clock.Store(base.UnixMilli())
	s, e := Open(context.Background(), privateTestDir(t), Options{Now: func() time.Time { return time.UnixMilli(clock.Load()).UTC() }})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	p, e := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll, RollingRetention: time.Minute}, 0)
	if e != nil {
		t.Fatal(e)
	}
	expired := event("expires", "remove source plaintext", 1, EventOriginal)
	expired.OriginalSentAt = base
	expired.ExpiresAt = base.Add(30 * time.Second)
	r1 := apply(t, s, expired, p.Generation)
	retention := event("retention", "remove retention plaintext", 1, EventOriginal)
	retention.OriginalSentAt = base
	r2 := apply(t, s, retention, p.Generation)
	clock.Store(base.Add(2 * time.Minute).UnixMilli())
	if e = s.SweepExpired(context.Background()); e != nil {
		t.Fatal(e)
	}
	var body, normalized, state string
	var revision uint64
	for _, r := range []ApplyResult{r1, r2} {
		if e = s.readDB.QueryRow("SELECT state,body,normalized_body,local_revision FROM messages WHERE id=?", r.MessageID).Scan(&state, &body, &normalized, &revision); e != nil {
			t.Fatal(e)
		}
		if state == "active" || body != "" || normalized != "" || revision <= r.Version.Revision {
			t.Fatalf("uncleared state=%s body=%q normalized=%q rev=%d", state, body, normalized, revision)
		}
	}
	var count int
	s.readDB.QueryRow("SELECT count(*) FROM message_fts").Scan(&count)
	if count != 0 {
		t.Fatal("expired FTS retained")
	}
	s.readDB.QueryRow("SELECT count(*) FROM dirty_windows WHERE generation=? AND archive_revision>2", p.Generation).Scan(&count)
	if count != 1 {
		t.Fatal("expiry did not durably invalidate source window")
	}
	expired.ExpiresAt = base.Add(time.Hour)
	expired.SourceRevision = 99
	if r := apply(t, s, expired, p.Generation); r.Outcome == OutcomeApplied {
		t.Fatal("expired source resurrected")
	}
}

func TestSecondArchiveHandleRefusedUntilLifetimeLockRelease(t *testing.T) {
	dir := privateTestDir(t)
	first, e := Open(context.Background(), dir, Options{})
	if e != nil {
		t.Fatal(e)
	}
	second, e := Open(context.Background(), dir, Options{})
	if second != nil {
		second.Close()
	}
	if !errors.Is(e, ErrInstanceAlreadyRunning) {
		first.Close()
		t.Fatalf("second writer accepted: %v", e)
	}
	if e = first.Close(); e != nil {
		t.Fatal(e)
	}
	second, e = Open(context.Background(), dir, Options{})
	if e != nil {
		t.Fatal(e)
	}
	second.Close()
}

func TestPreAccountUnsafeMarkerDeniesImmediatelyAndAfterRestart(t *testing.T) {
	dir := privateTestDir(t)
	s, e := Open(context.Background(), dir, Options{})
	if e != nil {
		t.Fatal(e)
	}
	id, e := s.BeginObservation(context.Background(), "")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.MarkUnsafe(context.Background(), "", "control_admission_failed"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.readSafety("future-account"); !errors.Is(e, ErrReconciliationRequired) {
		t.Fatal(e)
	}
	if e = s.EndObservation(context.Background(), id); !errors.Is(e, ErrReconciliationRequired) {
		t.Fatal("cleared pre-account unsafe")
	}
	s.Close()
	s, e = Open(context.Background(), dir, Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, e = s.readSafety("future-account"); !errors.Is(e, ErrReconciliationRequired) {
		t.Fatalf("unsafe reopened %v", e)
	}
}

func TestLifetimeLockSurvivesRejectedOpenAndDescriptorChecks(t *testing.T) {
	dir := privateTestDir(t)
	s, e := Open(context.Background(), dir, Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if second, e := Open(context.Background(), dir, Options{}); e != ErrInstanceAlreadyRunning {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second handle %v", e)
	}
	f, e := os.Open(filepath.Join(dir, "archive.lock"))
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestArchiveLockSubprocessHelper$")
	cmd.Env = append(os.Environ(), "WA_ARCHIVE_LOCK_TEST_DIR="+dir)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("external writer admitted after descriptor close: %v %s", e, out)
	}
}
func TestArchiveLockSubprocessHelper(t *testing.T) {
	dir := os.Getenv("WA_ARCHIVE_LOCK_TEST_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	s, e := Open(context.Background(), dir, Options{})
	if s != nil {
		s.Close()
	}
	if !errors.Is(e, ErrInstanceAlreadyRunning) {
		t.Fatalf("lifetime lock lost: %v", e)
	}
}
