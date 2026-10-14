//go:build sqlite_fts5

package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Hold a historical positive before real writer admission. Neither the SQLite
// writer nor a reader is stalled, so this specifically detects source scheduling.
type realtimeApplyBarrier struct {
	*archive.Store
	entered, release chan struct{}
	once             sync.Once
}

func (s *realtimeApplyBarrier) ApplySourceEvent(ctx context.Context, e archive.SourceEvent, generation archive.Generation) (archive.ApplyResult, error) {
	if e.Kind == archive.EventOriginal && e.Key.Message == "realtime-held" {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return archive.ApplyResult{}, ctx.Err()
		}
	}
	return s.Store.ApplySourceEvent(ctx, e, generation)
}

func realtimeBarrier(t *testing.T) (*archive.Store, *Runtime, *realtimeApplyBarrier, func()) {
	t.Helper()
	store, _ := durableFixture(t)
	barrier := &realtimeApplyBarrier{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
	r := durableRuntime(t, barrier)
	var once sync.Once
	release := func() { once.Do(func() { close(barrier.release) }) }
	t.Cleanup(release)
	if err := r.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store, r, barrier, release
}

func realtimeHistory(r *Runtime, records ...*waHistorySync.HistorySyncMsg) *events.Message {
	event := deliveredNotification(r, records...)
	event.Message.ProtocolMessage.Type = waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum()
	return event
}

func TestRealtimePlaintextScopedMCPWithinOneSecond(t *testing.T) {
	for _, history := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "history_in_flight"}[history], func(t *testing.T) {
			store, r, barrier, release := realtimeBarrier(t)
			if err := r.message(t.Context(), liveMessage("realtime-prefix", "realtimeprefixreceipt")); err != nil {
				t.Fatal(err)
			}
			prefix, err := durableSearch(t, store, "realtimeprefixreceipt")
			if err != nil || len(prefix.Hits) != 1 {
				t.Fatal("synthetic prefix was not searchable")
			}
			token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
			_, err = store.CreateCredential(t.Context(), archive.CredentialCreate{Label: "Synthetic realtime test", Principal: "client:realtime", Digest: sha256.Sum256([]byte(token)), Policy: archive.CredentialPolicy{Account: durableAccount, Chats: []archive.ChatID{prefix.Hits[0].ChatID}}, ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := mcpserver.NewHandler(mcpserver.Config{Mode: mcpserver.Bearer, Hosts: []string{"synthetic.invalid"}, RequestTimeout: time.Second}, store, store)
			if err != nil {
				t.Fatal(err)
			}
			if history {
				r.deliver(realtimeHistory(r, historyRecord("realtime-held", "realtimehistoryreceipt")))
				awaitHistoryDrain(t, barrier.entered, "held historical positive")
			}
			started := time.Now()
			r.deliver(liveMessage("realtime-independent", "realtimelivereceipt"))
			found := false
			for time.Since(started) < time.Second {
				var page struct {
					Hits []struct {
						MessageID string `json:"message_id"`
					} `json:"hits"`
				}
				deliveredLiveOutput(t, deliveredLiveRPC(t.Context(), handler, token, "search_messages", map[string]any{"query": "realtimelivereceipt", "limit": 1}), &page)
				if len(page.Hits) == 1 {
					found = true
					break
				}
				time.Sleep(time.Millisecond)
			}
			t.Logf("history_in_flight=%t live_visible=%t elapsed_ms=%d", history, found, time.Since(started).Milliseconds())
			if !found {
				t.Error("independent fresh plaintext was not searchable through scoped MCP within one second")
			}
			if history && r.Status().HistoryChunks != 0 {
				t.Error("history unexpectedly drained before fresh visibility")
			}
			release()
			if err := r.Disconnect(t.Context()); err != nil {
				t.Fatal(err)
			}
			if r.Status().Dropped != 0 || r.Status().ReconciliationRequired || !r.Quiesced() {
				t.Fatal("synthetic work did not drain safely")
			}
		})
	}
}

