//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestSavedViewStatsCurrentOnlyMatchesExistingMedia(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	ref, media, _ := extractionProgressVoice(t, s, p, readerEvent("completed", "voice", "", now.Add(-time.Hour)))
	work := extractionProgressClaim(t, s, extractionTestVersion(JobTranscription), media)
	document := extractionTestDocument(work, "saved-voice-root", "synthetic transcript")
	if ok, err := s.CommitExtraction(context.Background(), ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{document}}); err != nil || !ok {
		t.Fatal(err)
	}
	scope := readerScope(p, readerChat(t, s, ref.Version.MessageID))
	r, err := NewSavedOwnerRead(context.Background(), s, nil, scope, s)
	if err != nil {
		t.Fatal(err)
	}
	want, err := s.Stats(context.Background(), scope, StatsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := r.Stats(context.Background(), StatsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("saved current-only Stats differ: got %#v want %#v", got, want)
	}
}
func TestSavedViewUnifiedStatsAndChats(t *testing.T) {
	f := newHistoricalViewFixture(t)
	r := savedFixtureReader(t, f)
	stats, _, err := r.Stats(context.Background(), StatsRequest{Breakdown: "people"})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 4 || stats.Chats != 2 || stats.Breakdown.Total != 1 {
		t.Fatalf("union aggregate: %#v", stats)
	}
	chats, _, err := r.Chats(context.Background(), ChatListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(chats.Chats) != 2 || chats.Chats[0].MessageCount != 2 || chats.Chats[1].MessageCount != 2 {
		t.Fatalf("chat union %#v", chats)
	}
}
func TestSavedViewHistoricalTranscriptAndChangedParent(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "equivalent", true: "changed"}[changed], func(t *testing.T) {
			var parent MessageID
			f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, hp ArchivePolicy) {
				e := readerEvent("voice-parent", "synthetic-a", "", f.now.Add(-3*time.Minute))
				e.BodyKind = MessageVoice
				e.Media = []MediaReference{{Kind: "voice", ProtocolReference: []byte("synthetic-reference"), DeclaredBytes: 64, MimeType: "audio/ogg", Availability: "not_retained"}}
				parent = readerApply(t, h, hp, e)
				candidateRoot(t, h, hp, "saved-transcript", "uniquetranscript", []MessageID{parent}, derivedContextProvenance(DocumentTranscript))
				if changed {
					e.Media[0].ProtocolReference = []byte("changed-reference")
				}
				readerApply(t, f.live, f.policy, e)
			})
			r := savedFixtureReader(t, f)
			page, _, err := r.Search(context.Background(), SearchRequest{Query: "uniquetranscript"})
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if changed {
				want = 0
			}
			if len(page.Hits) != want {
				t.Fatalf("derived parent eligibility: got %d want %d", len(page.Hits), want)
			}
			stats, _, err := r.Stats(context.Background(), StatsRequest{Kind: "voice"})
			if err != nil {
				t.Fatal(err)
			}
			if stats.Media.TranscribedVoice != int64(want) {
				t.Fatalf("transcript stats=%d want %d", stats.Media.TranscribedVoice, want)
			}
			if !changed {
				contextPage, _, err := r.Context(context.Background(), ContextRequest{MessageID: historicalMessageHandle(f, parent)})
				if err != nil {
					t.Fatal(err)
				}
				if len(contextPage.Messages) != 1 || len(contextPage.Messages[0].Derived) != 1 {
					t.Fatalf("historical transcript context %#v", contextPage)
				}
			}
		})
	}
}

