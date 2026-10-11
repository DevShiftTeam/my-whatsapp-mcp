//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStatsPeopleBreakdownPagesCanonicalSendersWithinScope(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, policy := readerFixture(t, &now)
	var shared ChatID
	for person := 0; person < 13; person++ {
		for n := 0; n <= person; n++ {
			event := readerEvent(fmt.Sprintf("person-%d-%d", person, n), "shared-group@g.us", "synthetic", now.Add(-time.Duration(person)*time.Minute))
			event.Key.Sender = fmt.Sprintf("private-person-%02d@s.whatsapp.net", person)
			event.Key.FromMe = person == 0
			id := readerApply(t, s, policy, event)
			if shared == "" {
				shared = readerChat(t, s, id)
			}
		}
	}
	other := readerEvent("second-chat", "outside-scope@g.us", "synthetic", now)
	other.Key.Sender = "private-person-12@s.whatsapp.net"
	readerApply(t, s, policy, other)
	if _, err := s.writeDB.Exec(`UPDATE identities SET name=CASE WHEN source_identity='private-person-00@s.whatsapp.net' THEN '' ELSE 'Sender '||substr(source_identity,16,2) END`); err != nil {
		t.Fatal(err)
	}
	scope := readerScope(policy, shared)
	first, err := s.Stats(context.Background(), scope, StatsRequest{Breakdown: "people"})
	if err != nil || first.Messages != 91 || first.Breakdown.Dimension != "people" || first.Breakdown.Total != 13 || first.Breakdown.Page != 1 || first.Breakdown.PageSize != 10 || first.Breakdown.Sort != "messages" || len(first.Breakdown.Rows) != 10 {
		t.Fatalf("first people page: %#v, %v", first, err)
	}
	for n, row := range first.Breakdown.Rows {
		if row.Messages != int64(13-n) || row.Chats != 1 || row.Received != row.Messages || row.Sent != 0 {
			t.Fatalf("group sender count: %#v", row)
		}
	}
	last, err := s.Stats(context.Background(), scope, StatsRequest{Breakdown: "people", Page: 1000000})
	if err != nil || last.Breakdown.Page != 2 || last.Breakdown.Total != 13 || len(last.Breakdown.Rows) != 3 {
		t.Fatalf("clamped page: %#v, %v", last.Breakdown, err)
	}
	self := last.Breakdown.Rows[2]
	if self.Messages != 1 || self.Sent != 1 || self.Received != 0 || self.Name != "" {
		t.Fatalf("self direction: %#v", self)
	}
	encoded, err := json.Marshal(last.Breakdown)
	if err != nil || strings.Contains(string(encoded), "@") || strings.Contains(string(encoded), "private-person") {
		t.Fatal("breakdown contains a private source identity")
	}
	all := scope
	all.AllChats = true
	all.Chats = nil
	allPage, err := s.Stats(context.Background(), all, StatsRequest{Breakdown: "people", PageSize: 25})
	if err != nil || allPage.Breakdown.Total != 13 || allPage.Breakdown.Rows[0].Messages != 14 || allPage.Breakdown.Rows[0].Chats != 2 {
		t.Fatalf("same sender across readable chats: %#v, %v", allPage.Breakdown, err)
	}
	chats, err := s.Stats(context.Background(), all, StatsRequest{})
	if err != nil || chats.Breakdown.Dimension != "chats" || chats.Breakdown.Total != 2 || len(chats.Breakdown.Rows) != 2 || chats.Breakdown.Rows[0].Messages != 91 || chats.Breakdown.Rows[0].Sent != 1 || chats.Breakdown.Rows[0].Received != 90 || chats.Breakdown.Rows[0].Chats != 1 {
		t.Fatalf("chat breakdown: %#v, %v", chats.Breakdown, err)
	}
}

