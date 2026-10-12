//go:build sqlite_fts5

package archive

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

func nextNoticeWitness(t *testing.T, s *Store, prior ClassifiedHistoryWitness, stub int, evidence string) ClassifiedHistoryWitness {
	t.Helper()
	if err := s.MarkUnsafe(context.Background(), "account", "history_corrupt"); err != nil {
		t.Fatal(err)
	}
	w := prior
	w.Classification = ClassifiedHistoryChangeUsername
	w.Diagnostic = ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: stub, StubPresent: true, StubKnown: true, Presence: 411}
	w.EvidenceSHA256 = strings.Repeat(evidence, 64)
	if err := s.writeDB.QueryRow("SELECT epoch FROM account_safety WHERE account='account'").Scan(&w.UnsafeEpoch); err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.QueryRow("SELECT archive_revision,recovery_epoch FROM archive_meta WHERE id=1").Scan(&w.ArchiveRevision, &w.RecoveryEpoch); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(s.privateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	markers, err := captureSnapshotMarkers(root)
	if err != nil {
		t.Fatal(err)
	}
	w.ObservationMarkerSHA256 = markerDigest(markers, "observation.marker")
	w.ObservationMarkerIdentity = markerIdentity(markers, "observation.marker")
	w.UnsafeMarkerSHA256 = markerDigest(markers, "unsafe.marker")
	w.UnsafeMarkerIdentity = markerIdentity(markers, "unsafe.marker")
	return w
}

func TestClassifiedHistoryThirdNoticePreservesTwoPriorAudits(t *testing.T) {
	ctx := context.Background()
	s, first := classifiedRecoveryFixture(t)
	if _, err := s.RestoreClassifiedHistorySnapshot(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := nextNoticeWitness(t, s, first, 193, "c")
	if _, err := s.RestoreClassifiedHistorySnapshot(ctx, second); err != nil {
		t.Fatal(err)
	}
	oldAudits := rebuildRows(t, s, "snapshot_recovery_audit", "recovery_epoch,classification,witness_json,witness_sha256,phase,prior_unsafe_reason")
	before := snapshotRecoveryRows(t, s)
	fields := map[string]string{"rebuild_requests": "id,generation,state,consumed_at,operation_id,operation_revision", "rebuild_operation": "operation_id,generation,revision,state,copy_limit,copy_spent,discovery_limit,discovery_spent", "rebuild_guards": "account,chat,sender,message,from_me,expires_at,view_once,deleted,protocol_revision,source_kind", "rebuild_provenance": "generation,receipt"}
	retained := map[string]string{}
	for table, columns := range fields {
		retained[table] = rebuildRows(t, s, table, columns)
	}
	third := nextNoticeWitness(t, s, second, 217, "d")
	third.RuntimeBuildSHA = "028633b7af17c13f016b4b0e95ad3769c3b8dcb1"
	third.SourceRevisionConflicts = 3
	if third.RecoveryEpoch != 2 || third.UnsafeEpoch != second.UnsafeEpoch+2 {
		t.Fatal("not a third current fault")
	}
	for _, old := range []ClassifiedHistoryWitness{first, second} {
		if _, err := s.RestoreClassifiedHistorySnapshot(ctx, old); err == nil {
			t.Fatal("prior witness accepted later fault")
		}
	}
	s.snapshotRecoveryFault = func(stage string) error {
		if stage == "after_prepare" {
			return ErrArchiveUnavailable
		}
		return nil
	}
	if _, err := s.RestoreClassifiedHistorySnapshot(ctx, third); err != ErrArchiveUnavailable {
		t.Fatal("217 acceptance never prepared", err)
	}
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	s, err = Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	changed := third
	changed.EvidenceSHA256 = strings.Repeat("e", 64)
	if _, err = s.RestoreClassifiedHistorySnapshot(ctx, changed); err == nil {
		t.Fatal("different witness resumed217")
	}
	changed = third
	changed.Diagnostic.RawStub = 193
	if _, err = s.RestoreClassifiedHistorySnapshot(ctx, changed); err == nil {
		t.Fatal("193 witness resumed217")
	}
	receipt, err := s.RestoreClassifiedHistorySnapshot(ctx, third)
	if err != nil || receipt.RecoveryEpoch != 3 {
		t.Fatal("third recovery failed", receipt, err)
	}
	canonical, digest, err := canonicalClassifiedHistoryWitness(third)
	if err != nil {
		t.Fatal(err)
	}
	var savedJSON, savedDigest string
	if err = s.writeDB.QueryRow("SELECT witness_json,witness_sha256 FROM snapshot_recovery_audit WHERE recovery_epoch=3").Scan(&savedJSON, &savedDigest); err != nil {
		t.Fatal(err)
	}
	if savedJSON != canonical || savedDigest != digest {
		t.Fatal("217 tuple or conflict count rewritten in audit")
	}
	var migrations int
	if err = s.writeDB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&migrations); err != nil || migrations != 16 {
		t.Fatal("notice added migration", migrations, err)
	}
	if !reflect.DeepEqual(snapshotRecoveryRows(t, s), before) {
		t.Fatal("changed body or authority")
	}
	for table, columns := range fields {
		if rebuildRows(t, s, table, columns) != retained[table] {
			t.Fatal("changed retained metadata", table)
		}
	}
	if got := rebuildRows(t, s, "snapshot_recovery_audit WHERE recovery_epoch<=2", "recovery_epoch,classification,witness_json,witness_sha256,phase,prior_unsafe_reason"); got != oldAudits {
		t.Fatal("changed old audit bytes")
	}
}

