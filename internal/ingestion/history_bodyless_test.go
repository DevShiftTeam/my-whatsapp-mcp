package ingestion

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"google.golang.org/protobuf/proto"
)

func bodylessHistoryRecord(chat, id, body string) *waHistorySync.HistorySyncMsg {
	record := historyRecord(id, body)
	record.Message.Key.RemoteJID = proto.String(chat)
	if strings.HasSuffix(chat, "@g.us") {
		record.Message.Participant = proto.String("200@s.whatsapp.net")
	}
	return record
}

func bodylessHistoryStub(chat, id string, kind waWeb.WebMessageInfo_StubType) *waHistorySync.HistorySyncMsg {
	record := bodylessHistoryRecord(chat, id, "")
	record.Message.Message = nil
	record.Message.MessageStubType = kind.Enum()
	// This is metadata about the received stub, not the original body's date
	// or an authenticated deletion order that the adapter may invent.
	record.Message.MessageTimestamp = proto.Uint64(8000)
	return record
}

func bodylessHistoryWire(t *testing.T, chat string, records ...*waHistorySync.HistorySyncMsg) []byte {
	t.Helper()
	raw, err := proto.Marshal(&waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum(), Conversations: []*waHistorySync.Conversation{{ID: proto.String(chat), Messages: records}}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func bodylessHistoryKey(chat, id string) archive.SourceKey {
	return archive.SourceKey{Account: durableAccount, Chat: chat, Sender: "200@s.whatsapp.net", Message: id}
}

func bodylessHistoryStatus(t *testing.T, store *archive.Store) archive.StatusSnapshot {
	t.Helper()
	policy, err := store.Policy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.Status(context.Background(), archive.ReadScope{Account: durableAccount, Principal: "local_owner", AllChats: true, PolicyGeneration: policy.Generation})
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func TestHistoryBodylessAdminRevokeUsesOriginalIdentityAndContinuesTail(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		existing, originalClaim, fromMe bool
	}{
		{"actor_differs_from_original_key_author", true, true, false},
		{"unique_retained_target_without_original_claim", true, false, false},
		{"absent_target_with_explicit_original_claim", false, true, false},
		{"absent_target_owned_by_saved_account", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const chat = "300@g.us"
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			ctx := context.Background()
			key := bodylessHistoryKey(chat, "bodyless-admin-target")
			original := bodylessHistoryRecord(chat, key.Message, "bodylessadminrevokedreceipt original")
			stub := bodylessHistoryStub(chat, key.Message, waWeb.WebMessageInfo_ADMIN_REVOKE)
			stub.Message.Participant = proto.String("999@s.whatsapp.net") // administrator actor
			stub.Message.Key.Participant = nil
			if tc.fromMe {
				key.FromMe, key.Sender = true, durableAccount
				original.Message.Key.FromMe = proto.Bool(true)
				original.Message.Participant = proto.String(durableAccount)
				original.Message.OriginalSelfAuthorUserJIDString = proto.String(durableAccount)
				stub.Message.Key.FromMe = proto.Bool(true)
				stub.Message.OriginalSelfAuthorUserJIDString = proto.String(durableAccount)
			}
			if tc.originalClaim {
				stub.Message.Key.Participant = proto.String(key.Sender)
			}
			var expiry time.Time
			if tc.existing {
				expiry = time.Now().Add(time.Hour).Truncate(time.Second)
				original.Message.EphemeralExpirationTimestamp = proto.Uint64(uint64(expiry.Unix()))
				if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, original)); err != nil {
					t.Fatal(err)
				}
			}
			tail := bodylessHistoryRecord(chat, "bodyless-admin-tail", "bodylessadmintailreceipt שלום 👋")
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, stub, tail)); err != nil {
				t.Fatalf("admin actor was mistaken for the deleted original's author: %v", err)
			}
			queueBurstAssertSearch(t, store, "bodylessadminrevokedreceipt", 0)
			queueBurstAssertSearch(t, store, "bodylessadmintailreceipt", 1)
			metadata, err := store.SourceMetadata(ctx, key)
			if err != nil || !metadata.Version.Deleted || metadata.ProtocolRevision != 0 || metadata.OriginalDateKnown != tc.existing || !metadata.Version.ExpiresAt.Equal(expiry) {
				t.Fatalf("admin deletion lost original identity/date/expiry or invented operation time: deleted=%t revision=%d known=%t err=%v", metadata.Version.Deleted, metadata.ProtocolRevision, metadata.OriginalDateKnown, err)
			}
			if tc.existing && !metadata.OriginalSentAt.Equal(time.Unix(1000, 0)) {
				t.Fatal("admin stub timestamp replaced the original's date")
			}
			actor := bodylessHistoryKey(chat, key.Message)
			actor.Sender = "999@s.whatsapp.net"
			actorMetadata, err := store.SourceMetadata(ctx, actor)
			if err != nil || actorMetadata.Version.MessageID != "" || actorMetadata.Version.Deleted || actorMetadata.OriginalDateKnown {
				t.Fatal("administrator actor acquired a fabricated original or tombstone")
			}
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, original)); err != nil {
				t.Fatal(err)
			}
			queueBurstAssertSearch(t, store, "bodylessadminrevokedreceipt", 0)
			if r.Status().ReconciliationRequired || bodylessHistoryStatus(t, store).Messages != 1 {
				t.Fatal("terminal admin deletion fenced later messages or created a fake searchable row")
			}
		})
	}
}

