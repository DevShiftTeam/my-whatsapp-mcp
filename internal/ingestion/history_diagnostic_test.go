package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
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
		if string(historyFailureDiagnostic(historyStage(err, "mapping", "revoke"))) != "{\"event\":\"history_failure\",\"stage\":\"record_decode\",\"stub\":\"no_stub\",\"stub_type\":-1,\"raw_stub\":0,\"stub_present\":false,\"stub_known\":false,\"presence\":0}\n" {
			t.Fatal("outer wrapping replaced the precise failure stage")
		}
	}
	encoded := historyFailureDiagnostic(&historyStageError{stage: "SYNTHETIC_PRIVATE_STAGE", stub: "SYNTHETIC_PRIVATE_STUB", stubType: 987654, cause: privateHistoryError{}})
	if string(encoded) != "{\"event\":\"history_failure\",\"stage\":\"unknown\",\"stub\":\"no_stub\",\"stub_type\":-1,\"raw_stub\":0,\"stub_present\":false,\"stub_known\":false,\"presence\":0}\n" {
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
	if err != nil {
		t.Fatal("diagnostic read failed")
	}
	history, runtimeCount := historyDiagnosticFrames(t, encoded)
	if len(history) != 1 || runtimeCount != 2 {
		t.Fatal("later fenced history emitted another diagnostic")
	}
	var diagnostic map[string]any
	if json.Unmarshal(history[0], &diagnostic) != nil || len(diagnostic) != 8 || diagnostic["event"] != "history_failure" || diagnostic["stage"] != "record_body_missing" || diagnostic["stub"] != "no_stub" || diagnostic["stub_type"] != float64(0) {
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
	history, runtimeCount := historyDiagnosticFrames(t, encoded)
	if len(history) != 1 || runtimeCount != 1 || string(history[0]) != "{\"event\":\"history_failure\",\"stage\":\"admin_actor_invalid\",\"stub\":\"admin_revoke\",\"stub_type\":132,\"raw_stub\":0,\"stub_present\":false,\"stub_known\":false,\"presence\":0}" {
		t.Fatal("administrator diagnostic did not preserve its precise closed cause and enum")
	}
}

// A sticky history witness is distinct from the runtime's per-entry fencing
// diagnostic. Assert both schemas instead of treating extra lines as history.
func historyDiagnosticFrames(t *testing.T, encoded []byte) ([][]byte, int) {
	t.Helper()
	var histories [][]byte
	runtimeCount := 0
	for _, line := range strings.Split(strings.TrimSpace(string(encoded)), "\n") {
		var diagnostic map[string]any
		if json.Unmarshal([]byte(line), &diagnostic) != nil {
			t.Fatal("history diagnostic stream contains an invalid fixed schema")
		}
		switch diagnostic["event"] {
		case "history_failure":
			if len(diagnostic) != 8 || len(line) > 1024 {
				t.Fatal("history diagnostic changed its bounded fixed schema")
			}
			histories = append(histories, []byte(line))
		case "runtime_failure":
			if len(diagnostic) != 4 {
				t.Fatal("runtime diagnostic changed its fixed schema")
			}
			if diagnostic["stage"] != "unknown" || diagnostic["code"] != "history_corrupt" || diagnostic["class"] != "none" {
				t.Fatal("history fence emitted an unexpected runtime stage, code, or class")
			}
			runtimeCount++
		default:
			t.Fatal("history diagnostic stream contains an unexpected event")
		}
	}
	return histories, runtimeCount
}

// Raw enum values are bounded protocol numbers, not permission to project an
// unknown control. These tests intentionally use only pre-existing entrypoints.
func TestHistoryDiagnosticRawStubPreservesUnknownNumbers(t *testing.T) {
	for _, tc := range []struct {
		name           string
		message        *waWeb.WebMessageInfo
		raw            int32
		present, known bool
		legacy         int32
		presence       uint32
	}{
		{name: "missing_web", legacy: -1},
		{name: "missing_stub", message: &waWeb.WebMessageInfo{}, legacy: -1, presence: 1},
		{name: "explicit_zero", message: &waWeb.WebMessageInfo{MessageStubType: waWeb.WebMessageInfo_UNKNOWN.Enum()}, present: true, known: true, presence: 1},
		{name: "known", message: &waWeb.WebMessageInfo{MessageStubType: waWeb.WebMessageInfo_GROUP_CREATE.Enum()}, raw: 20, present: true, known: true, legacy: 20, presence: 1},
		{name: "unknown_positive", message: &waWeb.WebMessageInfo{MessageStubType: waWeb.WebMessageInfo_StubType(987654).Enum()}, raw: 987654, present: true, legacy: -1, presence: 1},
		{name: "unknown_negative", message: &waWeb.WebMessageInfo{MessageStubType: waWeb.WebMessageInfo_StubType(-2147483648).Enum()}, raw: -2147483648, present: true, legacy: -1, presence: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := historyBodyMissing(tc.message)
			if !errors.Is(err, errHistoryCorrupt) {
				t.Fatal("unknown metadata lost the fatal cause")
			}
			encoded := historyFailureDiagnostic(historyStage(err, "mapping", "no_stub"))
			var got map[string]any
			if json.Unmarshal(encoded, &got) != nil || len(got) != 8 || len(encoded) > 1024 {
				t.Fatal("diagnostic must have eight bounded fields")
			}
			if got["raw_stub"] != float64(tc.raw) || got["stub_present"] != tc.present || got["stub_known"] != tc.known || got["stub_type"] != float64(tc.legacy) || got["presence"] != float64(tc.presence) {
				t.Fatalf("numeric witness changed: %s", encoded)
			}
		})
	}
}

