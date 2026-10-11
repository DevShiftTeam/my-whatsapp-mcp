//go:build sqlite_fts5

package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func guardedExpectedSource(t *testing.T, s *Store, p ArchivePolicy, event SourceEvent, id MessageID) SourceReadVersion {
	t.Helper()
	metadata, err := s.SourceMetadata(context.Background(), event.Key)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.Context(context.Background(), readerScope(p, readerChat(t, s, id)), ContextRequest{MessageID: id})
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("source metadata context: %#v %v", page, err)
	}
	var hash, chatName, senderName string
	if err = s.readDB.QueryRow(`SELECT m.content_hash,c.name,i.name FROM messages m
 JOIN chats c ON c.id=m.chat_id AND c.account=m.account
 JOIN identities i ON i.id=m.sender_id AND i.account=m.account WHERE m.id=?`, id).Scan(&hash, &chatName, &senderName); err != nil {
		t.Fatal(err)
	}
	metadataHash, err := sourceMetadataHash(chatName, senderName)
	if err != nil {
		t.Fatal(err)
	}
	if len(hash) != 64 || metadata.Version.MessageID != id || !metadata.OriginalDateKnown {
		t.Fatalf("incomplete authoritative source: %#v hash length %d", metadata, len(hash))
	}
	return SourceReadVersion{
		Ref:    SourceRef{Account: event.Key.Account, Version: metadata.Version, ContentHash: hash},
		ChatID: page.Messages[0].ChatID, SenderID: page.Messages[0].SenderID,
		OriginalSentAt: metadata.OriginalSentAt, ExpiresAt: metadata.Version.ExpiresAt, MetadataHash: metadataHash,
	}
}

func guardedJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func guardedBodyRead(t *testing.T, s *Store, scope ReadScope, kind string, event SourceEvent, id MessageID) (any, ResponseGuard, ReadSnapshot) {
	t.Helper()
	if kind == "search" {
		page, guard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "original keyword"})
		want := event.Body
		if event.Caption != "" {
			want += "\n" + event.Caption
		}
		if err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID != id || page.Hits[0].Snippet != want {
			t.Fatalf("guarded FTS body: %#v %v", page, err)
		}
		return page, guard, page.Snapshot
	}
	page, guard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: id})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != id || page.Messages[0].Body != event.Body || page.Messages[0].Caption != event.Caption {
		t.Fatalf("guarded context body: %#v %v", page, err)
	}
	return page, guard, page.Snapshot
}

func guardedAssertSources(t *testing.T, guard ResponseGuard, want ...SourceReadVersion) {
	t.Helper()
	if len(guard.Sources) != len(want) {
		t.Fatalf("guard contributors: got %#v, want %#v", guard.Sources, want)
	}
	actual := make(map[MessageID]SourceReadVersion, len(guard.Sources))
	for _, source := range guard.Sources {
		if _, duplicate := actual[source.Ref.Version.MessageID]; duplicate {
			t.Fatalf("duplicate guarded contributor %q", source.Ref.Version.MessageID)
		}
		actual[source.Ref.Version.MessageID] = source
	}
	for _, source := range want {
		if got, exists := actual[source.Ref.Version.MessageID]; !exists || !reflect.DeepEqual(got, source) {
			t.Fatalf("guard source %q: got %#v, want %#v", source.Ref.Version.MessageID, got, source)
		}
	}
}