// A valid admin control identifies the deleted protocol ID even when history
// does not reveal its original author. Withhold that ID across all authors
// instead of fabricating the administrator as the source or stopping the tail.
func TestHistoryBodylessAdminRevokeUnknownAuthorWithholdsIDAndContinuesTail(t *testing.T) {
	const chat = "300@g.us"
	const target = "bodyless-admin-unresolved-target"
	store, dir := durableFixture(t)
	r := durableRuntime(t, store)
	ctx := context.Background()
	stub := bodylessHistoryStub(chat, target, waWeb.WebMessageInfo_ADMIN_REVOKE)
	stub.Message.Participant = proto.String("999@s.whatsapp.net") // actor only
	stub.Message.Key.Participant = nil
	tail := bodylessHistoryRecord(chat, "bodyless-admin-unresolved-tail", "bodylessadminunresolvedtailreceipt שלום")
	if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, stub, tail)); err != nil {
		t.Fatalf("valid senderless admin control stopped readable history: %v", err)
	}
	queueBurstAssertSearch(t, store, "שלום", 1)
	queueBurstAssertSearch(t, store, "bodylessadminunresolvedtailreceipt", 1)
	if r.Status().ReconciliationRequired || bodylessHistoryStatus(t, store).Messages != 1 {
		t.Fatal("senderless deletion fenced unrelated content or invented a searchable row")
	}
	for _, sender := range []string{"200@s.whatsapp.net", "999@s.whatsapp.net"} {
		key := bodylessHistoryKey(chat, target)
		key.Sender = sender
		metadata, err := store.SourceMetadata(ctx, key)
		if err != nil || metadata.Version.MessageID != "" || metadata.Version.Revision != 0 || metadata.ProtocolRevision != 0 || metadata.OriginalDateKnown || !metadata.OriginalSentAt.IsZero() {
			t.Fatalf("senderless control fabricated original author/date/revision: sender=%s err=%v", sender, err)
		}
	}
	policy, err := store.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	anchors, err := store.SelectedBackfillAnchors(ctx, archive.BackfillSelection{Account: durableAccount, PolicyGeneration: policy.Generation, AllSelected: true})
	if err != nil || len(anchors) != 1 || anchors[0].Key.Message != "bodyless-admin-unresolved-tail" {
		t.Fatalf("senderless control invented a history anchor: count=%d err=%v", len(anchors), err)
	}
	original := bodylessHistoryRecord(chat, target, "bodylessadminunresolvedtargetreceipt original")
	if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, original)); err != nil {
		t.Fatal(err)
	}
	queueBurstAssertSearch(t, store, "bodylessadminunresolvedtargetreceipt", 0)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := durableStore(t, dir)
	replayed := durableRuntime(t, reopened)
	if err := replayed.projectHistory(ctx, bodylessHistoryWire(t, chat, original)); err != nil {
		t.Fatal(err)
	}
	policy, err = reopened.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	edit := archive.SourceEvent{Key: bodylessHistoryKey(chat, target), Kind: archive.EventEditContent, OriginalSentAt: time.Unix(1000, 0), OriginalDateKnown: true, SourceRevision: 9000, Body: "bodylessadminunresolvedtargetreceipt later edit", BodyKind: archive.MessageText}
	if _, err := reopened.ApplySourceEvent(ctx, edit, policy.Generation); err != nil {
		t.Fatal(err)
	}
	queueBurstAssertSearch(t, reopened, "bodylessadminunresolvedtargetreceipt", 0)
	queueBurstAssertSearch(t, reopened, "שלום", 1)
	if replayed.Status().ReconciliationRequired || bodylessHistoryStatus(t, reopened).Messages != 1 {
		t.Fatal("durable senderless deletion was lost or stopped the readable tail")
	}
}

func TestHistoryBodylessAdminUnresolvedPersistenceFailureStaysStrict(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, archive.ErrArchiveUnavailable} {
		t.Run(failure.Error(), func(t *testing.T) {
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			r.store = &bodylessHistoryUnresolvedFailureStore{ConnectorStore: store, failure: failure}
			stub := bodylessHistoryStub("300@g.us", "bodyless-unresolved-write-failure", waWeb.WebMessageInfo_ADMIN_REVOKE)
			stub.Message.Participant = proto.String("999@s.whatsapp.net")
			stub.Message.Key.Participant = nil
			tail := bodylessHistoryRecord("300@g.us", "bodyless-unresolved-failure-tail", "bodylessunresolvedfailuretailreceipt")
			if err := r.projectHistory(context.Background(), bodylessHistoryWire(t, "300@g.us", stub, tail)); err == nil || !r.Status().ReconciliationRequired {
				t.Fatalf("failed durable senderless guard admitted later bodies: err=%v unsafe=%t", err, r.Status().ReconciliationRequired)
			}
			metadata, err := store.SourceMetadata(context.Background(), bodylessHistoryKey("300@g.us", "bodyless-unresolved-failure-tail"))
			if err != nil || metadata.Version.MessageID != "" || metadata.OriginalDateKnown {
				t.Fatal("failed guard persistence created a tail body or anchor")
			}
			if _, err := durableSearch(t, store, "bodylessunresolvedfailuretailreceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Fatalf("guard persistence failure left content authorized: %v", err)
			}
		})
	}
}

type bodylessHistoryUnresolvedFailureStore struct {
	archive.ConnectorStore
	failure error
}

