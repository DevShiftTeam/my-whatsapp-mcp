package ingestion

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/types"
)

type ownerVoiceRedeliveryStore interface {
	VoiceRedeliverySources(context.Context, archive.ReadScope, int) ([]archive.VoiceRedeliverySource, error)
	CurrentSource(context.Context, archive.SourceKey) (archive.SourceRef, error)
}

// RequestVoiceRedelivery asks the saved account's primary phone for up to eight
// exact, current voice-message copies after an explicit owner request. An SDK
// acknowledgement is only a request; normal authenticated Message events remain
// responsible for source acceptance and attachment references.
func (r *Runtime) RequestVoiceRedelivery(ctx context.Context, scope archive.ReadScope) (int, error) {
	if ctx == nil || scope.Principal != "owner" || scope.Account != r.SourceAccount() || scope.PolicyGeneration == 0 {
		return 0, ErrMediaInvalidAdmission
	}
	if err := ctx.Err(); err != nil {
		return 0, mediaFailure(err, ErrMediaCanceled)
	}
	r.op.Lock()
	defer r.op.Unlock()
	r.mu.Lock()
	ready := !r.closed && r.status.Connection == "connected" && !r.status.Paused && !r.status.ReconciliationRequired && r.admissions == 0
	runCtx := r.runCtx
	r.mu.Unlock()
	if !ready || runCtx == nil {
		return 0, ErrMediaCanceled
	}
	if r.client.Store.ID == nil || r.client.Store.ID.ToNonAD().String() != scope.Account {
		return 0, ErrMediaAuthentication
	}
	if r.sendHistoryRequest == nil && (!r.client.IsConnected() || !r.client.IsLoggedIn()) {
		return 0, ErrMediaCanceled
	}
	store, ok := r.store.(ownerVoiceRedeliveryStore)
	if !ok {
		return 0, ErrMediaUnavailable
	}
	batch, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(runCtx, cancel)
	defer stop()
	items, err := store.VoiceRedeliverySources(batch, scope, 8)
	if err != nil {
		return 0, mediaFailure(err, ErrMediaUnavailable)
	}
	if len(items) > 8 {
		return 0, ErrMediaLimit
	}
	if len(items) == 0 {
		return 0, nil
	}
	r.optionalMu.Lock()
	limited := !r.lastBackfillExplicit.IsZero() && time.Since(r.lastBackfillExplicit) < optionalCooldown
	r.optionalMu.Unlock()
	if limited {
		return 0, ErrMediaLimit
	}
	requested := 0
	authorized := false
	for i, item := range items {
		if batch.Err() != nil {
			return requested, mediaFailure(batch.Err(), ErrMediaDeadline)
		}
		for _, previous := range items[:i] {
			if previous.Key == item.Key {
				return requested, ErrMediaInvalidAdmission
			}
		}
		chat, sender, err := r.voiceRedeliveryJIDs(batch, scope, store, item)
		if err != nil {
			continue
		}
		request := r.client.BuildUnavailableMessageRequest(chat, sender, item.Key.Message)
		keys := request.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetPlaceholderMessageResendRequest()
		if len(keys) != 1 || keys[0].GetMessageKey().GetFromMe() != item.Key.FromMe {
			continue
		}
		if !r.optionalReady() {
			return requested, ErrMediaCanceled
		}
		if !authorized {
			r.optionalMu.Lock()
			if !r.lastBackfillExplicit.IsZero() && time.Since(r.lastBackfillExplicit) < optionalCooldown {
				r.optionalMu.Unlock()
				return requested, ErrMediaLimit
			}
			r.lastBackfillExplicit = time.Now()
			r.optionalMu.Unlock()
			authorized = true
		}
		attempt, done := context.WithTimeout(batch, 2*time.Second)
		if r.sendHistoryRequest != nil {
			err = r.sendHistoryRequest(attempt, request)
		} else {
			_, err = r.client.SendPeerMessage(attempt, request)
		}
		done()
		if err != nil {
			return requested, mediaFailure(err, ErrMediaUnavailable)
		}
		requested++
	}
	return requested, nil
}

