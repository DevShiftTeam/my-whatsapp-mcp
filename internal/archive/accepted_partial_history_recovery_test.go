//go:build sqlite_fts5

package archive

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
)

func acceptedPartialRecoveryFixture(t *testing.T) (*Store, ClassifiedHistoryWitness) {
	t.Helper()
	s, w := classifiedRecoveryFixture(t)
	w.Classification = AcceptedPartialHistoryGap
	w.AcceptUnknownHistoryGaps = true
	w.RuntimeBuildSHA = "eda63bbfbde526536685f40d6d74e6a2ef282214"
	w.Diagnostic = ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 229, StubPresent: true, StubKnown: false, Presence: 155}
	w.SourceRevisionConflicts = 3
	return s, w
}

func acceptedPartialGapRows(t *testing.T, s *Store) string {
	t.Helper()
	return rebuildRows(t, s, "unknown_history_coverage", "account,epoch,total_observations") + rebuildRows(t, s, "unknown_history_codes", "account,raw_stub,count,first_seen,last_seen")
}

func TestAcceptedPartialHistoryRecoveryPreservesStateAndRecordsUncertainty(t *testing.T) {
	s, w := acceptedPartialRecoveryFixture(t)
	before := snapshotRecoveryRows(t, s)
	const evidenceColumns = "operation_id,generation,revision,copy_limit,copy_spent,discovery_limit,discovery_spent"
	evidence := rebuildRows(t, s, "rebuild_operation", evidenceColumns)
	receipt, err := s.RestoreAcceptedPartialHistorySnapshot(context.Background(), w)
	if err != nil {
		t.Fatal("explicit partial acceptance rejected:", err)
	}
	if receipt.Mode != "committed_snapshot" || receipt.RecoveryEpoch != w.RecoveryEpoch+1 || uint64(receipt.ArchiveRevision) != w.ArchiveRevision+1 {
		t.Fatal("unexpected receipt", receipt)
	}
	if !reflect.DeepEqual(before, snapshotRecoveryRows(t, s)) || evidence != rebuildRows(t, s, "rebuild_operation", evidenceColumns) {
		t.Fatal("acceptance changed authority, committed bodies or operation spend")
	}
	var epoch, total, count, gaps, unknown int
	if err = s.readDB.QueryRow("SELECT epoch,total_observations FROM unknown_history_coverage WHERE account='account'").Scan(&epoch, &total); err != nil || epoch != 1 || total != 1 {
		t.Fatal("missing global uncertainty", epoch, total, err)
	}
	if err = s.readDB.QueryRow("SELECT count FROM unknown_history_codes WHERE account='account' AND raw_stub=229").Scan(&count); err != nil || count != 1 {
		t.Fatal("missing owner-only code evidence", count, err)
	}
	if err = s.readDB.QueryRow("SELECT count(*) FROM sync_gaps WHERE account='account' AND chat='' AND code='unknown_history_notice'").Scan(&gaps); err != nil || gaps != 1 {
		t.Fatal("missing generic global warning", gaps, err)
	}
	if err = s.readDB.QueryRow("SELECT coverage_unknown FROM sync_state WHERE account='account' AND chat=''").Scan(&unknown); err != nil || unknown != 1 {
		t.Fatal("coverage incorrectly complete", unknown, err)
	}
	var classification, witness string
	if err = s.readDB.QueryRow("SELECT classification,witness_json FROM snapshot_recovery_audit WHERE recovery_epoch=?", receipt.RecoveryEpoch).Scan(&classification, &witness); err != nil || classification != AcceptedPartialHistoryGap {
		t.Fatal("wrong audit classification", classification, err)
	}
	var recorded ClassifiedHistoryWitness
	if json.Unmarshal([]byte(witness), &recorded) != nil || recorded != w || recorded.Diagnostic.StubKnown {
		t.Fatal("audit upgraded unknown diagnostic")
	}
	gapBefore := acceptedPartialGapRows(t, s)
	if again, err := s.RestoreAcceptedPartialHistorySnapshot(context.Background(), w); err != nil || again != receipt || gapBefore != acceptedPartialGapRows(t, s) {
		t.Fatal("identical retry recounted or changed receipt", again, err)
	}
	if _, err = s.RestoreCommittedSnapshot(context.Background(), ""); err == nil {
		t.Fatal("generic recovery replayed explicit acceptance")
	}
	if _, err = s.RestoreClassifiedHistorySnapshot(context.Background(), w); err == nil {
		t.Fatal("known-benign recovery admitted unknown acceptance")
	}
}

