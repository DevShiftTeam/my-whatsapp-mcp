package app

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"sync"
	"sync/atomic"
	"time"
)

// ReadLiveArchive retains the current store and its response witness through
// owner serialization. Discovery and diagnostics retain their owner-only scope.
func (a *Application) ReadLiveArchive(ctx context.Context, q localui.ArchiveReadRequest) (localui.ArchiveReadResult, error) {
	a.data.RLock()
	if a.closed {
		a.data.RUnlock()
		return localui.ArchiveReadResult{}, archive.ErrArchiveUnavailable
	}
	queryCtx, done := a.query(ctx)
	var once sync.Once
	var released atomic.Bool
	release := func() { once.Do(func() { released.Store(true); done(); a.data.RUnlock() }) }
	fail := func(e error) (localui.ArchiveReadResult, error) { release(); return localui.ArchiveReadResult{}, e }
	var value any
	var checkSource func(context.Context) error
	if q.Kind == "chats" || q.Kind == "status" {
		var snapshot archive.ReadSnapshot
		if q.Kind == "chats" {
			if archive.IsHistoricalHandle(q.Chats.Cursor) {
				return fail(archive.ErrInvalidQuery)
			}
			page, err := a.store.DiscoveryChats(queryCtx, q.Chats)
			if err != nil {
				return fail(err)
			}
			value = page
			snapshot = page.Snapshot
		} else {
			page, err := a.statusLocked(queryCtx)
			if err != nil {
				return fail(err)
			}
			value = page
			snapshot = page.Snapshot
		}
		checkSource = func(c context.Context) error { return a.store.CheckOwnerMetadataSnapshot(c, snapshot) }
	} else {
		scope, err := a.scope(queryCtx)
		if errors.Is(err, archive.ErrUnauthorized) && q.Kind == "stats" && archive.ValidStatsRequest(q.Stats) && !archive.IsHistoricalHandle(string(q.Stats.ChatID)) {
			diagnostic, e := a.store.DiagnosticStatus(queryCtx)
			if e != nil {
				return fail(e)
			}
			empty := archive.EmptyStatsSnapshot(q.Stats, time.Now().UTC())
			empty.UnknownHistoryGap = diagnostic.Snapshot.UnknownHistoryGap
			empty.DeliveredHistorySnapshot = diagnostic.Snapshot.DeliveredHistorySnapshot
			value = empty
			checkSource = func(c context.Context) error { return a.store.CheckOwnerMetadataSnapshot(c, diagnostic.Snapshot) }
		} else {
			if err != nil {
				return fail(err)
			}
			var guard archive.ResponseGuard
			switch q.Kind {
			case "search":
				if !liveSearchHandles(q.Search) {
					return fail(archive.ErrInvalidQuery)
				}
				value, guard, err = a.resources.retrieval.SearchGuarded(queryCtx, scope, q.Search)
			case "context":
				if archive.IsHistoricalHandle(string(q.Context.MessageID)) {
					return fail(archive.ErrInvalidQuery)
				}
				value, guard, err = a.store.ContextGuarded(queryCtx, scope, q.Context)
			case "stats":
				if archive.IsHistoricalHandle(string(q.Stats.ChatID)) {
					return fail(archive.ErrInvalidQuery)
				}
				value, guard, err = a.store.StatsGuarded(queryCtx, scope, q.Stats)
			default:
				return fail(archive.ErrInvalidQuery)
			}
			if err != nil {
				return fail(err)
			}
			checkSource = func(c context.Context) error {
				_, e := a.store.CheckResponse(c, scope, archive.ResponseCheck{Guard: guard})
				return e
			}
		}
	}
	check := func(c context.Context) error {
		if released.Load() {
			return archive.ErrArchiveUnavailable
		}
		if err := queryCtx.Err(); err != nil {
			return err
		}
		if err := checkSource(c); err != nil {
			return err
		}
		return queryCtx.Err()
	}
	return localui.ArchiveReadResult{Value: value, Check: check, Release: release}, nil
}
