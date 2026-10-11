//go:build sqlite_fts5

package archive

import (
	"context"
	"crypto/sha256"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The injected clock pauses the real extension transaction after its writes
// and before the final commit fence. No test callback runs under safetyMu.
type documentCommitClock struct {
	millis atomic.Int64
	armed  atomic.Bool
	ready  chan struct{}
	resume chan struct{}
	once   sync.Once
}

func (c *documentCommitClock) now() time.Time {
	if pc, _, _, ok := runtime.Caller(2); ok {
		if fn := runtime.FuncForPC(pc); fn != nil && strings.HasSuffix(fn.Name(), ".(*Store).extensionTransaction") && c.armed.CompareAndSwap(true, false) {
			close(c.ready)
			<-c.resume
		}
	}
	return time.UnixMilli(c.millis.Load()).UTC()
}
func (c *documentCommitClock) release() { c.once.Do(func() { close(c.resume) }) }

func documentCredentialRetryFixture(t *testing.T, expiring bool) (*Store, *documentCommitClock, CredentialGrant, MessageID, MediaID) {
	t.Helper()
	clock := &documentCommitClock{ready: make(chan struct{}), resume: make(chan struct{})}
	clock.millis.Store(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC).UnixMilli())
	s, err := Open(context.Background(), privateTestDir(t), Options{Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clock.release(); s.Close() })
	p := testPolicy(t, s)
	r := apply(t, s, documentEvent(t, "credential-document"), p.Generation)
	documentFailDownload(t, s, "account")
	request := CredentialCreate{Label: "Synthetic document client", Principal: "client:document", Digest: sha256.Sum256([]byte("synthetic-document-client")), Policy: CredentialPolicy{Account: "account", AllChats: true}}
	if expiring {
		request.ExpiresAt = time.UnixMilli(clock.millis.Load()).Add(time.Minute)
	}
	credential, err := s.CreateCredential(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := s.ResolveCredentialID(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, clock, grant, r.MessageID, r.MediaIDs[0]
}

func documentAssertRetryUntouched(t *testing.T, s *Store, id MediaID) {
	t.Helper()
	var state string
	var attempts int
	if err := s.readDB.QueryRow(`SELECT state,attempts FROM extension_original_downloads WHERE media_id=?`, id).Scan(&state, &attempts); err != nil || state != "failed" || attempts != 1 {
		t.Fatal("denied credential retry mutated durable download state", state, attempts, err)
	}
	if _, err := s.readSafety("account"); err != nil {
		t.Fatal("optional credential fetch poisoned the archive", err)
	}
}

func TestDocumentCredentialRetryRejectsRevocationBeforeWriterAndScopeBypass(t *testing.T) {
	s, _, grant, id, media := documentCredentialRetryFixture(t, false)
	if _, err := s.RetryDocumentDownloads(context.Background(), grant.Scope, []MessageID{id}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("raw client scope bypassed credential authorization", err)
	}
	if err := s.denyCredentialImmediate(grant.CredentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetryCredentialDocumentDownloads(context.Background(), grant, []MessageID{id}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked captured grant queued a document", err)
	}
	documentAssertRetryUntouched(t, s, media)
}

func TestDocumentCredentialRetryRejectsRevocationAtCommit(t *testing.T) {
	s, clock, grant, id, media := documentCredentialRetryFixture(t, false)
	clock.armed.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := s.RetryCredentialDocumentDownloads(context.Background(), grant, []MessageID{id})
		done <- err
	}()
	select {
	case <-clock.ready:
	case <-time.After(5 * time.Second):
		clock.release()
		t.Fatal("document retry did not reach the final transaction clock")
	}
	if err := s.denyCredentialImmediate(grant.CredentialID); err != nil {
		clock.release()
		t.Fatal(err)
	}
	clock.release()
	if err := <-done; !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revocation between queue writes and commit was not atomic", err)
	}
	documentAssertRetryUntouched(t, s, media)
}

func TestDocumentCredentialRetryRejectsExpiryAtCommit(t *testing.T) {
	s, clock, grant, id, media := documentCredentialRetryFixture(t, true)
	clock.armed.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := s.RetryCredentialDocumentDownloads(context.Background(), grant, []MessageID{id})
		done <- err
	}()
	select {
	case <-clock.ready:
	case <-time.After(5 * time.Second):
		clock.release()
		t.Fatal("document retry did not reach the final transaction clock")
	}
	clock.millis.Add(time.Minute.Milliseconds())
	clock.release()
	if err := <-done; !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired captured grant committed document work", err)
	}
	documentAssertRetryUntouched(t, s, media)
}

func TestDocumentCredentialRetryAuthorizesOnlyCurrentGrant(t *testing.T) {
	s, _, grant, id, media := documentCredentialRetryFixture(t, false)
	forged := grant
	forged.Scope.AllChats = false
	forged.Scope.Chats = []ChatID{"outside-the-grant"}
	if _, err := s.RetryCredentialDocumentDownloads(context.Background(), forged, []MessageID{id}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("altered captured credential scope gained queue authority", err)
	}
	documentAssertRetryUntouched(t, s, media)
	if result, err := s.RetryCredentialDocumentDownloads(context.Background(), grant, []MessageID{id}); err != nil || result.Queued != 1 || result.Skipped != 0 {
		t.Fatal("current authenticated document fetch was unavailable", result, err)
	}
	work, err := s.ClaimOriginalDownload(context.Background(), OriginalDownloadClaim{Account: "account", TTL: time.Minute})
	if err != nil || work.Lease.MediaID != media || work.Kind != MessageDocument || work.Attempts != 1 {
		t.Fatal("authorized retry did not preserve the exact document", err)
	}
	if err = s.CheckCredentialGrant(context.Background(), grant); err != nil {
		t.Fatal("optional fetch changed credential authority", err)
	}
}

func TestDocumentCredentialRetryCancellationAtCommitRollsBack(t *testing.T) {
	s, clock, grant, id, media := documentCredentialRetryFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock.armed.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := s.RetryCredentialDocumentDownloads(ctx, grant, []MessageID{id})
		done <- err
	}()
	select {
	case <-clock.ready:
	case <-time.After(5 * time.Second):
		clock.release()
		t.Fatal("document retry did not reach the final transaction clock")
	}
	cancel()
	clock.release()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled document fetch committed staged work", err)
	}
	documentAssertRetryUntouched(t, s, media)
	if err := s.CheckCredentialGrant(context.Background(), grant); err != nil {
		t.Fatal("cancelled optional fetch denied the client", err)
	}
}
