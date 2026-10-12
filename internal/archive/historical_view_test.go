//go:build sqlite_fts5

package archive

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type historicalViewFixture struct {
	live, historical           *Store
	policy                     ArchivePolicy
	now                        *time.Time
	liveChats, historicalChats []ChatID
	messages                   []MessageID
	meta                       HistoricalViewMetadata
}

func newHistoricalViewFixture(t *testing.T) historicalViewFixture {
	return newHistoricalViewFixtureWith(t, nil)
}

func newHistoricalViewFixtureWith(t *testing.T, setup func(*historicalViewFixture, *Store, ArchivePolicy)) historicalViewFixture {
	t.Helper()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	live, lp := readerFixture(t, &now)
	history, hp := readerFixture(t, &now)
	f := historicalViewFixture{live: live, now: &now, meta: HistoricalViewMetadata{Source: "historical", SnapshotID: "0123456789abcdef", CapturedAt: now.Add(-time.Hour), ReadOnly: true, Coverage: "partial", CurrentPhoneState: "unknown"}}
	for _, chat := range []string{"synthetic-a", "synthetic-b"} {
		liveID := readerApply(t, live, lp, readerEvent("live-"+chat, chat, "live record", now.Add(-time.Minute)))
		f.liveChats = append(f.liveChats, readerChat(t, live, liveID))
		id := readerApply(t, history, hp, readerEvent("history-"+chat, chat, "historical canary", now.Add(-2*time.Minute)))
		f.messages = append(f.messages, id)
		f.historicalChats = append(f.historicalChats, readerChat(t, history, id))
	}
	var err error
	f.policy, err = live.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, lp.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(&f, history, hp)
	}
	dir := history.privateDir
	if err = history.Close(); err != nil {
		t.Fatal(err)
	}
	f.historical, err = OpenHistoricalSnapshot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	// Only these synthetic stores use a controllable security clock.
	f.historical.options.Now = func() time.Time { return now }
	t.Cleanup(func() { f.historical.Close() })
	return f
}

func (f historicalViewFixture) view(t *testing.T) *HistoricalView {
	t.Helper()
	v, err := NewHistoricalView(context.Background(), f.live, f.historical, f.meta)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (f historicalViewFixture) grant(t *testing.T, all bool, chats ...ChatID) (Credential, CredentialGrant) {
	t.Helper()
	c := createCredential(t, f.live, "historical-view", CredentialPolicy{Account: "account", AllChats: all, Chats: chats})
	g, err := f.live.ResolveCredentialID(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c, g
}

func historicalMessageHandle(f historicalViewFixture, id MessageID) MessageID {
	return MessageID("h1." + f.meta.SnapshotID + ".m." + string(id))
}

func TestHistoricalViewNewLiveCredentialReadsWithoutFrozenCredential(t *testing.T) {
	f := newHistoricalViewFixture(t)
	_, grant := f.grant(t, true)
	var count int
	if err := f.historical.readDB.QueryRow("SELECT count(*) FROM credentials").Scan(&count); err != nil || count != 0 {
		t.Fatalf("fixture has frozen credentials: %d %v", count, err)
	}
	r, err := f.view(t).ForCredential(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	page, guard, err := r.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "historical canary"})
	if err != nil || len(page.Hits) != 2 {
		t.Fatalf("current live credential did not authorize historical read: %#v %v", page, err)
	}
	if !reflect.DeepEqual(r.Metadata(), f.meta) {
		t.Fatalf("metadata lost: %#v", r.Metadata())
	}
	for _, hit := range page.Hits {
		if !strings.HasPrefix(string(hit.MessageID), "h1."+f.meta.SnapshotID+".m.") || !strings.HasPrefix(string(hit.ChatID), "h1."+f.meta.SnapshotID+".c.") {
			t.Fatalf("unbound public ids: %#v", hit)
		}
	}
	if guard.PolicyGeneration == grant.Scope.PolicyGeneration {
		t.Fatal("fixture did not exercise distinct store policy generations")
	}
	if _, err = r.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); err != nil {
		t.Fatalf("guard consulted frozen credential authority: %v", err)
	}
	if _, err = f.live.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); err == nil {
		t.Fatal("historical guard became a live guard")
	}
}

