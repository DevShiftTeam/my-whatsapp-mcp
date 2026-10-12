//go:build sqlite_fts5

package archive

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func deliveredWriterFixture(t *testing.T) (*Store, DeliveredHistoryAdmission, SourceEvent) {
	t.Helper()
	s, receipt, e := rebuildFixture(t)
	selection, err := s.SelectRebuildHistoryPolicy(context.Background(), RebuildHistorySelectionOptions{Policy: RebuildHistoryDelivered, AcceptDeliveredSnapshot: true})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Policy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s, DeliveredHistoryAdmission{StrategyEpoch: selection.StrategyEpoch, ObservationGeneration: receipt.Generation, PolicyGeneration: p.Generation}, e
}

func TestDeliveredHistoryWriterAcceptsFiftyBodiesWithoutConsumingExactRequests(t *testing.T) {
	s, admission, base := deliveredWriterFixture(t)
	ctx := context.Background()
	beforeRequests := rebuildRows(t, s, "rebuild_requests", "id,generation,state,consumed_at,operation_id,operation_revision")
	beforeProvenance := rebuildRows(t, s, "rebuild_provenance", "generation,receipt")
	for i := 0; i < 50; i++ {
		e := base
		e.Key.Message = fmt.Sprintf("delivered-%02d", i)
		e.Body = fmt.Sprintf("delivered batch body %02d", i)
		e.DeliveredHistory = &admission
		result, err := s.ApplySourceEvent(ctx, e, admission.PolicyGeneration)
		if err != nil || result.Outcome != OutcomeApplied {
			t.Fatalf("delivered body %d not admitted: %v %v", i, result.Outcome, err)
		}
	}
	var count int
	if err := s.readDB.QueryRow("SELECT count(*) FROM messages WHERE state='active'").Scan(&count); err != nil || count != 50 {
		t.Fatal("fifty stored bodies missing", count, err)
	}
	if got := rebuildRows(t, s, "rebuild_requests", "id,generation,state,consumed_at,operation_id,operation_revision"); got != beforeRequests {
		t.Fatal("delivered import changed exact requests")
	}
	if got := rebuildRows(t, s, "rebuild_provenance", "generation,receipt"); got != beforeProvenance {
		t.Fatal("delivered import changed provenance")
	}
	progress, err := s.RebuildProgress(ctx)
	if err != nil || progress.DeliveredAccepted != 50 || progress.ReadableMessages != 50 {
		t.Fatal("delivered progress missing", progress, err)
	}
	e := base
	e.Key.Message = "delivered-00"
	e.Body = "delivered batch body 00"
	e.DeliveredHistory = &admission
	if _, err = s.ApplySourceEvent(ctx, e, admission.PolicyGeneration); err != nil {
		t.Fatal(err)
	}
	next, err := s.RebuildProgress(ctx)
	if err != nil || next.DeliveredAccepted != 50 {
		t.Fatal("duplicate used another credit", next, err)
	}
}

func TestDeliveredHistoryWriterRejectsMissingMixedAndStaleProof(t *testing.T) {
	for _, kind := range []string{"missing", "mixed", "stale_epoch", "stale_observation", "stale_policy"} {
		t.Run(kind, func(t *testing.T) {
			s, admission, e := deliveredWriterFixture(t)
			ctx := context.Background()
			e.Key.Message = "rejected-" + kind
			e.DeliveredHistory = &admission
			switch kind {
			case "missing":
				e.DeliveredHistory = nil
			case "mixed":
				e.RebuildRequestID = "not-a-request"
			case "stale_epoch":
				admission.StrategyEpoch++
			case "stale_observation":
				admission.ObservationGeneration = "00000000000000000000000000000000"
			case "stale_policy":
				admission.PolicyGeneration++
			}
			before := snapshotRecoveryRows(t, s)
			result, err := s.ApplySourceEvent(ctx, e, admission.PolicyGeneration)
			if err != nil || result.Outcome == OutcomeApplied {
				t.Fatal("invalid positive proof not filtered", result, err)
			}
			if !reflect.DeepEqual(before, snapshotRecoveryRows(t, s)) {
				t.Fatal("filtered positive changed bodies")
			}
		})
	}
}

