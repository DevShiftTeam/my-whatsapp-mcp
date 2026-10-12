//go:build sqlite_fts5

package archive

import (
	"context"
	"testing"
)

func TestDeliveredHistoryProgressKeepsLegacyLedgerButNotInactiveQueue(t *testing.T) {
	s, receipt, e := rebuildFixture(t)
	ctx := context.Background()
	policy, err := s.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := RebuildRequest{ID: "progress-consumed", Generation: receipt.Generation, Key: e.Key, PolicyGeneration: policy.Generation, CreatedAt: s.now()}
	if err = rebuildRegisterTest(s, ctx, first); err != nil {
		t.Fatal(err)
	}
	e.RebuildRequestID = first.ID
	if result, err := s.ApplySourceEvent(ctx, e, policy.Generation); err != nil || result.Outcome != OutcomeApplied {
		t.Fatal("initial exact copy", result, err)
	}
	pending := SourceAnchor{Key: e.Key, OriginalSentAt: e.OriginalSentAt}
	pending.Key.Message = "progress-pending"
	queued := pending
	queued.Key.Message = "progress-unrequested"
	for _, anchor := range []SourceAnchor{pending, queued} {
		if err = s.RecordRebuildDiscovery(ctx, anchor); err != nil {
			t.Fatal(err)
		}
	}
	second := first
	second.ID = "progress-pending-request"
	second.Key = pending.Key
	if err = rebuildRegisterTest(s, ctx, second); err != nil {
		t.Fatal(err)
	}
	selected, err := s.RebuildProgress(ctx)
	if err != nil || selected.Queued != 1 || selected.Pending != 1 || selected.Accepted != 1 || selected.CopySpent != 2 {
		t.Fatal("selected-original progress changed", selected, err)
	}
	rows := rebuildRows(t, s, "rebuild_requests", "id,state,consumed_at,policy_generation")
	if _, err = s.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{Policy: RebuildHistoryDelivered, AcceptDeliveredSnapshot: true}); err != nil {
		t.Fatal(err)
	}
	admission, err := s.CaptureDeliveredHistoryAdmission(ctx)
	if err != nil || admission == nil {
		t.Fatal(err)
	}
	e.Key.Message = "progress-delivered"
	e.RebuildRequestID = ""
	e.DeliveredHistory = admission
	if result, err := s.ApplySourceEvent(ctx, e, policy.Generation); err != nil || result.Outcome != OutcomeApplied {
		t.Fatal("delivered body", result, err)
	}
	delivered, err := s.RebuildProgress(ctx)
	if err != nil || delivered.Strategy != RebuildHistoryDelivered || delivered.Queued != 0 || delivered.Pending != 1 || delivered.Accepted != 1 || delivered.CopySpent != 2 || delivered.DiscoverySpent != 0 || delivered.ReadableMessages != 2 || delivered.DeliveredAccepted != 1 || delivered.State != "running" {
		t.Fatal("delivered owner progress lost evidence or projected inactive queue", delivered, err)
	}
	if rows != rebuildRows(t, s, "rebuild_requests", "id,state,consumed_at,policy_generation") {
		t.Fatal("progress changed legacy ledger")
	}
}
