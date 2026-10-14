//go:build sqlite_fts5

package ingestion

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

type realtimeDualBarrier struct {
	*realtimeApplyBarrier
	liveEntered, liveRelease chan struct{}
	liveOnce                 sync.Once
}

func (s *realtimeDualBarrier) ApplySourceEvent(ctx context.Context, e archive.SourceEvent, generation archive.Generation) (archive.ApplyResult, error) {
	if e.Kind == archive.EventOriginal && e.Key.Message == "realtime-live-held" {
		s.liveOnce.Do(func() { close(s.liveEntered) })
		select {
		case <-s.liveRelease:
		case <-ctx.Done():
			return archive.ApplyResult{}, ctx.Err()
		}
	}
	return s.realtimeApplyBarrier.ApplySourceEvent(ctx, e, generation)
}

func realtimeDualFixture(t *testing.T) (*archive.Store, *Runtime, *realtimeDualBarrier, func(), func()) {
	t.Helper()
	store, _ := durableFixture(t)
	barrier := &realtimeDualBarrier{realtimeApplyBarrier: &realtimeApplyBarrier{Store: store, entered: make(chan struct{}), release: make(chan struct{})}, liveEntered: make(chan struct{}), liveRelease: make(chan struct{})}
	r := durableRuntime(t, barrier)
	var historyOnce, liveOnce sync.Once
	releaseHistory := func() { historyOnce.Do(func() { close(barrier.release) }) }
	releaseLive := func() { liveOnce.Do(func() { close(barrier.liveRelease) }) }
	t.Cleanup(releaseHistory)
	t.Cleanup(releaseLive)
	return store, r, barrier, releaseHistory, releaseLive
}

func TestRealtimeSharedQueuedInflightAndWaitingBudgets(t *testing.T) {
	for _, byBytes := range []bool{false, true} {
		t.Run(map[bool]string{false: "count", true: "bytes"}[byBytes], func(t *testing.T) {
			store, r, barrier, releaseHistory, releaseLive := realtimeDualFixture(t)
			history := realtimeHistory(r, historyRecord("realtime-held", "heldreceipt"))
			live := liveMessage("realtime-live-held", "dualheldlivereceipt")
			third := liveMessage("third-live", "dualthirdlivereceipt")
			charged := 128 + retainedBytes(history) + retainedBytes(live)
			if byBytes {
				r.config.BodyQueueBytes = charged
			} else {
				r.config.BodyQueueCount = 2
			}
			if err := r.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			r.deliver(history)
			awaitHistoryDrain(t, barrier.entered, "history worker")
			r.deliver(live)
			awaitHistoryDrain(t, barrier.liveEntered, "live worker")
			done := queueBurstDelivery(r, third)
			select {
			case <-done:
				t.Fatal("third body bypassed the shared admission bound")
			case <-time.After(20 * time.Millisecond):
			}
			snapshot := r.IngestionLatency()
			if !snapshot.Valid() || snapshot.QueuedCount != 2 || snapshot.QueuedBytes != charged || snapshot.WaitingCount != 1 || snapshot.WaitingBytes != 64+retainedBytes(third) || snapshot.Unsafe {
				t.Fatal("two workers borrowed independent capacity", snapshot)
			}
			releaseLive()
			queueBurstAwaitDelivery(t, done)
			deadline := time.Now().Add(time.Second)
			for {
				page, err := durableSearch(t, store, "dualthirdlivereceipt")
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Hits) == 1 {
					break
				}
				if !time.Now().Before(deadline) {
					t.Fatal("waiting live work did not use released shared capacity")
				}
				time.Sleep(time.Millisecond)
			}
			releaseHistory()
			if err := r.Disconnect(t.Context()); err != nil {
				t.Fatal(err)
			}
			snapshot = r.IngestionLatency()
			if !snapshot.Valid() || snapshot.Running || snapshot.QueuedCount != 0 || snapshot.WaitingCount != 0 || snapshot.LivePending != 0 || snapshot.HistoryPending != 0 {
				t.Fatal("source drain leaked admission state")
			}
		})
	}
}

func TestRealtimeReservedSlotDoesNotDeadlockEarlierWaitingHistory(t *testing.T) {
	store, r, barrier, release := realtimeBarrier(t)
	r.config.BodyQueueCount = 2
	r.deliver(realtimeHistory(r, historyRecord("realtime-held", "heldreceipt")))
	awaitHistoryDrain(t, barrier.entered, "first prepared history")
	waitingHistory := queueBurstDelivery(r, realtimeHistory(r, historyRecord("reserved-history", "reservedhistoryreceipt")))
	deadline := time.Now().Add(time.Second)
	for r.IngestionLatency().WaitingCount != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if r.IngestionLatency().WaitingCount != 1 {
		t.Fatal("history did not respect the realtime reservation")
	}
	liveDone := queueBurstDelivery(r, liveMessage("reserved-live", "reservedlivereceipt"))
	select {
	case <-liveDone:
		t.Fatal("live callback enqueued ahead of its waiting prerequisite")
	case <-time.After(20 * time.Millisecond):
	}
	snapshot := r.IngestionLatency()
	if !snapshot.Valid() || snapshot.QueuedCount != 2 || snapshot.WaitingCount != 1 {
		t.Fatal("reserved live capacity was not bounded")
	}
	realtimeAssertBlocked(t, store, "reservedlivereceipt")
	release()
	queueBurstAwaitDelivery(t, waitingHistory)
	queueBurstAwaitDelivery(t, liveDone)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := r.Disconnect(ctx); err != nil {
		t.Fatal("later live item deadlocked its prerequisite history", err)
	}
	for _, query := range []string{"reservedhistoryreceipt", "reservedlivereceipt"} {
		page, err := durableSearch(t, store, query)
		if err != nil || len(page.Hits) != 1 {
			t.Fatal("reservation lost accepted source work")
		}
	}
}

