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

func TestSavedViewBrowseAttachesDerivedAndGuardsContributors(t *testing.T) {
	var liveVoice, histVoice MessageID
	var histKey SourceKey
	f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, hp ArchivePolicy) {
		evLive := readerEvent("browse-live-voice", "synthetic-a", "", f.now.Add(-10*time.Second))
		evLive.BodyKind = MessageVoice
		evLive.Media = []MediaReference{{Kind: "voice", Availability: "not_retained", ProtocolReference: []byte("live-voice-ref")}}
		liveVoice = readerApply(t, f.live, f.policy, evLive)
		candidateRoot(t, f.live, f.policy, "browse-live-root", "live voice transcript preview", []MessageID{liveVoice}, derivedContextProvenance(DocumentTranscript))

		evHist := readerEvent("browse-hist-voice", "synthetic-a", "", f.now.Add(-20*time.Second))
		evHist.BodyKind = MessageVoice
		evHist.Media = []MediaReference{{Kind: "voice", Availability: "not_retained", ProtocolReference: []byte("hist-voice-ref")}}
		histKey = evHist.Key
		histVoice = readerApply(t, h, hp, evHist)
		candidateRoot(t, h, hp, "browse-hist-root", "historical voice transcript preview", []MessageID{histVoice}, derivedContextProvenance(DocumentTranscript))
	})
	r := savedFixtureReader(t, f)
	page, guard, err := r.Browse(context.Background(), SavedBrowseRequest{})
	if err != nil {
		t.Fatal(err)
	}
	foundLive, foundHist := false, false
	for _, hit := range page.Hits {
		if hit.MessageID == liveVoice && len(hit.Derived) == 1 && hit.Derived[0].Text == "live voice transcript preview" {
			foundLive = true
		}
		if hit.MessageID == historicalMessageHandle(f, histVoice) && len(hit.Derived) == 1 && hit.Derived[0].Text == "historical voice transcript preview" {
			foundHist = true
		}
	}
	if !foundLive || !foundHist {
		t.Fatalf("browse derived excerpts missing: live=%v hist=%v hits=%#v", foundLive, foundHist, page.Hits)
	}
	if err = r.Check(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
	if _, err = f.live.writeDB.Exec("INSERT INTO unresolved_deletions(account,chat,message) VALUES(?,?,?)", histKey.Account, histKey.Chat, histKey.Message); err != nil {
		t.Fatal(err)
	}
	if err = r.Check(context.Background(), guard); err == nil {
		t.Fatal("deleted historical voice contributor passed browse response guard")
	}
}