func (s *bodylessHistoryUnresolvedFailureStore) RecordUnresolvedDeletion(context.Context, string, string, string) error {
	return s.failure
}

func TestHistoryBodylessAdminRevokeRejectsAmbiguousOriginalIdentity(t *testing.T) {
	for _, name := range []string{"mismatched_original_claim", "invalid_administrator_actor", "ambiguous_retained_targets", "mismatched_direction", "wrong_saved_owner_author"} {
		t.Run(name, func(t *testing.T) {
			const chat = "300@g.us"
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			ctx := context.Background()
			original := bodylessHistoryRecord(chat, "bodyless-admin-unsafe-target", "bodylessadminunsafetargetreceipt")
			key := bodylessHistoryKey(chat, "bodyless-admin-unsafe-target")
			if name == "wrong_saved_owner_author" {
				original.Message.Key.FromMe = proto.Bool(true)
				original.Message.Participant = proto.String(durableAccount)
				original.Message.OriginalSelfAuthorUserJIDString = proto.String(durableAccount)
				key.FromMe, key.Sender = true, durableAccount
			}
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, original)); err != nil {
				t.Fatal(err)
			}
			stub := bodylessHistoryStub(chat, key.Message, waWeb.WebMessageInfo_ADMIN_REVOKE)
			stub.Message.Participant = proto.String("999@s.whatsapp.net")
			stub.Message.Key.Participant = proto.String(key.Sender)
			switch name {
			case "mismatched_original_claim":
				stub.Message.Key.Participant = proto.String("777@s.whatsapp.net")
			case "invalid_administrator_actor":
				stub.Message.Participant = proto.String("@s.whatsapp.net")
			case "ambiguous_retained_targets":
				other := bodylessHistoryRecord(chat, key.Message, "bodylessadminotherauthorreceipt")
				other.Message.Participant = proto.String("777@s.whatsapp.net")
				if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, other)); err != nil {
					t.Fatal(err)
				}
			case "mismatched_direction":
				stub.Message.Key.FromMe = proto.Bool(true)
				stub.Message.Key.Participant = proto.String(durableAccount)
				stub.Message.OriginalSelfAuthorUserJIDString = proto.String(durableAccount)
			case "wrong_saved_owner_author":
				stub.Message.Key.FromMe = proto.Bool(true)
				stub.Message.OriginalSelfAuthorUserJIDString = proto.String("999@s.whatsapp.net")
			}
			tail := bodylessHistoryRecord(chat, "bodyless-admin-unsafe-tail", "bodylessadminunsafetailreceipt")
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, stub, tail)); err == nil || !r.Status().ReconciliationRequired {
				t.Fatalf("unresolved original-author authority bypassed the source fence: err=%v unsafe=%t", err, r.Status().ReconciliationRequired)
			}
			metadata, err := store.SourceMetadata(ctx, key)
			if err != nil || metadata.Version.Deleted {
				t.Fatal("ambiguous admin record applied an unauthorized terminal deletion")
			}
			tailMetadata, err := store.SourceMetadata(ctx, bodylessHistoryKey(chat, "bodyless-admin-unsafe-tail"))
			if err != nil || tailMetadata.Version.MessageID != "" || tailMetadata.OriginalDateKnown {
				t.Fatal("unresolved admin control admitted later message content or a fake cursor")
			}
			if _, err := durableSearch(t, store, "bodylessadminunsafetargetreceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
				t.Fatalf("unresolved admin control left content available: %v", err)
			}
		})
	}
}

