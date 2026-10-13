//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const pinnedHistoryProducer = "30954642f78a33171d4ba1de7ae987af0b42d159"

func TestAcceptedPartialHistoryPinnedExactTupleAndProducer(t *testing.T) {
	_, w := classifiedRecoveryFixture(t)
	w.Classification = AcceptedPartialHistoryGap
	w.AcceptUnknownHistoryGaps = true
	w.RuntimeBuildSHA = pinnedHistoryProducer
	w.Diagnostic = ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 177, StubPresent: true, StubKnown: true, Presence: 411}
	t.Run("exact_tuple", func(t *testing.T) {
		if _, _, err := canonicalAcceptedPartialHistoryWitness(w); err != nil {
			t.Fatal("exact pinned notice rejected", err)
		}
	})
	t.Run("known_route_rejected", func(t *testing.T) {
		v := w
		v.Classification = ClassifiedHistoryChangeUsername
		v.AcceptUnknownHistoryGaps = false
		if _, _, err := canonicalClassifiedHistoryWitness(v); err == nil {
			t.Fatal("known route accepted unresolved pinned gap")
		}
	})
	t.Run("bounded_conflicts", func(t *testing.T) {
		v := w
		v.SourceRevisionConflicts = 1
		if _, _, err := canonicalAcceptedPartialHistoryWitness(v); err != nil {
			t.Fatal("accepted pinned producer rejected", err)
		}
	})
	mutations := map[string]func(*ClassifiedHistoryWitness){
		"wrong_producer": func(v *ClassifiedHistoryWitness) { v.RuntimeBuildSHA = "eda63bbfbde526536685f40d6d74e6a2ef282214" },
		"wrong_diagnostic_pair": func(v *ClassifiedHistoryWitness) {
			v.Diagnostic.RawStub = 229
			v.Diagnostic.StubKnown = false
			v.Diagnostic.Presence = 155
		},
		"nearby176":          func(v *ClassifiedHistoryWitness) { v.Diagnostic.RawStub = 176 },
		"nearby178":          func(v *ClassifiedHistoryWitness) { v.Diagnostic.RawStub = 178 },
		"unknown":            func(v *ClassifiedHistoryWitness) { v.Diagnostic.StubKnown = false },
		"absent":             func(v *ClassifiedHistoryWitness) { v.Diagnostic.StubPresent = false },
		"presence":           func(v *ClassifiedHistoryWitness) { v.Diagnostic.Presence = 475 },
		"body_presence":      func(v *ClassifiedHistoryWitness) { v.Diagnostic.Presence |= 4 },
		"control_stage":      func(v *ClassifiedHistoryWitness) { v.Diagnostic.Stage = "protocol_metadata" },
		"event":              func(v *ClassifiedHistoryWitness) { v.Diagnostic.Event = "source_apply" },
		"unknown_acceptance": func(v *ClassifiedHistoryWitness) { v.AcceptUnknownHistoryGaps = false },
		"wrong_class":        func(v *ClassifiedHistoryWitness) { v.Classification = ClassifiedHistoryGroupParticipantChangeNumber },
		"unqualified_conflict": func(v *ClassifiedHistoryWitness) {
			v.SourceRevisionConflicts = 1
			v.RuntimeBuildSHA = strings.Repeat("f", 40)
		},
		"excess_conflicts": func(v *ClassifiedHistoryWitness) {
			v.SourceRevisionConflicts = 33
			v.RuntimeBuildSHA = pinnedHistoryProducer
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := w
			mutate(&changed)
			if _, _, err := canonicalAcceptedPartialHistoryWitness(changed); err == nil {
				t.Fatal("nonexact pinned witness accepted")
			}
		})
	}
}