func TestAcceptedPartialHistoryRecoveryCrashRetryCountsOnce(t *testing.T) {
	for _, stage := range []string{"after_prepare", "before_observation_remove", "before_unsafe_remove", "before_directory_sync", "before_complete"} {
		t.Run(stage, func(t *testing.T) {
			s, w := acceptedPartialRecoveryFixture(t)
			s.snapshotRecoveryFault = func(current string) error {
				if current == stage {
					return ErrArchiveUnavailable
				}
				return nil
			}
			if _, err := s.RestoreAcceptedPartialHistorySnapshot(context.Background(), w); err != ErrArchiveUnavailable {
				t.Fatal("missing injected prepare failure", err)
			}
			gapBefore := acceptedPartialGapRows(t, s)
			dir := s.privateDir
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(context.Background(), dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			changed := w
			changed.OperatorReceiptSHA256 = strings.Repeat("f", 64)
			if _, err = reopened.RestoreAcceptedPartialHistorySnapshot(context.Background(), changed); err == nil {
				t.Fatal("changed witness resumed")
			}
			if _, err = reopened.RestoreCommittedSnapshot(context.Background(), ""); err == nil {
				t.Fatal("generic recovery resumed explicit prepare")
			}
			if _, err = reopened.RestoreAcceptedPartialHistorySnapshot(context.Background(), w); err != nil {
				t.Fatal("identical witness failed", err)
			}
			if gapBefore != acceptedPartialGapRows(t, reopened) {
				t.Fatal("retry counted uncertainty twice")
			}
			if err = reopened.MarkUnsafe(context.Background(), "account", "history_corrupt"); err != nil {
				t.Fatal(err)
			}
			if _, err = reopened.RestoreAcceptedPartialHistorySnapshot(context.Background(), w); err == nil {
				t.Fatal("stale witness cleared later failure")
			}
		})
	}
}

func TestAcceptedPartialHistoryRecoveryRejectsLegacyOrAuthorityDrift(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*ClassifiedHistoryWitness)
	}{
		{"acceptance", func(w *ClassifiedHistoryWitness) { w.AcceptUnknownHistoryGaps = false }},
		{"classification", func(w *ClassifiedHistoryWitness) { w.Classification = ClassifiedHistoryChangeUsername }},
		{"producer", func(w *ClassifiedHistoryWitness) { w.RuntimeBuildSHA = strings.Repeat("b", 40) }},
		{"binary", func(w *ClassifiedHistoryWitness) { w.RuntimeBinarySHA256 = "invalid" }},
		{"code", func(w *ClassifiedHistoryWitness) { w.Diagnostic.RawStub++ }},
		{"known", func(w *ClassifiedHistoryWitness) { w.Diagnostic.StubKnown = true }},
		{"absent", func(w *ClassifiedHistoryWitness) { w.Diagnostic.StubPresent = false }},
		{"presence", func(w *ClassifiedHistoryWitness) { w.Diagnostic.Presence++ }},
		{"stage", func(w *ClassifiedHistoryWitness) { w.Diagnostic.Stage = "protocol_metadata" }},
		{"conflicts", func(w *ClassifiedHistoryWitness) { w.SourceRevisionConflicts = 33 }},
		{"generation", func(w *ClassifiedHistoryWitness) { w.RebuildGeneration = strings.Repeat("f", 32) }},
		{"epoch", func(w *ClassifiedHistoryWitness) { w.UnsafeEpoch++ }},
		{"revision", func(w *ClassifiedHistoryWitness) { w.ArchiveRevision++ }},
		{"marker", func(w *ClassifiedHistoryWitness) { w.UnsafeMarkerSHA256 = strings.Repeat("f", 64) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			s, w := acceptedPartialRecoveryFixture(t)
			before := snapshotRecoveryRows(t, s)
			change.mutate(&w)
			if _, err := s.RestoreAcceptedPartialHistorySnapshot(context.Background(), w); err == nil {
				t.Fatal("accepted drift")
			}
			if !reflect.DeepEqual(before, snapshotRecoveryRows(t, s)) {
				t.Fatal("denial changed committed authority/body")
			}
			if _, err := os.Stat(s.privateDir + "/unsafe.marker"); err != nil {
				t.Fatal("denial removed unsafe fence", err)
			}
		})
	}
}