func TestGuardedReaderPreservesLegacyPagesAndOriginalSourceSnapshot(t *testing.T) {
	for _, kind := range []string{"search", "context"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			event := readerEvent("original", "allowed", "original keyword שָׁלוֹם", now.Add(-time.Hour))
			event.Caption, event.ExpiresAt = "original caption", now.Add(time.Hour)
			id := readerApply(t, s, p, event)
			scope := readerScope(p, readerChat(t, s, id))
			expected := guardedExpectedSource(t, s, p, event, id)
			page, guard, snapshot := guardedBodyRead(t, s, scope, kind, event, id)
			guardedAssertSources(t, guard, expected)
			if guard.PolicyGeneration != snapshot.PolicyGeneration || guard.indexGeneration != snapshot.IndexGeneration || !guard.minSent.Equal(expected.OriginalSentAt) || !guard.minExpiry.Equal(expected.ExpiresAt) {
				t.Fatalf("guard did not retain its source snapshot: %#v", guard)
			}
			var legacy any
			var err error
			if kind == "search" {
				legacy, err = s.Search(context.Background(), scope, SearchRequest{Query: "original keyword"})
			} else {
				legacy, err = s.Context(context.Background(), scope, ContextRequest{MessageID: id})
			}
			if err != nil || !bytes.Equal(guardedJSON(t, page), guardedJSON(t, legacy)) {
				t.Fatalf("guarded method changed legacy page JSON: %v", err)
			}
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
				t.Fatalf("original guarded snapshot rejected: %v", err)
			}
			missingMetadata := guard
			missingMetadata.Sources = append([]SourceReadVersion(nil), guard.Sources...)
			missingMetadata.Sources[0].MetadataHash = [32]byte{}
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: missingMetadata}); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("body guard accepted an absent display-metadata pin: %v", err)
			}
		})
	}
}

func TestGuardedBodyFinalCheckRejectsPostReadChanges(t *testing.T) {
	for _, kind := range []string{"search", "context"} {
		for _, scenario := range []string{"unrelated_original", "edit", "delete", "expiry", "rolling", "date_scope", "policy", "unsafe"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
				s, p := readerFixture(t, &now)
				event := readerEvent("original", "allowed", "original keyword immutable excerpt", now.Add(-time.Hour+time.Minute))
				if scenario == "expiry" {
					event.ExpiresAt = now.Add(time.Minute)
				}
				id := readerApply(t, s, p, event)
				scope := readerScope(p, readerChat(t, s, id))
				if scenario == "rolling" {
					scope.Rolling = time.Hour
				}
				expected := guardedExpectedSource(t, s, p, event, id)
				page, guard, _ := guardedBodyRead(t, s, scope, kind, event, id)
				guardedAssertSources(t, guard, expected)
				pageBefore, guardBefore := guardedJSON(t, page), guardedJSON(t, guard)
				want := ErrSourceUnavailable
				var err error
				// Every change occurs after the body and its guard have returned.
				switch scenario {
				case "unrelated_original":
					readerApply(t, s, p, readerEvent("unrelated", "other-chat", "new unrelated original", now))
					want = ""
				case "edit":
					event.Kind, event.SourceRevision, event.Body = EventEditContent, 2, "changed original keyword body"
					readerApply(t, s, p, event)
				case "delete":
					event.Kind, event.SourceRevision, event.Body = EventDelete, 2, ""
					readerApply(t, s, p, event)
				case "expiry":
					now = event.ExpiresAt
				case "rolling":
					now = now.Add(2 * time.Minute)
				case "date_scope":
					scope.From = now
					want = ErrUnauthorized
				case "policy":
					_, err = s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateSince, ImportStart: now, ChatMode: ChatsAll}, p.Generation)
					want = ErrPolicyChanged
				case "unsafe":
					err = s.MarkUnsafe(context.Background(), "account", UnsafeReason("mutation_metadata_missing"))
					want = ErrReconciliationRequired
				}
				if err != nil {
					t.Fatal(err)
				}
				_, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard})
				if (want == "" && err != nil) || (want != "" && !errors.Is(err, want)) {
					t.Fatalf("post-read %s check: %v, want %v", scenario, err, want)
				}
				if !bytes.Equal(pageBefore, guardedJSON(t, page)) || !bytes.Equal(guardBefore, guardedJSON(t, guard)) {
					t.Fatal("final check changed the original body or its remembered contributors")
				}
			})
		}
	}
}

