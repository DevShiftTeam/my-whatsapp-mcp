package app

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

// HistoricalViewConfig is trusted operator composition. No owner or MCP request
// can supply a path, capture identity, or authorization source.
type HistoricalViewConfig struct {
	DataDir    string
	SnapshotID string
	CapturedAt time.Time
}
type historicalViewOwner struct {
	store   *archive.Store
	lease   *appdata.InstanceLock
	factory archive.HistoricalReadFactory
}

func disjointHistoricalRoot(a, b string) bool {
	if !filepath.IsAbs(b) || filepath.Clean(b) != b {
		return false
	}
	nested := func(parent, child string) bool {
		rel, e := filepath.Rel(parent, child)
		return e == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
	}
	return !nested(a, b) && !nested(b, a)
}
func openHistoricalView(ctx context.Context, livePaths appdata.Paths, live *archive.Store, cfg Config, sourceGuards *LiveRebuildSourceGuards) (*historicalViewOwner, error) {
	if cfg.HistoricalView == nil {
		return nil, nil
	}
	c := cfg.HistoricalView
	if cfg.Snapshot != nil || cfg.Demo || !disjointHistoricalRoot(livePaths.Root, c.DataDir) {
		return nil, appdata.ErrUnsafeDataPath
	}
	// A derivative must not create lock or SQLite sidecars anywhere inside a
	// retained predecessor namespace, even though its own live root is distinct.
	if sourceGuards != nil {
		for _, ancestor := range sourceGuards.roots {
			if !disjointHistoricalRoot(ancestor, c.DataDir) {
				return nil, appdata.ErrUnsafeDataPath
			}
		}
	}
	paths, err := appdata.ExistingPaths(c.DataDir)
	if err != nil {
		return nil, err
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		return nil, err
	}
	owner := &historicalViewOwner{lease: lease}
	// Validate capture metadata under the derivative lease. Current authority
	// always comes from live, never from archived client records.
	if err = validateHistoricalSnapshot(paths, Config{DataDir: c.DataDir, Snapshot: &SnapshotConfig{CapturedAt: c.CapturedAt}}); err != nil {
		_ = owner.close()
		return nil, err
	}
	owner.store, err = archive.OpenHistoricalSnapshot(ctx, paths.ArchiveDir)
	if err == nil {
		owner.factory, err = archive.NewHistoricalView(ctx, live, owner.store, archive.HistoricalViewMetadata{Source: "historical", SnapshotID: c.SnapshotID, CapturedAt: c.CapturedAt, ReadOnly: true, Coverage: "partial", CurrentPhoneState: "unknown"})
	}
	if err != nil {
		for owner.close() != nil {
			time.Sleep(50 * time.Millisecond)
		}
		return nil, err
	}
	return owner, nil
}
func (h *historicalViewOwner) close() error {
	if h == nil {
		return nil
	}
	if h.store != nil {
		if err := h.store.Close(); err != nil {
			return err
		}
		h.store = nil
		h.factory = nil
	}
	if h.lease != nil {
		if err := h.lease.Close(); err != nil {
			return err
		}
		h.lease = nil
	}
	return nil
}
func (a *Application) historicalReadFactory() archive.HistoricalReadFactory {
	if a.historicalView == nil {
		return nil
	}
	return a.historicalView.factory
}
func (a *Application) historicalViewMetadata() *archive.HistoricalViewMetadata {
	if factory := a.historicalReadFactory(); factory != nil {
		v := factory.Metadata()
		return &v
	}
	return nil
}

// ReadArchive is only called by an already authenticated owner route. Its
// result retains data ownership through final HTTP delivery; ordinary settings
// and send operations never call this entry point.
func (a *Application) ReadArchive(ctx context.Context, request localui.ArchiveReadRequest) (localui.ArchiveReadResult, error) {
	a.data.RLock()
	if a.closed || a.historicalReadFactory() == nil {
		a.data.RUnlock()
		return localui.ArchiveReadResult{}, archive.ErrArchiveUnavailable
	}
	queryCtx, done := a.query(ctx)
	var once sync.Once
	var released atomic.Bool
	release := func() { once.Do(func() { released.Store(true); done(); a.data.RUnlock() }) }
	fail := func(err error) (localui.ArchiveReadResult, error) { release(); return localui.ArchiveReadResult{}, err }
	scope, err := a.scope(queryCtx)
	if err != nil {
		return fail(err)
	}
	reader, err := a.historicalReadFactory().ForOwner(queryCtx, scope)
	if err != nil {
		return fail(err)
	}
	var value any
	var guard archive.ResponseGuard
	switch request.Kind {
	case "search":
		value, guard, err = reader.SearchGuarded(queryCtx, scope, request.Search)
	case "context":
		value, guard, err = reader.ContextGuarded(queryCtx, scope, request.Context)
	case "chats":
		value, guard, err = reader.ListChatsGuarded(queryCtx, scope, request.Chats)
	case "status":
		value, guard, err = reader.StatusGuarded(queryCtx, scope)
	case "stats":
		value, guard, err = reader.StatsGuarded(queryCtx, scope, request.Stats)
	default:
		return fail(archive.ErrInvalidQuery)
	}
	if err != nil {
		return fail(err)
	}
	check := func(checkCtx context.Context) error {
		if released.Load() {
			return archive.ErrArchiveUnavailable
		}
		if e := queryCtx.Err(); e != nil {
			return e
		}
		_, e := reader.CheckResponse(checkCtx, scope, archive.ResponseCheck{Guard: guard})
		if e != nil {
			return e
		}
		return queryCtx.Err()
	}
	return localui.ArchiveReadResult{Value: value, Metadata: reader.Metadata(), Check: check, Release: release}, nil
}
func liveSearchHandles(request archive.SearchRequest) bool {
	if archive.IsHistoricalHandle(request.Cursor) {
		return false
	}
	for _, chat := range request.Chats {
		if archive.IsHistoricalHandle(string(chat)) {
			return false
		}
	}
	return true
}