func TestHistoricalViewAllChatsMaterializesLiveSelectivePolicyBeforeCountsAndPaging(t *testing.T) {
	f := newHistoricalViewFixture(t)
	var err error
	f.policy, err = f.live.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective, IncludedChats: f.liveChats[:1]}, f.policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	_, grant := f.grant(t, true)
	if !grant.Scope.AllChats {
		t.Fatal("fixture lost critical AllChats case")
	}
	r, err := f.view(t).ForCredential(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	page, _, err := r.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "historical", Limit: 1})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID != historicalMessageHandle(f, f.messages[0]) || page.NextCursor != "" {
		t.Fatalf("selective policy applied after paging: %#v %v", page, err)
	}
	status, _, err := r.StatusGuarded(context.Background(), grant.Scope)
	if err != nil || status.Messages != 1 || len(status.Chats) != 1 || status.Connection != "historical_snapshot" || !status.Paused || status.Coverage != "partial" || status.Rebuild != nil || status.HistoricalSnapshot == nil {
		t.Fatalf("selective status leaked or claimed live progress: %#v %v", status, err)
	}
	stats, _, err := r.StatsGuarded(context.Background(), grant.Scope, StatsRequest{})
	if err != nil || stats.Messages != 1 || stats.Chats != 1 || stats.Breakdown.Total != 1 {
		t.Fatalf("counts widened current policy: %#v %v", stats, err)
	}
}

func TestHistoricalViewCurrentRevocationScopeAndPolicyFences(t *testing.T) {
	for _, mutation := range []string{"revoke", "credential_scope", "live_policy", "unsafe", "owner_policy"} {
		t.Run(mutation, func(t *testing.T) {
			f := newHistoricalViewFixture(t)
			credential, grant := f.grant(t, true)
			v := f.view(t)
			var r HistoricalRead
			var err error
			if mutation == "owner_policy" {
				r, err = v.ForOwner(context.Background(), grant.Scope)
			} else {
				r, err = v.ForCredential(context.Background(), grant)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, guard, err := r.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "historical"})
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "revoke":
				err = f.live.RevokeCredential(context.Background(), credential.ID, credential.Revision)
			case "credential_scope":
				_, err = f.live.SetCredentialPolicy(context.Background(), credential.ID, CredentialPolicy{Account: "account", Chats: f.liveChats[:1]}, credential.Revision)
			case "live_policy", "owner_policy":
				_, err = f.live.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective, IncludedChats: f.liveChats[:1]}, f.policy.Generation)
			case "unsafe":
				err = f.live.MarkUnsafe(context.Background(), "account", "mutation_metadata_missing")
			}
			if err != nil {
				t.Fatal(err)
			}
			check := ResponseCheck{Guard: guard, Credential: &grant}
			if mutation == "owner_policy" {
				check.Credential = nil
			}
			if _, err = r.CheckResponse(context.Background(), grant.Scope, check); err == nil {
				t.Fatal("buffered response survived current authority change")
			}
			if mutation != "owner_policy" {
				if _, err = v.ForCredential(context.Background(), grant); err == nil {
					t.Fatal("stale grant admitted new historical request")
				}
			}
		})
	}
}