func TestDeliveredHistorySchema18ReadsSnapshotButRejectsForwardEvidence(t *testing.T) {
	s, _, _ := deliveredWriterFixture(t)
	ctx := context.Background()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := rebuildFileHash(filepath.Join(s.privateDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := OpenHistoricalSnapshot(ctx, s.privateDir)
	if err != nil {
		t.Fatal("populated delivered snapshot rejected", err)
	}
	var read ReadSnapshot
	if err = unknownHistoryGapSnapshot(ctx, snapshot.readDB, "account", &read); err != nil || !read.DeliveredHistorySnapshot || read.UnknownHistoryGap {
		t.Fatal("sticky delivered label missing or fabricated unknown gap", read, err)
	}
	if err = snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := rebuildFileHash(filepath.Join(s.privateDir, "archive.sqlite"))
	if err != nil || before != after {
		t.Fatal("snapshot changed physical bytes", err)
	}
	_, _, err = forwardBootstrapTest(t, s)
	if err == nil {
		t.Fatal("forward discarded delivered evidence")
	}
}

func TestDeliveredHistorySelectionLifetimeRestartAndSwitchback(t *testing.T) {
	s, admission, e := deliveredWriterFixture(t)
	ctx := context.Background()
	before := rebuildRows(t, s, "rebuild_history_selection_audit", "epoch,command_id,mode")
	if _, err := s.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{Policy: RebuildHistorySelectedOriginal}); err == nil {
		t.Fatal("runtime mode switched")
	}
	if _, err := s.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{Policy: RebuildHistoryDelivered, AcceptDeliveredSnapshot: true}); err != nil {
		t.Fatal(err)
	}
	if rebuildRows(t, s, "rebuild_history_selection_audit", "epoch,command_id,mode") != before {
		t.Fatal("same selection duplicated audit")
	}
	e.DeliveredHistory = &admission
	if _, err := s.ApplySourceEvent(ctx, e, admission.PolicyGeneration); err != nil {
		t.Fatal(err)
	}
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err = reopened.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{}); err == nil {
		t.Fatal("omitted selection silently switched persisted mode")
	}
	selected, err := reopened.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{Policy: RebuildHistorySelectedOriginal})
	if err != nil || selected.StrategyEpoch != admission.StrategyEpoch+1 {
		t.Fatal("explicit switchback", selected, err)
	}
	var snapshot ReadSnapshot
	if err = unknownHistoryGapSnapshot(ctx, reopened.readDB, "account", &snapshot); err != nil || !snapshot.DeliveredHistorySnapshot || snapshot.UnknownHistoryGap {
		t.Fatal("switchback lost or fabricated sticky coverage", snapshot, err)
	}
	progress, err := reopened.RebuildProgress(ctx)
	if err != nil || progress.DeliveredAccepted != 1 || progress.DeliveredLimit != DefaultDeliveredHistoryLimit {
		t.Fatal("restart reset credit", progress, err)
	}
	oldRows := snapshotRecoveryRows(t, reopened)
	e.Key.Message = "late-chunk"
	result, err := reopened.ApplySourceEvent(ctx, e, admission.PolicyGeneration)
	if err != nil || result.Outcome == OutcomeApplied || !reflect.DeepEqual(oldRows, snapshotRecoveryRows(t, reopened)) {
		t.Fatal("old chunk survived switchback", result, err)
	}
}

func TestDeliveredHistoryNegativesWinWhenPausedStaleOrExhausted(t *testing.T) {
	for _, mode := range []string{"paused", "exhausted", "stale_policy"} {
		t.Run(mode, func(t *testing.T) {
			s, a, e := deliveredWriterFixture(t)
			ctx := context.Background()
			e.DeliveredHistory = &a
			if _, err := s.ApplySourceEvent(ctx, e, a.PolicyGeneration); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "paused":
				if _, err := s.writeDB.Exec("UPDATE rebuild_operation SET state='paused'"); err != nil {
					t.Fatal(err)
				}
			case "exhausted":
				if _, err := s.writeDB.Exec("UPDATE rebuild_history_selection SET body_spent=body_limit"); err != nil {
					t.Fatal(err)
				}
			case "stale_policy":
				a.PolicyGeneration++
			}
			e.Key.Message = "rejected-positive"
			e.ExpiresAt = s.now().Add(time.Hour)
			result, err := s.ApplySourceEvent(ctx, e, a.PolicyGeneration)
			if err != nil || result.Outcome == OutcomeApplied {
				t.Fatal("positive admitted", result, err)
			}
			guard, err := s.RebuildGuard(ctx, e.Key)
			if err != nil || !guard.ExpiresAt.Equal(e.ExpiresAt.Truncate(time.Millisecond)) {
				t.Fatal("filtered positive lost expiry", guard, err)
			}
			e.Kind = EventDelete
			e.Key.Message = "old"
			e.DeliveredHistory = nil
			e.Body = ""
			p, _ := s.Policy(ctx)
			if _, err = s.ApplySourceEvent(ctx, e, p.Generation); err != nil {
				t.Fatal("suffix delete rejected", err)
			}
			var active int
			if err = s.readDB.QueryRow("SELECT count(*) FROM messages WHERE state='active'").Scan(&active); err != nil || active != 0 {
				t.Fatal("negative did not remove body", active, err)
			}
		})
	}
}