func TestSavedViewStatsLargeMetadataBudget(t *testing.T) {
	bulk := func(t *testing.T, s *Store, seed MessageID, start, count int) {
		t.Helper()
		tx, err := s.writeDB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		statement, err := tx.Prepare(`INSERT INTO messages(id,account,chat_id,sender_id,source_chat,source_sender,source_message,from_me,original_sent_at,original_date_known,protocol_revision,local_revision,state,source_kind,body_kind,content_hash)
 SELECT ?,account,chat_id,sender_id,source_chat,source_sender,?,from_me,original_sent_at,original_date_known,protocol_revision,local_revision,state,source_kind,body_kind,content_hash FROM messages WHERE id=?`)
		if err != nil {
			t.Fatal(err)
		}
		defer statement.Close()
		for i := start; i < start+count; i++ {
			key := fmt.Sprintf("scale-%06d", i)
			if _, err = statement.Exec(key, key, seed); err != nil {
				t.Fatal(err)
			}
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	f := newHistoricalViewFixtureWith(t, func(f *historicalViewFixture, h *Store, hp ArchivePolicy) {
		event := readerEvent("scale-seed", "synthetic-a", "synthetic metadata", f.now.Add(-time.Hour))
		live := readerApply(t, f.live, f.policy, event)
		history := readerApply(t, h, hp, event)
		bulk(t, f.live, live, 0, 20000)
		bulk(t, h, history, 15000, 8000)
	})
	start := time.Now()
	r := savedFixtureReader(t, f)
	stats, guard, err := r.Stats(context.Background(), StatsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 23005 {
		t.Fatalf("exact metadata count=%d want 23005", stats.Messages)
	}
	if err = r.Check(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
	if err = r.Check(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
	t.Logf("23005 eligible contributors, Stats plus two delivery checks: %v", time.Since(start))
}

func TestSavedViewMetadataOverflowFailsExactly(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	seed := readerApply(t, s, p, readerEvent("seed", "synthetic", "metadata", now.Add(-time.Hour)))
	_, err := s.writeDB.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<100001)
 INSERT INTO messages(id,account,chat_id,sender_id,source_chat,source_sender,source_message,from_me,original_sent_at,original_date_known,protocol_revision,local_revision,state,source_kind,body_kind,content_hash)
 SELECT 'overflow-'||n.i,m.account,m.chat_id,m.sender_id,m.source_chat,m.source_sender,'overflow-'||n.i,m.from_me,m.original_sent_at,m.original_date_known,m.protocol_revision,m.local_revision,m.state,m.source_kind,m.body_kind,m.content_hash FROM messages m CROSS JOIN n WHERE m.id=?`, seed)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewSavedOwnerRead(context.Background(), s, nil, readerScope(p, readerChat(t, s, seed)), s)
	if err != nil {
		t.Fatal(err)
	}
	page, _, err := r.Stats(context.Background(), StatsRequest{})
	if !errors.Is(err, ErrSavedReadLimit) {
		t.Fatalf("overflow returned plausible truncated count=%d err=%v", page.Messages, err)
	}
	if page.Messages != 0 {
		t.Fatal("limit error leaked an exact-looking total")
	}
	if _, err = s.writeDB.Exec(`INSERT INTO source_anchors(account,chat,sender,message,from_me,original_sent_at,date_known) SELECT account,source_chat,source_sender,source_message,from_me,original_sent_at,1 FROM messages WHERE id<>?`, seed); err != nil {
		t.Fatal(err)
	}
	small := readerApply(t, s, p, readerEvent("small", "small-chat", "bounded context", now.Add(-time.Minute)))
	smallReader, err := NewSavedOwnerRead(context.Background(), s, nil, ReadScope{Account: "account", Principal: "owner", PolicyGeneration: p.Generation, AllChats: true}, s)
	if err != nil {
		t.Fatal(err)
	}
	contextPage, guard, err := smallReader.Context(context.Background(), ContextRequest{MessageID: small})
	if err != nil || len(contextPage.Messages) != 1 {
		t.Fatalf("unrelated metadata/anchors denied bounded context: %d %v", len(contextPage.Messages), err)
	}
	if err = smallReader.Check(context.Background(), guard); err != nil {
		t.Fatal(err)
	}
}

func TestSavedViewChatQueryUsesCurrentNameAndFullCount(t *testing.T) {
	f := newHistoricalViewFixture(t)
	if _, err := f.live.writeDB.Exec("UPDATE chats SET name='Renamed conversation' WHERE id=?", f.liveChats[0]); err != nil {
		t.Fatal(err)
	}
	r := savedFixtureReader(t, f)
	page, _, err := r.Chats(context.Background(), ChatListRequest{Query: "Renamed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Chats) != 1 || page.Chats[0].MessageCount != 2 || page.Chats[0].Name != "Renamed conversation" {
		t.Fatalf("name filter lost historical members: %#v", page)
	}
}