func TestHistoricalViewDateAndRollingScopeRemainRelativeAcrossRequests(t *testing.T) {
	f := newHistoricalViewFixture(t)
	credential, err := f.live.CreateCredential(context.Background(), CredentialCreate{Label: "Rolling", Principal: "client:rolling", Digest: sha256.Sum256([]byte("synthetic-history-rolling")), Policy: CredentialPolicy{Account: "account", AllChats: true, Rolling: 3 * time.Minute}, ExpiresAt: f.now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := f.live.ResolveCredentialID(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := f.view(t)
	r, err := v.ForCredential(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	page, guard, err := r.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "historical", Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("rolling page: %#v %v", page, err)
	}
	*f.now = f.now.Add(10 * time.Second)
	r2, err := v.ForCredential(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	if next, _, err := r2.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "historical", Limit: 1, Cursor: page.NextCursor}); err != nil || len(next.Hits) != 1 {
		t.Fatalf("relative unchanged scope invalidated normal cursor: %#v %v", next, err)
	}
	*f.now = f.now.Add(time.Minute)
	if _, err = r.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("rolling final delivery used captured clock: %v", err)
	}
}

func TestHistoricalViewForeignHandlesCursorsAndAuthorityAreRejected(t *testing.T) {
	f := newHistoricalViewFixture(t)
	_, grant := f.grant(t, true)
	v := f.view(t)
	r, err := v.ForCredential(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	page, guard, err := r.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "historical", Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("cursor fixture: %#v %v", page, err)
	}
	for _, id := range []MessageID{f.messages[0], MessageID(strings.Replace(string(page.Hits[0].MessageID), f.meta.SnapshotID, "fedcba9876543210", 1)), MessageID(strings.Replace(string(page.Hits[0].MessageID), ".m.", ".c.", 1))} {
		if _, _, err = r.ContextGuarded(context.Background(), grant.Scope, ContextRequest{MessageID: id}); err == nil {
			t.Fatalf("cross-source id admitted: %s", id)
		}
	}
	for _, cursor := range []string{strings.TrimPrefix(page.NextCursor, "h1."+f.meta.SnapshotID+".p."), strings.Replace(page.NextCursor, f.meta.SnapshotID, "fedcba9876543210", 1), "h1." + f.meta.SnapshotID + ".p." + strings.Repeat("a", 2049)} {
		if _, _, err = r.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "historical", Limit: 1, Cursor: cursor}); err == nil {
			t.Fatal("foreign/oversized cursor admitted")
		}
	}
	other := createCredential(t, f.live, "historical-other", CredentialPolicy{Account: "account", AllChats: true})
	otherGrant, err := f.live.ResolveCredentialID(context.Background(), other.ID)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := v.ForCredential(context.Background(), otherGrant)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = r2.SearchGuarded(context.Background(), otherGrant.Scope, SearchRequest{Query: "historical", Limit: 1, Cursor: page.NextCursor}); err == nil {
		t.Fatal("cursor crossed current credentials")
	}
	if _, err = r2.CheckResponse(context.Background(), otherGrant.Scope, ResponseCheck{Guard: guard, Credential: &otherGrant}); err == nil {
		t.Fatal("guard crossed current credentials")
	}
	if _, err = r.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard}); err == nil {
		t.Fatal("remote ticket silently downgraded to owner")
	}
	changed := grant.Scope
	changed.From = f.now.Add(-time.Minute)
	if _, _, err = r.SearchGuarded(context.Background(), changed, SearchRequest{Query: "historical"}); err == nil {
		t.Fatal("request-local reader accepted replacement scope")
	}
}

func TestHistoricalViewReadOnlyBytesAndQueryOnlyAdmission(t *testing.T) {
	f := newHistoricalViewFixture(t)
	before := map[string][]byte{}
	for _, name := range []string{"archive.sqlite", "archive.sqlite-wal", "archive.sqlite-shm"} {
		b, err := os.ReadFile(filepath.Join(f.historical.privateDir, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		before[name] = b
	}
	_, grant := f.grant(t, true)
	r, err := f.view(t).ForCredential(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	page, guard, err := r.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "historical"})
	if err != nil || len(page.Hits) != 2 {
		t.Fatal(err)
	}
	if _, err = r.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); err != nil {
		t.Fatal(err)
	}
	if err = f.historical.Close(); err != nil {
		t.Fatal(err)
	}
	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(f.historical.privateDir, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("historical read changed %s", name)
		}
	}
	if _, err = NewHistoricalView(context.Background(), f.live, f.live, f.meta); err == nil {
		t.Fatal("live writable store admitted as history")
	}
}