func TestDeliveredHistoryPostBoundaryProofAndOrdinaryLiveCredits(t *testing.T) {
	s, a, e := deliveredWriterFixture(t)
	ctx := context.Background()
	e.Key.Message = "post-boundary"
	e.OriginalSentAt = s.now().Add(time.Minute)
	e.DeliveredHistory = &a
	a.StrategyEpoch++
	r, err := s.ApplySourceEvent(ctx, e, a.PolicyGeneration)
	if err != nil || r.Outcome == OutcomeApplied {
		t.Fatal("postboundary shortcut accepted forged epoch", r, err)
	}
	e.DeliveredHistory = nil
	r, err = s.ApplySourceEvent(ctx, e, a.PolicyGeneration)
	if err != nil || r.Outcome != OutcomeApplied {
		t.Fatal("ordinary live path changed", r, err)
	}
	p, err := s.RebuildProgress(ctx)
	if err != nil || p.DeliveredAccepted != 0 {
		t.Fatal("live event spent delivered credit", p, err)
	}
}

func TestDeliveredHistoryExtendCapIsAtomicAndReplayDoesNotRefill(t *testing.T) {
	s, _, _ := deliveredWriterFixture(t)
	ctx := context.Background()
	p, err := s.RebuildProgress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	command := RebuildControl{ExpectedRevision: p.Revision, CommandID: "delivered-extend", Action: "extend"}
	next, err := s.ControlRebuild(ctx, command)
	if err != nil || next.DeliveredLimit != 2*DefaultDeliveredHistoryLimit {
		t.Fatal("body extension missing", next, err)
	}
	repeat, err := s.ControlRebuild(ctx, command)
	if err != nil || repeat.DeliveredLimit != next.DeliveredLimit || repeat.Revision != next.Revision {
		t.Fatal("replay refilled", repeat, err)
	}
	if _, err = s.writeDB.Exec("UPDATE rebuild_history_selection SET body_limit=?", maxRebuildCopyLimit); err != nil {
		t.Fatal(err)
	}
	before := rebuildRows(t, s, "rebuild_operation", "revision,copy_limit,copy_spent,discovery_limit,discovery_spent")
	_, err = s.ControlRebuild(ctx, RebuildControl{ExpectedRevision: next.Revision, CommandID: "cap-extension", Action: "extend"})
	if err != ErrLimitExceeded {
		t.Fatal("delivered cap silently extended other credits", err)
	}
	if before != rebuildRows(t, s, "rebuild_operation", "revision,copy_limit,copy_spent,discovery_limit,discovery_spent") {
		t.Fatal("failed extension partially changed credits")
	}
}

func TestDeliveredHistoryUnchangedSelectionAllowsFencedDiagnostics(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		t.Run(fmt.Sprint(delivered), func(t *testing.T) {
			s, _, _ := rebuildFixture(t)
			ctx := context.Background()
			if delivered {
				if _, err := s.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{Policy: RebuildHistoryDelivered, AcceptDeliveredSnapshot: true}); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.MarkUnsafe(ctx, "account", UnsafeReason("history_corrupt")); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			next, err := Open(ctx, s.privateDir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			opts := RebuildHistorySelectionOptions{}
			if delivered {
				opts.Policy = RebuildHistoryDelivered
				opts.AcceptDeliveredSnapshot = true
			}
			before := rebuildRows(t, next, "rebuild_history_selection_audit", "epoch,command_id,mode")
			if _, err = next.SelectRebuildHistoryPolicy(ctx, opts); err != nil {
				t.Fatal("unchanged fenced startup lost diagnostics", err)
			}
			if before != rebuildRows(t, next, "rebuild_history_selection_audit", "epoch,command_id,mode") {
				t.Fatal("unchanged fenced selection wrote audit")
			}
			if _, err = next.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{Policy: RebuildHistoryDelivered, AcceptDeliveredSnapshot: true}); !delivered && err == nil {
				t.Fatal("fenced store changed mode")
			}
		})
	}
}

