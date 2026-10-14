//go:build sqlite_fts5

package ingestion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// The fake transport is the only replacement: admission, authentication,
// invalidation, history projection and durable read guards are the real paths.
func heldHistorySafetyDownload(t *testing.T, r *Runtime, payload []byte) (context.Context, func()) {
	t.Helper()
	entered, release := make(chan context.Context, 1), make(chan struct{})
	var once sync.Once
	releaseDownload := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseDownload)
	r.download = func(ctx context.Context, _ whatsmeow.DownloadableMessage, file whatsmeow.File) error {
		entered <- ctx
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			if err := ctx.Err(); err != nil {
				return err
			}
			_, err := file.Write(payload)
			return err
		}
	}
	if err := r.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.deliver(historyDrainEnvelope(r, len(payload)))
	select {
	case ctx := <-entered:
		return ctx, releaseDownload
	case <-time.After(2 * time.Second):
		t.Fatal("authenticated history download did not start")
		return nil, releaseDownload
	}
}

func historySafetyHits(t *testing.T, store *archive.Store, query string, want int) {
	t.Helper()
	page, err := durableSearch(t, store, query)
	if err != nil || len(page.Hits) != want {
		t.Fatalf("source visibility: got %d hits, want %d, error %v", len(page.Hits), want, err)
	}
}

func historySafetyNoUnsafeMarker(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(dir, "unsafe.marker")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("valid source observation acquired an unsafe marker: %v", err)
	}
}

