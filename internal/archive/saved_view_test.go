//go:build sqlite_fts5

package archive

import (
	"context"
	"testing"
	"time"
)

func TestSavedViewOverlapAndNegatives(t *testing.T) {
	var old, deleted MessageID
	f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, hp ArchivePolicy) {
		at := f.now.Add(-3 * time.Minute)
		old = readerApply(t, h, hp, readerEvent("overlap", "synthetic-a", "oldphrase", at))
		readerApply(t, f.live, f.policy, readerEvent("overlap", "synthetic-a", "newphrase", at))
		deleted = readerApply(t, h, hp, readerEvent("deleted", "synthetic-a", "oldphrase", at))
		e := readerEvent("deleted", "synthetic-a", "", at)
		e.Kind = EventDelete
		e.SourceRevision = 1
		readerApply(t, f.live, f.policy, e)
	})
	scope := ReadScope{Account: "account", Principal: "owner", PolicyGeneration: f.policy.Generation, AllChats: true}
	r, err := NewSavedOwnerRead(context.Background(), f.live, f.view(t), scope, f.live)
	if err != nil {
		t.Fatal(err)
	}
	page, guard, err := r.Browse(context.Background(), SavedBrowseRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 5 {
		t.Fatalf("saved browse=%d, want five unique permitted originals", len(page.Hits))
	}
	if err = r.Check(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
	search, _, err := r.Search(context.Background(), SearchRequest{Query: "oldphrase"})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range search.Hits {
		if hit.MessageID == historicalMessageHandle(f, old) || hit.MessageID == historicalMessageHandle(f, deleted) {
			t.Fatal("historical fallback bypassed current source state")
		}
	}
	stats, _, err := r.Stats(context.Background(), StatsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 5 {
		t.Fatalf("saved count=%d, want 5", stats.Messages)
	}
}
