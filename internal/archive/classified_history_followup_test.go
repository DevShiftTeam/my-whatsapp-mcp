//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClassifiedHistorySecondClassPreservesPriorAcceptance(t *testing.T) {
	s, firstWitness := classifiedRecoveryFixture(t)
	ctx := context.Background()
	first, err := s.RestoreClassifiedHistorySnapshot(ctx, firstWitness)
	if err != nil {
		t.Fatal(err)
	}
	oldAudit := rebuildRows(t, s, "snapshot_recovery_audit", "recovery_epoch,classification,witness_json,witness_sha256,phase,prior_unsafe_reason")
	createCredential(t, s, "second-class", CredentialPolicy{Account: "account", AllChats: true})
	pending := event("pending-after-first", "", 1, EventOriginal)
	if err = s.RecordRebuildDiscovery(ctx, SourceAnchor{Key: pending.Key, OriginalSentAt: pending.OriginalSentAt}); err != nil {
		t.Fatal(err)
	}
	continuationRegister(t, s, pending.Key, "pending-after-first")
	blocked := pending.Key
	blocked.Message = "retained-negative"
	if err = s.RecordRebuildRestriction(ctx, blocked, time.Time{}, true); err != nil {
		t.Fatal(err)
	}
	before := snapshotRecoveryRows(t, s)
	columns := map[string]string{"rebuild_requests": "id,generation,state,consumed_at,operation_id,operation_revision", "rebuild_operation": "operation_id,generation,revision,state,copy_limit,copy_spent,discovery_limit,discovery_spent", "rebuild_guards": "account,chat,sender,message,from_me,expires_at,view_once,deleted,protocol_revision,source_kind", "rebuild_provenance": "generation,receipt"}
	retained := map[string]string{}
	for table, fields := range columns {
		retained[table] = rebuildRows(t, s, table, fields)
	}
	if err = s.MarkUnsafe(ctx, "account", "history_corrupt"); err != nil {
		t.Fatal(err)
	}
	next := firstWitness
	next.Classification = ClassifiedHistoryChangeUsername
	next.Diagnostic = ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 193, StubPresent: true, StubKnown: true, Presence: 411}
	next.EvidenceSHA256 = strings.Repeat("c", 64)
	if err = s.writeDB.QueryRow("SELECT epoch FROM account_safety WHERE account='account'").Scan(&next.UnsafeEpoch); err != nil {
		t.Fatal(err)
	}
	if err = s.writeDB.QueryRow("SELECT archive_revision,recovery_epoch FROM archive_meta WHERE id=1").Scan(&next.ArchiveRevision, &next.RecoveryEpoch); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(s.privateDir)
	if err != nil {
		t.Fatal(err)
	}
	markers, err := captureSnapshotMarkers(root)
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	next.ObservationMarkerSHA256 = markerDigest(markers, "observation.marker")
	next.ObservationMarkerIdentity = markerIdentity(markers, "observation.marker")
	next.UnsafeMarkerSHA256 = markerDigest(markers, "unsafe.marker")
	next.UnsafeMarkerIdentity = markerIdentity(markers, "unsafe.marker")
	if next.UnsafeEpoch != firstWitness.UnsafeEpoch+2 || next.RecoveryEpoch != first.RecoveryEpoch {
		t.Fatal("fixture did not create distinct fault")
	}
	if _, err = s.RestoreClassifiedHistorySnapshot(ctx, firstWitness); err == nil {
		t.Fatal("old33 witness accepted newer fault")
	}
	s.snapshotRecoveryFault = func(stage string) error {
		if stage == "after_prepare" {
			return ErrArchiveUnavailable
		}
		return nil
	}
	if _, err = s.RestoreClassifiedHistorySnapshot(ctx, next); err != ErrArchiveUnavailable {
		t.Fatal("new193 acceptance never prepared", err)
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
	changed := next
	changed.Diagnostic.Presence = 475
	if _, err = s.RestoreClassifiedHistorySnapshot(ctx, changed); err == nil {
		t.Fatal("193 borrowed33 presence")
	}
	changed = next
	changed.EvidenceSHA256 = strings.Repeat("d", 64)
	if _, err = s.RestoreClassifiedHistorySnapshot(ctx, changed); err == nil {
		t.Fatal("different193 witness resumed")
	}
	second, err := s.RestoreClassifiedHistorySnapshot(ctx, next)
	if err != nil {
		t.Fatal("new193 acceptance failed", err)
	}
	if second.RecoveryEpoch != first.RecoveryEpoch+1 {
		t.Fatal("second acceptance reused audit epoch")
	}
	if !reflect.DeepEqual(snapshotRecoveryRows(t, s), before) {
		t.Fatal("second classification changed committed body/authority")
	}
	for table, fields := range columns {
		if rebuildRows(t, s, table, fields) != retained[table] {
			t.Fatal("second classification changed retained evidence", table)
		}
	}
	if got := rebuildRows(t, s, "snapshot_recovery_audit WHERE recovery_epoch=1", "recovery_epoch,classification,witness_json,witness_sha256,phase,prior_unsafe_reason"); got != oldAudit {
		t.Fatal("prior completed33 audit changed")
	}
}

