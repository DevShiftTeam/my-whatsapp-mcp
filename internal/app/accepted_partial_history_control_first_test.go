package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

const acceptedControlFirstProducer = "4588d5015b8548ab6a57a734a9a95dd433e63c4c"

// This is an attestation of an unresolved historical shape, not a fixture that
// claims the lost wire record would pass a later parser.
func acceptedControlFirstEvidence(t *testing.T, cfg *ClassifiedHistoryRecoveryConfig, e *ClassifiedHistoryEvidence) {
	t.Helper()
	e.Witness.Classification = archive.AcceptedPartialHistoryGap
	e.Witness.AcceptUnknownHistoryGaps = true
	e.Witness.RuntimeBuildSHA = acceptedControlFirstProducer
	e.Witness.Diagnostic = archive.ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_safety_metadata", RawStub: 229, StubPresent: true, StubKnown: false, Presence: 155}
	acceptedControlFirstResign(t, cfg, e)
}

func acceptedControlFirstResign(t *testing.T, cfg *ClassifiedHistoryRecoveryConfig, e *ClassifiedHistoryEvidence) {
	t.Helper()
	raw, err := os.ReadFile(e.LaunchEvidencePath)
	var launch ClassifiedHistoryLaunchEvidence
	if err != nil || json.Unmarshal(raw, &launch) != nil {
		t.Fatal("read synthetic launch", err)
	}
	launch.RuntimeBuildSHA = e.Witness.RuntimeBuildSHA
	raw, err = json.Marshal(launch)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.LaunchEvidencePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	d := e.Witness.Diagnostic
	stubType := d.RawStub
	if !d.StubKnown {
		stubType = -1
	}
	log := []byte(fmt.Sprintf("{\"event\":%q,\"stage\":%q,\"stub\":\"other_stub\",\"stub_type\":%d,\"raw_stub\":%d,\"stub_present\":%t,\"stub_known\":%t,\"presence\":%d}\n", d.Event, d.Stage, stubType, d.RawStub, d.StubPresent, d.StubKnown, d.Presence))
	if err := os.WriteFile(e.DiagnosticLogPath, log, 0600); err != nil {
		t.Fatal(err)
	}
	e.Witness.DiagnosticLogSHA256 = liveRebuildDigest(log)
	classifiedIntegrationResign(t, cfg, e)
}

func TestAcceptedPartialControlFirstEvidenceRequiresExactReviewedTuple(t *testing.T) {
	for _, fixture := range []struct {
		name, producer, stage string
		raw                   int
		known                 bool
		presence              uint64
	}{
		{"unresolved_shape", acceptedControlFirstProducer, "record_safety_metadata", 229, false, 155},
		{"unresolved_shape_with_expiry_deadlines", acceptedControlFirstProducer, "record_safety_metadata", 229, false, 155},
		{"legacy_unknown", acceptedPartialSyntheticProducer, "record_body_missing", 229, false, 155},
		{"legacy_pinned_shape", "30954642f78a33171d4ba1de7ae987af0b42d159", "record_body_missing", 177, true, 411},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			cfg, e := syntheticClassifiedEvidence(t)
			acceptedControlFirstEvidence(t, &cfg, &e)
			e.Witness.RuntimeBuildSHA = fixture.producer
			e.Witness.Diagnostic.Stage, e.Witness.Diagnostic.RawStub = fixture.stage, fixture.raw
			e.Witness.Diagnostic.StubKnown, e.Witness.Diagnostic.Presence = fixture.known, fixture.presence
			acceptedControlFirstResign(t, &cfg, &e)
			if fixture.name == "unresolved_shape_with_expiry_deadlines" {
				log, err := os.ReadFile(e.DiagnosticLogPath)
				if err != nil {
					t.Fatal(err)
				}
				deadline := "{\"event\":\"archive_failure\",\"stage\":\"expiry_maintenance_timer\",\"code\":\"expiry_cleanup_failed\",\"class\":\"deadline\"}\n"
				log = append([]byte(deadline+deadline), log...)
				if err := os.WriteFile(e.DiagnosticLogPath, log, 0600); err != nil {
					t.Fatal(err)
				}
				e.Witness.DiagnosticLogSHA256 = liveRebuildDigest(log)
				classifiedIntegrationResign(t, &cfg, &e)
			}
			got, err := readClassifiedHistoryEvidenceMode(cfg, true)
			if err != nil || got.Witness.Diagnostic != e.Witness.Diagnostic || got.Witness.RuntimeBuildSHA != fixture.producer {
				t.Fatal("exact owner-accepted diagnostic refused", err)
			}
			if _, err := readClassifiedHistoryEvidenceMode(cfg, false); err == nil {
				t.Fatal("known-notice API admitted owner-accepted unresolved content")
			}
		})
	}
}