func TestClassifiedHistory217ExactTupleAndConflictProducer(t *testing.T) {
	_, w := classifiedRecoveryFixture(t)
	w.Classification = ClassifiedHistoryChangeUsername
	w.Diagnostic = ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 217, StubPresent: true, StubKnown: true, Presence: 411}
	if _, _, err := canonicalClassifiedHistoryWitness(w); err != nil {
		t.Fatal("exact217 rejected", err)
	}
	for _, mutate := range []func(*ClassifiedHistoryWitness){
		func(w *ClassifiedHistoryWitness) { w.Diagnostic.RawStub = 218 },
		func(w *ClassifiedHistoryWitness) { w.Diagnostic.RawStub = -1 },
		func(w *ClassifiedHistoryWitness) { w.Diagnostic.Presence = 475 },
		func(w *ClassifiedHistoryWitness) { w.Diagnostic.Presence |= 4 },
		func(w *ClassifiedHistoryWitness) { w.Diagnostic.Presence |= 512 },
		func(w *ClassifiedHistoryWitness) { w.Diagnostic.StubPresent = false },
		func(w *ClassifiedHistoryWitness) { w.Diagnostic.StubKnown = false },
		func(w *ClassifiedHistoryWitness) { w.Diagnostic.Stage = "protocol_metadata" },
	} {
		changed := w
		mutate(&changed)
		if _, _, err := canonicalClassifiedHistoryWitness(changed); err == nil {
			t.Fatal("nonexact217 accepted")
		}
	}
	w.RuntimeBuildSHA = "028633b7af17c13f016b4b0e95ad3769c3b8dcb1"
	w.SourceRevisionConflicts = 3
	if _, _, err := canonicalClassifiedHistoryWitness(w); err != nil {
		t.Fatal("qualified217 producer rejected", err)
	}
	w.RuntimeBuildSHA = strings.Repeat("f", 40)
	if _, _, err := canonicalClassifiedHistoryWitness(w); err == nil {
		t.Fatal("unreviewed producer accepted")
	}
	w.RuntimeBuildSHA = "028633b7af17c13f016b4b0e95ad3769c3b8dcb1"
	w.SourceRevisionConflicts = 33
	if _, _, err := canonicalClassifiedHistoryWitness(w); err == nil {
		t.Fatal("excess conflicts accepted")
	}
}