func TestAcceptedPartialHistoryPinnedRecoveryPreservesDeliveredEvidence(t *testing.T) {
	ctx := context.Background()
	s, first := classifiedRecoveryFixture(t)
	if _, err := s.RestoreClassifiedHistorySnapshot(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{Policy: RebuildHistoryDelivered, AcceptDeliveredSnapshot: true}); err != nil {
		t.Fatal(err)
	}
	admission, err := s.CaptureDeliveredHistoryAdmission(ctx)
	if err != nil || admission == nil {
		t.Fatal("capture", err)
	}
	e := event("pinned-preserved-delivered", "preserved delivered body", 1, EventOriginal)
	e.DeliveredHistory = admission
	if result, err := s.ApplySourceEvent(ctx, e, admission.PolicyGeneration); err != nil || result.Outcome != OutcomeApplied {
		t.Fatal("body fixture", result, err)
	}
	if _, err := s.writeDB.Exec("UPDATE rebuild_operation SET last_attempt_at=0"); err != nil {
		t.Fatal(err)
	}
	page, err := s.NextDeliveredHistoryPage(ctx)
	if err != nil || page == nil {
		t.Fatal("page fixture", err)
	}
	if err := s.RegisterDeliveredHistoryPage(ctx, *page); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveDeliveredHistoryAnchor(ctx, *admission, page.Anchor); err != nil {
		t.Fatal(err)
	}
	second := nextNoticeWitness(t, s, first, 193, "c")
	if _, err := s.RestoreClassifiedHistorySnapshot(ctx, second); err != nil {
		t.Fatal(err)
	}
	third := nextNoticeWitness(t, s, second, 217, "d")
	if _, err := s.RestoreClassifiedHistorySnapshot(ctx, third); err != nil {
		t.Fatal(err)
	}
	fields := []string{"rebuild_history_selection", "rebuild_history_selection_audit", "rebuild_history_bodies", "rebuild_history_pages", "rebuild_history_frontiers", "rebuild_requests", "rebuild_operation", "rebuild_provenance", "rebuild_guards", "identity_aliases", "chat_aliases", "source_tombstones", "chat_tombstones", "unresolved_deletions"}
	retained := map[string]string{}
	for _, table := range fields {
		retained[table] = pinnedRecoveryRows(t, s, table)
	}
	unknownCodes := pinnedRecoveryRows(t, s, "unknown_history_codes")
	oldAudits := pinnedRecoveryRows(t, s, "snapshot_recovery_audit")
	before := snapshotRecoveryRows(t, s)
	fourth := nextNoticeWitness(t, s, third, 177, "e")
	fourth.Classification = AcceptedPartialHistoryGap
	fourth.AcceptUnknownHistoryGaps = true
	fourth.RuntimeBuildSHA = pinnedHistoryProducer
	fourth.SourceRevisionConflicts = 1
	for _, prior := range []ClassifiedHistoryWitness{first, second, third} {
		if _, err := s.RestoreClassifiedHistorySnapshot(ctx, prior); err == nil {
			t.Fatal("prior witness authorized pinned fault")
		}
	}
	s.snapshotRecoveryFault = func(stage string) error {
		if stage == "after_prepare" {
			return ErrArchiveUnavailable
		}
		return nil
	}
	if _, err := s.RestoreAcceptedPartialHistorySnapshot(ctx, fourth); err != ErrArchiveUnavailable {
		t.Fatal("pinned acceptance never prepared", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, s.privateDir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s = reopened
	changed := fourth
	changed.EvidenceSHA256 = strings.Repeat("f", 64)
	if _, err := s.RestoreAcceptedPartialHistorySnapshot(ctx, changed); err == nil {
		t.Fatal("changed witness resumed pinned fault")
	}
	if _, err := s.RestoreCommittedSnapshot(ctx, ""); err == nil {
		t.Fatal("generic recovery resumed pinned fault")
	}
	receipt, err := s.RestoreAcceptedPartialHistorySnapshot(ctx, fourth)
	if err != nil || receipt.RecoveryEpoch != 4 || receipt.Code != "owner_restored_snapshot" {
		t.Fatal("pinned recovery", receipt, err)
	}
	if retry, err := s.RestoreAcceptedPartialHistorySnapshot(ctx, fourth); err != nil || retry != receipt {
		t.Fatal("identical retry", retry, err)
	}
	if !reflect.DeepEqual(before, snapshotRecoveryRows(t, s)) {
		t.Fatal("committed body or authority changed")
	}
	for _, table := range fields {
		if retained[table] != pinnedRecoveryRows(t, s, table) {
			t.Fatal("retained evidence changed", table)
		}
	}
	if oldAudits != pinnedRecoveryRows(t, s, "snapshot_recovery_audit WHERE recovery_epoch<=3") {
		t.Fatal("prior audit bytes changed")
	}
	if unknownCodes != pinnedRecoveryRows(t, s, "unknown_history_codes") {
		t.Fatal("known177 was recorded as unknown raw code")
	}
	var read ReadSnapshot
	if err := unknownHistoryGapSnapshot(ctx, s.readDB, "account", &read); err != nil || !read.UnknownHistoryGap || !read.DeliveredHistorySnapshot {
		t.Fatal("sticky partial flags missing", read, err)
	}
	diagnostics, err := s.UnknownHistoryGapDiagnostics(ctx, "account")
	if err != nil || diagnostics.OwnerAcceptedShapeGaps != 1 || diagnostics.TotalObservations != 0 || len(diagnostics.Codes) != 0 {
		t.Fatal("accepted shape mislabeled as unknown raw observation", diagnostics, err)
	}
	if read.CoverageEpoch != admission.StrategyEpoch+receipt.RecoveryEpoch {
		t.Fatal("accepted epoch not pinned", read.CoverageEpoch)
	}
	testPinnedHistoricalTicket(t, s, receipt.RecoveryEpoch)
	var version int
	if err := s.writeDB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != 19 {
		t.Fatal("unexpected migration", version, err)
	}
}

func pinnedRecoveryRows(t *testing.T, s *Store, table string) string {
	t.Helper()
	rows, err := s.readDB.Query("SELECT * FROM " + table + " LIMIT 0")
	if err != nil {
		t.Fatal(err)
	}
	columns, err := rows.Columns()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	return rebuildRows(t, s, table, strings.Join(columns, ","))
}

func TestAcceptedPartialHistoryPinnedPreservesUnknownCountersAndRejectsForward(t *testing.T) {
	for _, priorUnknown := range []bool{false, true} {
		t.Run(fmt.Sprint(priorUnknown), func(t *testing.T) {
			s, w := classifiedRecoveryFixture(t)
			if priorUnknown {
				// The fixture is deliberately fenced; seed prior committed diagnostic rows.
				if _, err := s.writeDB.Exec("INSERT INTO unknown_history_coverage VALUES('account',1,1); INSERT INTO unknown_history_codes VALUES('account',229,1,1,1); INSERT INTO sync_gaps VALUES('account','','unknown_history_notice')"); err != nil {
					t.Fatal(err)
				}
			}
			before := acceptedPartialGapRows(t, s)
			w.Classification = AcceptedPartialHistoryGap
			w.AcceptUnknownHistoryGaps = true
			w.RuntimeBuildSHA = pinnedHistoryProducer
			w.Diagnostic = ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 177, StubPresent: true, StubKnown: true, Presence: 411}
			if _, err := s.RestoreAcceptedPartialHistorySnapshot(context.Background(), w); err != nil {
				t.Fatal(err)
			}
			if before != acceptedPartialGapRows(t, s) {
				t.Fatal("known177 changed undeclared raw counters")
			}
			diagnostics, err := s.UnknownHistoryGapDiagnostics(context.Background(), "account")
			expected := uint64(0)
			if priorUnknown {
				expected = 1
			}
			if err != nil || diagnostics.OwnerAcceptedShapeGaps != 1 || diagnostics.TotalObservations != expected {
				t.Fatal("owner diagnostics", diagnostics, err)
			}
			if _, _, err := forwardBootstrapTest(t, s); err != ErrSchemaChanged {
				t.Fatal("forward discarded accepted shape evidence", err)
			}
		})
	}
}

func testPinnedHistoricalTicket(t *testing.T, committed *Store, epoch uint64) {
	t.Helper()
	f := newHistoricalViewFixture(t)
	ctx := context.Background()
	scope := readerScope(f.policy, f.liveChats...)
	view := f.view(t)
	old, err := view.ForOwner(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	_, guard, err := old.SearchGuarded(ctx, scope, SearchRequest{Query: "historical canary"})
	if err != nil {
		t.Fatal(err)
	}
	// Transfer the actual canonical accepted audit into this isolated reader
	// fixture to model the atomic recovery commit after a ticket was issued.
	rows, err := committed.readDB.Query("SELECT * FROM snapshot_recovery_audit WHERE recovery_epoch=?", epoch)
	if err != nil {
		t.Fatal(err)
	}
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("missing committed audit")
	}
	values := make([]any, len(columns))
	refs := make([]any, len(columns))
	for i := range values {
		refs[i] = &values[i]
	}
	if err := rows.Scan(refs...); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",")
	if _, err := f.live.writeDB.Exec("INSERT INTO snapshot_recovery_audit("+strings.Join(columns, ",")+") VALUES("+placeholders+")", values...); err != nil {
		t.Fatal(err)
	}
	if _, err := f.live.writeDB.Exec("UPDATE archive_meta SET recovery_epoch=?; INSERT INTO sync_gaps VALUES('account','','owner_accepted_history_gap')", epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := old.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrCursorStale) {
		t.Fatal("historical ticket survived accepted live gap", err)
	}
	current, err := view.ForOwner(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	page, _, err := current.SearchGuarded(ctx, scope, SearchRequest{Query: "historical canary"})
	if err != nil || !page.Snapshot.UnknownHistoryGap || page.Snapshot.CoverageEpoch != 0 {
		t.Fatal("accepted live gap missing or replaced derivative epoch", page.Snapshot, err)
	}
}
