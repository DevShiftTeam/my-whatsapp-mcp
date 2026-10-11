package ingestion

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Hold the first original before it reaches the real writer. Received controls
// can therefore still use the archive's independent control admission lane.
type queueBurstStore struct {
	archive.ConnectorStore
	started, unblock       chan struct{}
	startOnce, releaseOnce sync.Once
}

func (s *queueBurstStore) ApplySourceEvent(ctx context.Context, e archive.SourceEvent, generation archive.Generation) (archive.ApplyResult, error) {
	if e.Kind == archive.EventOriginal && e.Key.Message == "burst-first" {
		s.startOnce.Do(func() { close(s.started) })
		select {
		case <-s.unblock:
		case <-ctx.Done():
			return archive.ApplyResult{}, ctx.Err()
		}
	}
	return s.ConnectorStore.ApplySourceEvent(ctx, e, generation)
}

func (s *queueBurstStore) release() { s.releaseOnce.Do(func() { close(s.unblock) }) }

func queueBurstFixture(t *testing.T, byBytes bool) (*archive.Store, *Runtime, *queueBurstStore, *events.Message) {
	t.Helper()
	store, _ := durableFixture(t)
	barrier := &queueBurstStore{ConnectorStore: store, started: make(chan struct{}), unblock: make(chan struct{})}
	r := durableRuntime(t, barrier)
	first := liveMessage("burst-first", "burstfirstreceipt שלום 👋 "+strings.Repeat("a", 400))
	second := liveMessage("burst-second", "burstsecondreceipt שלום 👋 "+strings.Repeat("b", 400))
	r.config.BodyQueueCount = 1
	if byBytes {
		r.config.BodyQueueCount = 8
		r.config.BodyQueueBytes = 64 + retainedBytes(first)
		if 64+retainedBytes(second) > r.config.BodyQueueBytes {
			r.config.BodyQueueBytes = 64 + retainedBytes(second)
		}
	}
	// Registered last so an assertion failure cannot leave Close behind the
	// intentionally blocked source commit.
	t.Cleanup(barrier.release)
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(first)
	select {
	case <-barrier.started:
	case <-time.After(time.Second):
		t.Fatal("first body did not reach the source barrier")
	}
	return store, r, barrier, second
}

func queueBurstDelivery(r *Runtime, event any) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		r.deliver(event)
		close(done)
	}()
	return done
}

func queueBurstAssertWaiting(t *testing.T, r *Runtime, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("temporary saturation rejected the frame before the source commit released capacity")
	case <-time.After(20 * time.Millisecond):
	}
	status := r.Status()
	if status.ReconciliationRequired || status.Dropped != 0 {
		t.Fatalf("temporary saturation fenced readable import: unsafe=%t dropped=%d codes=%v", status.ReconciliationRequired, status.Dropped, status.Codes)
	}
	r.mu.Lock()
	admissions, retained := r.admissions, r.queueBytes
	r.mu.Unlock()
	if admissions != 1 || retained > r.config.BodyQueueBytes {
		t.Fatalf("waiting frame borrowed in-flight capacity: count=%d retained=%d", admissions, retained)
	}
}

func queueBurstAwaitDelivery(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("delivery did not finish after capacity became available")
	}
}

func queueBurstAssertSearch(t *testing.T, store *archive.Store, query string, count int) {
	t.Helper()
	page, err := durableSearch(t, store, query)
	if err != nil || len(page.Hits) != count {
		t.Fatalf("search %q: hits=%d want=%d err=%v", query, len(page.Hits), count, err)
	}
}

func TestQueueBackpressureRetainsCountAndByteBurst(t *testing.T) {
	for _, byBytes := range []bool{false, true} {
		t.Run(map[bool]string{false: "count", true: "bytes"}[byBytes], func(t *testing.T) {
			store, r, barrier, second := queueBurstFixture(t, byBytes)
			done := queueBurstDelivery(r, second)
			queueBurstAssertWaiting(t, r, done)
			barrier.release()
			queueBurstAwaitDelivery(t, done)
			if err := r.Disconnect(context.Background()); err != nil {
				t.Fatal(err)
			}
			queueBurstAssertSearch(t, store, "burstfirstreceipt", 1)
			queueBurstAssertSearch(t, store, "burstsecondreceipt", 1)
			page, err := durableSearch(t, store, "שלום")
			if err != nil || len(page.Hits) != 2 || !strings.Contains(page.Hits[1].Snippet, "👋") {
				t.Fatalf("Unicode burst import was incomplete: hits=%d err=%v", len(page.Hits), err)
			}
			if r.Status().Dropped != 0 || r.Status().ReconciliationRequired || !r.Quiesced() {
				t.Fatalf("recovered burst did not end cleanly: %+v", r.Status())
			}
		})
	}
}