func TestAcceptedPartialControlFirstEvidenceRejectsNearMisses(t *testing.T) {
	for _, kind := range []string{"other_producer", "old_producer", "old_stage", "raw", "known", "absent", "presence", "witness_denial", "operator_denial", "log_stage", "log_known", "duplicate_failure", "extra_log_field"} {
		t.Run(kind, func(t *testing.T) {
			cfg, e := syntheticClassifiedEvidence(t)
			acceptedControlFirstEvidence(t, &cfg, &e)
			switch kind {
			case "other_producer":
				e.Witness.RuntimeBuildSHA = strings.Repeat("f", 40)
			case "old_producer":
				e.Witness.RuntimeBuildSHA = acceptedPartialSyntheticProducer
			case "old_stage":
				e.Witness.Diagnostic.Stage = "record_body_missing"
			case "raw":
				e.Witness.Diagnostic.RawStub = 228
			case "known":
				e.Witness.Diagnostic.StubKnown = true
			case "absent":
				e.Witness.Diagnostic.StubPresent = false
			case "presence":
				e.Witness.Diagnostic.Presence = 411
			case "witness_denial":
				e.Witness.AcceptUnknownHistoryGaps = false
			}
			acceptedControlFirstResign(t, &cfg, &e)
			if strings.HasPrefix(kind, "log_") || kind == "duplicate_failure" || kind == "extra_log_field" {
				raw, err := os.ReadFile(e.DiagnosticLogPath)
				if err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "log_stage":
					raw = []byte(strings.ReplaceAll(string(raw), "record_safety_metadata", "record_body_missing"))
				case "log_known":
					raw = []byte(strings.ReplaceAll(string(raw), `"stub_known":false`, `"stub_known":true`))
				case "duplicate_failure":
					raw = append(raw, raw...)
				case "extra_log_field":
					raw = append([]byte(`{"safe":true,`), raw[1:]...)
				}
				if err := os.WriteFile(e.DiagnosticLogPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
				e.Witness.DiagnosticLogSHA256 = liveRebuildDigest(raw)
				classifiedIntegrationResign(t, &cfg, &e)
			}
			if kind == "operator_denial" {
				raw, err := os.ReadFile(e.OperatorReceiptPath)
				if err != nil {
					t.Fatal(err)
				}
				var receipt ClassifiedHistoryOperatorReceipt
				if err := json.Unmarshal(raw, &receipt); err != nil {
					t.Fatal(err)
				}
				receipt.AcceptUnknownHistoryGaps = false
				raw, err = json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(e.OperatorReceiptPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
				e.Witness.OperatorReceiptSHA256 = liveRebuildDigest(raw)
				raw, err = json.Marshal(e)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfg.EvidencePath, raw, 0600); err != nil {
					t.Fatal(err)
				}
				cfg.ExpectedEvidenceSHA256 = liveRebuildDigest(raw)
			}
			if _, err := readClassifiedHistoryEvidenceMode(cfg, true); err == nil {
				t.Fatal("near-miss evidence admitted", kind)
			}
		})
	}
}