func TestGuardedContextIncludesAllowedReplyContributorsOnly(t *testing.T) {
	for _, scenario := range []string{"edit_target", "delete_target", "edit_denied_target"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			allowed := readerEvent("allowed-target", "permitted-target-chat", "allowed original quote", now.Add(-time.Hour))
			denied := readerEvent("denied-target", "denied-target-chat", "DENIED_REPLY_CANARY", now.Add(-time.Hour))
			allowedID := readerApply(t, s, p, allowed)
			deniedID := readerApply(t, s, p, denied)
			center := readerEvent("center", "context-chat", "original keyword center", now.Add(-time.Minute))
			center.ReplyTo = &allowed.Key
			centerID := readerApply(t, s, p, center)
			after := readerEvent("after", "context-chat", "original keyword after", now.Add(-30*time.Second))
			after.ReplyTo = &denied.Key
			afterID := readerApply(t, s, p, after)
			scope := readerScope(p, readerChat(t, s, centerID), readerChat(t, s, allowedID))
			want := []SourceReadVersion{guardedExpectedSource(t, s, p, center, centerID), guardedExpectedSource(t, s, p, after, afterID), guardedExpectedSource(t, s, p, allowed, allowedID)}
			page, guard, err := s.ContextGuarded(context.Background(), scope, ContextRequest{MessageID: centerID, After: 1})
			if err != nil || len(page.Messages) != 2 {
				t.Fatalf("guarded reply context: %#v %v", page, err)
			}
			if page.Messages[0].ReplyTo != allowedID || page.Messages[0].ReplyQuote != allowed.Body || page.Messages[1].ReplyTo != "" || page.Messages[1].ReplyQuote != "" || strings.Contains(string(guardedJSON(t, page)), denied.Body) {
				t.Fatalf("reply authorization failed: %#v", page.Messages)
			}
			guardedAssertSources(t, guard, want...)
			for _, source := range guard.Sources {
				if source.Ref.Version.MessageID == deniedID {
					t.Fatal("denied reply target entered the delivery guard")
				}
			}
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
				t.Fatalf("allowed reply response rejected: %v", err)
			}
			target := allowed
			target.Kind, target.SourceRevision, target.Body = EventEditContent, 2, "edited quote"
			expectedErr := ErrSourceUnavailable
			if scenario == "delete_target" {
				target.Kind, target.Body = EventDelete, ""
			} else if scenario == "edit_denied_target" {
				target = denied
				target.Kind, target.SourceRevision, target.Body = EventEditContent, 2, "edited denied quote"
				expectedErr = ""
			}
			readerApply(t, s, p, target)
			_, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard})
			if (expectedErr == "" && err != nil) || (expectedErr != "" && !errors.Is(err, expectedErr)) {
				t.Fatalf("post-read reply mutation: %v, want %v", err, expectedErr)
			}
		})
	}
}

func TestGuardedReaderFinalCheckRejectsRevokedCredential(t *testing.T) {
	for _, kind := range []string{"search", "context", "list", "status"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			event := readerEvent("original", "allowed", "original keyword credential body", now.Add(-time.Minute))
			id := readerApply(t, s, p, event)
			credential := createCredential(t, s, "guarded-"+kind, CredentialPolicy{Account: "account", Chats: []ChatID{readerChat(t, s, id)}})
			grant, err := s.ResolveCredentialID(context.Background(), credential.ID)
			if err != nil {
				t.Fatal(err)
			}
			var guard ResponseGuard
			if kind == "search" || kind == "context" {
				_, guard, _ = guardedBodyRead(t, s, grant.Scope, kind, event, id)
			} else {
				_, _, guard, _ = guardedMetadataRead(t, s, grant.Scope, kind)
			}
			check := ResponseCheck{Guard: guard, Credential: &grant}
			if _, err = s.CheckResponse(context.Background(), grant.Scope, check); err != nil {
				t.Fatalf("current credential rejected guarded response: %v", err)
			}
			if err = s.RevokeCredential(context.Background(), credential.ID, credential.Revision); err != nil {
				t.Fatal(err)
			}
			if _, err = s.CheckResponse(context.Background(), grant.Scope, check); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("revoked credential retained guarded body authority: %v", err)
			}
		})
	}
}

