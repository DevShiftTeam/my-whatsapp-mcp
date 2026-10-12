package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

type partialGapReader struct {
	fakeReader
	snapshot archive.ReadSnapshot
}

func (r *partialGapReader) SearchGuarded(ctx context.Context, s archive.ReadScope, q archive.SearchRequest) (archive.SearchPage, archive.ResponseGuard, error) {
	p, g, e := r.fakeReader.SearchGuarded(ctx, s, q)
	p.Snapshot = r.snapshot
	return p, g, e
}
func (r *partialGapReader) ContextGuarded(ctx context.Context, s archive.ReadScope, q archive.ContextRequest) (archive.ContextPage, archive.ResponseGuard, error) {
	p, g, e := r.fakeReader.ContextGuarded(ctx, s, q)
	p.Snapshot = r.snapshot
	return p, g, e
}
func (r *partialGapReader) ListChatsGuarded(ctx context.Context, s archive.ReadScope, q archive.ChatListRequest) (archive.ChatPage, archive.ResponseGuard, error) {
	p, g, e := r.fakeReader.ListChatsGuarded(ctx, s, q)
	p.Snapshot = r.snapshot
	return p, g, e
}
func (r *partialGapReader) StatusGuarded(ctx context.Context, s archive.ReadScope) (archive.StatusSnapshot, archive.ResponseGuard, error) {
	p, g, e := r.fakeReader.StatusGuarded(ctx, s)
	p.Snapshot = r.snapshot
	p.Coverage = "complete"
	return p, g, e
}

type partialGapAttachments struct {
	fakeAttachments
	snapshot archive.ReadSnapshot
}

func (a *partialGapAttachments) Search(ctx context.Context, g archive.CredentialGrant, q archive.AttachmentSearchRequest) (archive.AttachmentPage, archive.ResponseGuard, error) {
	p, guard, e := a.fakeAttachments.Search(ctx, g, q)
	p.Snapshot = a.snapshot
	return p, guard, e
}
func (a *partialGapAttachments) Prepare(ctx context.Context, g archive.CredentialGrant, id archive.MediaID, fetch, inline bool) (AttachmentDelivery, archive.ResponseGuard, error) {
	p, guard, e := a.fakeAttachments.Prepare(ctx, g, id, fetch, inline)
	p.Snapshot = a.snapshot
	return p, guard, e
}

type partialGapHistoricalRead struct {
	*partialGapReader
	metadata archive.HistoricalViewMetadata
}

func (r *partialGapHistoricalRead) Metadata() archive.HistoricalViewMetadata { return r.metadata }
func (r *partialGapHistoricalRead) StatsGuarded(context.Context, archive.ReadScope, archive.StatsRequest) (archive.StatsSnapshot, archive.ResponseGuard, error) {
	return archive.StatsSnapshot{}, archive.ResponseGuard{}, archive.ErrInvalidQuery
}

type partialGapHistoricalFactory struct{ reader *partialGapHistoricalRead }

func (f *partialGapHistoricalFactory) Metadata() archive.HistoricalViewMetadata {
	return f.reader.Metadata()
}
func (f *partialGapHistoricalFactory) ForCredential(context.Context, archive.CredentialGrant) (archive.HistoricalRead, error) {
	return f.reader, nil
}
func (f *partialGapHistoricalFactory) ForOwner(context.Context, archive.ReadScope) (archive.HistoricalRead, error) {
	return nil, archive.ErrUnauthorized
}

func partialGapPayload(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var envelope struct {
		Result struct {
			IsError           bool                       `json:"isError"`
			StructuredContent map[string]json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(body), &envelope) != nil || envelope.Result.IsError || envelope.Result.StructuredContent == nil {
		t.Fatalf("archive result unavailable: %s", body)
	}
	return envelope.Result.StructuredContent
}
func assertPartialGapLabel(t *testing.T, payload map[string]json.RawMessage) {
	t.Helper()
	var gap bool
	var phone, warning string
	if json.Unmarshal(payload["unknown_history_gap"], &gap) != nil || !gap || json.Unmarshal(payload["current_phone_state"], &phone) != nil || phone != "unknown" || json.Unmarshal(payload["coverage_warning"], &warning) != nil {
		t.Fatal("partial-history machine state or phone uncertainty absent")
	}
	for _, word := range []string{"partial", "Unsupported", "edits", "deletions", "retention"} {
		if !strings.Contains(warning, word) {
			t.Fatalf("missing generic unsupported-change warning: %s", warning)
		}
	}
	encoded, _ := json.Marshal(payload)
	for _, key := range []string{"coverage_epoch", "CoverageEpoch", "raw_stub", "total_observations", "codes_by_stub", "first_seen", "last_seen"} {
		if strings.Contains(string(encoded), `"`+key+`"`) {
			t.Fatalf("owner diagnostic escaped scoped MCP: %s", key)
		}
	}
	if strings.Contains(string(encoded), "987654321") {
		t.Fatal("private coverage epoch escaped scoped metadata")
	}
}

func TestPartialHistoryMCPAllEightArchiveReadsCarryOnlyGenericGap(t *testing.T) {
	snapshot := archive.ReadSnapshot{UnknownHistoryGap: true, CoverageEpoch: 987654321}
	for _, name := range []string{"search_messages", "get_context", "list_chats", "search_contacts", "search_groups", "get_index_status", "search_attachments", "get_attachment"} {
		t.Run(name, func(t *testing.T) {
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
				t.Fatal("scoped archive read failed")
			}
			payload := partialGapPayload(t, body)
			assertPartialGapLabel(t, payload)
			if name == "get_index_status" {
				var coverage string
				if json.Unmarshal(payload["coverage"], &coverage) != nil || coverage != "partial" {
					t.Fatal("status claimed complete after unknown controls")
				}
			}
		})
	}
}

