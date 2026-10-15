package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"google.golang.org/protobuf/proto"
)

func TestHistoryBodylessStagePreservesPrivateCausesAndClosedSchema(t *testing.T) {
	const private = "SYNTHETIC_PRIVATE_BODYLESS_STAGE_CANARY"
	web := bodylessHistoryStub("300@g.us", private, waWeb.WebMessageInfo_REVOKE).Message
	web.PushName, web.MessageStubParameters = proto.String(private), []string{private}
	if historyBodylessStage(nil, web, "revoke_guard_failed") != nil {
		t.Fatal("successful projection gained a diagnostic failure")
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, archive.ErrArchiveUnavailable, privateHistoryError{}} {
		err := historyBodylessStage(cause, web, "revoke_guard_failed")
		if !errors.Is(err, cause) || err.Error() != "revoke_guard_failed" {
			t.Fatal("bodyless diagnostic changed its cause or formatted private data")
		}
		if historyStage(err, "mapping", "no_stub") != err {
			t.Fatal("outer wrapping replaced the precise failure")
		}
		encoded := historyFailureDiagnostic(err)
		var fields map[string]any
		if json.Unmarshal(encoded, &fields) != nil || len(fields) != 8 || fields["stage"] != "revoke_guard_failed" || len(encoded) > 1024 || strings.Contains(string(encoded), private) || strings.Contains(string(encoded), "@") {
			t.Fatal("bodyless diagnostic exposed values or changed its bounded schema")
		}
	}
	if encoded := historyFailureDiagnostic(historyBodylessStage(privateHistoryError{}, web, private)); strings.Contains(string(encoded), private) || !strings.Contains(string(encoded), `"stage":"unknown"`) {
		t.Fatal("unreviewed diagnostic stage was not closed")
	}
}

func TestHistoryRevokeDiagnosticDistinguishesAuthorityFailures(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		seed        func(*waWeb.WebMessageInfo)
		mutate      func(*waWeb.WebMessageInfo)
	}{
		{"invalid_author", "revoke_author_invalid", nil, func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("400@g.us") }},
		{"conflicting_authors", "revoke_author_conflict", nil, func(m *waWeb.WebMessageInfo) {
			m.Key.Participant = proto.String("777@s.whatsapp.net")
		}},
		{"incoming_self_claim", "revoke_direction_mismatch", nil, func(m *waWeb.WebMessageInfo) {
			m.OriginalSelfAuthorUserJIDString = proto.String(durableAccount)
		}},
		{"wrong_self_claim", "revoke_self_author_mismatch", nil, func(m *waWeb.WebMessageInfo) {
			m.Key.FromMe = proto.Bool(true)
			m.OriginalSelfAuthorUserJIDString = proto.String("777@s.whatsapp.net")
		}},
		{"parser_author_mismatch", "revoke_author_mismatch", nil, func(m *waWeb.WebMessageInfo) {
			m.Key.FromMe = proto.Bool(true)
		}},
		{"retained_author_mismatch", "revoke_target_author_mismatch", func(m *waWeb.WebMessageInfo) {
			m.Participant = proto.String("777@s.whatsapp.net")
		}, nil},
		{"retained_direction_mismatch", "revoke_target_direction_mismatch", func(m *waWeb.WebMessageInfo) {
			m.Key.FromMe = proto.Bool(true)
			m.Participant = proto.String(durableAccount)
			m.OriginalSelfAuthorUserJIDString = proto.String(durableAccount)
		}, func(m *waWeb.WebMessageInfo) { m.Participant = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const chat, private = "300@g.us", "SYNTHETIC_PRIVATE_REVOKE_DIAGNOSTIC_CANARY"
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			if tc.seed != nil {
				original := bodylessHistoryRecord(chat, private, "revokediagnosticretainedreceipt")
				tc.seed(original.Message)
				if err := r.projectHistory(t.Context(), bodylessHistoryWire(t, chat, original)); err != nil {
					t.Fatal(err)
				}
			}
			stub := bodylessHistoryStub(chat, private, waWeb.WebMessageInfo_REVOKE)
			stub.Message.PushName = proto.String(private)
			stub.Message.MessageStubParameters = []string{private}
			if tc.mutate != nil {
				tc.mutate(stub.Message)
			}
			err := r.projectHistory(t.Context(), bodylessHistoryWire(t, chat, stub))
			if !errors.Is(err, errHistoryCorrupt) || !r.Status().ReconciliationRequired {
				t.Fatal("diagnostic changed a genuine authority failure into readable history")
			}
			encoded := historyFailureDiagnostic(err)
			var diagnostic struct {
				Stage       string `json:"stage"`
				Stub        string `json:"stub"`
				RawStub     int32  `json:"raw_stub"`
				StubPresent bool   `json:"stub_present"`
				Presence    uint32 `json:"presence"`
			}
			if json.Unmarshal(encoded, &diagnostic) != nil || diagnostic.Stage != tc.stage || diagnostic.Stub != "revoke" || !diagnostic.StubPresent || diagnostic.RawStub != int32(waWeb.WebMessageInfo_REVOKE) || diagnostic.Presence != historyPresence(stub.Message) {
				t.Fatalf("diagnostic lost its closed cause or actual field presence: %s", encoded)
			}
			if len(encoded) > 1024 || strings.Contains(string(encoded), private) || strings.Contains(string(encoded), "@") {
				t.Fatal("revocation diagnostic leaked source values or exceeded its bound")
			}
			if _, err := durableSearch(t, store, "revokediagnosticretainedreceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Fatal("diagnostic bypassed the protected-read fence")
			}
		})
	}
}
