package app

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

var _ localui.VoiceRetryApplication = (*Application)(nil)

func (a *Application) RetryVoiceDownloads(ctx context.Context) (localui.VoiceRetrySnapshot, error) {
	return a.RetryVoiceDownloadsForChat(ctx, "")
}

func (a *Application) RetryVoiceDownloadsForChat(ctx context.Context, chat archive.ChatID) (localui.VoiceRetrySnapshot, error) {
	if a.snapshot != nil {
		return localui.VoiceRetrySnapshot{}, archive.ErrHistoricalSnapshotReadOnly
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return localui.VoiceRetrySnapshot{}, localui.ErrVoiceRetryUnavailable
	}
	ctx, done := a.query(ctx)
	defer done()
	scope, err := a.voiceRetryScope(ctx, chat)
	if err != nil {
		return localui.VoiceRetrySnapshot{}, localui.ErrVoiceRetryUnavailable
	}
	value, err := a.store.RetryFailedVoiceDownloads(ctx, scope, 32)
	if err != nil {
		return localui.VoiceRetrySnapshot{}, localui.ErrVoiceRetryUnavailable
	}
	if remaining := 32 - value.Queued - value.Skipped; remaining > 0 {
		redelivered, retryErr := a.store.RetryRedeliveredVoiceDownloads(ctx, scope, remaining)
		if retryErr != nil {
			return localui.VoiceRetrySnapshot{}, localui.ErrVoiceRetryUnavailable
		}
		value.Queued += redelivered.Queued
		value.Skipped += redelivered.Skipped
	}
	if value.Queued > 0 {
		a.wakeOriginalDownloads()
	}
	result := localui.VoiceRetrySnapshot{Queued: value.Queued, Skipped: value.Skipped}
	var request func(context.Context, archive.ReadScope) (int, error)
	if runtime, ok := a.runtime.(interface {
		RequestOwnerVoiceRedownloads(context.Context, archive.ReadScope) (int, error)
	}); ok {
		request = runtime.RequestOwnerVoiceRedownloads
	} else if runtime, ok := a.runtime.(interface {
		RequestVoiceRedelivery(context.Context, archive.ReadScope) (int, error)
	}); ok {
		request = runtime.RequestVoiceRedelivery
	}
	if request != nil {
		requested, requestErr := request(ctx, scope)
		if requestErr != nil {
			result.Recovery.Code = "voice_retry_unavailable"
		}
		if requested < 0 || requested > 8 {
			return localui.VoiceRetrySnapshot{}, localui.ErrVoiceRetryUnavailable
		}
		result.Requested = requested
	}
	recovery, statusErr := a.store.OwnerVoiceRecoveryStatus(ctx, scope)
	if statusErr != nil {
		return localui.VoiceRetrySnapshot{}, localui.ErrVoiceRetryUnavailable
	}
	if result.Recovery.Code != "" && recovery.Code == "" {
		recovery.Code = result.Recovery.Code
	}
	result.Recovery = recovery
	return result, nil
}

func (a *Application) voiceRetryScope(ctx context.Context, chat archive.ChatID) (archive.ReadScope, error) {
	scope, err := a.scope(ctx)
	if err != nil || chat == "" {
		return scope, err
	}
	if len(chat) > 128 || !utf8.ValidString(string(chat)) || strings.ContainsRune(string(chat), 0) {
		return archive.ReadScope{}, archive.ErrUnauthorized
	}
	if !scope.AllChats {
		permitted := false
		for _, id := range scope.Chats {
			if id == chat {
				permitted = true
				break
			}
		}
		if !permitted {
			return archive.ReadScope{}, archive.ErrUnauthorized
		}
	}
	scope.AllChats = false
	scope.Chats = []archive.ChatID{chat}
	// A bounded scoped read proves this opaque chat currently belongs to the
	// owner and policy before any retry operation can produce work.
	page, err := a.store.ListChats(ctx, scope, archive.ChatListRequest{Limit: 1})
	if err != nil || len(page.Chats) != 1 || page.Chats[0].ID != chat {
		return archive.ReadScope{}, archive.ErrUnauthorized
	}
	return scope, nil
}

func (a *Application) VoiceRecoveryStatus(ctx context.Context, chat archive.ChatID) (archive.OwnerVoiceRecoveryStatus, error) {
	if a.snapshot != nil {
		return archive.OwnerVoiceRecoveryStatus{}, archive.ErrHistoricalSnapshotReadOnly
	}
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return archive.OwnerVoiceRecoveryStatus{}, localui.ErrVoiceRetryUnavailable
	}
	ctx, done := a.query(ctx)
	defer done()
	scope, err := a.voiceRetryScope(ctx, chat)
	if err != nil {
		return archive.OwnerVoiceRecoveryStatus{}, localui.ErrVoiceRetryUnavailable
	}
	return a.store.OwnerVoiceRecoveryStatus(ctx, scope)
}
