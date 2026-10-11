//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestStatsFiltersReadableSourcesBeforeAggregating(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, policy := readerFixture(t, &now)
	first := readerApply(t, s, policy, readerEvent("old", "allowed", "synthetic old text", now.Add(-48*time.Hour)))
	image := readerEvent("image", "allowed", "synthetic caption", now.Add(-2*time.Hour))
	image.BodyKind, image.Key.FromMe = MessageImage, true
	readerApply(t, s, policy, image)
	voice := readerEvent("voice", "allowed", "", now.Add(-time.Hour))
	voice.BodyKind = MessageVoice
	readerApply(t, s, policy, voice)
	other := readerEvent("other", "allowed", "", now.Add(-30*time.Minute))
	other.BodyKind = MessageUnsupported
	readerApply(t, s, policy, other)
	readerApply(t, s, policy, readerEvent("excluded", "excluded", "private excluded metadata", now.Add(-time.Hour)))
	expired := readerEvent("expired", "allowed", "expired synthetic", now.Add(-time.Hour))
	expired.ExpiresAt = now.Add(-time.Minute)
	readerApply(t, s, policy, expired)
	deleted := readerEvent("deleted", "allowed", "deleted synthetic", now.Add(-time.Hour))
	readerApply(t, s, policy, deleted)
	deleted.Kind, deleted.SourceRevision, deleted.Body = EventDelete, 2, ""
	readerApply(t, s, policy, deleted)
	conflict := readerEvent("conflict", "allowed", "conflicted synthetic", now.Add(-time.Hour))
	readerApply(t, s, policy, conflict)
	conflict.Body = "conflicting same-revision content"
	if _, err := s.ApplySourceEvent(context.Background(), conflict, policy.Generation); !errors.Is(err, ErrSourceRevisionConflict) {
		t.Fatalf("fixture did not invalidate conflicting source: %v", err)
	}
	chat := readerChat(t, s, first)
	scope := readerScope(policy, chat)
	page, err := s.Stats(context.Background(), scope, StatsRequest{})
	if err != nil || page.Messages != 4 || page.Chats != 1 || page.Sent != 1 || page.Received != 3 {
		t.Fatalf("readable totals: %#v, %v", page, err)
	}
	if len(page.TopChats) != 1 || page.TopChats[0].ChatID != chat || len(page.Kinds) != 4 || page.Media.VoiceMessages != 1 {
		t.Fatalf("scoped aggregates: %#v", page)
	}
	if page.Breakdown.Total != 1 || len(page.Breakdown.Rows) != 1 || page.Breakdown.Rows[0].Messages != 4 || page.Breakdown.Rows[0].Sent != 1 || page.Insights.ActiveDays != 2 || page.Insights.LongestStreakDays != 1 {
		t.Fatalf("invalid or denied source entered new aggregates: %#v", page)
	}
	var rhythmTotal int64
	for _, slot := range page.Rhythm {
		rhythmTotal += slot.Messages
	}
	if rhythmTotal != 4 {
		t.Fatalf("invalid or denied source entered rhythm: %#v", page.Rhythm)
	}
	var timelineTotal int64
	for _, day := range page.Daily {
		timelineTotal += day.Messages
	}
	if timelineTotal != page.Messages || len(page.Daily) != 3 || page.ActivityInterval != "day" {
		t.Fatalf("timeline totals: %#v", page.Daily)
	}
	page, err = s.Stats(context.Background(), scope, StatsRequest{From: now.Add(-2 * time.Hour), Until: now.Add(-time.Hour)})
	if err != nil || page.Messages != 1 || page.Sent != 1 || page.Media.VoiceMessages != 0 {
		t.Fatalf("inclusive start/exclusive end: %#v, %v", page, err)
	}
	page, err = s.Stats(context.Background(), scope, StatsRequest{Kind: "other"})
	if err != nil || page.Messages != 1 || len(page.Kinds) != 1 || page.Kinds[0].Kind != "other" {
		t.Fatalf("other classification: %#v, %v", page, err)
	}
	page, err = s.Stats(context.Background(), scope, StatsRequest{ChatID: "opaque_unselected_chat"})
	if err != nil || page.Messages != 0 || len(page.TopChats) != 0 || len(page.Daily) != 0 {
		t.Fatalf("requested denied chat: %#v, %v", page, err)
	}
}