func TestRealtimeStopJoinsBothWorkersAndDeadlineRemainsUnsafe(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "deadline"}[deadline], func(t *testing.T) {
			store, r, barrier, releaseHistory, releaseLive := realtimeDualFixture(t)
			if err := r.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			r.deliver(realtimeHistory(r, historyRecord("realtime-held", "heldreceipt")))
			awaitHistoryDrain(t, barrier.entered, "held history")
			r.deliver(liveMessage("realtime-live-held", "heldlivereceipt"))
			awaitHistoryDrain(t, barrier.liveEntered, "held live")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stopped := make(chan error, 1)
			go func() { stopped <- r.Pause(ctx) }()
			// Observe the admission cutoff without racing on lifecycle channels.
			for {
				r.deliveryMu.Lock()
				accepting := r.accepting
				r.deliveryMu.Unlock()
				if !accepting {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if deadline {
				cancel()
				if err := <-stopped; err == nil {
					t.Fatal("missed two-worker drain reported clean stop")
				}
				releaseHistory()
				releaseLive()
				if err := r.Disconnect(t.Context()); !errors.Is(err, archive.ErrReconciliationRequired) {
					t.Fatal("retry cleared the strict drain failure")
				}
				if _, err := durableSearch(t, store, "heldlivereceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
					t.Fatal("failed drain left protected reads available")
				}
				if r.observation == "" || !r.Status().ReconciliationRequired {
					t.Fatal("failed drain ended its source observation")
				}
				return
			}
			releaseHistory()
			select {
			case err := <-stopped:
				t.Fatal("stop acknowledged while live worker remained held", err)
			case <-time.After(20 * time.Millisecond):
			}
			releaseLive()
			if err := <-stopped; err != nil {
				t.Fatal(err)
			}
			if !r.Quiesced() || r.observation != "" || r.IngestionLatency().QueuedCount != 0 {
				t.Fatal("clean stop did not join all source ownership")
			}
		})
	}
}

func TestRealtimeClassifierKeepsRecoveryAndRetentionOnSourceLane(t *testing.T) {
	r := testRuntime(fixtureStore())
	r.config.Rebuild = &RebuildConfig{Boundary: time.Unix(1500, 0)}
	for _, kind := range []string{"ordinary", "extended", "device_sent", "message_context", "older", "recovery", "web", "ephemeral", "expiry", "mutation", "media", "unknown", "nested_unknown"} {
		t.Run(kind, func(t *testing.T) {
			e := liveMessage("classification", "textreceipt")
			e.Info.Timestamp = time.Unix(2000, 0)
			want := false
			switch kind {
			case "ordinary":
				want = true
			case "extended":
				e.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("textreceipt")}}
				want = true
			case "device_sent":
				e.RawMessage = &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: proto.Clone(e.Message).(*waE2E.Message)}}
				want = true
			case "message_context":
				e.Message.MessageContextInfo = &waE2E.MessageContextInfo{MessageSecret: []byte{1, 2}}
				want = true
			case "older":
				e.Info.Timestamp = time.Unix(1000, 0)
			case "recovery":
				e.UnavailableRequestID = "registered-request"
			case "web":
				e.SourceWebMsg = historyRecord("classification", "textreceipt").Message
			case "ephemeral":
				e.IsEphemeral = true
			case "expiry":
				e.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("textreceipt"), ContextInfo: &waE2E.ContextInfo{Expiration: proto.Uint32(0)}}}
			case "mutation":
				e = mutation("classification", waE2E.ProtocolMessage_MESSAGE_EDIT)
			case "media":
				e.Message = validImageEvent().Message
			case "unknown":
				addUnknownHistoryField(e.Message)
			case "nested_unknown":
				e.Message = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("textreceipt")}}
				addUnknownHistoryField(e.Message.ExtendedTextMessage)
			}
			if r.realtimePlaintext(e) != want {
				t.Fatal("plaintext lane admitted uncertain source authority or rejected an ordinary wrapper")
			}
		})
	}
}

func TestRealtimePreparationAllowsOrdinaryMediaButFallsBackOnNestedUnknown(t *testing.T) {
	for _, kind := range []string{"ordinary_media", "unknown_media_field", "unreviewed_quote_context"} {
		t.Run(kind, func(t *testing.T) {
			r := testRuntime(fixtureStore())
			ticket := &sourceTicket{sequence: 1, admitted: true, history: true}
			r.sourceTickets = map[uint64]*sourceTicket{1: ticket}
			r.sourceChanged = make(chan struct{})
			ctx := context.WithValue(t.Context(), sourceTicketContextKey{}, ticket)
			record := historyRecord("media-preparation", "")
			record.Message.Message = validImageEvent().Message
			// The shared media-reference fixture intentionally contains a quote.
			// Ordinary media still qualifies, while unreviewed quote/source
			// association metadata retains the conservative ordered barrier.
			if kind != "unreviewed_quote_context" {
				record.Message.Message.ImageMessage.ContextInfo = nil
			}
			if kind == "unknown_media_field" {
				addUnknownHistoryField(record.Message.Message.ImageMessage)
			}
			if err := r.prepareHistoryOvertaking(ctx, historyWire(record)); err != nil {
				t.Fatal(err)
			}
			if ticket.prepared != (kind == "ordinary_media") {
				t.Fatal("media preparation treated an unknown shape as ordinary authority")
			}
		})
	}
}