func guardedMetadataRead(t *testing.T, s *Store, scope ReadScope, kind string) (any, []ChatView, ResponseGuard, ReadSnapshot) {
	t.Helper()
	if kind == "list" {
		page, guard, err := s.ListChatsGuarded(context.Background(), scope, ChatListRequest{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		return page, page.Chats, guard, page.Snapshot
	}
	page, guard, err := s.StatusGuarded(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	return page, page.Chats, guard, page.Snapshot
}

func TestGuardedMetadataKeepsLegacyJSONAndPrivateAggregateAuthority(t *testing.T) {
	for _, kind := range []string{"list", "status"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			event := readerEvent("original", "allowed", "metadata original", now.Add(-time.Minute))
			event.ExpiresAt = now.Add(time.Hour)
			readerApply(t, s, p, event)
			scope := ReadScope{Account: "account", Principal: "owner", PolicyGeneration: p.Generation, AllChats: true}
			page, chats, guard, snapshot := guardedMetadataRead(t, s, scope, kind)
			epoch, err := s.readSafety(scope.Account)
			if err != nil {
				t.Fatal(err)
			}
			if len(chats) != 1 || len(guard.Sources) != 0 || guard.PolicyGeneration != p.Generation || guard.indexGeneration != snapshot.IndexGeneration || guard.safetyEpoch != epoch || !guard.minSent.Equal(event.OriginalSentAt) || !guard.minExpiry.Equal(event.ExpiresAt) {
				t.Fatalf("metadata guard lacks private aggregate authority: %#v", guard)
			}
			var legacy any
			if kind == "list" {
				legacy, err = s.ListChats(context.Background(), scope, ChatListRequest{Limit: 100})
			} else {
				legacy, err = s.Status(context.Background(), scope)
			}
			if err != nil || !bytes.Equal(guardedJSON(t, page), guardedJSON(t, legacy)) {
				t.Fatalf("metadata guard changed legacy page JSON: %v", err)
			}
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
				t.Fatalf("original aggregate guard rejected: %v", err)
			}
			readerApply(t, s, p, readerEvent("unrelated", "unrelated-chat", "new metadata original", now))
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
				t.Fatalf("unrelated original invalidated remembered metadata: %v", err)
			}
			event.Kind, event.SourceRevision, event.Body = EventDelete, 2, ""
			readerApply(t, s, p, event)
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrPolicyChanged) {
				t.Fatalf("metadata guard ignored deletion epoch: %v", err)
			}
		})
	}
}

func TestGuardedMetadataTimeHorizonIncludesSourcesBeyondDisplayedChats(t *testing.T) {
	for _, kind := range []string{"list", "status"} {
		for _, scenario := range []string{"expiry", "scope_rolling", "policy_rolling"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
				s, p := readerFixture(t, &now)
				if scenario == "policy_rolling" {
					var err error
					p, err = s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll, RollingRetention: time.Hour}, p.Generation)
					if err != nil {
						t.Fatal(err)
					}
				}
				var hiddenChat ChatID
				var hiddenSourceChat string
				for n := 0; n < 102; n++ {
					raw := fmt.Sprintf("aggregate-chat-%03d", n)
					id := readerApply(t, s, p, readerEvent(fmt.Sprintf("baseline-%03d", n), raw, "baseline metadata source", now.Add(-time.Minute)))
					chat := readerChat(t, s, id)
					if chat > hiddenChat {
						hiddenChat, hiddenSourceChat = chat, raw
					}
				}
				edge := readerEvent("hidden-edge", hiddenSourceChat, "source outside displayed chats", now.Add(-time.Hour+time.Minute))
				if scenario == "expiry" {
					edge.ExpiresAt = now.Add(time.Minute)
				}
				readerApply(t, s, p, edge)
				scope := ReadScope{Account: "account", Principal: "owner", PolicyGeneration: p.Generation, AllChats: true}
				if scenario == "scope_rolling" {
					scope.Rolling = time.Hour
				}
				page, chats, guard, snapshot := guardedMetadataRead(t, s, scope, kind)
				if len(chats) != 100 || len(guard.Sources) != 0 || guard.indexGeneration != snapshot.IndexGeneration {
					t.Fatalf("aggregate guard truncated to displayed chats: chats=%d snapshot=%#v guard=%#v", len(chats), snapshot, guard)
				}
				for _, chat := range chats {
					if chat.ID == hiddenChat {
						t.Fatal("hidden edge fixture appeared in first 100 chats")
					}
				}
				if kind == "status" && page.(StatusSnapshot).Messages != 103 {
					t.Fatalf("status did not count hidden source: %#v", page)
				}
				if scenario == "expiry" {
					if !guard.minExpiry.Equal(edge.ExpiresAt) {
						t.Fatalf("hidden expiry missing from aggregate guard: %#v", guard)
					}
				} else if guard.rolling != time.Hour || !guard.minSent.Equal(edge.OriginalSentAt) {
					t.Fatalf("hidden source missing from effective rolling horizon: %#v", guard)
				}
				if _, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
					t.Fatalf("current hidden-source horizon rejected: %v", err)
				}
				now = now.Add(2 * time.Minute)
				if _, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
					t.Fatalf("aggregate final check ignored hidden %s horizon: %v", scenario, err)
				}
			})
		}
	}
}