func TestStatsVoiceCountsRetainedOriginalsAndCurrentTranscript(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, policy := readerFixture(t, &now)
	ref, media, original := extractionProgressVoice(t, s, policy, readerEvent("completed", "voice", "", now.Add(-time.Hour)))
	version := extractionTestVersion(JobTranscription)
	work := extractionProgressClaim(t, s, version, media)
	document := extractionTestDocument(work, "stats-voice-root", "שלום synthetic transcript")
	if ok, err := s.CommitExtraction(context.Background(), ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{document}}); err != nil || !ok {
		t.Fatalf("transcript commit: %v, %v", ok, err)
	}
	scope := readerScope(policy, readerChat(t, s, ref.Version.MessageID))
	assertMedia := func(want StatsMedia) {
		t.Helper()
		page, err := s.Stats(context.Background(), scope, StatsRequest{Kind: "voice"})
		if err != nil || page.Media != want {
			t.Fatalf("voice aggregate: %#v, want %#v, %v", page.Media, want, err)
		}
	}
	assertMedia(StatsMedia{VoiceMessages: 1, TranscribedVoice: 1, RetainedVoice: 1})
	if _, err := s.writeDB.Exec("UPDATE extension_media_artifacts SET state='evicting',cleanup_ack=1 WHERE id=?", original.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec("UPDATE extension_media_associations SET artifact_id='',availability='not_retained' WHERE id=?", media); err != nil {
		t.Fatal(err)
	}
	assertMedia(StatsMedia{VoiceMessages: 1, TranscribedVoice: 1})
	mutation := readerEvent("completed", "voice", "", now.Add(-time.Hour))
	mutation.Kind, mutation.SourceRevision = EventDelete, 2
	readerApply(t, s, policy, mutation)
	assertMedia(StatsMedia{})
}

func TestStatsAggregateGuardProtectsExpiryAndRevocation(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "expiry", true: "delete"}[revoke], func(t *testing.T) {
			now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
			s, policy := readerFixture(t, &now)
			event := readerEvent("guarded", "voice", "synthetic", now.Add(-time.Hour))
			event.ExpiresAt = now.Add(time.Minute)
			id := readerApply(t, s, policy, event)
			scope := readerScope(policy, readerChat(t, s, id))
			page, guard, err := s.StatsGuarded(context.Background(), scope, StatsRequest{})
			if err != nil || page.Messages != 1 {
				t.Fatalf("guarded stats: %#v, %v", page, err)
			}
			if revoke {
				event.Kind, event.SourceRevision, event.Body = EventDelete, 2, ""
				readerApply(t, s, policy, event)
			} else {
				now = now.Add(2 * time.Minute)
			}
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err == nil {
				t.Fatal("expired or revoked aggregate admitted")
			}
		})
	}
}

func TestStatsRejectsInvalidQueryAndUnsafeArchive(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, policy := readerFixture(t, &now)
	id := readerApply(t, s, policy, readerEvent("present", "allowed", "synthetic", now))
	scope := readerScope(policy, readerChat(t, s, id))
	for _, query := range []StatsRequest{{Kind: "video"}, {From: now, Until: now}, {ChatID: "invalid\x00"}} {
		if _, err := s.Stats(context.Background(), scope, query); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("invalid query returned %v", err)
		}
	}
	if err := s.MarkUnsafe(context.Background(), "account", UnsafeReason("mutation_metadata_missing")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stats(context.Background(), scope, StatsRequest{}); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("unsafe stats returned %v", err)
	}
}