func TestStatsBreakdownSortsAndBoundsUntrustedNames(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, policy := readerFixture(t, &now)
	var ids []ChatID
	for n := 0; n < 3; n++ {
		id := readerApply(t, s, policy, readerEvent(fmt.Sprint(n), fmt.Sprint(n), "synthetic", now.Add(-time.Duration(n)*time.Minute)))
		ids = append(ids, readerChat(t, s, id))
	}
	scope := readerScope(policy, ids...)
	if _, err := s.writeDB.Exec(`UPDATE chats SET name='Same'`); err != nil {
		t.Fatal(err)
	}
	page, err := s.Stats(context.Background(), scope, StatsRequest{Sort: "name"})
	if err != nil {
		t.Fatal(err)
	}
	ordered := append([]ChatID(nil), ids...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for n, row := range page.Breakdown.Rows {
		if row.ID != string(ordered[n]) {
			t.Fatalf("equal-name tie is unstable: %#v", page.Breakdown.Rows)
		}
	}
	page, err = s.Stats(context.Background(), scope, StatsRequest{Sort: "recent"})
	if err != nil || page.Breakdown.Rows[0].ID != string(ids[0]) || page.Breakdown.Rows[2].ID != string(ids[2]) {
		t.Fatalf("recent sort: %#v, %v", page.Breakdown, err)
	}
	if _, err := s.writeDB.Exec(`UPDATE chats SET name=? WHERE id=?`, strings.Repeat("א", 200), ids[0]); err != nil {
		t.Fatal(err)
	}
	page, err = s.Stats(context.Background(), scope, StatsRequest{Sort: "recent"})
	if err != nil || len(page.Breakdown.Rows[0].Name) > 256 || page.Breakdown.Rows[0].Name == "" {
		t.Fatalf("bounded name: %#v, %v", page.Breakdown, err)
	}
}

func TestStatsUTCInsightsAndRhythmFollowAllFilters(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, policy := readerFixture(t, &now)
	times := []string{"2026-10-07T23:59:00Z", "2026-10-08T00:01:00Z", "2026-10-08T12:00:00Z", "2026-10-09T00:00:00Z", "2026-10-09T12:00:00Z", "2026-10-11T00:00:00Z"}
	var chat ChatID
	for n, stamp := range times {
		at, _ := time.Parse(time.RFC3339, stamp)
		id := readerApply(t, s, policy, readerEvent(fmt.Sprint(n), "allowed", "synthetic", at))
		chat = readerChat(t, s, id)
	}
	readerApply(t, s, policy, readerEvent("denied", "denied", "synthetic", now.Add(-24*time.Hour)))
	scope := readerScope(policy, chat)
	page, err := s.Stats(context.Background(), scope, StatsRequest{})
	if err != nil || page.Insights.ActiveDays != 4 || page.Insights.LongestStreakDays != 3 || page.Insights.BusiestDate != "2026-10-08" || page.Insights.BusiestDateMessages != 2 {
		t.Fatalf("UTC highlights: %#v, %v", page.Insights, err)
	}
	var sum int64
	var midnightSunday bool
	for _, slot := range page.Rhythm {
		if slot.Weekday < 0 || slot.Weekday > 6 || slot.Hour < 0 || slot.Hour > 23 {
			t.Fatalf("invalid UTC rhythm slot: %#v", slot)
		}
		sum += slot.Messages
		midnightSunday = midnightSunday || slot.Weekday == 0 && slot.Hour == 0 && slot.Messages == 1
	}
	if sum != page.Messages || !midnightSunday || len(page.Rhythm) > 168 {
		t.Fatalf("UTC rhythm totals: %#v", page.Rhythm)
	}
	from, _ := time.Parse("2006-01-02", "2026-10-09")
	until, _ := time.Parse("2006-01-02", "2026-10-11")
	page, err = s.Stats(context.Background(), scope, StatsRequest{From: from, Until: until})
	if err != nil || page.Messages != 2 || page.Insights.ActiveDays != 1 || page.Insights.LongestStreakDays != 1 || page.Insights.BusiestDate != "2026-10-09" || page.Breakdown.Rows[0].Messages != 2 {
		t.Fatalf("filtered insights: %#v, %v", page, err)
	}
	page, err = s.Stats(context.Background(), scope, StatsRequest{Kind: "voice", Breakdown: "people", Page: 5, PageSize: 50, Sort: "recent"})
	if err != nil || page.Messages != 0 || page.Insights != (StatsInsights{}) || page.Rhythm == nil || len(page.Rhythm) != 0 || page.Breakdown.Page != 1 || page.Breakdown.Total != 0 || page.Breakdown.Rows == nil || len(page.Breakdown.Rows) != 0 || page.Breakdown.Dimension != "people" || page.Breakdown.PageSize != 50 || page.Breakdown.Sort != "recent" {
		t.Fatalf("empty bounded response: %#v, %v", page, err)
	}
}

func TestStatsBreakdownRejectsInvalidBoundsBeforeReading(t *testing.T) {
	for _, query := range []StatsRequest{{Breakdown: "users"}, {Sort: "unsafe"}, {Page: -1}, {Page: 1000001}, {PageSize: -1}, {PageSize: 1}, {PageSize: 51}, {PageSize: int(^uint(0) >> 1)}} {
		if ValidStatsRequest(query) {
			t.Fatalf("invalid stats request admitted: %#v", query)
		}
		s := &Store{}
		if _, _, err := s.StatsGuarded(context.Background(), ReadScope{}, query); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("invalid request reached reader: %#v, %v", query, err)
		}
	}
}

