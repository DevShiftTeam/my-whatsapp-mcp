package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDoctorReportsActualSetupWithoutClaimingNativeAcceptance(t *testing.T) {
	root := privateTestRoot(t)
	a, err := New(context.Background(), Config{DataDir: root, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	report, err := a.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Archive != "owned" || report.Connection != "setup_required" || report.NativeAcceptance != "NOT_RUN" || report.NativeClients != "recovery_pending" || report.KeywordAvailable {
		t.Fatal("doctor misrepresented readiness")
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), root) || strings.Contains(string(encoded), "token") || strings.Contains(string(encoded), "sha256") {
		t.Fatal("doctor exposed private authority")
	}
}

func TestDoctorConfiguredBoundKeywordAvailability(t *testing.T) {
	a := pairedNativeTestApp(t)
	report, err := a.Doctor(context.Background())
	if err != nil || !report.KeywordAvailable {
		t.Fatal("configured bound keyword scope unavailable")
	}
}
