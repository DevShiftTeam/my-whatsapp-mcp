//go:build sqlite_fts5

package ingestion

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// A business privacy transition is not established as benign by the SDK enum.
// Explicit partial mode must record that uncertainty, not silently classify it
// as a notice or discard the supported deletion that follows it.
func TestExplicitPartialBusinessPrivacyTransitionKeepsGapAndDeletion(t *testing.T) {
	store, dir := durableFixture(t)
	r := durableRuntime(t, store)
	r.config.UnknownHistoryPolicy = UnknownHistoryPartial
	if err := r.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.message(t.Context(), liveMessage("partial-biz-target", "partialbizforbiddenreceipt")); err != nil {
		t.Fatal(err)
	}
	notice := bodylessHistoryStub("200@s.whatsapp.net", "partial-biz-transition", waWeb.WebMessageInfo_BIZ_COEX_PRIVACY_TRANSITION)
	notice.Message.Participant = nil
	if historyPresence(notice.Message) != 155 {
		t.Fatal("fixture lost the phone-delivered notice shape")
	}
	deletion := bodylessHistoryStub("200@s.whatsapp.net", "partial-biz-target", waWeb.WebMessageInfo_REVOKE)
	r.deliver(realtimeHistory(r, notice, deletion, historyRecord("partial-biz-tail", "partialbizreadablereceipt")))
	if err := r.Disconnect(t.Context()); err != nil {
		t.Fatal("explicit partial history did not drain", err)
	}
	queueBurstAssertSearch(t, store, "partialbizforbiddenreceipt", 0)
	queueBurstAssertSearch(t, store, "partialbizreadablereceipt", 1)
	policy, err := store.Policy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scope := archive.ReadScope{Account: durableAccount, Principal: "local_owner", AllChats: true, PolicyGeneration: policy.Generation}
	status, err := store.Status(t.Context(), scope)
	if err != nil || status.ReconciliationRequired || status.Coverage != "partial" || !status.Snapshot.UnknownHistoryGap {
		t.Fatal("unsupported business transition lost its explicit uncertainty warning", err)
	}
	metadata, err := store.SourceMetadata(t.Context(), bodylessHistoryKey("200@s.whatsapp.net", "partial-biz-transition"))
	if err != nil || metadata.Version.MessageID != "" || metadata.OriginalDateKnown {
		t.Fatal("discarded notice created source content or an original date")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := durableStore(t, dir)
	status, err = reopened.Status(t.Context(), scope)
	if err != nil || !status.Snapshot.UnknownHistoryGap {
		t.Fatal("partial-history uncertainty disappeared on reopen", err)
	}
	queueBurstAssertSearch(t, reopened, "partialbizforbiddenreceipt", 0)
	queueBurstAssertSearch(t, reopened, "partialbizreadablereceipt", 1)
}

func verifiedBusinessPrivacyTransitionStub(chat, id string, fromMe bool) *waHistorySync.HistorySyncMsg {
	record := bodylessHistoryStub(chat, id, waWeb.WebMessageInfo_BIZ_COEX_PRIVACY_TRANSITION)
	record.MsgOrderID = proto.Uint64(1)
	record.Message.Participant = nil
	record.Message.Key.Participant = nil
	record.Message.Key.FromMe = proto.Bool(fromMe)
	record.Message.Status = waWeb.WebMessageInfo_SERVER_ACK.Enum()
	record.Message.IsMentionedInStatus = proto.Bool(false)
	return record
}

func TestStrictBusinessPrivacyTransitionKeepsUncertaintyDeletionEditAndSpool(t *testing.T) {
	for _, tc := range []struct {
		name      string
		chat      string
		canonical string
		lid       bool
	}{
		{"direct_pn", "200@s.whatsapp.net", "200@s.whatsapp.net", false},
		{"mapped_lid", "900@lid", "200@s.whatsapp.net", true},
		{"unmapped_lid", "901@lid", "901@lid", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canonicalChat := tc.canonical
			ctx := t.Context()
			store, dir := durableFixture(t)
			r := durableRuntime(t, store)
			spoolDir := filepath.Join(dir, "history-spool")
			spool, err := openHistorySpool(spoolDir)
			if err != nil {
				t.Fatal(err)
			}
			r.historySpool = spool
			if r.config.UnknownHistoryPolicy != UnknownHistoryStrict {
				t.Fatal("fixture must exercise default strict history policy")
			}
			if tc.lid {
				if err := store.ApplyAlias(ctx, archive.SourceAlias{Account: durableAccount, PhoneJID: canonicalChat, LID: tc.chat, Provenance: archive.AliasConnectorMapping}); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			revokeOriginal := bodylessHistoryRecord(tc.chat, "strict-biz-revoke-target", "strictbizforbiddenreceipt")
			revokeOriginal.Message.Participant = nil
			editOriginal := bodylessHistoryRecord(tc.chat, "strict-biz-edit-target", "strictbizoldeditreceipt")
			editOriginal.Message.Participant = nil
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, tc.chat, revokeOriginal, editOriginal)); err != nil {
				t.Fatal(err)
			}
			notice := verifiedBusinessPrivacyTransitionStub(tc.chat, "strict-biz-transition", true)
			if historyPresence(notice.Message) != 155 {
				t.Fatal("fixture lost the phone-delivered notice shape")
			}
			encrypted := bodylessHistoryStub(tc.chat, "strict-biz-e2e-encrypted", waWeb.WebMessageInfo_E2E_ENCRYPTED)
			encrypted.MsgOrderID = proto.Uint64(2)
			encrypted.Message.Participant = nil
			encrypted.Message.Key.FromMe = proto.Bool(true)
			encrypted.Message.Status = waWeb.WebMessageInfo_SERVER_ACK.Enum()
			encrypted.Message.IsMentionedInStatus = proto.Bool(false)
			deletion := bodylessHistoryStub(tc.chat, "strict-biz-revoke-target", waWeb.WebMessageInfo_REVOKE)
			deletion.Message.Participant = nil
			edit := bodylessHistoryRecord(tc.chat, "strict-biz-edit", "")
			edit.Message.Participant = nil
			edit.Message.MessageTimestamp = proto.Uint64(2000)
			edit.Message.Message = &waE2E.Message{
				ProtocolMessage: &waE2E.ProtocolMessage{
					Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
					Key:           &waCommon.MessageKey{ID: proto.String("strict-biz-edit-target"), RemoteJID: proto.String(tc.chat), FromMe: proto.Bool(false)},
					TimestampMS:   proto.Int64(2000123),
					EditedMessage: &waE2E.Message{Conversation: proto.String("strictbizneweditreceipt")},
				},
			}
			tail := bodylessHistoryRecord(tc.chat, "strict-biz-tail", "strictbizreadablereceipt")
			tail.Message.Participant = nil

			envelope := &waHistorySync.HistorySync{
				SyncType:      waHistorySync.HistorySync_ON_DEMAND.Enum(),
				Conversations: []*waHistorySync.Conversation{{ID: proto.String(tc.chat), Messages: []*waHistorySync.HistorySyncMsg{notice, encrypted, deletion, edit, tail}}},
			}
			raw, err := proto.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			notification := realtimeHistory(r)
			notification.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = compressed(raw)
			r.deliver(notification)
			if err := r.Disconnect(ctx); err != nil {
				t.Fatal("strict history with verified business privacy transition did not drain", err)
			}
			if r.Status().ReconciliationRequired || r.Status().HistoryChunks != 1 {
				t.Fatalf("unexpected runtime state: unsafe=%t chunks=%d", r.Status().ReconciliationRequired, r.Status().HistoryChunks)
			}
			if len(retainedHistoryBatchIDs(t, spoolDir)) != 1 {
				t.Fatal("strict business privacy transition discarded raw evidence from history spool")
			}
			queueBurstAssertSearch(t, store, "strictbizforbiddenreceipt", 0)
			queueBurstAssertSearch(t, store, "strictbizoldeditreceipt", 0)
			queueBurstAssertSearch(t, store, "strictbizneweditreceipt", 1)
			queueBurstAssertSearch(t, store, "strictbizreadablereceipt", 1)

			policy, err := store.Policy(ctx)
			if err != nil {
				t.Fatal(err)
			}
			scope := archive.ReadScope{Account: durableAccount, Principal: "local_owner", AllChats: true, PolicyGeneration: policy.Generation}
			status, err := store.Status(ctx, scope)
			if err != nil || status.ReconciliationRequired || status.Coverage != "partial" || !status.Snapshot.UnknownHistoryGap || !slices.Contains(status.Codes, archive.UnknownHistoryGapCode) {
				t.Fatalf("strict business privacy transition lost uncertainty status: %+v err=%v", status, err)
			}
			diagnostics, err := store.HistoryUncertaintyDiagnostics(ctx, durableAccount)
			if err != nil || diagnostics.TotalObservations != 1 || diagnostics.Epoch != 1 || len(diagnostics.Reasons) != 1 || diagnostics.Reasons[0].Reason != archive.HistoryUncertaintyUnsupportedNotice || diagnostics.Reasons[0].Count != 1 {
				t.Fatalf("unexpected uncertainty diagnostics: %+v err=%v", diagnostics, err)
			}
			for _, id := range []string{"strict-biz-transition", "strict-biz-e2e-encrypted"} {
				for _, fromMe := range []bool{false, true} {
					sender := canonicalChat
					if fromMe {
						sender = durableAccount
					}
					metadata, err := store.SourceMetadata(ctx, archive.SourceKey{Account: durableAccount, Chat: canonicalChat, Sender: sender, Message: id, FromMe: fromMe})
					if err != nil || metadata.Version.MessageID != "" || metadata.OriginalDateKnown || !metadata.OriginalSentAt.IsZero() || metadata.Version.Deleted {
						t.Fatalf("notice %s fabricated source metadata: %+v err=%v", id, metadata, err)
					}
				}
			}
			editMetadata, err := store.SourceMetadata(ctx, archive.SourceKey{Account: durableAccount, Chat: canonicalChat, Sender: canonicalChat, Message: "strict-biz-edit-target"})
			if err != nil || editMetadata.ProtocolRevision != 2000123 || !editMetadata.OriginalSentAt.Equal(time.Unix(1000, 0)) {
				t.Fatalf("following edit lost original date or protocol revision: %+v err=%v", editMetadata, err)
			}
			anchors, err := store.BackfillAnchors(ctx, durableAccount, 32)
			if err != nil {
				t.Fatal(err)
			}
			for _, anchor := range anchors {
				if anchor.Key.Message == "strict-biz-transition" || anchor.Key.Message == "strict-biz-e2e-encrypted" {
					t.Fatalf("notice fabricated backfill anchor: %+v", anchor)
				}
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := durableStore(t, dir)
			status, err = reopened.Status(ctx, scope)
			if err != nil || !status.Snapshot.UnknownHistoryGap || status.Coverage != "partial" {
				t.Fatal("strict business privacy transition uncertainty disappeared on reopen", err)
			}
			queueBurstAssertSearch(t, reopened, "strictbizforbiddenreceipt", 0)
			queueBurstAssertSearch(t, reopened, "strictbizoldeditreceipt", 0)
			queueBurstAssertSearch(t, reopened, "strictbizneweditreceipt", 1)
			queueBurstAssertSearch(t, reopened, "strictbizreadablereceipt", 1)
		})
	}
}