func TestGuardedSearchPinsPublishedContentAndPreservesServerScope(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	displayed := readerEvent("displayed", "allowed", "original keyword", now.Add(-time.Minute))
	lookahead := readerEvent("lookahead", "allowed", "original keyword", now.Add(-2*time.Minute))
	displayedID := readerApply(t, s, p, displayed)
	readerApply(t, s, p, lookahead)
	scope := ReadScope{Account: "account", Principal: "owner", PolicyGeneration: p.Generation, AllChats: true}
	want := guardedExpectedSource(t, s, p, displayed, displayedID)
	page, guard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "original keyword", Limit: 1})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID != displayedID || page.NextCursor == "" {
		t.Fatalf("guarded pagination: %#v %v", page, err)
	}
	// The undisplayed row establishes pagination without contributing plaintext
	// or source authority to the published result's response guard.
	guardedAssertSources(t, guard, want)
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
		t.Fatalf("current paginated response rejected: %v", err)
	}
	lookahead.Kind, lookahead.SourceRevision, lookahead.Body = EventEditContent, 2, "original keyword UNDISPLAYED_LOOKAHEAD_CANARY"
	readerApply(t, s, p, lookahead)
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
		t.Fatalf("undisplayed lookahead revoked unchanged published source: %v", err)
	}
	if next, nextGuard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "original keyword", Limit: 1, Cursor: page.NextCursor}); !errors.Is(err, ErrCursorStale) || !reflect.DeepEqual(next, SearchPage{}) || !reflect.DeepEqual(nextGuard, ResponseGuard{}) {
		t.Fatalf("lookahead mutation preserved stale cursor authority: %#v %#v %v", next, nextGuard, err)
	}
	refreshed, refreshedGuard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "original keyword", Limit: 1})
	if err != nil || len(refreshed.Hits) != 1 || refreshed.Hits[0].MessageID != displayedID || refreshed.Hits[0].Snippet != page.Hits[0].Snippet || strings.Contains(string(guardedJSON(t, refreshed)), "UNDISPLAYED_LOOKAHEAD_CANARY") {
		t.Fatalf("undisplayed text entered the selected result: %#v %v", refreshed, err)
	}
	guardedAssertSources(t, refreshedGuard, want)
	if !reflect.DeepEqual(refreshedGuard.Sources, guard.Sources) {
		t.Fatalf("lookahead changed published-source authority: %#v %#v", refreshedGuard, guard)
	}
	// Client-side narrowing must not replace the authenticated server scope
	// remembered by the guard and used again at final response delivery.
	readerApply(t, s, p, readerEvent("other-chat-canary", "other-chat", "original keyword", now.Add(-time.Minute)))
	readerApply(t, s, p, readerEvent("before-from-canary", "allowed", "original keyword", now.Add(-3*time.Minute)))
	readerApply(t, s, p, readerEvent("at-until-canary", "allowed", "original keyword", now))
	page, guard, err = s.SearchGuarded(context.Background(), scope, SearchRequest{
		Query: "original keyword", Chats: []ChatID{readerChat(t, s, displayedID)},
		From: now.Add(-90 * time.Second), Until: now, Limit: 1,
	})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID != displayedID || page.NextCursor != "" {
		t.Fatalf("narrow guarded request: %#v %v", page, err)
	}
	guardedAssertSources(t, guard, want)
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
		t.Fatalf("narrow request changed server-scope guard identity: %v", err)
	}
}

