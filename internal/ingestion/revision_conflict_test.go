package ingestion

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestCommittedRevisionConflictContinuesLiveAndHistory(t *testing.T) {
	for _, history := range []bool{false, true} {
		s, _ := durableFixture(t)
		r := durableRuntime(t, s)
		ctx := context.Background()
		if err := r.Connect(ctx); err != nil {
			t.Fatal("synthetic connection setup failed")
		}
		if err := r.message(ctx, liveMessage("conflict", "conflictoriginal receipt")); err != nil {
			t.Fatal("initial synthetic message failed")
		}
		if history {
			// The same bounded history chunk must advance beyond its quarantined row.
			editReplay := historyRecord("edit-envelope", "")
			editReplay.Message.Message = mutation("conflict", waE2E.ProtocolMessage_MESSAGE_EDIT).Message
			editReplay.Message.Message.ProtocolMessage.TimestampMS = proto.Int64(1000000)
			if err := r.projectHistory(ctx, historyWire(historyRecord("conflict", "conflictchanged receipt"), historyRecord("conflict", "conflictoriginal receipt"), editReplay, historyRecord("survivor", "conflictsurvivor searchable receipt"))); err != nil {
				t.Fatal("committed conflict stopped the remaining history records")
			}
		} else {
			r.deliver(liveMessage("conflict", "conflictchanged receipt"))
			r.deliver(liveMessage("conflict", "conflictoriginal receipt"))
			r.deliver(liveMessage("survivor", "conflictsurvivor searchable receipt"))
			deadline := time.NewTimer(time.Second)
			ticker := time.NewTicker(time.Millisecond)
			for !r.Status().ReconciliationRequired && r.Status().Applied < 2 {
				select {
				case <-ticker.C:
				case <-deadline.C:
					ticker.Stop()
					t.Fatal("bounded synthetic live projection did not finish")
				}
			}
			ticker.Stop()
			deadline.Stop()
		}
		if r.Status().ReconciliationRequired || r.runCtx.Err() != nil {
			t.Fatal("per-message quarantine canceled readable ingestion")
		}
		if slices.Contains(r.Status().Codes, "source_revision_conflict") {
			t.Error("chat-scoped conflict leaked through global runtime codes")
		}
		for _, term := range []string{"conflictoriginal", "conflictchanged"} {
			page, err := durableSearch(t, s, term)
			if err != nil || len(page.Hits) != 0 {
				t.Error("quarantined conflict remained searchable")
			}
		}
		page, err := durableSearch(t, s, "conflictsurvivor")
		if err != nil || len(page.Hits) != 1 {
			t.Fatal("subsequent readable message was not searchable")
		}
		policy, err := s.Policy(ctx)
		if err != nil {
			t.Fatal("synthetic policy read failed")
		}
		scope := archive.ReadScope{Account: durableAccount, Principal: "local_owner", AllChats: true, PolicyGeneration: policy.Generation}
		status, err := s.Status(ctx, scope)
		if err != nil || status.ReconciliationRequired || status.Messages != 1 || status.Coverage != "partial" || !slices.Contains(status.Codes, "source_revision_conflict") {
			t.Error("safe status omitted committed conflict coverage")
		}
		// Older replay cannot restore the quarantined row either.
		older := liveMessage("conflict", "conflictoriginal receipt")
		older.Info.Timestamp = time.Unix(999, 0)
		if err := r.messageWithMediaOutcome(ctx, older); err != nil {
			t.Error("existing quarantine barrier stopped unrelated work")
		}
		page, err = durableSearch(t, s, "conflictoriginal")
		if err != nil || len(page.Hits) != 0 {
			t.Error("older replay resurrected the quarantined row before delete")
		}
		replayEdit := mutation("conflict", waE2E.ProtocolMessage_MESSAGE_EDIT)
		replayEdit.Message.ProtocolMessage.TimestampMS = proto.Int64(1000000)
		r.deliver(replayEdit)
		if r.Status().ReconciliationRequired || r.runCtx.Err() != nil {
			t.Error("committed control barrier canceled unrelated ingestion")
		}
		page, err = durableSearch(t, s, "edited")
		if err != nil || len(page.Hits) != 0 {
			t.Error("equal-revision edit restored quarantined content")
		}
		// A valid later delete remains authoritative, including over the barrier.
		r.deliver(mutation("conflict", waE2E.ProtocolMessage_REVOKE))
		result, err := s.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: durableAccount, Chat: "200@s.whatsapp.net", Sender: "200@s.whatsapp.net", Message: "conflict"}, Kind: archive.EventOriginal, SourceRevision: 1000000, OriginalSentAt: time.Unix(1000, 0), OriginalDateKnown: true, BodyKind: archive.MessageText, Body: "conflictoriginal receipt"}, policy.Generation)
		if err != nil || !result.Version.Deleted {
			t.Error("late delete lost its terminal source barrier")
		}
		page, err = durableSearch(t, s, "conflictoriginal")
		if err != nil || len(page.Hits) != 0 {
			t.Error("old original resurrected after a delete")
		}
		// Continuing a committed conflict never accepts a malformed edit.
		bad := mutation("survivor", waE2E.ProtocolMessage_MESSAGE_EDIT)
		bad.Message.ProtocolMessage.EditedMessage = ambiguousProjectionMessage().Message
		r.deliver(bad)
		if !r.Status().ReconciliationRequired || !slices.Contains(r.Status().Codes, "ambiguous_edit_structure") {
			t.Error("known malformed edit lost strict denial")
		}
		if _, err := durableSearch(t, s, "conflictsurvivor"); !errors.Is(err, archive.ErrReconciliationRequired) {
			t.Error("malformed edit left content readable")
		}
	}
}
