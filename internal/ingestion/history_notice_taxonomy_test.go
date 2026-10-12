package ingestion

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"google.golang.org/protobuf/proto"
)

// This explicit matrix is the reviewed display/admin/roster cohort. It does not
// grant message, identity, history-sharing or local permission authority.
var reviewedAdditionalHistoryNotices = []waWeb.WebMessageInfo_StubType{
	waWeb.WebMessageInfo_BROADCAST_CREATE,
	waWeb.WebMessageInfo_BROADCAST_ADD,
	waWeb.WebMessageInfo_BROADCAST_REMOVE,
	waWeb.WebMessageInfo_GROUP_CHANGE_NO_FREQUENTLY_FORWARDED,
	waWeb.WebMessageInfo_COMMUNITY_PARENT_GROUP_SUBJECT_CHANGED,
	waWeb.WebMessageInfo_COMMUNITY_CHANGE_DESCRIPTION,
	waWeb.WebMessageInfo_REPORT_TO_ADMIN_ENABLED_STATUS,
	waWeb.WebMessageInfo_GENERAL_CHAT_CREATE,
	waWeb.WebMessageInfo_GENERAL_CHAT_ADD,
	waWeb.WebMessageInfo_GENERAL_CHAT_AUTO_ADD_DISABLED,
	waWeb.WebMessageInfo_SUGGESTED_SUBGROUP_ANNOUNCE,
	waWeb.WebMessageInfo_COMMUNITY_OWNER_UPDATED,
	waWeb.WebMessageInfo_GROUP_MEMBER_LINK_MODE,
}

func TestReviewedHistoryNoticesHaveNoSourceOrIdentityAuthority(t *testing.T) {
	for _, kind := range reviewedAdditionalHistoryNotices {
		t.Run(kind.String(), func(t *testing.T) {
			store := fixtureStore()
			r := testRuntime(store)
			notice := bodylessHistoryStub("300@g.us", "synthetic-reviewed-notice", kind)
			notice.Message.Participant = nil
			notice.Message.MessageStubParameters = []string{"901@s.whatsapp.net", "902@s.whatsapp.net", "SYNTHETIC_NOTICE_PARAMETER_CANARY"}
			if historyPresence(notice.Message) != 411 {
				t.Fatal("notice fixture changed shape")
			}
			if err := r.projectHistory(context.Background(), bodylessHistoryWire(t, "300@g.us", notice)); err != nil {
				t.Fatalf("reviewed notice rejected: %v", err)
			}
			if r.Status().ReconciliationRequired || len(store.events) != 0 || len(store.aliases) != 0 || len(store.names) != 0 || len(store.metadata) != 0 {
				t.Fatal("notice fabricated source or identity authority")
			}
			assertReviewedHistoryNoticeSuffix(t, kind)
		})
	}
}

func TestReviewedHistoryNoticeSafetyMetadataAlwaysFences(t *testing.T) {
	for _, kind := range reviewedAdditionalHistoryNotices {
		for _, field := range []string{"EphemeralStartTimestamp", "EphemeralDuration", "EphemeralExpirationTimestamp", "EphemeralOffToOn", "EphemeralOutOfSync", "KeepInChat", "FutureproofData", "TargetMessageID", "MessageAddOns", "QuarantinedMessage", "GroupHistoryBundleInfo"} {
			t.Run(kind.String()+"/"+field, func(t *testing.T) {
				store := fixtureStore()
				r := testRuntime(store)
				notice := bodylessHistoryStub("300@g.us", "synthetic-unsafe-notice", kind)
				notice.Message.Participant = nil
				value := reflect.ValueOf(notice.Message).Elem().FieldByName(field)
				switch value.Kind() {
				case reflect.Pointer:
					value.Set(reflect.New(value.Type().Elem()))
				case reflect.Slice:
					value.Set(reflect.MakeSlice(value.Type(), 1, 1))
				default:
					t.Fatal("safety metadata fixture requires explicit presence")
				}
				tail := bodylessHistoryRecord("300@g.us", "unsafe-notice-tail", "mustnotadmitreceipt")
				if err := r.projectHistory(context.Background(), bodylessHistoryWire(t, "300@g.us", notice, tail)); err == nil || !r.Status().ReconciliationRequired {
					t.Fatal("notice swallowed safety metadata")
				}
				if len(store.events) != 0 || len(store.aliases) != 0 {
					t.Fatal("unsafe notice admitted content or identity authority")
				}
			})
		}
	}
}