func TestQueueBackpressureDoesNotDelayReceivedDeletion(t *testing.T) {
	store, r, barrier, second := queueBurstFixture(t, false)
	ctx := context.Background()
	if err := r.message(ctx, liveMessage("burst-delete-target", "burstdeletetargetreceipt")); err != nil {
		t.Fatal(err)
	}
	done := queueBurstDelivery(r, second)
	queueBurstAssertWaiting(t, r, done)
	r.deliver(&events.DeleteForMe{ChatJID: types.NewJID("200", types.DefaultUserServer), SenderJID: types.NewJID("200", types.DefaultUserServer), MessageID: "burst-delete-target", Timestamp: time.Unix(2000, 0)})
	queueBurstAssertSearch(t, store, "burstdeletetargetreceipt", 0)
	if r.Status().Controls != 1 || r.Status().ReconciliationRequired {
		t.Fatalf("body pressure blocked received deletion: %+v", r.Status())
	}
	barrier.release()
	queueBurstAwaitDelivery(t, done)
	if err := r.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	queueBurstAssertSearch(t, store, "burstsecondreceipt", 1)
	queueBurstAssertSearch(t, store, "burstdeletetargetreceipt", 0)
}

func TestQueueBackpressureRetainsHistoryTailControl(t *testing.T) {
	store, r, barrier, _ := queueBurstFixture(t, false)
	ctx := context.Background()
	if err := r.message(ctx, liveMessage("burst-history-target", "bursthistorytargetreceipt")); err != nil {
		t.Fatal(err)
	}
	body := historyRecord("burst-history-body", "bursthistorybodyreceipt שלום 👋")
	control := historyRecord("burst-history-control", "")
	control.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("burst-history-target"), RemoteJID: proto.String("200@s.whatsapp.net"), FromMe: proto.Bool(false)}}}
	notification := liveMessage("burst-history-notification", "")
	notification.Info.IsFromMe = true
	notification.Info.Sender = *r.client.Store.ID
	notification.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(), HistorySyncNotification: &waE2E.HistorySyncNotification{InitialHistBootstrapInlinePayload: compressed(historyWire(body, control))}}}
	done := queueBurstDelivery(r, notification)
	queueBurstAssertWaiting(t, r, done)
	barrier.release()
	queueBurstAwaitDelivery(t, done)
	if err := r.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	queueBurstAssertSearch(t, store, "bursthistorybodyreceipt", 1)
	queueBurstAssertSearch(t, store, "bursthistorytargetreceipt", 0)
	metadata, err := store.SourceMetadata(ctx, archive.SourceKey{Account: durableAccount, Chat: "200@s.whatsapp.net", Sender: "200@s.whatsapp.net", Message: "burst-history-target"})
	if err != nil || !metadata.Version.Deleted {
		t.Fatalf("history tail deletion was lost: deleted=%t err=%v", metadata.Version.Deleted, err)
	}
}

func TestQueueBackpressureStopReleasesWaiterBeforeHandlerJoin(t *testing.T) {
	for _, pause := range []bool{false, true} {
		t.Run(map[bool]string{false: "disconnect", true: "pause"}[pause], func(t *testing.T) {
			store, r, barrier, second := queueBurstFixture(t, false)
			done := queueBurstDelivery(r, second)
			queueBurstAssertWaiting(t, r, done)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			stopped := make(chan error, 1)
			go func() {
				if pause {
					stopped <- r.Pause(ctx)
				} else {
					stopped <- r.Disconnect(ctx)
				}
			}()
			// Stop must wake the admitted delivery waiter while the first source
			// remains blocked; otherwise handler removal/join can deadlock.
			queueBurstAwaitDelivery(t, done)
			barrier.release()
			var stopErr error
			select {
			case stopErr = <-stopped:
			case <-time.After(time.Second):
				t.Fatal("stop failed to join the body worker and waiting delivery")
			}
			if stopErr != nil && !errors.Is(stopErr, archive.ErrReconciliationRequired) {
				t.Fatalf("stop exhausted its join deadline: %v", stopErr)
			}
			if !r.Quiesced() {
				t.Fatal("stopped source observation did not quiesce")
			}
			if stopErr == nil {
				queueBurstAssertSearch(t, store, "burstsecondreceipt", 1)
			} else if !r.Status().ReconciliationRequired {
				t.Fatal("uncommitted received frame was discarded without strict coverage protection")
			}
		})
	}
}

