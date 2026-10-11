package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"go.mau.fi/whatsmeow/proto/waWeb"
)

type privateHistoryError struct{}

func (privateHistoryError) Error() string { panic("history diagnostic formatted a provider error") }

func TestHistoryDiagnosticsUseClosedValuesAndPreserveCauses(t *testing.T) {
	for _, tc := range []struct {
		kind waWeb.WebMessageInfo_StubType
		want string
	}{
		{waWeb.WebMessageInfo_UNKNOWN, "no_stub"},
		{waWeb.WebMessageInfo_REVOKE, "revoke"},
		{waWeb.WebMessageInfo_ADMIN_REVOKE, "admin_revoke"},
		{waWeb.WebMessageInfo_CIPHERTEXT, "ciphertext"},
		{waWeb.WebMessageInfo_StubType(987654), "other_stub"},
	} {
		if got := historyStubClass(&waWeb.WebMessageInfo{MessageStubType: &tc.kind}); got != tc.want {
			t.Fatalf("stub class: got %s want %s", got, tc.want)
		}
	}
	for _, cause := range []error{errLimitExceeded, context.Canceled, context.DeadlineExceeded, privateHistoryError{}} {
		err := historyStage(cause, "record_decode", "no_stub")
		if !errors.Is(err, cause) || err.Error() != "record_decode" {
			t.Fatal("diagnostic wrapping changed cause or formatted provider data")
		}
		if string(historyFailureDiagnostic(historyStage(err, "mapping", "revoke"))) != "{\"event\":\"history_failure\",\"stage\":\"record_decode\",\"stub\":\"no_stub\",\"stub_type\":-1}\n" {
			t.Fatal("outer wrapping replaced the precise failure stage")
		}
	}
	encoded := historyFailureDiagnostic(&historyStageError{stage: "SYNTHETIC_PRIVATE_STAGE", stub: "SYNTHETIC_PRIVATE_STUB", stubType: 987654, cause: privateHistoryError{}})
	if string(encoded) != "{\"event\":\"history_failure\",\"stage\":\"unknown\",\"stub\":\"no_stub\",\"stub_type\":-1}\n" {
		t.Fatal("diagnostic admitted an unrecognized value")
	}
	if got := historyFailureDiagnostic(historyBodyMissing(&waWeb.WebMessageInfo{MessageStubType: waWeb.WebMessageInfo_GROUP_CREATE.Enum()})); !strings.Contains(string(got), "\"stub_type\":20") {
		t.Fatal("known other stub lost its finite protocol enum")
	}
}

func TestBodylessHistoryDiagnosticIsPreciseStickyAndStillFatal(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal("diagnostic pipe setup failed")
	}
	previous := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = previous; _ = read.Close(); _ = write.Close() }()
	s := fixtureStore()
	r := testRuntime(s)
	record := historyRecord("synthetic-id", "")
	record.Message.Message = nil
	record.Message.MessageStubType = waWeb.WebMessageInfo_UNKNOWN.Enum()
	err = r.projectHistory(context.Background(), historyWire(record))
	if !errors.Is(err, errHistoryCorrupt) || !r.Status().ReconciliationRequired || len(s.unsafe) == 0 {
		t.Fatal("diagnostic pass relaxed the existing history fence")
	}
	_ = r.projectHistory(context.Background(), []byte{0xff})
	os.Stderr = previous
	_ = write.Close()
	encoded, err := io.ReadAll(io.LimitReader(read, 512))
	if err != nil || strings.Count(string(encoded), "\n") != 1 {
		t.Fatal("later fenced history emitted another diagnostic")
	}
	var diagnostic map[string]any
	if json.Unmarshal(encoded, &diagnostic) != nil || len(diagnostic) != 4 || diagnostic["event"] != "history_failure" || diagnostic["stage"] != "record_body_missing" || diagnostic["stub"] != "no_stub" || diagnostic["stub_type"] != float64(0) {
		t.Fatal("bodyless history did not retain its precise closed stage")
	}
}

func TestHistoryAdminFailureDiagnosticHasClosedReason(t *testing.T) {
	const private = "SYNTHETIC_PRIVATE_ADMIN_DIAGNOSTIC_CANARY"
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal("diagnostic pipe setup failed")
	}
	previous := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = previous; _ = read.Close(); _ = write.Close() }()
	store := fixtureStore()
	r := testRuntime(store)
	stub := bodylessHistoryStub("300@g.us", private, waWeb.WebMessageInfo_ADMIN_REVOKE)
	actor := "@s.whatsapp.net"
	name := private
	stub.Message.Participant = &actor
	stub.Message.PushName = &name
	stub.Message.MessageStubParameters = []string{private}
	err = r.projectHistory(context.Background(), bodylessHistoryWire(t, "300@g.us", stub))
	if !errors.Is(err, archive.ErrInvalidEvent) || !r.Status().ReconciliationRequired || len(store.unsafe) == 0 || len(store.events) != 0 {
		t.Fatal("diagnostic pass changed malformed administrator-deletion behavior")
	}
	os.Stderr = previous
	_ = write.Close()
	encoded, err := io.ReadAll(io.LimitReader(read, 512))
	if err != nil || strings.Contains(string(encoded), private) || strings.Contains(string(encoded), actor) {
		t.Fatal("administrator diagnostic exposed fixture identities or parameters")
	}
	if string(encoded) != "{\"event\":\"history_failure\",\"stage\":\"admin_actor_invalid\",\"stub\":\"admin_revoke\",\"stub_type\":132}\n" {
		t.Fatal("administrator diagnostic did not preserve its precise closed cause and enum")
	}
}