func TestHistoryBodylessRevokeIsTerminalAndContinuesReadableTail(t *testing.T) {
	for _, chat := range []string{"200@s.whatsapp.net", "300@g.us"} {
		for _, existing := range []bool{false, true} {
			name := chat + "/unknown"
			if existing {
				name = chat + "/existing"
			}
			t.Run(name, func(t *testing.T) {
				store, _ := durableFixture(t)
				r := durableRuntime(t, store)
				ctx := context.Background()
				original := bodylessHistoryRecord(chat, "bodyless-revoke-target", "bodylessrevokedreceipt original")
				if existing {
					if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, original)); err != nil {
						t.Fatal(err)
					}
				}
				stub := bodylessHistoryStub(chat, "bodyless-revoke-target", waWeb.WebMessageInfo_REVOKE)
				tail := bodylessHistoryRecord(chat, "bodyless-revoke-tail", "bodylessrevoketailreceipt שלום 👋")
				if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, stub, original, tail)); err != nil {
					t.Fatalf("authentic bodyless revoke stopped readable history: %v", err)
				}
				if r.Status().ReconciliationRequired {
					t.Fatal("projected bodyless revoke fenced the archive")
				}
				queueBurstAssertSearch(t, store, "bodylessrevokedreceipt", 0)
				queueBurstAssertSearch(t, store, "bodylessrevoketailreceipt", 1)
				metadata, err := store.SourceMetadata(ctx, bodylessHistoryKey(chat, "bodyless-revoke-target"))
				if err != nil || !metadata.Version.Deleted || metadata.ProtocolRevision != 0 {
					t.Fatalf("bodyless revoke did not preserve a zero-order terminal delete: deleted=%t revision=%d err=%v", metadata.Version.Deleted, metadata.ProtocolRevision, err)
				}
				// The later genuine original can fill metadata for an initially
				// unknown target; the stub's 8000 timestamp cannot supply it.
				if !metadata.OriginalDateKnown || !metadata.OriginalSentAt.Equal(time.Unix(1000, 0)) {
					t.Fatal("stub timestamp replaced the real original date")
				}
				// Replay in another batch must not resurrect a terminal source.
				if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, original)); err != nil {
					t.Fatal(err)
				}
				queueBurstAssertSearch(t, store, "bodylessrevokedreceipt", 0)
			})
		}
	}
	for _, tc := range []struct {
		name                     string
		fromMe, optionalDefaults bool
	}{
		{"own_author_from_me", true, false},
		{"default_false_and_omitted_remote_jid", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const chat = "200@s.whatsapp.net"
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			ctx := context.Background()
			original := bodylessHistoryRecord(chat, "bodyless-optional-revoke-target", "bodylessoptionalrevokedreceipt")
			stub := bodylessHistoryStub(chat, "bodyless-optional-revoke-target", waWeb.WebMessageInfo_REVOKE)
			key := bodylessHistoryKey(chat, "bodyless-optional-revoke-target")
			if tc.fromMe {
				original.Message.Key.FromMe = proto.Bool(true)
				original.Message.OriginalSelfAuthorUserJIDString = proto.String(durableAccount)
				stub.Message.Key.FromMe = proto.Bool(true)
				stub.Message.OriginalSelfAuthorUserJIDString = proto.String(durableAccount)
				key.FromMe = true
				key.Sender = durableAccount
			}
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, original)); err != nil {
				t.Fatal(err)
			}
			if tc.optionalDefaults {
				// The pinned parser defaults an omitted fromMe to false and
				// receives its chat from the containing conversation.
				stub.Message.Key.FromMe = nil
				stub.Message.Key.RemoteJID = nil
			}
			tail := bodylessHistoryRecord(chat, "bodyless-optional-tail", "bodylessoptionaltailreceipt שלום 👋")
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, stub, original, tail)); err != nil {
				t.Fatalf("valid SDK direction/defaults could not resolve a terminal revoke: %v", err)
			}
			queueBurstAssertSearch(t, store, "bodylessoptionalrevokedreceipt", 0)
			queueBurstAssertSearch(t, store, "bodylessoptionaltailreceipt", 1)
			metadata, err := store.SourceMetadata(ctx, key)
			if err != nil || !metadata.Version.Deleted || metadata.ProtocolRevision != 0 || !metadata.OriginalSentAt.Equal(time.Unix(1000, 0)) {
				t.Fatalf("valid SDK defaults/direction selected the wrong source: deleted=%t revision=%d err=%v", metadata.Version.Deleted, metadata.ProtocolRevision, err)
			}
		})
	}
}

