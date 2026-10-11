//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func readerFixture(t *testing.T, now *time.Time) (*Store, ArchivePolicy) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dir, Options{Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	p, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}
func readerEvent(id, chat, body string, sent time.Time) SourceEvent {
	return SourceEvent{Key: SourceKey{Account: "account", Chat: chat, Sender: "private-sender@s.whatsapp.net", Message: id}, Kind: EventOriginal, OriginalSentAt: sent, OriginalDateKnown: true, SourceRevision: 1, Body: body, BodyKind: MessageText}
}
func readerApply(t *testing.T, s *Store, p ArchivePolicy, e SourceEvent) MessageID {
	t.Helper()
	r, err := s.ApplySourceEvent(context.Background(), e, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	return r.MessageID
}
func readerScope(p ArchivePolicy, chats ...ChatID) ReadScope {
	return ReadScope{Account: "account", Principal: "owner", PolicyGeneration: p.Generation, Chats: chats}
}
func readerChat(t *testing.T, s *Store, id MessageID) ChatID {
	t.Helper()
	var chat ChatID
	if err := s.readDB.QueryRow("SELECT chat_id FROM messages WHERE id=?", id).Scan(&chat); err != nil {
		t.Fatal(err)
	}
	return chat
}

func TestKeywordNormalizedOriginalOfflineAndScopedTopK(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	original := "שָׁלוֹם ד״ר כהן Project 42 https://Example.com 😀"
	want := readerApply(t, s, p, readerEvent("allowed", "allowed", original, now.Add(-time.Hour)))
	for i := 0; i < 120; i++ {
		readerApply(t, s, p, readerEvent(strings.Repeat("d", i+1), "denied", "שלום Project 42", now.Add(-time.Minute)))
	}
	scope := readerScope(p, readerChat(t, s, want))
	for _, query := range []string{"שלום", "שָׁלוֹם", "ד\"ר", "Project 42", "https://example.com", "כהן 😀"} {
		page, err := s.Search(context.Background(), scope, SearchRequest{Query: query, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Hits) != 1 || page.Hits[0].MessageID != want || page.Hits[0].Snippet != original {
			t.Fatalf("query %q got %#v", query, page)
		}
	}
	denied, err := s.Search(context.Background(), readerScope(p), SearchRequest{Query: "שלום"})
	if !errors.Is(err, ErrUnauthorized) || len(denied.Hits) != 0 {
		t.Fatalf("empty scope %v %#v", err, denied)
	}
	scope.Principal = ""
	if _, err := s.Search(context.Background(), scope, SearchRequest{Query: "שלום"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("missing principal %v", err)
	}
}

func TestKeywordFiltersOnlyNarrowAndMutationCursorStale(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	var ids []MessageID
	for i := 0; i < 3; i++ {
		ids = append(ids, readerApply(t, s, p, readerEvent(string(rune('a'+i)), "allowed", "search canary", now.Add(-time.Duration(i+1)*time.Hour))))
	}
	other := readerApply(t, s, p, readerEvent("other", "other", "search canary", now))
	scope := readerScope(p, readerChat(t, s, ids[0]))
	query := SearchRequest{Query: "canary", Limit: 1}
	page, err := s.Search(context.Background(), scope, query)
	if err != nil || len(page.Hits) != 1 || page.NextCursor == "" {
		t.Fatalf("first %#v %v", page, err)
	}
	query.Cursor = page.NextCursor
	page, err = s.Search(context.Background(), scope, query)
	if err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID == ids[0] {
		t.Fatalf("next %#v %v", page, err)
	}
	query.Cursor = ""
	query.Chats = []ChatID{readerChat(t, s, other)}
	page, err = s.Search(context.Background(), scope, query)
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("widened %#v %v", page, err)
	}
	query.Chats = nil
	query.From = now.Add(-90 * time.Minute)
	page, err = s.Search(context.Background(), scope, query)
	if err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID != ids[0] {
		t.Fatalf("date %#v %v", page, err)
	}
	query.From = time.Time{}
	page, err = s.Search(context.Background(), scope, query)
	if err != nil {
		t.Fatal(err)
	}
	query.Cursor = page.NextCursor
	readerApply(t, s, p, readerEvent("new", "allowed", "search canary", now))
	if _, err = s.Search(context.Background(), scope, query); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("mutation accepted cursor %v", err)
	}
	query.Cursor = ""
	query.Limit = 101
	if _, err = s.Search(context.Background(), scope, query); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("oversized limit %v", err)
	}
}

