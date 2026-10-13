//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSavedViewBrowsePaginationAndTampering(t *testing.T) {
	f := newHistoricalViewFixture(t)
	r := savedFixtureReader(t, f)
	seen := map[MessageID]bool{}
	cursor := ""
	for pageNum := 0; pageNum < 10; pageNum++ {
		page, _, err := r.Browse(context.Background(), SavedBrowseRequest{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range page.Hits {
			if seen[hit.MessageID] {
				t.Fatal("duplicate page boundary")
			}
			seen[hit.MessageID] = true
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 4 {
		t.Fatalf("pagination lost messages: %d", len(seen))
	}
	page, _, err := r.Browse(context.Background(), SavedBrowseRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = r.Browse(context.Background(), SavedBrowseRequest{Limit: 1, Cursor: page.NextCursor + "x"}); !errors.Is(err, ErrSavedCursorStale) {
		t.Fatalf("tamper: %v", err)
	}
	if _, _, err = r.Browse(context.Background(), SavedBrowseRequest{Limit: 2, Cursor: page.NextCursor}); !errors.Is(err, ErrSavedCursorStale) {
		t.Fatalf("different filter: %v", err)
	}
}
func TestSavedViewSearchConsumedPagination(t *testing.T) {
	f := newHistoricalViewFixture(t)
	r := savedFixtureReader(t, f)
	seen := map[MessageID]bool{}
	cursor := ""
	for n := 0; n < 10; n++ {
		page, _, err := r.Search(context.Background(), SearchRequest{Query: "historical", Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range page.Hits {
			if seen[hit.MessageID] {
				t.Fatal("duplicate")
			}
			seen[hit.MessageID] = true
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 2 {
		t.Fatalf("got %d", len(seen))
	}
}

func TestSavedViewBrowseResumesAfterCandidateBudget(t *testing.T) {
	f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, hp ArchivePolicy) {
		seed := readerApply(t, h, hp, readerEvent("withheld-seed", "synthetic-a", "withheld", f.now.Add(-30*time.Second)))
		tx, err := h.writeDB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		stmt, err := tx.Prepare(`INSERT INTO messages(id,account,chat_id,sender_id,source_chat,source_sender,source_message,from_me,original_sent_at,original_date_known,protocol_revision,local_revision,state,source_kind,body_kind,content_hash) SELECT ?,account,chat_id,sender_id,source_chat,source_sender,?,from_me,original_sent_at,original_date_known,protocol_revision,local_revision,state,source_kind,body_kind,content_hash FROM messages WHERE id=?`)
		if err != nil {
			t.Fatal(err)
		}
		defer stmt.Close()
		for i := 0; i < 2001; i++ {
			key := fmt.Sprintf("withheld-%04d", i)
			if _, err = stmt.Exec(key, key, seed); err != nil {
				t.Fatal(err)
			}
			if _, err = f.live.writeDB.Exec("INSERT INTO unresolved_deletions(account,chat,message) VALUES('account','synthetic-a',?)", key); err != nil {
				t.Fatal(err)
			}
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if _, err = f.live.writeDB.Exec("INSERT INTO unresolved_deletions(account,chat,message) VALUES('account','synthetic-a','withheld-seed')"); err != nil {
			t.Fatal(err)
		}
	})
	r := savedFixtureReader(t, f)
	first, _, err := r.Browse(context.Background(), SavedBrowseRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("candidate budget pretended historical exhaustion")
	}
	second, _, err := r.Browse(context.Background(), SavedBrowseRequest{Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Hits)+len(second.Hits) != 4 {
		t.Fatalf("budget continuation lost rows: %d+%d", len(first.Hits), len(second.Hits))
	}
}

func TestSavedViewCursorRejectsCrossScope(t *testing.T) {
	f := newHistoricalViewFixture(t)
	r := savedFixtureReader(t, f)
	page, _, err := r.Browse(context.Background(), SavedBrowseRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewSavedOwnerRead(context.Background(), f.live, f.view(t), ReadScope{Account: "account", Principal: "other-owner", PolicyGeneration: f.policy.Generation, AllChats: true}, f.live)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = other.Browse(context.Background(), SavedBrowseRequest{Limit: 1, Cursor: page.NextCursor}); !errors.Is(err, ErrSavedCursorStale) {
		t.Fatalf("cross-scope cursor: %v", err)
	}
}

func TestSavedViewCandidateFrontierOrdering(t *testing.T) {
	for _, mode := range []string{"same_timestamp_browse", "suppressed_search"} {
		t.Run(mode, func(t *testing.T) {
			var firstLive, secondLive, historicalTail MessageID
			f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, hp ArchivePolicy) {
				at := f.now.Add(-30 * time.Second)
				firstLive = readerApply(t, f.live, f.policy, readerEvent("a-first-live", "synthetic-a", "budgetterm", at))
				secondLive = readerApply(t, f.live, f.policy, readerEvent("b-second-live", "synthetic-a", "budgetterm", at.Add(-time.Second)))
				historicalTail = readerApply(t, h, hp, readerEvent("z-tail", "synthetic-a", "budgetterm", at.Add(-time.Second)))
				seed := readerApply(t, h, hp, readerEvent("z-seed", "synthetic-a", "budgetterm", at))
				tx, err := h.writeDB.Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				for i := 0; i < 2001; i++ {
					key := fmt.Sprintf("z-budget-%04d", i)
					_, err = tx.Exec(`INSERT INTO messages(id,account,chat_id,sender_id,source_chat,source_sender,source_message,from_me,original_sent_at,original_date_known,protocol_revision,local_revision,state,source_kind,body_kind,content_hash,body,normalized_body) SELECT ?,account,chat_id,sender_id,source_chat,source_sender,?,from_me,original_sent_at,original_date_known,protocol_revision,local_revision,state,source_kind,body_kind,content_hash,body,normalized_body FROM messages WHERE id=?`, key, key, seed)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = tx.Exec(`INSERT INTO message_fts(rowid,normalized_body,normalized_caption) SELECT rowid,normalized_body,normalized_caption FROM messages WHERE id=?`, key); err != nil {
						t.Fatal(err)
					}
					if mode == "suppressed_search" {
						if _, err = f.live.writeDB.Exec("INSERT INTO unresolved_deletions(account,chat,message) VALUES('account','synthetic-a',?)", key); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
				if mode == "suppressed_search" {
					if _, err = f.live.writeDB.Exec("INSERT INTO unresolved_deletions(account,chat,message) VALUES('account','synthetic-a','z-seed')"); err != nil {
						t.Fatal(err)
					}
				}
			})
			r := savedFixtureReader(t, f)
			if mode == "same_timestamp_browse" {
				page, _, err := r.Browse(context.Background(), SavedBrowseRequest{Limit: 1})
				if err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID != firstLive {
					t.Fatalf("full-key frontier skipped current first: %#v %v", page.Hits, err)
				}
				return
			}
			var got []MessageID
			cursor := ""
			for i := 0; i < 4; i++ {
				page, _, err := r.Search(context.Background(), SearchRequest{Query: "budgetterm", Cursor: cursor})
				if err != nil {
					t.Fatal(err)
				}
				for _, hit := range page.Hits {
					got = append(got, hit.MessageID)
				}
				cursor = page.NextCursor
				if cursor == "" {
					break
				}
			}
			want := []MessageID{firstLive, historicalMessageHandle(f, historicalTail), secondLive}
			if len(got) != len(want) {
				t.Fatalf("candidate rank continuation: %v want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("candidate rank order: %v want %v", got, want)
				}
			}
		})
	}
}