func TestHistoryBodylessMetadataPreservesReadableTailAndRealControl(t *testing.T) {
	type metadataCase struct {
		kind            waWeb.WebMessageInfo_StubType
		authorlessGroup bool
	}
	var cases []metadataCase
	for _, kind := range []waWeb.WebMessageInfo_StubType{
		waWeb.WebMessageInfo_CIPHERTEXT,
		waWeb.WebMessageInfo_GROUP_CREATE,
		waWeb.WebMessageInfo_GROUP_CHANGE_SUBJECT,
		waWeb.WebMessageInfo_GROUP_CHANGE_ICON,
		waWeb.WebMessageInfo_GROUP_CHANGE_INVITE_LINK,
		waWeb.WebMessageInfo_GROUP_CHANGE_DESCRIPTION,
		waWeb.WebMessageInfo_GROUP_CHANGE_RESTRICT,
		waWeb.WebMessageInfo_GROUP_CHANGE_ANNOUNCE,
		waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD,
		waWeb.WebMessageInfo_GROUP_PARTICIPANT_REMOVE,
		waWeb.WebMessageInfo_GROUP_PARTICIPANT_PROMOTE,
		waWeb.WebMessageInfo_GROUP_PARTICIPANT_DEMOTE,
		waWeb.WebMessageInfo_GROUP_PARTICIPANT_INVITE,
		waWeb.WebMessageInfo_GROUP_PARTICIPANT_LEAVE,
		waWeb.WebMessageInfo_GROUP_PARTICIPANT_LINKED_GROUP_JOIN,
		waWeb.WebMessageInfo_GROUP_V4_ADD_INVITE_SENT,
		waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD_REQUEST_JOIN,
		waWeb.WebMessageInfo_GROUP_INVITE_LINK_GROWTH_LOCKED,
		waWeb.WebMessageInfo_COMMUNITY_LINK_PARENT_GROUP,
		waWeb.WebMessageInfo_COMMUNITY_LINK_SIBLING_GROUP,
		waWeb.WebMessageInfo_COMMUNITY_LINK_SUB_GROUP,
		waWeb.WebMessageInfo_COMMUNITY_UNLINK_PARENT_GROUP,
		waWeb.WebMessageInfo_COMMUNITY_UNLINK_SIBLING_GROUP,
		waWeb.WebMessageInfo_COMMUNITY_UNLINK_SUB_GROUP,
		waWeb.WebMessageInfo_GROUP_PARTICIPANT_ACCEPT,
		waWeb.WebMessageInfo_COMMUNITY_CREATE,
		waWeb.WebMessageInfo_GROUP_MEMBERSHIP_JOIN_APPROVAL_REQUEST,
		waWeb.WebMessageInfo_GROUP_MEMBERSHIP_JOIN_APPROVAL_MODE,
		waWeb.WebMessageInfo_INTEGRITY_UNLINK_PARENT_GROUP,
		waWeb.WebMessageInfo_COMMUNITY_PARTICIPANT_PROMOTE,
		waWeb.WebMessageInfo_COMMUNITY_PARTICIPANT_DEMOTE,
		waWeb.WebMessageInfo_COMMUNITY_LINK_PARENT_GROUP_MEMBERSHIP_APPROVAL,
		waWeb.WebMessageInfo_GROUP_PARTICIPANT_JOINED_GROUP_AND_PARENT_GROUP,
		waWeb.WebMessageInfo_CAG_INVITE_AUTO_ADD,
		waWeb.WebMessageInfo_CAG_INVITE_AUTO_JOINED,
		waWeb.WebMessageInfo_COMMUNITY_INVITE_RICH,
		waWeb.WebMessageInfo_COMMUNITY_INVITE_AUTO_ADD_RICH,
		waWeb.WebMessageInfo_SUB_GROUP_INVITE_RICH,
		waWeb.WebMessageInfo_SUB_GROUP_PARTICIPANT_ADD_RICH,
		waWeb.WebMessageInfo_COMMUNITY_LINK_PARENT_GROUP_RICH,
		waWeb.WebMessageInfo_COMMUNITY_PARTICIPANT_ADD_RICH,
		waWeb.WebMessageInfo_GROUP_MEMBER_ADD_MODE,
		waWeb.WebMessageInfo_GROUP_MEMBERSHIP_JOIN_APPROVAL_REQUEST_NON_ADMIN_ADD,
		waWeb.WebMessageInfo_COMMUNITY_ALLOW_MEMBER_ADDED_GROUPS,
		waWeb.WebMessageInfo_EMPTY_SUBGROUP_CREATE,
		waWeb.WebMessageInfo_SUBGROUP_ADMIN_TRIGGERED_AUTO_ADD_RICH,
		waWeb.WebMessageInfo_E2E_IDENTITY_CHANGED,
		waWeb.WebMessageInfo_E2E_ENCRYPTED,
		waWeb.WebMessageInfo_E2E_DEVICE_CHANGED,
		waWeb.WebMessageInfo_E2E_ENCRYPTED_NOW,
		waWeb.WebMessageInfo_BIZ_PRIVACY_MODE_INIT_FB,
		waWeb.WebMessageInfo_BIZ_PRIVACY_MODE_INIT_BSP,
		waWeb.WebMessageInfo_BIZ_PRIVACY_MODE_TO_FB,
		waWeb.WebMessageInfo_BIZ_PRIVACY_MODE_TO_BSP,
		waWeb.WebMessageInfo_CALL_MISSED_VOICE,
		waWeb.WebMessageInfo_CALL_MISSED_VIDEO,
		waWeb.WebMessageInfo_CALL_MISSED_GROUP_VOICE,
		waWeb.WebMessageInfo_CALL_MISSED_GROUP_VIDEO,
	} {
		cases = append(cases, metadataCase{kind: kind})
	}
	cases = append(cases,
		metadataCase{kind: waWeb.WebMessageInfo_E2E_ENCRYPTED, authorlessGroup: true},
		metadataCase{kind: waWeb.WebMessageInfo_GROUP_CREATE, authorlessGroup: true},
	)
	for _, tc := range cases {
		kind := tc.kind
		name := kind.String()
		if tc.authorlessGroup {
			name += "_authorless_group"
		}
		t.Run(name, func(t *testing.T) {
			chat := "300@g.us"
			if !tc.authorlessGroup {
				switch kind {
				case waWeb.WebMessageInfo_E2E_IDENTITY_CHANGED, waWeb.WebMessageInfo_E2E_ENCRYPTED, waWeb.WebMessageInfo_E2E_DEVICE_CHANGED, waWeb.WebMessageInfo_E2E_ENCRYPTED_NOW,
					waWeb.WebMessageInfo_BIZ_PRIVACY_MODE_INIT_FB, waWeb.WebMessageInfo_BIZ_PRIVACY_MODE_INIT_BSP, waWeb.WebMessageInfo_BIZ_PRIVACY_MODE_TO_FB, waWeb.WebMessageInfo_BIZ_PRIVACY_MODE_TO_BSP,
					waWeb.WebMessageInfo_CALL_MISSED_VOICE, waWeb.WebMessageInfo_CALL_MISSED_VIDEO:
					chat = "200@s.whatsapp.net"
				}
			}
			store, dir := durableFixture(t)
			r := durableRuntime(t, store)
			ctx := context.Background()
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, bodylessHistoryRecord(chat, "bodyless-control-target", "bodylesscontroltargetreceipt"))); err != nil {
				t.Fatal(err)
			}
			stub := bodylessHistoryStub(chat, "bodyless-metadata", kind)
			if tc.authorlessGroup {
				stub.Message.Participant = nil
				stub.Message.Key.Participant = nil
			}
			tail := bodylessHistoryRecord(chat, "bodyless-readable-tail", "bodylessreadabletailreceipt שלום 👋")
			control := bodylessHistoryRecord(chat, "bodyless-real-control", "")
			control.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("bodyless-control-target"), RemoteJID: proto.String(chat), Participant: proto.String("200@s.whatsapp.net"), FromMe: proto.Bool(false)}}}
			if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, stub, tail, control)); err != nil {
				t.Fatalf("supported bodyless metadata stopped history: %v", err)
			}
			queueBurstAssertSearch(t, store, "bodylessreadabletailreceipt", 1)
			queueBurstAssertSearch(t, store, "bodylesscontroltargetreceipt", 0)
			metadata, err := store.SourceMetadata(ctx, bodylessHistoryKey(chat, "bodyless-metadata"))
			if err != nil || metadata.OriginalDateKnown || metadata.Version.MessageID != "" || metadata.Version.Deleted {
				t.Fatalf("metadata stub became an active body, deletion, or source anchor: %+v err=%v", metadata, err)
			}
			anchors, err := store.BackfillAnchors(ctx, durableAccount, 32)
			if err != nil {
				t.Fatal(err)
			}
			for _, anchor := range anchors {
				if anchor.Key.Message == "bodyless-metadata" {
					t.Fatal("bodyless metadata became a fake history cursor")
				}
			}
			status := bodylessHistoryStatus(t, store)
			if status.Messages != 1 || r.Status().ReconciliationRequired {
				t.Fatalf("bodyless metadata produced a fake searchable row or global fence: messages=%d unsafe=%t", status.Messages, r.Status().ReconciliationRequired)
			}
			if kind == waWeb.WebMessageInfo_CIPHERTEXT {
				if status.Coverage != "partial" || !slices.Contains(status.Codes, "undecryptable_message") {
					t.Fatalf("ciphertext omission was not durably flagged: coverage=%s codes=%v", status.Coverage, status.Codes)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				status = bodylessHistoryStatus(t, durableStore(t, dir))
				if status.Coverage != "partial" || !slices.Contains(status.Codes, "undecryptable_message") {
					t.Fatal("ciphertext history gap disappeared after archive reopen")
				}
			} else if slices.Contains(status.Codes, "undecryptable_message") {
				t.Fatal("benign group metadata invented an unreadable message gap")
			}
		})
	}
}