func TestHistoricalViewEmptyCurrentScopeUnknownMappingsAndAccountMismatchDeny(t *testing.T) {
	for _, scenario := range []string{"empty_selective", "unknown_scope", "historical_only", "ambiguous_alias", "account"} {
		t.Run(scenario, func(t *testing.T) {
			f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, hp ArchivePolicy) {
				if scenario == "historical_only" {
					readerApply(t, h, hp, readerEvent("unknown", "history-only-chat", "HISTORICAL_ONLY_CANARY", f.now.Add(-time.Minute)))
				}
				if scenario == "ambiguous_alias" {
					if _, err := h.writeDB.Exec("INSERT INTO identity_aliases(account,alias,canonical,provenance) VALUES('account','synthetic-b','synthetic-a','connector_mapping')"); err != nil {
						t.Fatal(err)
					}
				}
			})
			scope := ReadScope{Account: "account", Principal: "owner", PolicyGeneration: f.policy.Generation, AllChats: true}
			if scenario == "empty_selective" {
				p, err := f.live.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective}, f.policy.Generation)
				if err != nil {
					t.Fatal(err)
				}
				scope.PolicyGeneration = p.Generation
			}
			if scenario == "unknown_scope" {
				scope.AllChats, scope.Chats = false, []ChatID{"unknown-opaque-chat"}
			}
			if scenario == "account" {
				scope.Account = "another-account"
			}
			v, err := NewHistoricalView(context.Background(), f.live, f.historical, f.meta)
			if scenario == "ambiguous_alias" {
				if err == nil {
					t.Fatal("ambiguous canonical mapping admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r, err := v.ForOwner(context.Background(), scope)
			if scenario == "historical_only" {
				if err != nil {
					t.Fatal(err)
				}
				status, _, err := r.StatusGuarded(context.Background(), scope)
				if err != nil || status.Messages != 2 {
					t.Fatalf("historical-only rows minted live authority: %#v %v", status, err)
				}
				page, _, err := r.SearchGuarded(context.Background(), scope, SearchRequest{Query: "HISTORICAL_ONLY_CANARY"})
				if err != nil || len(page.Hits) != 0 {
					t.Fatalf("historical-only body exposed: %#v %v", page, err)
				}
			} else if err == nil {
				t.Fatal("empty, foreign or unknown authority admitted")
			}
		})
	}
}

