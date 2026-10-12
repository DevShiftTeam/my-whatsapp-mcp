package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

const acceptedPartialSyntheticProducer = "eda63bbfbde526536685f40d6d74e6a2ef282214"

func TestAcceptedPartialHistoryRequiresReviewedProducerBuild(t *testing.T) {
	if !archive.AcceptedPartialHistoryProducerSupported(acceptedPartialSyntheticProducer) {
		t.Fatal("reviewed unknown-history diagnostic producer is unavailable")
	}
	for _, build := range []string{"", "eda", strings.Repeat("3", 40), strings.ToUpper(acceptedPartialSyntheticProducer), acceptedPartialSyntheticProducer + "-dirty"} {
		if archive.AcceptedPartialHistoryProducerSupported(build) {
			t.Fatal("unreviewed producer accepted")
		}
	}
}

func TestAcceptedPartialHistoryDiagnosticRemainsSeparateFromKnownNotices(t *testing.T) {
	unknown := archive.ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 229, StubPresent: true, StubKnown: false, Presence: 155}
	if !archive.AcceptedPartialHistoryDiagnosticAllowed(unknown) {
		t.Fatal("explicit owner partial-acceptance diagnostic is unavailable")
	}
	if archive.ClassifiedHistoryDiagnosticAllowed(archive.ClassifiedHistoryChangeUsername, unknown) || archive.ClassifiedHistoryDiagnosticAllowed(archive.ClassifiedHistoryGroupParticipantChangeNumber, unknown) {
		t.Fatal("unknown content became a classified known notice")
	}
	for _, test := range []struct {
		name   string
		change func(*archive.ClassifiedHistoryDiagnostic)
	}{
		{"known", func(d *archive.ClassifiedHistoryDiagnostic) { d.StubKnown = true }},
		{"absent_stub", func(d *archive.ClassifiedHistoryDiagnostic) { d.StubPresent = false }},
		{"zero_stub", func(d *archive.ClassifiedHistoryDiagnostic) { d.RawStub = 0 }},
		{"known_notice", func(d *archive.ClassifiedHistoryDiagnostic) { d.RawStub = 217 }},
		{"wrong_stage", func(d *archive.ClassifiedHistoryDiagnostic) { d.Stage = "protocol_metadata" }},
		{"body_present", func(d *archive.ClassifiedHistoryDiagnostic) { d.Presence |= 4 }},
		{"control_present", func(d *archive.ClassifiedHistoryDiagnostic) { d.Presence |= 512 }},
		{"wrong_event", func(d *archive.ClassifiedHistoryDiagnostic) { d.Event = "runtime_failure" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := unknown
			test.change(&changed)
			if archive.AcceptedPartialHistoryDiagnosticAllowed(changed) {
				t.Fatal("partial acceptance broadened beyond reviewed diagnostic evidence")
			}
		})
	}
}

func TestAcceptedPartialHistoryLegacyWitnessOmitsNewAcceptanceAuthority(t *testing.T) {
	cfg, e := syntheticClassifiedEvidence(t)
	if e.Witness.AcceptUnknownHistoryGaps {
		t.Fatal("legacy known-notice fixture gained partial acceptance authority")
	}
	raw, err := json.Marshal(e)
	if err != nil || strings.Contains(string(raw), "accept_unknown_history_gaps") {
		t.Fatal("zero acceptance field changed legacy evidence encoding", err)
	}
	if _, err := readClassifiedHistoryEvidence(cfg); err != nil {
		t.Fatal("legacy known-notice evidence no longer validates", err)
	}
}

func acceptedPartialFreshFence(t *testing.T, f classifiedIntegrationFixture, classification string, stub int, presence uint64, known, accept bool) (ClassifiedHistoryRecoveryConfig, ClassifiedHistoryEvidence) {
	t.Helper()
	ctx := context.Background()
	s, err := archive.Open(ctx, f.paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginObservation(ctx, f.bootstrap.ExpectedAccountID); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.MarkUnsafe(ctx, f.bootstrap.ExpectedAccountID, "history_corrupt"); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	cfg, e := syntheticClassifiedEvidence(t)
	cfg.DataDir = f.paths.Root
	e.ReadySHA256 = f.evidence.ReadySHA256
	w := &e.Witness
	w.Classification, w.AcceptUnknownHistoryGaps = classification, accept
	w.RebuildGeneration = f.evidence.Witness.RebuildGeneration
	w.Diagnostic = archive.ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: stub, StubPresent: true, StubKnown: known, Presence: presence}
	stubType := stub
	if !known {
		stubType = -1
		w.RuntimeBuildSHA = acceptedPartialSyntheticProducer
		launchRaw, err := os.ReadFile(e.LaunchEvidencePath)
		var launch ClassifiedHistoryLaunchEvidence
		if err != nil || json.Unmarshal(launchRaw, &launch) != nil {
			t.Fatal("invalid synthetic launch evidence", err)
		}
		launch.RuntimeBuildSHA = w.RuntimeBuildSHA
		launchRaw, err = json.Marshal(launch)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.LaunchEvidencePath, launchRaw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	log := []byte(fmt.Sprintf("{\"event\":\"history_failure\",\"stage\":\"record_body_missing\",\"stub\":\"other_stub\",\"stub_type\":%d,\"raw_stub\":%d,\"stub_present\":true,\"stub_known\":%t,\"presence\":%d}\n", stubType, stub, known, presence))
	if err := os.WriteFile(e.DiagnosticLogPath, log, 0600); err != nil {
		t.Fatal(err)
	}
	w.DiagnosticLogSHA256 = liveRebuildDigest(log)
	db := classifiedIntegrationDB(t, f.paths)
	if err := db.QueryRow("SELECT epoch FROM account_safety WHERE account=?", f.bootstrap.ExpectedAccountID).Scan(&w.UnsafeEpoch); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT archive_revision,recovery_epoch FROM archive_meta WHERE id=1").Scan(&w.ArchiveRevision, &w.RecoveryEpoch); err != nil {
		t.Fatal(err)
	}
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
	classifiedIntegrationResign(t, &cfg, &e)
	return cfg, e
}

