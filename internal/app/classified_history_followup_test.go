package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func TestClassifiedHistoryUsernameEvidenceRequiresExactNewTuple(t *testing.T) {
	cfg, e := syntheticClassifiedEvidence(t)
	e.Witness.Classification = archive.ClassifiedHistoryChangeUsername
	e.Witness.Diagnostic = archive.ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 193, StubPresent: true, StubKnown: true, Presence: 411}
	raw := []byte(`{"event":"history_failure","stage":"record_body_missing","stub":"other_stub","stub_type":193,"raw_stub":193,"stub_present":true,"stub_known":true,"presence":411}` + "\n")
	os.WriteFile(e.DiagnosticLogPath, raw, 0600)
	e.Witness.DiagnosticLogSHA256 = liveRebuildDigest(raw)
	classifiedIntegrationResign(t, &cfg, &e)
	if _, err := readClassifiedHistoryEvidence(cfg); err != nil {
		t.Fatal("exact username evidence refused")
	}
	e.Witness.Diagnostic.Presence = 475
	classifiedIntegrationResign(t, &cfg, &e)
	if _, err := readClassifiedHistoryEvidence(cfg); err == nil {
		t.Fatal("borrowed prior notice tuple accepted")
	}
}
func TestClassifiedHistoryNewBindingPreservesLegacySealAndRequiresFreshHash(t *testing.T) {
	paths, err := appdata.Prepare(shortNativeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	_, old := syntheticClassifiedEvidence(t)
	old.Witness.EvidenceSHA256 = strings.Repeat("a", 64)
	legacy := classifiedHistoryBinding{1, paths.Root, old.ReadySHA256, old.Witness}
	raw, _ := json.Marshal(legacy)
	legacyPath := filepath.Join(paths.WorkingDir, classifiedHistoryBindingFile)
	os.WriteFile(legacyPath, raw, 0600)
	e := old
	e.Witness.Classification = archive.ClassifiedHistoryChangeUsername
	e.Witness.EvidenceSHA256 = strings.Repeat("b", 64)
	e.Witness.RecoveryEpoch = 1
	e.Witness.UnsafeEpoch = 3
	db := []byte("synthetic coherent current archive")
	os.WriteFile(filepath.Join(paths.ArchiveDir, "archive.sqlite"), db, 0600)
	e.Witness.OriginalArchiveSHA256 = liveRebuildDigest(db)
	if err := prepareClassifiedHistoryBinding(context.Background(), paths, e); err != nil {
		t.Fatal("new evidence could not append separate seal", err)
	}
	got, _ := os.ReadFile(legacyPath)
	if !bytes.Equal(raw, got) {
		t.Fatal("old seal changed")
	}
	if _, err := os.Stat(classifiedHistoryBindingPath(paths, e.Witness.EvidenceSHA256)); err != nil {
		t.Fatal("new hash-named seal absent")
	}
	if err := prepareClassifiedHistoryBinding(context.Background(), paths, old); err != nil {
		t.Fatal("exact legacy retry lost")
	}
	bad := e
	bad.Witness.EvidenceSHA256 = strings.Repeat("c", 64)
	bad.Witness.OriginalArchiveSHA256 = strings.Repeat("d", 64)
	if err := prepareClassifiedHistoryBinding(context.Background(), paths, bad); err == nil {
		t.Fatal("old evidence waived new physical hash")
	}
	if _, err := os.Stat(classifiedHistoryBindingPath(paths, bad.Witness.EvidenceSHA256)); !os.IsNotExist(err) {
		t.Fatal("failed fresh proof created a seal")
	}
}

func classifiedUsernameEvidenceWithConflicts(t *testing.T, lines string, count uint32) (ClassifiedHistoryRecoveryConfig, ClassifiedHistoryEvidence) {
	t.Helper()
	cfg, e := syntheticClassifiedEvidence(t)
	e.Witness.Classification = archive.ClassifiedHistoryChangeUsername
	e.Witness.Diagnostic = archive.ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_body_missing", RawStub: 193, StubPresent: true, StubKnown: true, Presence: 411}
	e.Witness.SourceRevisionConflicts = count
	e.Witness.RuntimeBuildSHA = "ff0a3bd06a7f921e50d9b5c6484c481066070db3"
	raw := []byte(lines + `{"event":"history_failure","stage":"record_body_missing","stub":"other_stub","stub_type":193,"raw_stub":193,"stub_present":true,"stub_known":true,"presence":411}` + "\n")
	os.WriteFile(e.DiagnosticLogPath, raw, 0600)
	e.Witness.DiagnosticLogSHA256 = liveRebuildDigest(raw)
	raw, _ = os.ReadFile(e.LaunchEvidencePath)
	var launch ClassifiedHistoryLaunchEvidence
	json.Unmarshal(raw, &launch)
	launch.RuntimeBuildSHA = e.Witness.RuntimeBuildSHA
	raw, _ = json.Marshal(launch)
	os.WriteFile(e.LaunchEvidencePath, raw, 0600)
	classifiedIntegrationResign(t, &cfg, &e)
	return cfg, e
}
func TestClassifiedHistoryCommittedConflictsRequireExactCountAndGrammar(t *testing.T) {
	record := `{"event":"runtime_failure","stage":"source_apply","code":"source_operation_failed","class":"source_revision_conflict"}` + "\n"
	cfg, _ := classifiedUsernameEvidenceWithConflicts(t, strings.Repeat(record, 4), 4)
	if _, err := readClassifiedHistoryEvidence(cfg); err != nil {
		t.Fatal("four attested durable conflicts refused")
	}
	for _, tc := range []struct {
		name, log string
		count     uint32
	}{
		{"case_alias", strings.ReplaceAll(record, `"stage":`, `"Stage":`), 1}, {"uncounted", record, 0}, {"overcounted", record, 2}, {"other_stage", strings.ReplaceAll(record, "source_apply", "history_apply"), 1}, {"other_class", strings.ReplaceAll(record, "source_revision_conflict", "archive_unavailable"), 1}, {"extra_key", strings.ReplaceAll(record, `"event":`, `"extra":true,"event":`), 1}, {"other_code", strings.ReplaceAll(record, "source_operation_failed", "mutation_metadata_missing"), 1}, {"bound", strings.Repeat(record, 33), 33},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := classifiedUsernameEvidenceWithConflicts(t, tc.log, tc.count)
			if _, err := readClassifiedHistoryEvidence(cfg); err == nil {
				t.Fatal("unclassified or unmatched failure admitted")
			}
		})
	}
}

