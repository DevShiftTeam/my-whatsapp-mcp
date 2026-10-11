package app

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"

	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

var _ localui.VoiceRetryApplication = (*Application)(nil)

func (a *Application) RetryVoiceDownloads(ctx context.Context) (localui.VoiceRetrySnapshot, error) {
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return localui.VoiceRetrySnapshot{}, localui.ErrVoiceRetryUnavailable
	}
	ctx, done := a.query(ctx)
	defer done()
	scope, err := a.scope(ctx)
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
	if runtime, ok := a.runtime.(interface {
		RequestVoiceRedelivery(context.Context, archive.ReadScope) (int, error)
	}); ok {
		requested, _ := runtime.RequestVoiceRedelivery(ctx, scope)
		if requested < 0 || requested > 8 {
			return localui.VoiceRetrySnapshot{}, localui.ErrVoiceRetryUnavailable
		}
		result.Requested = requested
	}
	return result, nil
}
