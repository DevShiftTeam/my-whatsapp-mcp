//go:build sqlite_fts5

package archive

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

func TestExpiryMaintenanceTimeoutKeepsReadableArchiveAndFinalExpiryGuards(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	live := readerEvent("maintenance-live", "maintenance-chat", "readablemaintenance שלום", now.Add(-time.Minute))
	liveID := readerApply(t, s, p, live)
	expiring := readerEvent("maintenance-expiring", "maintenance-chat", "expiredmaintenance content", now.Add(-time.Minute))
	expiring.ExpiresAt = now.Add(time.Minute)
	expiringID := readerApply(t, s, p, expiring)
	credential := createCredential(t, s, "expiry-maintenance", CredentialPolicy{Account: "account", AllChats: true})
	digest := sha256.Sum256([]byte("synthetic-bearer-expiry-maintenance"))
	grant, err := s.ResolveCredential(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	page, expiredGuard, err := s.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "expiredmaintenance"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID != expiringID {
		t.Fatal("expiry fixture did not retain its pre-expiry delivery guard")
	}
	now = expiring.ExpiresAt
	_, metadataGuard, err := s.ListChatsGuarded(context.Background(), grant.Scope, ChatListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := s.readSafety("account")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.writeDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	baseline := s.writeDB.Stats().WaitCount
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err = s.SweepExpired(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || s.writeDB.Stats().WaitCount <= baseline {
		t.Fatal("expiry cleanup did not reach the blocked writer connection before its deadline")
	}
	// The caller can observe its deadline before the actor records its final
	// epoch invalidation. Join that canceled task before creating fresh guards,
	// keeping the writer connection held so physical cleanup cannot run.
	if err = s.Drain(context.Background()); err != nil {
		t.Fatal("canceled expiry cleanup did not settle while the writer connection remained held")
	}
	if _, err = s.readSafety("account"); err != nil {
		t.Fatal("derived expiry cleanup timeout poisoned the readable source archive")
	}
	if after, err := s.readSafety("account"); err != nil || after <= epoch {
		t.Fatal("uncertain expiry cleanup did not invalidate old response snapshots")
	}
	if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: metadataGuard}); err == nil {
		t.Fatal("old aggregate metadata passed after uncertain expiry cleanup")
	}
	if _, err = os.Stat(filepath.Join(s.privateDir, "unsafe.marker")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expiry cleanup timeout created a source safety marker")
	}
	if policy, err := s.SourcePolicy(context.Background(), "account"); err != nil || policy.Generation != p.Generation {
		t.Fatal("expiry cleanup timeout made the source policy unavailable")
	}
	if freshGrant, err := s.ResolveCredential(context.Background(), digest); err != nil || freshGrant.CredentialID != credential.ID || freshGrant.CredentialRevision != credential.Revision {
		t.Fatal("expiry cleanup timeout changed the saved client credential")
	}
	var rawState, rawBody string
	if err = s.readDB.QueryRow("SELECT state,body FROM messages WHERE id=?", expiringID).Scan(&rawState, &rawBody); err != nil || rawState != "active" || rawBody != expiring.Body {
		t.Fatal("held writer did not preserve the physically unscrubbed expired row")
	}
	livePage, liveGuard, err := s.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "readablemaintenance"})
	if err != nil || len(livePage.Hits) != 1 || livePage.Hits[0].MessageID != liveID {
		t.Fatal("readable message was lost after the expiry cleanup timeout")
	}
	if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: liveGuard, Credential: &grant}); err != nil {
		t.Fatal("fresh readable message failed its final delivery check")
	}
	if page, err := s.Context(context.Background(), grant.Scope, ContextRequest{MessageID: liveID}); err != nil || len(page.Messages) != 1 || page.Messages[0].ID != liveID {
		t.Fatal("readable conversation context failed after expiry cleanup timeout")
	}
	if expired, err := s.Search(context.Background(), grant.Scope, SearchRequest{Query: "expiredmaintenance"}); err != nil || len(expired.Hits) != 0 {
		t.Fatal("delayed physical cleanup exposed expired content")
	}
	if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: expiredGuard, Credential: &grant}); !errors.Is(err, ErrSourceUnavailable) && !errors.Is(err, ErrPolicyChanged) {
		t.Fatal("expired pre-read content passed its final delivery check")
	}
	if _, err = s.CurrentSource(context.Background(), expiring.Key); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatal("delayed cleanup retained authority for an expired source")
	}
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = s.SweepExpired(context.Background()); err != nil {
		t.Fatal("fresh bounded expiry cleanup could not resume after the writer became available")
	}
	var state, body string
	if err = s.readDB.QueryRow("SELECT state,body FROM messages WHERE id=?", expiringID).Scan(&state, &body); err != nil || state != "expired" || body != "" {
		t.Fatal("successful retry did not scrub the expired source")
	}
	if current, err := s.CurrentSource(context.Background(), live.Key); err != nil || current.Version.MessageID != liveID {
		t.Fatal("expiry cleanup retry changed an unrelated readable source")
	}
}

