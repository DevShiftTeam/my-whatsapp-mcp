//go:build sqlite_fts5

package ingestion

import (
	"context"
	"errors"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// The phone-delivered failure shape is a direct incoming REVOKE with presence
// 475: no body or key participant, but a web participant and stub parameters.
// The SDK derives its source author from the direct chat, not that participant.
func directRevokeMarkerRecord(chat, id string) *waWeb.WebMessageInfo {
	web := bodylessHistoryStub(chat, id, waWeb.WebMessageInfo_REVOKE).Message
	web.Participant = proto.String("synthetic-marker@broadcast")
	web.MessageStubParameters = []string{"synthetic-deletion-marker"}
	return web
}

func TestHistoryDirectRevokeBroadcastMarkerPreservesTerminalDeletion(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown_target", true: "retained_target"}[existing], func(t *testing.T) {
			const chat, target = "200@s.whatsapp.net", "direct-marker-target"
			ctx := context.Background()
			store, dir := durableFixture(t)
			r := durableRuntime(t, store)
			original := historyRecord(target, "directmarkerforbiddenreceipt")
			if existing {
				if err := r.projectHistory(ctx, historyWire(original)); err != nil {
					t.Fatal(err)
				}
			}
			stub := bodylessHistoryStub(chat, target, waWeb.WebMessageInfo_REVOKE)
			stub.Message = directRevokeMarkerRecord(chat, target)
			if historyPresence(stub.Message) != 475 {
				t.Fatal("fixture no longer matches the phone-delivered presence shape")
			}
			parsed, err := r.client.ParseWebMessage(types.NewJID("200", types.DefaultUserServer), stub.Message)
			if err != nil || parsed.Info.Sender.String() != chat || parsed.Info.IsFromMe {
				t.Fatal("pinned SDK did not derive the incoming direct author from the chat")
			}
			tail := historyRecord("direct-marker-tail", "directmarkertailreceipt")
			if err := r.projectHistory(ctx, historyWire(stub, tail)); err != nil {
				t.Fatal("valid direct deletion marker stopped unrelated history", err)
			}
			metadata, err := store.SourceMetadata(ctx, bodylessHistoryKey(chat, target))
			if err != nil || !metadata.Version.Deleted || metadata.Key.Sender != chat || metadata.ProtocolRevision != 0 {
				t.Fatal("marker replaced the original author or lost the terminal deletion")
			}
			if !existing && (metadata.OriginalDateKnown || !metadata.OriginalSentAt.IsZero()) {
				t.Fatal("stub receipt time became a fabricated original date")
			}
			if err := r.projectHistory(ctx, historyWire(original)); err != nil {
				t.Fatal(err)
			}
			queueBurstAssertSearch(t, store, "directmarkerforbiddenreceipt", 0)
			queueBurstAssertSearch(t, store, "directmarkertailreceipt", 1)
			if r.Status().ReconciliationRequired {
				t.Fatal("understood direct marker globally fenced the archive")
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := durableStore(t, dir)
			replayed := durableRuntime(t, reopened)
			if err := replayed.projectHistory(ctx, historyWire(original)); err != nil {
				t.Fatal(err)
			}
			queueBurstAssertSearch(t, reopened, "directmarkerforbiddenreceipt", 0)
			queueBurstAssertSearch(t, reopened, "directmarkertailreceipt", 1)
		})
	}
}

func TestHistoryDirectRevokeMarkerDoesNotRelaxOtherAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, chat string
		mutate     func(*waWeb.WebMessageInfo)
	}{
		{"group_actor", "300@g.us", nil},
		{"broadcast_conversation", "synthetic-chat@broadcast", nil},
		{"nonbroadcast_participant", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("300@g.us") }},
		{"invalid_participant", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("@broadcast") }},
		{"device_suffixed_marker", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("synthetic-marker:2@broadcast") }},
		{"control_character_marker", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("synthetic\x00marker@broadcast") }},
		{"trailing_identity_segment", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) {
			m.Participant = proto.String("synthetic-marker@broadcast@synthetic-user@s.whatsapp.net")
		}},
		{"other_control_character", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("synthetic\x01marker@broadcast") }},
		{"unicode_space_marker", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("synthetic\u00a0marker@broadcast") }},
		{"zero_device_marker", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("synthetic-marker:0@broadcast") }},
		{"zero_agent_marker", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("synthetic-marker.0@broadcast") }},
		{"outgoing_direction", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.Key.FromMe = proto.Bool(true) }},
		{"omitted_direction", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.Key.FromMe = nil }},
		{"conflicting_key_author", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.Key.Participant = proto.String("777@s.whatsapp.net") }},
		{"incoming_self_claim", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.OriginalSelfAuthorUserJIDString = proto.String(durableAccount) }},
		{"explicit_expiry", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.EphemeralDuration = proto.Uint32(0) }},
		{"different_remote_chat", "200@s.whatsapp.net", func(m *waWeb.WebMessageInfo) { m.Key.RemoteJID = proto.String("777@s.whatsapp.net") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			stub := bodylessHistoryStub(tc.chat, "invalid-direct-marker", waWeb.WebMessageInfo_REVOKE)
			stub.Message = directRevokeMarkerRecord(tc.chat, "invalid-direct-marker")
			if tc.mutate != nil {
				tc.mutate(stub.Message)
			}
			if err := r.projectHistory(t.Context(), bodylessHistoryWire(t, tc.chat, stub)); err == nil || !r.Status().ReconciliationRequired {
				t.Fatal("unreviewed marker or conflicting authority bypassed strict history")
			}
			if _, err := durableSearch(t, store, "unusedreceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Fatal("marker compatibility bypassed the protected-read fence")
			}
		})
	}
}

func TestHistoryDirectRevokeMarkerPreservesRetainedDirectionMismatch(t *testing.T) {
	store, _ := durableFixture(t)
	r := durableRuntime(t, store)
	const chat, target = "200@s.whatsapp.net", "marker-own-target"
	original := historyRecord(target, "directmarkerownreceipt")
	original.Message.Key.FromMe = proto.Bool(true)
	original.Message.OriginalSelfAuthorUserJIDString = proto.String(durableAccount)
	if err := r.projectHistory(t.Context(), historyWire(original)); err != nil {
		t.Fatal(err)
	}
	stub := bodylessHistoryStub(chat, target, waWeb.WebMessageInfo_REVOKE)
	stub.Message = directRevokeMarkerRecord(chat, target)
	if err := r.projectHistory(t.Context(), historyWire(stub)); err == nil || !r.Status().ReconciliationRequired {
		t.Fatal("marker discarded conflicting retained author or direction")
	}
	key := bodylessHistoryKey(chat, target)
	key.Sender, key.FromMe = durableAccount, true
	metadata, err := store.SourceMetadata(t.Context(), key)
	if err != nil || metadata.Version.Deleted {
		t.Fatal("mismatched marker applied an unauthorized deletion")
	}
}