func TestAcceptedPartialHistoryRecoveryRequiresNewOwnerAcceptanceAndPreservesState(t *testing.T) {
	ctx := context.Background()
	f, secondConfig, _ := classifiedFollowupFixture(t)
	if second, err := RecoverClassifiedHistorySnapshot(ctx, secondConfig); err != nil || second.RecoveryEpoch != 2 {
		t.Fatal("known193 acceptance", second, err)
	}
	thirdConfig, _ := acceptedPartialFreshFence(t, f, archive.ClassifiedHistoryChangeUsername, 217, 411, true, false)
	if third, err := RecoverClassifiedHistorySnapshot(ctx, thirdConfig); err != nil || third.RecoveryEpoch != 3 {
		t.Fatal("known217 acceptance", third, err)
	}
	priorSeals := classifiedFollowupBindings(t, f.paths.WorkingDir)
	if len(priorSeals) != 3 {
		t.Fatal("missing preceding known-notice seals")
	}
	s, err := archive.Open(ctx, f.paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := s.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := s.RebuildProgress(ctx)
	if err != nil || progress == nil {
		t.Fatal("missing recovery operation", err)
	}
	candidates, err := s.NextRebuildCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 {
		t.Fatal("missing synthetic candidate", err)
	}
	if err := s.PendingRebuildRequest(ctx, archive.RebuildRequest{ID: "partial-preserved-request", Generation: progress.Generation, OperationID: progress.OperationID, OperationRevision: progress.Revision, Key: candidates[0].Key, PolicyGeneration: progress.PolicyGeneration, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: f.bootstrap.ExpectedAccountID, Chat: "15550000002@s.whatsapp.net", Sender: "15550000002@s.whatsapp.net", Message: "partial-current-body"}, Kind: archive.EventOriginal, Body: "synthetic body survives owner partial acceptance", BodyKind: archive.MessageText, OriginalSentAt: time.Now(), OriginalDateKnown: true, SourceRevision: 1}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
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
		if _, err := db.Exec("INSERT INTO send_requests(credential_id,request_id,account,chat_id,digest,protocol_id,state,accepted_at) VALUES(?,?,?,?,?,?,?,0)", credential, "partial-"+outcome, f.bootstrap.ExpectedAccountID, chat, make([]byte, 32), "synthetic-"+outcome, outcome); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cfg, evidence := acceptedPartialFreshFence(t, f, archive.AcceptedPartialHistoryGap, 229, 155, false, true)
	if evidence.Witness.RecoveryEpoch != 3 {
		t.Fatal("unknown fence was not captured after the third recovery")
	}
	db = classifiedIntegrationDB(t, f.paths)
	tables := []string{"archive_policy", "policy_chats", "credentials", "credential_chats", "send_permissions", "send_permission_modes", "send_requests", "extension_original_budget", "messages", "rebuild_provenance", "rebuild_guards", "rebuild_requests", "rebuild_operation", "rebuild_commands", "rebuild_discovery_attempts"}
	before := map[string]string{}
	for _, table := range tables {
		before[table] = classifiedNoticesRows(t, db, "SELECT * FROM "+table+" ORDER BY rowid")
	}
	priorAudits := classifiedNoticesRows(t, db, "SELECT * FROM snapshot_recovery_audit WHERE recovery_epoch<=3 ORDER BY recovery_epoch")
	var messages, spent int
	if err := db.QueryRow("SELECT count(*) FROM messages").Scan(&messages); err != nil || messages == 0 {
		t.Fatal("missing current body fixture", err)
	}
	if err := db.QueryRow("SELECT copy_spent FROM rebuild_operation WHERE id=1").Scan(&spent); err != nil || spent != 1 {
		t.Fatal("missing spent recovery budget", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fenced := classifiedFollowupFiles(t, f.paths.Root)
	if _, err := RecoverClassifiedHistorySnapshot(ctx, cfg); err == nil {
		t.Fatal("known-notice operation accepted unknown content")
	}
	if _, err := RecoverClassifiedHistorySnapshot(ctx, thirdConfig); err == nil {
		t.Fatal("old known217 evidence authorized the new unknown fence")
	}
	denied := evidence
	denied.Witness.AcceptUnknownHistoryGaps = false
	classifiedIntegrationResign(t, &cfg, &denied)
	if _, err := RecoverAcceptedPartialHistorySnapshot(ctx, cfg); err == nil {
		t.Fatal("partial acceptance proceeded without explicit owner consent")
	}
	classifiedFollowupAssertExistingFiles(t, fenced)
	classifiedIntegrationResign(t, &cfg, &evidence)
	for _, kind := range []string{"operator_denial", "operator_malformed", "operator_launch_mismatch"} {
		t.Run(kind, func(t *testing.T) {
			badConfig, badEvidence := cfg, evidence
			classifiedIntegrationResign(t, &badConfig, &badEvidence)
			raw, err := os.ReadFile(badEvidence.OperatorReceiptPath)
			if err != nil {
				t.Fatal(err)
			}
			var operator map[string]any
			if err := json.Unmarshal(raw, &operator); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "operator_denial":
				operator["accept_unknown_history_gaps"] = false
			case "operator_launch_mismatch":
				operator["launch_sha256"] = strings.Repeat("0", 64)
			}
			raw, err = json.Marshal(operator)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "operator_malformed" {
				raw = []byte("{")
			}
			if err := os.WriteFile(badEvidence.OperatorReceiptPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			// Keep both digests honest so rejection exercises the operator
			// attestation itself, not merely a changed-file hash mismatch.
			badEvidence.Witness.OperatorReceiptSHA256 = liveRebuildDigest(raw)
			raw, err = json.Marshal(badEvidence)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(badConfig.EvidencePath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			badConfig.ExpectedEvidenceSHA256 = liveRebuildDigest(raw)
			if !badEvidence.Witness.AcceptUnknownHistoryGaps {
				t.Fatal("negative fixture lost witness acceptance")
			}
			if _, err := RecoverAcceptedPartialHistorySnapshot(ctx, badConfig); err == nil {
				t.Fatal("witness acceptance bypassed an invalid operator receipt")
			}
			classifiedFollowupAssertExistingFiles(t, fenced)
		})
	}
	classifiedIntegrationResign(t, &cfg, &evidence)
	receipt, err := RecoverAcceptedPartialHistorySnapshot(ctx, cfg)
	if err != nil || receipt.RecoveryEpoch != 4 || uint64(receipt.ArchiveRevision) <= evidence.Witness.ArchiveRevision || receipt.Mode != "committed_snapshot" {
		t.Fatal("explicit unknown-history partial acceptance failed", receipt, err)
	}
	f.assertPreserved(t)
	classifiedFollowupAssertExistingFiles(t, priorSeals)
	db = classifiedIntegrationDB(t, f.paths)
	for _, table := range tables {
		if got := classifiedNoticesRows(t, db, "SELECT * FROM "+table+" ORDER BY rowid"); got != before[table] {
			t.Fatal("partial acceptance changed preserved table", table)
		}
	}
	if got := classifiedNoticesRows(t, db, "SELECT * FROM snapshot_recovery_audit WHERE recovery_epoch<=3 ORDER BY recovery_epoch"); got != priorAudits {
		t.Fatal("partial acceptance rewrote a prior known-notice audit")
	}
	var witnessJSON string
	if err := db.QueryRow("SELECT witness_json FROM snapshot_recovery_audit WHERE recovery_epoch=4 AND phase='complete'").Scan(&witnessJSON); err != nil {
		t.Fatal(err)
	}
	var recorded archive.ClassifiedHistoryWitness
	if err := json.Unmarshal([]byte(witnessJSON), &recorded); err != nil || !recorded.AcceptUnknownHistoryGaps || recorded.Diagnostic.StubKnown || recorded.Diagnostic.RawStub != 229 {
		t.Fatal("audit lost explicit acceptance or falsely classified the unknown diagnostic", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if again, err := RecoverAcceptedPartialHistorySnapshot(ctx, cfg); err != nil || again != receipt {
		t.Fatal("exact partial acceptance retry failed", again, err)
	}
	s, err = archive.Open(ctx, f.paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetSyncStatus(ctx, archive.SyncUpdate{Account: f.bootstrap.ExpectedAccountID, Connection: "connected"}); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx, archive.ReadScope{Account: f.bootstrap.ExpectedAccountID, Principal: "owner", PolicyGeneration: policy.Generation, AllChats: true})
	if err != nil || status.Coverage != "partial" || !status.Snapshot.UnknownHistoryGap || status.Snapshot.CoverageEpoch == 0 {
		t.Fatal("reopen or later sync lost sticky partial coverage", status.Coverage, err)
	}
	diagnostics, err := s.UnknownHistoryGapDiagnostics(ctx, f.bootstrap.ExpectedAccountID)
	if err != nil || diagnostics == nil || diagnostics.TotalObservations != 1 || len(diagnostics.Codes) != 1 || diagnostics.Codes[0].RawStub != 229 {
		t.Fatal("retry duplicated or lost the accepted unknown gap", diagnostics, err)
	}
	f.assertPreserved(t)
}