func TestQueueBackpressureIndividuallyOversizedGraphRemainsStrict(t *testing.T) {
	store, _ := durableFixture(t)
	r := durableRuntime(t, store)
	r.config.BodyQueueBytes = 256
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	event := liveMessage("burst-oversized", "small")
	event.RawMessage = &waE2E.Message{Conversation: proto.String(strings.Repeat("x", 512))}
	r.deliver(event)
	if !r.Status().ReconciliationRequired || !slices.Contains(r.Status().Codes, "queue_limit_exceeded") {
		t.Fatalf("individual retained graph bypassed admission hard limit: %+v", r.Status())
	}
	if _, err := durableSearch(t, store, "small"); !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatalf("oversized source loss left archive readable: %v", err)
	}
}

func TestQueueBackpressureDefaultCountLeavesByteBudgetUnchanged(t *testing.T) {
	root := canonicalSessionTestDir(t)
	r, err := New(Config{SessionDir: filepath.Join(root, "session"), TemporaryDir: filepath.Join(root, "working")}, fixtureStore(), Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if r.config.BodyQueueCount != 64 || r.config.BodyQueueBytes != 16<<20 {
		t.Fatalf("unexpected burst defaults: count=%d bytes=%d", r.config.BodyQueueCount, r.config.BodyQueueBytes)
	}
}

func TestQueueBackpressureAdmissionDeadlineRemainsStrict(t *testing.T) {
	store, r, barrier, second := queueBurstFixture(t, false)
	r.admissionTimeout = 5 * time.Millisecond
	started := time.Now()
	done := queueBurstDelivery(r, second)
	queueBurstAwaitDelivery(t, done)
	if elapsed := time.Since(started); elapsed < 5*time.Millisecond {
		t.Fatalf("admission rejected temporary pressure before its deadline: %s", elapsed)
	}
	if !r.Status().ReconciliationRequired || r.Status().Dropped != 1 || !slices.Contains(r.Status().Codes, "queue_limit_exceeded") || r.runCtx.Err() == nil {
		t.Fatalf("unresolved bounded admission did not retain strict coverage protection: %+v", r.Status())
	}
	if _, err := durableSearch(t, store, "burstfirstreceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatalf("timed-out source observation remained readable: %v", err)
	}
	barrier.release()
	if err := r.Disconnect(context.Background()); !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatalf("timed-out admission cleared its unsafe observation: %v", err)
	}
}

func TestQueueBackpressurePendingCountAndBytesRemainBounded(t *testing.T) {
	for _, byBytes := range []bool{false, true} {
		t.Run(map[bool]string{false: "count", true: "bytes"}[byBytes], func(t *testing.T) {
			store, r, barrier, second := queueBurstFixture(t, byBytes)
			done := queueBurstDelivery(r, second)
			queueBurstAssertWaiting(t, r, done)
			r.mu.Lock()
			waiting, pendingBytes := r.waitingAdmissions, r.waitingBytes
			r.mu.Unlock()
			if waiting != 1 || pendingBytes != 64+retainedBytes(second) || pendingBytes > r.config.BodyQueueBytes {
				t.Fatalf("pending graph was not charged within its own admission ledger: waiting=%d bytes=%d", waiting, pendingBytes)
			}
			third := liveMessage("burst-third", second.Message.GetConversation())
			// The additional callback cannot create an unbounded collection of
			// retained graphs while the worker's first commit remains blocked.
			extra := queueBurstDelivery(r, third)
			queueBurstAwaitDelivery(t, extra)
			if !r.Status().ReconciliationRequired || r.Status().Dropped == 0 || !slices.Contains(r.Status().Codes, "queue_limit_exceeded") {
				t.Fatalf("pending admission overflow was not strict: %+v", r.Status())
			}
			queueBurstAwaitDelivery(t, done)
			r.mu.Lock()
			waiting, pendingBytes = r.waitingAdmissions, r.waitingBytes
			r.mu.Unlock()
			if waiting != 0 || pendingBytes != 0 {
				t.Fatalf("strict overflow leaked pending graph capacity: waiting=%d bytes=%d", waiting, pendingBytes)
			}
			if _, err := durableSearch(t, store, "burstfirstreceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Fatalf("overflowed received-source observation stayed readable: %v", err)
			}
			barrier.release()
			if err := r.Disconnect(context.Background()); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Fatalf("pending overflow cleared its unsafe observation: %v", err)
			}
		})
	}
}
