//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func savedFixtureReader(t *testing.T, f historicalViewFixture) SavedOwnerRead {
	t.Helper()
	r, err := NewSavedOwnerRead(context.Background(), f.live, f.view(t), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: f.policy.Generation}, f.live)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestSavedViewGuardRejectsCurrentDuplicateArrival(t *testing.T) {
	f := newHistoricalViewFixture(t)
	r := savedFixtureReader(t, f)
	_, guard, err := r.Browse(context.Background(), SavedBrowseRequest{})
	if err != nil {
		t.Fatal(err)
	}
	readerApply(t, f.live, f.policy, readerEvent("history-synthetic-a", "synthetic-a", "changed content", f.now.Add(-2*time.Minute)))
	if err = r.Check(context.Background(), guard); err == nil {
		t.Fatal("new current duplicate failed to suppress historical content")
	}
}
func TestSavedViewGuardAllowsUnrelatedImport(t *testing.T) {
	for _, stats := range []bool{false, true} {
		t.Run(map[bool]string{false: "browse", true: "stats"}[stats], func(t *testing.T) {
			f := newHistoricalViewFixture(t)
			r := savedFixtureReader(t, f)
			var guard SavedResponseGuard
			var err error
			if stats {
				_, guard, err = r.Stats(context.Background(), StatsRequest{})
			} else {
				_, guard, err = r.Browse(context.Background(), SavedBrowseRequest{})
			}
			if err != nil {
				t.Fatal(err)
			}
			readerApply(t, f.live, f.policy, readerEvent("unrelated", "synthetic-a", "fresh unrelated message", f.now.Add(-time.Second)))
			if err = r.Check(context.Background(), guard); err != nil {
				t.Fatalf("unrelated import rejected captured contributors: %v", err)
			}
		})
	}
}
func TestSavedViewOpaqueGuardAndCancelledRequest(t *testing.T) {
	f := newHistoricalViewFixture(t)
	r := savedFixtureReader(t, f)
	if err := r.Check(context.Background(), SavedResponseGuard{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.Browse(ctx, SavedBrowseRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestSavedViewAliasedNegativeSuppressesHistory(t *testing.T) {
	f := newHistoricalViewFixture(t)
	if _, err := f.live.writeDB.Exec("INSERT INTO identity_aliases(account,alias,canonical,provenance) VALUES('account','old-chat','synthetic-a','session_metadata')"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.live.writeDB.Exec("INSERT INTO unresolved_deletions(account,chat,message) VALUES('account','old-chat','history-synthetic-a')"); err != nil {
		t.Fatal(err)
	}
	r := savedFixtureReader(t, f)
	page, _, err := r.Browse(context.Background(), SavedBrowseRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 3 {
		t.Fatalf("aliased unresolved delete bypass: %d", len(page.Hits))
	}
}

func TestSavedViewGuardAllowsNewChatButRejectsPolicyChange(t *testing.T) {
	f := newHistoricalViewFixture(t)
	r := savedFixtureReader(t, f)
	_, guard, err := r.Stats(context.Background(), StatsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	readerApply(t, f.live, f.policy, readerEvent("new-chat-record", "new-authoritative-chat", "new unrelated record", f.now.Add(-time.Second)))
	if err = r.Check(context.Background(), guard); err != nil {
		t.Fatalf("new unrelated chat invalidated captured contributors: %v", err)
	}
	if _, err = f.live.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective, IncludedChats: f.liveChats[:1]}, f.policy.Generation); err != nil {
		t.Fatal(err)
	}
	if err = r.Check(context.Background(), guard); err == nil {
		t.Fatal("policy narrowing did not invalidate response")
	}
}
func TestSavedViewOffPageExpiryAndAliasChange(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(map[bool]string{false: "expiry", true: "alias"}[alias], func(t *testing.T) {
			f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, hp ArchivePolicy) {
				e := readerEvent("expiring", "synthetic-a", "bounded content", f.now.Add(-time.Hour))
				e.ExpiresAt = f.now.Add(time.Minute)
				readerApply(t, h, hp, e)
			})
			r := savedFixtureReader(t, f)
			_, guard, err := r.Stats(context.Background(), StatsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if alias {
				if _, err = f.live.writeDB.Exec("INSERT INTO identity_aliases(account,alias,canonical,provenance) VALUES('account',?,'changed@s.whatsapp.net','session_metadata')", readerEvent("x", "x", "", *f.now).Key.Sender); err != nil {
					t.Fatal(err)
				}
			} else {
				*f.now = f.now.Add(2 * time.Minute)
			}
			if err = r.Check(context.Background(), guard); err == nil {
				t.Fatal("off-page contributor change escaped final guard")
			}
		})
	}
}

func TestSavedViewFinalFenceRechecksHistoricalAbsence(t *testing.T) {
	f := newHistoricalViewFixture(t)
	r := savedFixtureReader(t, f)
	_, guard, err := r.Browse(context.Background(), SavedBrowseRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var samples atomic.Int32
	f.live.options.Now = func() time.Time { samples.Add(1); return *f.now }
	if err = r.Check(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
	last := samples.Load()
	samples.Store(0)
	f.live.options.Now = func() time.Time {
		if samples.Add(1) == last {
			readerApply(t, f.live, f.policy, readerEvent("history-synthetic-a", "synthetic-a", "arrived at final fence", f.now.Add(-2*time.Minute)))
		}
		return *f.now
	}
	if err = r.Check(context.Background(), guard); err == nil {
		t.Fatal("final pre-fence duplicate arrival escaped absence check")
	}
}
func TestSavedViewCurrentOnlyChatAndScope(t *testing.T) {
	f := newHistoricalViewFixture(t)
	id := readerApply(t, f.live, f.policy, readerEvent("new-current-chat", "current-only-chat", "currentonly", f.now.Add(-time.Minute)))
	chat := readerChat(t, f.live, id)
	for _, all := range []bool{true, false} {
		t.Run(map[bool]string{true: "filter", false: "scope"}[all], func(t *testing.T) {
			scope := ReadScope{Account: "account", Principal: "owner", AllChats: all, PolicyGeneration: f.policy.Generation}
			if !all {
				scope.Chats = []ChatID{chat}
			}
			r, err := NewSavedOwnerRead(context.Background(), f.live, f.view(t), scope, f.live)
			if err != nil {
				t.Fatal(err)
			}
			page, _, err := r.Browse(context.Background(), SavedBrowseRequest{Chats: []ChatID{chat}})
			if err != nil || len(page.Hits) != 1 {
				t.Fatalf("current-only chat browse: %d %v", len(page.Hits), err)
			}
			stats, _, err := r.Stats(context.Background(), StatsRequest{ChatID: chat})
			if err != nil || stats.Messages != 1 {
				t.Fatalf("current-only chat Stats: %d %v", stats.Messages, err)
			}
		})
	}
}
func TestSavedViewHistoricalCanonicalCollisionWithheld(t *testing.T) {
	f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, p ArchivePolicy) {
		e := readerEvent("collision", "synthetic-a", "collisionneedle", f.now.Add(-time.Minute))
		e.Key.Sender = "alias-one"
		readerApply(t, h, p, e)
		e.Key.Sender = "alias-two"
		readerApply(t, h, p, e)
		for _, alias := range []string{"alias-one", "alias-two"} {
			if _, err := f.live.writeDB.Exec("INSERT INTO identity_aliases(account,alias,canonical,provenance) VALUES('account',?,'canonical-sender','session_metadata')", alias); err != nil {
				t.Fatal(err)
			}
		}
	})
	r := savedFixtureReader(t, f)
	if _, _, err := r.Browse(context.Background(), SavedBrowseRequest{}); err == nil {
		t.Fatal("browse chose ambiguous historical original")
	}
	if _, _, err := r.Search(context.Background(), SearchRequest{Query: "collisionneedle"}); err == nil {
		t.Fatal("search chose ambiguous historical original")
	}
}

func TestSavedViewInheritedNegativeControls(t *testing.T) {
	for _, kind := range []string{"cutoff", "view_once", "revision", "date"} {
		t.Run(kind, func(t *testing.T) {
			f := newHistoricalViewFixture(t)
			e := readerEvent("history-synthetic-a", "synthetic-a", "", f.now.Add(-2*time.Minute))
			switch kind {
			case "cutoff":
				if _, err := f.live.writeDB.Exec("INSERT INTO chat_tombstones(account,chat,cutoff) VALUES(?,?,?)", e.Key.Account, e.Key.Chat, unixms(e.OriginalSentAt)); err != nil {
					t.Fatal(err)
				}
			case "date":
				if _, err := f.live.writeDB.Exec("INSERT INTO source_anchors(account,chat,sender,message,from_me,original_sent_at,date_known) VALUES(?,?,?,?,?,?,1)", e.Key.Account, e.Key.Chat, e.Key.Sender, e.Key.Message, e.Key.FromMe, unixms(e.OriginalSentAt.Add(-time.Hour))); err != nil {
					t.Fatal(err)
				}
			default:
				if _, err := f.live.writeDB.Exec(`CREATE TABLE rebuild_guards(account TEXT,chat TEXT,sender TEXT,message TEXT,from_me INTEGER,original_sent_at INTEGER,date_known INTEGER,expires_at INTEGER,view_once INTEGER,protocol_revision INTEGER,revision_barrier TEXT,source_kind TEXT,deleted INTEGER)`); err != nil {
					t.Fatal(err)
				}
				revision := 0
				if kind == "revision" {
					revision = 2
				}
				if _, err := f.live.writeDB.Exec("INSERT INTO rebuild_guards VALUES(?,?,?,?,?,0,0,0,?,?,'','original',0)", e.Key.Account, e.Key.Chat, e.Key.Sender, e.Key.Message, e.Key.FromMe, kind == "view_once", revision); err != nil {
					t.Fatal(err)
				}
			}
			r := savedFixtureReader(t, f)
			page, _, err := r.Browse(context.Background(), SavedBrowseRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Hits) != 3 {
				t.Fatalf("negative %s bypassed: %d", kind, len(page.Hits))
			}
			stats, _, err := r.Stats(context.Background(), StatsRequest{})
			if err != nil || stats.Messages != 3 {
				t.Fatalf("negative Stats %s: %d %v", kind, stats.Messages, err)
			}
		})
	}
}
func TestSavedViewHistoricalDerivedAndReplyContributorGuards(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(map[bool]string{false: "reply", true: "multi_source_root"}[multi], func(t *testing.T) {
			var anchor MessageID
			var target SourceKey
			f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, p ArchivePolicy) {
				e := readerEvent("derived-target", "synthetic-a", "quoted contributor", f.now.Add(-3*time.Minute))
				target = e.Key
				id := readerApply(t, h, p, e)
				e = readerEvent("derived-anchor", "synthetic-a", "anchor", f.now.Add(-2*time.Minute))
				e.ReplyTo = &target
				anchor = readerApply(t, h, p, e)
				if multi {
					candidateRoot(t, h, p, "saved-multi-root", "multiplerootneedle", []MessageID{anchor, id}, derivedContextProvenance(DocumentTranscript))
				}
			})
			r := savedFixtureReader(t, f)
			var guard SavedResponseGuard
			var err error
			if multi {
				var page SearchPage
				page, guard, err = r.Search(context.Background(), SearchRequest{Query: "multiplerootneedle"})
				if err == nil && len(page.Hits) != 1 {
					t.Fatal("missing historical-only derived root")
				}
			} else {
				var page ContextPage
				page, guard, err = r.Context(context.Background(), ContextRequest{MessageID: historicalMessageHandle(f, anchor)})
				if err == nil && (len(page.Messages) != 1 || page.Messages[0].ReplyQuote != "quoted contributor") {
					t.Fatal("historical reply target missing")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.live.writeDB.Exec("INSERT INTO unresolved_deletions(account,chat,message) VALUES(?,?,?)", target.Account, target.Chat, target.Message); err != nil {
				t.Fatal(err)
			}
			if err = r.Check(context.Background(), guard); err == nil {
				t.Fatal("deleted off-anchor contributor escaped final guard")
			}
			if multi {
				next := savedFixtureReader(t, f)
				page, _, err := next.Search(context.Background(), SearchRequest{Query: "multiplerootneedle"})
				if err != nil || len(page.Hits) != 0 {
					t.Fatalf("denied multi-root still searchable: %d %v", len(page.Hits), err)
				}
			}
		})
	}
}

func TestSavedViewSingleConnectionConcurrentEndpoints(t *testing.T) {
	f := newHistoricalViewFixture(t)
	f.live.readDB.SetMaxOpenConns(1)
	f.historical.readDB.SetMaxOpenConns(1)
	r := savedFixtureReader(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 4)
	for _, run := range []func() error{
		func() error { _, _, e := r.Stats(ctx, StatsRequest{}); return e },
		func() error { _, _, e := r.Chats(ctx, ChatListRequest{}); return e },
		func() error { _, _, e := r.Search(ctx, SearchRequest{Query: "historical"}); return e },
		func() error { _, _, e := r.Browse(ctx, SavedBrowseRequest{}); return e },
	} {
		go func(run func() error) { <-start; results <- run() }(run)
	}
	close(start)
	for i := 0; i < 4; i++ {
		if e := <-results; e != nil {
			t.Fatal(e)
		}
	}
}

func TestSavedViewEquivalentMultiSourceRoot(t *testing.T) {
	f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, p ArchivePolicy) {
		var liveIDs, oldIDs []MessageID
		for _, key := range []string{"multi-equivalent-a", "multi-equivalent-b"} {
			ev := readerEvent(key, "synthetic-a", "", f.now.Add(-30*time.Second))
			ev.Media = []MediaReference{{Kind: "voice", Availability: "not_retained", ProtocolReference: []byte("same-media")}}
			liveIDs = append(liveIDs, readerApply(t, f.live, f.policy, ev))
			oldIDs = append(oldIDs, readerApply(t, h, p, ev))
		}
		provenance := derivedContextProvenance(DocumentTranscript)
		candidateRoot(t, f.live, f.policy, "live-multi-equivalent", "equivalentrootneedle", liveIDs, provenance)
		candidateRoot(t, h, p, "old-multi-equivalent", "equivalentrootneedle", oldIDs, provenance)
		candidateRoot(t, h, p, "old-multi-distinct", "distinctrootneedle", oldIDs, provenance)
		candidateRoot(t, f.live, f.policy, "live-ordered", "reversedrootneedle", liveIDs, provenance)
		candidateRoot(t, h, p, "old-reversed", "reversedrootneedle", []MessageID{oldIDs[1], oldIDs[0]}, provenance)
	})
	r := savedFixtureReader(t, f)
	page, _, err := r.Search(context.Background(), SearchRequest{Query: "equivalentrootneedle"})
	if err != nil || len(page.Hits) != 1 || strings.HasPrefix(string(page.Hits[0].MessageID), "h1.") {
		t.Fatalf("equivalent multi-source root: %#v %v", page.Hits, err)
	}
	page, _, err = r.Search(context.Background(), SearchRequest{Query: "reversedrootneedle"})
	if err != nil || len(page.Hits) != 2 {
		t.Fatalf("different root anchor suppressed: %#v %v", page.Hits, err)
	}
	page, guard, err := r.Search(context.Background(), SearchRequest{Query: "distinctrootneedle"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("distinct root lost: %#v %v", page.Hits, err)
	}
	if _, err = f.live.writeDB.Exec("INSERT INTO unresolved_deletions(account,chat,message) VALUES('account','synthetic-a','multi-equivalent-b')"); err != nil {
		t.Fatal(err)
	}
	if err = r.Check(context.Background(), guard); err == nil {
		t.Fatal("one denied parent passed final multi-source guard")
	}
	page, _, err = r.Search(context.Background(), SearchRequest{Query: "distinctrootneedle"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("denied multi-source root exposed: %#v %v", page.Hits, err)
	}
}

type savedImportingSearch struct {
	store *Store
	after func()
}

func (s savedImportingSearch) SearchGuarded(ctx context.Context, scope ReadScope, req SearchRequest) (SearchPage, ResponseGuard, error) {
	page, guard, err := s.store.SearchGuarded(ctx, scope, req)
	if err == nil {
		s.after()
	}
	return page, guard, err
}
func TestSavedViewSearchAllowsUnrelatedImport(t *testing.T) {
	f := newHistoricalViewFixture(t)
	search := savedImportingSearch{f.live, func() {
		readerApply(t, f.live, f.policy, readerEvent("search-unrelated", "new-chat", "unrelated", *f.now))
	}}
	r, err := NewSavedOwnerRead(context.Background(), f.live, f.view(t), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: f.policy.Generation}, search)
	if err != nil {
		t.Fatal(err)
	}
	page, _, err := r.Search(context.Background(), SearchRequest{Query: "live"})
	if err != nil || len(page.Hits) != 2 {
		t.Fatalf("unrelated import rejected captured search: %d %v", len(page.Hits), err)
	}
}
func TestSavedViewGuardRejectsNewHistoricalCollision(t *testing.T) {
	f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, p ArchivePolicy) {
		e := readerEvent("new-alias-collision", "synthetic-a", "collisionneedle", f.now.Add(-30*time.Second))
		e.Key.Sender = "canonical-sender"
		readerApply(t, h, p, e)
		e.Key.Sender = "alias-sender"
		readerApply(t, h, p, e)
	})
	r := savedFixtureReader(t, f)
	_, guard, err := r.Search(context.Background(), SearchRequest{Query: "collisionneedle"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.live.writeDB.Exec("INSERT INTO identity_aliases(account,alias,canonical,provenance) VALUES('account','alias-sender','canonical-sender','session_metadata')"); err != nil {
		t.Fatal(err)
	}
	if err = r.Check(context.Background(), guard); err == nil {
		t.Fatal("new historical collision passed final guard")
	}
}

func TestSavedViewContextDerivedPageBudgetPrioritizesCenter(t *testing.T) {
	f := newHistoricalViewFixture(t)
	var center MessageID
	for n := 0; n < 5; n++ {
		ev := readerEvent(fmt.Sprintf("context-budget-%d", n), "synthetic-a", "", f.now.Add(time.Duration(n-10)*time.Second))
		ev.Media = []MediaReference{{Kind: "voice", Availability: "not_retained", ProtocolReference: []byte("synthetic-reference")}}
		center = readerApply(t, f.live, f.policy, ev)
		for j := 0; j < 8; j++ {
			candidateRoot(t, f.live, f.policy, fmt.Sprintf("context-root-%d-%d", n, j), "bounded transcript", []MessageID{center}, derivedContextProvenance(DocumentTranscript))
		}
	}
	r := savedFixtureReader(t, f)
	page, _, err := r.Context(context.Background(), ContextRequest{MessageID: center, Before: 4})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	centerCount := 0
	for _, m := range page.Messages {
		count += len(m.Derived)
		if m.ID == center {
			centerCount = len(m.Derived)
		}
	}
	if count != maxContextDerivedPerPage || centerCount != maxContextDerivedPerMessage {
		t.Fatalf("derived budget/center: total %d center %d", count, centerCount)
	}
}
