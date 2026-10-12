package ingestion

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
)

// A linked phone can supply authenticated history metadata without a body.
// Understood stubs do not authorize a fabricated original message.
func (r *Runtime) projectBodylessHistory(ctx context.Context, chat types.JID, web *waWeb.WebMessageInfo) error {
	if web == nil || web.GetMessage() != nil || web.GetKey() == nil || web.GetKey().GetID() == "" || len(web.GetKey().GetID()) > 256 || !utf8.ValidString(web.GetKey().GetID()) || strings.IndexByte(web.GetKey().GetID(), 0) >= 0 {
		if web.GetMessageStubType() == waWeb.WebMessageInfo_ADMIN_REVOKE {
			return historyAdminStage(errHistoryCorrupt, "admin_key_invalid")
		}
		return historyStage(errHistoryCorrupt, "record_structure", historyStubClass(web))
	}
	// Unresolved expiry or embedded control metadata must not disappear merely
	// because this record has no message body.
	if web.EphemeralStartTimestamp != nil || web.EphemeralDuration != nil || web.EphemeralExpirationTimestamp != nil || web.EphemeralOffToOn != nil || web.EphemeralOutOfSync != nil || web.KeepInChat != nil || len(web.FutureproofData) != 0 || web.TargetMessageID != nil || len(web.MessageAddOns) != 0 || web.QuarantinedMessage != nil || web.GroupHistoryBundleInfo != nil {
		return historyBodylessFailure(web, "record_safety_metadata")
	}
	kind := web.GetMessageStubType()
	if kind != waWeb.WebMessageInfo_REVOKE && kind != waWeb.WebMessageInfo_ADMIN_REVOKE && kind != waWeb.WebMessageInfo_CIPHERTEXT && !benignHistoryStub(kind) {
		return historyBodyMissing(web)
	}
	canonicalChat, err := r.canonical(ctx, chat.ToNonAD().String())
	if err != nil {
		if kind == waWeb.WebMessageInfo_ADMIN_REVOKE {
			return historyAdminStage(err, "admin_chat_invalid")
		}
		return historyStage(err, "record_jid_mismatch", historyStubClass(web))
	}
	if kind == waWeb.WebMessageInfo_ADMIN_REVOKE {
		return r.projectBodylessAdminRevoke(ctx, chat, canonicalChat, web)
	}
	keyParticipant, webParticipant := web.GetKey().GetParticipant(), web.GetParticipant()
	var author string
	for _, claim := range []string{keyParticipant, webParticipant} {
		if claim == "" {
			continue
		}
		canonical, err := r.canonical(ctx, claim)
		if err != nil || (author != "" && author != canonical) {
			return historyStage(errHistoryCorrupt, "record_structure", historyStubClass(web))
		}
		author = canonical
	}
	// Conversation notices have no original body or author authority to apply.
	// Validate present claims, then skip without inventing a source or cursor.
	if benignHistoryStub(kind) {
		return nil
	}
	var targets []archive.SourceMetadata
	if web.GetMessageStubType() == waWeb.WebMessageInfo_REVOKE {
		targets, err = r.store.MutationTargets(ctx, r.SourceAccount(), canonicalChat, web.GetKey().GetID())
		if err != nil {
			return historyStage(err, "record_projection", "revoke")
		}
		if len(targets) > 1 {
			return historyStage(errHistoryCorrupt, "record_structure", "revoke")
		}
	}
	var info types.MessageInfo
	if chat.Server == types.GroupServer && !web.GetKey().GetFromMe() && author == "" {
		// A unique retained target can establish the original author. The actor
		// who performed a group deletion cannot establish that author's identity.
		if len(targets) != 1 || targets[0].Key.FromMe {
			return historyStage(errHistoryCorrupt, "record_structure", historyStubClass(web))
		}
		sender, err := types.ParseJID(targets[0].Key.Sender)
		if err != nil {
			return historyStage(errHistoryCorrupt, "record_structure", "revoke")
		}
		info = types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: true}, ID: web.GetKey().GetID()}
	} else {
		event, err := r.client.ParseWebMessage(chat, web)
		if err != nil {
			return historyStage(errHistoryCorrupt, "record_parse", historyStubClass(web))
		}
		info = event.Info
	}
	key, err := r.envelopeKey(ctx, info)
	if err != nil {
		return historyStage(err, "record_structure", historyStubClass(web))
	}
	if key.Chat != canonicalChat || (author != "" && author != key.Sender) {
		return historyStage(errHistoryCorrupt, "record_structure", historyStubClass(web))
	}
	switch web.GetMessageStubType() {
	case waWeb.WebMessageInfo_REVOKE:
		deletion := archive.SourceEvent{Key: key, Kind: archive.EventDelete, SourceRevision: 0, BodyKind: archive.MessageUnsupported}
		if len(targets) == 1 {
			target := targets[0]
			if target.Key.Sender != key.Sender || target.Key.FromMe != key.FromMe {
				return historyStage(errHistoryCorrupt, "record_structure", "revoke")
			}
			deletion.OriginalSentAt = target.OriginalSentAt
			deletion.OriginalDateKnown = target.OriginalDateKnown
			deletion.ExpiresAt = target.Version.ExpiresAt
		}
		// Deletion is terminal even without an authenticated operation time.
		// The stub's receive timestamp must not replace the original's date.
		_, err = r.apply(ctx, deletion)
		return historyStage(err, "record_projection", "revoke")
	case waWeb.WebMessageInfo_CIPHERTEXT:
		if err = r.store.SetSyncStatus(ctx, archive.SyncUpdate{Account: key.Account, GapCode: "undecryptable_message", CoverageUnknown: true}); err != nil {
			return historyStage(err, "record_projection", "ciphertext")
		}
		r.mu.Lock()
		found := false
		for _, code := range r.status.Codes {
			found = found || code == "undecryptable_message"
		}
		if !found {
			r.status.Codes = append(r.status.Codes, "undecryptable_message")
		}
		r.mu.Unlock()
		return nil
	default:
		return historyBodyMissing(web)
	}
}

