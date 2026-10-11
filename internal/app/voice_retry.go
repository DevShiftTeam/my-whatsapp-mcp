package app

import (
	"context"

	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

var _ localui.VoiceRetryApplication = (*Application)(nil)

func (a *Application) RetryVoiceDownloads(ctx context.Context) (localui.VoiceRetrySnapshot, error) {
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	defer a.data.Unlock()
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
	if value.Queued > 0 {
		a.wakeOriginalDownloads()
	}
	return localui.VoiceRetrySnapshot{Queued: value.Queued, Skipped: value.Skipped}, nil
}
