//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"testing"
	"time"
)

// These fixtures exercise the read contract after a completed owner recovery.
// The recovery command's durability and audit transitions have separate tests.
func readerRecoveredSnapshot(t *testing.T, s *Store, epoch uint64) {
	t.Helper()
	if _, err := s.writeDB.Exec(`UPDATE archive_meta SET recovery_epoch=?,recovery_code='owner_restored_snapshot' WHERE id=1`, epoch); err != nil {
		t.Fatal(err)
	}
}

func assertReaderRecoverySnapshot(t *testing.T, snapshot ReadSnapshot, epoch uint64) {
	t.Helper()
	if snapshot.RecoveryCode != "owner_restored_snapshot" || snapshot.RecoveryEpoch != epoch {
		t.Fatal("read omitted its recovered snapshot identity")
	}
}

func TestRecoveredSnapshotReadsCarryIdentityAndScopedCoverage(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("readable", "allowed", "synthetic recovery searchable", now.Add(-time.Minute)))
	readerApply(t, s, p, readerEvent("private", "denied", "private recovery canary", now))
	scope := readerScope(p, readerChat(t, s, id))
	readerRecoveredSnapshot(t, s, 3)

	search, searchGuard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "synthetic"})
	if err != nil || len(search.Hits) != 1 || search.Hits[0].MessageID != id {
		t.Fatalf("recovered search lost scoped content: %v", err)
	}
	assertReaderRecoverySnapshot(t, search.Snapshot, 3)
	page, contextGuard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: id})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != id {
		t.Fatalf("recovered context lost scoped content: %v", err)
	}
	assertReaderRecoverySnapshot(t, page.Snapshot, 3)
	chats, chatGuard, err := s.ListChatsGuarded(context.Background(), scope, ChatListRequest{})
	if err != nil || len(chats.Chats) != 1 || chats.Chats[0].ID != scope.Chats[0] || chats.Chats[0].MessageCount != 1 {
		t.Fatalf("recovered chat page broadened its grant: %v", err)
	}
	assertReaderRecoverySnapshot(t, chats.Snapshot, 3)
	if chats.Chats[0].Coverage != "partial" || len(chats.Chats[0].GapCodes) != 1 || chats.Chats[0].GapCodes[0] != "owner_restored_snapshot" {
		t.Fatal("permitted chat concealed interruption coverage")
	}
	status, statusGuard, err := s.StatusGuarded(context.Background(), scope)
	if err != nil || status.ReconciliationRequired || status.Messages != 1 || len(status.Chats) != 1 || status.Coverage != "partial" || len(status.Codes) != 1 || status.Codes[0] != "owner_restored_snapshot" {
		t.Fatalf("restored readable status did not disclose interruption coverage: %v", err)
	}
	assertReaderRecoverySnapshot(t, status.Snapshot, 3)
	for _, guard := range []ResponseGuard{searchGuard, contextGuard, chatGuard, statusGuard} {
		checked, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard})
		if err != nil {
			t.Fatalf("restored read did not retain final response checks: %v", err)
		}
		assertReaderRecoverySnapshot(t, checked, 3)
	}
}

func TestRecoveredSnapshotStatusDisclosesGapForEmptyPermittedScope(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("older", "allowed", "synthetic old recovery source", now.Add(-time.Hour)))
	readerApply(t, s, p, readerEvent("private", "denied", "private recovery source", now))
	scope := readerScope(p, readerChat(t, s, id))
	scope.From = now.Add(-time.Minute)
	readerRecoveredSnapshot(t, s, 1)
	status, err := s.Status(context.Background(), scope)
	if err != nil || status.Messages != 0 || len(status.Chats) != 0 || status.ReconciliationRequired || status.Coverage != "partial" || len(status.Codes) != 1 || status.Codes[0] != "owner_restored_snapshot" {
		t.Fatalf("empty permitted scope omitted the fixed recovery warning or leaked excluded counts: %v", err)
	}
	assertReaderRecoverySnapshot(t, status.Snapshot, 1)
	search, err := s.Search(context.Background(), scope, SearchRequest{Query: "synthetic"})
	if err != nil || len(search.Hits) != 0 {
		t.Fatalf("recovery broadened an empty date scope: %v", err)
	}
	assertReaderRecoverySnapshot(t, search.Snapshot, 1)
}

func TestRecoveryEpochChangeRejectsAlreadySelectedResponses(t *testing.T) {
	for _, kind := range []string{"search", "context", "empty_search", "list", "status"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			event := readerEvent("readable", "allowed", "original keyword synthetic recovery source", now.Add(-time.Minute))
			id := readerApply(t, s, p, event)
			scope := readerScope(p, readerChat(t, s, id))
			var guard ResponseGuard
			if kind == "empty_search" {
				_, guard, _ = s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "absent"})
			} else if kind == "list" || kind == "status" {
				_, _, guard, _ = guardedMetadataRead(t, s, scope, kind)
			} else {
				_, guard, _ = guardedBodyRead(t, s, scope, kind, event, id)
			}
			readerRecoveredSnapshot(t, s, 1)
			if _, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrCursorStale) {
				t.Fatalf("previous recovery identity survived final response checks: %v", err)
			}
		})
	}
}