func TestStrictBusinessPrivacyTransitionRejectsUnverifiedShapesAndMetadata(t *testing.T) {
	unknownBytes := protowire.AppendVarint(protowire.AppendTag(nil, 777, protowire.VarintType), 0)
	cases := []struct {
		name   string
		chat   string
		mutate func(*waHistorySync.HistorySyncMsg)
	}{
		{"group_chat", "300@g.us", func(*waHistorySync.HistorySyncMsg) {}},
		{"broadcast_chat", "synthetic@broadcast", func(*waHistorySync.HistorySyncMsg) {}},
		{"newsletter_chat", "120363000@newsletter", func(*waHistorySync.HistorySyncMsg) {}},
		{"device_suffixed_lid_chat", "999:1@lid", func(*waHistorySync.HistorySyncMsg) {}},
		{"missing_key", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.Key = nil }},
		{"empty_id", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.Key.ID = proto.String("") }},
		{"missing_from_me", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.Key.FromMe = nil }},
		{"missing_remote_jid", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.Key.RemoteJID = nil }},
		{"mismatched_remote_jid", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.Key.RemoteJID = proto.String("777@s.whatsapp.net") }},
		{"non_canonical_remote_jid", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.Key.RemoteJID = proto.String("200:0@s.whatsapp.net") }},
		{"zero_timestamp", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.MessageTimestamp = proto.Uint64(0) }},
		{"missing_timestamp", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.MessageTimestamp = nil }},
		{"key_participant", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.Key.Participant = proto.String("200@s.whatsapp.net") }},
		{"web_participant", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.Participant = proto.String("200@s.whatsapp.net") }},
		{"self_author_claim", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) {
			r.Message.OriginalSelfAuthorUserJIDString = proto.String(durableAccount)
		}},
		{"stub_parameters", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) {
			r.Message.MessageStubParameters = []string{"synthetic-parameter"}
		}},
		{"mentioned_in_status_true", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.IsMentionedInStatus = proto.Bool(true) }},
		{"starred_false", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.Starred = proto.Bool(false) }},
		{"biz_privacy_status", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.BizPrivacyStatus = waWeb.WebMessageInfo_FB.Enum() }},
		{"verified_biz_name", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.VerifiedBizName = proto.String("synthetic-biz") }},
		{"zero_expiry", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.EphemeralDuration = proto.Uint32(0) }},
		{"explicit_off_to_on", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.EphemeralOffToOn = proto.Bool(false) }},
		{"keep_in_chat", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.KeepInChat = &waWeb.KeepInChat{} }},
		{"target_message_id", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) {
			r.Message.TargetMessageID = &waCommon.MessageKey{ID: proto.String("synthetic-target")}
		}},
		{"pin_in_chat", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.PinInChat = &waWeb.PinInChat{} }},
		{"revoke_timestamp", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.RevokeMessageTimestamp = proto.Uint64(0) }},
		{"clear_media", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.ClearMedia = proto.Bool(false) }},
		{"futureproof_data", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.FutureproofData = []byte{1} }},
		{"message_add_ons", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.MessageAddOns = []*waWeb.MessageAddOn{{}} }},
		{"quarantined_message", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.QuarantinedMessage = &waWeb.QuarantinedMessage{} }},
		{"group_history_bundle", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) {
			r.Message.GroupHistoryBundleInfo = &waWeb.GroupHistoryBundleInfo{}
		}},
		{"unknown_web_wire_field", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.ProtoReflect().SetUnknown(unknownBytes) }},
		{"unknown_key_wire_field", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.Message.Key.ProtoReflect().SetUnknown(unknownBytes) }},
		{"unknown_wrapper_wire_field", "200@s.whatsapp.net", func(r *waHistorySync.HistorySyncMsg) { r.ProtoReflect().SetUnknown(unknownBytes) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			if err := r.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			stub := verifiedBusinessPrivacyTransitionStub(tc.chat, "biz-unsafe-notice", false)
			tc.mutate(stub)
			tail := bodylessHistoryRecord(tc.chat, "biz-unsafe-tail", "bizunsafetailreceipt")
			tail.Message.Participant = nil
			wire, err := (proto.MarshalOptions{AllowPartial: true}).Marshal(&waHistorySync.HistorySync{
				SyncType:      waHistorySync.HistorySync_RECENT.Enum(),
				Conversations: []*waHistorySync.Conversation{{ID: proto.String(tc.chat), Messages: []*waHistorySync.HistorySyncMsg{stub, tail}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.projectHistory(context.Background(), wire); err == nil || !r.Status().ReconciliationRequired {
				t.Fatal("unverified business privacy transition bypassed strict safety fence")
			}
			if _, err := durableSearch(t, store, "bizunsafetailreceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Fatal("unsafe business privacy transition left protected reads open")
			}
		})
	}
}

func TestStrictWireVerifiedUncertaintyNoticesKeepUncertaintyAndSpool(t *testing.T) {
	ctx := t.Context()
	store, dir := durableFixture(t)
	r := durableRuntime(t, store)
	spoolDir := filepath.Join(dir, "history-spool")
	spool, err := openHistorySpool(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	r.historySpool = spool
	if err := r.Connect(ctx); err != nil {
		t.Fatal(err)
	}

	ephemeral := bodylessHistoryStub("200@s.whatsapp.net", "strict-ephemeral-72", waWeb.WebMessageInfo_CHANGE_EPHEMERAL_SETTING)
	ephemeral.MsgOrderID = proto.Uint64(1)
	ephemeral.Message.Participant = nil
	ephemeral.Message.Key.Participant = nil
	ephemeral.Message.Key.FromMe = proto.Bool(true)
	ephemeral.Message.Status = waWeb.WebMessageInfo_SERVER_ACK.Enum()
	ephemeral.Message.IsMentionedInStatus = proto.Bool(false)
	ephemeral.Message.MessageStubParameters = []string{"604800"}

	disappearing := bodylessHistoryStub("901@lid", "strict-disappearing-130", waWeb.WebMessageInfo_DISAPPEARING_MODE)
	disappearing.MsgOrderID = proto.Uint64(1)
	disappearing.Message.Participant = nil
	disappearing.Message.Key.Participant = nil
	disappearing.Message.Key.FromMe = proto.Bool(true)
	disappearing.Message.Status = waWeb.WebMessageInfo_SERVER_ACK.Enum()
	disappearing.Message.IsMentionedInStatus = proto.Bool(false)
	disappearing.Message.MessageStubParameters = []string{"86400", "901@lid"}

	limitSharing := bodylessHistoryStub("300@g.us", "strict-limit-sharing-216", waWeb.WebMessageInfo_CHANGE_LIMIT_SHARING)
	limitSharing.MsgOrderID = proto.Uint64(1)
	limitSharing.Message.Participant = proto.String("901@lid")
	limitSharing.Message.Key.Participant = nil
	limitSharing.Message.Key.FromMe = proto.Bool(false)
	limitSharing.Message.IsMentionedInStatus = proto.Bool(false)
	limitSharing.Message.MessageStubParameters = []string{"false", "1", "901@lid"}

	unknown229 := bodylessHistoryStub("200@s.whatsapp.net", "strict-unknown-229", historyStubUnknownDirectNotice229)
	unknown229.MsgOrderID = proto.Uint64(2)
	unknown229.Message.Participant = nil
	unknown229.Message.Key.Participant = nil
	unknown229.Message.Key.FromMe = proto.Bool(true)
	unknown229.Message.Status = waWeb.WebMessageInfo_SERVER_ACK.Enum()
	unknown229.Message.IsMentionedInStatus = proto.Bool(false)

	unknown263 := bodylessHistoryStub("300@g.us", "strict-unknown-263", historyStubUnknownGroupNotice263)
	unknown263.MsgOrderID = proto.Uint64(2)
	unknown263.Message.Participant = nil
	unknown263.Message.Key.Participant = nil
	unknown263.Message.Key.FromMe = proto.Bool(true)
	unknown263.Message.Status = waWeb.WebMessageInfo_SERVER_ACK.Enum()
	unknown263.Message.IsMentionedInStatus = proto.Bool(false)

	directTail := bodylessHistoryRecord("200@s.whatsapp.net", "strict-direct-tail", "strictverifieddirecttailreceipt")
	directTail.Message.Participant = nil
	lidTail := bodylessHistoryRecord("901@lid", "strict-lid-tail", "strictverifiedlidtailreceipt")
	lidTail.Message.Participant = nil
	groupTail := bodylessHistoryRecord("300@g.us", "strict-group-tail", "strictverifiedgrouptailreceipt")

	envelope := &waHistorySync.HistorySync{
		SyncType: waHistorySync.HistorySync_INITIAL_BOOTSTRAP.Enum(),
		Conversations: []*waHistorySync.Conversation{
			{ID: proto.String("200@s.whatsapp.net"), Messages: []*waHistorySync.HistorySyncMsg{ephemeral, unknown229, directTail}},
			{ID: proto.String("901@lid"), Messages: []*waHistorySync.HistorySyncMsg{disappearing, lidTail}},
			{ID: proto.String("300@g.us"), Messages: []*waHistorySync.HistorySyncMsg{limitSharing, unknown263, groupTail}},
		},
	}
	raw, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	notification := realtimeHistory(r)
	notification.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = compressed(raw)
	r.deliver(notification)
	if err := r.Disconnect(ctx); err != nil {
		t.Fatal("strict history with wire-verified uncertainty notices did not drain", err)
	}
	if r.Status().ReconciliationRequired || r.Status().HistoryChunks != 1 {
		t.Fatalf("unexpected runtime state: unsafe=%t chunks=%d", r.Status().ReconciliationRequired, r.Status().HistoryChunks)
	}
	if len(retainedHistoryBatchIDs(t, spoolDir)) != 1 {
		t.Fatal("strict wire-verified uncertainty notices discarded raw evidence from history spool")
	}
	queueBurstAssertSearch(t, store, "strictverifieddirecttailreceipt", 1)
	queueBurstAssertSearch(t, store, "strictverifiedlidtailreceipt", 1)
	queueBurstAssertSearch(t, store, "strictverifiedgrouptailreceipt", 1)

	diagnostics, err := store.HistoryUncertaintyDiagnostics(ctx, durableAccount)
	if err != nil || diagnostics.TotalObservations != 5 || len(diagnostics.Reasons) != 1 || diagnostics.Reasons[0].Reason != archive.HistoryUncertaintyUnsupportedNotice || diagnostics.Reasons[0].Count != 5 {
		t.Fatalf("unexpected uncertainty diagnostics: %+v err=%v", diagnostics, err)
	}
}

func TestStrictWireVerifiedUncertaintyNoticesRejectUnverifiedShapes(t *testing.T) {
	cases := []struct {
		name  string
		chat  string
		build func() *waHistorySync.HistorySyncMsg
	}{
		{
			name: "ephemeral_72_non_digit_param",
			chat: "200@s.whatsapp.net",
			build: func() *waHistorySync.HistorySyncMsg {
				r := bodylessHistoryStub("200@s.whatsapp.net", "bad-72", waWeb.WebMessageInfo_CHANGE_EPHEMERAL_SETTING)
				r.MsgOrderID = proto.Uint64(1)
				r.Message.Participant = nil
				r.Message.Key.FromMe = proto.Bool(true)
				r.Message.IsMentionedInStatus = proto.Bool(false)
				r.Message.MessageStubParameters = []string{"604800s"}
				return r
			},
		},
		{
			name: "disappearing_130_group_chat",
			chat: "300@g.us",
			build: func() *waHistorySync.HistorySyncMsg {
				r := bodylessHistoryStub("300@g.us", "bad-130", waWeb.WebMessageInfo_DISAPPEARING_MODE)
				r.MsgOrderID = proto.Uint64(1)
				r.Message.Participant = nil
				r.Message.Key.FromMe = proto.Bool(true)
				r.Message.IsMentionedInStatus = proto.Bool(false)
				r.Message.MessageStubParameters = []string{"86400", "200@s.whatsapp.net"}
				return r
			},
		},
		{
			name: "limit_sharing_216_non_bool_param",
			chat: "300@g.us",
			build: func() *waHistorySync.HistorySyncMsg {
				r := bodylessHistoryStub("300@g.us", "bad-216", waWeb.WebMessageInfo_CHANGE_LIMIT_SHARING)
				r.MsgOrderID = proto.Uint64(1)
				r.Message.Participant = proto.String("200@s.whatsapp.net")
				r.Message.Key.FromMe = proto.Bool(false)
				r.Message.IsMentionedInStatus = proto.Bool(false)
				r.Message.MessageStubParameters = []string{"maybe", "1", "200@s.whatsapp.net"}
				return r
			},
		},
		{
			name: "unknown_229_from_me_false",
			chat: "200@s.whatsapp.net",
			build: func() *waHistorySync.HistorySyncMsg {
				r := bodylessHistoryStub("200@s.whatsapp.net", "bad-229", historyStubUnknownDirectNotice229)
				r.MsgOrderID = proto.Uint64(1)
				r.Message.Participant = nil
				r.Message.Key.FromMe = proto.Bool(false)
				r.Message.IsMentionedInStatus = proto.Bool(false)
				return r
			},
		},
		{
			name: "unknown_263_direct_chat",
			chat: "200@s.whatsapp.net",
			build: func() *waHistorySync.HistorySyncMsg {
				r := bodylessHistoryStub("200@s.whatsapp.net", "bad-263", historyStubUnknownGroupNotice263)
				r.MsgOrderID = proto.Uint64(1)
				r.Message.Participant = nil
				r.Message.Key.FromMe = proto.Bool(true)
				r.Message.IsMentionedInStatus = proto.Bool(false)
				return r
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			if err := r.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			wire, err := proto.Marshal(&waHistorySync.HistorySync{
				SyncType:      waHistorySync.HistorySync_RECENT.Enum(),
				Conversations: []*waHistorySync.Conversation{{ID: proto.String(tc.chat), Messages: []*waHistorySync.HistorySyncMsg{tc.build()}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := r.projectHistory(context.Background(), wire); err == nil || !r.Status().ReconciliationRequired {
				t.Fatal("unverified uncertainty notice shape bypassed strict safety fence")
			}
		})
	}
}
