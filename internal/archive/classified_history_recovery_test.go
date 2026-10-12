//go:build sqlite_fts5

package archive

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func classifiedRecoveryFixture(t *testing.T) (*Store, ClassifiedHistoryWitness) {
	t.Helper()
	ctx := context.Background()
	s, receipt, e := rebuildFixture(t)
	continuationRegister(t, s, e.Key, "classified-preserved-request")
	policy, _ := s.Policy(ctx)
	e.RebuildRequestID = "classified-preserved-request"
	apply(t, s, e, policy.Generation)
	if _, err := s.BeginObservation(ctx, "account"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnsafe(ctx, "account", "history_corrupt"); err != nil {
		t.Fatal(err)
	}
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	hash := strings.Repeat("a", 64)
	w := ClassifiedHistoryWitness{Version: 1, Classification: ClassifiedHistoryGroupParticipantChangeNumber, RebuildGeneration: receipt.Generation, OriginalArchiveSHA256: hash, EvidenceSHA256: hash, OperatorReceiptSHA256: hash, DiagnosticLogSHA256: hash, LaunchEvidenceSHA256: hash, RuntimeBuildSHA: strings.Repeat("b", 40), RuntimeBinarySHA256: hash, RuntimePID: 123, LaunchEvidenceKind: "owner_attested_launch_interval_v1", LaunchActionRecordedAt: time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC), ProcessObservedAt: time.Date(2026, 10, 12, 1, 2, 4, 0, time.UTC), StoppedAt: time.Date(2026, 10, 12, 1, 2, 5, 0, time.UTC), ProcessReadReceiptSHA256: hash, JoinedExitReceiptSHA256: hash, Diagnostic: ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 33, StubPresent: true, StubKnown: true, Presence: 475}}
	if err = s.writeDB.QueryRow("SELECT epoch FROM account_safety WHERE account='account'").Scan(&w.UnsafeEpoch); err != nil {
		t.Fatal(err)
	}
	if err = s.writeDB.QueryRow("SELECT archive_revision,recovery_epoch FROM archive_meta WHERE id=1").Scan(&w.ArchiveRevision, &w.RecoveryEpoch); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
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
	return s, w
}

func TestClassifiedHistoryRecoveryPreservesCommittedState(t *testing.T) {
	s, w := classifiedRecoveryFixture(t)
	ctx := context.Background()
	before := snapshotRecoveryRows(t, s)
	evidence := map[string]string{}
	for table, columns := range map[string]string{"rebuild_guards": "account,chat,sender,message,from_me,protocol_revision,source_kind,deleted", "rebuild_requests": "id,generation,state,consumed_at,operation_id,operation_revision", "rebuild_operation": "operation_id,generation,revision,copy_limit,copy_spent,discovery_limit,discovery_spent", "rebuild_provenance": "generation,receipt"} {
		evidence[table] = rebuildRows(t, s, table, columns)
	}
	receipt, err := s.RestoreClassifiedHistorySnapshot(ctx, w)
	if err != nil {
		t.Fatal("classified acceptance:", err)
	}
	if receipt.Mode != "committed_snapshot" || receipt.Code != "owner_restored_snapshot" || receipt.RecoveryEpoch != w.RecoveryEpoch+1 || uint64(receipt.ArchiveRevision) != w.ArchiveRevision+1 {
		t.Fatal("bad partial receipt", receipt)
	}
	if !reflect.DeepEqual(snapshotRecoveryRows(t, s), before) {
		t.Fatal("changed committed state")
	}
	for table, columns := range map[string]string{"rebuild_guards": "account,chat,sender,message,from_me,protocol_revision,source_kind,deleted", "rebuild_requests": "id,generation,state,consumed_at,operation_id,operation_revision", "rebuild_operation": "operation_id,generation,revision,copy_limit,copy_spent,discovery_limit,discovery_spent", "rebuild_provenance": "generation,receipt"} {
		if rebuildRows(t, s, table, columns) != evidence[table] {
			t.Fatal("changed recovery evidence", table)
		}
	}
	if _, err = s.RestoreCommittedSnapshot(ctx, ""); err == nil {
		t.Fatal("ordinary restore replayed classified audit")
	}
	if again, err := s.RestoreClassifiedHistorySnapshot(ctx, w); err != nil || again != receipt {
		t.Fatal("identical replay failed", again, err)
	}
}