func TestStatsUTCInsightsPreserveLeapDaysAndPreEpochMidnight(t *testing.T) {
	for _, test := range []struct {
		name  string
		times []string
		first string
	}{
		{"leap-day", []string{"2024-02-28T23:59:59.999Z", "2024-02-29T00:00:00Z", "2024-03-01T00:00:00Z"}, "2024-02-28"},
		{"pre-epoch", []string{"1969-12-31T23:59:59.999Z", "1970-01-01T00:00:00Z", "1970-01-02T00:00:00Z"}, "1969-12-31"},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
			s, policy := readerFixture(t, &now)
			var chat ChatID
			for n, stamp := range test.times {
				at, err := time.Parse(time.RFC3339Nano, stamp)
				if err != nil {
					t.Fatal(err)
				}
				chat = readerChat(t, s, readerApply(t, s, policy, readerEvent(fmt.Sprint(n), "allowed", "synthetic", at)))
			}
			page, err := s.Stats(context.Background(), readerScope(policy, chat), StatsRequest{})
			if err != nil || page.Insights.ActiveDays != 3 || page.Insights.LongestStreakDays != 3 || page.Insights.BusiestDate != test.first || page.Insights.BusiestDateMessages != 1 {
				t.Fatalf("UTC calendar boundary: %#v, %v", page.Insights, err)
			}
			var late int64
			for _, slot := range page.Rhythm {
				if slot.Hour == 23 {
					late += slot.Messages
				}
			}
			if late != 1 {
				t.Fatalf("fractional pre-midnight rounded into next day: %#v", page.Rhythm)
			}
		})
	}
}