func (r *Runtime) projectBodylessAdminRevoke(ctx context.Context, chat types.JID, canonicalChat string, web *waWeb.WebMessageInfo) error {
	if chat.Server != types.GroupServer {
		return historyAdminStage(errHistoryCorrupt, "admin_chat_invalid")
	}
	// The WebMessageInfo participant can be the administrator who performed
	// the revoke. Only MessageKey.Participant identifies the original author.
	if actor := web.GetParticipant(); actor != "" {
		if _, err := r.historyUserClaim(ctx, actor); err != nil {
			return historyAdminStage(err, "admin_actor_invalid")
		}
	}
	var author string
	var err error
	if claim := web.GetKey().GetParticipant(); claim != "" {
		author, err = r.historyUserClaim(ctx, claim)
		if err != nil {
			return historyAdminStage(err, "admin_author_invalid")
		}
	}
	fromMe := web.GetKey().GetFromMe()
	own, err := r.canonical(ctx, r.SourceAccount())
	if err != nil {
		return historyAdminStage(err, "admin_author_invalid")
	}
	if claim := web.GetOriginalSelfAuthorUserJIDString(); claim != "" {
		self, err := r.historyUserClaim(ctx, claim)
		if err != nil {
			return historyAdminStage(err, "admin_author_invalid")
		}
		if !fromMe {
			return historyAdminStage(errHistoryCorrupt, "admin_direction_mismatch")
		}
		if self != own {
			return historyAdminStage(errHistoryCorrupt, "admin_author_mismatch")
		}
	}
	targets, err := r.store.MutationTargets(ctx, r.SourceAccount(), canonicalChat, web.GetKey().GetID())
	if err != nil {
		if errors.Is(err, archive.ErrSourceUnavailable) {
			return historyAdminStage(err, "admin_target_ambiguous")
		}
		return historyAdminStage(err, "admin_store_failure")
	}
	if len(targets) > 1 {
		return historyAdminStage(errHistoryCorrupt, "admin_target_ambiguous")
	}
	deletion := archive.SourceEvent{Kind: archive.EventDelete, SourceRevision: 0, BodyKind: archive.MessageUnsupported}
	if len(targets) == 1 {
		target := targets[0]
		if target.Key.Account != r.SourceAccount() || target.Key.Chat != canonicalChat || target.Key.Message != web.GetKey().GetID() {
			return historyAdminStage(errHistoryCorrupt, "admin_envelope_invalid")
		}
		if target.Key.FromMe != fromMe {
			return historyAdminStage(errHistoryCorrupt, "admin_direction_mismatch")
		}
		if author != "" && author != target.Key.Sender {
			return historyAdminStage(errHistoryCorrupt, "admin_author_mismatch")
		}
		deletion.Key = target.Key
		deletion.OriginalSentAt = target.OriginalSentAt
		deletion.OriginalDateKnown = target.OriginalDateKnown
		deletion.ExpiresAt = target.Version.ExpiresAt
	} else {
		if fromMe {
			if author != "" && author != own {
				return historyAdminStage(errHistoryCorrupt, "admin_author_mismatch")
			}
			author = own
		} else if author == "" {
			// An actor cannot supply the original author. Durably withhold this
			// message ID across all source identities before processing the tail.
			if err := r.store.RecordUnresolvedDeletion(ctx, r.SourceAccount(), canonicalChat, web.GetKey().GetID()); err != nil {
				return historyAdminStage(err, "admin_store_failure")
			}
			r.mu.Lock()
			found := false
			for _, code := range r.status.Codes {
				found = found || code == "unresolved_deletion"
			}
			if !found {
				r.status.Codes = append(r.status.Codes, "unresolved_deletion")
			}
			r.mu.Unlock()
			return nil
		}
		deletion.Key = archive.SourceKey{Account: r.SourceAccount(), Chat: canonicalChat, Sender: author, Message: web.GetKey().GetID(), FromMe: fromMe}
	}
	sender, err := r.historyUserClaim(ctx, deletion.Key.Sender)
	if err != nil {
		return historyAdminStage(err, "admin_author_invalid")
	}
	if sender != deletion.Key.Sender {
		return historyAdminStage(errHistoryCorrupt, "admin_author_mismatch")
	}
	if (sender == own) != fromMe {
		return historyAdminStage(errHistoryCorrupt, "admin_direction_mismatch")
	}
	senderJID, err := types.ParseJID(sender)
	if err != nil {
		return historyAdminStage(errHistoryCorrupt, "admin_author_invalid")
	}
	validated, err := r.envelopeKey(ctx, types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: senderJID, IsGroup: true, IsFromMe: fromMe}, ID: web.GetKey().GetID()})
	if err != nil {
		return historyAdminStage(err, "admin_envelope_invalid")
	}
	if validated != deletion.Key {
		return historyAdminStage(errHistoryCorrupt, "admin_envelope_invalid")
	}
	// Zero revision is a terminal source tombstone, not a guessed operation time.
	_, err = r.apply(ctx, deletion)
	return historyAdminStage(err, "admin_store_failure")
}

func historyAdminStage(err error, stage string) error {
	if err == nil {
		return nil
	}
	return &historyStageError{stage: closedHistoryStage(stage), stub: "admin_revoke", stubType: int32(waWeb.WebMessageInfo_ADMIN_REVOKE), cause: err}
}

func (r *Runtime) historyUserClaim(ctx context.Context, claim string) (string, error) {
	canonical, err := r.canonical(ctx, claim)
	if err != nil {
		return "", err
	}
	jid, err := types.ParseJID(canonical)
	if err != nil || jid.User == "" || (jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer) {
		return "", archive.ErrInvalidEvent
	}
	return canonical, nil
}

func benignHistoryStub(kind waWeb.WebMessageInfo_StubType) bool {
	switch kind {
	case waWeb.WebMessageInfo_GROUP_CREATE,
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
		waWeb.WebMessageInfo_GROUP_PARTICIPANT_CHANGE_NUMBER,
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
		waWeb.WebMessageInfo_GROUP_PARTICIPANT_LINKED_GROUP_JOIN,
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
		waWeb.WebMessageInfo_CHANGE_USERNAME,
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
		waWeb.WebMessageInfo_CALL_MISSED_GROUP_VIDEO:
		return true
	default:
		return false
	}
}