func TestAcceptedPartialControlFirstRecoveryPreservesPriorAuthority(t *testing.T) {
	ctx := context.Background()
	f, second, _ := classifiedFollowupFixture(t)
	if receipt, err := RecoverClassifiedHistorySnapshot(ctx, second); err != nil || receipt.RecoveryEpoch != 2 {
		t.Fatal("known193 recovery", receipt, err)
	}
	third, _ := acceptedPartialFreshFence(t, f, archive.ClassifiedHistoryChangeUsername, 217, 411, true, false)
	if receipt, err := RecoverClassifiedHistorySnapshot(ctx, third); err != nil || receipt.RecoveryEpoch != 3 {
		t.Fatal("known217 recovery", receipt, err)
	}
	priorSeals := classifiedFollowupBindings(t, f.paths.WorkingDir)
	store, err := archive.Open(ctx, f.paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	progress, err := store.RebuildProgress(ctx)
	if err != nil || progress == nil {
		t.Fatal("missing operation", err)
	}
	candidates, err := store.NextRebuildCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 {
		t.Fatal("missing candidate", err)
	}
	if err := store.PendingRebuildRequest(ctx, archive.RebuildRequest{ID: "shape-preserved-pending", Generation: progress.Generation, OperationID: progress.OperationID, OperationRevision: progress.Revision, Key: candidates[0].Key, PolicyGeneration: progress.PolicyGeneration, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db := classifiedIntegrationDB(t, f.paths)
	var credential, chat string
	if err := db.QueryRow("SELECT id FROM credentials ORDER BY id LIMIT 1").Scan(&credential); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT id FROM chats ORDER BY id LIMIT 1").Scan(&chat); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"accepted", "failed", "outcome_unknown"} {
		if _, err := db.Exec("INSERT INTO send_requests(credential_id,request_id,account,chat_id,digest,protocol_id,state,accepted_at) VALUES(?,?,?,?,?,?,?,0)", credential, "shape-"+outcome, f.bootstrap.ExpectedAccountID, chat, make([]byte, 32), "synthetic-"+outcome, outcome); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cfg, e := acceptedPartialFreshFence(t, f, archive.AcceptedPartialHistoryGap, 229, 155, false, true)
	acceptedControlFirstEvidence(t, &cfg, &e)
	db = classifiedIntegrationDB(t, f.paths)
	tables := []string{"accounts", "archive_policy", "policy_chats", "credentials", "credential_chats", "send_permissions", "send_permission_modes", "send_requests", "extension_original_budget", "messages", "rebuild_provenance", "rebuild_guards", "rebuild_requests", "rebuild_operation", "rebuild_commands", "rebuild_discovery_attempts", "unknown_history_coverage", "unknown_history_codes"}
	before := map[string]string{}
	for _, table := range tables {
		before[table] = classifiedNoticesRows(t, db, "SELECT * FROM "+table+" ORDER BY rowid")
	}
	priorAudits := classifiedNoticesRows(t, db, "SELECT * FROM snapshot_recovery_audit WHERE recovery_epoch<=3 ORDER BY recovery_epoch")
	var spent int
	if err := db.QueryRow("SELECT copy_spent FROM rebuild_operation WHERE id=1").Scan(&spent); err != nil || spent != 1 {
		t.Fatal("missing spent request", spent, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fencedFiles := classifiedFollowupFiles(t, f.paths.Root)
	if _, err := RecoverClassifiedHistorySnapshot(ctx, cfg); err == nil {
		t.Fatal("known API admitted unresolved shape")
	}
	if _, err := RecoverClassifiedHistorySnapshot(ctx, third); err == nil {
		t.Fatal("old known evidence authorized fresh fence")
	}
	classifiedFollowupAssertExistingFiles(t, fencedFiles)
	receipt, err := RecoverAcceptedPartialHistorySnapshot(ctx, cfg)
	if err != nil || receipt.RecoveryEpoch != 4 || uint64(receipt.ArchiveRevision) != e.Witness.ArchiveRevision+1 {
		t.Fatal("owner-accepted unresolved-shape recovery", receipt, err)
	}
	f.assertPreserved(t)
	classifiedFollowupAssertExistingFiles(t, priorSeals)
	db = classifiedIntegrationDB(t, f.paths)
	for _, table := range tables {
		if got := classifiedNoticesRows(t, db, "SELECT * FROM "+table+" ORDER BY rowid"); got != before[table] {
			t.Fatal("changed preserved table", table)
		}
	}
	if got := classifiedNoticesRows(t, db, "SELECT * FROM snapshot_recovery_audit WHERE recovery_epoch<=3 ORDER BY recovery_epoch"); got != priorAudits {
		t.Fatal("rewrote prior known audits")
	}
	var gap int
	if err := db.QueryRow("SELECT count(*) FROM sync_gaps WHERE account=? AND chat='' AND code='owner_accepted_history_gap'", f.bootstrap.ExpectedAccountID).Scan(&gap); err != nil || gap != 1 {
		t.Fatal("missing unresolved shape gap", gap, err)
	}
	var encoded string
	if err := db.QueryRow("SELECT witness_json FROM snapshot_recovery_audit WHERE recovery_epoch=4 AND phase='complete'").Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var recorded archive.ClassifiedHistoryWitness
	if err := json.Unmarshal([]byte(encoded), &recorded); err != nil || recorded.Diagnostic != e.Witness.Diagnostic || recorded.RuntimeBuildSHA != acceptedControlFirstProducer || !recorded.AcceptUnknownHistoryGaps {
		t.Fatal("audit lost unresolved diagnostic and consent", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if again, err := RecoverAcceptedPartialHistorySnapshot(ctx, cfg); err != nil || again != receipt {
		t.Fatal("exact retry", again, err)
	}
	if len(classifiedFollowupBindings(t, f.paths.WorkingDir)) != 4 {
		t.Fatal("retry duplicated evidence seal")
	}
	f.assertPreserved(t)
}
