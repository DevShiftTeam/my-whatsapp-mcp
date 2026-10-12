package app

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func classifiedMemberLinkEvidence(t *testing.T) (ClassifiedHistoryRecoveryConfig, ClassifiedHistoryEvidence) {
	t.Helper()
	record := `{"event":"runtime_failure","stage":"source_apply","code":"source_operation_failed","class":"source_revision_conflict"}` + "\n"
	cfg, e := classifiedUsernameEvidenceWithConflicts(t, strings.Repeat(record, 3), 3)
	e.Witness.RuntimeBuildSHA = "028633b7af17c13f016b4b0e95ad3769c3b8dcb1"
	e.Witness.Diagnostic.RawStub = 217
	raw, err := os.ReadFile(e.DiagnosticLogPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), ":193", ":217"))
	if err = os.WriteFile(e.DiagnosticLogPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	e.Witness.DiagnosticLogSHA256 = liveRebuildDigest(raw)
	raw, err = os.ReadFile(e.LaunchEvidencePath)
	if err != nil {
		t.Fatal(err)
	}
	var launch ClassifiedHistoryLaunchEvidence
	if err = json.Unmarshal(raw, &launch); err != nil {
		t.Fatal(err)
	}
	launch.RuntimeBuildSHA = e.Witness.RuntimeBuildSHA
	raw, err = json.Marshal(launch)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(e.LaunchEvidencePath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	classifiedIntegrationResign(t, &cfg, &e)
	return cfg, e
}

func TestClassifiedHistoryMemberLinkEvidenceRequiresExactCurrentTuple(t *testing.T) {
	cfg, _ := classifiedMemberLinkEvidence(t)
	got, err := readClassifiedHistoryEvidence(cfg)
	if err != nil {
		t.Fatal("exact217 current-producer evidence refused", err)
	}
	if got.Witness.Diagnostic.RawStub != 217 || got.Witness.SourceRevisionConflicts != 3 {
		t.Fatal("actual evidence tuple rewritten")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*ClassifiedHistoryEvidence)
	}{
		{"old193tuple", func(e *ClassifiedHistoryEvidence) { e.Witness.Diagnostic.RawStub = 193 }},
		{"borrowed475", func(e *ClassifiedHistoryEvidence) { e.Witness.Diagnostic.Presence = 475 }},
		{"wrongcount", func(e *ClassifiedHistoryEvidence) { e.Witness.SourceRevisionConflicts = 2 }},
		{"unknownstub", func(e *ClassifiedHistoryEvidence) { e.Witness.Diagnostic.RawStub = 218 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, e := classifiedMemberLinkEvidence(t)
			tc.mutate(&e)
			classifiedIntegrationResign(t, &cfg, &e)
			if _, err := readClassifiedHistoryEvidence(cfg); err == nil {
				t.Fatal("mismatched current evidence accepted")
			}
		})
	}
}