func realtimeAssertBlocked(t *testing.T, store *archive.Store, query string) {
	t.Helper()
	// A deterministic source barrier remains held throughout these real reads.
	for started := time.Now(); time.Since(started) < 30*time.Millisecond; {
		page, err := durableSearch(t, store, query)
		if err != nil || len(page.Hits) != 0 {
			t.Fatal("live body crossed an earlier source barrier")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRealtimePreparedHistoryKeepsSameMessageIDOrdered(t *testing.T) {
	store, r, barrier, release := realtimeBarrier(t)
	r.deliver(realtimeHistory(r, historyRecord("realtime-held", "heldreceipt"), historyRecord("same-mid", "samemidreceipt")))
	awaitHistoryDrain(t, barrier.entered, "held positive")
	live := liveMessage("same-mid", "samemidreceipt")
	// Even another chat/sender with this ID must not evade a conservative key
	// barrier through an alias or transport-identity difference.
	live.Info.Chat = types.NewJID("300", types.DefaultUserServer)
	live.Info.Sender = live.Info.Chat
	r.deliver(live)
	if snapshot := r.IngestionLatency(); !snapshot.Valid() || !snapshot.HistoryPrepared || snapshot.LivePending != 1 {
		t.Fatal("prepared chunk or live barrier was not tracked")
	}
	realtimeAssertBlocked(t, store, "samemidreceipt")
	release()
	if err := r.Disconnect(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err := durableSearch(t, store, "samemidreceipt")
	if err != nil || len(page.Hits) != 2 {
		t.Fatal("same-ID work was lost instead of ordered")
	}
}

func TestRealtimeQueuedHistoryAndLifecycleGateBeforeWorkerStarts(t *testing.T) {
	for _, kind := range []string{"history", "disconnected", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			store, r, barrier, release := realtimeBarrier(t)
			r.deliver(realtimeHistory(r, historyRecord("realtime-held", "heldreceipt")))
			awaitHistoryDrain(t, barrier.entered, "prepared first chunk")
			switch kind {
			case "history":
				r.deliver(realtimeHistory(r, historyRecord("queued-history", "queuedhistoryreceipt")))
			case "disconnected":
				r.deliver(&events.Disconnected{})
			case "unreadable":
				r.deliver(&events.UndecryptableMessage{})
			}
			r.deliver(liveMessage("queued-independent", "queuedlivereceipt"))
			realtimeAssertBlocked(t, store, "queuedlivereceipt")
			release()
			if err := r.Disconnect(t.Context()); err != nil {
				t.Fatal(err)
			}
			page, err := durableSearch(t, store, "queuedlivereceipt")
			if err != nil || len(page.Hits) != 1 {
				t.Fatal("queued live work was lost")
			}
		})
	}
}

func TestRealtimeOrderSensitiveHistoryFallsBackWithoutLosingSuffix(t *testing.T) {
	for _, kind := range []string{"delete", "edit", "prefix_dependent_edit", "expired", "view_once", "admin_revoke", "partial_unknown"} {
		t.Run(kind, func(t *testing.T) {
			store, r, barrier, release := realtimeBarrier(t)
			target := liveMessage("negative-target", "negativetargetreceipt")
			if kind == "admin_revoke" {
				target.Info.Chat = types.NewJID("300", types.GroupServer)
				target.Info.IsGroup = true
			}
			if err := r.message(t.Context(), target); err != nil {
				t.Fatal(err)
			}
			tail := historyRecord("sensitive-tail", "sensitivepositivereceipt")
			switch kind {
			case "delete", "edit":
				operation := waE2E.ProtocolMessage_REVOKE
				if kind == "edit" {
					operation = waE2E.ProtocolMessage_MESSAGE_EDIT
				}
				tail.Message.Message = mutation("negative-target", operation).Message
			case "prefix_dependent_edit":
				tail.Message.Message = mutation("realtime-held", waE2E.ProtocolMessage_MESSAGE_EDIT).Message
			case "expired":
				tail.Message.EphemeralExpirationTimestamp = proto.Uint64(1)
			case "view_once":
				tail.Message.Message = &waE2E.Message{ViewOnceMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("sensitivepositivereceipt")}}}}
			case "admin_revoke":
				tail = bodylessHistoryStub("300@g.us", "negative-target", waWeb.WebMessageInfo_ADMIN_REVOKE)
			case "partial_unknown":
				r.config.UnknownHistoryPolicy = UnknownHistoryPartial
				tail.Message.Message = nil
				tail.Message.MessageStubType = waWeb.WebMessageInfo_StubType(229).Enum()
			}
			notification := realtimeHistory(r, historyRecord("realtime-held", "heldreceipt"), tail, historyRecord("fallback-suffix", "fallbacksuffixreceipt"))
			if kind == "admin_revoke" {
				notification.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = compressed(bodylessHistoryWire(t, "300@g.us", bodylessHistoryRecord("300@g.us", "realtime-held", "heldreceipt"), tail, bodylessHistoryRecord("300@g.us", "fallback-suffix", "fallbacksuffixreceipt")))
			}
			r.deliver(notification)
			awaitHistoryDrain(t, barrier.entered, "fallback positive prefix")
			r.deliver(liveMessage("fallback-live", "fallbacklivereceipt"))
			if r.IngestionLatency().HistoryPrepared {
				t.Fatal("order-sensitive chunk was incorrectly prepared")
			}
			realtimeAssertBlocked(t, store, "fallbacklivereceipt")
			release()
			if err := r.Disconnect(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{"fallbacksuffixreceipt", "fallbacklivereceipt"} {
				page, err := durableSearch(t, store, query)
				if err != nil || len(page.Hits) != 1 {
					t.Fatal("fallback discarded valid history or live work")
				}
			}
			if kind == "delete" || kind == "edit" || kind == "admin_revoke" {
				page, err := durableSearch(t, store, "negativetargetreceipt")
				if err != nil || len(page.Hits) != 0 {
					t.Fatal("history negative was lost")
				}
			}
			if kind == "expired" || kind == "view_once" {
				page, err := durableSearch(t, store, "sensitivepositivereceipt")
				if err != nil || len(page.Hits) != 0 {
					t.Fatal("restricted historical body became readable")
				}
			}
			if kind == "prefix_dependent_edit" {
				page, err := durableSearch(t, store, "edited")
				if err != nil || len(page.Hits) != 1 {
					t.Fatal("preparation guessed edit authority before its original prefix committed")
				}
			}
		})
	}
}