func TestRecoveryEpochAloneInvalidatesSearchAndChatCursors(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	idA := readerApply(t, s, p, readerEvent("a", "allowed-a", "synthetic recovery source", now.Add(-time.Minute)))
	idB := readerApply(t, s, p, readerEvent("b", "allowed-b", "synthetic recovery source", now))
	scope := readerScope(p, readerChat(t, s, idA), readerChat(t, s, idB))
	search, err := s.Search(context.Background(), scope, SearchRequest{Query: "synthetic", Limit: 1})
	if err != nil || search.NextCursor == "" {
		t.Fatalf("search did not produce a fixture cursor: %v", err)
	}
	chats, err := s.ListChats(context.Background(), scope, ChatListRequest{Limit: 1})
	if err != nil || chats.NextCursor == "" {
		t.Fatalf("chat list did not produce a fixture cursor: %v", err)
	}
	// Leave archive/index revisions unchanged to prove the recovery identity
	// itself prevents a cursor from crossing the restored-snapshot boundary.
	readerRecoveredSnapshot(t, s, 1)
	if _, err := s.Search(context.Background(), scope, SearchRequest{Query: "synthetic", Limit: 1, Cursor: search.NextCursor}); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("search cursor crossed recovery epoch: %v", err)
	}
	if _, err := s.ListChats(context.Background(), scope, ChatListRequest{Limit: 1, Cursor: chats.NextCursor}); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("chat cursor crossed recovery epoch: %v", err)
	}
}

func TestRecoveryEpochChangeBetweenCandidateSelectionAndHydration(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("readable", "allowed", "synthetic recovery source", now.Add(-time.Minute)))
	scope := readerScope(p, readerChat(t, s, id))
	pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: SearchRequest{Query: "synthetic"}, KeywordOnly: true})
	if err != nil || len(pair.Keyword) != 1 {
		t.Fatalf("candidate selection did not produce the fixture source: %v", err)
	}
	readerRecoveredSnapshot(t, s, 1)
	page, err := s.HydrateDocuments(context.Background(), scope, HydrateRequest{Snapshot: pair.Snapshot, Documents: []DocumentRef{pair.Keyword[0].Ref}})
	if !errors.Is(err, ErrCursorStale) || len(page.Documents) != 0 {
		t.Fatalf("hydration reissued a current guard for a previous recovery snapshot: %v", err)
	}
}

func TestRecoveryEpochChangeDuringLegacyReadDiscardsSelectedContent(t *testing.T) {
	for _, kind := range []string{"search", "context", "list", "status"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			id := readerApply(t, s, p, readerEvent("readable", "allowed", "synthetic recovery source", now.Add(-time.Minute)))
			scope := readerScope(p, readerChat(t, s, id))
			calls := 0
			s.options.Now = func() time.Time {
				calls++
				if calls == 2 {
					readerRecoveredSnapshot(t, s, 1)
				}
				return now
			}
			var err error
			switch kind {
			case "search":
				_, err = s.Search(context.Background(), scope, SearchRequest{Query: "synthetic"})
			case "context":
				_, err = s.Context(context.Background(), scope, ContextRequest{MessageID: id})
			case "list":
				_, err = s.ListChats(context.Background(), scope, ChatListRequest{})
			case "status":
				_, err = s.Status(context.Background(), scope)
			}
			if !errors.Is(err, ErrCursorStale) {
				t.Fatalf("selected content crossed a recovery transition before finish: %v", err)
			}
		})
	}
}

func TestRecoveredSnapshotStillWithholdsDeletedAndExpiredSources(t *testing.T) {
	for _, mutation := range []string{"delete", "expiry"} {
		t.Run(mutation, func(t *testing.T) {
			now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			event := readerEvent("readable", "allowed", "original keyword synthetic recovery source", now.Add(-time.Minute))
			if mutation == "expiry" {
				event.ExpiresAt = now.Add(time.Minute)
			}
			id := readerApply(t, s, p, event)
			scope := readerScope(p, readerChat(t, s, id))
			readerRecoveredSnapshot(t, s, 1)
			_, guard, _ := guardedBodyRead(t, s, scope, "context", event, id)
			if mutation == "delete" {
				event.Kind, event.SourceRevision, event.Body = EventDelete, 2, ""
				readerApply(t, s, p, event)
			} else {
				now = event.ExpiresAt
			}
			if _, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("restored snapshot retained %s source authority: %v", mutation, err)
			}
			if _, err := s.Context(context.Background(), scope, ContextRequest{MessageID: id}); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("recovered context exposed a %s source: %v", mutation, err)
			}
		})
	}
}