func TestClassifiedHistoryRecoveryCrashRetryRequiresIdenticalWitness(t *testing.T) {
	for _, stage := range []string{"after_prepare", "before_observation_remove", "before_unsafe_remove", "before_directory_sync", "before_complete"} {
		t.Run(stage, func(t *testing.T) {
			s, w := classifiedRecoveryFixture(t)
			ctx := context.Background()
			s.snapshotRecoveryFault = func(current string) error {
				if current == stage {
					return ErrArchiveUnavailable
				}
				return nil
			}
			if _, err := s.RestoreClassifiedHistorySnapshot(ctx, w); err != ErrArchiveUnavailable {
				t.Fatal("missing injected failure", err)
			}
			dir := s.privateDir
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err = s.RestoreCommittedSnapshot(ctx, ""); err == nil {
				t.Fatal("ordinary recovery completed classified prepare")
			}
			changed := w
			changed.EvidenceSHA256 = strings.Repeat("c", 64)
			if _, err = s.RestoreClassifiedHistorySnapshot(ctx, changed); err == nil {
				t.Fatal("different witness resumed")
			}
			if _, err = s.RestoreClassifiedHistorySnapshot(ctx, w); err != nil {
				t.Fatal("same witness could not resume", err)
			}
			if err = s.MarkUnsafe(ctx, "account", "history_corrupt"); err != nil {
				t.Fatal(err)
			}
			if _, err = s.RestoreClassifiedHistorySnapshot(ctx, w); err == nil {
				t.Fatal("old witness cleared new fault")
			}
		})
	}
}

func TestClassifiedHistoryRecoveryRejectsWitnessDrift(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*ClassifiedHistoryWitness)
	}{
		{"unknown", func(w *ClassifiedHistoryWitness) { w.Diagnostic.RawStub = -1 }},
		{"stage", func(w *ClassifiedHistoryWitness) { w.Diagnostic.Stage = "protocol_metadata" }},
		{"body", func(w *ClassifiedHistoryWitness) { w.Diagnostic.Presence |= 4 }},
		{"control", func(w *ClassifiedHistoryWitness) { w.Diagnostic.Presence |= 512 }},
		{"generation", func(w *ClassifiedHistoryWitness) { w.RebuildGeneration = strings.Repeat("f", 32) }},
		{"epoch", func(w *ClassifiedHistoryWitness) { w.UnsafeEpoch++ }},
		{"revision", func(w *ClassifiedHistoryWitness) { w.ArchiveRevision++ }},
		{"marker", func(w *ClassifiedHistoryWitness) { w.UnsafeMarkerSHA256 = strings.Repeat("f", 64) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			s, w := classifiedRecoveryFixture(t)
			change.mutate(&w)
			if _, err := s.RestoreClassifiedHistorySnapshot(context.Background(), w); err == nil {
				t.Fatal("accepted drift")
			}
		})
	}
}

func TestClassifiedHistoryRecoveryAllowsLaterOrdinaryFault(t *testing.T) {
	for _, reason := range []string{"drain_failed", "unclean_shutdown", "new_observation"} {
		t.Run(reason, func(t *testing.T) {
			s, w := classifiedRecoveryFixture(t)
			ctx := context.Background()
			first, err := s.RestoreClassifiedHistorySnapshot(ctx, w)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.RestoreCommittedSnapshot(ctx, ""); err == nil {
				t.Fatal("ordinary replay accepted typed completion")
			}
			if reason == "new_observation" {
				if _, err = s.BeginObservation(ctx, "account"); err != nil {
					t.Fatal(err)
				}
			} else if err = s.MarkUnsafe(ctx, "account", UnsafeReason(reason)); err != nil {
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
			second, err := s.RestoreCommittedSnapshot(ctx, "")
			if err != nil {
				t.Fatal("later ordinary fault cannot recover", err)
			}
			if second.RecoveryEpoch != first.RecoveryEpoch+1 {
				t.Fatal("ordinary fault reused typed audit")
			}
		})
	}
}
