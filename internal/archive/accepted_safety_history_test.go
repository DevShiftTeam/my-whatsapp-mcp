//go:build sqlite_fts5

package archive

import (
	"context"
	"testing"
)

func TestAcceptedPartialHistorySafety229ExactEvidenceAndStorage(t *testing.T) {
	s, w := classifiedRecoveryFixture(t)
	w.Classification = AcceptedPartialHistoryGap
	w.AcceptUnknownHistoryGaps = true
	w.RuntimeBuildSHA = "4588d5015b8548ab6a57a734a9a95dd433e63c4c"
	w.Diagnostic = ClassifiedHistoryDiagnostic{Event: "history_failure", Stage: "record_safety_metadata", RawStub: 229, StubPresent: true, StubKnown: false, Presence: 155}
	t.Run("exact", func(t *testing.T) {
		if _, _, err := canonicalAcceptedPartialHistoryWitness(w); err != nil {
			t.Fatal("exact owner-accepted safety shape rejected", err)
		}
	})
	for name, mutate := range map[string]func(*ClassifiedHistoryWitness){
		"body_stage":    func(v *ClassifiedHistoryWitness) { v.Diagnostic.Stage = "record_body_missing" },
		"old_producer":  func(v *ClassifiedHistoryWitness) { v.RuntimeBuildSHA = "eda63bbfbde526536685f40d6d74e6a2ef282214" },
		"known":         func(v *ClassifiedHistoryWitness) { v.Diagnostic.StubKnown = true },
		"nearby":        func(v *ClassifiedHistoryWitness) { v.Diagnostic.RawStub = 230 },
		"presence":      func(v *ClassifiedHistoryWitness) { v.Diagnostic.Presence = 411 },
		"no_acceptance": func(v *ClassifiedHistoryWitness) { v.AcceptUnknownHistoryGaps = false },
	} {
		t.Run(name, func(t *testing.T) {
			changed := w
			mutate(&changed)
			if _, _, err := canonicalAcceptedPartialHistoryWitness(changed); err == nil {
				t.Fatal("near-miss safety shape admitted")
			}
		})
	}
	before := acceptedPartialGapRows(t, s)
	if _, err := s.RestoreAcceptedPartialHistorySnapshot(context.Background(), w); err != nil {
		t.Fatal("safety recovery", err)
	}
	if before != acceptedPartialGapRows(t, s) {
		t.Fatal("unresolved safety shape falsely counted as undeclared raw229")
	}
	d, err := s.UnknownHistoryGapDiagnostics(context.Background(), "account")
	if err != nil || d.OwnerAcceptedShapeGaps != 1 || d.TotalObservations != 0 || len(d.Codes) != 0 {
		t.Fatal("safety shape diagnostics", d, err)
	}
	if _, err := s.RestoreAcceptedPartialHistorySnapshot(context.Background(), w); err != nil {
		t.Fatal("same witness retry", err)
	}
	if _, _, err := forwardBootstrapTest(t, s); err != ErrSchemaChanged {
		t.Fatal("forward lost unresolved safety shape", err)
	}
}
