//go:build sqlite_fts5

package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func namedChatFixture(t *testing.T, s *Store, p ArchivePolicy, source, name string, sent time.Time) ChatID {
	t.Helper()
	id := readerApply(t, s, p, readerEvent(source, source, "synthetic message", sent))
	if err := s.SetSourceNames(context.Background(), SourceNames{Account: "account", Chat: source, ChatName: name}); err != nil {
		t.Fatal(err)
	}
	return readerChat(t, s, id)
}

func allChatScope(p ArchivePolicy) ReadScope {
	return ReadScope{Account: "account", Principal: "owner", PolicyGeneration: p.Generation, AllChats: true}
}

func TestChatSearchMatchesNameBeyondFirstPage(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	for n := 0; n < 8; n++ {
		namedChatFixture(t, s, p, fmt.Sprintf("person-%d@s.whatsapp.net", n), "Unrelated contact", now.Add(-time.Hour))
	}
	scope := allChatScope(p)
	all, err := s.ListChats(context.Background(), scope, ChatListRequest{Limit: 100})
	if err != nil || len(all.Chats) != 8 {
		t.Fatalf("fixture chat count %d, %v", len(all.Chats), err)
	}
	want := all.Chats[len(all.Chats)-1].ID
	if _, err := s.writeDB.Exec("UPDATE chats SET name='Noa Levi' WHERE id=?", want); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"noalevi", "NOA LEVI", "Noa-Levi", "Levi"} {
		page, guard, err := s.ListChatsGuarded(context.Background(), scope, ChatListRequest{Query: query, ChatType: "direct", Limit: 1})
		if err != nil || len(page.Chats) != 1 || page.Chats[0].ID != want || page.NextCursor != "" {
			t.Fatalf("query %q: page %#v, %v", query, page, err)
		}
		if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
			t.Fatalf("query %q response guard: %v", query, err)
		}
	}
}

func TestChatSearchFiltersBeforePaginationAndBindsCursor(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	want := map[ChatID]bool{}
	for n := 0; n < 6; n++ {
		kind, name := "s.whatsapp.net", "Archive team unrelated"
		if n%2 == 0 {
			kind, name = "g.us", "Archive Team group"
		}
		id := namedChatFixture(t, s, p, fmt.Sprintf("chat-%d@%s", n, kind), name, now.Add(-time.Hour))
		if n%2 == 0 {
			want[id] = true
		}
	}
	scope := allChatScope(p)
	request := ChatListRequest{Query: "archiveteam", ChatType: "group", Limit: 1}
	first, err := s.ListChats(context.Background(), scope, request)
	if err != nil || len(first.Chats) != 1 || first.NextCursor == "" {
		t.Fatalf("first filtered page %#v, %v", first, err)
	}
	seen := map[ChatID]bool{}
	page := first
	for {
		if len(page.Chats) != 1 || !want[page.Chats[0].ID] || seen[page.Chats[0].ID] {
			t.Fatalf("filtered page has a duplicate or wrong chat: %#v", page)
		}
		seen[page.Chats[0].ID] = true
		if page.NextCursor == "" {
			break
		}
		request.Cursor = page.NextCursor
		page, err = s.ListChats(context.Background(), scope, request)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("filtered pagination found %d of %d chats", len(seen), len(want))
	}
	for _, changed := range []ChatListRequest{
		{Query: "unrelated", ChatType: "group", Limit: 1, Cursor: first.NextCursor},
		{Query: "archiveteam", ChatType: "direct", Limit: 1, Cursor: first.NextCursor},
		{Query: "archiveteam", ChatType: "all", Limit: 1, Cursor: first.NextCursor},
	} {
		if _, err = s.ListChats(context.Background(), scope, changed); !errors.Is(err, ErrCursorStale) {
			t.Fatalf("cursor reused with changed query/kind: %v", err)
		}
	}
	canonical := ChatListRequest{Query: "ARCHIVE-TEAM", ChatType: "group", Limit: 1, Cursor: first.NextCursor}
	if page, err = s.ListChats(context.Background(), scope, canonical); err != nil || len(page.Chats) != 1 {
		t.Fatalf("equivalent normalized query could not resume: %#v, %v", page, err)
	}
}

