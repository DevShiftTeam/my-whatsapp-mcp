package archive

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"github.com/Bodokh/wa-indexer/internal/sqlitelex"
	"github.com/Bodokh/wa-indexer/internal/sqlitevec"
	_ "github.com/mattn/go-sqlite3"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Options bounds all background admissions and foreground SQLite reads.
type Options struct {
	BodyQueueCount    int
	BodyQueueBytes    int
	ControlQueueCount int
	ControlDeadline   time.Duration
	ReadTimeout       time.Duration
	ReadConnections   int
	Now               func() time.Time
}

// Store owns one serialized writer and a separate limited query-only read pool.
// SQLite handles and protected paths never cross the public interfaces.
type Store struct {
	lockFile           *os.File
	writeDB, readDB    *sql.DB
	privateDir         string
	options            Options
	extensionGuards    extensionGuardState
	credentialFence    credentialFenceState
	cursorKey          []byte
	bodies, controls   chan command
	stop               chan struct{}
	done               chan struct{}
	lifeMu             sync.RWMutex
	closed             bool
	queuedBytes        atomic.Int64
	queuedCount        atomic.Int64
	safetyMu           sync.RWMutex
	safetyEpoch        uint64
	unsafe             map[string]bool
	unclean            bool
	observation        ObservationID
	observationAccount string
}

