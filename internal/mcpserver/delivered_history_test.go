package mcpserver

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func deliveredSnapshot(t *testing.T) archive.ReadSnapshot {
	t.Helper()
	var snapshot archive.ReadSnapshot
	field := reflect.ValueOf(&snapshot).Elem().FieldByName("DeliveredHistorySnapshot")
	if !field.IsValid() || field.Kind() != reflect.Bool {
		t.Fatal("delivered snapshot read metadata missing")
	}
	field.SetBool(true)
	return snapshot
}
func assertDeliveredLabel(t *testing.T, payload map[string]json.RawMessage) {
	t.Helper()
	var delivered bool
	var phone, warning string
	if json.Unmarshal(payload["delivered_history_snapshot"], &delivered) != nil || !delivered || json.Unmarshal(payload["current_phone_state"], &phone) != nil || phone != "unknown" || json.Unmarshal(payload["coverage_warning"], &warning) != nil {
		t.Fatal("delivered snapshot uncertainty absent")
	}
	for _, term := range []string{"delivered", "partial", "individual"} {
		if !strings.Contains(warning, term) {
			t.Fatalf("missing delivered snapshot trust warning %q", term)
		}
	}
	for _, key := range []string{"strategy_epoch", "delivered_accepted", "delivered_limit", "page_attempts", "body_credits", "readable_messages"} {
		if _, ok := payload[key]; ok {
			t.Fatalf("owner progress leaked to scoped MCP: %s", key)
		}
	}
}
func TestDeliveredHistoryMCPLabelsEightReads(t *testing.T) {
	for _, name := range []string{"search_messages", "get_context", "list_chats", "search_contacts", "search_groups", "get_index_status", "search_attachments", "get_attachment"} {
		t.Run(name, func(t *testing.T) {
			snapshot := deliveredSnapshot(t)
			reader := &partialGapReader{snapshot: snapshot}
			attachments := &partialGapAttachments{snapshot: snapshot}
			server, _ := httpFixture(t, reader, credentialsFixture(), func(c *Config) { c.Attachments = attachments })
			args := map[string]any{}
			switch name {
			case "search_messages", "search_contacts", "search_groups":
				args["query"] = "synthetic"
			case "get_context":
				args["message_id"] = "opaque-message"
			case "get_attachment":
				args["attachment_id"] = "opaque-attachment"
			}
			status, body := rpc(t, server, chatToolRPC(t, name, args), nil)
			if status != http.StatusOK {
				t.Fatal("scoped read failed")
			}
			payload := partialGapPayload(t, body)
			assertDeliveredLabel(t, payload)
			if name == "get_index_status" && string(payload["coverage"]) != `"partial"` {
				t.Fatal("delivered snapshot reported complete")
			}
		})
	}
}
func TestDeliveredHistoryMCPHistoricalWarningComposes(t *testing.T) {
	snapshot := deliveredSnapshot(t)
	snapshot.UnknownHistoryGap = true
	metadata := archive.HistoricalViewMetadata{Source: "historical", SnapshotID: "0123456789abcdef", CapturedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ReadOnly: true, Coverage: "partial", CurrentPhoneState: "unknown"}
	value, err := withHistoricalMetadata(searchDTO{contentLabel: sourceLabel(snapshot)}, metadata)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	assertDeliveredLabel(t, payload)
	assertPartialGapLabel(t, payload)
	if !strings.Contains(string(payload["coverage_warning"]), "Captured-time") {
		t.Fatal("capture limit lost")
	}
}
func TestDeliveredHistoryMCPDefaultLabelUnchanged(t *testing.T) {
	encoded, err := json.Marshal(sourceLabel(archive.ReadSnapshot{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "delivered_history_snapshot") {
		t.Fatal("ordinary archive mislabeled delivered")
	}
}
