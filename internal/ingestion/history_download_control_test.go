//go:build sqlite_fts5

package ingestion

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestHistoryDownloadSurvivesReceivedPrivacyControl(t *testing.T) {
	store, _ := durableFixture(t)
	r := durableRuntime(t, store)
	entered, release, cancelled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseDownload := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseDownload)
	if err := r.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.message(t.Context(), liveMessage("download-control-target", "downloadcontroltargetreceipt")); err != nil {
		t.Fatal(err)
	}
	r.download = func(ctx context.Context, _ whatsmeow.DownloadableMessage, file whatsmeow.File) error {
		close(entered)
		select {
		case <-ctx.Done():
			close(cancelled)
			return ctx.Err()
		case <-release:
			_, err := file.Write(compressed(historyWire(historyRecord("download-positive", "downloadpositivereceipt"))))
			return err
		}
	}
	history := realtimeHistory(r, historyRecord("unused-inline", "unusedreceipt"))
	history.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = nil
	r.deliver(history)
	awaitHistoryDrain(t, entered, "authenticated history download")
	r.deliver(&events.DeleteForMe{
		ChatJID: types.NewJID("200", types.DefaultUserServer), SenderJID: types.NewJID("200", types.DefaultUserServer),
		MessageID: "download-control-target", Timestamp: time.Unix(2000, 0),
	})
	page, err := durableSearch(t, store, "downloadcontroltargetreceipt")
	if err != nil || len(page.Hits) != 0 {
		t.Fatal("received privacy control was delayed by source download", err)
	}
	select {
	case <-cancelled:
		t.Fatal("privacy invalidation cancelled admitted history and falsely fenced the archive")
	case <-time.After(20 * time.Millisecond):
	}
	releaseDownload()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := r.Disconnect(ctx); err != nil {
		t.Fatal("valid history failed its owned drain", err)
	}
	page, err = durableSearch(t, store, "downloadpositivereceipt")
	if err != nil || len(page.Hits) != 1 || r.Status().ReconciliationRequired {
		t.Fatal("privacy control lost downloaded source work or introduced a safety pause", err)
	}
}