func TestPartialHistoryMCPHistoricalCaptureWarningKeepsLiveGap(t *testing.T) {
	metadata := archive.HistoricalViewMetadata{Source: "historical", SnapshotID: "0123456789abcdef", CapturedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), ReadOnly: true, Coverage: "partial", CurrentPhoneState: "unknown"}
	for _, name := range []string{"search_messages", "get_context", "list_chats", "search_contacts", "search_groups", "get_index_status"} {
		t.Run(name, func(t *testing.T) {
			historical := &partialGapHistoricalRead{partialGapReader: &partialGapReader{snapshot: archive.ReadSnapshot{UnknownHistoryGap: true, CoverageEpoch: 987654321}}, metadata: metadata}
			server, _ := httpFixture(t, &fakeReader{}, credentialsFixture(), func(c *Config) { c.HistoricalReads = &partialGapHistoricalFactory{reader: historical} })
			args := map[string]any{"source": "historical"}
			switch name {
			case "search_messages", "search_contacts", "search_groups":
				args["query"] = "synthetic"
			case "get_context":
				args["message_id"] = "h1.0123456789abcdef.m.synthetic"
			}
			_, body := rpc(t, server, chatToolRPC(t, name, args), nil)
			payload := partialGapPayload(t, body)
			assertPartialGapLabel(t, payload)
			var view archive.HistoricalViewMetadata
			if json.Unmarshal(payload["archive_view"], &view) != nil || view != metadata {
				t.Fatal("gap warning erased captured historical identity")
			}
			var warning string
			json.Unmarshal(payload["coverage_warning"], &warning)
			if !strings.Contains(warning, "Captured-time") || !strings.Contains(warning, "read-only") {
				t.Fatal("gap warning erased historical limits")
			}
		})
	}
}

func TestPartialHistoryMCPStrictNoGapLabelRemainsUnchanged(t *testing.T) {
	server, _ := httpFixture(t, &partialGapReader{snapshot: archive.ReadSnapshot{CoverageEpoch: 987654321}}, credentialsFixture(), nil)
	_, body := rpc(t, server, statusRPC, nil)
	payload := partialGapPayload(t, body)
	if _, ok := payload["unknown_history_gap"]; ok {
		t.Fatal("zero-gap strict response gained opt-in state")
	}
	var phone, warning string
	json.Unmarshal(payload["current_phone_state"], &phone)
	json.Unmarshal(payload["coverage_warning"], &warning)
	if phone != "unverified" || strings.Contains(warning, "Unsupported") || strings.Contains(body, "987654321") {
		t.Fatal("strict no-gap labels or private epoch changed")
	}
}

func TestPartialHistoryMCPGapMetadataDoesNotChangeCatalogOrGrant(t *testing.T) {
	reader := &partialGapReader{snapshot: archive.ReadSnapshot{UnknownHistoryGap: true, CoverageEpoch: 987654321}}
	credentials := credentialsFixture()
	backend := &sendBackend{}
	server, _ := httpFixture(t, reader, credentials, func(c *Config) {
		c.Attachments = &partialGapAttachments{snapshot: reader.snapshot}
		c.Outbound = backend
	})
	session, ctx := sdkSession(t, server, fakeToken())
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 12 {
		t.Fatal("partial coverage changed tool catalogue")
	}
	_, body := rpc(t, server, chatToolRPC(t, "get_context", map[string]any{"message_id": "opaque-message"}), nil)
	assertPartialGapLabel(t, partialGapPayload(t, body))
	if reader.lastScope.Account != credentials.grant.Scope.Account || reader.lastScope.Principal != credentials.grant.Scope.Principal || reader.lastScope.PolicyGeneration != credentials.grant.Scope.PolicyGeneration {
		t.Fatal("gap label altered current client authority")
	}
	if backend.callCount() != 0 {
		t.Fatal("gap metadata triggered outgoing provider work")
	}
}