func TestDeliveredHistoryProgressCountsOnlyReadableKnownDates(t *testing.T) {
	for _, mode := range []string{"unknown_date", "rolling_floor", "expired"} {
		t.Run(mode, func(t *testing.T) {
			s, a, e := deliveredWriterFixture(t)
			ctx := context.Background()
			e.DeliveredHistory = &a
			if _, err := s.ApplySourceEvent(ctx, e, a.PolicyGeneration); err != nil {
				t.Fatal(err)
			}
			var err error
			switch mode {
			case "unknown_date":
				_, err = s.writeDB.Exec("UPDATE messages SET original_date_known=0")
			case "expired":
				_, err = s.writeDB.Exec("UPDATE messages SET expires_at=?", unixms(s.now().Add(-time.Second)))
			case "rolling_floor":
				now := s.now().Truncate(time.Millisecond).Add(500 * time.Microsecond)
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, openErr := Open(ctx, s.privateDir, Options{Now: func() time.Time { return now }})
				if openErr != nil {
					t.Fatal(openErr)
				}
				defer reopened.Close()
				s = reopened
				if _, err = s.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{Policy: RebuildHistoryDelivered, AcceptDeliveredSnapshot: true}); err != nil {
					t.Fatal(err)
				}
				_, err = s.writeDB.Exec("UPDATE archive_policy SET rolling_retention=?", int64(time.Hour))
				if err == nil {
					_, err = s.writeDB.Exec("UPDATE messages SET original_sent_at=?", unixms(now.Add(-time.Hour)))
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			p, err := s.RebuildProgress(ctx)
			if err != nil || p.ReadableMessages != 0 || !p.DeliveredOldest.IsZero() {
				t.Fatal("nonreadable row inflated progress", p, err)
			}
		})
	}
}

func TestDeliveredHistoryKeepsPreviouslyPendingExactRequestsInactive(t *testing.T) {
	s, receipt, e := rebuildFixture(t)
	ctx := context.Background()
	p, err := s.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := RebuildRequest{ID: "pending-before-delivered", Generation: receipt.Generation, Key: e.Key, PolicyGeneration: p.Generation, CreatedAt: s.now()}
	if err = rebuildRegisterTest(s, ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{Policy: RebuildHistoryDelivered, AcceptDeliveredSnapshot: true}); err != nil {
		t.Fatal(err)
	}
	before := rebuildRows(t, s, "rebuild_requests", "id,generation,state,consumed_at,operation_id,operation_revision")
	spent := rebuildRows(t, s, "rebuild_operation", "copy_spent,discovery_spent")
	e.RebuildRequestID = request.ID
	r, err := s.ApplySourceEvent(ctx, e, p.Generation)
	if err != nil || r.Outcome == OutcomeApplied {
		t.Fatal("inactive pending exact request admitted", r, err)
	}
	if before != rebuildRows(t, s, "rebuild_requests", "id,generation,state,consumed_at,operation_id,operation_revision") || spent != rebuildRows(t, s, "rebuild_operation", "copy_spent,discovery_spent") {
		t.Fatal("inactive ledger or spend changed")
	}
	var active int
	if err = s.readDB.QueryRow("SELECT count(*) FROM messages WHERE state='active'").Scan(&active); err != nil || active != 0 {
		t.Fatal("inactive request body retained", active, err)
	}
}

func TestDeliveredHistoryDurableModeRejectsExactRequestsBeforeStartupSelection(t *testing.T) {
	for _, action := range []string{"match", "apply", "register"} {
		t.Run(action, func(t *testing.T) {
			s, receipt, e := rebuildFixture(t)
			ctx := context.Background()
			p, err := s.Policy(ctx)
			if err != nil {
				t.Fatal(err)
			}
			original := RebuildRequest{ID: "durable-pending", Generation: receipt.Generation, Key: e.Key, PolicyGeneration: p.Generation, CreatedAt: s.now()}
			if err = rebuildRegisterTest(s, ctx, original); err != nil {
				t.Fatal(err)
			}
			candidate := SourceAnchor{Key: e.Key, OriginalSentAt: e.OriginalSentAt}
			candidate.Key.Message = "never-requested-candidate"
			if err = s.RecordRebuildDiscovery(ctx, candidate); err != nil {
				t.Fatal(err)
			}
			if _, err = s.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{Policy: RebuildHistoryDelivered, AcceptDeliveredSnapshot: true}); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, s.privateDir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			before := rebuildRows(t, reopened, "rebuild_requests", "id,generation,state,consumed_at,operation_id,operation_revision")
			switch action {
			case "match":
				ok, err := reopened.MatchRebuildRequest(ctx, original.ID, e.Key, receipt.Generation, p.Generation)
				if err != nil || ok {
					t.Fatal("persisted delivered mode matched exact request before selection", ok, err)
				}
			case "apply":
				e.RebuildRequestID = original.ID
				r, err := reopened.ApplySourceEvent(ctx, e, p.Generation)
				if err != nil || r.Outcome == OutcomeApplied {
					t.Fatal("persisted delivered mode admitted exact request before selection", r, err)
				}
			case "register":
				original.ID = "durable-new-request"
				original.Key = candidate.Key
				if err := rebuildRegisterTest(reopened, ctx, original); err == nil {
					t.Fatal("persisted delivered mode registered exact request before selection")
				}
			}
			if before != rebuildRows(t, reopened, "rebuild_requests", "id,generation,state,consumed_at,operation_id,operation_revision") {
				t.Fatal("inactive durable ledger changed before startup selection")
			}
		})
	}
}
