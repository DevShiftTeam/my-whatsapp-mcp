package app

import (
	"context"
	"errors"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// Stats is an owner aggregate read. It never broadens an MCP client's scope.
func (a *Application) Stats(ctx context.Context, request archive.StatsRequest) (archive.StatsSnapshot, error) {
	if archive.IsHistoricalHandle(string(request.ChatID)) {
		return archive.StatsSnapshot{}, archive.ErrInvalidQuery
	}
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return archive.StatsSnapshot{}, archive.ErrArchiveUnavailable
	}
	if !archive.ValidStatsRequest(request) {
		return archive.StatsSnapshot{}, archive.ErrInvalidQuery
	}
	ctx, done := a.query(ctx)
	defer done()
	scope, err := a.scope(ctx)
	if errors.Is(err, archive.ErrUnauthorized) {
		// An owner can view an empty dashboard before linking an account.
		return archive.EmptyStatsSnapshot(request, time.Now().UTC()), nil
	}
	if err != nil {
		return archive.StatsSnapshot{}, err
	}
	page, guard, err := a.store.StatsGuarded(ctx, scope, request)
	if err != nil {
		return archive.StatsSnapshot{}, err
	}
	if _, err = a.store.CheckResponse(ctx, scope, archive.ResponseCheck{Guard: guard}); err != nil {
		return archive.StatsSnapshot{}, err
	}
	return page, nil
}
