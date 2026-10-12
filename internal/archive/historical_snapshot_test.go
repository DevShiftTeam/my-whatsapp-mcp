//go:build sqlite_fts5

package archive

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A marked copy must never acquire an ordinary writer, even on restart.
func TestHistoricalSnapshotMarkerBlocksOrdinaryOpen(t *testing.T) {
	s := testArchive(t)
	testPolicy(t, s)
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "historical-snapshot.json"), []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(context.Background(), dir, Options{})
	if opened != nil {
		opened.Close()
	}
	if err == nil {
		t.Fatal("ordinary writer opened a historical snapshot")
	}
}

// Query-only opening, searching, expiry checks and denied writes must preserve
// the core database across multiple restarts, including saved authorization.
func TestHistoricalSnapshotReadsWithoutWrites(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	apply(t, s, event("historic", "snapshot canary", 1, EventOriginal), p.Generation)
	createCredential(t, s, "historical", CredentialPolicy{Account: "account", AllChats: true})
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		ro, err := OpenHistoricalSnapshot(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		grant, err := ro.ResolveCredential(ctx, sha256.Sum256([]byte("synthetic-bearer-historical")))
		if err != nil {
			t.Fatal(err)
		}
		page, err := ro.Search(ctx, grant.Scope, SearchRequest{Query: "canary"})
		if err != nil || len(page.Hits) != 1 {
			t.Fatalf("historical search: %v hits=%d", err, len(page.Hits))
		}
		if _, err = ro.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, p.Generation); !errors.Is(err, ErrHistoricalSnapshotReadOnly) {
			t.Fatalf("write admitted: %v", err)
		}
		if _, err = ro.RestoreCommittedSnapshot(ctx, ""); !errors.Is(err, ErrHistoricalSnapshotReadOnly) {
			t.Fatalf("repair admitted: %v", err)
		}
		if _, err = ro.BeginObservation(ctx, "account"); !errors.Is(err, ErrHistoricalSnapshotReadOnly) {
			t.Fatalf("observation admitted: %v", err)
		}
		if _, err = ro.readDB.Exec("DELETE FROM credentials"); err == nil {
			t.Fatal("query-only connection accepted SQL mutation")
		}
		if err = ro.Close(); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(filepath.Join(dir, "archive.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		if sha256.Sum256(before) != sha256.Sum256(after) {
			t.Fatal("snapshot core changed")
		}
		for _, name := range []string{"observation.marker", "unsafe.marker"} {
			if _, err = os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
				t.Fatal("write attempt created safety marker")
			}
		}
	}
}

func TestHistoricalSnapshotUsesCurrentExpiry(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	apply(t, s, event("historic", "snapshot canary", 1, EventOriginal), p.Generation)
	c := createCredential(t, s, "expired", CredentialPolicy{Account: "account", AllChats: true})
	if _, err := s.writeDB.Exec("UPDATE credentials SET expires_at=? WHERE id=?", time.Now().Add(-time.Hour).UnixMilli(), c.ID); err != nil {
		t.Fatal(err)
	}
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenHistoricalSnapshot(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err = ro.ResolveCredential(ctx, sha256.Sum256([]byte("synthetic-bearer-expired"))); err == nil {
		t.Fatal("expired historical grant accepted")
	}
}

func TestHistoricalSnapshotDoesNotFreezeSourceExpiryOrSweep(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	applied := apply(t, s, event("expired-source", "expired canary", 1, EventOriginal), p.Generation)
	if _, err := s.writeDB.Exec("UPDATE messages SET expires_at=? WHERE id=?", time.Now().Add(-time.Hour).UnixMilli(), applied.MessageID); err != nil {
		t.Fatal(err)
	}
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	ro, err := OpenHistoricalSnapshot(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	page, err := ro.Search(ctx, ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "canary"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("historical source expiry frozen: %v", err)
	}
	if err = ro.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "archive.sqlite"))
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("source expiry caused sweep mutation")
	}
}

func TestHistoricalSnapshotRejectsUnsettledSafety(t *testing.T) {
	for _, marker := range []string{"unsafe.marker", "observation.marker", credentialFenceMarker + ".pending"} {
		t.Run(marker, func(t *testing.T) {
			s := testArchive(t)
			p := testPolicy(t, s)
			apply(t, s, event("safe", "canary", 1, EventOriginal), p.Generation)
			dir := s.privateDir
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, marker), []byte("v1\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ro, err := OpenHistoricalSnapshot(context.Background(), dir)
			if ro != nil {
				ro.Close()
			}
			if !errors.Is(err, ErrReconciliationRequired) {
				t.Fatalf("unsafe copy opened: %v", err)
			}
		})
	}
}