func TestReviewedHistoryNoticeClaimsRemainStrict(t *testing.T) {
	for _, kind := range reviewedAdditionalHistoryNotices {
		for _, tc := range []struct {
			name   string
			mutate func(*waWeb.WebMessageInfo)
		}{
			{"missing_key", func(m *waWeb.WebMessageInfo) { m.Key = nil }},
			{"empty_id", func(m *waWeb.WebMessageInfo) { m.Key.ID = proto.String("") }},
			{"wrong_chat", func(m *waWeb.WebMessageInfo) { m.Key.RemoteJID = proto.String("301@g.us") }},
			{"malformed_actor", func(m *waWeb.WebMessageInfo) { m.Participant = proto.String("@s.whatsapp.net") }},
			{"conflicting_actor", func(m *waWeb.WebMessageInfo) {
				m.Participant = proto.String("200@s.whatsapp.net")
				m.Key.Participant = proto.String("777@s.whatsapp.net")
			}},
		} {
			t.Run(kind.String()+"/"+tc.name, func(t *testing.T) {
				store := fixtureStore()
				r := testRuntime(store)
				notice := bodylessHistoryStub("300@g.us", "synthetic-invalid-notice", kind)
				tc.mutate(notice.Message)
				tail := bodylessHistoryRecord("300@g.us", "invalid-notice-tail", "mustnotadmitreceipt")
				// Deliberately malformed claims must reach the decoder, including missing required keys.
				wire, err := (proto.MarshalOptions{AllowPartial: true}).Marshal(&waHistorySync.HistorySync{
					SyncType:      waHistorySync.HistorySync_RECENT.Enum(),
					Conversations: []*waHistorySync.Conversation{{ID: proto.String("300@g.us"), Messages: []*waHistorySync.HistorySyncMsg{notice, tail}}},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := r.projectHistory(context.Background(), wire); err == nil || !r.Status().ReconciliationRequired {
					t.Fatal("invalid notice claims bypassed fence")
				}
				if len(store.events) != 0 || len(store.aliases) != 0 {
					t.Fatal("invalid claims fabricated authority")
				}
			})
		}
	}
}

func TestHistoryNoticeTaxonomyDoesNotAdmitUnclassifiedControls(t *testing.T) {
	// Explicit exclusions from the reviewed taxonomy: history sharing, visibility,
	// sharing limits, deletion/deactivation, identity migration, retention, quarantine,
	// generic modification/futureproof, and unknown numeric values.
	for _, value := range []int32{186, 221, 208, 216, 43, 149, 203, 204, 213, 72, 74, 123, 130, 143, 248, 250, 220, 37, 3, 0, -1, 987654} {
		kind := waWeb.WebMessageInfo_StubType(value)
		t.Run(kind.String(), func(t *testing.T) {
			store := fixtureStore()
			r := testRuntime(store)
			notice := bodylessHistoryStub("300@g.us", "synthetic-unclassified-control", kind)
			tail := bodylessHistoryRecord("300@g.us", "unclassified-tail", "mustnotadmitreceipt")
			if err := r.projectHistory(context.Background(), bodylessHistoryWire(t, "300@g.us", notice, tail)); err == nil || !r.Status().ReconciliationRequired {
				t.Fatal("unclassified control was treated as a notice")
			}
			if len(store.events) != 0 || len(store.aliases) != 0 {
				t.Fatal("unclassified control admitted content or identity authority")
			}
		})
	}
}

func TestHistoryMemberLinkModeObservedShapeContinuesBodyRevokeAndEdit(t *testing.T) {
	assertReviewedHistoryNoticeSuffix(t, waWeb.WebMessageInfo_GROUP_MEMBER_LINK_MODE)
}

func assertReviewedHistoryNoticeSuffix(t *testing.T, kind waWeb.WebMessageInfo_StubType) {
	t.Helper()
	const chat = "300@g.us"
	const noticeID = "synthetic-memberlink-change-notice"
	const oldName = "SYNTHETIC_OLD_MEMBER_LINK_CANARY"
	const newName = "SYNTHETIC_NEW_MEMBER_LINK_CANARY"
	store, dir := durableFixture(t)
	r := durableRuntime(t, store)
	ctx := context.Background()
	target := bodylessHistoryRecord(chat, "memberlink-revoke-target", "memberlinkrevoketargetreceipt")
	editTarget := bodylessHistoryRecord(chat, "memberlink-edit-target", "memberlinkoldeditreceipt")
	if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, target, editTarget)); err != nil {
		t.Fatal(err)
	}
	notice := bodylessHistoryStub(chat, noticeID, kind)
	notice.Message.Participant = nil
	notice.Message.MessageStubParameters = []string{oldName, newName}
	if got := historyPresence(notice.Message); got != 411 {
		t.Fatalf("fixture does not match observed presence411: %d", got)
	}
	tail := bodylessHistoryRecord(chat, "memberlink-tail", "memberlinktailreceipt")
	revoke := bodylessHistoryRecord(chat, "memberlink-revoke", "")
	revoke.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("memberlink-revoke-target"), RemoteJID: proto.String(chat), FromMe: proto.Bool(false)}}}
	edit := bodylessHistoryRecord(chat, "memberlink-edit", "")
	edit.Message.MessageTimestamp = proto.Uint64(2000)
	edit.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("memberlink-edit-target"), RemoteJID: proto.String(chat), FromMe: proto.Bool(false), Participant: proto.String("200@s.whatsapp.net")}, TimestampMS: proto.Int64(2000123), EditedMessage: &waE2E.Message{Conversation: proto.String("memberlinkneweditreceipt")}}}
	if err := r.projectHistory(ctx, bodylessHistoryWire(t, chat, notice, tail, revoke, edit)); err != nil {
		t.Fatalf("known memberlink notice stopped valid body/control suffix: %v", err)
	}
	if r.Status().ReconciliationRequired {
		t.Fatal("memberlink notice fenced valid history")
	}
	queueBurstAssertSearch(t, store, "memberlinktailreceipt", 1)
	queueBurstAssertSearch(t, store, "memberlinkrevoketargetreceipt", 0)
	queueBurstAssertSearch(t, store, "memberlinkoldeditreceipt", 0)
	queueBurstAssertSearch(t, store, "memberlinkneweditreceipt", 1)
	metadata, err := store.SourceMetadata(ctx, bodylessHistoryKey(chat, noticeID))
	if err != nil || metadata.Version.MessageID != "" || metadata.OriginalDateKnown || metadata.Version.Deleted {
		t.Fatal("memberlink notice fabricated source metadata")
	}
	editMetadata, err := store.SourceMetadata(ctx, bodylessHistoryKey(chat, "memberlink-edit-target"))
	if err != nil || editMetadata.ProtocolRevision != 2000123 || !editMetadata.OriginalSentAt.Equal(time.Unix(1000, 0)) {
		t.Fatal("following edit lost original date or protocol revision")
	}
	anchors, err := store.BackfillAnchors(ctx, durableAccount, 32)
	if err != nil {
		t.Fatal(err)
	}
	for _, anchor := range anchors {
		if anchor.Key.Message == noticeID {
			t.Fatal("memberlink notice fabricated a cursor")
		}
	}
	if got := bodylessHistoryStatus(t, store).Messages; got != 2 {
		t.Fatalf("notice created searchable content: %d", got)
	}
	durableAssertAbsent(t, dir, oldName, newName, noticeID)
}

