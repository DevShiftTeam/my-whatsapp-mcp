package ingestion

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
)

func TestUnreadableMessagesRecordDurableGapAndContinue(t *testing.T) {
	for _, unreadable := range []*events.UndecryptableMessage{
		{},
		{IsUnavailable: true},
		{IsUnavailable: true, UnavailableType: events.UnavailableTypeViewOnce},
	} {
		s, dir := durableFixture(t)
		r := durableRuntime(t, s)
		ctx := context.Background()
		if err := r.Connect(ctx); err != nil {
			t.Fatal("synthetic connection setup failed")
		}
		r.deliver(&events.Connected{})
		unreadable.Info = liveMessage("unreadable", "").Info
		unreadable.Info.PushName = "UNREADABLE_PRIVATE_METADATA"
		r.deliver(unreadable)
		r.deliver(liveMessage("readable", "readablegap searchable receipt"))
		deadline := time.NewTimer(time.Second)
		ticker := time.NewTicker(time.Millisecond)
		for !r.Status().ReconciliationRequired && r.Status().Applied != 1 {
			select {
			case <-ticker.C:
			case <-deadline.C:
				ticker.Stop()
				t.Fatal("bounded synthetic projection did not finish")
			}
		}
		ticker.Stop()
		deadline.Stop()
		if r.Status().ReconciliationRequired || r.runCtx.Err() != nil || !slices.Contains(r.Status().Codes, "undecryptable_message") {
			t.Error("unreadable event stopped ingestion or hid its fixed gap")
		}
		assertReadableGap(t, s, "connected")
		if err := r.Disconnect(ctx); err != nil {
			t.Error("unreadable event prevented a clean owner stop")
		}
		if err := r.Close(); err != nil {
			t.Error("unreadable event prevented a clean owner close")
		}
		if err := s.Close(); err != nil {
			t.Fatal("synthetic archive close failed")
		}
		durableAssertAbsent(t, dir, "UNREADABLE_PRIVATE_METADATA")
		reopened := durableStore(t, dir)
		assertReadableGap(t, reopened, "disconnected")

		// Readability does not weaken a known, parsed malformed mutation.
		guarded := durableRuntime(t, reopened)
		if err := guarded.Connect(ctx); err != nil {
			t.Error("coverage gap incorrectly prevented session reuse")
			continue
		}
		edit := mutation("readable", waE2E.ProtocolMessage_MESSAGE_EDIT)
		edit.Message.ProtocolMessage.EditedMessage = ambiguousProjectionMessage().Message
		guarded.deliver(edit)
		if !guarded.Status().ReconciliationRequired || !slices.Contains(guarded.Status().Codes, "ambiguous_edit_structure") {
			t.Error("known malformed edit lost its strict denial")
		}
		if _, err := durableSearch(t, reopened, "readablegap"); !errors.Is(err, archive.ErrReconciliationRequired) {
			t.Error("known malformed control left content readable")
		}
	}
}

func assertReadableGap(t *testing.T, s *archive.Store, connection archive.ConnectionState) {
	t.Helper()
	page, err := durableSearch(t, s, "readablegap")
	if err != nil || len(page.Hits) != 1 || page.Hits[0].Snippet != "readablegap searchable receipt" {
		t.Error("subsequent readable message was not durably searchable")
	}
	policy, err := s.Policy(context.Background())
	if err != nil {
		t.Fatal("synthetic policy read failed")
	}
	status, err := s.Status(context.Background(), archive.ReadScope{Account: durableAccount, Principal: "local_owner", AllChats: true, PolicyGeneration: policy.Generation})
	if err != nil || status.ReconciliationRequired || status.Connection != connection || status.Messages != 1 || status.Keyword.State != "ready" || status.Coverage != "partial" || !slices.Contains(status.Codes, "undecryptable_message") {
		t.Error("safe status did not preserve readable counts and partial gap")
	}
}
