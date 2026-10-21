package ingestion

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func (r *Runtime) ensureAccount(ctx context.Context) error {
	if r.client == nil || r.client.Store.ID == nil {
		return archive.ErrInvalidEvent
	}
	account := r.client.Store.ID.ToNonAD().String()
	r.mu.Lock()
	old := r.account
	r.mu.Unlock()
	if old != "" && old != account {
		return archive.ErrUnauthorized
	}
	if _, err := r.store.SourcePolicy(ctx, account); err != nil {
		return sourceOperationFailure(ctx, "source_policy", err)
	}
	r.mu.Lock()
	r.account = account
	r.mu.Unlock()
	if !r.client.Store.LID.IsEmpty() {
		return r.aliasAt(ctx, "source_session_alias", account, r.client.Store.LID.ToNonAD().String(), archive.AliasSessionMetadata)
	}
	return nil
}
func (r *Runtime) canonical(ctx context.Context, id string) (string, error) {
	if id == "" || len(id) > 256 {
		return "", archive.ErrInvalidEvent
	}
	jid, err := types.ParseJID(id)
	if err != nil || jid.User == "" || jid.Server == "" {
		return "", archive.ErrInvalidEvent
	}
	canonical, err := r.store.Canonical(ctx, r.SourceAccount(), jid.ToNonAD().String())
	return canonical, sourceOperationFailure(ctx, "source_canonical", err)
}
func (r *Runtime) envelopeKey(ctx context.Context, info types.MessageInfo) (archive.SourceKey, error) {
	var key archive.SourceKey
	if err := r.ensureAccount(ctx); err != nil {
		return key, err
	}
	if info.ID == "" || len(info.ID) > 256 {
		return key, archive.ErrInvalidEvent
	}
	chat, err := r.canonical(ctx, info.Chat.ToNonAD().String())
	if err != nil {
		return key, err
	}
	sender, err := r.canonical(ctx, info.Sender.ToNonAD().String())
	if err != nil {
		return key, err
	}
	if info.IsFromMe {
		own, err := r.canonical(ctx, r.SourceAccount())
		if err != nil {
			return key, err
		}
		if sender != own {
			return key, archive.ErrInvalidEvent
		}
	}
	return archive.SourceKey{Account: r.SourceAccount(), Chat: chat, Sender: sender, Message: info.ID, FromMe: info.IsFromMe}, nil
}
func rawProtocolStatus(m *waE2E.Message) (*waE2E.ProtocolMessage, error) {
	for depth := 0; depth < 32; depth++ {
		if m == nil {
			return nil, nil
		}
		if m.GetDocumentMessage() != nil && !standaloneDocument(m) {
			return nil, archive.ErrInvalidEvent
		}
		var next *waE2E.Message
		for _, child := range []*waE2E.Message{m.GetDeviceSentMessage().GetMessage(), m.GetBotInvokeMessage().GetMessage(), m.GetEphemeralMessage().GetMessage(), m.GetViewOnceMessage().GetMessage(), m.GetViewOnceMessageV2().GetMessage(), m.GetViewOnceMessageV2Extension().GetMessage(), m.GetLottieStickerMessage().GetMessage(), m.GetDocumentWithCaptionMessage().GetMessage(), m.GetEditedMessage().GetMessage()} {
			if child == nil {
				continue
			}
			if next != nil {
				return nil, archive.ErrInvalidEvent
			}
			next = child
		}
		if p := m.GetProtocolMessage(); p != nil {
			if next != nil {
				return nil, archive.ErrInvalidEvent
			}
			return p, nil
		}
		if next == nil {
			return nil, nil
		}
		m = next
	}
	return nil, archive.ErrInvalidEvent
}
func rawProtocol(m *waE2E.Message) *waE2E.ProtocolMessage { p, _ := rawProtocolStatus(m); return p }
func validateMessageStructure(e *events.Message) error {
	for _, m := range []*waE2E.Message{e.RawMessage, e.SourceWebMsg.GetMessage(), e.Message} {
		if _, err := rawProtocolStatus(m); err != nil {
			return err
		}
	}
	return nil
}
func mutationProtocol(e *events.Message) *waE2E.ProtocolMessage {
	if e == nil {
		return nil
	}
	p := rawProtocol(e.RawMessage)
	if p == nil {
		p = rawProtocol(e.SourceWebMsg.GetMessage())
	}
	if p == nil {
		p = rawProtocol(e.Message)
	}
	return p
}
func standaloneSenderKeyDistributionEnvelope(event *events.Message) bool {
	return event != nil && !event.IsEdit && !event.IsViewOnce && event.UnavailableRequestID == "" && event.SourceWebMsg == nil &&
		(event.NewsletterMeta == nil || event.NewsletterMeta.EditTS.IsZero()) &&
		standaloneSenderKeyDistribution(event.Message) && (event.RawMessage == nil || standaloneSenderKeyDistribution(event.RawMessage))
}
func isMutation(e *events.Message) bool {
	if e == nil || e.Message == nil || standaloneSenderKeyDistributionEnvelope(e) {
		return false
	}
	p := mutationProtocol(e)
	newsletterEdit := e.NewsletterMeta != nil && !e.NewsletterMeta.EditTS.IsZero()
	return newsletterEdit || e.IsEdit || (e.Info.IsGroup && (e.Info.Edit == types.EditAttributeAdminRevoke || e.Info.Edit == types.EditAttributeAdminEdit)) || (p != nil && (p.GetType() == waE2E.ProtocolMessage_REVOKE || p.GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT))
}
func (r *Runtime) trustedMutation(ctx context.Context, e *events.Message) (archive.SourceEvent, *waE2E.Message, error) {
	var out archive.SourceEvent
	if err := validateMessageStructure(e); err != nil {
		r.failClosed("ambiguous_message_structure")
		return out, nil, err
	}
	if !isMutation(e) {
		return out, nil, archive.ErrInvalidEvent
	}
	key, err := r.envelopeKey(ctx, e.Info)
	if err != nil {
		return out, nil, err
	}
	out = archive.SourceEvent{Key: key, Kind: archive.EventEditInvalidation, SourceRevision: e.Info.Timestamp.UnixMilli(), BodyKind: archive.MessageUnsupported}
	body := e.Message
	p := mutationProtocol(e)
	if e.NewsletterMeta != nil && !e.NewsletterMeta.EditTS.IsZero() {
		out.SourceRevision = e.NewsletterMeta.EditTS.UnixMilli()
		out.OriginalSentAt = e.NewsletterMeta.OriginalTS
		out.OriginalDateKnown = !out.OriginalSentAt.IsZero()
	} else if e.IsEdit && p == nil {
		r.mutationMetadataMissing("edit_protocol_missing", e)
		return out, nil, archive.ErrMutationMetadataMissing
	}
	if e.Info.IsGroup && (e.Info.Edit == types.EditAttributeAdminRevoke || e.Info.Edit == types.EditAttributeAdminEdit) {
		return r.trustedAdminRevoke(ctx, e, key)
	}
	if p != nil && (p.GetType() == waE2E.ProtocolMessage_REVOKE || p.GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT) {
		target := p.GetKey()
		if target.GetID() == "" || len(target.GetID()) > 256 {
			return out, nil, archive.ErrInvalidEvent
		}
		out.Key.Message = target.GetID()
		for _, claim := range []struct{ value, want string }{{target.GetRemoteJID(), key.Chat}, {target.GetParticipant(), key.Sender}} {
			if claim.value == "" {
				continue
			}
			canonical, err := r.canonical(ctx, claim.value)
			if err != nil {
				return out, nil, err
			}
			if canonical != claim.want {
				return out, nil, archive.ErrInvalidEvent
			}
		}
		// Payload FromMe is relative to its author. Only the authenticated envelope
		// determines local direction, chat, and sender.
		if p.GetType() == waE2E.ProtocolMessage_REVOKE {
			out.Kind = archive.EventDelete
			body = nil
		} else {
			body = p.GetEditedMessage()
			if p.GetTimestampMS() > 0 {
				out.SourceRevision = p.GetTimestampMS()
			}
		}
	}
	targets, err := r.store.MutationTargets(ctx, key.Account, key.Chat, out.Key.Message)
	if err != nil {
		return out, nil, err
	}
	matched := false
	for _, target := range targets {
		if target.Key.Sender == key.Sender && target.Key.FromMe == key.FromMe {
			matched = true
			out.OriginalSentAt = target.OriginalSentAt
			out.OriginalDateKnown = target.OriginalDateKnown
			out.ExpiresAt = target.Version.ExpiresAt
			break
		}
	}
	if len(targets) > 0 && !matched {
		return out, nil, archive.ErrInvalidEvent
	}
	bodyProtocol, structureErr := rawProtocolStatus(body)
	if structureErr != nil {
		r.failClosed("ambiguous_edit_structure")
		return out, nil, structureErr
	}
	if !out.OriginalDateKnown || bodyProtocol != nil {
		body = nil
	}
	return out, body, nil
}
func (r *Runtime) applyInvalidation(ctx context.Context, event any) (bool, error) {
	switch e := event.(type) {
	case *events.Message:
		if !isMutation(e) {
			return false, nil
		}
		out, _, err := r.trustedMutation(ctx, e)
		if err != nil {
			return true, err
		}
		_, err = r.apply(ctx, out)
		return true, err
	case *events.DeleteForMe:
		if err := r.ensureAccount(ctx); err != nil {
			return true, err
		}
		sender := e.SenderJID
		if e.IsFromMe {
			sender = *r.client.Store.ID
		} else if sender.IsEmpty() {
			if e.ChatJID.Server == types.GroupServer {
				r.mutationMetadataMissing("delete_for_me_author_missing", &events.Message{Info: types.MessageInfo{ID: e.MessageID, Timestamp: e.Timestamp, MessageSource: types.MessageSource{Chat: e.ChatJID, Sender: e.SenderJID, IsFromMe: e.IsFromMe, IsGroup: true}}})
				return true, archive.ErrMutationMetadataMissing
			}
			sender = e.ChatJID
		}
		key, err := r.envelopeKey(ctx, types.MessageInfo{ID: e.MessageID, MessageSource: types.MessageSource{Chat: e.ChatJID, Sender: sender, IsFromMe: e.IsFromMe}})
		if err != nil {
			return true, err
		}
		out := archive.SourceEvent{Key: key, Kind: archive.EventDelete, SourceRevision: e.Timestamp.UnixMilli(), BodyKind: archive.MessageUnsupported}
		_, err = r.apply(ctx, out)
		return true, err
	case *events.ClearChat:
		if err := r.ensureAccount(ctx); err != nil {
			return true, err
		}
		chat, err := r.canonical(ctx, e.JID.ToNonAD().String())
		if err != nil {
			return true, err
		}
		return true, r.store.ClearChat(ctx, r.SourceAccount(), chat, e.Timestamp)
	case *events.DeleteChat:
		if err := r.ensureAccount(ctx); err != nil {
			return true, err
		}
		chat, err := r.canonical(ctx, e.JID.ToNonAD().String())
		if err != nil {
			return true, err
		}
		return true, r.store.ClearChat(ctx, r.SourceAccount(), chat, e.Timestamp)
	}
	return false, nil
}
func policyPermits(p archive.IngestionPolicy, e archive.SourceEvent, now time.Time) bool {
	if !e.OriginalDateKnown || e.OriginalSentAt.IsZero() {
		return false
	}
	if p.DateMode == archive.DateSince && e.OriginalSentAt.Before(p.ImportStart) {
		return false
	}
	if p.DateMode != archive.DateSince && p.DateMode != archive.DateAllAvailable {
		return false
	}
	if p.RollingRetention > 0 && e.OriginalSentAt.Before(now.Add(-p.RollingRetention)) {
		return false
	}
	if !e.ExpiresAt.IsZero() && !e.ExpiresAt.After(now) {
		return false
	}
	if p.ChatMode == archive.ChatsAll {
		return true
	}
	if p.ChatMode != archive.ChatsSelective {
		return false
	}
	for _, chat := range p.IncludedSourceChats {
		if chat == e.Key.Chat {
			return true
		}
	}
	return false
}
func (r *Runtime) apply(ctx context.Context, e archive.SourceEvent) (archive.ApplyResult, error) {
	if e.DeliveredHistory != nil {
		// Authenticated chunk admission is immutable. Never refresh a rejected
		// positive's policy through the ordinary live retry loop.
		result, err := r.store.ApplySourceEvent(ctx, e, e.DeliveredHistory.PolicyGeneration)
		if err == archive.ErrPolicyChanged {
			return archive.ApplyResult{Outcome: archive.OutcomeFiltered}, nil
		}
		return result, sourceOperationFailure(ctx, "source_apply", err)
	}
	for tries := 0; tries < 2; tries++ {
		policy, err := r.store.SourcePolicy(ctx, e.Key.Account)
		if err != nil {
			return archive.ApplyResult{}, sourceOperationFailure(ctx, "source_policy", err)
		}
		candidate := e
		if e.Kind == archive.EventOriginal || e.Kind == archive.EventEditContent {
			if !policyPermits(policy, e, time.Now()) {
				candidate.Body = ""
				candidate.Caption = ""
				candidate.Media = nil
				candidate.ReplyTo = nil
			}
		}
		result, err := r.store.ApplySourceEvent(ctx, candidate, policy.Generation)
		if !errors.Is(err, archive.ErrPolicyChanged) {
			return result, sourceOperationFailure(ctx, "source_apply", err)
		}
	}
	return archive.ApplyResult{}, sourceOperationFailure(ctx, "source_apply", archive.ErrPolicyChanged)
}
func (r *Runtime) alias(ctx context.Context, a, b string, provenance archive.AliasProvenance) error {
	return r.aliasAt(ctx, "source_alias", a, b, provenance)
}
func (r *Runtime) aliasAt(ctx context.Context, stage, a, b string, provenance archive.AliasProvenance) error {
	if a == "" || b == "" {
		return nil
	}
	ja, ea := types.ParseJID(a)
	jb, eb := types.ParseJID(b)
	if ea != nil || eb != nil {
		return archive.ErrInvalidEvent
	}
	ja = ja.ToNonAD()
	jb = jb.ToNonAD()
	if ja.Server == types.HiddenUserServer && jb.Server == types.DefaultUserServer {
		ja, jb = jb, ja
	}
	if ja.Server != types.DefaultUserServer || jb.Server != types.HiddenUserServer {
		return archive.ErrInvalidEvent
	}
	err := r.store.ApplyAlias(ctx, archive.SourceAlias{Account: r.SourceAccount(), PhoneJID: ja.String(), LID: jb.String(), Provenance: provenance})
	return sourceOperationFailure(ctx, stage, err)
}
func (r *Runtime) message(ctx context.Context, event *events.Message) error {
	return r.messageWithDeliveredAdmission(ctx, event, nil)
}
func (r *Runtime) messageWithDeliveredAdmission(ctx context.Context, event *events.Message, delivered *archive.DeliveredHistoryAdmission) error {
	if r.Status().ReconciliationRequired {
		return archive.ErrReconciliationRequired
	}
	if handled, err := r.ownerVoiceDescriptorReply(ctx, event); handled || err != nil {
		return err
	}
	if event != nil && event.Message == nil && event.SourceWebMsg != nil && event.UnavailableRequestID != "" {
		if !recordWithinLimit(event) {
			r.failClosed("history_limit_exceeded")
			return errLimitExceeded
		}
		if err := r.ensureAccount(ctx); err != nil {
			return err
		}
		chat, err := r.canonical(ctx, event.Info.Chat.ToNonAD().String())
		if err != nil {
			return err
		}
		claimed, err := r.canonical(ctx, event.SourceWebMsg.GetKey().GetRemoteJID())
		if err != nil || chat != claimed {
			return archive.ErrInvalidEvent
		}
		return r.projectBodylessHistory(ctx, event.Info.Chat, event.SourceWebMsg)
	}
	if event == nil || event.Message == nil {
		return archive.ErrInvalidEvent
	}
	if r.Status().ReconciliationRequired {
		return archive.ErrReconciliationRequired
	}
	if err := validateMessageStructure(event); err != nil {
		r.failClosed("ambiguous_message_structure")
		return err
	}
	if event.Message.GetProtocolMessage().GetHistorySyncNotification() != nil {
		return r.historyFrom(ctx, event)
	}
	if !recordWithinLimit(event) {
		r.failClosed("history_limit_exceeded")
		return errLimitExceeded
	}
	var e archive.SourceEvent
	var body *waE2E.Message
	var err error
	if isMutation(event) {
		e, body, err = r.trustedMutation(ctx, event)
		if err != nil {
			return err
		}
		if delivered != nil && e.Kind != archive.EventDelete {
			if err = r.sourceExpiry(ctx, &e, event); err != nil {
				return err
			}
			if err = r.recordRebuildRestriction(ctx, &e, event); err != nil {
				return err
			}
		}
		if _, err = r.apply(ctx, e); err != nil {
			if err != archive.ErrSourceRevisionConflict {
				r.failClosed("archive_unavailable")
			}
			return err
		}
		if e.Kind == archive.EventDelete || body == nil {
			return nil
		}
		e.Kind = archive.EventEditContent
	} else {
		if err = r.ensureAccount(ctx); err != nil {
			return err
		}
		if !event.Info.SenderAlt.IsEmpty() {
			if err = r.aliasAt(ctx, "source_sender_alias", event.Info.Sender.ToNonAD().String(), event.Info.SenderAlt.ToNonAD().String(), archive.AliasConnectorMapping); err != nil {
				return err
			}
		}
		if !event.Info.IsGroup && !event.Info.RecipientAlt.IsEmpty() {
			if err = r.aliasAt(ctx, "source_recipient_alias", event.Info.Chat.ToNonAD().String(), event.Info.RecipientAlt.ToNonAD().String(), archive.AliasConnectorMapping); err != nil {
				return err
			}
		}
		key, err := r.envelopeKey(ctx, event.Info)
		if err != nil {
			return err
		}
		if err = r.sourceNames(ctx, key, event); err != nil {
			return sourceOperationFailure(ctx, "source_names", err)
		}
		e = archive.SourceEvent{Key: key, Kind: archive.EventOriginal, OriginalSentAt: event.Info.Timestamp, OriginalDateKnown: !event.Info.Timestamp.IsZero(), SourceRevision: event.Info.Timestamp.UnixMilli(), BodyKind: archive.MessageUnsupported}
		body = event.Message
		if standaloneSenderKeyDistributionEnvelope(event) {
			return nil
		}
	}
	if err = r.sourceExpiry(ctx, &e, event); err != nil {
		return sourceOperationFailure(ctx, "source_expiry", err)
	}
	policy, err := r.store.SourcePolicy(ctx, e.Key.Account)
	if err != nil {
		return sourceOperationFailure(ctx, "source_policy", err)
	}
	var allowed bool
	if delivered != nil {
		allowed, err = r.deliveredAdmission(ctx, &e, event, delivered)
	} else {
		allowed, err = r.rebuildAdmission(ctx, &e, event, policy)
	}
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	var pending *pendingMedia
	if (delivered == nil || policy.Generation == delivered.PolicyGeneration) && policyPermits(policy, e, time.Now()) {
		if event.IsViewOnce {
			r.projectViewOnce(&e, body)
		} else {
			pending, err = r.projectBody(ctx, &e, body)
			if err != nil {
				return sourceOperationFailure(ctx, "source_body", err)
			}
		}
	}
	if !utf8.ValidString(e.Body) || !utf8.ValidString(e.Caption) {
		return archive.ErrInvalidEvent
	}
	commitStarted := time.Now()
	result, err := r.apply(ctx, e)
	trace, traced := ctx.Value(sourceLatencyContextKey{}).(sourceLatencyTrace)
	if traced {
		r.latency.record(trace.lane, latencyCommit, time.Since(commitStarted))
	}
	if err != nil {
		return err
	}
	if traced && !trace.receivedAt.IsZero() && e.Kind == archive.EventOriginal && e.BodyKind == archive.MessageText && e.Body != "" && result.Outcome == archive.OutcomeApplied {
		r.latency.record(trace.lane, latencySearchReady, time.Since(trace.receivedAt))
	}
	r.mu.Lock()
	r.status.Received++
	if result.Outcome == archive.OutcomeApplied {
		r.status.Applied++
	}
	r.mu.Unlock()
	if err = r.store.SetSyncStatus(ctx, archive.SyncUpdate{Account: e.Key.Account, Chat: e.Key.Chat, ObservedOldest: e.OriginalSentAt, ObservedLatest: e.OriginalSentAt, Received: 1, Applied: boolCount(result.Outcome == archive.OutcomeApplied), CoverageUnknown: true}); err != nil {
		return sourceOperationFailure(ctx, "source_sync", err)
	}
	if pending != nil && result.Outcome == archive.OutcomeApplied && (r.callbacks.MediaAdmitter != nil || r.legacyMediaReceiver && r.callbacks.MediaReceiver != nil) {
		defer wipePending(pending)
		if r.callbacks.MediaAdmitter != nil {
			if len(result.MediaIDs) != 1 || result.MediaIDs[0] == "" {
				return ErrMediaInvalidAdmission
			}
			// The source transaction enqueued exact durable association work.
			// Live and imported-history projection only nudge its single loop.
			r.WakeOriginalDownloads()
			return nil
		}
		pending.key = e.Key
		pending.version = result.Version
		return r.downloadMedia(ctx, pending)
	}
	return nil
}
func boolCount(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