func TestStatsPeopleUseCanonicalIDsAndKeepEqualNamesSeparate(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, policy := readerFixture(t, &now)
	alias := SourceAlias{Account: "account", PhoneJID: "123@s.whatsapp.net", LID: "456@lid", Provenance: AliasConnectorMapping}
	for n, sender := range []string{alias.PhoneJID, alias.LID, "789@s.whatsapp.net"} {
		event := readerEvent(fmt.Sprint(n), "group@g.us", "synthetic", now)
		event.Key.Sender = sender
		readerApply(t, s, policy, event)
	}
	scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: policy.Generation}
	_, guard, err := s.StatsGuarded(context.Background(), scope, StatsRequest{Breakdown: "people"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApplyAlias(context.Background(), alias); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err == nil {
		t.Fatal("pre-merge aggregate authority survived identity merge")
	}
	if _, err = s.writeDB.Exec(`UPDATE identities SET name='Same display name'`); err != nil {
		t.Fatal(err)
	}
	page, err := s.Stats(context.Background(), scope, StatsRequest{Breakdown: "people"})
	if err != nil || page.Messages != 3 || page.Breakdown.Total != 2 || len(page.Breakdown.Rows) != 2 || page.Breakdown.Rows[0].Messages != 2 || page.Breakdown.Rows[1].Messages != 1 || page.Breakdown.Rows[0].ID == page.Breakdown.Rows[1].ID {
		t.Fatalf("canonical distinct sender groups: %#v, %v", page.Breakdown, err)
	}
}

func TestStatsPeoplePageGuardIncludesAnOffPageExpiringSender(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, policy := readerFixture(t, &now)
	var chat ChatID
	for person := 0; person < 11; person++ {
		count := 2
		if person == 10 {
			count = 1
		}
		for n := 0; n < count; n++ {
			event := readerEvent(fmt.Sprintf("person-%d-%d", person, n), "group@g.us", "synthetic", now.Add(-time.Hour))
			event.Key.Sender = fmt.Sprintf("private-person-%d@s.whatsapp.net", person)
			if person == 10 {
				event.ExpiresAt = now.Add(time.Minute)
			}
			chat = readerChat(t, s, readerApply(t, s, policy, event))
		}
	}
	scope := readerScope(policy, chat)
	page, guard, err := s.StatsGuarded(context.Background(), scope, StatsRequest{Breakdown: "people"})
	if err != nil || page.Messages != 21 || page.Breakdown.Total != 11 || len(page.Breakdown.Rows) != 10 {
		t.Fatalf("first people page: %#v, %v", page, err)
	}
	for _, row := range page.Breakdown.Rows {
		if row.Messages != 2 {
			t.Fatalf("expiring last sender unexpectedly appears on first page: %#v", row)
		}
	}
	now = now.Add(2 * time.Minute)
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err == nil {
		t.Fatal("off-page expiry failed to revoke aggregate response")
	}
	page, err = s.Stats(context.Background(), scope, StatsRequest{Breakdown: "people"})
	if err != nil || page.Messages != 20 || page.Breakdown.Total != 10 || page.Insights.BusiestDateMessages != 20 {
		t.Fatalf("expired off-page sender remains in aggregates: %#v, %v", page, err)
	}
}

// BenchmarkStatsMetadata measures guarded aggregate reads over a synthetic
// 100-chat/500-sender corpus. Bulk clones omit search and media publications;
// this is not an ingestion, transcription, or phone performance benchmark.
// Select 100000 rows with WA_INDEXER_BENCH_MESSAGES=100000 and -benchtime=1x.
func BenchmarkStatsMetadata(b *testing.B) {
	count := 10000
	if raw := os.Getenv("WA_INDEXER_BENCH_MESSAGES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || (n != 10000 && n != 100000) {
			b.Fatal("benchmark scale must be 10000 or 100000")
		}
		count = n
	}
	dir, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, err := Open(context.Background(), dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	policy, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, 0)
	if err != nil {
		b.Fatal(err)
	}
	const seeds = 500
	ids := make([]MessageID, seeds)
	for n := range ids {
		event := readerEvent(fmt.Sprintf("seed-%d", n), fmt.Sprintf("group-%d@g.us", n%100), "synthetic metadata corpus", now.Add(-time.Duration(n)*time.Minute))
		event.Key.Sender = fmt.Sprintf("synthetic-sender-%d@s.whatsapp.net", n)
		event.Key.FromMe = n%17 == 0
		if n%20 == 0 {
			event.BodyKind = MessageVoice
		}
		result, err := s.ApplySourceEvent(context.Background(), event, policy.Generation)
		if err != nil {
			b.Fatal(err)
		}
		ids[n] = result.MessageID
	}
	err = s.transaction(context.Background(), func(tx *sql.Tx) error {
		statement, err := tx.Prepare(`INSERT INTO messages(id,account,chat_id,sender_id,source_chat,source_sender,source_message,from_me,
		 original_sent_at,original_date_known,protocol_revision,local_revision,state,source_kind,body,caption,normalized_body,normalized_caption,body_kind,content_hash)
		 SELECT ?,account,chat_id,sender_id,source_chat,source_sender,?,from_me,?,original_date_known,protocol_revision,local_revision,state,source_kind,
		 body,caption,normalized_body,normalized_caption,body_kind,content_hash FROM messages WHERE id=?`)
		if err != nil {
			return err
		}
		defer statement.Close()
		for n := seeds; n < count; n++ {
			if _, err = statement.Exec(fmt.Sprintf("synthetic-message-%d", n), fmt.Sprintf("synthetic-source-%d", n), now.Add(-time.Duration(n)*time.Minute).UnixMilli(), ids[n%seeds]); err != nil {
				return err
			}
		}
		_, err = bumpRevision(context.Background(), tx)
		return err
	})
	if err != nil {
		b.Fatal(err)
	}
	scope := ReadScope{Account: "account", Principal: "synthetic-owner", AllChats: true, PolicyGeneration: policy.Generation}
	for _, dimension := range []string{"chats", "people"} {
		b.Run(dimension, func(b *testing.B) {
			query := StatsRequest{Breakdown: dimension, Page: 2, PageSize: 50}
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				page, err := s.Stats(context.Background(), scope, query)
				if err != nil || page.Messages != int64(count) || len(page.Breakdown.Rows) != 50 || len(page.Rhythm) > 168 || len(page.Daily) > maxStatsBuckets {
					b.Fatalf("guarded metadata query: count=%d, rows=%d, err=%v", page.Messages, len(page.Breakdown.Rows), err)
				}
			}
		})
	}
}
