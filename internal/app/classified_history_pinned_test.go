package app

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func classifiedPinnedEvidence(t *testing.T) (ClassifiedHistoryRecoveryConfig, ClassifiedHistoryEvidence) {
	t.Helper()
	cfg, e := classifiedMemberLinkEvidence(t)
	e.Witness.RuntimeBuildSHA = "30954642f78a33171d4ba1de7ae987af0b42d159"
	e.Witness.Diagnostic.RawStub = 177
	e.Witness.Classification = archive.AcceptedPartialHistoryGap
	e.Witness.AcceptUnknownHistoryGaps = true
	raw, err := os.ReadFile(e.DiagnosticLogPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), ":217", ":177"))
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

func TestAcceptedPartialHistoryPinnedEvidenceRequiresExactTupleAndProducer(t *testing.T) {
	cfg, _ := classifiedPinnedEvidence(t)
	got, err := readClassifiedHistoryEvidenceMode(cfg, true)
	if err != nil {
		t.Fatal("exact pinned notice evidence refused", err)
	}
	if _, err := readClassifiedHistoryEvidence(cfg); err == nil {
		t.Fatal("known classified route admitted accepted gap")
	}
	if got.Witness.Diagnostic.RawStub != 177 || got.Witness.SourceRevisionConflicts != 3 {
		t.Fatal("actual evidence changed")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*ClassifiedHistoryEvidence)
	}{
		{"old_tuple", func(e *ClassifiedHistoryEvidence) { e.Witness.Diagnostic.RawStub = 217 }},
		{"wrong_presence", func(e *ClassifiedHistoryEvidence) { e.Witness.Diagnostic.Presence = 475 }},
		{"conflict_count", func(e *ClassifiedHistoryEvidence) { e.Witness.SourceRevisionConflicts = 2 }},
		{"unreviewed_producer", func(e *ClassifiedHistoryEvidence) { e.Witness.RuntimeBuildSHA = strings.Repeat("a", 40) }},
		{"missing_consent", func(e *ClassifiedHistoryEvidence) { e.Witness.AcceptUnknownHistoryGaps = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, e := classifiedPinnedEvidence(t)
			tc.mutate(&e)
			classifiedIntegrationResign(t, &cfg, &e)
			if _, err := readClassifiedHistoryEvidenceMode(cfg, true); err == nil {
				t.Fatal("mismatched pinned evidence accepted")
			}
		})
	}
}

func TestAcceptedPartialHistoryPinnedRequiresOperatorConsent(t *testing.T) {
	cfg, e := classifiedPinnedEvidence(t)
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
	if _, err := readClassifiedHistoryEvidenceMode(cfg, true); err == nil {
		t.Fatal("missing operator acceptance admitted")
	}
}