func TestClassifiedHistoryUsernameMigrationPreservesCompleted33Audit(t *testing.T) {
	_, w := classifiedRecoveryFixture(t)
	canonical, digest, err := canonicalClassifiedHistoryWitness(w)
	if err != nil {
		t.Fatal(err)
	}
	dir := classifiedVersionFixture(t, 15, rebuildSourceOrdinary)
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "archive.sqlite")+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	columns := "recovery_epoch,account,prior_unsafe_epoch,prior_unsafe_reason,prior_archive_revision,archive_revision,observation_sha256,unsafe_sha256,observation_file_identity,unsafe_file_identity,phase,created_at,completed_at,classification,witness_json,witness_sha256"
	if _, err = db.Exec("INSERT INTO snapshot_recovery_audit("+columns+") VALUES(1,'account',1,'history_corrupt',0,1,'','','','','complete',123,124,?,?,?)", w.Classification, canonical, digest); err != nil {
		t.Fatal(err)
	}
	before := rebuildRows(t, &Store{readDB: db}, "snapshot_recovery_audit", columns)
	if err = migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if after := rebuildRows(t, &Store{readDB: db}, "snapshot_recovery_audit", columns); after != before {
		t.Fatal("016 changed prior completed33 audit")
	}
	if _, err = db.Exec("INSERT INTO snapshot_recovery_audit(recovery_epoch,account,prior_unsafe_epoch,prior_unsafe_reason,prior_archive_revision,archive_revision,phase,created_at,classification,witness_json,witness_sha256) VALUES(2,'account',3,'history_corrupt',1,2,'prepared',125,'unreviewed_notice',?,?)", canonical, digest); err == nil {
		t.Fatal("016 admitted unknown class")
	}
}

func TestClassifiedHistoryExactClassTupleMapping(t *testing.T) {
	_, base := classifiedRecoveryFixture(t)
	for _, class := range []string{ClassifiedHistoryGroupParticipantChangeNumber, ClassifiedHistoryChangeUsername} {
		expected, ok := ClassifiedHistoryDiagnosticFor(class)
		if !ok {
			t.Fatal("known exact class absent")
		}
		w := base
		w.Classification = class
		w.Diagnostic = expected
		if _, _, err := canonicalClassifiedHistoryWitness(w); err != nil {
			t.Fatal("known exact tuple rejected", err)
		}
		for _, mutate := range []func(*ClassifiedHistoryDiagnostic){func(d *ClassifiedHistoryDiagnostic) { d.RawStub = -1 }, func(d *ClassifiedHistoryDiagnostic) { d.Presence |= 4 }, func(d *ClassifiedHistoryDiagnostic) { d.Presence |= 512 }, func(d *ClassifiedHistoryDiagnostic) { d.StubKnown = false }, func(d *ClassifiedHistoryDiagnostic) { d.Stage = "protocol_metadata" }} {
			changed := w
			mutate(&changed.Diagnostic)
			if _, _, err := canonicalClassifiedHistoryWitness(changed); err == nil {
				t.Fatal("nonexact diagnostic accepted")
			}
		}
	}
	if _, ok := ClassifiedHistoryDiagnosticFor("v1_unknown"); ok {
		t.Fatal("unknown class accepted")
	}
}

func TestClassifiedHistoryConflictWitnessBoundedAndProducerBound(t *testing.T) {
	_, base := classifiedRecoveryFixture(t)
	oldJSON, _, err := canonicalClassifiedHistoryWitness(base)
	if err != nil || strings.Contains(oldJSON, "source_revision_conflicts") {
		t.Fatal("zero field changed legacy canonical witness", err)
	}
	w := base
	w.Classification = ClassifiedHistoryChangeUsername
	w.Diagnostic, _ = ClassifiedHistoryDiagnosticFor(w.Classification)
	for _, producer := range []string{"ff0a3bd06a7f921e50d9b5c6484c481066070db3", "7a7ca4a2ec0f619c0aff0a25839abcba68805a0a"} {
		w.RuntimeBuildSHA = producer
		for _, count := range []uint32{1, 4, 32} {
			w.SourceRevisionConflicts = count
			if _, _, err = canonicalClassifiedHistoryWitness(w); err != nil {
				t.Fatal("reviewed bounded conflict rejected", err)
			}
		}
	}
	for _, mutate := range []func(*ClassifiedHistoryWitness){
		func(w *ClassifiedHistoryWitness) { w.SourceRevisionConflicts = 33 },
		func(w *ClassifiedHistoryWitness) { w.RuntimeBuildSHA = strings.Repeat("d", 40) },
		func(w *ClassifiedHistoryWitness) {
			w.Classification = ClassifiedHistoryGroupParticipantChangeNumber
			w.Diagnostic, _ = ClassifiedHistoryDiagnosticFor(w.Classification)
		},
		func(w *ClassifiedHistoryWitness) { w.Diagnostic.RawStub = 33 },
	} {
		changed := w
		mutate(&changed)
		if _, _, err = canonicalClassifiedHistoryWitness(changed); err == nil {
			t.Fatal("unreviewed conflict evidence accepted")
		}
	}
}
