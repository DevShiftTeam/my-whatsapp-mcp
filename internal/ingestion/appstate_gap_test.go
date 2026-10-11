package ingestion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func durableAppStateGapStatus(t *testing.T, s *archive.Store) archive.StatusSnapshot {
	t.Helper()
	p, err := s.Policy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(context.Background(), archive.ReadScope{Account: durableAccount, Principal: "local_owner", AllChats: true, PolicyGeneration: p.Generation})
	if err != nil {
		t.Fatal(err)
	}
	if status.Coverage != "partial" || !slices.Contains(status.Codes, "app_state_sync_error") {
		t.Fatalf("app-state gap was not durably visible: coverage=%s codes=%v", status.Coverage, status.Codes)
	}
	return status
}

func TestDurableAppStateGapContinuesReadableMessagesAndReceivedDeletion(t *testing.T) {
	s, dir := durableFixture(t)
	r := durableRuntime(t, s)
	ctx := context.Background()
	if err := r.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	activeCanceled := make(chan struct{}, 1)
	r.activeMu.Lock()
	r.activeCancel = func() {
		select {
		case activeCanceled <- struct{}{}:
		default:
		}
	}
	r.activeMu.Unlock()
	r.deliver(&events.AppStateSyncError{Name: appstate.WAPatchRegular, Error: appstate.ErrMismatchingLTHash, FullSync: true})
	if r.Status().ReconciliationRequired || r.runCtx.Err() != nil {
		t.Error("app-state coverage gap canceled readable-message admission")
	}
	select {
	case <-activeCanceled:
		t.Error("app-state coverage gap canceled active work")
	default:
	}
	r.activeMu.Lock()
	r.activeCancel = nil
	r.activeMu.Unlock()
	r.deliver(liveMessage("gap-readable", "appstategapreadable שלום 👋"))
	if err := r.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := durableSearch(t, s, "appstategapreadable")
	if err != nil || len(page.Hits) != 1 || page.Hits[0].Snippet != "appstategapreadable שלום 👋" {
		t.Fatalf("readable message did not continue through the gap: hits=%v err=%v", page.Hits, err)
	}
	durableAppStateGapStatus(t, s)
	if err := r.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	r.deliver(&events.DeleteForMe{ChatJID: types.NewJID("200", types.DefaultUserServer), SenderJID: types.NewJID("200", types.DefaultUserServer), MessageID: "gap-readable", Timestamp: time.Unix(1100, 0)})
	r.deliver(liveMessage("gap-later", "appstategaplater received after deletion"))
	if err := r.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	page, err = durableSearch(t, s, "appstategapreadable")
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("received deletion stopped applying after app-state gap: hits=%v err=%v", page.Hits, err)
	}
	page, err = durableSearch(t, s, "appstategaplater")
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("later readable message did not continue: hits=%v err=%v", page.Hits, err)
	}
	metadata, err := s.SourceMetadata(ctx, archive.SourceKey{Account: durableAccount, Chat: "200@s.whatsapp.net", Sender: "200@s.whatsapp.net", Message: "gap-readable"})
	if err != nil || !metadata.Version.Deleted {
		t.Fatalf("received deletion did not persist its terminal state: deleted=%t err=%v", metadata.Version.Deleted, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "unsafe.marker")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nonfatal app-state coverage gap wrote an unsafe marker: %v", err)
	}
}

func TestDurableAppStateGapSurvivesReconnectAndReopen(t *testing.T) {
	s, dir := durableFixture(t)
	r := durableRuntime(t, s)
	ctx := context.Background()
	if err := r.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.AppStateSyncError{Name: appstate.WAPatchRegularHigh, Error: appstate.ErrMismatchingPatchMAC})
	if err := r.Reconnect(ctx); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	r.deliver(&events.AppStateSyncComplete{Name: appstate.WAPatchRegularHigh})
	if err := r.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	durableAppStateGapStatus(t, s)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := durableStore(t, dir)
	durableAppStateGapStatus(t, reopened)
	fresh := durableRuntime(t, reopened)
	if err := fresh.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	fresh.deliver(&events.Connected{})
	fresh.deliver(&events.AppStateSyncComplete{Name: appstate.WAPatchRegularHigh})
	fresh.deliver(liveMessage("gap-reopened", "appstategapreopened readable receipt"))
	if err := fresh.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := durableSearch(t, reopened, "appstategapreopened")
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("durable gap blocked fresh runtime admission: hits=%v err=%v", page.Hits, err)
	}
	durableAppStateGapStatus(t, reopened)
}

type appStateGapPersistenceFailureStore struct {
	archive.ConnectorStore
	err error
}

func (s *appStateGapPersistenceFailureStore) SetSyncStatus(ctx context.Context, update archive.SyncUpdate) error {
	if update.GapCode == "app_state_sync_error" {
		return s.err
	}
	return s.ConnectorStore.SetSyncStatus(ctx, update)
}

func TestDurableAppStateGapPersistenceFailureRemainsStrict(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, errors.New("synthetic_gap_write_failure")} {
		s, dir := durableFixture(t)
		r := durableRuntime(t, &appStateGapPersistenceFailureStore{ConnectorStore: s, err: failure})
		ctx := context.Background()
		if err := r.message(ctx, liveMessage("before-gap-write-failure", "appstategapwritefailure original")); err != nil {
			t.Fatal(err)
		}
		if err := r.Connect(ctx); err != nil {
			t.Fatal(err)
		}
		r.deliver(&events.AppStateSyncError{Name: appstate.WAPatchRegular, Error: appstate.ErrMismatchingLTHash})
		if !r.Status().ReconciliationRequired || r.runCtx.Err() == nil || !slices.Contains(r.Status().Codes, "archive_unavailable") {
			t.Error("failed durable gap persistence did not stop and fence the runtime")
		}
		r.deliver(liveMessage("after-gap-write-failure", "appstategapwritefailure later"))
		if _, err := durableSearch(t, s, "appstategapwritefailure"); !errors.Is(err, archive.ErrReconciliationRequired) {
			t.Errorf("failed durable gap persistence left archive readable: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "unsafe.marker")); err != nil {
			t.Errorf("failed durable gap persistence did not retain unsafe marker: %v", err)
		}
		if err := r.Disconnect(ctx); !errors.Is(err, archive.ErrReconciliationRequired) {
			t.Errorf("failed gap persistence allowed a clean disconnect: %v", err)
		}
	}
}
