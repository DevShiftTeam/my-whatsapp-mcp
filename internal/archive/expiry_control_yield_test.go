//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

// Model exhaustion at a known SQL work boundary rather than relying on a
// machine's speed. Unlike context.CancelFunc, this retains the deadline class
// seen by the real submit/await/actor path. No private production data is used.
type expiryControlWorkDeadline struct {
	done chan struct{}
	once sync.Once
}

func (c *expiryControlWorkDeadline) Deadline() (time.Time, bool) {
	return time.Now().Add(time.Hour), true
}
func (c *expiryControlWorkDeadline) Done() <-chan struct{} { return c.done }
func (c *expiryControlWorkDeadline) Value(any) any         { return nil }
func (c *expiryControlWorkDeadline) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (c *expiryControlWorkDeadline) expire() { c.once.Do(func() { close(c.done) }) }

func TestExpiryMaintenanceYieldsToQueuedPrivacyControl(t *testing.T) {
	for _, expireBeforeYield := range []bool{false, true} {
		name := "queued_control_must_progress"
		if expireBeforeYield {
			name = "expired_control_must_still_fail_closed"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
			s, policy := readerFixture(t, &now)
			original := readerEvent("expiry-yield-live", "expiry-yield-chat", "privacycontrolreceipt", now.Add(-time.Minute))
			originalID := readerApply(t, s, policy, original)
			var expiredIDs []MessageID
			for _, id := range []string{"expiry-yield-first", "expiry-yield-second"} {
				e := readerEvent(id, "expiry-yield-chat", "expiredcleanupreceipt", now.Add(-time.Hour))
				e.ExpiresAt = now.Add(time.Minute)
				expiredIDs = append(expiredIDs, readerApply(t, s, policy, e))
			}
			scope := readerScope(policy, readerChat(t, s, originalID))
			page, oldGuard, err := s.SearchGuarded(ctx, scope, SearchRequest{Query: "privacycontrolreceipt"})
			if err != nil || len(page.Hits) != 1 {
				t.Fatal("fixture lacks a readable source and an original response guard")
			}
			expiredGuards := make(map[MessageID]ResponseGuard)
			for _, id := range expiredIDs {
				_, guard, guardErr := s.ContextGuarded(ctx, scope, ContextRequest{MessageID: id})
				if guardErr != nil {
					t.Fatal("fixture lacks pre-expiry source guards")
				}
				expiredGuards[id] = guard
			}
			now = now.Add(2 * time.Minute)
			first, resume := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(resume) }) }
			deadline := &expiryControlWorkDeadline{done: make(chan struct{})}
			var cleanupRows atomic.Int64
			var admittedControl command
			conn, err := s.writeDB.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = conn.Raw(func(raw any) error {
				return raw.(*sqlite3.SQLiteConn).RegisterFunc("synthetic_expiry_control_checkpoint", func() int {
					switch cleanupRows.Add(1) {
					case 1:
						close(first)
						<-resume
					case 2:
						// A second cleanup row while a source control is waiting
						// represents consumed control budget, deterministically.
						deadline.expire()
						<-admittedControl.ctx.Done()
					}
					return 0
				}, false)
			})
			if err == nil {
				_, err = conn.ExecContext(ctx, `CREATE TEMP TRIGGER synthetic_expiry_control_checkpoint BEFORE UPDATE OF state ON messages WHEN OLD.state='active' AND NEW.state IN ('expired','retention_expired') BEGIN SELECT synthetic_expiry_control_checkpoint(); END`)
			}
			_ = conn.Close()
			if err != nil {
				t.Fatal(err)
			}
			// Register last: assertion failures must release SQLite before Close.
			t.Cleanup(release)
			expiryDone := make(chan error, 1)
			go func() { expiryDone <- s.SweepExpired(ctx) }()
			select {
			case <-first:
			case <-ctx.Done():
				t.Fatal("real expiry actor did not reach its first cleanup row")
			}
			deleted := original
			deleted.Kind, deleted.Body, deleted.SourceRevision = EventDelete, "", original.SourceRevision+1
			controlDone := make(chan error, 1)
			go func() {
				_, applyErr := s.ApplySourceEvent(deadline, deleted, policy.Generation)
				controlDone <- applyErr
			}()
			// The actor is inside real cleanup SQL. Observe/reinsert the exact
			// production command, proving admission before resuming maintenance.
			select {
			case admittedControl = <-s.controls:
				payload, ok := admittedControl.payload.(eventCommand)
				if !ok || payload.event.Kind != EventDelete || !admittedControl.sourceSensitive {
					t.Fatal("barrier did not capture a real authoritative negative control")
				}
				s.controls <- admittedControl
			case <-ctx.Done():
				t.Fatal("incoming control was not admitted behind maintenance")
			}
			if expireBeforeYield {
				deadline.expire()
				<-admittedControl.ctx.Done()
			}
			release()
			var controlErr error
			select {
			case controlErr = <-controlDone:
			case <-ctx.Done():
				t.Fatal("incoming control failed to settle")
			}
			select {
			case err = <-expiryDone:
				if err != nil {
					t.Fatalf("independent maintenance failed: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("maintenance did not release the writer")
			}
			if err = s.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			if expireBeforeYield {
				if !errors.Is(controlErr, context.DeadlineExceeded) {
					t.Fatalf("lost negative control did not preserve its deadline: %v", controlErr)
				}
				if _, err = s.CheckResponse(ctx, scope, ResponseCheck{Guard: oldGuard}); !errors.Is(err, ErrReconciliationRequired) {
					t.Fatal("an actually lost incoming negative control left buffered content readable")
				}
				if _, err = os.Stat(filepath.Join(s.privateDir, "unsafe.marker")); err != nil {
					t.Fatal("actual control uncertainty did not retain its durable safety marker")
				}
				return
			}
			if controlErr != nil {
				if _, guardErr := s.CheckResponse(ctx, scope, ResponseCheck{Guard: oldGuard}); !errors.Is(guardErr, ErrReconciliationRequired) {
					t.Fatal("starved incoming control did not reproduce the global response fence")
				}
				if _, markerErr := os.Stat(filepath.Join(s.privateDir, "unsafe.marker")); markerErr != nil {
					t.Fatal("starved incoming control did not reproduce the durable source pause")
				}
				t.Fatalf("maintenance consumed the waiting privacy control budget: err=%v cleanup_rows=%d", controlErr, cleanupRows.Load())
			}
			if cleanupRows.Load() != 1 {
				t.Fatalf("maintenance did not yield between rows: cleanup_rows=%d", cleanupRows.Load())
			}
			if _, err = s.CheckResponse(ctx, scope, ResponseCheck{Guard: oldGuard}); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("committed control did not invalidate its original source guard: %v", err)
			}
			var pendingID MessageID
			var pendingBody string
			if err = s.readDB.QueryRowContext(ctx, "SELECT id,body FROM messages WHERE id IN (?,?) AND state='active'", expiredIDs[0], expiredIDs[1]).Scan(&pendingID, &pendingBody); err != nil || pendingBody != "expiredcleanupreceipt" {
				t.Fatal("yield did not preserve one physically unscrubbed expired row")
			}
			if _, err = s.CheckResponse(ctx, scope, ResponseCheck{Guard: expiredGuards[pendingID]}); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("pending cleanup allowed a pre-expiry response to pass: %v", err)
			}
			if expiredPage, readErr := s.Search(ctx, scope, SearchRequest{Query: "expiredcleanupreceipt"}); readErr != nil || len(expiredPage.Hits) != 0 {
				t.Fatal("yield exposed physically unscrubbed expired text")
			}
			if _, err = s.Context(ctx, scope, ContextRequest{MessageID: pendingID}); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatal("yield exposed expired context by opaque ID")
			}
			fresh := readerApply(t, s, policy, readerEvent("expiry-yield-fresh", "expiry-yield-chat", "freshaftercontrolreceipt", now))
			if page, err = s.Search(ctx, scope, SearchRequest{Query: "freshaftercontrolreceipt"}); err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID != fresh {
				t.Fatal("successful control progress globally paused independent content")
			}
			if err = s.SweepExpired(ctx); err != nil {
				t.Fatalf("later maintenance could not complete the yielded cleanup: %v", err)
			}
			var pendingState string
			if err = s.readDB.QueryRowContext(ctx, "SELECT state,body FROM messages WHERE id=?", pendingID).Scan(&pendingState, &pendingBody); err != nil || pendingState != "expired" || pendingBody != "" || cleanupRows.Load() != 2 {
				t.Fatal("later maintenance did not finish and scrub the pending expired row")
			}
			if _, err = os.Stat(filepath.Join(s.privateDir, "unsafe.marker")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("successful yielding and resumed cleanup created a source safety marker")
			}
		})
	}
}
