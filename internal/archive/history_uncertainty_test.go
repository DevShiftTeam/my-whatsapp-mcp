//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestHistoryUncertaintyPersistsAndInvalidatesEveryReadGuard(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	e := readerEvent("readable", "allowed", "original keyword", now.Add(-time.Minute))
	id := readerApply(t, s, p, e)
	_, media := attachmentFixture(t, s, p, "attachment", "allowed", "synthetic.png", "attachment", now.Add(-time.Minute))
	scope := readerScope(p, readerChat(t, s, id))
	ctx := context.Background()
	observation, err := s.BeginObservation(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	guards := []ResponseGuard{}
	for _, kind := range []string{"search", "context"} {
		_, g, _ := guardedBodyRead(t, s, scope, kind, e, id)
		guards = append(guards, g)
	}
	empty, g, err := s.SearchGuarded(ctx, scope, SearchRequest{Query: "absent"})
	if err != nil {
		t.Fatal(err)
	}
	guards = append(guards, g)
	_, g, err = s.ListChatsGuarded(ctx, scope, ChatListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	guards = append(guards, g)
	_, g, err = s.StatusGuarded(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	guards = append(guards, g)
	_, g, err = s.SearchAttachmentsGuarded(ctx, scope, AttachmentSearchRequest{Filename: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	guards = append(guards, g)
	_, g, err = s.AttachmentGuarded(ctx, scope, media)
	if err != nil {
		t.Fatal(err)
	}
	guards = append(guards, g)
	_, g, err = s.StatsGuarded(ctx, scope, StatsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	guards = append(guards, g)
	beforeRows := snapshotRecoveryRows(t, s)
	rawBefore := acceptedPartialGapRows(t, s)
	if err = s.RecordHistoryUncertainty(ctx, scope.Account, HistoryUncertaintyUnsupportedControl, observation); err != nil {
		t.Fatal("durable unknown gap rejected", err)
	}
	if !reflect.DeepEqual(beforeRows, snapshotRecoveryRows(t, s)) {
		t.Fatal("gap changed stored body or authority")
	}
	if acceptedPartialGapRows(t, s) != rawBefore {
		t.Fatal("generic gap changed raw undeclared counts")
	}
	for _, guard := range guards {
		if _, err = s.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrCursorStale) && !errors.Is(err, ErrPolicyChanged) {
			t.Fatal("old final guard survived gap", err)
		}
	}
	page, g, err := s.SearchGuarded(ctx, scope, SearchRequest{Query: "original keyword"})
	if err != nil || len(page.Hits) != 1 || !page.Snapshot.UnknownHistoryGap || page.Snapshot.CoverageEpoch != 1 || page.Snapshot.ArchiveRevision <= empty.Snapshot.ArchiveRevision {
		t.Fatal("partial readable snapshot missing", page, err)
	}
	if _, err = s.CheckResponse(ctx, scope, ResponseCheck{Guard: g}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetSyncStatus(ctx, SyncUpdate{Account: scope.Account, Connection: "connected"}); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx, scope)
	if err != nil || status.Coverage != "partial" || !status.Snapshot.UnknownHistoryGap || len(status.Chats) != 1 || status.Chats[0].Coverage != "partial" {
		t.Fatal("later sync cleared gap", status, err)
	}
	if err = s.RecordHistoryUncertainty(ctx, scope.Account, HistoryUncertaintyUnsupportedControl, observation); err != nil {
		t.Fatal(err)
	}
	if err = s.EndObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	dir := s.privateDir
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	diagnostics, err := s.HistoryUncertaintyDiagnostics(ctx, scope.Account)
	if err != nil || diagnostics.TotalObservations != 2 || diagnostics.Epoch != 2 || len(diagnostics.Reasons) != 1 || diagnostics.Reasons[0].Reason != HistoryUncertaintyUnsupportedControl || diagnostics.Reasons[0].Count != 2 {
		t.Fatal("gap diagnostics did not survive restart", diagnostics, err)
	}
	status, err = s.Status(ctx, scope)
	if err != nil || !status.Snapshot.UnknownHistoryGap {
		t.Fatal("gap lost on restart", err)
	}
}

func TestHistoryUncertaintyHistoricalLiveEpochIsSeparate(t *testing.T) {
	f := newHistoricalViewFixture(t)
	ctx := context.Background()
	observation, err := f.live.BeginObservation(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	scope := readerScope(f.policy, f.liveChats...)
	v := f.view(t)
	old, err := v.ForOwner(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	_, guard, err := old.SearchGuarded(ctx, scope, SearchRequest{Query: "historical canary"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.live.RecordHistoryUncertainty(ctx, "account", HistoryUncertaintyUnsupportedMetadata, observation); err != nil {
		t.Fatal(err)
	}
	if _, err = old.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrCursorStale) {
		t.Fatal("historical ticket survived live gap", err)
	}
	current, err := v.ForOwner(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	page, guard, err := current.SearchGuarded(ctx, scope, SearchRequest{Query: "historical canary"})
	if err != nil || !page.Snapshot.UnknownHistoryGap || page.Snapshot.CoverageEpoch != 0 {
		t.Fatal("live gap replaced derivative epoch or vanished", page.Snapshot, err)
	}
	chats, _, err := current.ListChatsGuarded(ctx, scope, ChatListRequest{})
	if err != nil || len(chats.Chats) == 0 {
		t.Fatal("historical chats", err)
	}
	for _, chat := range chats.Chats {
		if chat.Coverage != "partial" {
			t.Fatal("historical live gap not projected into chat coverage")
		}
	}
	snapshot, err := current.CheckResponse(ctx, scope, ResponseCheck{Guard: guard})
	if err != nil || !snapshot.UnknownHistoryGap || snapshot.CoverageEpoch != 0 {
		t.Fatal("historical final label missing live gap", snapshot, err)
	}
	if err = f.live.RecordHistoryUncertainty(ctx, "account", HistoryUncertaintyUnsupportedMetadata, observation); err != nil {
		t.Fatal(err)
	}
	if _, err = current.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrCursorStale) {
		t.Fatal("second live gap reused ticket", err)
	}
}

func TestHistoryUncertaintyRejectsStaleObservationWithoutPoisoningCurrent(t *testing.T) {
	s := testArchive(t)
	testPolicy(t, s)
	ctx := context.Background()
	old, err := s.BeginObservation(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EndObservation(ctx, old); err != nil {
		t.Fatal(err)
	}
	current, err := s.BeginObservation(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	before := pinnedRecoveryRows(t, s, "archive_meta") + pinnedRecoveryRows(t, s, "account_safety")
	if err = s.RecordHistoryUncertainty(ctx, "account", HistoryUncertaintyUnsupportedNotice, old); err != errHistoryObservationChanged {
		t.Fatal("stale token not rejected", err)
	}
	if after := pinnedRecoveryRows(t, s, "archive_meta") + pinnedRecoveryRows(t, s, "account_safety"); after != before {
		t.Fatal("stale caller changed current generation")
	}
	diagnostics, err := s.HistoryUncertaintyDiagnostics(ctx, "account")
	if err != nil || diagnostics.Epoch != 0 || diagnostics.TotalObservations != 0 {
		t.Fatal("stale caller changed ledger", diagnostics, err)
	}
	if err = s.RecordHistoryUncertainty(ctx, "account", HistoryUncertaintyUnsupportedNotice, current); err != nil {
		t.Fatal("stale caller poisoned current observation", err)
	}
}

func TestHistoryUncertaintyReasonsAreFixedAndCountsRemainSeparate(t *testing.T) {
	s := testArchive(t)
	testPolicy(t, s)
	ctx := context.Background()
	observation, err := s.BeginObservation(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []HistoryUncertaintyReason{"", "undeclared_229", "unsupported_control/identity", "arbitrary_future"} {
		if err := s.RecordHistoryUncertainty(ctx, "account", reason, observation); err != ErrInvalidEvent {
			t.Fatal("arbitrary reason accepted", reason, err)
		}
	}
	for i := 0; i < 40; i++ {
		for _, reason := range []HistoryUncertaintyReason{HistoryUncertaintyUnsupportedMetadata, HistoryUncertaintyUnsupportedNotice, HistoryUncertaintyUnsupportedControl} {
			if err := s.RecordHistoryUncertainty(ctx, "account", reason, observation); err != nil {
				t.Fatal(err)
			}
		}
	}
	d, err := s.HistoryUncertaintyDiagnostics(ctx, "account")
	if err != nil || d.Epoch != 120 || d.TotalObservations != 120 || len(d.Reasons) != 3 {
		t.Fatal("fixed reason bounds/counts", d, err)
	}
	for _, v := range d.Reasons {
		if v.Count != 40 {
			t.Fatal("wrong reason count", v)
		}
	}
	raw, err := s.UnknownHistoryGapDiagnostics(ctx, "account")
	if err != nil || raw.TotalObservations != 0 || len(raw.Codes) != 0 || raw.OwnerAcceptedShapeGaps != 0 {
		t.Fatal("generic counts leaked into another basis", raw, err)
	}
}

func TestHistoryUncertaintyPersistenceFailureRollsBackAndFences(t *testing.T) {
	s := testArchive(t)
	testPolicy(t, s)
	ctx := context.Background()
	observation, err := s.BeginObservation(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec("CREATE TRIGGER reject_generic_gap BEFORE INSERT ON history_uncertainty_reasons BEGIN SELECT RAISE(ABORT,'synthetic failure'); END"); err != nil {
		t.Fatal(err)
	}
	before := snapshotRecoveryRows(t, s)
	if err := s.RecordHistoryUncertainty(ctx, "account", HistoryUncertaintyUnsupportedControl, observation); err == nil {
		t.Fatal("failed commit reported success")
	}
	var rows int
	if err := s.readDB.QueryRow("SELECT (SELECT count(*) FROM history_uncertainty_coverage)+(SELECT count(*) FROM history_uncertainty_reasons)+(SELECT count(*) FROM sync_gaps WHERE code='unsupported_history_semantics')").Scan(&rows); err != nil || rows != 0 {
		t.Fatal("partial uncertainty commit", rows, err)
	}
	if !reflect.DeepEqual(before, snapshotRecoveryRows(t, s)) {
		t.Fatal("failed gap altered body/authority")
	}
	s.safetyMu.RLock()
	denied := s.unsafe["account"]
	s.safetyMu.RUnlock()
	if !denied {
		t.Fatal("failed durable gap did not fence")
	}
}

func TestHistoryUncertaintyArithmeticOverflowIsAtomic(t *testing.T) {
	for _, kind := range []string{"epoch", "total", "reason", "combined"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _ := deliveredWriterFixture(t)
			ctx := context.Background()
			observation, err := s.BeginObservation(ctx, "account")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.writeDB.Exec("INSERT INTO history_uncertainty_coverage VALUES('account',1,1); INSERT INTO history_uncertainty_reasons VALUES('account','unsupported_notice',1,1,1)"); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "epoch":
				_, err = s.writeDB.Exec("UPDATE history_uncertainty_coverage SET epoch=?", int64(math.MaxInt64))
			case "total":
				_, err = s.writeDB.Exec("UPDATE history_uncertainty_coverage SET total_observations=?", int64(math.MaxInt64))
			case "reason":
				_, err = s.writeDB.Exec("UPDATE history_uncertainty_reasons SET count=?", int64(math.MaxInt64))
			case "combined":
				_, err = s.writeDB.Exec("INSERT INTO unknown_history_coverage VALUES('account',?,1); INSERT INTO unknown_history_codes VALUES('account',229,1,1,1); UPDATE rebuild_history_selection SET epoch=?", int64(math.MaxInt64), int64(math.MaxInt64))
			}
			if err != nil {
				t.Fatal(err)
			}
			before := pinnedRecoveryRows(t, s, "history_uncertainty_coverage") + pinnedRecoveryRows(t, s, "history_uncertainty_reasons")
			if err := s.RecordHistoryUncertainty(ctx, "account", HistoryUncertaintyUnsupportedNotice, observation); err == nil {
				t.Fatal("overflow committed")
			}
			if after := pinnedRecoveryRows(t, s, "history_uncertainty_coverage") + pinnedRecoveryRows(t, s, "history_uncertainty_reasons"); after != before {
				t.Fatal("overflow partially mutated counts")
			}
		})
	}
}

func TestHistoryUncertaintyForwardRejectsEvidenceLoss(t *testing.T) {
	s, _, _ := rebuildFixture(t)
	ctx := context.Background()
	observation, err := s.BeginObservation(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordHistoryUncertainty(ctx, "account", HistoryUncertaintyUnsupportedMetadata, observation); err != nil {
		t.Fatal(err)
	}
	if err = s.EndObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	if _, _, err := forwardBootstrapTest(t, s); err != ErrSchemaChanged {
		t.Fatal("forward discarded generic uncertainty", err)
	}
}

func TestHistoryUncertaintyCanceledStaleObservationDoesNotPoisonCurrent(t *testing.T) {
	s := testArchive(t)
	testPolicy(t, s)
	ctx := context.Background()
	old, err := s.BeginObservation(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EndObservation(ctx, old); err != nil {
		t.Fatal(err)
	}
	current, err := s.BeginObservation(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	before := pinnedRecoveryRows(t, s, "archive_meta") + pinnedRecoveryRows(t, s, "account_safety")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	err = s.RecordHistoryUncertainty(canceled, "account", HistoryUncertaintyUnsupportedNotice, old)
	if err != errHistoryObservationChanged {
		t.Errorf("stale canceled callback did not reject stale token before admission: %v", err)
	}
	if after := pinnedRecoveryRows(t, s, "archive_meta") + pinnedRecoveryRows(t, s, "account_safety"); after != before {
		t.Error("stale canceled caller changed current generation")
	}
	if err = s.RecordHistoryUncertainty(ctx, "account", HistoryUncertaintyUnsupportedNotice, current); err != nil {
		t.Fatal("stale canceled callback poisoned current observation", err)
	}
	d, err := s.HistoryUncertaintyDiagnostics(ctx, "account")
	if err != nil || d.TotalObservations != 1 {
		t.Fatal("unexpected current ledger", d, err)
	}
}

func TestHistoryUncertaintyObservationChangesBetweenCheckAndAdmission(t *testing.T) {
	s := testArchive(t)
	testPolicy(t, s)
	ctx := context.Background()
	old, err := s.BeginObservation(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	var current ObservationID
	command := historyUncertaintyCommand{"account", HistoryUncertaintyUnsupportedControl, old}
	err = s.admitHistoryUncertainty(command, func() error {
		if err := s.EndObservation(ctx, old); err != nil {
			t.Fatal(err)
		}
		var err error
		current, err = s.BeginObservation(ctx, "account")
		if err != nil {
			t.Fatal(err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = s.submit(canceled, command, "account", true, 0)
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("expected canceled admission", err)
	}
	if err = s.RecordHistoryUncertainty(ctx, "account", HistoryUncertaintyUnsupportedControl, current); err != nil {
		t.Fatal("old admission failure poisoned new observation", err)
	}
	d, err := s.HistoryUncertaintyDiagnostics(ctx, "account")
	if err != nil || d.TotalObservations != 1 {
		t.Fatal("wrong current ledger", d, err)
	}
}

func TestHistoryUncertaintyGenericQueueDoesNotApplyUnboundFailureFence(t *testing.T) {
	if commandSourceSensitive(historyUncertaintyCommand{}, true) {
		t.Fatal("uncertainty command uses unbound generic admission/wait/writer failure fence")
	}
}