func TestKeywordExpiryAndRollingCursorUseActualClock(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	e := readerEvent("expiring", "chat", "clock canary", now.Add(-time.Hour))
	e.ExpiresAt = now.Add(time.Minute)
	exp := readerApply(t, s, p, e)
	readerApply(t, s, p, readerEvent("plain", "chat", "clock canary", now.Add(-2*time.Hour)))
	scope := readerScope(p, readerChat(t, s, exp))
	query := SearchRequest{Query: "clock", Limit: 1}
	page, err := s.Search(context.Background(), scope, query)
	if err != nil {
		t.Fatal(err)
	}
	query.Cursor = page.NextCursor
	now = now.Add(time.Minute)
	if _, err = s.Search(context.Background(), scope, query); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("expiry cursor %v", err)
	}
	query.Cursor = ""
	page, err = s.Search(context.Background(), scope, query)
	if err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID == exp {
		t.Fatalf("expired %#v %v", page, err)
	}
	scope.Rolling = 3 * time.Hour
	page, err = s.Search(context.Background(), scope, SearchRequest{Query: "clock"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	page, err = s.Search(context.Background(), scope, SearchRequest{Query: "clock"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("rolling %#v %v", page, err)
	}
}

func TestKeywordRankingUnaffectedByDeniedCorpus(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	first := readerApply(t, s, p, readerEvent("short", "allowed", "alpha", now.Add(-time.Hour)))
	readerApply(t, s, p, readerEvent("repeated", "allowed", "alpha alpha beta beta", now.Add(-time.Hour)))
	scope := readerScope(p, readerChat(t, s, first))
	query := SearchRequest{Query: "alpha"}
	before, err := s.Search(context.Background(), scope, query)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		readerApply(t, s, p, readerEvent(string(rune('a'+i)), "denied", strings.Repeat("alpha ", 1000), now))
	}
	after, err := s.Search(context.Background(), scope, query)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Hits) != len(after.Hits) {
		t.Fatalf("scoped count changed")
	}
	for i := range before.Hits {
		if before.Hits[i].MessageID != after.Hits[i].MessageID || before.Hits[i].Score != after.Hits[i].Score {
			t.Fatalf("denied corpus changed permitted ranks/scores: before %#v after %#v", before.Hits, after.Hits)
		}
	}
}

func TestReaderRollingPolicyAndScopeIntersect(t *testing.T) {
	for _, pair := range [][2]time.Duration{{time.Hour, 3 * time.Hour}, {3 * time.Hour, time.Hour}} {
		t.Run(pair[0].String()+"_"+pair[1].String(), func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			id := readerApply(t, s, p, readerEvent("outside", "chat", "retention canary", now.Add(-2*time.Hour)))
			p, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll, RollingRetention: pair[0]}, p.Generation)
			if err != nil {
				t.Fatal(err)
			}
			scope := readerScope(p, readerChat(t, s, id))
			scope.Rolling = pair[1]
			page, err := s.Search(context.Background(), scope, SearchRequest{Query: "retention"})
			if err != nil || len(page.Hits) != 0 {
				t.Fatalf("widened rolling bound %#v %v", page, err)
			}
		})
	}
}

func TestKeywordPreservesFinalLettersAndDoesNotStemEnglish(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	id := readerApply(t, s, p, readerEvent("source", "chat", "מלך running", now.Add(-time.Hour)))
	scope := readerScope(p, readerChat(t, s, id))
	for _, query := range []string{"מלכ", "run"} {
		page, err := s.Search(context.Background(), scope, SearchRequest{Query: query})
		if err != nil || len(page.Hits) != 0 {
			t.Fatalf("conflated query %q %#v %v", query, page, err)
		}
	}
	for _, query := range []string{"מלך", "running"} {
		page, err := s.Search(context.Background(), scope, SearchRequest{Query: query})
		if err != nil || len(page.Hits) != 1 {
			t.Fatalf("original query %q %#v %v", query, page, err)
		}
	}
}