func TestStatsDiscardsAggregatesAfterConcurrentDeletion(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, policy := readerFixture(t, &now)
	event := readerEvent("concurrent", "allowed", "synthetic", now.Add(-time.Minute))
	id := readerApply(t, s, policy, event)
	scope := readerScope(policy, readerChat(t, s, id))
	var calls atomic.Int64
	ready, release := make(chan struct{}), make(chan struct{})
	s.options.Now = func() time.Time {
		if calls.Add(1) == 2 {
			close(ready)
			<-release
		}
		return now
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.Stats(context.Background(), scope, StatsRequest{})
		result <- err
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("aggregate did not reach final check")
	}
	event.Kind, event.SourceRevision, event.Body = EventDelete, 2, ""
	readerApply(t, s, policy, event)
	close(release)
	if err := <-result; err == nil {
		t.Fatal("revoked aggregate returned")
	}
}

func TestStatsActivitySpansHistoryWithoutUnboundedBuckets(t *testing.T) {
	for _, period := range []struct {
		first, last string
		interval    string
	}{
		{"2026-10-01", "2026-10-11", "day"},
		{"2025-10-01", "2026-10-11", "week"},
		{"2020-01-01", "2026-10-11", "month"},
		{"1900-01-01", "2026-10-11", "year"},
		{"0001-01-01", "9999-12-31", "year"},
	} {
		first, _ := time.Parse("2006-01-02", period.first)
		last, _ := time.Parse("2006-01-02", period.last)
		interval := statsChooseInterval(first, last)
		count := 0
		for at := interval.start(first); !at.After(last); at = interval.next(at) {
			count++
		}
		if interval.name != period.interval || count > maxStatsBuckets || count == 0 {
			t.Fatalf("%s to %s: %#v, %d buckets", period.first, period.last, interval, count)
		}
	}
	// Sunday is still part of the preceding Monday's UTC week.
	day, _ := time.Parse("2006-01-02", "2026-10-11")
	if got := (statsInterval{"week", 1}).start(day).Format("2006-01-02"); got != "2026-10-05" {
		t.Fatalf("week start: %s", got)
	}
}

func TestStatsCalendarBucketsAggregateInSQL(t *testing.T) {
	for _, period := range []struct {
		first, last string
		interval    string
		lastBucket  string
	}{
		{"1970-01-01", "1970-01-02", "day", "1970-01-02"},
		{"2025-10-01", "2026-10-11", "week", "2026-10-05"},
		{"2020-01-01", "2026-10-11", "month", "2026-10-01"},
		{"2000-01-01", "2026-10-11", "year", "2026-01-01"},
		{"1800-01-01", "2026-10-11", "year", "2025-01-01"},
	} {
		t.Run(period.interval+period.first, func(t *testing.T) {
			now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
			s, policy := readerFixture(t, &now)
			first, _ := time.Parse("2006-01-02", period.first)
			last, _ := time.Parse("2006-01-02", period.last)
			id := readerApply(t, s, policy, readerEvent("first", "allowed", "synthetic first", first))
			readerApply(t, s, policy, readerEvent("last", "allowed", "synthetic last", last))
			page, err := s.Stats(context.Background(), readerScope(policy, readerChat(t, s, id)), StatsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			var total int64
			for _, bucket := range page.Daily {
				total += bucket.Messages
			}
			if page.ActivityInterval != period.interval || total != 2 || page.Messages != 2 || page.FirstMessageAt != first.Unix() || page.LastMessageAt != last.Unix() || page.Daily[len(page.Daily)-1].Date != period.lastBucket || page.TopChats[0].LastMessageAt != last.Unix() {
				t.Fatalf("calendar aggregates: %#v", page)
			}
		})
	}
}

func TestStatsRollingScopeAndFinalClockHorizon(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s, policy := readerFixture(t, &now)
	id := readerApply(t, s, policy, readerEvent("current", "allowed", "synthetic", now.Add(-time.Hour)))
	readerApply(t, s, policy, readerEvent("old", "allowed", "outside rolling window", now.Add(-2*time.Hour)))
	scope := readerScope(policy, readerChat(t, s, id))
	scope.Rolling = 90 * time.Minute
	page, guard, err := s.StatsGuarded(context.Background(), scope, StatsRequest{})
	if err != nil || page.Messages != 1 {
		t.Fatalf("rolling aggregate: %#v, %v", page, err)
	}
	now = now.Add(31 * time.Minute)
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("crossed rolling horizon: %v", err)
	}
}