func TestAcceptedPartialHistoryKnownRecoveryRejectsAcceptanceBitAndPreservesOldJSON(t *testing.T) {
	s, w := classifiedRecoveryFixture(t)
	old, _, err := canonicalClassifiedHistoryWitness(w)
	if err != nil || strings.Contains(old, "accept_unknown_history_gaps") {
		t.Fatal("old witness bytes changed", err)
	}
	w.AcceptUnknownHistoryGaps = true
	if _, err = s.RestoreClassifiedHistorySnapshot(context.Background(), w); err == nil {
		t.Fatal("known recovery accepted new unknown trust bit")
	}
}

func TestAcceptedPartialHistoryRecoveryPreservesPriorKnownAudits(t *testing.T) {
	s, w := classifiedRecoveryFixture(t)
	ctx := context.Background()
	if _, err := s.RestoreClassifiedHistorySnapshot(ctx, w); err != nil {
		t.Fatal(err)
	}
	for _, stub := range []int{193, 217} {
		w = nextNoticeWitness(t, s, w, stub, "c")
		if _, err := s.RestoreClassifiedHistorySnapshot(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	const cols = "recovery_epoch,account,prior_unsafe_epoch,prior_unsafe_reason,prior_archive_revision,archive_revision,observation_sha256,unsafe_sha256,observation_file_identity,unsafe_file_identity,phase,created_at,completed_at,classification,witness_json,witness_sha256"
	before := rebuildRows(t, s, "snapshot_recovery_audit", cols)
	w = nextNoticeWitness(t, s, w, 229, "d")
	w.Classification = AcceptedPartialHistoryGap
	w.AcceptUnknownHistoryGaps = true
	w.RuntimeBuildSHA = "eda63bbfbde526536685f40d6d74e6a2ef282214"
	w.Diagnostic = ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 229, StubPresent: true, StubKnown: false, Presence: 155}
	if _, err := s.RestoreAcceptedPartialHistorySnapshot(ctx, w); err != nil {
		t.Fatal(err)
	}
	if before != rebuildRows(t, s, "snapshot_recovery_audit WHERE recovery_epoch<=3", cols) {
		t.Fatal("old33/193/217 audit bytes changed")
	}
}

func TestAcceptedPartialHistoryRecoveryGapRollbackAndCompletionFence(t *testing.T) {
	t.Run("overflow", func(t *testing.T) {
		s, w := acceptedPartialRecoveryFixture(t)
		if _, err := s.writeDB.Exec("INSERT INTO unknown_history_coverage VALUES('account',?,?); INSERT INTO unknown_history_codes VALUES('account',229,?,1,2)", math.MaxInt64, math.MaxInt64, math.MaxInt64); err != nil {
			t.Fatal(err)
		}
		before := acceptedPartialGapRows(t, s)
		if _, err := s.RestoreAcceptedPartialHistorySnapshot(context.Background(), w); err == nil {
			t.Fatal("overflow acceptance succeeded")
		}
		var revision uint64
		var audits, unsafe int
		if err := s.readDB.QueryRow("SELECT archive_revision,(SELECT count(*) FROM snapshot_recovery_audit),(SELECT unsafe FROM account_safety WHERE account='account') FROM archive_meta").Scan(&revision, &audits, &unsafe); err != nil || revision != w.ArchiveRevision || audits != 0 || unsafe != 1 {
			t.Fatal("prepare failure committed partially", revision, audits, unsafe, err)
		}
		if before != acceptedPartialGapRows(t, s) {
			t.Fatal("overflow modified evidence")
		}
	})
	for _, tamper := range []string{"DELETE FROM unknown_history_coverage", "DELETE FROM unknown_history_codes", "DELETE FROM sync_gaps WHERE code='unknown_history_notice'", "UPDATE sync_state SET coverage_unknown=0 WHERE chat=''"} {
		t.Run(tamper, func(t *testing.T) {
			s, w := acceptedPartialRecoveryFixture(t)
			s.snapshotRecoveryFault = func(stage string) error {
				if stage == "before_complete" {
					_, err := s.writeDB.Exec(tamper)
					return err
				}
				return nil
			}
			if _, err := s.RestoreAcceptedPartialHistorySnapshot(context.Background(), w); err == nil {
				t.Fatal("lost uncertainty authorized completion")
			}
			var unsafe int
			if err := s.readDB.QueryRow("SELECT unsafe FROM account_safety WHERE account='account'").Scan(&unsafe); err != nil || unsafe != 1 {
				t.Fatal("completion cleared SQL fence", unsafe, err)
			}
			s.snapshotRecoveryFault = nil
			if _, err := s.RestoreAcceptedPartialHistorySnapshot(context.Background(), w); err == nil {
				t.Fatal("retry erased missing uncertainty")
			}
		})
	}
}