func TestHistoricalViewIntersectDatePoliciesAndRequestBoundHandles(t *testing.T) {
	f := newHistoricalViewFixture(t)
	scope := ReadScope{Account: "account", Principal: "owner", PolicyGeneration: f.policy.Generation, AllChats: true, From: f.now.Add(-90 * time.Second)}
	r, err := f.view(t).ForOwner(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	page, _, err := r.StatusGuarded(context.Background(), scope)
	if err != nil || page.Messages != 0 {
		t.Fatalf("live date scope ignored in historical counts: %#v %v", page, err)
	}
	scope.From, scope.Until = f.now.Add(-time.Minute), f.now.Add(-2*time.Minute)
	if _, err = f.view(t).ForOwner(context.Background(), scope); err == nil {
		t.Fatal("empty date intersection admitted")
	}
	scope.From, scope.Until = time.Time{}, time.Time{}
	r, err = f.view(t).ForOwner(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	chat := ChatID("h1." + f.meta.SnapshotID + ".c." + string(f.historicalChats[0]))
	stats, _, err := r.StatsGuarded(context.Background(), scope, StatsRequest{ChatID: chat, Breakdown: "people"})
	if err != nil || stats.Messages != 1 || len(stats.TopChats) != 1 || !strings.HasPrefix(stats.Breakdown.Rows[0].ID, "h1."+f.meta.SnapshotID+".i.") {
		t.Fatalf("namespaced stats request: %#v %v", stats, err)
	}
	for _, invalid := range []ChatID{f.liveChats[0], f.historicalChats[0], ChatID("h1." + f.meta.SnapshotID + ".c.unknown"), ChatID("h1." + f.meta.SnapshotID + ".m." + string(f.messages[0]))} {
		if _, _, err = r.SearchGuarded(context.Background(), scope, SearchRequest{Query: "historical", Chats: []ChatID{invalid}}); err == nil {
			t.Fatal("historical search accepted live, foreign or wrong-kind chat")
		}
		if _, _, err = r.StatsGuarded(context.Background(), scope, StatsRequest{ChatID: invalid}); err == nil {
			t.Fatal("historical stats accepted live, foreign or wrong-kind chat")
		}
	}
}

func TestHistoricalViewHistoricalPolicyOnlyFurtherRestricts(t *testing.T) {
	f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, hp ArchivePolicy) {
		if _, err := h.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective, IncludedChats: f.historicalChats[:1]}, hp.Generation); err != nil {
			t.Fatal(err)
		}
	})
	_, grant := f.grant(t, true)
	r, err := f.view(t).ForCredential(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	page, _, err := r.StatusGuarded(context.Background(), grant.Scope)
	if err != nil || page.Messages != 1 || len(page.Chats) != 1 {
		t.Fatalf("historical policy widened: %#v %v", page, err)
	}
}

func TestHistoricalViewCredentialAndContributorExpiryAtFinalDelivery(t *testing.T) {
	for _, scenario := range []string{"credential", "source", "reply"} {
		t.Run(scenario, func(t *testing.T) {
			var center MessageID
			f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, hp ArchivePolicy) {
				if scenario == "source" {
					if _, err := h.writeDB.Exec("UPDATE messages SET expires_at=? WHERE id=?", f.now.Add(30*time.Second).UnixMilli(), f.messages[0]); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "reply" {
					target := readerEvent("expiring-reply", "synthetic-b", "expiring quoted text", f.now.Add(-time.Minute))
					target.ExpiresAt = f.now.Add(30 * time.Second)
					readerApply(t, h, hp, target)
					e := readerEvent("center", "synthetic-a", "reply center", f.now.Add(-time.Minute))
					e.ReplyTo = &target.Key
					center = readerApply(t, h, hp, e)
				}
			})
			c, err := f.live.CreateCredential(context.Background(), CredentialCreate{Label: "Expiry", Principal: "client:expiry", Digest: sha256.Sum256([]byte("synthetic-historical-expiry")), Policy: CredentialPolicy{Account: "account", AllChats: true}, ExpiresAt: f.now.Add(30 * time.Second)})
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "credential" {
				if _, err = f.live.writeDB.Exec("UPDATE credentials SET expires_at=0 WHERE id=?", c.ID); err != nil {
					t.Fatal(err)
				}
			}
			grant, err := f.live.ResolveCredentialID(context.Background(), c.ID)
			if err != nil {
				t.Fatal(err)
			}
			r, err := f.view(t).ForCredential(context.Background(), grant)
			if err != nil {
				t.Fatal(err)
			}
			var guard ResponseGuard
			if scenario == "reply" {
				page, g, e := r.ContextGuarded(context.Background(), grant.Scope, ContextRequest{MessageID: historicalMessageHandle(f, center)})
				if e != nil || len(page.Messages) != 1 || page.Messages[0].ReplyTo == "" {
					t.Fatalf("reply fixture: %#v %v", page, e)
				}
				guard = g
			} else {
				_, guard, err = r.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "historical"})
				if err != nil {
					t.Fatal(err)
				}
			}
			*f.now = f.now.Add(31 * time.Second)
			want := ErrSourceUnavailable
			if scenario == "credential" {
				want = ErrUnauthorized
			}
			if _, err = r.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); !errors.Is(err, want) {
				t.Fatalf("%s expiry survived buffered delivery: %v", scenario, err)
			}
		})
	}
}