func (r *Runtime) voiceRedeliveryJIDs(ctx context.Context, scope archive.ReadScope, store ownerVoiceRedeliveryStore, item archive.VoiceRedeliverySource) (types.JID, types.JID, error) {
	var zero types.JID
	if item.Key.Account != scope.Account || item.Source.Account != scope.Account || item.Source.Version.PolicyGeneration != scope.PolicyGeneration || item.Source.Version.Deleted || item.Source.Version.Excluded || item.OriginalSentAt.IsZero() || len(item.Key.Message) == 0 || len(item.Key.Message) > 512 || !utf8.ValidString(item.Key.Message) || strings.ContainsRune(item.Key.Message, 0) {
		return zero, zero, ErrMediaInvalidAdmission
	}
	chat, err := types.ParseJID(item.Key.Chat)
	if err != nil || chat.IsEmpty() || chat.String() != chat.ToNonAD().String() || chat.Server != types.DefaultUserServer && chat.Server != types.HiddenUserServer && chat.Server != types.GroupServer {
		return zero, zero, ErrMediaInvalidAdmission
	}
	sender, err := types.ParseJID(item.Key.Sender)
	if err != nil || sender.IsEmpty() || sender.String() != sender.ToNonAD().String() || sender.Server != types.DefaultUserServer && sender.Server != types.HiddenUserServer {
		return zero, zero, ErrMediaInvalidAdmission
	}
	canonical, err := r.store.Canonical(ctx, scope.Account, item.Key.Chat)
	if err != nil || canonical != item.Key.Chat {
		return zero, zero, ErrMediaSourceChanged
	}
	canonical, err = r.store.Canonical(ctx, scope.Account, item.Key.Sender)
	if err != nil || canonical != item.Key.Sender {
		return zero, zero, ErrMediaSourceChanged
	}
	current, err := store.CurrentSource(ctx, item.Key)
	if err != nil || current != item.Source {
		return zero, zero, ErrMediaSourceChanged
	}
	metadata, err := r.store.SourceMetadata(ctx, item.Key)
	if err != nil || metadata.Key != item.Key || metadata.Version != item.Source.Version || !metadata.OriginalDateKnown || !metadata.OriginalSentAt.Equal(item.OriginalSentAt) {
		return zero, zero, ErrMediaSourceChanged
	}
	policy, err := r.store.SourcePolicy(ctx, scope.Account)
	if err != nil || policy.Generation != scope.PolicyGeneration {
		return zero, zero, ErrMediaSourceChanged
	}
	now := time.Now()
	if !policyPermits(policy, archive.SourceEvent{Key: item.Key, OriginalSentAt: item.OriginalSentAt, OriginalDateKnown: true, ExpiresAt: item.Source.Version.ExpiresAt}, now) || !scope.From.IsZero() && item.OriginalSentAt.Before(scope.From) || !scope.Until.IsZero() && !item.OriginalSentAt.Before(scope.Until) || scope.Rolling > 0 && item.OriginalSentAt.Before(now.Add(-scope.Rolling)) {
		return zero, zero, ErrMediaSourceChanged
	}
	chat, err = r.voiceRedeliveryTransportJID(ctx, scope.Account, chat)
	if err != nil {
		return zero, zero, err
	}
	sender, err = r.voiceRedeliveryTransportJID(ctx, scope.Account, sender)
	if err != nil {
		return zero, zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, zero, errors.Join(ErrMediaCanceled, err)
	}
	return chat, sender, nil
}

// Canonical archive identities authorize the source. The SDK's saved mapping
// selects the phone transport address only when the archive agrees with it.
func (r *Runtime) voiceRedeliveryTransportJID(ctx context.Context, account string, authorized types.JID) (types.JID, error) {
	if authorized.Server != types.DefaultUserServer {
		return authorized, nil
	}
	if r.client.Store.LIDs == nil {
		return types.EmptyJID, ErrMediaUnavailable
	}
	mapped, err := r.client.Store.LIDs.GetLIDForPN(ctx, authorized)
	if err != nil {
		return types.EmptyJID, mediaFailure(err, ErrMediaUnavailable)
	}
	if mapped == types.EmptyJID {
		return authorized, nil
	}
	if mapped.User == "" || mapped.Server != types.HiddenUserServer || mapped != mapped.ToNonAD() || mapped.Integrator != 0 {
		return types.EmptyJID, ErrMediaInvalidAdmission
	}
	canonical, err := r.store.Canonical(ctx, account, mapped.String())
	if err != nil || canonical != authorized.String() {
		return types.EmptyJID, ErrMediaSourceChanged
	}
	return mapped, nil
}
