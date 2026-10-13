package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

// ReadSavedArchive borrows both existing stores and the live hybrid engine.
// The owner response owns this read lifetime until delivery or discard.
func (a *Application) ReadSavedArchive(ctx context.Context, q localui.ArchiveReadRequest) (localui.ArchiveReadResult, error) {
	a.data.RLock()
	if a.closed {
		a.data.RUnlock()
		return localui.ArchiveReadResult{}, archive.ErrArchiveUnavailable
	}
	linked, done := a.query(ctx)
	queryCtx, cancel := context.WithTimeout(linked, 5*time.Second)
	var once sync.Once
	var released atomic.Bool
	release := func() { once.Do(func() { released.Store(true); cancel(); done(); a.data.RUnlock() }) }
	fail := func(err error) (localui.ArchiveReadResult, error) { release(); return localui.ArchiveReadResult{}, err }
	metadata := &archive.SavedViewMetadata{Source: "saved", Historical: a.historicalViewMetadata(), Coverage: "partial", CurrentPhoneState: "unknown"}
	scope, err := a.scope(queryCtx)
	var value any
	var checkSource func(context.Context) error
	if errors.Is(err, archive.ErrUnauthorized) && q.Kind == "stats" && archive.ValidStatsRequest(q.Stats) && !foreignArchiveHandle(string(q.Stats.ChatID)) {
		// Preserve the setup dashboard without granting snapshot content authority.
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
		var historical *archive.HistoricalView
		if factory := a.historicalReadFactory(); factory != nil {
			var ok bool
			historical, ok = factory.(*archive.HistoricalView)
			if !ok {
				return fail(archive.ErrArchiveUnavailable)
			}
		}
		if a.resources == nil || a.resources.retrieval == nil {
			return fail(archive.ErrArchiveUnavailable)
		}
		reader, e := archive.NewSavedOwnerRead(queryCtx, a.store, historical, scope, a.resources.retrieval)
		if e != nil {
			return fail(e)
		}
		var guard archive.SavedResponseGuard
		switch q.Kind {
		case "search":
			value, guard, err = reader.Search(queryCtx, q.Search)
		case "browse":
			value, guard, err = reader.Browse(queryCtx, q.Browse)
		case "context":
			value, guard, err = reader.Context(queryCtx, q.Context)
		case "chats":
			value, guard, err = reader.Chats(queryCtx, q.Chats)
		case "stats":
			value, guard, err = reader.Stats(queryCtx, q.Stats)
		default:
			return fail(archive.ErrInvalidQuery)
		}
		if err != nil {
			return fail(err)
		}
		checkSource = func(c context.Context) error { return reader.Check(c, guard) }
	}
	check := func(c context.Context) error {
		if released.Load() {
			return archive.ErrArchiveUnavailable
		}
		if err := queryCtx.Err(); err != nil {
			return err
		}
		if err := c.Err(); err != nil {
			return err
		}
		deadline, _ := queryCtx.Deadline()
		checked, stop := context.WithDeadline(c, deadline)
		defer stop()
		unlink := context.AfterFunc(queryCtx, stop)
		defer unlink()
		if err := checkSource(checked); err != nil {
			return err
		}
		return queryCtx.Err()
	}
	return localui.ArchiveReadResult{Value: value, SavedMetadata: metadata, Check: check, Release: release}, nil
}

func foreignArchiveHandle(id string) bool {
	return archive.IsHistoricalHandle(id) || strings.HasPrefix(id, "s1.")
}