func TestHistoricalViewRevocationDuringSnapshotReadDiscardsEveryPage(t *testing.T) {
	for _, kind := range []string{"search", "context", "list", "status", "stats"} {
		t.Run(kind, func(t *testing.T) {
			f := newHistoricalViewFixture(t)
			c, grant := f.grant(t, true)
			r, err := f.view(t).ForCredential(context.Background(), grant)
			if err != nil {
				t.Fatal(err)
			}
			ready, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var calls atomic.Int64
			f.historical.options.Now = func() time.Time {
				if calls.Add(1) == 2 {
					close(ready)
					<-release
				}
				return *f.now
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			type result struct {
				page  any
				guard ResponseGuard
				err   error
			}
			out := make(chan result, 1)
			go func() {
				var got result
				switch kind {
				case "search":
					got.page, got.guard, got.err = r.SearchGuarded(ctx, grant.Scope, SearchRequest{Query: "historical"})
				case "context":
					got.page, got.guard, got.err = r.ContextGuarded(ctx, grant.Scope, ContextRequest{MessageID: historicalMessageHandle(f, f.messages[0])})
				case "list":
					got.page, got.guard, got.err = r.ListChatsGuarded(ctx, grant.Scope, ChatListRequest{})
				case "status":
					got.page, got.guard, got.err = r.StatusGuarded(ctx, grant.Scope)
				case "stats":
					got.page, got.guard, got.err = r.StatsGuarded(ctx, grant.Scope, StatsRequest{})
				}
				out <- got
			}()
			select {
			case <-ready:
			case got := <-out:
				t.Fatalf("read skipped final snapshot hook: %#v", got)
			case <-time.After(2 * time.Second):
				t.Fatal("read did not reach snapshot hook")
			}
			if err = f.live.RevokeCredential(context.Background(), c.ID, c.Revision); err != nil {
				t.Fatal(err)
			}
			unblock()
			got := <-out
			var empty any
			switch kind {
			case "search":
				empty = SearchPage{}
			case "context":
				empty = ContextPage{}
			case "list":
				empty = ChatPage{}
			case "status":
				empty = StatusSnapshot{}
			case "stats":
				empty = StatsSnapshot{}
			}
			if !errors.Is(got.err, ErrUnauthorized) || !reflect.DeepEqual(got.page, empty) || !reflect.DeepEqual(got.guard, ResponseGuard{}) {
				t.Fatalf("current revocation exposed buffered historical result: %#v", got)
			}
		})
	}
}

func TestHistoricalViewNestedReplyDerivedAndContributorAuthority(t *testing.T) {
	var center, after, quoted, denied MessageID
	f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, hp ArchivePolicy) {
		quote := readerEvent("quote", "synthetic-a", "permitted quote", f.now.Add(-time.Minute))
		quoted = readerApply(t, h, hp, quote)
		private := readerEvent("denied-quote", "synthetic-b", "DENIED_REPLY_CANARY", f.now.Add(-time.Minute))
		denied = readerApply(t, h, hp, private)
		first := readerEvent("nested-center", "synthetic-a", "nested center", f.now.Add(-30*time.Second))
		first.ReplyTo = &quote.Key
		center = readerApply(t, h, hp, first)
		last := readerEvent("nested-after", "synthetic-a", "nested after", f.now.Add(-20*time.Second))
		last.ReplyTo = &private.Key
		after = readerApply(t, h, hp, last)
		candidateRoot(t, h, hp, "allowed-root", "derived canary retained text", []MessageID{center}, DerivedProvenance{Kind: DocumentOCR, ProfileID: "unchanged-profile", ExtractorVersion: "unchanged-version"})
		candidateRoot(t, h, hp, "mixed-root", "derived canary DENIED_DERIVED_CANARY", []MessageID{center, denied}, DerivedProvenance{})
	})
	_, grant := f.grant(t, false, f.liveChats[0])
	r, err := f.view(t).ForCredential(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	page, guard, err := r.ContextGuarded(context.Background(), grant.Scope, ContextRequest{MessageID: historicalMessageHandle(f, center), After: 1})
	if err != nil || len(page.Messages) != 2 {
		t.Fatalf("nested historical context: %#v %v", page, err)
	}
	if page.Messages[0].ID != historicalMessageHandle(f, center) || page.Messages[1].ID != historicalMessageHandle(f, after) || page.Messages[0].ReplyTo != historicalMessageHandle(f, quoted) || page.Messages[0].ReplyQuote != "permitted quote" || page.Messages[1].ReplyTo != "" || page.Messages[1].ReplyQuote != "" {
		t.Fatalf("reply namespace or current contributor scope lost: %#v", page)
	}
	encoded := string(guardedJSON(t, page))
	if strings.Contains(encoded, "DENIED_REPLY_CANARY") || strings.Contains(encoded, "DENIED_DERIVED_CANARY") {
		t.Fatal("denied nested contributor exposed historical text")
	}
	for _, message := range page.Messages {
		if !strings.HasPrefix(message.SenderID, "h1."+f.meta.SnapshotID+".i.") || message.MediaAvailability != "unavailable" {
			t.Fatalf("identity/media namespace lost: %#v", message)
		}
	}
	if len(page.Messages[0].Derived) != 1 {
		t.Fatalf("retained derived text lost: %#v", page)
	}
	derived := page.Messages[0].Derived[0]
	if !strings.HasPrefix(string(derived.DocumentID), "h1."+f.meta.SnapshotID+".d.") || derived.Source.MessageID != historicalMessageHandle(f, center) || !strings.HasPrefix(string(derived.Provenance.MediaID), "h1."+f.meta.SnapshotID+".a.") || derived.Provenance.ProfileID != "unchanged-profile" || derived.Provenance.ExtractorVersion != "unchanged-version" {
		t.Fatalf("derived namespace mutated labels or missed public ids: %#v", derived)
	}
	seenQuote := false
	for _, source := range guard.Sources {
		if source.Ref.Version.MessageID == denied {
			t.Fatal("denied quote entered internal final guard")
		}
		if IsHistoricalHandle(string(source.Ref.Version.MessageID)) {
			t.Fatal("public namespace mutated internal guard")
		}
		if source.Ref.Version.MessageID == quoted {
			seenQuote = true
		}
	}
	if !seenQuote {
		t.Fatal("permitted reply contributor lost its guard")
	}
	if _, err = r.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); err != nil {
		t.Fatal(err)
	}
	search, _, err := r.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "derived canary"})
	if err != nil || len(search.Hits) != 1 || strings.Contains(string(guardedJSON(t, search)), "DENIED_DERIVED_CANARY") {
		t.Fatalf("partial derived contributor guard leaked keyword result: %#v %v", search, err)
	}
	for _, source := range search.Hits[0].Sources {
		if !IsHistoricalHandle(string(source.MessageID)) {
			t.Fatal("search source version id was not namespaced")
		}
	}
}

