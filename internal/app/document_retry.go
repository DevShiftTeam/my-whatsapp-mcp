package app

import (
	"context"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

var _ localui.DocumentRetryApplication = (*Application)(nil)

func (a *Application) RetryDocumentDownloads(ctx context.Context, ids []archive.MessageID) (localui.DocumentRetrySnapshot, error) {
	if !localui.ValidDocumentRetryIDs(ids) {
		return localui.DocumentRetrySnapshot{}, localui.ErrDocumentRetryUnavailable
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return localui.DocumentRetrySnapshot{}, localui.ErrDocumentRetryUnavailable
	}
	ctx, done := a.query(ctx)
	defer done()
	scope, err := a.scope(ctx)
	if err != nil {
		return localui.DocumentRetrySnapshot{}, localui.ErrDocumentRetryUnavailable
	}
	value, err := a.store.RetryDocumentDownloads(ctx, scope, ids)
	if err != nil {
		return localui.DocumentRetrySnapshot{}, localui.ErrDocumentRetryUnavailable
	}
	if value.Queued > 0 {
		a.wakeOriginalDownloads()
	}
	result := localui.DocumentRetrySnapshot{Queued: value.Queued, Skipped: value.Skipped}
	if runtime, ok := a.runtime.(interface {
		RequestDocumentRedelivery(context.Context, archive.ReadScope, []archive.MessageID) (int, error)
	}); ok {
		requested, retryErr := runtime.RequestDocumentRedelivery(ctx, scope, ids)
		if retryErr != nil || requested < 0 || requested > len(ids) {
			return localui.DocumentRetrySnapshot{}, localui.ErrDocumentRetryUnavailable
		}
		result.Requested = requested
	}
	return result, nil
}