func normalizedOptions(o Options) (Options, error) {
	if o.BodyQueueCount == 0 {
		o.BodyQueueCount = 256
	}
	if o.BodyQueueBytes == 0 {
		o.BodyQueueBytes = 8 << 20
	}
	if o.ControlQueueCount == 0 {
		o.ControlQueueCount = 64
	}
	if o.ControlDeadline == 0 {
		o.ControlDeadline = 500 * time.Millisecond
	}
	if o.ReadTimeout == 0 {
		o.ReadTimeout = 5 * time.Second
	}
	if o.ReadConnections == 0 {
		o.ReadConnections = 4
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.BodyQueueCount < 1 || o.BodyQueueCount > 4096 || o.BodyQueueBytes < 1024 || o.BodyQueueBytes > 64<<20 || o.ControlQueueCount < 1 || o.ControlQueueCount > 1024 || o.ControlDeadline <= 0 || o.ControlDeadline > 2*time.Second || o.ReadTimeout <= 0 || o.ReadTimeout > 30*time.Second || o.ReadConnections < 1 || o.ReadConnections > 8 {
		return o, ErrInvalidPolicy
	}
	return o, nil
}

// Open requires the application instance lease to be held by the caller
// for the entire Store lifetime. internal/appdata owns that process boundary.
func Open(ctx context.Context, privateDir string, options Options) (*Store, error) {
	if err := sqlitelex.Register(); err != nil {
		return nil, ErrArchiveUnavailable
	}
	if err := sqlitevec.Register(); err != nil {
		return nil, ErrArchiveUnavailable
	}
	o, err := normalizedOptions(options)
	if err != nil {
		return nil, err
	}
	dir, err := preparePrivateDir(privateDir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "archive.sqlite")
	for _, name := range []string{path, path + "-wal", path + "-shm", filepath.Join(dir, "observation.marker"), filepath.Join(dir, "unsafe.marker"), filepath.Join(dir, "archive.lock")} {
		if err = checkPrivateFile(name); err != nil {
			return nil, err
		}
	}
	lockFile, err := acquireArchiveLock(ctx, dir)
	if err != nil {
		return nil, err
	}
	lockHeld := true
	defer func() {
		if lockHeld {
			lockFile.Close()
		}
	}()
	// Pre-create the DB privately so sidecars inherit owner-only permissions.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, ErrArchiveUnavailable
	}
	f.Close()
	base := url.URL{Scheme: "file", Path: path}
	values := url.Values{"_busy_timeout": {"500"}, "_foreign_keys": {"on"}, "_cache_size": {"-4096"}, "_synchronous": {"FULL"}, "_secure_delete": {"on"}}
	base.RawQuery = values.Encode()
	db, err := sql.Open("sqlite3", base.String())
	if err != nil {
		return nil, ErrArchiveUnavailable
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(e error) (*Store, error) { db.Close(); return nil, e }
	if _, err = db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
		return fail(ErrArchiveUnavailable)
	}
	if err = migrate(ctx, db); err != nil {
		return fail(err)
	}
	values.Set("mode", "ro")
	values.Set("_query_only", "on")
	base.RawQuery = values.Encode()
	reads, err := sql.Open("sqlite3", base.String())
	if err != nil {
		return fail(ErrArchiveUnavailable)
	}
	reads.SetMaxOpenConns(o.ReadConnections)
	reads.SetMaxIdleConns(o.ReadConnections)
	if err = reads.PingContext(ctx); err != nil {
		reads.Close()
		return fail(ErrArchiveUnavailable)
	}
	// go-sqlite3 does not recognize _query_only: install query_only for every
	// pooled connection with a dedicated registered driver (defined in migrate.go).
	reads.Close()
	reads, err = sql.Open(queryOnlyDriverName, base.String())
	if err != nil {
		return fail(ErrArchiveUnavailable)
	}
	reads.SetMaxOpenConns(o.ReadConnections)
	reads.SetMaxIdleConns(o.ReadConnections)
	s := &Store{writeDB: db, readDB: reads, lockFile: lockFile, privateDir: dir, options: o, bodies: make(chan command, o.BodyQueueCount), controls: make(chan command, o.ControlQueueCount), stop: make(chan struct{}), done: make(chan struct{}), unsafe: make(map[string]bool)}
	if err = db.QueryRowContext(ctx, "SELECT cursor_key FROM archive_meta WHERE id=1").Scan(&s.cursorKey); err != nil {
		reads.Close()
		return fail(ErrArchiveUnavailable)
	}
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		if err = checkPrivateFile(name); err != nil {
			reads.Close()
			return fail(err)
		}
	}
	for _, marker := range []string{"observation.marker", "unsafe.marker"} {
		if _, err = os.Lstat(filepath.Join(dir, marker)); err == nil {
			s.unclean = true
		} else if !os.IsNotExist(err) {
			reads.Close()
			return fail(ErrUnsafeDataPath)
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT account FROM account_safety WHERE unsafe=1")
	if err != nil {
		reads.Close()
		return fail(ErrArchiveUnavailable)
	}
	for rows.Next() {
		var account string
		if err = rows.Scan(&account); err != nil {
			rows.Close()
			reads.Close()
			return fail(ErrArchiveUnavailable)
		}
		s.unsafe[account] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		reads.Close()
		return fail(ErrArchiveUnavailable)
	}
	if _, err = s.sweepExpired(ctx); err != nil {
		reads.Close()
		return fail(err)
	}
	if err = initializeCredentialFence(s); err != nil {
		reads.Close()
		return fail(err)
	}
	go s.writerLoop()
	lockHeld = false
	return s, nil
}
func (s *Store) Close() error {
	s.lifeMu.Lock()
	if s.closed {
		s.lifeMu.Unlock()
		return nil
	}
	s.closed = true
	close(s.stop)
	s.lifeMu.Unlock()
	<-s.done
	// Observation markers are cleared only by EndObservation after caller drain.
	e1 := s.readDB.Close()
	_, e2 := s.writeDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	e3 := s.writeDB.Close()
	e4 := s.lockFile.Close()
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return ErrArchiveUnavailable
	}
	return nil
}
func (s *Store) now() time.Time { return s.options.Now().UTC() }
func (s *Store) readContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.options.ReadTimeout)
}
func (s *Store) readSafety(account string) (uint64, error) {
	s.safetyMu.RLock()
	defer s.safetyMu.RUnlock()
	if account == "" {
		return 0, ErrUnauthorized
	}
	if s.unclean || s.unsafe[account] {
		return 0, ErrReconciliationRequired
	}
	return s.safetyEpoch, nil
}
func (s *Store) verifyReadSafety(account string, epoch uint64) error {
	current, e := s.readSafety(account)
	if e != nil {
		return e
	}
	if current != epoch {
		return ErrPolicyChanged
	}
	return nil
}
func randomID() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", ErrArchiveUnavailable
	}
	return hex.EncodeToString(b[:]), nil
}
func unixms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMilli()
}
func fromMillis(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}
func preparePrivateDir(path string) (string, error) {
	abs, e := filepath.Abs(path)
	if e != nil || path == "" {
		return "", ErrUnsafeDataPath
	}
	abs = filepath.Clean(abs)
	// Every existing component must be a real directory. Ancestors may have
	// ordinary owner/group permissions; only our archive root is private.
	for part := abs; ; part = filepath.Dir(part) {
		st, e := os.Lstat(part)
		if e == nil {
			if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
				return "", ErrUnsafeDataPath
			}
		} else if !os.IsNotExist(e) {
			return "", ErrUnsafeDataPath
		}
		if filepath.Dir(part) == part {
			break
		}
	}
	if e = os.MkdirAll(abs, 0700); e != nil {
		return "", ErrUnsafeDataPath
	}
	if e = os.Chmod(abs, 0700); e != nil {
		return "", ErrUnsafeDataPath
	}
	return abs, nil
}
func checkPrivateFile(path string) error {
	st, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
		return ErrUnsafeDataPath
	}
	return nil
}
