package app

import (
	"context"
	"errors"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/localui"
)

var _ localui.OriginalBudgetApplication = (*Application)(nil)

func publicOriginalBudget(value archive.OriginalBudget) localui.OriginalBudgetSnapshot {
	return localui.OriginalBudgetSnapshot{MaximumBytes: value.MaximumBytes, ChargedBytes: value.ChargedBytes, Revision: value.Revision, Pressure: value.Pressure}
}

func (a *Application) OriginalBudget(ctx context.Context) (localui.OriginalBudgetSnapshot, error) {
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed {
		return localui.OriginalBudgetSnapshot{}, localui.ErrOriginalBudgetUnavailable
	}
	ctx, done := a.query(ctx)
	defer done()
	value, err := a.store.GetOriginalBudget(ctx)
	if err != nil {
		return localui.OriginalBudgetSnapshot{}, localui.ErrOriginalBudgetUnavailable
	}
	return publicOriginalBudget(value), nil
}

func (a *Application) SetOriginalBudget(ctx context.Context, request localui.OriginalBudgetRequest) (localui.OriginalBudgetSnapshot, error) {
	if request.MaximumBytes < archive.MinimumOriginalBudgetBytes || request.MaximumBytes > archive.MaximumOriginalBudgetBytes || request.ExpectedRevision == 0 {
		return localui.OriginalBudgetSnapshot{}, localui.ErrOriginalBudgetUnavailable
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed {
		return localui.OriginalBudgetSnapshot{}, localui.ErrOriginalBudgetUnavailable
	}
	previous, err := a.store.GetOriginalBudget(ctx)
	if err != nil {
		return localui.OriginalBudgetSnapshot{}, localui.ErrOriginalBudgetUnavailable
	}
	value, err := a.store.SetOriginalBudget(ctx, archive.OriginalBudgetUpdate{MaximumBytes: request.MaximumBytes, ExpectedRevision: request.ExpectedRevision})
	if errors.Is(err, archive.ErrOriginalBudgetChanged) {
		return localui.OriginalBudgetSnapshot{}, localui.ErrOriginalBudgetConflict
	}
	if err != nil {
		return localui.OriginalBudgetSnapshot{}, localui.ErrOriginalBudgetUnavailable
	}
	if value.MaximumBytes > previous.MaximumBytes {
		a.wakeOriginalDownloads()
	}
	return publicOriginalBudget(value), nil
}