func TestExpiryMaintenanceCommitFailureInvalidatesMetadataWithoutSourcePoison(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	live := readerEvent("maintenance-commit-live", "maintenance-chat", "readablecommitmaintenance", now.Add(-time.Minute))
	liveID := readerApply(t, s, p, live)
	expiring := readerEvent("maintenance-commit-expiring", "maintenance-chat", "expiredcommitmaintenance", now.Add(-time.Minute))
	expiring.ExpiresAt = now.Add(time.Minute)
	expiringID := readerApply(t, s, p, expiring)
	scope := readerScope(p, readerChat(t, s, liveID))
	_, metadata, err := s.ListChatsGuarded(context.Background(), scope, ChatListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := s.readSafety("account")
	if err != nil {
		t.Fatal(err)
	}
	now = expiring.ExpiresAt
	conn, err := s.writeDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = conn.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(operation int, first, second, database string) int {
			if operation == sqlite3.SQLITE_TRANSACTION && first == "COMMIT" {
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	})
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	conn.Close()
	defer func() {
		conn, err := s.writeDB.Conn(context.Background())
		if err == nil {
			_ = conn.Raw(func(raw any) error { raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(nil); return nil })
			_ = conn.Close()
		}
	}()
	if err = s.SweepExpired(context.Background()); err != ErrArchiveUnavailable {
		t.Fatal("expiry COMMIT rejection did not preserve the archive error")
	}
	if after, err := s.readSafety("account"); err != nil || after <= epoch {
		t.Fatal("expiry COMMIT error poisoned source safety or retained the old metadata epoch")
	}
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: metadata}); err == nil {
		t.Fatal("old metadata response passed an ambiguous expiry commit")
	}
	var state, body string
	if err = s.readDB.QueryRow("SELECT state,body FROM messages WHERE id=?", expiringID).Scan(&state, &body); err != nil || state != "active" || body != expiring.Body {
		t.Fatal("denied COMMIT did not roll back its partial expiry cleanup")
	}
	if page, err := s.Search(context.Background(), scope, SearchRequest{Query: "readablecommitmaintenance"}); err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID != liveID {
		t.Fatal("unrelated readable source failed after housekeeping COMMIT rejection")
	}
	if page, err := s.Search(context.Background(), scope, SearchRequest{Query: "expiredcommitmaintenance"}); err != nil || len(page.Hits) != 0 {
		t.Fatal("unscrubbed expired content became readable after housekeeping COMMIT rejection")
	}
}

func TestExpiryMaintenanceTimerFailureRemainsBoundedAndRetriesWithCooldown(t *testing.T) {
	s := testArchive(t)
	testPolicy(t, s)
	epoch, err := s.readSafety("account")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := s.writeDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	pending, err := s.runExpiryMaintenance(ctx, "expiry_maintenance_timer")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("timer cleanup did not respect its bounded writer context")
	}
	if after, err := s.readSafety("account"); err != nil || after <= epoch {
		t.Fatal("timer housekeeping failure poisoned source safety or retained its old epoch")
	}
	if delay := expiryMaintenanceDelay(pending, err); delay < time.Second {
		t.Fatal("timer housekeeping failure created a busy retry loop")
	}
	if delay := expiryMaintenanceDelay(true, ErrArchiveUnavailable); delay < time.Second {
		t.Fatal("pending rows bypassed the maintenance error cooldown")
	}
	if delay := expiryMaintenanceDelay(true, nil); delay != time.Millisecond {
		t.Fatal("successful bounded maintenance changed existing batch pacing")
	}
}

func TestExpiryMaintenanceFailureDiagnosticIsClosedAndDoesNotClearUnsafe(t *testing.T) {
	encoded := expiryMaintenanceDiagnostic("SYNTHETIC_PRIVATE_ALIAS", opaqueAliasDiagnosticError{})
	var record map[string]string
	if err := json.Unmarshal(encoded, &record); err != nil || len(record) != 4 || record["event"] != "archive_failure" || record["stage"] != "unknown" || record["code"] != "expiry_cleanup_failed" || record["class"] != "unknown" || strings.Contains(string(encoded), "SYNTHETIC_PRIVATE_ALIAS") {
		t.Fatal("maintenance diagnostic exposed private data or lost its closed schema")
	}
	s := testArchive(t)
	testPolicy(t, s)
	if err := s.MarkUnsafe(context.Background(), "account", "mutation_metadata_missing"); err != nil {
		t.Fatal(err)
	}
	s.recordExpiryMaintenanceFailure("expiry_maintenance_timer", context.DeadlineExceeded)
	if _, err := s.readSafety("account"); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatal("housekeeping failure cleared a pre-existing source safety fence")
	}
}

func TestExpiryMaintenanceSeparationKeepsIncomingDeletionFailClosed(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	original := event("expiry-control-fence", "readable original", 1, EventOriginal)
	apply(t, s, original, p.Generation)
	conn, err := s.writeDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deleted := original
	deleted.Kind, deleted.SourceRevision, deleted.Body = EventDelete, 2, ""
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_, err = s.ApplySourceEvent(ctx, deleted, p.Generation)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("incoming deletion did not encounter the held writer deadline")
	}
	if _, err = s.readSafety(original.Key.Account); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatal("housekeeping separation weakened the incoming deletion safety fence")
	}
}
