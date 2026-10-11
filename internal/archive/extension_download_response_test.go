//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var originalDownloadCommitDriverSequence atomic.Uint64

// Wrap the actual SQLite COMMIT, rather than count clock calls or manufacture
// query results. Delegating ConnBeginTx preserves the driver's transaction API.
type originalDownloadCommitDriver struct {
	driver.Driver
	armed, committed *atomic.Bool
	delay            time.Duration
}
type originalDownloadCommitConn struct {
	driver.Conn
	owner *originalDownloadCommitDriver
}
type originalDownloadCommitTx struct {
	driver.Tx
	owner *originalDownloadCommitDriver
}

func (d *originalDownloadCommitDriver) Open(name string) (driver.Conn, error) {
	c, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return originalDownloadCommitConn{Conn: c, owner: d}, nil
}
func (c originalDownloadCommitConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, ErrArchiveUnavailable
	}
	tx, err := beginner.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return originalDownloadCommitTx{Tx: tx, owner: c.owner}, nil
}
func (c originalDownloadCommitConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return q.QueryContext(ctx, query, args)
}
func (c originalDownloadCommitConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return e.ExecContext(ctx, query, args)
}
func (tx originalDownloadCommitTx) Commit() error {
	if tx.owner.armed.Load() && tx.owner.delay > 0 {
		time.Sleep(tx.owner.delay)
	}
	err := tx.Tx.Commit()
	if err == nil && tx.owner.armed.Load() {
		tx.owner.committed.Store(true)
	}
	return err
}
func originalDownloadTestCommitBoundary(t *testing.T, s *Store, delay time.Duration) (*atomic.Bool, *atomic.Bool) {
	t.Helper()
	armed, committed := new(atomic.Bool), new(atomic.Bool)
	name := fmt.Sprintf("original_download_commit_%d", originalDownloadCommitDriverSequence.Add(1))
	sql.Register(name, &originalDownloadCommitDriver{Driver: s.writeDB.Driver(), armed: armed, committed: committed, delay: delay})
	if err := s.writeDB.Close(); err != nil {
		t.Fatal(err)
	}
	dsn := url.URL{Scheme: "file", Path: filepath.Join(s.privateDir, "archive.sqlite"), RawQuery: "_foreign_keys=on&_journal_mode=WAL&_synchronous=FULL&_busy_timeout=500&_txlock=immediate"}
	var err error
	s.writeDB, err = sql.Open(name, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	s.writeDB.SetMaxOpenConns(1)
	return armed, committed
}

func TestOriginalDownloadLoadRevocationDuringPostCommitClockReturnsNoInput(t *testing.T) {
	s, clock, _ := originalDownloadTestArchive(t)
	port := originalDownloadTestPort(t, s)
	p := testPolicy(t, s)
	originalDownloadTestSource(t, s, p, "response-revocation", MessageImage)
	w := originalDownloadTestClaim(t, port)
	armed, committed := originalDownloadTestCommitBoundary(t, s, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var blocked atomic.Bool
	previousClock := s.options.Now
	s.options.Now = func() time.Time {
		if committed.Load() && blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return fromMillis(clock.Load())
	}
	type outcome struct {
		input OriginalDownloadInput
		err   error
	}
	loaded := make(chan outcome, 1)
	unsafeDone := make(chan error, 1)
	loadStarted, loadJoined, unsafeStarted, unsafeJoined := false, false, false, false
	defer func() {
		once.Do(func() { close(release) })
		if loadStarted && !loadJoined {
			select {
			case <-loaded:
			case <-time.After(time.Second):
				t.Error("Load cleanup did not join")
			}
		}
		if unsafeStarted && !unsafeJoined {
			select {
			case <-unsafeDone:
			case <-time.After(time.Second):
				t.Error("revocation cleanup did not join")
			}
		}
		s.options.Now = previousClock
	}()
	armed.Store(true)
	loadStarted = true
	go func() {
		in, err := port.LoadOriginalDownload(context.Background(), w.Lease)
		loaded <- outcome{in, err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Load did not reach post-COMMIT clock boundary")
	}
	unsafeStarted = true
	go func() {
		unsafeDone <- s.MarkUnsafe(context.Background(), "account", UnsafeReason("mutation_metadata_missing"))
	}()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if s.safetyMu.TryRLock() {
			denied := s.unclean || s.unsafe["account"]
			s.safetyMu.RUnlock()
			if denied {
				break
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("concurrent privacy revocation did not deny immediately")
		case <-tick.C:
		}
	}
	once.Do(func() { close(release) })
	select {
	case result := <-loaded:
		loadJoined = true
		if !errors.Is(result.err, ErrReconciliationRequired) || len(result.input.ProtocolReference) != 0 || result.input.Work.Lease.MediaID != "" {
			t.Fatalf("revoked Load returned private authority: %#v %v", result.input.Work, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("revoked Load did not finish")
	}
	select {
	case err := <-unsafeDone:
		unsafeJoined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("revocation persistence did not finish")
	}
}

func TestOriginalDownloadLoadDelayedCommitUsesElapsedLeaseHorizon(t *testing.T) {
	s, _, _ := originalDownloadTestArchive(t)
	port := originalDownloadTestPort(t, s)
	p := testPolicy(t, s)
	originalDownloadTestSource(t, s, p, "response-expiry", MessageImage)
	w, err := port.ClaimOriginalDownload(context.Background(), OriginalDownloadClaim{Account: "account", TTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	armed, _ := originalDownloadTestCommitBoundary(t, s, 1100*time.Millisecond)
	armed.Store(true)
	in, err := port.LoadOriginalDownload(context.Background(), w.Lease)
	if !errors.Is(err, ErrNoWork) || len(in.ProtocolReference) != 0 || in.Work.Lease.MediaID != "" {
		t.Fatalf("elapsed scheduling lease returned input: %#v %v", in.Work, err)
	}
}
