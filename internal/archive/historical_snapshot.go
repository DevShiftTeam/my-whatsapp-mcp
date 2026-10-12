package archive

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Bodokh/my-whatsapp-mcp/internal/sqlitelex"
	"github.com/Bodokh/my-whatsapp-mcp/internal/sqlitevec"
)

const HistoricalSnapshotMarker = "historical-snapshot.json"

var ErrHistoricalSnapshotReadOnly = errors.New("historical_snapshot_read_only")

// RefuseHistoricalSnapshot prevents ordinary constructors and maintenance tools
// from acquiring write authority over a deliberately sealed historical copy.
func RefuseHistoricalSnapshot(privateDir string) error {
	if _, err := os.Lstat(filepath.Join(privateDir, HistoricalSnapshotMarker)); !os.IsNotExist(err) {
		return ErrHistoricalSnapshotReadOnly
	}
	return nil
}

// OpenHistoricalSnapshot requires the caller's instance lease and explicit
// trusted snapshot configuration. It never creates a database, migrates schema,
// starts a writer, repairs pins, sweeps expiry, or checkpoints on close.
func OpenHistoricalSnapshot(ctx context.Context, privateDir string) (*Store, error) {
	if err := sqlitelex.Register(); err != nil {
		return nil, ErrArchiveUnavailable
	}
	if err := sqlitevec.Register(); err != nil {
		return nil, ErrArchiveUnavailable
	}
	path, err := originalCacheExistingArchive(privateDir, true)
	if err != nil {
		return nil, err
	}
	lock, err := acquireArchiveLock(ctx, privateDir)
	if err != nil {
		return nil, err
	}
	held := true
	defer func() {
		if held {
			lock.Close()
		}
	}()
	for _, name := range []string{"observation.marker", "unsafe.marker", credentialFenceMarker + ".pending"} {
		if _, err := os.Lstat(filepath.Join(privateDir, name)); !os.IsNotExist(err) {
			return nil, ErrReconciliationRequired
		}
	}
	fence := filepath.Join(privateDir, credentialFenceMarker)
	if err := checkPrivateFile(fence); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(fence); err != nil {
		return nil, ErrUnsafeDataPath
	}
	if err := validateOriginalCacheRecoverySchemaMode(ctx, path, true); err != nil {
		return nil, err
	}
	base := url.URL{Scheme: "file", Path: path}
	base.RawQuery = url.Values{"mode": {"ro"}, "_busy_timeout": {"500"}, "_foreign_keys": {"on"}}.Encode()
	db, err := sql.Open(queryOnlyDriverName, base.String())
	if err != nil {
		return nil, ErrArchiveUnavailable
	}
	o, _ := normalizedOptions(Options{}) // The security clock is always current time.
	db.SetMaxOpenConns(o.ReadConnections)
	db.SetMaxIdleConns(o.ReadConnections)
	s := &Store{readDB: db, lockFile: lock, privateDir: privateDir, options: o, historicalSnapshot: true, unsafe: make(map[string]bool)}
	fail := func(err error) (*Store, error) { db.Close(); return nil, err }
	if err := db.QueryRowContext(ctx, "SELECT cursor_key FROM archive_meta WHERE id=1").Scan(&s.cursorKey); err != nil {
		return fail(ErrArchiveUnavailable)
	}
	if err := initializeCredentialFence(s); err != nil {
		return fail(err)
	}
	if s.credentialFence.allDenied {
		return fail(ErrUnauthorized)
	}
	held = false
	return s, nil
}