func TestChatSearchHebrewNamesAndExactChatTypes(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	phone := namedChatFixture(t, s, p, "hebrew@s.whatsapp.net", "נוֹעָה לֵוִי", now.Add(-time.Hour))
	lid := namedChatFixture(t, s, p, "hebrew@lid", "נועה לוי", now.Add(-time.Hour))
	group := namedChatFixture(t, s, p, "hebrew@g.us", "נועה לוי", now.Add(-time.Hour))
	for _, source := range []string{"status@broadcast", "hebrew@newsletter", "hebrew@g.us.extra", "hebrew", "hebrew@unknown"} {
		namedChatFixture(t, s, p, source, "נועה לוי", now.Add(-time.Hour))
	}
	scope := allChatScope(p)
	for _, tc := range []struct {
		kind string
		want map[ChatID]bool
	}{
		{"direct", map[ChatID]bool{phone: true, lid: true}},
		{"group", map[ChatID]bool{group: true}},
	} {
		for _, query := range []string{"נועהלוי", "נוֹעָה לוי"} {
			page, err := s.ListChats(context.Background(), scope, ChatListRequest{Query: query, ChatType: tc.kind})
			if err != nil || len(page.Chats) != len(tc.want) {
				t.Fatalf("%s Hebrew search: %#v, %v", tc.kind, page, err)
			}
			for _, chat := range page.Chats {
				if !tc.want[chat.ID] {
					t.Fatalf("%s exposed an unrelated chat %#v", tc.kind, chat)
				}
			}
		}
	}
}

func TestChatSearchDoesNotExposeDeniedOrExpiredMetadata(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	allowed := namedChatFixture(t, s, p, "allowed@g.us", "Shared team", now.Add(-time.Hour))
	denied := namedChatFixture(t, s, p, "denied@g.us", "Private team canary", now.Add(-time.Hour))
	expired := readerEvent("expired", "expired@g.us", "Expired body canary", now.Add(-time.Hour))
	expired.ExpiresAt = now.Add(time.Minute)
	readerApply(t, s, p, expired)
	if err := s.SetSourceNames(context.Background(), SourceNames{Account: "account", Chat: expired.Key.Chat, ChatName: "Expired team canary"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	scope := readerScope(p, allowed)
	page, err := s.ListChats(context.Background(), scope, ChatListRequest{Query: "team", ChatType: "group", Limit: 1})
	if err != nil || len(page.Chats) != 1 || page.Chats[0].ID != allowed || page.NextCursor != "" {
		t.Fatalf("scoped search %#v, %v", page, err)
	}
	encoded, _ := json.Marshal(page)
	for _, hidden := range []string{"Private", "Expired", string(denied), "@g.us", "@s.whatsapp.net"} {
		if strings.Contains(string(encoded), hidden) {
			t.Fatalf("hidden name or source identifier %q appeared in chat response", hidden)
		}
	}
	page, err = s.ListChats(context.Background(), allChatScope(p), ChatListRequest{Query: "expiredteam", ChatType: "group"})
	if err != nil || len(page.Chats) != 0 || page.NextCursor != "" {
		t.Fatalf("expired metadata appeared in search %#v, %v", page, err)
	}
	page, err = s.ListChats(context.Background(), scope, ChatListRequest{Query: "private", ChatType: "group"})
	if err != nil || len(page.Chats) != 0 {
		t.Fatalf("out-of-scope metadata appeared in search %#v, %v", page, err)
	}
}

func TestChatSearchRejectsInvalidInputAndTreatsSQLAsLiteral(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	namedChatFixture(t, s, p, "valid@s.whatsapp.net", "Normal contact", now.Add(-time.Hour))
	scope := allChatScope(p)
	for _, request := range []ChatListRequest{
		{Query: strings.Repeat("a", 257)},
		{Query: strings.Repeat("ש", 129)},
		{Query: string([]byte{0xff})},
		{Query: "name\x00suffix"},
		{Query: " -- 😀 \u0301 "},
		{Query: "valid", ChatType: "broadcast"},
		{ChatType: "GROUP"},
	} {
		page, guard, err := s.ListChatsGuarded(context.Background(), scope, request)
		if !errors.Is(err, ErrInvalidQuery) || len(page.Chats) != 0 || !reflect.DeepEqual(guard, ResponseGuard{}) {
			t.Fatalf("invalid request %#v returned %#v, %v", request, page, err)
		}
	}
	for _, query := range []string{"' OR 1=1 --", "%", "_"} {
		page, err := s.ListChats(context.Background(), scope, ChatListRequest{Query: query})
		if query == "%" || query == "_" {
			if !errors.Is(err, ErrInvalidQuery) {
				t.Fatalf("separator-only query %q accepted: %v", query, err)
			}
		} else if err != nil || len(page.Chats) != 0 {
			t.Fatalf("SQL query text changed query structure: %#v, %v", page, err)
		}
	}
	for _, kind := range []string{"", "all"} {
		page, err := s.ListChats(context.Background(), scope, ChatListRequest{ChatType: kind})
		if err != nil || len(page.Chats) != 1 {
			t.Fatalf("empty query/type compatibility %#v, %v", page, err)
		}
	}
}
