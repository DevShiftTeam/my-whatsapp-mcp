//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestUnknownHistoryGapPersistsAndInvalidatesEveryReadGuard(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	e := readerEvent("readable", "allowed", "original keyword", now.Add(-time.Minute))
	id := readerApply(t, s, p, e)
	_, media := attachmentFixture(t, s, p, "attachment", "allowed", "synthetic.png", "attachment", now.Add(-time.Minute))
	scope := readerScope(p, readerChat(t, s, id))
	ctx := context.Background()
	guards := []ResponseGuard{}
	for _, kind := range []string{"search", "context"} {
		_, g, _ := guardedBodyRead(t, s, scope, kind, e, id)
		guards = append(guards, g)
	}
	empty, g, err := s.SearchGuarded(ctx, scope, SearchRequest{Query: "absent"})
	if err != nil {
		t.Fatal(err)
	}
	guards = append(guards, g)
	_, g, err = s.ListChatsGuarded(ctx, scope, ChatListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	guards = append(guards, g)
	_, g, err = s.StatusGuarded(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	guards = append(guards, g)
	_, g, err = s.SearchAttachmentsGuarded(ctx, scope, AttachmentSearchRequest{Filename: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	guards = append(guards, g)
	_, g, err = s.AttachmentGuarded(ctx, scope, media)
	if err != nil {
		t.Fatal(err)
	}
	guards = append(guards, g)
	_, g, err = s.StatsGuarded(ctx, scope, StatsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	guards = append(guards, g)
	beforeRows := snapshotRecoveryRows(t, s)
	if err = s.RecordUnknownHistoryGap(ctx, scope.Account, 229); err != nil {
		t.Fatal("durable unknown gap rejected", err)
	}
	if !reflect.DeepEqual(beforeRows, snapshotRecoveryRows(t, s)) {
		t.Fatal("gap changed stored body or authority")
	}
	for _, guard := range guards {
		if _, err = s.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrCursorStale) && !errors.Is(err, ErrPolicyChanged) {
			t.Fatal("old final guard survived gap", err)
		}
	}
	page, g, err := s.SearchGuarded(ctx, scope, SearchRequest{Query: "original keyword"})
	if err != nil || len(page.Hits) != 1 || !page.Snapshot.UnknownHistoryGap || page.Snapshot.CoverageEpoch != 1 || page.Snapshot.ArchiveRevision <= empty.Snapshot.ArchiveRevision {
		t.Fatal("partial readable snapshot missing", page, err)
	}
	if _, err = s.CheckResponse(ctx, scope, ResponseCheck{Guard: g}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetSyncStatus(ctx, SyncUpdate{Account: scope.Account, Connection: "connected"}); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx, scope)
	if err != nil || status.Coverage != "partial" || !status.Snapshot.UnknownHistoryGap || len(status.Chats) != 1 || status.Chats[0].Coverage != "partial" {
		t.Fatal("later sync cleared gap", status, err)
	}
	if err = s.RecordUnknownHistoryGap(ctx, scope.Account, 229); err != nil {
		t.Fatal(err)
	}
	dir := s.privateDir
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	diagnostics, err := s.UnknownHistoryGapDiagnostics(ctx, scope.Account)
	if err != nil || diagnostics.TotalObservations != 2 || diagnostics.CoverageEpoch != 2 || len(diagnostics.Codes) != 1 || diagnostics.Codes[0].RawStub != 229 || diagnostics.Codes[0].Count != 2 {
		t.Fatal("gap diagnostics did not survive restart", diagnostics, err)
	}
	status, err = s.Status(ctx, scope)
	if err != nil || !status.Snapshot.UnknownHistoryGap {
		t.Fatal("gap lost on restart", err)
	}
}

func TestUnknownHistoryGapDistinctCodeBoundIsAtomic(t *testing.T) {
	s := testArchive(t)
	ctx := context.Background()
	for i := 0; i < MaxUnknownHistoryGapCodes; i++ {
		if err := s.RecordUnknownHistoryGap(ctx, "account", int32(1000+i)); err != nil {
			t.Fatal("gap count rejected", i, err)
		}
	}
	before, err := s.UnknownHistoryGapDiagnostics(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordUnknownHistoryGap(ctx, "account", 2000); err == nil {
		t.Fatal("unbounded new raw code accepted")
	}
	after, err := s.UnknownHistoryGapDiagnostics(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if before.CoverageEpoch != after.CoverageEpoch || before.TotalObservations != after.TotalObservations || len(after.Codes) != 64 {
		t.Fatal("overflow partially committed")
	}
}

func TestUnknownHistoryGapHistoricalLiveEpochIsSeparate(t *testing.T) {
	f := newHistoricalViewFixture(t)
	ctx := context.Background()
	scope := readerScope(f.policy, f.liveChats...)
	v := f.view(t)
	old, err := v.ForOwner(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	_, guard, err := old.SearchGuarded(ctx, scope, SearchRequest{Query: "historical canary"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.live.RecordUnknownHistoryGap(ctx, "account", 229); err != nil {
		t.Fatal(err)
	}
	if _, err = old.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrCursorStale) {
		t.Fatal("historical ticket survived live gap", err)
	}
	current, err := v.ForOwner(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	page, guard, err := current.SearchGuarded(ctx, scope, SearchRequest{Query: "historical canary"})
	if err != nil || !page.Snapshot.UnknownHistoryGap || page.Snapshot.CoverageEpoch != 0 {
		t.Fatal("live gap replaced derivative epoch or vanished", page.Snapshot, err)
	}
	chats, _, err := current.ListChatsGuarded(ctx, scope, ChatListRequest{})
	if err != nil || len(chats.Chats) == 0 {
		t.Fatal("historical chats", err)
	}
	for _, chat := range chats.Chats {
		if chat.Coverage != "partial" {
			t.Fatal("historical live gap not projected into chat coverage")
		}
	}
	snapshot, err := current.CheckResponse(ctx, scope, ResponseCheck{Guard: guard})
	if err != nil || !snapshot.UnknownHistoryGap || snapshot.CoverageEpoch != 0 {
		t.Fatal("historical final label missing live gap", snapshot, err)
	}
	if err = f.live.RecordUnknownHistoryGap(ctx, "account", 229); err != nil {
		t.Fatal(err)
	}
	if _, err = current.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrCursorStale) {
		t.Fatal("second live gap reused ticket", err)
	}
}

func TestUnknownHistoryGapFailureRollsBackAndFences(t *testing.T) {
	s := testArchive(t)
	ctx := context.Background()
	var before ArchiveRevision
	if err := s.writeDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec("CREATE TRIGGER reject_unknown_gap BEFORE INSERT ON unknown_history_codes BEGIN SELECT RAISE(ABORT,'synthetic failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUnknownHistoryGap(ctx, "account", 229); err == nil {
		t.Fatal("failed durable gap accepted")
	}
	var after ArchiveRevision
	var coverage int
	if err := s.writeDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.QueryRow("SELECT count(*) FROM unknown_history_coverage").Scan(&coverage); err != nil {
		t.Fatal(err)
	}
	if before != after || coverage != 0 {
		t.Fatal("failed gap partially committed")
	}
	if _, err := s.readSafety("account"); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatal("failed gap left reads enabled", err)
	}
}

func TestUnknownHistoryGapLegacyReadonlyBridgeDoesNotCreateTables(t *testing.T) {
	for _, version := range []int{14, 15, 16} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
			live, p := readerFixture(t, &now)
			id := readerApply(t, live, p, readerEvent("live-record", "legacy-chat-source", "live body", now.Add(-time.Minute)))
			scope := readerScope(p, readerChat(t, live, id))
			dir := classifiedVersionFixture(t, version, rebuildSourceOrdinary)
			path := filepath.Join(dir, "archive.sqlite")
			db, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("INSERT INTO chats(id,account,source_chat) VALUES('legacy-chat','account','legacy-chat-source')"); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := rebuildFileHash(path)
			if err != nil {
				t.Fatal(err)
			}
			history, err := OpenHistoricalSnapshot(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			defer history.Close()
			history.options.Now = func() time.Time { return now }
			oldScope := readerScope(ArchivePolicy{Generation: 1}, ChatID("legacy-chat"))
			direct, guard, err := history.SearchGuarded(ctx, oldScope, SearchRequest{Query: "absent"})
			if err != nil || direct.Snapshot.UnknownHistoryGap || direct.Snapshot.CoverageEpoch != 0 {
				t.Fatal("legacy reader required new gap schema", direct.Snapshot, err)
			}
			if _, err = history.CheckResponse(ctx, oldScope, ResponseCheck{Guard: guard}); err != nil {
				t.Fatal(err)
			}
			if err = live.RecordUnknownHistoryGap(ctx, "account", 229); err != nil {
				t.Fatal(err)
			}
			view, err := NewHistoricalView(ctx, live, history, HistoricalViewMetadata{Source: "historical", SnapshotID: "0123456789abcdef", CapturedAt: now.Add(-time.Hour), ReadOnly: true, Coverage: "partial", CurrentPhoneState: "unknown"})
			if err != nil {
				t.Fatal(err)
			}
			reader, err := view.ForOwner(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			page, guard, err := reader.SearchGuarded(ctx, scope, SearchRequest{Query: "absent"})
			if err != nil || !page.Snapshot.UnknownHistoryGap || page.Snapshot.CoverageEpoch != 0 {
				t.Fatal("legacy bridge lost live gap or replaced epoch", page.Snapshot, err)
			}
			final, err := reader.CheckResponse(ctx, scope, ResponseCheck{Guard: guard})
			if err != nil || !final.UnknownHistoryGap || final.CoverageEpoch != 0 {
				t.Fatal("legacy final label lost gap", final, err)
			}
			var maxVersion int
			if err = history.readDB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&maxVersion); err != nil || maxVersion != version {
				t.Fatal("legacy reader migrated", maxVersion, err)
			}
			if err = history.Close(); err != nil {
				t.Fatal(err)
			}
			after, err := rebuildFileHash(path)
			if err != nil || before != after {
				t.Fatal("legacy archive bytes changed", err)
			}
		})
	}
}

func TestUnknownHistoryGapOwnerMetadataFinalCheck(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	ctx := context.Background()
	readerApply(t, s, p, readerEvent("first", "owner-inventory", "original keyword", now.Add(-time.Minute)))
	page, err := s.DiscoveryChats(ctx, ChatListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CheckOwnerMetadataSnapshot(ctx, page.Snapshot); err != nil {
		t.Fatal(err)
	}
	readerApply(t, s, p, readerEvent("second", "owner-inventory", "new message", now))
	if err = s.CheckOwnerMetadataSnapshot(ctx, page.Snapshot); !errors.Is(err, ErrCursorStale) {
		t.Fatal("owner metadata accepted changed archive", err)
	}
	page, err = s.DiscoveryChats(ctx, ChatListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordUnknownHistoryGap(ctx, "account", 229); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckOwnerMetadataSnapshot(ctx, page.Snapshot); !errors.Is(err, ErrCursorStale) {
		t.Fatal("owner metadata accepted new gap", err)
	}
	page, err = s.DiscoveryChats(ctx, ChatListRequest{})
	if err != nil || !page.Snapshot.UnknownHistoryGap || len(page.Chats) != 1 || page.Chats[0].Coverage != "partial" {
		t.Fatal("owner inventory gap missing", page, err)
	}
	if err = s.MarkUnsafe(ctx, "account", "history_corrupt"); err != nil {
		t.Fatal(err)
	}
	status, err := s.DiagnosticStatus(ctx)
	if err != nil || !status.ReconciliationRequired {
		t.Fatal("unsafe diagnostics unavailable", status, err)
	}
	if err = s.CheckOwnerMetadataSnapshot(ctx, status.Snapshot); err != nil {
		t.Fatal("owner diagnostic check changed unsafe semantics", err)
	}
}