// Frozen pinned inventory: SDK additions or renames require a fresh disposition,
// rather than silently expanding production notice acceptance.
func TestPinnedHistoryNoticeTaxonomyExhaustive(t *testing.T) {
	expectedNames := map[int32]string{
		0:   "UNKNOWN",
		1:   "REVOKE",
		2:   "CIPHERTEXT",
		3:   "FUTUREPROOF",
		4:   "NON_VERIFIED_TRANSITION",
		5:   "UNVERIFIED_TRANSITION",
		6:   "VERIFIED_TRANSITION",
		7:   "VERIFIED_LOW_UNKNOWN",
		8:   "VERIFIED_HIGH",
		9:   "VERIFIED_INITIAL_UNKNOWN",
		10:  "VERIFIED_INITIAL_LOW",
		11:  "VERIFIED_INITIAL_HIGH",
		12:  "VERIFIED_TRANSITION_ANY_TO_NONE",
		13:  "VERIFIED_TRANSITION_ANY_TO_HIGH",
		14:  "VERIFIED_TRANSITION_HIGH_TO_LOW",
		15:  "VERIFIED_TRANSITION_HIGH_TO_UNKNOWN",
		16:  "VERIFIED_TRANSITION_UNKNOWN_TO_LOW",
		17:  "VERIFIED_TRANSITION_LOW_TO_UNKNOWN",
		18:  "VERIFIED_TRANSITION_NONE_TO_LOW",
		19:  "VERIFIED_TRANSITION_NONE_TO_UNKNOWN",
		20:  "GROUP_CREATE",
		21:  "GROUP_CHANGE_SUBJECT",
		22:  "GROUP_CHANGE_ICON",
		23:  "GROUP_CHANGE_INVITE_LINK",
		24:  "GROUP_CHANGE_DESCRIPTION",
		25:  "GROUP_CHANGE_RESTRICT",
		26:  "GROUP_CHANGE_ANNOUNCE",
		27:  "GROUP_PARTICIPANT_ADD",
		28:  "GROUP_PARTICIPANT_REMOVE",
		29:  "GROUP_PARTICIPANT_PROMOTE",
		30:  "GROUP_PARTICIPANT_DEMOTE",
		31:  "GROUP_PARTICIPANT_INVITE",
		32:  "GROUP_PARTICIPANT_LEAVE",
		33:  "GROUP_PARTICIPANT_CHANGE_NUMBER",
		34:  "BROADCAST_CREATE",
		35:  "BROADCAST_ADD",
		36:  "BROADCAST_REMOVE",
		37:  "GENERIC_NOTIFICATION",
		38:  "E2E_IDENTITY_CHANGED",
		39:  "E2E_ENCRYPTED",
		40:  "CALL_MISSED_VOICE",
		41:  "CALL_MISSED_VIDEO",
		42:  "INDIVIDUAL_CHANGE_NUMBER",
		43:  "GROUP_DELETE",
		44:  "GROUP_ANNOUNCE_MODE_MESSAGE_BOUNCE",
		45:  "CALL_MISSED_GROUP_VOICE",
		46:  "CALL_MISSED_GROUP_VIDEO",
		47:  "PAYMENT_CIPHERTEXT",
		48:  "PAYMENT_FUTUREPROOF",
		49:  "PAYMENT_TRANSACTION_STATUS_UPDATE_FAILED",
		50:  "PAYMENT_TRANSACTION_STATUS_UPDATE_REFUNDED",
		51:  "PAYMENT_TRANSACTION_STATUS_UPDATE_REFUND_FAILED",
		52:  "PAYMENT_TRANSACTION_STATUS_RECEIVER_PENDING_SETUP",
		53:  "PAYMENT_TRANSACTION_STATUS_RECEIVER_SUCCESS_AFTER_HICCUP",
		54:  "PAYMENT_ACTION_ACCOUNT_SETUP_REMINDER",
		55:  "PAYMENT_ACTION_SEND_PAYMENT_REMINDER",
		56:  "PAYMENT_ACTION_SEND_PAYMENT_INVITATION",
		57:  "PAYMENT_ACTION_REQUEST_DECLINED",
		58:  "PAYMENT_ACTION_REQUEST_EXPIRED",
		59:  "PAYMENT_ACTION_REQUEST_CANCELLED",
		60:  "BIZ_VERIFIED_TRANSITION_TOP_TO_BOTTOM",
		61:  "BIZ_VERIFIED_TRANSITION_BOTTOM_TO_TOP",
		62:  "BIZ_INTRO_TOP",
		63:  "BIZ_INTRO_BOTTOM",
		64:  "BIZ_NAME_CHANGE",
		65:  "BIZ_MOVE_TO_CONSUMER_APP",
		66:  "BIZ_TWO_TIER_MIGRATION_TOP",
		67:  "BIZ_TWO_TIER_MIGRATION_BOTTOM",
		68:  "OVERSIZED",
		69:  "GROUP_CHANGE_NO_FREQUENTLY_FORWARDED",
		70:  "GROUP_V4_ADD_INVITE_SENT",
		71:  "GROUP_PARTICIPANT_ADD_REQUEST_JOIN",
		72:  "CHANGE_EPHEMERAL_SETTING",
		73:  "E2E_DEVICE_CHANGED",
		74:  "VIEWED_ONCE",
		75:  "E2E_ENCRYPTED_NOW",
		76:  "BLUE_MSG_BSP_FB_TO_BSP_PREMISE",
		77:  "BLUE_MSG_BSP_FB_TO_SELF_FB",
		78:  "BLUE_MSG_BSP_FB_TO_SELF_PREMISE",
		79:  "BLUE_MSG_BSP_FB_UNVERIFIED",
		80:  "BLUE_MSG_BSP_FB_UNVERIFIED_TO_SELF_PREMISE_VERIFIED",
		81:  "BLUE_MSG_BSP_FB_VERIFIED",
		82:  "BLUE_MSG_BSP_FB_VERIFIED_TO_SELF_PREMISE_UNVERIFIED",
		83:  "BLUE_MSG_BSP_PREMISE_TO_SELF_PREMISE",
		84:  "BLUE_MSG_BSP_PREMISE_UNVERIFIED",
		85:  "BLUE_MSG_BSP_PREMISE_UNVERIFIED_TO_SELF_PREMISE_VERIFIED",
		86:  "BLUE_MSG_BSP_PREMISE_VERIFIED",
		87:  "BLUE_MSG_BSP_PREMISE_VERIFIED_TO_SELF_PREMISE_UNVERIFIED",
		88:  "BLUE_MSG_CONSUMER_TO_BSP_FB_UNVERIFIED",
		89:  "BLUE_MSG_CONSUMER_TO_BSP_PREMISE_UNVERIFIED",
		90:  "BLUE_MSG_CONSUMER_TO_SELF_FB_UNVERIFIED",
		91:  "BLUE_MSG_CONSUMER_TO_SELF_PREMISE_UNVERIFIED",
		92:  "BLUE_MSG_SELF_FB_TO_BSP_PREMISE",
		93:  "BLUE_MSG_SELF_FB_TO_SELF_PREMISE",
		94:  "BLUE_MSG_SELF_FB_UNVERIFIED",
		95:  "BLUE_MSG_SELF_FB_UNVERIFIED_TO_SELF_PREMISE_VERIFIED",
		96:  "BLUE_MSG_SELF_FB_VERIFIED",
		97:  "BLUE_MSG_SELF_FB_VERIFIED_TO_SELF_PREMISE_UNVERIFIED",
		98:  "BLUE_MSG_SELF_PREMISE_TO_BSP_PREMISE",
		99:  "BLUE_MSG_SELF_PREMISE_UNVERIFIED",
		100: "BLUE_MSG_SELF_PREMISE_VERIFIED",
		101: "BLUE_MSG_TO_BSP_FB",
		102: "BLUE_MSG_TO_CONSUMER",
		103: "BLUE_MSG_TO_SELF_FB",
		104: "BLUE_MSG_UNVERIFIED_TO_BSP_FB_VERIFIED",
		105: "BLUE_MSG_UNVERIFIED_TO_BSP_PREMISE_VERIFIED",
		106: "BLUE_MSG_UNVERIFIED_TO_SELF_FB_VERIFIED",
		107: "BLUE_MSG_UNVERIFIED_TO_VERIFIED",
		108: "BLUE_MSG_VERIFIED_TO_BSP_FB_UNVERIFIED",
		109: "BLUE_MSG_VERIFIED_TO_BSP_PREMISE_UNVERIFIED",
		110: "BLUE_MSG_VERIFIED_TO_SELF_FB_UNVERIFIED",
		111: "BLUE_MSG_VERIFIED_TO_UNVERIFIED",
		112: "BLUE_MSG_BSP_FB_UNVERIFIED_TO_BSP_PREMISE_VERIFIED",
		113: "BLUE_MSG_BSP_FB_UNVERIFIED_TO_SELF_FB_VERIFIED",
		114: "BLUE_MSG_BSP_FB_VERIFIED_TO_BSP_PREMISE_UNVERIFIED",
		115: "BLUE_MSG_BSP_FB_VERIFIED_TO_SELF_FB_UNVERIFIED",
		116: "BLUE_MSG_SELF_FB_UNVERIFIED_TO_BSP_PREMISE_VERIFIED",
		117: "BLUE_MSG_SELF_FB_VERIFIED_TO_BSP_PREMISE_UNVERIFIED",
		118: "E2E_IDENTITY_UNAVAILABLE",
		119: "GROUP_CREATING",
		120: "GROUP_CREATE_FAILED",
		121: "GROUP_BOUNCED",
		122: "BLOCK_CONTACT",
		123: "EPHEMERAL_SETTING_NOT_APPLIED",
		124: "SYNC_FAILED",
		125: "SYNCING",
		126: "BIZ_PRIVACY_MODE_INIT_FB",
		127: "BIZ_PRIVACY_MODE_INIT_BSP",
		128: "BIZ_PRIVACY_MODE_TO_FB",
		129: "BIZ_PRIVACY_MODE_TO_BSP",
		130: "DISAPPEARING_MODE",
		131: "E2E_DEVICE_FETCH_FAILED",
		132: "ADMIN_REVOKE",
		133: "GROUP_INVITE_LINK_GROWTH_LOCKED",
		134: "COMMUNITY_LINK_PARENT_GROUP",
		135: "COMMUNITY_LINK_SIBLING_GROUP",
		136: "COMMUNITY_LINK_SUB_GROUP",
		137: "COMMUNITY_UNLINK_PARENT_GROUP",
		138: "COMMUNITY_UNLINK_SIBLING_GROUP",
		139: "COMMUNITY_UNLINK_SUB_GROUP",
		140: "GROUP_PARTICIPANT_ACCEPT",
		141: "GROUP_PARTICIPANT_LINKED_GROUP_JOIN",
		142: "COMMUNITY_CREATE",
		143: "EPHEMERAL_KEEP_IN_CHAT",
		144: "GROUP_MEMBERSHIP_JOIN_APPROVAL_REQUEST",
		145: "GROUP_MEMBERSHIP_JOIN_APPROVAL_MODE",
		146: "INTEGRITY_UNLINK_PARENT_GROUP",
		147: "COMMUNITY_PARTICIPANT_PROMOTE",
		148: "COMMUNITY_PARTICIPANT_DEMOTE",
		149: "COMMUNITY_PARENT_GROUP_DELETED",
		150: "COMMUNITY_LINK_PARENT_GROUP_MEMBERSHIP_APPROVAL",
		151: "GROUP_PARTICIPANT_JOINED_GROUP_AND_PARENT_GROUP",
		152: "MASKED_THREAD_CREATED",
		153: "MASKED_THREAD_UNMASKED",
		154: "BIZ_CHAT_ASSIGNMENT",
		155: "CHAT_PSA",
		156: "CHAT_POLL_CREATION_MESSAGE",
		157: "CAG_MASKED_THREAD_CREATED",
		158: "COMMUNITY_PARENT_GROUP_SUBJECT_CHANGED",
		159: "CAG_INVITE_AUTO_ADD",
		160: "BIZ_CHAT_ASSIGNMENT_UNASSIGN",
		161: "CAG_INVITE_AUTO_JOINED",
		162: "SCHEDULED_CALL_START_MESSAGE",
		163: "COMMUNITY_INVITE_RICH",
		164: "COMMUNITY_INVITE_AUTO_ADD_RICH",
		165: "SUB_GROUP_INVITE_RICH",
		166: "SUB_GROUP_PARTICIPANT_ADD_RICH",
		167: "COMMUNITY_LINK_PARENT_GROUP_RICH",
		168: "COMMUNITY_PARTICIPANT_ADD_RICH",
		169: "SILENCED_UNKNOWN_CALLER_AUDIO",
		170: "SILENCED_UNKNOWN_CALLER_VIDEO",
		171: "GROUP_MEMBER_ADD_MODE",
		172: "GROUP_MEMBERSHIP_JOIN_APPROVAL_REQUEST_NON_ADMIN_ADD",
		173: "COMMUNITY_CHANGE_DESCRIPTION",
		174: "SENDER_INVITE",
		175: "RECEIVER_INVITE",
		176: "COMMUNITY_ALLOW_MEMBER_ADDED_GROUPS",
		177: "PINNED_MESSAGE_IN_CHAT",
		178: "PAYMENT_INVITE_SETUP_INVITER",
		179: "PAYMENT_INVITE_SETUP_INVITEE_RECEIVE_ONLY",
		180: "PAYMENT_INVITE_SETUP_INVITEE_SEND_AND_RECEIVE",
		181: "LINKED_GROUP_CALL_START",
		182: "REPORT_TO_ADMIN_ENABLED_STATUS",
		183: "EMPTY_SUBGROUP_CREATE",
		184: "SCHEDULED_CALL_CANCEL",
		185: "SUBGROUP_ADMIN_TRIGGERED_AUTO_ADD_RICH",
		186: "GROUP_CHANGE_RECENT_HISTORY_SHARING",
		187: "PAID_MESSAGE_SERVER_CAMPAIGN_ID",
		188: "GENERAL_CHAT_CREATE",
		189: "GENERAL_CHAT_ADD",
		190: "GENERAL_CHAT_AUTO_ADD_DISABLED",
		191: "SUGGESTED_SUBGROUP_ANNOUNCE",
		192: "BIZ_BOT_1P_MESSAGING_ENABLED",
		193: "CHANGE_USERNAME",
		194: "BIZ_COEX_PRIVACY_INIT_SELF",
		195: "BIZ_COEX_PRIVACY_TRANSITION_SELF",
		196: "SUPPORT_AI_EDUCATION",
		197: "BIZ_BOT_3P_MESSAGING_ENABLED",
		198: "REMINDER_SETUP_MESSAGE",
		199: "REMINDER_SENT_MESSAGE",
		200: "REMINDER_CANCEL_MESSAGE",
		201: "BIZ_COEX_PRIVACY_INIT",
		202: "BIZ_COEX_PRIVACY_TRANSITION",
		203: "GROUP_DEACTIVATED",
		204: "COMMUNITY_DEACTIVATE_SIBLING_GROUP",
		205: "EVENT_UPDATED",
		206: "EVENT_CANCELED",
		207: "COMMUNITY_OWNER_UPDATED",
		208: "COMMUNITY_SUB_GROUP_VISIBILITY_HIDDEN",
		209: "CAPI_GROUP_NE2EE_SYSTEM_MESSAGE",
		210: "STATUS_MENTION",
		211: "USER_CONTROLS_SYSTEM_MESSAGE",
		212: "SUPPORT_SYSTEM_MESSAGE",
		213: "CHANGE_LID",
		214: "BIZ_CUSTOMER_3PD_DATA_SHARING_OPT_IN_MESSAGE",
		215: "BIZ_CUSTOMER_3PD_DATA_SHARING_OPT_OUT_MESSAGE",
		216: "CHANGE_LIMIT_SHARING",
		217: "GROUP_MEMBER_LINK_MODE",
		218: "BIZ_AUTOMATICALLY_LABELED_CHAT_SYSTEM_MESSAGE",
		219: "PHONE_NUMBER_HIDING_CHAT_DEPRECATED_MESSAGE",
		220: "QUARANTINED_MESSAGE",
		221: "GROUP_MEMBER_SHARE_GROUP_HISTORY_MODE",
		222: "GROUP_OPEN_BOT_ADDED",
		223: "GROUP_TEE_BOT_ADDED",
		224: "CONTACT_INFO",
		225: "SCHEDULED_MESSAGE_CREATED",
		226: "IDENTITY_TRUST_MARKED",
		227: "IDENTITY_TRUST_UNMARKED",
		228: "IDENTITY_TRUST_REVOKED",
		230: "CTWA_CONSUMER_DISCLOSURE",
		240: "CHANGE_ACP2_SETTING",
		244: "BIZ_CALLBACK_DISABLED",
		247: "BIZ_CALLBACK_ENABLED",
		248: "EPHEMERAL_CHANGED_FOR_COEX",
		249: "UGC_BOT_PROFILE_UPDATED",
		250: "ORDER_EPHEMERAL_EXEMPTION",
		254: "CAMEO_CHAT_CREATED",
		255: "SENDER_SIDE_CONTACT_INFO",
		256: "CAMEO_TRANSITIONED",
	}
	expectedNotices := map[int32]bool{
		20:  true,
		21:  true,
		22:  true,
		23:  true,
		24:  true,
		25:  true,
		26:  true,
		27:  true,
		28:  true,
		29:  true,
		30:  true,
		31:  true,
		32:  true,
		33:  true,
		34:  true,
		35:  true,
		36:  true,
		38:  true,
		39:  true,
		40:  true,
		41:  true,
		45:  true,
		46:  true,
		69:  true,
		70:  true,
		71:  true,
		73:  true,
		75:  true,
		126: true,
		127: true,
		128: true,
		129: true,
		133: true,
		134: true,
		135: true,
		136: true,
		137: true,
		138: true,
		139: true,
		140: true,
		141: true,
		142: true,
		144: true,
		145: true,
		146: true,
		147: true,
		148: true,
		150: true,
		151: true,
		158: true,
		159: true,
		161: true,
		163: true,
		164: true,
		165: true,
		166: true,
		167: true,
		168: true,
		171: true,
		172: true,
		173: true,
		176: true,
		182: true,
		183: true,
		185: true,
		188: true,
		189: true,
		190: true,
		191: true,
		193: true,
		207: true,
		217: true,
	}
	if len(waWeb.WebMessageInfo_StubType_name) != len(expectedNames) {
		t.Fatal("pinned enum inventory changed; taxonomy review required")
	}
	for value, name := range expectedNames {
		t.Run(name, func(t *testing.T) {
			if waWeb.WebMessageInfo_StubType_name[value] != name {
				t.Fatal("pinned enum name changed")
			}
			kind := waWeb.WebMessageInfo_StubType(value)
			if benignHistoryStub(kind) != expectedNotices[value] {
				t.Fatal("notice classification differs from reviewed taxonomy")
			}
			if expectedNotices[value] || value == 1 || value == 2 || value == 132 {
				return
			}
			store := fixtureStore()
			r := testRuntime(store)
			notice := bodylessHistoryStub("300@g.us", "synthetic-denied-enum", kind)
			if err := r.projectHistory(context.Background(), bodylessHistoryWire(t, "300@g.us", notice)); err == nil || !r.Status().ReconciliationRequired {
				t.Fatal("non-notice enum bypassed strict bodyless handling")
			}
			if len(store.events) != 0 || len(store.aliases) != 0 {
				t.Fatal("non-notice enum fabricated source authority")
			}
		})
	}
}