func TestHistoryDiagnosticPresenceIsBoundedAndBodyFree(t *testing.T) {
	const canary = "SYNTHETIC_PRIVATE_HISTORY_WITNESS_CANARY"
	message := historyRecord(canary, canary).Message
	message.MessageStubType = waWeb.WebMessageInfo_StubType(987654).Enum()
	message.Key.Participant = new(string)
	*message.Key.Participant = canary
	message.Participant = new(string)
	*message.Participant = canary
	message.MessageStubParameters = []string{strings.Repeat(canary, 32768)}
	message.FutureproofData = []byte(canary)
	// Bits 0..8 describe web/key/body and key-field/parameter presence; bit 15
	// describes futureproof bytes. Values and parameter lengths are never emitted.
	encoded := historyFailureDiagnostic(historyBodyMissing(message))
	var got map[string]any
	if json.Unmarshal(encoded, &got) != nil || got["presence"] != float64(511|1<<15) {
		t.Fatalf("presence bitmap missing: %s", encoded)
	}
	if len(encoded) > 1024 || strings.Contains(string(encoded), canary) || len(got) != 8 {
		t.Fatal("witness retained unbounded or private data")
	}
	// The error snapshots scalars; changing the original protobuf cannot alter it.
	err := historyBodyMissing(message)
	message.MessageStubType = nil
	message.MessageStubParameters = nil
	message.FutureproofData = nil
	if string(historyFailureDiagnostic(err)) != string(encoded) {
		t.Fatal("witness retained the mutable protobuf")
	}
}

func TestHistoryDiagnosticUnknownStubStaysFatalAndFirstOnly(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal("diagnostic pipe setup failed")
	}
	previous := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = previous; _ = read.Close(); _ = write.Close() }()
	store := fixtureStore()
	runtime := testRuntime(store)
	record := historyRecord("synthetic-unknown-stub", "")
	record.Message.Message = nil
	record.Message.MessageStubType = waWeb.WebMessageInfo_StubType(987654).Enum()
	err = runtime.projectHistory(context.Background(), historyWire(record))
	if !errors.Is(err, errHistoryCorrupt) || !runtime.Status().ReconciliationRequired || len(store.unsafe) != 1 || len(store.events) != 0 {
		t.Fatal("unknown stub did not retain fatal zero-admission behavior")
	}
	_ = runtime.projectHistory(context.Background(), historyWire(record))
	os.Stderr = previous
	_ = write.Close()
	encoded, err := io.ReadAll(io.LimitReader(read, 4096))
	if err != nil {
		t.Fatal("diagnostic read failed")
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(encoded)), "\n") {
		var got map[string]any
		if json.Unmarshal([]byte(line), &got) != nil {
			t.Fatal("invalid diagnostic JSON")
		}
		if got["event"] != "history_failure" {
			continue
		}
		count++
		if got["raw_stub"] != float64(987654) || got["stub_present"] != true || got["stub_known"] != false || len(line) > 1024 {
			t.Fatal("unknown protocol number was lost or witness was unbounded")
		}
	}
	if count != 1 || len(store.unsafe) != 1 || len(store.events) != 0 {
		t.Fatal("later fenced input changed the first failure or admitted content")
	}
}