func TestNonRollingCursorSurvivesClockTickAndRollingCursorStales(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	first := readerApply(t, s, p, readerEvent("first", "chat", "paging", now.Add(-time.Hour)))
	readerApply(t, s, p, readerEvent("second", "chat", "paging", now.Add(-2*time.Hour)))
	scope := readerScope(p, readerChat(t, s, first))
	request := SearchRequest{Query: "paging", Limit: 1}
	page, err := s.Search(context.Background(), scope, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Cursor = page.NextCursor
	now = now.Add(time.Millisecond)
	if _, err = s.Search(context.Background(), scope, request); err != nil {
		t.Fatalf("owner cursor clock tick %v", err)
	}
	scope.Rolling = 3 * time.Hour
	request.Cursor = ""
	page, err = s.Search(context.Background(), scope, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Cursor = page.NextCursor
	now = now.Add(time.Millisecond)
	if _, err = s.Search(context.Background(), scope, request); err != nil {
		t.Fatalf("rolling cursor small clock tick %v", err)
	}
	now = now.Add(time.Nanosecond)
	if _, err = s.Search(context.Background(), scope, request); err != nil {
		t.Fatalf("rolling cursor nanosecond tick %v", err)
	}
	now = now.Add(time.Hour)
	if _, err = s.Search(context.Background(), scope, request); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("rolling source crossing cursor %v", err)
	}
}

func TestReaderFractionalBoundsAuthorizeStoredMillisecondsExactly(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	sent := now.Add(-time.Hour)
	id := readerApply(t, s, p, readerEvent("source", "chat", "fractional", sent))
	scope := readerScope(p, readerChat(t, s, id))
	scope.From = sent.Add(500 * time.Microsecond)
	page, err := s.Search(context.Background(), scope, SearchRequest{Query: "fractional"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("scope lower fractional widened %#v %v", page, err)
	}
	scope.From = time.Time{}
	page, err = s.Search(context.Background(), scope, SearchRequest{Query: "fractional", From: sent.Add(500 * time.Microsecond)})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("request lower fractional widened %#v %v", page, err)
	}
	scope.Until = sent.Add(500 * time.Microsecond)
	page, err = s.Search(context.Background(), scope, SearchRequest{Query: "fractional"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("scope upper fractional denied valid source %#v %v", page, err)
	}
	scope.Until = time.Time{}
	page, err = s.Search(context.Background(), scope, SearchRequest{Query: "fractional", Until: sent.Add(500 * time.Microsecond)})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("request upper fractional denied valid source %#v %v", page, err)
	}
}

func TestCursorBindsExpiryAndRollingHorizonsSeparatelyAtTie(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	crossing := now.Add(time.Minute)
	expiring := readerEvent("expiring", "chat", "horizon tie", now.Add(-time.Minute))
	expiring.ExpiresAt = crossing
	first := readerApply(t, s, p, expiring)
	readerApply(t, s, p, readerEvent("rolling", "chat", "horizon tie", crossing.Add(-time.Hour)))
	readerApply(t, s, p, readerEvent("survivor", "chat", "horizon tie", now.Add(-2*time.Minute)))
	scope := readerScope(p, readerChat(t, s, first))
	scope.Rolling = time.Hour
	request := SearchRequest{Query: "horizon", Limit: 1}
	page, err := s.Search(context.Background(), scope, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Cursor = page.NextCursor
	now = crossing
	if _, err = s.Search(context.Background(), scope, request); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("equal horizons hid expired row change: %v", err)
	}
}

// Explicit opt-in scale harness. Keep runs bounded with -benchtime=50x and
// WA_INDEXER_BENCH_MESSAGES=10000 or 100000. Larger corpora require a separate
// coordinated gate; this development host is not the intended 8 GiB proof.
func BenchmarkScopedKeyword(b *testing.B) {
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
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, err := Open(context.Background(), dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	policy, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, 0)
	if err != nil {
		b.Fatal(err)
	}
	const corpus = `שָׁלוֹם צוות הפרויקט הזמנה 42 ד״ר כהן פגישת לקוח מחר בשעה 10 Project schedule customer invoice 2026 https://example.test/docs release notes 😀`
	var selected ChatID
	var chars, words int64
	for n := 0; n < count; n++ {
		body := fmt.Sprintf("%s reference %d", corpus, n)
		if n%97 == 0 {
			body += " needle"
		}
		chars += int64(len([]rune(body)))
		words += int64(len(strings.Fields(body)))
		e := SourceEvent{Key: SourceKey{Account: "benchmark", Chat: fmt.Sprintf("chat-%03d", n%100), Sender: "synthetic-sender", Message: fmt.Sprintf("message-%d", n)}, Kind: EventOriginal, OriginalDateKnown: true, OriginalSentAt: now.Add(-time.Duration(count-n) * time.Second), SourceRevision: 1, Body: body, BodyKind: MessageText}
		result, err := s.ApplySourceEvent(context.Background(), e, policy.Generation)
		if err != nil {
			b.Fatal(err)
		}
		if n == 0 {
			if err = s.readDB.QueryRow("SELECT chat_id FROM messages WHERE id=?", result.MessageID).Scan(&selected); err != nil {
				b.Fatal(err)
			}
		}
	}
	host, _ := os.Hostname()
	b.Logf("host=%s os=%s arch=%s cpus=%d corpus=synthetic_uniform_100_chats_v1 messages=%d chats=100 avg_runes=%.1f avg_words=%.1f chat_spacing=100s context_max_messages=41 models=absent", host, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), count, float64(chars)/float64(count), float64(words)/float64(count))
	all := ReadScope{Account: "benchmark", Principal: "benchmark-owner", AllChats: true, PolicyGeneration: policy.Generation}
	scoped := all
	scoped.AllChats = false
	scoped.Chats = []ChatID{selected}
	cases := []struct {
		name, query string
		scope       ReadScope
		active      bool
	}{{"common_all_idle", "שלום", all, false}, {"common_selective_idle", "שלום", scoped, false}, {"rare_selective_idle", "needle", scoped, false}, {"common_selective_ingestion_100_per_s", "שלום", scoped, true}}
	var activeSequence atomic.Int64
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			request := SearchRequest{Query: tc.query, Limit: 50}
			for n := 0; n < 3; n++ {
				if _, err := s.Search(context.Background(), tc.scope, request); err != nil {
					b.Fatal(err)
				}
			}
			stop, done, activeReady := make(chan struct{}), make(chan struct{}), make(chan struct{})
			writeErrors := make(chan error, 1)
			var writes atomic.Int64
			var stopOnce sync.Once
			stopIngestion := func() {
				stopOnce.Do(func() {
					if tc.active {
						close(stop)
						<-done
					}
				})
			}
			defer stopIngestion()
			if tc.active {
				go func() {
					defer close(done)
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-stop:
							return
						case <-ticker.C:
							n := activeSequence.Add(1)
							e := SourceEvent{Key: SourceKey{Account: "benchmark", Chat: "chat-000", Sender: "synthetic-sender", Message: fmt.Sprintf("active-%d", n)}, Kind: EventOriginal, OriginalDateKnown: true, OriginalSentAt: now.Add(time.Duration(n) * time.Millisecond), SourceRevision: 1, Body: "bounded ingestion heartbeat", BodyKind: MessageText}
							if _, err := s.ApplySourceEvent(context.Background(), e, policy.Generation); err != nil {
								writeErrors <- err
								return
							}
							if writes.Add(1) == 1 {
								close(activeReady)
							}
						}
					}
				}()
			}
			if tc.active {
				select {
				case <-activeReady:
				case err := <-writeErrors:
					b.Fatal(err)
				case <-time.After(time.Second):
					b.Fatal("active ingestion did not start")
				}
			}
			latencies := make([]time.Duration, 0, min(b.N, 10000))
			successful, rejected, peakQueued := 0, 0, int64(0)
			b.ReportAllocs()
			b.ResetTimer()
			commitsAtStart := writes.Load()
			measureStart := time.Now()
			for n := 0; n < b.N; n++ {
				start := time.Now()
				_, err := s.Search(context.Background(), tc.scope, request)
				elapsed := time.Since(start)
				if err == nil {
					successful++
					if len(latencies) < 10000 {
						latencies = append(latencies, elapsed)
					}
				} else if errors.Is(err, ErrCursorStale) || errors.Is(err, ErrPolicyChanged) {
					rejected++
				} else {
					b.Fatal(err)
				}
				if queued := s.queuedBytes.Load(); queued > peakQueued {
					peakQueued = queued
				}
			}
			measuredElapsed := time.Since(measureStart)
			measuredCommits := writes.Load() - commitsAtStart
			b.StopTimer()
			stopIngestion()
			if tc.active {
				select {
				case err := <-writeErrors:
					b.Fatal(err)
				default:
				}
			}
			if len(latencies) > 0 {
				sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
				p95 := latencies[(len(latencies)*95+99)/100-1]
				b.ReportMetric(float64(p95)/float64(time.Millisecond), "successful_p95_ms")
			}
			var archiveBytes, walBytes int64
			if stat, err := os.Stat(filepath.Join(dir, "archive.sqlite")); err == nil {
				archiveBytes = stat.Size()
			}
			if stat, err := os.Stat(filepath.Join(dir, "archive.sqlite-wal")); err == nil {
				walBytes = stat.Size()
			}
			var memory runtime.MemStats
			runtime.ReadMemStats(&memory)
			b.ReportMetric(float64(successful), "successful_queries")
			b.ReportMetric(float64(rejected), "discarded_queries")
			b.ReportMetric(float64(measuredCommits), "ingested_messages")
			if tc.active {
				b.ReportMetric(float64(measuredElapsed)/float64(time.Millisecond), "active_elapsed_ms")
				b.ReportMetric(float64(measuredCommits)/measuredElapsed.Seconds(), "observed_ingestion_per_s")
				if measuredCommits == 0 {
					b.Log("active_load_evidence=NOT_RUN no commits during measured window; rerun with more samples")
				}
			}
			b.ReportMetric(float64(archiveBytes), "archive_bytes")
			b.ReportMetric(float64(walBytes), "wal_bytes")
			b.ReportMetric(float64(peakQueued), "observed_queue_bytes")
			b.ReportMetric(float64(memory.HeapInuse)/(1<<20), "heap_MiB")
			b.Logf("configured_queue_count=%d configured_queue_bytes=%d sample_cap=10000 successful=%d discarded=%d process_rss=external_time_l_required", s.options.BodyQueueCount, s.options.BodyQueueBytes, successful, rejected)
		})
	}
}