func TestHistoryBodylessUnsafeMetadataAndAmbiguousControlsRemainStrict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   waWeb.WebMessageInfo_StubType
		mutate func(*waWeb.WebMessageInfo)
	}{
		{"unknown", waWeb.WebMessageInfo_StubType(987654), nil},
		{"missing_stub", waWeb.WebMessageInfo_UNKNOWN, func(m *waWeb.WebMessageInfo) { m.MessageStubType = nil }},
		{"admin_revoke_without_unique_target", waWeb.WebMessageInfo_ADMIN_REVOKE, nil},
		{"expiry_control", waWeb.WebMessageInfo_CHANGE_EPHEMERAL_SETTING, nil},
		{"viewed_once_control", waWeb.WebMessageInfo_VIEWED_ONCE, nil},
		{"disappearing_mode_control", waWeb.WebMessageInfo_DISAPPEARING_MODE, nil},
		{"keep_control", waWeb.WebMessageInfo_EPHEMERAL_KEEP_IN_CHAT, nil},
		{"group_sender_number_migration_conflicting_actor", waWeb.WebMessageInfo_GROUP_PARTICIPANT_CHANGE_NUMBER, func(m *waWeb.WebMessageInfo) { m.Key.Participant = proto.String("777@s.whatsapp.net") }},
		{"individual_number_migration", waWeb.WebMessageInfo_INDIVIDUAL_CHANGE_NUMBER, nil},
		{"lid_migration", waWeb.WebMessageInfo_CHANGE_LID, nil},
		{"group_delete", waWeb.WebMessageInfo_GROUP_DELETE, nil},
		{"community_parent_group_delete", waWeb.WebMessageInfo_COMMUNITY_PARENT_GROUP_DELETED, nil},
		{"group_deactivated", waWeb.WebMessageInfo_GROUP_DEACTIVATED, nil},
		{"community_sibling_group_deactivated", waWeb.WebMessageInfo_COMMUNITY_DEACTIVATE_SIBLING_GROUP, nil},
		{"quarantined_message", waWeb.WebMessageInfo_QUARANTINED_MESSAGE, nil},
		{"expiry_timestamp", waWeb.WebMessageInfo_GROUP_CREATE, func(m *waWeb.WebMessageInfo) { m.EphemeralExpirationTimestamp = proto.Uint64(9000) }},
		{"expiry_duration", waWeb.WebMessageInfo_CIPHERTEXT, func(m *waWeb.WebMessageInfo) { m.EphemeralDuration = proto.Uint32(60) }},
		{"encryption_notice_with_expiry_metadata", waWeb.WebMessageInfo_E2E_ENCRYPTED, func(m *waWeb.WebMessageInfo) { m.EphemeralDuration = proto.Uint32(60) }},
		{"keep_metadata", waWeb.WebMessageInfo_GROUP_CREATE, func(m *waWeb.WebMessageInfo) { m.KeepInChat = &waWeb.KeepInChat{} }},
		{"encryption_notice_with_keep_metadata", waWeb.WebMessageInfo_E2E_ENCRYPTED_NOW, func(m *waWeb.WebMessageInfo) { m.KeepInChat = &waWeb.KeepInChat{} }},
		{"future_metadata", waWeb.WebMessageInfo_GROUP_CREATE, func(m *waWeb.WebMessageInfo) { m.FutureproofData = []byte{1} }},
		{"missing_group_sender", waWeb.WebMessageInfo_REVOKE, func(m *waWeb.WebMessageInfo) { m.Participant = nil }},
		{"invalid_group_sender", waWeb.WebMessageInfo_REVOKE, func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("@s.whatsapp.net") }},
		{"mismatched_participants", waWeb.WebMessageInfo_REVOKE, func(m *waWeb.WebMessageInfo) { m.Key.Participant = proto.String("999@s.whatsapp.net") }},
		{"benign_mismatched_participants", waWeb.WebMessageInfo_GROUP_CREATE, func(m *waWeb.WebMessageInfo) { m.Key.Participant = proto.String("999@s.whatsapp.net") }},
		{"benign_malformed_present_participant", waWeb.WebMessageInfo_E2E_ENCRYPTED, func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("@s.whatsapp.net") }},
		{"benign_malformed_key_participant", waWeb.WebMessageInfo_GROUP_CREATE, func(m *waWeb.WebMessageInfo) { m.Key.Participant = proto.String("@s.whatsapp.net") }},
		{"mismatched_chat", waWeb.WebMessageInfo_REVOKE, func(m *waWeb.WebMessageInfo) { m.Key.RemoteJID = proto.String("999@g.us") }},
		{"missing_id", waWeb.WebMessageInfo_REVOKE, func(m *waWeb.WebMessageInfo) { m.Key.ID = nil }},
		{"benign_missing_id", waWeb.WebMessageInfo_E2E_ENCRYPTED, func(m *waWeb.WebMessageInfo) { m.Key.ID = nil }},
		{"benign_nul_id", waWeb.WebMessageInfo_E2E_ENCRYPTED, func(m *waWeb.WebMessageInfo) { m.Key.ID = proto.String("bodyless\x00id") }},
		{"benign_invalid_utf8_id", waWeb.WebMessageInfo_E2E_ENCRYPTED, func(m *waWeb.WebMessageInfo) { m.Key.ID = proto.String("bodyless-invalid-id") }},
		{"benign_oversized_id", waWeb.WebMessageInfo_GROUP_CREATE, func(m *waWeb.WebMessageInfo) { m.Key.ID = proto.String(strings.Repeat("x", 257)) }},
		{"from_me_wrong_author", waWeb.WebMessageInfo_REVOKE, func(m *waWeb.WebMessageInfo) {
			m.Key.FromMe = proto.Bool(true)
			m.OriginalSelfAuthorUserJIDString = proto.String("999@s.whatsapp.net")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const chat = "300@g.us"
			store := fixtureStore()
			r := testRuntime(store)
			stub := bodylessHistoryStub(chat, "bodyless-unsafe-stub", tc.kind)
			if tc.mutate != nil {
				tc.mutate(stub.Message)
			}
			tail := bodylessHistoryRecord(chat, "bodyless-unsafe-tail", "bodylessunsafetailreceipt")
			wire := bodylessHistoryWire(t, chat, stub, tail)
			if tc.name == "benign_invalid_utf8_id" {
				// Protobuf's normal encoder rejects invalid UTF-8. Preserve the
				// declared wire lengths while emulating the malformed source bytes.
				marker := []byte("bodyless-invalid-id")
				wire = bytes.Replace(wire, marker, bytes.Repeat([]byte{0xff}, len(marker)), 1)
			}
			if err := r.projectHistory(context.Background(), wire); err == nil || !r.Status().ReconciliationRequired {
				t.Fatalf("unprojectable bodyless metadata bypassed the source fence: err=%v unsafe=%t", err, r.Status().ReconciliationRequired)
			}
			if len(store.unsafe) == 0 || len(store.events) != 0 {
				t.Fatal("ambiguous stub admitted later bodies or failed to mark source coverage unsafe")
			}
		})
	}
	// Failure to record an omission remains strict even for supported ciphertext.
	for _, failure := range []error{context.DeadlineExceeded, errors.New("synthetic_history_gap_write_failure")} {
		t.Run("gap_persistence_"+failure.Error(), func(t *testing.T) {
			store := fixtureStore()
			r := testRuntime(store)
			r.store = &bodylessHistoryGapFailureStore{ConnectorStore: store, failure: failure}
			stub := bodylessHistoryStub("200@s.whatsapp.net", "bodyless-gap-write-failure", waWeb.WebMessageInfo_CIPHERTEXT)
			if err := r.projectHistory(context.Background(), bodylessHistoryWire(t, "200@s.whatsapp.net", stub, historyRecord("bodyless-gap-failure-tail", "bodylessgapfailuretailreceipt"))); err == nil || !r.Status().ReconciliationRequired || len(store.events) != 0 {
				t.Fatalf("failed ciphertext gap persistence continued readable bodies: err=%v unsafe=%t", err, r.Status().ReconciliationRequired)
			}
		})
	}
}