func TestRealtimeMalformedHistorySuffixFencesBeforeAnyFreshPublication(t *testing.T) {
	store, r, _, _ := realtimeBarrier(t)
	bad := historyRecord("invalid-record", "neverpublishedreceipt")
	bad.Message.Message = ambiguousProjectionMessage().Message
	r.deliver(realtimeHistory(r, historyRecord("realtime-held", "heldreceipt"), bad))
	r.deliver(liveMessage("malformed-live", "neverpublishedlivereceipt"))
	deadline := time.Now().Add(time.Second)
	for !r.Status().ReconciliationRequired && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !r.Status().ReconciliationRequired {
		t.Fatal("complete preparation did not detect malformed suffix")
	}
	page, err := durableSearch(t, store, "neverpublishedlivereceipt")
	if !errors.Is(err, archive.ErrReconciliationRequired) || len(page.Hits) != 0 {
		t.Fatal("malformed history allowed a fresh response")
	}
}

func TestRealtimePreparationKeyOverflowKeepsOrderedImport(t *testing.T) {
	for _, byteBound := range []bool{false, true} {
		t.Run(map[bool]string{false: "count", true: "bytes"}[byteBound], func(t *testing.T) {
			store := fixtureStore()
			r := testRuntime(store)
			records := []*waHistorySync.HistorySyncMsg{historyRecord("overflow-prefix", "prefixreceipt")}
			for i := 0; i < preparedKeyCount; i++ {
				id := fmt.Sprintf("prepared-%04d", i)
				if byteBound {
					id += strings.Repeat("x", 230)
				}
				records = append(records, historyRecord(id, "overflowhistoryreceipt"))
			}
			ticket := &sourceTicket{sequence: 1, admitted: true, history: true}
			r.sourceTickets = map[uint64]*sourceTicket{1: ticket}
			r.sourceChanged = make(chan struct{})
			ctx := context.WithValue(t.Context(), sourceTicketContextKey{}, ticket)
			raw := historyWire(records...)
			if err := r.prepareHistoryOvertaking(ctx, raw); err != nil {
				t.Fatal(err)
			}
			if ticket.prepared || ticket.keys != nil {
				t.Fatal("key budget overflow retained keys or allowed overtaking")
			}
			if err := r.projectHistory(t.Context(), raw); err != nil {
				t.Fatal(err)
			}
			if r.Status().Received != int64(len(records)) || r.Status().Dropped != 0 {
				t.Fatal("key overflow discarded records")
			}
		})
	}
}
