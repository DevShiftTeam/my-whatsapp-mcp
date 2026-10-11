//go:build sqlite_fts5

package archive

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestContextReauthorizesEveryMessageAndReply(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	denied := readerEvent("denied", "other", "private quoted canary", now.Add(-time.Hour))
	deniedID := readerApply(t, s, p, denied)
	old := readerApply(t, s, p, readerEvent("old", "allowed", "date denied canary", now.Add(-5*time.Hour)))
	center := readerEvent("center", "allowed", "current source", now.Add(-time.Hour))
	center.ReplyTo = &denied.Key
	centerID := readerApply(t, s, p, center)
	last := readerApply(t, s, p, readerEvent("after", "allowed", "next source", now.Add(-time.Minute)))
	scope := readerScope(p, readerChat(t, s, centerID))
	scope.From = now.Add(-2 * time.Hour)
	page, err := s.Context(context.Background(), scope, ContextRequest{MessageID: centerID, Before: 20, After: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.Messages[0].ID != centerID || page.Messages[1].ID != last {
		t.Fatalf("context %#v", page)
	}
	if page.Messages[0].ReplyTo != "" || page.Messages[0].ReplyQuote != "" || strings.Contains(page.Messages[0].SenderID, "@") {
		t.Fatalf("reply or private sender exposed %#v", page.Messages[0])
	}
	for _, id := range []MessageID{deniedID, old, "guess"} {
		if _, err = s.Context(context.Background(), scope, ContextRequest{MessageID: id}); !errors.Is(err, ErrSourceUnavailable) {
			t.Fatalf("guessed/denied %q %v", id, err)
		}
	}
}

func TestReaderPolicySafetyStatusAndRestart(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	p := testPolicy(t, s)
	id := readerApply(t, s, p, readerEvent("persist", "allowed", "offline Hebrew שלום", now.Add(-time.Hour)))
	chat := readerChat(t, s, id)
	denied := readerApply(t, s, p, readerEvent("private", "denied", "hidden metadata", now))
	_ = denied
	scope := readerScope(p, chat)
	status, err := s.Status(context.Background(), scope)
	if err != nil || status.Messages != 1 || len(status.Chats) != 1 || status.Semantic.State != "not_configured" {
		t.Fatalf("status %#v %v", status, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(context.Background(), dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.Search(context.Background(), scope, SearchRequest{Query: "שלום"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID != id {
		t.Fatalf("restart %#v %v", page, err)
	}
	p2, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Context(context.Background(), scope, ContextRequest{MessageID: id}); !errors.Is(err, ErrPolicyChanged) {
		t.Fatalf("stale policy %v", err)
	}
	scope.PolicyGeneration = p2.Generation
	if _, err = s.Context(context.Background(), scope, ContextRequest{MessageID: id}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("excluded source %v", err)
	}
	if err = s.MarkUnsafe(context.Background(), "account", UnsafeReason("mutation_metadata_missing")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Search(context.Background(), scope, SearchRequest{Query: "offline"}); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("unsafe read %v", err)
	}
}

func TestContextResponseBoundIncludesEscapedTextAndMetadata(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	var middle MessageID
	for n := 0; n < 41; n++ {
		id := readerApply(t, s, p, readerEvent(string(rune('A'+n)), "chat", strings.Repeat("\u0001", 16000), now.Add(time.Duration(n-42)*time.Minute)))
		if n == 20 {
			middle = id
		}
	}
	page, err := s.Context(context.Background(), readerScope(p, readerChat(t, s, middle)), ContextRequest{MessageID: middle, Before: 20, After: 20})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxContextBytes {
		t.Fatalf("response bytes %d exceed %d", len(encoded), maxContextBytes)
	}
	if len(page.Messages) != 41 {
		t.Fatalf("message window lost: %d", len(page.Messages))
	}
	if page.Messages[20].ID != middle || page.Messages[20].Body == "" {
		t.Fatal("context budget discarded requested source body")
	}
}

func TestReaderDiscardsBodyAfterCommittedInvalidationDuringFinalCheck(t *testing.T) {
	for _, kind := range []SourceEventKind{EventDelete, EventEditInvalidation} {
		t.Run(string(kind), func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			e := readerEvent("source", "chat", "must not escape", now.Add(-time.Minute))
			id := readerApply(t, s, p, e)
			scope := readerScope(p, readerChat(t, s, id))
			var clockCalls atomic.Int64
			ready := make(chan struct{})
			release := make(chan struct{})
			s.options.Now = func() time.Time {
				if clockCalls.Add(1) == 2 {
					close(ready)
					<-release
				}
				return now
			}
			result := make(chan error, 1)
			go func() {
				page, err := s.Context(context.Background(), scope, ContextRequest{MessageID: id})
				if err == nil && len(page.Messages) > 0 {
					result <- errors.New("revoked body returned")
					return
				}
				result <- nil
			}()
			select {
			case <-ready:
			case <-time.After(2 * time.Second):
				t.Fatal("reader did not reach final check")
			}
			e.Kind = kind
			e.SourceRevision = 2
			e.Body = ""
			readerApply(t, s, p, e)
			close(release)
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReaderPreservesUnchangedResultDuringUnrelatedOriginalInsertion(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", "unchanged body", now.Add(-time.Minute)))
	scope := readerScope(p, readerChat(t, s, id))
	var clockCalls atomic.Int64
	ready := make(chan struct{})
	release := make(chan struct{})
	s.options.Now = func() time.Time {
		if clockCalls.Add(1) == 2 {
			close(ready)
			<-release
		}
		return now
	}
	type result struct {
		page ContextPage
		err  error
	}
	out := make(chan result, 1)
	go func() {
		page, err := s.Context(context.Background(), scope, ContextRequest{MessageID: id})
		out <- result{page, err}
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not reach final check")
	}
	readerApply(t, s, p, readerEvent("new-unrelated", "other-chat", "unrelated insertion", now))
	close(release)
	r := <-out
	if r.err != nil || len(r.page.Messages) != 1 || r.page.Messages[0].ID != id || r.page.Messages[0].Body != "unchanged body" {
		t.Fatalf("unrelated insertion discarded unchanged body %#v %v", r.page, r.err)
	}
}

func TestReaderSnapshotFinalCheckIgnoresUnrelatedInsertButStillInvalidatesCursor(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", "same body", now.Add(-time.Minute)))
	scope := readerScope(p, readerChat(t, s, id))
	ctx := context.Background()
	frame, err := s.beginRead(ctx, scope, nil, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	defer frame.close()
	rows, err := frame.contextRows(" AND m.id=?", []any{id}, "m.original_sent_at,m.id", 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("snapshot row %v", err)
	}
	readerApply(t, s, p, readerEvent("new-unrelated", "other", "new unrelated body", now))
	if err = frame.finish(); err != nil {
		t.Fatalf("unchanged source discarded by global archive revision: %v", err)
	}
	readerApply(t, s, p, readerEvent("second", "chat", "same body", now.Add(-2*time.Minute)))
	page, err := s.Search(ctx, scope, SearchRequest{Query: "same", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	readerApply(t, s, p, readerEvent("newer-unrelated", "other", "new unrelated body", now))
	if _, err = s.Search(ctx, scope, SearchRequest{Query: "same", Limit: 1, Cursor: page.NextCursor}); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("archive mutation cursor not stale %v", err)
	}
}

func TestReaderStatusRechecksRollingClockAcrossEntireScope(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("edge", "chat", "near rolling edge", now.Add(-time.Hour+time.Millisecond)))
	scope := readerScope(p, readerChat(t, s, id))
	scope.Rolling = time.Hour
	calls := 0
	s.options.Now = func() time.Time {
		calls++
		if calls > 1 {
			return now.Add(2 * time.Millisecond)
		}
		return now
	}
	if page, err := s.Status(context.Background(), scope); !errors.Is(err, ErrSourceUnavailable) || page.Messages != 0 || len(page.Chats) != 0 {
		t.Fatalf("crossed clock leaked status %#v %v", page, err)
	}
}

func TestContextAllowedReplyUsesCurrentSourceAndRemovesDeletedQuote(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	target := readerEvent("target", "chat", "old quote", now.Add(-time.Hour))
	targetID := readerApply(t, s, p, target)
	source := readerEvent("reply", "chat", "reply body", now.Add(-time.Minute))
	source.ReplyTo = &target.Key
	sourceID := readerApply(t, s, p, source)
	scope := readerScope(p, readerChat(t, s, sourceID))
	target.Kind = EventEditContent
	target.SourceRevision = 2
	target.Body = "new current quote"
	readerApply(t, s, p, target)
	page, err := s.Context(context.Background(), scope, ContextRequest{MessageID: sourceID})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ReplyTo != targetID || page.Messages[0].ReplyQuote != "new current quote" {
		t.Fatalf("current reply %#v %v", page, err)
	}
	target.Kind = EventDelete
	target.SourceRevision = 3
	target.Body = ""
	readerApply(t, s, p, target)
	page, err = s.Context(context.Background(), scope, ContextRequest{MessageID: sourceID})
	if err != nil || page.Messages[0].ReplyTo != "" || page.Messages[0].ReplyQuote != "" {
		t.Fatalf("deleted reply %#v %v", page, err)
	}
	if _, err = s.Context(context.Background(), scope, ContextRequest{MessageID: sourceID, Before: 21}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("context bound %v", err)
	}
}

func TestReaderEnforcesExpiryLearnedFromDuplicateAndNeverExtendedByEdit(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(map[bool]string{false: "duplicate", true: "edit_extension"}[edit], func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			e := readerEvent("source", "chat", "expiry canary", now.Add(-time.Minute))
			if edit {
				e.ExpiresAt = now.Add(time.Minute)
			}
			id := readerApply(t, s, p, e)
			scope := readerScope(p, readerChat(t, s, id))
			e.ExpiresAt = now.Add(time.Minute)
			if edit {
				e.Kind = EventEditContent
				e.SourceRevision = 2
				e.Body = "expiry edited canary"
				e.ExpiresAt = now.Add(time.Hour)
			}
			readerApply(t, s, p, e)
			now = now.Add(time.Minute)
			page, err := s.Search(context.Background(), scope, SearchRequest{Query: "expiry"})
			if err != nil || len(page.Hits) != 0 {
				t.Fatalf("expiry body searchable %#v %v", page, err)
			}
			if _, err = s.Context(context.Background(), scope, ContextRequest{MessageID: id}); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("expiry context %v", err)
			}
		})
	}
}