type guardedReadResult struct {
	page     any
	guard    ResponseGuard
	snapshot ReadSnapshot
	err      error
}

func guardedReadRaw(ctx context.Context, s *Store, scope ReadScope, kind string, id MessageID) guardedReadResult {
	switch kind {
	case "search":
		page, guard, err := s.SearchGuarded(ctx, scope, SearchRequest{Query: "original keyword"})
		return guardedReadResult{page, guard, page.Snapshot, err}
	case "context":
		page, guard, err := s.ContextGuarded(ctx, scope, ContextRequest{MessageID: id})
		return guardedReadResult{page, guard, page.Snapshot, err}
	case "list":
		page, guard, err := s.ListChatsGuarded(ctx, scope, ChatListRequest{Limit: 100})
		return guardedReadResult{page, guard, page.Snapshot, err}
	default:
		page, guard, err := s.StatusGuarded(ctx, scope)
		return guardedReadResult{page, guard, page.Snapshot, err}
	}
}

func guardedEmptyPage(kind string) any {
	switch kind {
	case "search":
		return SearchPage{}
	case "context":
		return ContextPage{}
	case "list":
		return ChatPage{}
	default:
		return StatusSnapshot{}
	}
}

func TestGuardedReadersDiscardPageAndGuardAfterMutationDuringFinalCheck(t *testing.T) {
	for _, kind := range []string{"search", "context", "list", "status"} {
		for _, mutation := range []SourceEventKind{EventEditContent, EventDelete} {
			t.Run(kind+"/"+string(mutation), func(t *testing.T) {
				now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
				s, p := readerFixture(t, &now)
				event := readerEvent("original", "allowed", "original keyword must be discarded", now.Add(-time.Minute))
				id := readerApply(t, s, p, event)
				scope := readerScope(p, readerChat(t, s, id))
				var clockCalls atomic.Int64
				ready, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				s.options.Now = func() time.Time {
					if clockCalls.Add(1) == 2 {
						close(ready)
						<-release
					}
					return now
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				out := make(chan guardedReadResult, 1)
				go func() { out <- guardedReadRaw(ctx, s, scope, kind, id) }()
				select {
				case <-ready:
				case result := <-out:
					t.Fatalf("guarded reader returned before final-check hook: %#v", result)
				case <-time.After(2 * time.Second):
					t.Fatal("guarded reader did not reach final-check hook")
				}
				event.Kind, event.SourceRevision, event.Body = mutation, 2, ""
				if mutation == EventEditContent {
					event.Body = "new body after snapshot"
				}
				readerApply(t, s, p, event)
				unblock()
				result := <-out
				want := ErrSourceUnavailable
				if kind == "list" || kind == "status" {
					want = ErrPolicyChanged
				}
				if !errors.Is(result.err, want) || !reflect.DeepEqual(result.page, guardedEmptyPage(kind)) || !reflect.DeepEqual(result.guard, ResponseGuard{}) {
					t.Fatalf("mutated snapshot exposed a page or newer guard: %#v, want %v and zero outputs", result, want)
				}
			})
		}
	}
}

func guardedActivateIndex(t *testing.T, s *Store, id string) IndexGeneration {
	t.Helper()
	ctx := context.Background()
	generation, err := s.EnsureIndex(ctx, indexTestVersion(id))
	if err != nil {
		t.Fatal(err)
	}
	job := indexTestPublishedJob(t, s, generation)
	if ok, err := s.CommitJob(ctx, indexTestResult(job)); err != nil || !ok {
		t.Fatalf("complete guarded index: %v %v", ok, err)
	}
	if err = s.ActivateIndex(ctx, generation.ID); err != nil {
		t.Fatal(err)
	}
	return generation
}

func TestGuardedReadersBindConfiguredActiveGenerationAndIndexVersion(t *testing.T) {
	for _, kind := range []string{"search", "context", "list", "status"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			event := readerEvent("original", "allowed", "original keyword index source", now.Add(-time.Minute))
			id := readerApply(t, s, p, event)
			scope := readerScope(p, readerChat(t, s, id))
			first := guardedActivateIndex(t, s, "guarded-first")
			result := guardedReadRaw(context.Background(), s, scope, kind, id)
			if result.err != nil || result.guard.Generation != first.ID || result.guard.IndexVersionID != first.Version.ID || result.guard.indexGeneration != result.snapshot.IndexGeneration {
				t.Fatalf("configured generation missing from guarded original snapshot: %#v", result)
			}
			if (kind == "list" || kind == "status") && len(result.guard.Sources) != 0 {
				t.Fatalf("configured metadata guard acquired source contributors: %#v", result.guard)
			}
			if _, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: result.guard}); err != nil {
				t.Fatalf("current configured guard rejected: %v", err)
			}
			second := guardedActivateIndex(t, s, "guarded-second")
			if second.ID == first.ID || second.Version.ID == first.Version.ID {
				t.Fatal("index swap fixture did not create a new generation and version")
			}
			want := ErrCursorStale
			if kind == "list" || kind == "status" {
				want = ErrPolicyChanged // metadata safety epoch is checked first
			}
			if _, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: result.guard}); !errors.Is(err, want) {
				t.Fatalf("configured index swap retained old guard authority: %v, want %v", err, want)
			}
		})
	}
}