func TestHistoricalViewSharedCursorKeyDoesNotPermitSourceOrViewReplay(t *testing.T) {
	f := newHistoricalViewFixture(t)
	_, grant := f.grant(t, true)
	v := f.view(t)
	r, err := v.ForCredential(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	page, guard, err := r.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "historical", Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatalf("view cursor fixture: %#v %v", page, err)
	}
	meta := f.meta
	meta.SnapshotID = "fedcba9876543210"
	v2, err := NewHistoricalView(context.Background(), f.live, f.historical, meta)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := v2.ForCredential(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	forged := strings.Replace(page.NextCursor, f.meta.SnapshotID, meta.SnapshotID, 1)
	if _, _, err = r2.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "historical", Limit: 1, Cursor: forged}); err == nil {
		t.Fatal("view-swapped envelope admitted authenticated cursor with copied key")
	}
	if _, err = r2.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); err == nil {
		t.Fatal("guard crossed historical views with same source store")
	}
	liveScope := grant.Scope
	liveScope.Principal = "live cursor owner"
	f.live.cursorKey = append([]byte(nil), f.historical.cursorKey...)
	inner := strings.TrimPrefix(page.NextCursor, "h1."+f.meta.SnapshotID+".p.")
	if _, err = f.live.Search(context.Background(), liveScope, SearchRequest{Query: "historical", Limit: 1, Cursor: inner}); err == nil {
		t.Fatal("historical inner cursor crossed live source after copied key")
	}
}