func TestClassifiedHistoryNamedRetryValidatesLegacySeal(t *testing.T) {
	paths, err := appdata.Prepare(shortNativeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	_, e := syntheticClassifiedEvidence(t)
	e.Witness.EvidenceSHA256 = strings.Repeat("a", 64)
	db := []byte("synthetic archive")
	if err := os.WriteFile(filepath.Join(paths.ArchiveDir, "archive.sqlite"), db, 0600); err != nil {
		t.Fatal(err)
	}
	e.Witness.OriginalArchiveSHA256 = liveRebuildDigest(db)
	if err := prepareClassifiedHistoryBinding(context.Background(), paths, e); err != nil {
		t.Fatal(err)
	}
	named := classifiedHistoryBindingPath(paths, e.Witness.EvidenceSHA256)
	before, err := os.ReadFile(named)
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(paths.WorkingDir, classifiedHistoryBindingFile)
	malformed := []byte(`{"version":1}`)
	if err := os.WriteFile(legacy, malformed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareClassifiedHistoryBinding(context.Background(), paths, e); err == nil {
		t.Fatal("named retry bypassed malformed legacy seal")
	}
	after, err := os.ReadFile(named)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("named seal changed")
	}
	after, err = os.ReadFile(legacy)
	if err != nil || !bytes.Equal(malformed, after) {
		t.Fatal("legacy seal changed")
	}
}