func TestGuardedBodyFinalCheckRevalidatesContributingDisplayNames(t *testing.T) {
	for _, kind := range []string{"search", "context"} {
		for _, scenario := range []string{"chat_name", "sender_name", "unrelated_chat_name", "unrelated_sender_name"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
				s, p := readerFixture(t, &now)
				event := readerEvent("original", "allowed", "original keyword named contributor", now.Add(-time.Minute))
				id := readerApply(t, s, p, event)
				originalNames := SourceNames{Account: event.Key.Account, Chat: event.Key.Chat, Sender: event.Key.Sender, ChatName: "Original Chat", SenderName: "Original Sender"}
				if err := s.SetSourceNames(context.Background(), originalNames); err != nil {
					t.Fatal(err)
				}
				other := readerEvent("unrelated", "unrelated-chat", "unrelated original", now)
				other.Key.Sender = "different-private-sender@s.whatsapp.net"
				readerApply(t, s, p, other)
				otherNames := SourceNames{Account: other.Key.Account, Chat: other.Key.Chat, Sender: other.Key.Sender, ChatName: "Other Chat", SenderName: "Other Sender"}
				if err := s.SetSourceNames(context.Background(), otherNames); err != nil {
					t.Fatal(err)
				}
				scope := readerScope(p, readerChat(t, s, id))
				page, guard, _ := guardedBodyRead(t, s, scope, kind, event, id)
				if kind == "search" {
					hit := page.(SearchPage).Hits[0]
					if hit.ChatName != originalNames.ChatName || hit.SenderName != originalNames.SenderName {
						t.Fatalf("original display labels missing from search: %#v", hit)
					}
				} else if page.(ContextPage).Messages[0].SenderName != originalNames.SenderName {
					t.Fatalf("original display label missing from context: %#v", page)
				}
				if _, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
					t.Fatalf("current named contributor rejected: %v", err)
				}
				changed := originalNames
				want := ErrSourceUnavailable
				if strings.HasPrefix(scenario, "unrelated_") {
					changed = otherNames
					want = ""
				}
				if strings.HasSuffix(scenario, "chat_name") {
					changed.ChatName = "Renamed Chat"
				} else {
					changed.SenderName = "Renamed Sender"
				}
				if err := s.SetSourceNames(context.Background(), changed); err != nil {
					t.Fatal(err)
				}
				_, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard})
				if (want == "" && err != nil) || (want != "" && !errors.Is(err, want)) {
					t.Fatalf("post-read %s retained stale display metadata: %v, want %v", scenario, err, want)
				}
			})
		}
	}
}