func TestHistoricalViewValidatesTrustedMetadataAndDistinctAccount(t *testing.T) {
	f := newHistoricalViewFixture(t)
	for _, mutate := range []func(*HistoricalViewMetadata){
		func(m *HistoricalViewMetadata) { m.Source = "live" },
		func(m *HistoricalViewMetadata) { m.SnapshotID = "../private/path" },
		func(m *HistoricalViewMetadata) { m.SnapshotID = "ABCDEF0123456789" },
		func(m *HistoricalViewMetadata) { m.CapturedAt = time.Time{} },
		func(m *HistoricalViewMetadata) { m.CapturedAt = f.now.Add(time.Hour) },
		func(m *HistoricalViewMetadata) { m.ReadOnly = false },
		func(m *HistoricalViewMetadata) { m.Coverage = "complete" },
		func(m *HistoricalViewMetadata) { m.CurrentPhoneState = "verified" },
	} {
		meta := f.meta
		mutate(&meta)
		if _, err := NewHistoricalView(context.Background(), f.live, f.historical, meta); err == nil {
			t.Fatalf("invalid trusted metadata admitted: %#v", meta)
		}
	}
	other, policy := readerFixture(t, f.now)
	e := readerEvent("foreign", "foreign-chat", "foreign account", f.now.Add(-time.Minute))
	e.Key.Account = "other-account"
	readerApply(t, other, policy, e)
	if _, err := NewHistoricalView(context.Background(), other, f.historical, f.meta); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("account mismatch admitted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewHistoricalView(ctx, f.live, f.historical, f.meta); !errors.Is(err, context.Canceled) {
		t.Fatalf("constructor ignored cancellation: %v", err)
	}
}

func TestHistoricalViewFrozenValidCredentialCannotSurviveCurrentRevocation(t *testing.T) {
	var credential Credential
	f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, hp ArchivePolicy) {
		credential = createCredential(t, f.live, "frozen-existing", CredentialPolicy{Account: "account", AllChats: true})
		old := createCredential(t, h, "frozen-existing", CredentialPolicy{Account: "account", AllChats: true})
		if _, err := h.writeDB.Exec("UPDATE credentials SET id=? WHERE id=?", credential.ID, old.ID); err != nil {
			t.Fatal(err)
		}
	})
	grant, err := f.live.ResolveCredentialID(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	v := f.view(t)
	r, err := v.ForCredential(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	_, guard, err := r.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "historical"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.live.RevokeCredential(context.Background(), credential.ID, credential.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = f.historical.ResolveCredentialID(context.Background(), credential.ID); err != nil {
		t.Fatalf("fixture frozen credential is not independently valid: %v", err)
	}
	if _, err = v.ForCredential(context.Background(), grant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("frozen credential admitted request despite current revocation: %v", err)
	}
	if _, err = r.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("frozen credential admitted buffered delivery: %v", err)
	}
}
