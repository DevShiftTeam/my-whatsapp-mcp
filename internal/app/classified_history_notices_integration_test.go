package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// This sequence operates exclusively on stopped synthetic profiles. Neither an
// application constructor nor a network send is involved.
func TestClassifiedHistoryNoticesThirdAcceptancePreservesPriorAuthority(t *testing.T) {
	ctx := context.Background()
	f, secondConfig, secondEvidence := classifiedFollowupFixture(t)
	second, err := RecoverClassifiedHistorySnapshot(ctx, secondConfig)
	if err != nil || second.RecoveryEpoch != 2 {
		t.Fatal("second193 acceptance failed", second, err)
	}
	f.assertPreserved(t)
	priorSeals := classifiedFollowupBindings(t, f.paths.WorkingDir)
	if len(priorSeals) != 2 {
		t.Fatal("missing prior33 and193 evidence seals")
	}
	store, err := archive.Open(ctx, f.paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	progress, err := store.RebuildProgress(ctx)
	if err != nil || progress == nil {
		store.Close()
		t.Fatal("missing durable operation", err)
	}
	candidates, err := store.NextRebuildCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 {
		store.Close()
		t.Fatal("missing synthetic recovery candidate", err)
	}
	if err := store.PendingRebuildRequest(ctx, archive.RebuildRequest{ID: "notices-preserved-pending", Generation: progress.Generation, OperationID: progress.OperationID, OperationRevision: progress.Revision, Key: candidates[0].Key, PolicyGeneration: progress.PolicyGeneration, CreatedAt: time.Now()}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	spent, err := store.RebuildProgress(ctx)
	if err != nil || spent.CopySpent != 1 {
		store.Close()
		t.Fatal("fixture did not persist a spent request", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Seed all three durable outbound outcomes directly while this disposable
	// archive is stopped. These are journal fixtures, not outbound operations.
	db := classifiedIntegrationDB(t, f.paths)
	var credential, chat string
	if err := db.QueryRow("SELECT id FROM credentials ORDER BY id LIMIT 1").Scan(&credential); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT id FROM chats ORDER BY id LIMIT 1").Scan(&chat); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"accepted", "failed", "outcome_unknown"} {
		if _, err := db.Exec("INSERT INTO send_requests(credential_id,request_id,account,chat_id,digest,protocol_id,state,accepted_at) VALUES(?,?,?,?,?,?,?,?)", credential, "notices-"+outcome, f.bootstrap.ExpectedAccountID, chat, make([]byte, 32), "synthetic-"+outcome, outcome, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = archive.Open(ctx, f.paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.BeginObservation(ctx, f.bootstrap.ExpectedAccountID); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err = store.MarkUnsafe(ctx, f.bootstrap.ExpectedAccountID, "history_corrupt"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	thirdConfig, thirdEvidence := syntheticClassifiedEvidence(t)
	thirdConfig.DataDir = f.paths.Root
	thirdEvidence.ReadySHA256 = secondEvidence.ReadySHA256
	w := &thirdEvidence.Witness
	w.Classification = archive.ClassifiedHistoryChangeUsername
	w.RebuildGeneration = secondEvidence.Witness.RebuildGeneration
	w.Diagnostic = archive.ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 217, StubPresent: true, StubKnown: true, Presence: 411}
	log := []byte(`{"event":"history_failure","stage":"record_body_missing","stub":"other_stub","stub_type":217,"raw_stub":217,"stub_present":true,"stub_known":true,"presence":411}` + "\n")
	if err := os.WriteFile(thirdEvidence.DiagnosticLogPath, log, 0600); err != nil {
		t.Fatal(err)
	}
	w.DiagnosticLogSHA256 = liveRebuildDigest(log)
	db = classifiedIntegrationDB(t, f.paths)
	if err := db.QueryRow("SELECT epoch FROM account_safety WHERE account=?", f.bootstrap.ExpectedAccountID).Scan(&w.UnsafeEpoch); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT archive_revision,recovery_epoch FROM archive_meta WHERE id=1").Scan(&w.ArchiveRevision, &w.RecoveryEpoch); err != nil {
		t.Fatal(err)
	}
	if w.UnsafeEpoch <= secondEvidence.Witness.UnsafeEpoch || w.RecoveryEpoch != 2 {
		t.Fatal("third evidence does not bind a fresh fence")
	}
	// Eight authority/policy/journal tables, plus the original-file budget and
	// durable recovery operation, are compared in full rather than by counts.
	tables := []string{"credentials", "credential_chats", "send_permissions", "send_permission_modes", "send_requests", "archive_policy", "policy_chats", "accounts", "extension_original_budget", "rebuild_provenance", "rebuild_guards", "rebuild_requests", "rebuild_operation", "rebuild_commands", "rebuild_discovery_attempts", "rebuild_candidate_deferrals"}
	before := map[string]string{}
	for _, table := range tables {
		before[table] = classifiedNoticesRows(t, db, "SELECT * FROM "+table+" ORDER BY rowid")
	}
	priorAudits := classifiedNoticesRows(t, db, "SELECT * FROM snapshot_recovery_audit WHERE recovery_epoch<=2 ORDER BY recovery_epoch")
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	w.OriginalArchiveSHA256, err = classifiedArchiveHash(ctx, filepath.Join(f.paths.ArchiveDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	w.UnsafeMarkerSHA256, w.UnsafeMarkerIdentity = classifiedIntegrationMarker(t, filepath.Join(f.paths.ArchiveDir, "unsafe.marker"))
	w.ObservationMarkerSHA256, w.ObservationMarkerIdentity = classifiedIntegrationMarker(t, filepath.Join(f.paths.ArchiveDir, "observation.marker"))
	classifiedIntegrationResign(t, &thirdConfig, &thirdEvidence)
	fencedFiles := classifiedFollowupFiles(t, f.paths.Root)
	if _, err := RecoverClassifiedHistorySnapshot(ctx, secondConfig); err == nil {
		t.Fatal("old193 witness authorized a fresh217 fence")
	}
	classifiedFollowupAssertExistingFiles(t, fencedFiles)
	third, err := RecoverClassifiedHistorySnapshot(ctx, thirdConfig)
	if err != nil || third.RecoveryEpoch != 3 || uint64(third.ArchiveRevision) != w.ArchiveRevision+1 || third.Code != "owner_restored_snapshot" {
		t.Fatal("third217 acceptance failed", third, err)
	}
	classifiedFollowupAssertExistingFiles(t, priorSeals)
	if len(classifiedFollowupBindings(t, f.paths.WorkingDir)) != 3 {
		t.Fatal("third acceptance did not append exactly one evidence seal")
	}
	f.assertPreserved(t)
	db = classifiedIntegrationDB(t, f.paths)
	for _, table := range tables {
		if got := classifiedNoticesRows(t, db, "SELECT * FROM "+table+" ORDER BY rowid"); got != before[table] {
			t.Fatal("third acceptance changed preserved table", table)
		}
	}
	if got := classifiedNoticesRows(t, db, "SELECT * FROM snapshot_recovery_audit WHERE recovery_epoch<=2 ORDER BY recovery_epoch"); got != priorAudits {
		t.Fatal("third acceptance rewrote an earlier audit")
	}
	var audits int
	if err := db.QueryRow("SELECT count(*) FROM snapshot_recovery_audit WHERE phase='complete'").Scan(&audits); err != nil || audits != 3 {
		t.Fatal("expected three independent completed audits", audits, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	seals := classifiedFollowupBindings(t, f.paths.WorkingDir)
	if again, err := RecoverClassifiedHistorySnapshot(ctx, thirdConfig); err != nil || again != third {
		t.Fatal("exact217 retry failed", again, err)
	}
	if !reflect.DeepEqual(seals, classifiedFollowupBindings(t, f.paths.WorkingDir)) {
		t.Fatal("exact retry changed evidence seals")
	}
	f.assertPreserved(t)
}

func classifiedNoticesRows(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var records [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		for i, value := range values {
			if raw, ok := value.([]byte); ok {
				values[i] = append([]byte(nil), raw...)
			}
		}
		records = append(records, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