func TestHistoryDownloadPrivacyControlsPreserveNegativeGuards(t *testing.T) {
	chat := types.NewJID("200", types.DefaultUserServer)
	cutoff := time.Unix(2000, 0)
	type controlCase struct {
		name    string
		control func() any
		cleared bool
		edited  bool
	}
	cases := []controlCase{
		{name: "live_revoke", control: func() any { return mutation("held-target", waE2E.ProtocolMessage_REVOKE) }},
		{name: "live_edit", edited: true, control: func() any {
			e := mutation("held-target", waE2E.ProtocolMessage_MESSAGE_EDIT)
			e.Message.ProtocolMessage.EditedMessage.Conversation = proto.String("heldeditedreceipt")
			return e
		}},
	}
	for _, fullSync := range []bool{false, true} {
		suffix := "incremental"
		if fullSync {
			suffix = "full_sync"
		}
		cases = append(cases,
			controlCase{name: "delete_for_me_" + suffix, control: func() any {
				return &events.DeleteForMe{ChatJID: chat, SenderJID: chat, MessageID: "held-target", Timestamp: cutoff, FromFullSync: fullSync}
			}},
			controlCase{name: "clear_chat_" + suffix, cleared: true, control: func() any {
				return &events.ClearChat{JID: chat, Timestamp: cutoff, FromFullSync: fullSync}
			}},
			controlCase{name: "delete_chat_" + suffix, cleared: true, control: func() any {
				return &events.DeleteChat{JID: chat, Timestamp: cutoff, FromFullSync: fullSync}
			}},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, dir := durableFixture(t)
			r := durableRuntime(t, store)
			if err := r.message(t.Context(), liveMessage("held-target", "heldoriginalreceipt")); err != nil {
				t.Fatal(err)
			}
			historySafetyHits(t, store, "heldoriginalreceipt", 1)
			atCutoff := historyRecord("unseen-at-cutoff", "heldcutoffreceipt")
			atCutoff.Message.MessageTimestamp = proto.Uint64(uint64(cutoff.Unix()))
			afterCutoff := historyRecord("unseen-after-cutoff", "heldaftercutoffreceipt")
			afterCutoff.Message.MessageTimestamp = proto.Uint64(uint64(cutoff.Unix() + 1))
			otherChat := historyRecord("held-target", "heldotherchatreceipt")
			otherChat.Message.Key.RemoteJID = proto.String("201@s.whatsapp.net")
			raw, err := proto.Marshal(&waHistorySync.HistorySync{
				SyncType: waHistorySync.HistorySync_RECENT.Enum(),
				Conversations: []*waHistorySync.Conversation{
					{ID: proto.String(chat.String()), Messages: []*waHistorySync.HistorySyncMsg{
						historyRecord("held-target", "heldoriginalreceipt"),
						historyRecord("unseen-before-cutoff", "heldbeforereceipt"), atCutoff, afterCutoff,
					}},
					{ID: proto.String("201@s.whatsapp.net"), Messages: []*waHistorySync.HistorySyncMsg{otherChat}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			downloadCtx, release := heldHistorySafetyDownload(t, r, compressed(raw))
			r.deliver(tc.control())
			if downloadCtx.Err() != nil || r.runCtx.Err() != nil || r.Status().ReconciliationRequired || r.Status().Controls != 1 {
				t.Fatal("received privacy control canceled source history or falsely fenced the archive")
			}
			// Invalidation is synchronous even though the source worker is held.
			historySafetyHits(t, store, "heldoriginalreceipt", 0)
			historySafetyNoUnsafeMarker(t, dir)
			release()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if err := r.Disconnect(ctx); err != nil {
				t.Fatal("controlled source download did not drain", err)
			}
			if r.Status().HistoryChunks != 1 || r.Status().ReconciliationRequired {
				t.Fatal("privacy control discarded its accepted history envelope")
			}
			check := func(s *archive.Store) {
				historySafetyHits(t, s, "heldoriginalreceipt", 0)
				historySafetyHits(t, s, "heldaftercutoffreceipt", 1)
				historySafetyHits(t, s, "heldotherchatreceipt", 1)
				wantOld := 1
				if tc.cleared {
					wantOld = 0
				}
				historySafetyHits(t, s, "heldbeforereceipt", wantOld)
				historySafetyHits(t, s, "heldcutoffreceipt", wantOld)
				if tc.edited {
					historySafetyHits(t, s, "heldeditedreceipt", 1)
				}
			}
			check(store)
			historySafetyNoUnsafeMarker(t, dir)
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			check(durableStore(t, dir))
			historySafetyNoUnsafeMarker(t, dir)
		})
	}
}

func TestHistoryDownloadPrivacyControlStillCancelsMediaSlots(t *testing.T) {
	store, dir := durableFixture(t)
	r := durableRuntime(t, store)
	legacyCtx, cancelLegacy := context.WithCancel(t.Context())
	defer cancelLegacy()
	r.activeMu.Lock()
	r.activeCancel = cancelLegacy
	r.activeMu.Unlock()
	downloadCtx, release := heldHistorySafetyDownload(t, r, compressed(historyWire(historyRecord("media-slot-source", "heldmediaslotreceipt"))))
	// The optional loop cannot claim work while source history owns admission.
	// Register a real attempt context to qualify both existing media-only slots.
	r.optionalMu.Lock()
	n := r.optional
	r.optionalMu.Unlock()
	optionalCtx, finishOptional := n.attempt(30 * time.Second)
	defer finishOptional()
	r.deliver(&events.DeleteForMe{
		ChatJID: types.NewJID("200", types.DefaultUserServer), SenderJID: types.NewJID("200", types.DefaultUserServer),
		MessageID: "media-slot-negative", Timestamp: time.Unix(2000, 0),
	})
	if !errors.Is(legacyCtx.Err(), context.Canceled) || !errors.Is(optionalCtx.Err(), context.Canceled) {
		t.Fatal("history isolation disabled received-control cancellation of media work")
	}
	if downloadCtx.Err() != nil || r.runCtx.Err() != nil || r.Status().ReconciliationRequired {
		t.Fatal("media-only cancellation reached admitted source history")
	}
	release()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	historySafetyHits(t, store, "heldmediaslotreceipt", 1)
	historySafetyNoUnsafeMarker(t, dir)
}

func TestHistoryDownloadGenuineFailClosedCancelsRunAndPersists(t *testing.T) {
	store, dir := durableFixture(t)
	r := durableRuntime(t, store)
	if err := r.message(t.Context(), liveMessage("genuine-safety-source", "heldgenuinereceipt")); err != nil {
		t.Fatal(err)
	}
	downloadCtx, _ := heldHistorySafetyDownload(t, r, compressed(historyWire(historyRecord("never-completed", "heldneverreceipt"))))
	r.failClosed("archive_unavailable")
	if !errors.Is(downloadCtx.Err(), context.Canceled) || !errors.Is(r.runCtx.Err(), context.Canceled) {
		t.Fatal("genuine fail-closed no longer cancels admitted history through its run context")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.Disconnect(ctx); !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatal("genuine safety failure reported a clean source drain", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "unsafe.marker")); err != nil {
		t.Fatal("genuine failure omitted its durable unsafe marker", err)
	}
	if _, err := durableSearch(t, store, "heldgenuinereceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatal("genuine source failure left protected reads available", err)
	}
	if err := r.Close(); err != nil && !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := durableStore(t, dir)
	if _, err := durableSearch(t, reopened, "heldgenuinereceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatal("restart cleared a genuine source safety failure", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "unsafe.marker")); err != nil {
		t.Fatal("restart removed the genuine unsafe marker", err)
	}
}

func TestHistoryDownloadDeadlineRemainsBoundedAndFailsClosed(t *testing.T) {
	store, dir := durableFixture(t)
	r := durableRuntime(t, store)
	var observed context.Context
	var remaining time.Duration
	r.download = func(ctx context.Context, _ whatsmeow.DownloadableMessage, _ whatsmeow.File) error {
		observed = ctx
		deadline, ok := ctx.Deadline()
		if ok {
			remaining = time.Until(deadline)
		}
		// Qualify the deadline-error path without waiting thirty seconds.
		return context.DeadlineExceeded
	}
	if err := r.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Exercise the notification as its concrete SDK pointer, not an inline body.
	err := r.historyFrom(r.runCtx, historyDrainEnvelope(r, 1))
	if !errors.Is(err, context.DeadlineExceeded) || observed == nil || remaining <= 29*time.Second || remaining > 30*time.Second {
		t.Fatal("history transfer lost its original thirty-second bound or deadline cause")
	}
	var staged *historyStageError
	if !errors.As(err, &staged) || staged == nil || staged.stage != "download" {
		t.Fatal("download deadline lost its typed source-stage error")
	}
	if !r.Status().ReconciliationRequired || !errors.Is(r.runCtx.Err(), context.Canceled) || !errors.Is(observed.Err(), context.Canceled) {
		t.Fatal("a genuine download deadline no longer fences and cancels the source run")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := r.Disconnect(ctx); !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatal("download deadline was treated as optional media failure", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "unsafe.marker")); err != nil {
		t.Fatal("download deadline omitted its durable unsafe marker", err)
	}
}