type bodylessHistoryGapFailureStore struct {
	archive.ConnectorStore
	failure error
}

func (s *bodylessHistoryGapFailureStore) SetSyncStatus(ctx context.Context, update archive.SyncUpdate) error {
	if update.GapCode == "undecryptable_message" {
		return s.failure
	}
	return s.ConnectorStore.SetSyncStatus(ctx, update)
}

func TestHistoryGroupNumberChangeObservedShapeContinuesBodyAndRevoke(t *testing.T) {
	const chat = "300@g.us"
	const noticeID = "synthetic-number-change-notice"
	const oldNumber = "901@s.whatsapp.net"
	const newNumber = "902@s.whatsapp.net"
	store, dir := durableFixture(t)
	r := durableRuntime(t, store)
	ctx := context.Background()
	target := bodylessHistoryRecord(chat, "number-change-target", "numberchangetargetreceipt")
	if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, target)); err != nil {
		t.Fatal(err)
	}
	notice := bodylessHistoryStub(chat, noticeID, waWeb.WebMessageInfo_GROUP_PARTICIPANT_CHANGE_NUMBER)
	notice.Message.Key.Participant = nil
	notice.Message.MessageStubParameters = []string{oldNumber, newNumber}
	if got := historyPresence(notice.Message); got != 475 {
		t.Fatalf("fixture does not match observed presence475: %d", got)
	}
	tail := bodylessHistoryRecord(chat, "number-change-tail", "numberchangetailreceipt")
	control := bodylessHistoryRecord(chat, "number-change-control", "")
	control.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("number-change-target"), RemoteJID: proto.String(chat), Participant: proto.String("200@s.whatsapp.net"), FromMe: proto.Bool(false)}}}
	if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, notice, tail, control)); err != nil {
		t.Fatalf("known number-change notice stopped valid body/control suffix: %v", err)
	}
	if r.Status().ReconciliationRequired {
		t.Fatal("known notice fenced valid history")
	}
	queueBurstAssertSearch(t, store, "numberchangetailreceipt", 1)
	queueBurstAssertSearch(t, store, "numberchangetargetreceipt", 0)
	targetMetadata, err := store.SourceMetadata(ctx, bodylessHistoryKey(chat, "number-change-target"))
	if err != nil || !targetMetadata.Version.Deleted {
		t.Fatal("following revoke was lost")
	}
	metadata, err := store.SourceMetadata(ctx, bodylessHistoryKey(chat, noticeID))
	if err != nil || metadata.Version.MessageID != "" || metadata.OriginalDateKnown || metadata.Version.Deleted {
		t.Fatal("notice fabricated source metadata")
	}
	anchors, err := store.BackfillAnchors(ctx, durableAccount, 32)
	if err != nil {
		t.Fatal(err)
	}
	for _, anchor := range anchors {
		if anchor.Key.Message == noticeID {
			t.Fatal("notice fabricated a cursor")
		}
	}
	for _, number := range []string{oldNumber, newNumber} {
		canonical, err := store.Canonical(ctx, durableAccount, number)
		if err != nil || canonical != number {
			t.Fatal("number parameters changed identity authority")
		}
	}
	if got := bodylessHistoryStatus(t, store).Messages; got != 1 {
		t.Fatalf("notice created searchable content: %d", got)
	}
	durableAssertAbsent(t, dir, oldNumber, newNumber, noticeID)
}

func TestHistoryGroupNumberChangePreservesSafetyAndActorFences(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*waWeb.WebMessageInfo)
	}{
		{"invalid_actor", func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("@s.whatsapp.net") }},
		{"conflicting_actor", func(m *waWeb.WebMessageInfo) { m.Key.Participant = proto.String("777@s.whatsapp.net") }},
		{"invalid_key", func(m *waWeb.WebMessageInfo) { m.Key.ID = proto.String("") }},
		{"wrong_chat", func(m *waWeb.WebMessageInfo) { m.Key.RemoteJID = proto.String("301@g.us") }},
		{"expiry_even_zero", func(m *waWeb.WebMessageInfo) { m.EphemeralDuration = proto.Uint32(0) }},
		{"futureproof", func(m *waWeb.WebMessageInfo) { m.FutureproofData = []byte{1} }},
		{"keep", func(m *waWeb.WebMessageInfo) { m.KeepInChat = &waWeb.KeepInChat{} }},
		{"target_control", func(m *waWeb.WebMessageInfo) {
			m.TargetMessageID = &waCommon.MessageKey{ID: proto.String("synthetic-target")}
		}},
		{"unknown_enum", func(m *waWeb.WebMessageInfo) { m.MessageStubType = waWeb.WebMessageInfo_StubType(987654).Enum() }},
		{"unknown_zero", func(m *waWeb.WebMessageInfo) { m.MessageStubType = waWeb.WebMessageInfo_UNKNOWN.Enum() }},
		{"individual_number_change", func(m *waWeb.WebMessageInfo) {
			m.MessageStubType = waWeb.WebMessageInfo_INDIVIDUAL_CHANGE_NUMBER.Enum()
		}},
		{"expiry_control", func(m *waWeb.WebMessageInfo) {
			m.MessageStubType = waWeb.WebMessageInfo_CHANGE_EPHEMERAL_SETTING.Enum()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := fixtureStore()
			r := testRuntime(store)
			notice := bodylessHistoryStub("300@g.us", "synthetic-number-change", waWeb.WebMessageInfo_GROUP_PARTICIPANT_CHANGE_NUMBER)
			notice.Message.MessageStubParameters = []string{"901@s.whatsapp.net", "902@s.whatsapp.net"}
			tc.mutate(notice.Message)
			tail := bodylessHistoryRecord("300@g.us", "unsafe-number-change-tail", "mustnotadmitreceipt")
			if err := r.projectHistory(context.Background(), bodylessHistoryWire(t, "300@g.us", notice, tail)); err == nil || !r.Status().ReconciliationRequired {
				t.Fatal("unsafe notice bypassed fence")
			}
			if len(store.events) != 0 || len(store.aliases) != 0 {
				t.Fatal("unsafe notice fabricated content or alias authority")
			}
		})
	}
}
