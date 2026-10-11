//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

func indexTestVersion(id string) IndexVersion {
	return IndexVersion{ID: id, EncoderProfile: "test-encoder", TokenizerSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TokenizerImplementation: "test-tokenizer", PrefixVersion: "test-prefix", ChunkingVersion: "test-chunks", NormalizationVersion: LexicalNormalizationVersion, Dimensions: 2, MaxPassageTokens: 256}
}

func indexTestStore(t *testing.T, clockHooks ...func(time.Time) time.Time) (*Store, *atomic.Int64, string) {
	t.Helper()
	if len(clockHooks) > 1 {
		t.Fatal("index test store accepts at most one clock hook")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clock := new(atomic.Int64)
	clock.Store(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC).UnixMilli())
	s, err := Open(context.Background(), dir, Options{Now: func() time.Time {
		now := time.UnixMilli(clock.Load())
		if len(clockHooks) == 1 && clockHooks[0] != nil {
			return clockHooks[0](now)
		}
		return now
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, clock, dir
}

func indexTestPublication(t *testing.T, s *Store, g IndexGeneration, limit int) WindowPublication {
	t.Helper()
	w, err := s.ClaimDirty(context.Background(), DirtyClaim{Account: "account", Generation: g.ID, TTL: time.Minute, Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	input, err := s.LoadWindowMessages(context.Background(), w)
	if err != nil {
		t.Fatal(err)
	}
	p := WindowPublication{Work: w, Version: g.Version}
	for i, m := range input {
		p.Documents = append(p.Documents, Document{ID: DocumentID(fmt.Sprintf("%s-%s-%d", g.ID, w.LeaseID, i)), Kind: DocumentWindow, Text: m.Body, Sources: []SourceRef{m.Source}, Spans: []SourceSpan{{Source: m.Source, Field: "body", StartByte: 0, EndByte: len(m.Body)}}, PassageTokens: 4, Provenance: DerivedProvenance{Kind: DocumentWindow, OffsetKind: "none"}})
	}
	return p
}

func indexTestPublishedJob(t *testing.T, s *Store, g IndexGeneration) Job {
	t.Helper()
	p := indexTestPublication(t, s, g, 256)
	if ok, err := s.PublishWindows(context.Background(), p); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}
	j, err := s.ClaimJob(context.Background(), JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func indexTestResult(j Job) JobResult {
	r := JobResult{Lease: j.Lease, Generation: j.Generation, Version: j.Version, Sources: j.Sources, ContentHash: j.ContentHash}
	for _, id := range j.Documents {
		r.Embeddings = append(r.Embeddings, Embedding{DocumentID: id, Values: []float32{1, 0}})
	}
	return r
}

func TestIndexExpiredLeaseCannotCommitAndRecoversAfterReopen(t *testing.T) {
	ctx := context.Background()
	s, clock, dir := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("lease", "durable embedding input", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(ctx, indexTestVersion("lease-v1"))
	if err != nil {
		t.Fatal(err)
	}
	j := indexTestPublishedJob(t, s, g)
	clock.Add(int64((2 * time.Minute) / time.Millisecond))
	if ok, err := s.CommitJob(ctx, indexTestResult(j)); err != nil || ok {
		t.Fatalf("expired commit: %v %v", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, dir, Options{Now: func() time.Time { return time.UnixMilli(clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	recovered, err := s2.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != j.ID || recovered.Lease.LeaseID == j.Lease.LeaseID || recovered.Attempts != 2 {
		t.Fatal("crashed job lease was not recovered with a new identity")
	}
	input, err := s2.LoadJobInput(ctx, recovered.Lease)
	if err != nil || len(input.Documents) != 1 || input.Documents[0].Text != "durable embedding input" {
		t.Fatalf("recovered input: %d %v", len(input.Documents), err)
	}
	if ok, err := s2.CommitJob(ctx, indexTestResult(j)); err != nil || ok {
		t.Fatalf("old lease: %v %v", ok, err)
	}
	if ok, err := s2.CommitJob(ctx, indexTestResult(recovered)); err != nil || !ok {
		t.Fatalf("current lease: %v %v", ok, err)
	}
}

func TestIndexDirtyAcknowledgementKeepsNewerRevision(t *testing.T) {
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("first", "first input", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(context.Background(), indexTestVersion("dirty-v1"))
	if err != nil {
		t.Fatal(err)
	}
	publication := indexTestPublication(t, s, g, 256)
	e := event("second", "new input", 1, EventOriginal)
	e.OriginalSentAt = e.OriginalSentAt.Add(time.Minute)
	apply(t, s, e, p.Generation)
	if ok, err := s.PublishWindows(context.Background(), publication); err != nil || !ok {
		t.Fatalf("unchanged old members: %v %v", ok, err)
	}
	next, err := s.ClaimDirty(context.Background(), DirtyClaim{Account: "account", Generation: g.ID, TTL: time.Minute, Limit: 256})
	if err != nil || next.DirtyRevision <= publication.Work.DirtyRevision {
		t.Fatalf("new dirt erased: revision=%d err=%v", next.DirtyRevision, err)
	}
	input, err := s.LoadWindowMessages(context.Background(), next)
	if err != nil || len(input) != 2 {
		t.Fatalf("expanded neighboring range: count=%d err=%v", len(input), err)
	}
}

func TestIndexDirtyPagesBoundIdenticalTimestamps(t *testing.T) {
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	for i := 0; i < 7; i++ {
		apply(t, s, event(fmt.Sprintf("page-%d", i), "bounded input", 1, EventOriginal), p.Generation)
	}
	g, err := s.EnsureIndex(context.Background(), indexTestVersion("paged-v1"))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[MessageID]bool)
	for page := 0; page < 4; page++ {
		publication := indexTestPublication(t, s, g, 2)
		if len(publication.Documents) > 2 {
			t.Fatal("claim escaped its hard message count")
		}
		for _, d := range publication.Documents {
			id := d.Sources[0].Version.MessageID
			if seen[id] {
				t.Fatal("same-timestamp cursor repeated source")
			}
			seen[id] = true
		}
		if ok, err := s.PublishWindows(context.Background(), publication); err != nil || !ok {
			t.Fatalf("page: %v %v", ok, err)
		}
	}
	if len(seen) != 7 {
		t.Fatalf("source coverage: %d", len(seen))
	}
	if _, err := s.ClaimDirty(context.Background(), DirtyClaim{Account: "account", Generation: g.ID, TTL: time.Minute, Limit: 2}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("completed range: %v", err)
	}
}

func TestIndexSourceEditAndPolicyNarrowingRejectLatePublication(t *testing.T) {
	for _, change := range []string{"edit", "policy"} {
		t.Run(change, func(t *testing.T) {
			s, _, _ := indexTestStore(t)
			p := testPolicy(t, s)
			apply(t, s, event("changed", "old private input", 1, EventOriginal), p.Generation)
			g, err := s.EnsureIndex(context.Background(), indexTestVersion("stale-v1"))
			if err != nil {
				t.Fatal(err)
			}
			publication := indexTestPublication(t, s, g, 256)
			if change == "edit" {
				apply(t, s, event("changed", "edited input", 2, EventEditContent), p.Generation)
			} else if _, err = s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective}, p.Generation); err != nil {
				t.Fatal(err)
			}
			if ok, err := s.PublishWindows(context.Background(), publication); err != nil || ok {
				t.Fatalf("stale publication: %v %v", ok, err)
			}
			var count int
			if err := s.readDB.QueryRow("SELECT count(*) FROM extension_documents").Scan(&count); err != nil || count != 0 {
				t.Fatalf("stale documents: %d %v", count, err)
			}
		})
	}
}

func TestIndexGenerationSwitchRequiresCoverageAndKeepsOldActive(t *testing.T) {
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("generation", "generation input", 1, EventOriginal), p.Generation)
	ctx := context.Background()
	first, err := s.EnsureIndex(ctx, indexTestVersion("first"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateIndex(ctx, first.ID); err == nil {
		t.Fatal("activated incomplete coverage")
	}
	j := indexTestPublishedJob(t, s, first)
	if ok, err := s.CommitJob(ctx, indexTestResult(j)); err != nil || !ok {
		t.Fatalf("commit: %v %v", ok, err)
	}
	if err = s.ActivateIndex(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := s.EnsureIndex(ctx, indexTestVersion("second"))
	if err != nil {
		t.Fatal(err)
	}
	var active IndexGenerationID
	if err = s.readDB.QueryRow("SELECT id FROM extension_generations WHERE state='active'").Scan(&active); err != nil || active != first.ID {
		t.Fatal("building replacement displaced active index")
	}
	if err = s.ActivateIndex(ctx, second.ID); err == nil {
		t.Fatal("activated unbuilt replacement")
	}
	j = indexTestPublishedJob(t, s, second)
	if ok, err := s.CommitJob(ctx, indexTestResult(j)); err != nil || !ok {
		t.Fatalf("replacement commit: %v %v", ok, err)
	}
	if err = s.ActivateIndex(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.readDB.QueryRow("SELECT id FROM extension_generations WHERE state='active'").Scan(&active); err != nil || active != second.ID {
		t.Fatal("replacement did not activate atomically")
	}
	conflict := second.Version
	conflict.Dimensions++
	if _, err = s.EnsureIndex(ctx, conflict); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("mutated model space accepted: %v", err)
	}
}

func TestIndexCommitRejectsNonfiniteWrongDimensionsAndIncompleteMembers(t *testing.T) {
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("vector", "vector input", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(context.Background(), indexTestVersion("vectors"))
	if err != nil {
		t.Fatal(err)
	}
	j := indexTestPublishedJob(t, s, g)
	for _, values := range [][]float32{{1}, {float32(math.NaN()), 0}, {2, 0}, {0, 0}} {
		r := indexTestResult(j)
		r.Embeddings[0].Values = values
		if ok, err := s.CommitJob(context.Background(), r); ok || !errors.Is(err, ErrInvalidExtension) {
			t.Fatalf("invalid vector: %v %v", ok, err)
		}
	}
	r := indexTestResult(j)
	r.Sources = nil
	if ok, err := s.CommitJob(context.Background(), r); err != nil || ok {
		t.Fatalf("incomplete membership: %v %v", ok, err)
	}
	if ok, err := s.CommitJob(context.Background(), indexTestResult(j)); err != nil || !ok {
		t.Fatalf("valid vector: %v %v", ok, err)
	}
}

func TestIndexPublicationFailureRollsBackDocumentsAndDirtyAcknowledgement(t *testing.T) {
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("atomic-index", "atomic window", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(context.Background(), indexTestVersion("atomic-index"))
	if err != nil {
		t.Fatal(err)
	}
	publication := indexTestPublication(t, s, g, 256)
	if _, err = s.writeDB.Exec("CREATE TRIGGER fail_extension_job BEFORE INSERT ON extension_jobs BEGIN SELECT RAISE(ABORT,'test'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishWindows(context.Background(), publication); err == nil {
		t.Fatal("expected publication rollback")
	}
	for _, table := range []string{"extension_documents", "extension_document_fts", "extension_jobs"} {
		var count int
		if err = s.readDB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s: %d %v", table, count, err)
		}
	}
	var dirt int
	if err = s.readDB.QueryRow("SELECT count(*) FROM extension_dirty").Scan(&dirt); err != nil || dirt != 1 {
		t.Fatalf("lost dirty work: %d %v", dirt, err)
	}
	if _, err = s.writeDB.Exec("DROP TRIGGER fail_extension_job"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.PublishWindows(context.Background(), publication); err != nil || !ok {
		t.Fatalf("retry publication: %v %v", ok, err)
	}
}

func TestIndexCancelledBackgroundOperationsKeepKeywordReadable(t *testing.T) {
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("cancel", "available keyword", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(context.Background(), indexTestVersion("cancel"))
	if err != nil {
		t.Fatal(err)
	}
	j := indexTestPublishedJob(t, s, g)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, call := range []func() error{
		func() error {
			_, e := s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
			return e
		},
		func() error { _, e := s.RenewJob(ctx, j.Lease, time.Minute); return e },
		func() error {
			return s.RetryJob(ctx, JobRetry{Lease: j.Lease, Code: "model_unavailable", After: time.Minute})
		},
		func() error { _, e := s.CommitJob(ctx, indexTestResult(j)); return e },
	} {
		if err := call(); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	}
	page, err := s.Search(context.Background(), ReadScope{Account: "account", Principal: "owner", PolicyGeneration: p.Generation, AllChats: true}, SearchRequest{Query: "available", Limit: 10})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("background cancellation denied keyword: %d %v", len(page.Hits), err)
	}
}

func TestIndexAllSourceMembersRevalidateBeforeJobInputAndCommit(t *testing.T) {
	ctx := context.Background()
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	for _, id := range []string{"member-a", "member-b"} {
		apply(t, s, event(id, id, 1, EventOriginal), p.Generation)
	}
	g, err := s.EnsureIndex(ctx, indexTestVersion("all-members"))
	if err != nil {
		t.Fatal(err)
	}
	publication := indexTestPublication(t, s, g, 256)
	a, b := publication.Documents[0], publication.Documents[1]
	a.Text += "\n" + b.Text
	a.Sources = append(a.Sources, b.Sources...)
	a.Spans = append(a.Spans, b.Spans...)
	publication.Documents = []Document{a}
	if ok, err := s.PublishWindows(ctx, publication); err != nil || !ok {
		t.Fatalf("multi-source window: %v %v", ok, err)
	}
	j, err := s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, s, event("member-b", "changed second source", 2, EventEditContent), p.Generation)
	if _, err = s.LoadJobInput(ctx, j.Lease); err == nil {
		t.Fatal("input accepted only the unchanged anchor")
	}
	if ok, err := s.CommitJob(ctx, indexTestResult(j)); err != nil || ok {
		t.Fatalf("stale second member commit: %v %v", ok, err)
	}
}

func TestIndexClockExpiryRejectsLeaseInputAndCompletionWithoutSweep(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := indexTestStore(t)
	p := testPolicy(t, s)
	e := event("clock-source", "clock protected text", 1, EventOriginal)
	e.ExpiresAt = time.UnixMilli(clock.Load()).Add(30 * time.Second)
	apply(t, s, e, p.Generation)
	g, err := s.EnsureIndex(ctx, indexTestVersion("clock-source"))
	if err != nil {
		t.Fatal(err)
	}
	j := indexTestPublishedJob(t, s, g)
	clock.Add(int64(31 * time.Second / time.Millisecond))
	if _, err = s.LoadJobInput(ctx, j.Lease); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("expired input: %v", err)
	}
	if ok, err := s.CommitJob(ctx, indexTestResult(j)); err != nil || ok {
		t.Fatalf("expired source completion: %v %v", ok, err)
	}
	var count int
	if err = s.readDB.QueryRow("SELECT count(*) FROM extension_vectors").Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired vector: %d %v", count, err)
	}
}

func TestIndexDirtyWorkCannotBeForgedOrAcknowledgedWithoutAllMembers(t *testing.T) {
	ctx := context.Background()
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	for _, id := range []string{"covered-a", "covered-b"} {
		apply(t, s, event(id, id, 1, EventOriginal), p.Generation)
	}
	g, err := s.EnsureIndex(ctx, indexTestVersion("coverage"))
	if err != nil {
		t.Fatal(err)
	}
	publication := indexTestPublication(t, s, g, 256)
	forged := publication
	forged.Work.ChatID = "other-chat"
	if ok, err := s.PublishWindows(ctx, forged); err != nil || ok {
		t.Fatalf("forged claim: %v %v", ok, err)
	}
	omitted := publication
	omitted.Documents = omitted.Documents[:1]
	if ok, err := s.PublishWindows(ctx, omitted); ok || !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("incomplete source coverage: %v %v", ok, err)
	}
	if ok, err := s.PublishWindows(ctx, publication); err != nil || !ok {
		t.Fatalf("original claim: %v %v", ok, err)
	}
}

func TestIndexRenewAndRetryUseExactUnexpiredLeaseIdentity(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("retry", "retry input", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(ctx, indexTestVersion("retry"))
	if err != nil {
		t.Fatal(err)
	}
	j := indexTestPublishedJob(t, s, g)
	wrong := j.Lease
	wrong.LeaseID = "incorrect-lease"
	if _, err = s.RenewJob(ctx, wrong, time.Minute); !errors.Is(err, ErrNoWork) {
		t.Fatalf("forged renewal: %v", err)
	}
	if err = s.RetryJob(ctx, JobRetry{Lease: wrong, Code: "model_unavailable", After: time.Minute}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("forged retry: %v", err)
	}
	deadline, err := s.RenewJob(ctx, j.Lease, 2*time.Minute)
	if err != nil || !deadline.Equal(time.UnixMilli(clock.Load()).Add(2*time.Minute)) {
		t.Fatalf("renewal deadline: %v %v", deadline, err)
	}
	if err = s.RetryJob(ctx, JobRetry{Lease: j.Lease, Code: "model_unavailable", After: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("retry busy loop: %v", err)
	}
	clock.Add(int64(2 * time.Minute / time.Millisecond))
	reclaimed, err := s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
	if err != nil || reclaimed.Lease.LeaseID == j.Lease.LeaseID {
		t.Fatalf("retry recovery: %v", err)
	}
	if _, err = s.RenewJob(ctx, j.Lease, time.Minute); !errors.Is(err, ErrNoWork) {
		t.Fatalf("old lease renewal: %v", err)
	}
}

func TestIndexDiskBudgetWaitsPreservePendingJobUntilResourcesReturn(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("disk-wait", "durable quota waiting input", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(ctx, indexTestVersion("disk-wait"))
	if err != nil {
		t.Fatal(err)
	}
	j := indexTestPublishedJob(t, s, g)
	for i := 0; i < 12; i++ {
		beforeRetry := clock.Load()
		if err = s.RetryJob(ctx, JobRetry{Lease: j.Lease, Code: "waiting_for_disk_budget", After: time.Second}); err != nil {
			t.Fatalf("quota wait %d: %v", i, err)
		}
		var retryAt int64
		if err = s.readDB.QueryRow("SELECT retry_at FROM extension_jobs WHERE id=?", j.ID).Scan(&retryAt); err != nil {
			t.Fatal(err)
		}
		if retryAt < beforeRetry+int64(time.Minute/time.Millisecond) || retryAt > beforeRetry+int64(66*time.Second/time.Millisecond) {
			t.Fatalf("quota wait %d stored unbounded backoff: %dms", i, retryAt-beforeRetry)
		}
		if i == 0 {
			if ok, err := s.CommitJob(ctx, indexTestResult(j)); err != nil || ok {
				t.Fatalf("retried lease still commits: %v %v", ok, err)
			}
			if _, err = s.LoadJobInput(ctx, j.Lease); !errors.Is(err, ErrNoWork) {
				t.Fatalf("retried lease still reads input: %v", err)
			}
			if _, err = s.RenewJob(ctx, j.Lease, time.Minute); !errors.Is(err, ErrNoWork) {
				t.Fatalf("retried lease still renews: %v", err)
			}
		}
		clock.Add(int64(59 * time.Second / time.Millisecond))
		if _, err = s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute}); !errors.Is(err, ErrNoWork) {
			t.Fatalf("quota wait %d retried before minimum backoff: %v", i, err)
		}
		clock.Add(int64(61 * time.Second / time.Millisecond))
		previous := j
		j, err = s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
		if err != nil || j.ID != previous.ID || j.Lease.LeaseID == previous.Lease.LeaseID || j.Attempts != 1 {
			t.Fatalf("quota wait %d lost durable work or consumed a failed attempt: job=%+v error=%v", i, j, err)
		}
	}
	if ok, err := s.CommitJob(ctx, indexTestResult(j)); err != nil || !ok {
		t.Fatalf("resources returned: %v %v", ok, err)
	}
	if err = s.ActivateIndex(ctx, g.ID); err != nil {
		t.Fatalf("completed quota-waiting generation: %v", err)
	}
}

func TestIndexWindowCoverageRejectsFabricationGapsAndDerivedKinds(t *testing.T) {
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("coverage-integrity", "original logical text", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(context.Background(), indexTestVersion("coverage-integrity"))
	if err != nil {
		t.Fatal(err)
	}
	publication := indexTestPublication(t, s, g, 256)
	for _, mutation := range []string{"fabricated", "gap", "derived", "overlap", "foreign", "reversed", "adjacent"} {
		t.Run(mutation, func(t *testing.T) {
			candidate := publication
			candidate.Documents = extensionCloneDocuments(publication.Documents)
			d := &candidate.Documents[0]
			switch mutation {
			case "fabricated":
				d.Text = "invented text"
			case "gap":
				d.Text = d.Text[:1]
				d.Spans[0].EndByte = 1
			case "derived":
				d.Kind = DocumentOCR
			case "overlap":
				d.Spans = append(d.Spans, d.Spans[0])
				d.Text += "\n" + d.Text
			case "foreign":
				d.Sources[0].Version.MessageID = "unclaimed-source"
			case "reversed":
				first, second := d.Spans[0], d.Spans[0]
				first.EndByte = 4
				second.StartByte = 4
				d.Spans = []SourceSpan{second, first}
				d.Text = d.Text[4:] + "\n" + d.Text[:4]
			case "adjacent":
				first, second := d.Spans[0], d.Spans[0]
				first.EndByte = 4
				second.StartByte = 4
				d.Spans = []SourceSpan{first, second}
				d.Text = d.Text[:4] + "\n" + d.Text[4:]
			}
			if ok, err := s.PublishWindows(context.Background(), candidate); ok || !errors.Is(err, ErrInvalidExtension) {
				t.Fatalf("invalid %s coverage: %v %v", mutation, ok, err)
			}
		})
	}
	if ok, err := s.PublishWindows(context.Background(), publication); err != nil || !ok {
		t.Fatalf("original bytes: %v %v", ok, err)
	}
}

func TestIndexActivationRequiresEveryFragmentEmbedding(t *testing.T) {
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("fragmented", "two exact fragments", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(context.Background(), indexTestVersion("fragmented"))
	if err != nil {
		t.Fatal(err)
	}
	publication := indexTestPublication(t, s, g, 256)
	first := publication.Documents[0]
	second := extensionCloneDocuments([]Document{first})[0]
	first.Text = first.Text[:4]
	first.Spans[0].EndByte = 4
	second.ID += "-second"
	second.Text = second.Text[4:]
	second.Spans[0].StartByte = 4
	publication.Documents = []Document{first, second}
	if ok, err := s.PublishWindows(context.Background(), publication); err != nil || !ok {
		t.Fatalf("fragments: %v %v", ok, err)
	}
	for i := 0; i < 2; i++ {
		j, err := s.ClaimJob(context.Background(), JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := s.CommitJob(context.Background(), indexTestResult(j)); err != nil || !ok {
			t.Fatalf("fragment vector: %v %v", ok, err)
		}
		if i == 0 {
			if err := s.ActivateIndex(context.Background(), g.ID); err == nil {
				t.Fatal("one fragment vector certified the whole source")
			}
		}
	}
	if err := s.ActivateIndex(context.Background(), g.ID); err != nil {
		t.Fatal(err)
	}
}

func TestIndexDirtyLeaseReplacementRejectsOriginalClaim(t *testing.T) {
	s, clock, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("dirty-lease", "dirty lease input", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(context.Background(), indexTestVersion("dirty-lease"))
	if err != nil {
		t.Fatal(err)
	}
	original := indexTestPublication(t, s, g, 256)
	clock.Add(int64(2 * time.Minute / time.Millisecond))
	replacement := indexTestPublication(t, s, g, 256)
	if replacement.Work.LeaseID == original.Work.LeaseID {
		t.Fatal("dirty lease identity reused")
	}
	if _, err := s.LoadWindowMessages(context.Background(), original.Work); !errors.Is(err, ErrNoWork) {
		t.Fatalf("old dirty input: %v", err)
	}
	if ok, err := s.PublishWindows(context.Background(), original); err != nil || ok {
		t.Fatalf("old dirty publication: %v %v", ok, err)
	}
	if ok, err := s.PublishWindows(context.Background(), replacement); err != nil || !ok {
		t.Fatalf("replacement publication: %v %v", ok, err)
	}
}

func TestIndexAcceptedBackgroundCancellationKeepsArchiveAvailable(t *testing.T) {
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("admitted-cancel", "admitted available keyword", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(context.Background(), indexTestVersion("admitted-cancel"))
	if err != nil {
		t.Fatal(err)
	}
	j := indexTestPublishedJob(t, s, g)
	apply(t, s, event("admitted-publication", "publication available keyword", 1, EventOriginal), p.Generation)
	publication := indexTestPublication(t, s, g, 256)
	for name, call := range map[string]func(context.Context) error{
		"claim": func(ctx context.Context) error {
			_, err := s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
			return err
		},
		"renew": func(ctx context.Context) error { _, err := s.RenewJob(ctx, j.Lease, time.Minute); return err },
		"retry": func(ctx context.Context) error {
			return s.RetryJob(ctx, JobRetry{Lease: j.Lease, Code: "model_unavailable", After: time.Minute})
		},
		"commit":  func(ctx context.Context) error { _, err := s.CommitJob(ctx, indexTestResult(j)); return err },
		"publish": func(ctx context.Context) error { _, err := s.PublishWindows(ctx, publication); return err },
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := s.writeDB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			baseWait := s.writeDB.Stats().WaitCount
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() { result <- call(ctx) }()
			waitArchiveCondition(t, func() bool { return s.writeDB.Stats().WaitCount > baseWait })
			cancel()
			got := <-result
			conn.Close()
			if !errors.Is(got, context.Canceled) {
				t.Fatalf("accepted cancellation: %v", got)
			}
			if err := s.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			page, err := s.Search(context.Background(), ReadScope{Account: "account", Principal: "owner", PolicyGeneration: p.Generation, AllChats: true}, SearchRequest{Query: "available", Limit: 10})
			if err != nil || len(page.Hits) != 2 {
				t.Fatalf("accepted background timeout denied owner: %d %v", len(page.Hits), err)
			}
		})
	}
}

func TestIndexCrashHelper(t *testing.T) {
	dir, stage := os.Getenv("WA_INDEX_TEST_CRASH_DIR"), os.Getenv("WA_INDEX_TEST_CRASH_STAGE")
	if dir == "" || stage == "" {
		return
	}
	ctx := context.Background()
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, err := Open(ctx, dir, Options{Now: func() time.Time { return base }})
	if err != nil {
		t.Fatal(err)
	}
	p := testPolicy(t, s)
	apply(t, s, event("crash-source", "durable crash input", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(ctx, indexTestVersion("crash-source"))
	if err != nil {
		t.Fatal(err)
	}
	publication := indexTestPublication(t, s, g, 256)
	if stage == "dirty_claimed" {
		os.Exit(72)
	}
	crashTrigger := ""
	switch stage {
	case "during_window_job_insert":
		crashTrigger = "CREATE TRIGGER index_crash_hook BEFORE INSERT ON extension_jobs BEGIN SELECT index_test_crash(); END"
	case "during_dirty_ack":
		crashTrigger = "CREATE TRIGGER index_crash_hook BEFORE DELETE ON extension_dirty BEGIN SELECT index_test_crash(); END"
	case "during_vector_insert":
		crashTrigger = "CREATE TRIGGER index_crash_hook AFTER INSERT ON extension_vectors BEGIN SELECT index_test_crash(); END"
	case "during_job_complete":
		crashTrigger = "CREATE TRIGGER index_crash_hook AFTER UPDATE ON extension_jobs WHEN NEW.state='complete' BEGIN SELECT index_test_crash(); END"
	}
	if crashTrigger != "" {
		conn, err := s.writeDB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = conn.Raw(func(raw any) error {
			return raw.(*sqlite3.SQLiteConn).RegisterFunc("index_test_crash", func() int { os.Exit(72); return 0 }, false)
		})
		conn.Close()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.writeDB.Exec(crashTrigger); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := s.PublishWindows(ctx, publication); err != nil || !ok {
		t.Fatalf("publish: %v %v", ok, err)
	}
	if stage == "windows_published" {
		os.Exit(72)
	}
	j, err := s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if stage == "job_claimed" {
		os.Exit(72)
	}
	if ok, err := s.CommitJob(ctx, indexTestResult(j)); err != nil || !ok {
		t.Fatalf("commit: %v %v", ok, err)
	}
	if stage == "vector_committed" {
		os.Exit(72)
	}
	t.Fatal("unknown crash checkpoint")
}

func TestIndexAbruptCrashRecoversDurableWork(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"dirty_claimed", "windows_published", "job_claimed", "vector_committed", "during_window_job_insert", "during_dirty_ack", "during_vector_insert", "during_job_complete"} {
		t.Run(stage, func(t *testing.T) {
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			child := exec.Command(binary, "-test.run=^TestIndexCrashHelper$", "-test.count=1")
			child.Env = append(os.Environ(), "WA_INDEX_TEST_CRASH_DIR="+dir, "WA_INDEX_TEST_CRASH_STAGE="+stage)
			if output, err := child.CombinedOutput(); err == nil {
				t.Fatal("child did not terminate abruptly")
			} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 72 {
				t.Fatalf("child failed before checkpoint: %v %s", err, output)
			}
			now := time.Date(2026, 10, 6, 12, 2, 0, 0, time.UTC)
			s, err := Open(context.Background(), dir, Options{Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err = s.writeDB.Exec("DROP TRIGGER IF EXISTS index_crash_hook"); err != nil {
				t.Fatal(err)
			}
			if stage == "during_window_job_insert" || stage == "during_dirty_ack" {
				var dirt, documents, fts, jobs int
				for table, target := range map[string]*int{"extension_dirty": &dirt, "extension_documents": &documents, "extension_document_fts": &fts, "extension_jobs": &jobs} {
					if err := s.readDB.QueryRow("SELECT count(*) FROM " + table).Scan(target); err != nil {
						t.Fatal(err)
					}
				}
				if dirt != 1 || documents != 0 || fts != 0 || jobs != 0 {
					t.Fatalf("interrupted window transaction persisted partial rows: dirt=%d docs=%d fts=%d jobs=%d", dirt, documents, fts, jobs)
				}
			}
			if stage == "during_vector_insert" || stage == "during_job_complete" {
				var vectors, leased, membership int
				if err := s.readDB.QueryRow("SELECT count(*) FROM extension_vectors").Scan(&vectors); err != nil {
					t.Fatal(err)
				}
				if err := s.readDB.QueryRow("SELECT count(*) FROM extension_jobs WHERE state='leased'").Scan(&leased); err != nil {
					t.Fatal(err)
				}
				if err := s.readDB.QueryRow("SELECT count(*) FROM extension_job_documents").Scan(&membership); err != nil {
					t.Fatal(err)
				}
				if vectors != 0 || leased != 1 || membership != 1 {
					t.Fatalf("interrupted vector transaction persisted partial rows: vectors=%d leased=%d membership=%d", vectors, leased, membership)
				}
			}
			g, err := s.EnsureIndex(context.Background(), indexTestVersion("crash-source"))
			if err != nil {
				t.Fatal(err)
			}
			if stage == "dirty_claimed" || stage == "during_window_job_insert" || stage == "during_dirty_ack" {
				publication := indexTestPublication(t, s, g, 256)
				if len(publication.Documents) != 1 {
					t.Fatal("claimed dirty source lost on crash")
				}
				if ok, err := s.PublishWindows(context.Background(), publication); err != nil || !ok {
					t.Fatalf("recovered publication: %v %v", ok, err)
				}
			}
			if stage != "vector_committed" {
				j, err := s.ClaimJob(context.Background(), JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				if ok, err := s.CommitJob(context.Background(), indexTestResult(j)); err != nil || !ok {
					t.Fatalf("recovered commit: %v %v", ok, err)
				}
			}
			var vectors, dirt int
			if err := s.readDB.QueryRow("SELECT count(*) FROM extension_vectors").Scan(&vectors); err != nil {
				t.Fatal(err)
			}
			if err := s.readDB.QueryRow("SELECT count(*) FROM extension_dirty").Scan(&dirt); err != nil {
				t.Fatal(err)
			}
			if vectors != 1 || dirt != 0 {
				t.Fatalf("inconsistent recovered publication: vectors=%d dirt=%d", vectors, dirt)
			}
			if err := s.ActivateIndex(context.Background(), g.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIndexRepublishingUnchangedDeterministicDocumentPreservesVectorAndJob(t *testing.T) {
	ctx := context.Background()
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("deterministic-a", "unchanged input", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(ctx, indexTestVersion("deterministic"))
	if err != nil {
		t.Fatal(err)
	}
	original := indexTestPublication(t, s, g, 256)
	original.Documents[0].ID = "deterministic-a-document"
	if ok, err := s.PublishWindows(ctx, original); err != nil || !ok {
		t.Fatalf("initial publish: %v %v", ok, err)
	}
	j, err := s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CommitJob(ctx, indexTestResult(j)); err != nil || !ok {
		t.Fatalf("initial commit: %v %v", ok, err)
	}
	adjacent := apply(t, s, event("deterministic-b", "new adjacent input", 1, EventOriginal), p.Generation)
	// Isolate document identity recovery from the separately tested source
	// invalidation integration by explicitly scheduling this complete rebuild.
	if _, err := s.writeDB.Exec("INSERT INTO extension_dirty(id,account,chat_id,generation,policy_generation,from_time,until_time,dirty_revision) VALUES('deterministic-rebuild',?,?,?,?,?,?,?) ON CONFLICT(chat_id,generation) DO UPDATE SET from_time=MIN(from_time,excluded.from_time),until_time=MAX(until_time,excluded.until_time),dirty_revision=excluded.dirty_revision,cursor_time=0,cursor_id=''", original.Work.Account, original.Work.ChatID, original.Work.Generation, p.Generation, unixms(original.Work.From), unixms(original.Work.Until), adjacent.ArchiveRevision); err != nil {
		t.Fatal(err)
	}
	replacement := indexTestPublication(t, s, g, 256)
	for i := range replacement.Documents {
		if replacement.Documents[i].Text == "unchanged input" {
			replacement.Documents[i] = original.Documents[0]
		}
	}
	if ok, err := s.PublishWindows(ctx, replacement); err != nil || !ok {
		t.Fatalf("replacement publish: %v %v", ok, err)
	}
	var vectors, membership int
	if err := s.readDB.QueryRow("SELECT count(*) FROM extension_vectors WHERE document_id=?", original.Documents[0].ID).Scan(&vectors); err != nil {
		t.Fatal(err)
	}
	if err := s.readDB.QueryRow("SELECT count(*) FROM extension_job_documents WHERE job_id=?", j.ID).Scan(&membership); err != nil {
		t.Fatal(err)
	}
	if vectors != 1 || membership != 1 {
		t.Fatalf("unchanged publication lost durable work: vectors=%d membership=%d", vectors, membership)
	}
	next, err := s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CommitJob(ctx, indexTestResult(next)); err != nil || !ok {
		t.Fatalf("adjacent commit: %v %v", ok, err)
	}
	if err := s.ActivateIndex(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
}

func TestIndexFinalFenceRejectsElapsedPublicationLease(t *testing.T) {
	for _, kind := range []string{"job", "window"} {
		t.Run(kind, func(t *testing.T) {
			var armed atomic.Bool
			var calls atomic.Int64
			threshold := int64(3)
			if kind == "window" {
				threshold = 4
			}
			s, _, _ := indexTestStore(t, func(now time.Time) time.Time {
				if armed.Load() && calls.Add(1) >= threshold {
					return now.Add(2 * time.Second)
				}
				return now
			})
			p := testPolicy(t, s)
			apply(t, s, event("final-lease", "final lease input", 1, EventOriginal), p.Generation)
			g, err := s.EnsureIndex(context.Background(), indexTestVersion("final-lease"))
			if err != nil {
				t.Fatal(err)
			}
			var j Job
			var publication WindowPublication
			if kind == "job" {
				j = indexTestPublishedJob(t, s, g)
				if _, err = s.RenewJob(context.Background(), j.Lease, time.Second); err != nil {
					t.Fatal(err)
				}
			} else {
				work, err := s.ClaimDirty(context.Background(), DirtyClaim{Account: "account", Generation: g.ID, TTL: time.Second, Limit: 256})
				if err != nil {
					t.Fatal(err)
				}
				input, err := s.LoadWindowMessages(context.Background(), work)
				if err != nil {
					t.Fatal(err)
				}
				m := input[0]
				publication = WindowPublication{Work: work, Version: g.Version, Documents: []Document{{ID: "final-lease-window", Kind: DocumentWindow, Text: m.Body, Sources: []SourceRef{m.Source}, Spans: []SourceSpan{{Source: m.Source, Field: "body", EndByte: len(m.Body)}}, PassageTokens: 4}}}
			}
			armed.Store(true)
			var ok bool
			if kind == "job" {
				ok, err = s.CommitJob(context.Background(), indexTestResult(j))
			} else {
				ok, err = s.PublishWindows(context.Background(), publication)
			}
			armed.Store(false)
			if calls.Load() < threshold {
				t.Fatalf("final lease clock threshold not reached: calls=%d threshold=%d", calls.Load(), threshold)
			}
			if err != nil || ok {
				t.Fatalf("lease crossed final fence: %v %v", ok, err)
			}
			var vectors int
			if err := s.readDB.QueryRow("SELECT count(*) FROM extension_vectors").Scan(&vectors); err != nil || vectors != 0 {
				t.Fatalf("expired lease vectors: %d %v", vectors, err)
			}
			if kind == "job" {
				if _, err := s.LoadJobInput(context.Background(), j.Lease); err != nil {
					t.Fatalf("late completion lost recoverable lease: %v", err)
				}
			} else {
				if _, err := s.LoadWindowMessages(context.Background(), publication.Work); err != nil {
					t.Fatalf("late publication lost dirty claim: %v", err)
				}
			}
		})
	}
}

func TestIndexRollingCutoffRejectsJobInputAndCompletionWithoutRevision(t *testing.T) {
	s, clock, _ := indexTestStore(t)
	p, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll, RollingRetention: time.Minute}, 0)
	if err != nil {
		t.Fatal(err)
	}
	e := event("rolling-index", "rolling input", 1, EventOriginal)
	e.OriginalSentAt = time.UnixMilli(clock.Load()).Add(-30 * time.Second)
	apply(t, s, e, p.Generation)
	g, err := s.EnsureIndex(context.Background(), indexTestVersion("rolling-index"))
	if err != nil {
		t.Fatal(err)
	}
	j := indexTestPublishedJob(t, s, g)
	clock.Add(int64(31 * time.Second / time.Millisecond))
	if _, err := s.LoadJobInput(context.Background(), j.Lease); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("rolling input: %v", err)
	}
	if ok, err := s.CommitJob(context.Background(), indexTestResult(j)); err != nil || ok {
		t.Fatalf("rolling completion: %v %v", ok, err)
	}
}

func TestIndexOversizedPublicationMetadataRejectsBeforeWriterAdmission(t *testing.T) {
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("metadata-bound", "metadata bounded input", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(context.Background(), indexTestVersion("metadata-bound"))
	if err != nil {
		t.Fatal(err)
	}
	publication := indexTestPublication(t, s, g, 256)
	for _, field := range []string{"provenance", "span", "source"} {
		t.Run(field, func(t *testing.T) {
			candidate := publication
			candidate.Documents = extensionCloneDocuments(publication.Documents)
			switch field {
			case "provenance":
				candidate.Documents[0].Provenance.ProfileID = strings.Repeat("x", 129)
			case "span":
				candidate.Documents[0].Spans[0].Field = strings.Repeat("x", 1024)
			case "source":
				candidate.Documents[0].Sources[0].Account = strings.Repeat("x", 513)
			}
			conn, err := s.writeDB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			baseWait := s.writeDB.Stats().WaitCount
			blockedCtx, cancelBlocked := context.WithCancel(context.Background())
			defer cancelBlocked()
			blockedResult := make(chan error, 1)
			go func() {
				_, err := s.LoadWindowMessages(blockedCtx, publication.Work)
				blockedResult <- err
			}()
			waitArchiveCondition(t, func() bool { return s.writeDB.Stats().WaitCount > baseWait })
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			_, err = s.PublishWindows(ctx, candidate)
			cancel()
			if closeErr := conn.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			select {
			case blockedErr := <-blockedResult:
				if blockedErr != nil {
					t.Fatalf("valid blocked window read: %v", blockedErr)
				}
			case <-time.After(time.Second):
				t.Fatal("valid blocked window read did not resume")
			}
			if !errors.Is(err, ErrInvalidExtension) {
				t.Fatalf("metadata reached blocked writer: %v", err)
			}
		})
	}
}

func TestIndexRecreatedHistoricalDocumentRequeuesExactEmbeddingWork(t *testing.T) {
	ctx := context.Background()
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("historical-a", "stable A", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(ctx, indexTestVersion("historical"))
	if err != nil {
		t.Fatal(err)
	}
	original := indexTestPublication(t, s, g, 256)
	original.Documents[0].ID = "historical-a-document"
	if ok, err := s.PublishWindows(ctx, original); err != nil || !ok {
		t.Fatalf("initial: %v %v", ok, err)
	}
	old, err := s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CommitJob(ctx, indexTestResult(old)); err != nil || !ok {
		t.Fatalf("initial vector: %v %v", ok, err)
	}
	schedule := func(rev ArchiveRevision) {
		t.Helper()
		if _, err := s.writeDB.Exec("INSERT INTO extension_dirty(id,account,chat_id,generation,policy_generation,from_time,until_time,dirty_revision) VALUES('historical-rebuild',?,?,?,?,?,?,?) ON CONFLICT(chat_id,generation) DO UPDATE SET dirty_revision=excluded.dirty_revision,cursor_time=0,cursor_id=''", original.Work.Account, original.Work.ChatID, g.ID, p.Generation, unixms(original.Work.From), unixms(original.Work.Until), rev); err != nil {
			t.Fatal(err)
		}
	}
	b := apply(t, s, event("historical-b", "neighbor B", 1, EventOriginal), p.Generation)
	schedule(b.ArchiveRevision)
	combined := indexTestPublication(t, s, g, 256)
	merged := combined.Documents[0]
	merged.ID = "historical-merged-ab"
	merged.Text += "\n" + combined.Documents[1].Text
	merged.Sources = append(merged.Sources, combined.Documents[1].Sources...)
	merged.Spans = append(merged.Spans, combined.Documents[1].Spans...)
	combined.Documents = []Document{merged}
	if ok, err := s.PublishWindows(ctx, combined); err != nil || !ok {
		t.Fatalf("combined: %v %v", ok, err)
	}
	mergedJob, err := s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CommitJob(ctx, indexTestResult(mergedJob)); err != nil || !ok {
		t.Fatalf("combined vector: %v %v", ok, err)
	}
	deleted := apply(t, s, event("historical-b", "", 2, EventDelete), p.Generation)
	schedule(deleted.ArchiveRevision)
	recreated := indexTestPublication(t, s, g, 256)
	recreated.Documents = []Document{original.Documents[0]}
	if ok, err := s.PublishWindows(ctx, recreated); err != nil || !ok {
		t.Fatalf("recreated: %v %v", ok, err)
	}
	requeued, err := s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if requeued.ID != old.ID || requeued.Lease.LeaseID == old.Lease.LeaseID {
		t.Fatal("historical job identity was not safely re-leased")
	}
	if ok, err := s.CommitJob(ctx, indexTestResult(old)); err != nil || ok {
		t.Fatalf("old historical lease: %v %v", ok, err)
	}
	if ok, err := s.CommitJob(ctx, indexTestResult(requeued)); err != nil || !ok {
		t.Fatalf("requeued vector: %v %v", ok, err)
	}
}

func TestIndexRenewalCannotReturnDeadlineElapsedAtFinalFence(t *testing.T) {
	var armed atomic.Bool
	var calls atomic.Int64
	const threshold = int64(3)
	s, _, _ := indexTestStore(t, func(now time.Time) time.Time {
		if armed.Load() && calls.Add(1) >= threshold {
			return now.Add(2 * time.Second)
		}
		return now
	})
	p := testPolicy(t, s)
	apply(t, s, event("renew-fence", "renew fenced input", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(context.Background(), indexTestVersion("renew-fence"))
	if err != nil {
		t.Fatal(err)
	}
	j := indexTestPublishedJob(t, s, g)
	armed.Store(true)
	_, err = s.RenewJob(context.Background(), j.Lease, time.Second)
	armed.Store(false)
	if calls.Load() < threshold {
		t.Fatalf("renewal clock threshold not reached: calls=%d threshold=%d", calls.Load(), threshold)
	}
	if !errors.Is(err, ErrNoWork) {
		t.Fatalf("renewed deadline elapsed before acknowledgement: %v", err)
	}
	input, err := s.LoadJobInput(context.Background(), j.Lease)
	if err != nil || !input.Job.LeaseUntil.Equal(j.LeaseUntil) {
		t.Fatalf("failed renewal changed recoverable authority: %v", err)
	}
}

func TestIndexReplacingBoundaryWindowRetainsUnclaimedMemberWork(t *testing.T) {
	ctx := context.Background()
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	for i, id := range []string{"boundary-a", "boundary-b", "boundary-c"} {
		e := event(id, "boundary common "+id, 1, EventOriginal)
		e.OriginalSentAt = e.OriginalSentAt.Add(time.Duration(i) * 4 * time.Minute)
		apply(t, s, e, p.Generation)
	}
	g, err := s.EnsureIndex(ctx, indexTestVersion("boundary"))
	if err != nil {
		t.Fatal(err)
	}
	whole := indexTestPublication(t, s, g, 256)
	merged := whole.Documents[0]
	merged.ID = "boundary-whole-window"
	for _, d := range whole.Documents[1:] {
		merged.Text += "\n" + d.Text
		merged.Sources = append(merged.Sources, d.Sources...)
		merged.Spans = append(merged.Spans, d.Spans...)
	}
	whole.Documents = []Document{merged}
	if ok, err := s.PublishWindows(ctx, whole); err != nil || !ok {
		t.Fatalf("wholewindow: %v %v", ok, err)
	}
	var revision ArchiveRevision
	if err = s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec("INSERT INTO extension_dirty(id,account,chat_id,generation,policy_generation,from_time,until_time,dirty_revision) VALUES('boundary-narrow',?,?,?,?,?,?,?)", whole.Work.Account, whole.Work.ChatID, g.ID, p.Generation, unixms(whole.Work.From), unixms(whole.Work.From)+1, revision); err != nil {
		t.Fatal(err)
	}
	narrow := indexTestPublication(t, s, g, 1)
	if len(narrow.Documents) != 1 {
		t.Fatal("narrowclaimescapedrange")
	}
	if ok, err := s.PublishWindows(ctx, narrow); err != nil || !ok {
		t.Fatalf("narrowreplace: %v %v", ok, err)
	}
	reconciled := indexTestPublication(t, s, g, 256)
	if len(reconciled.Documents) != 3 || reconciled.Work.DirtyRevision <= narrow.Work.DirtyRevision {
		t.Fatalf("oldwindow members lost durable work: sources=%d revision=%d", len(reconciled.Documents), reconciled.Work.DirtyRevision)
	}
	if ok, err := s.PublishWindows(ctx, reconciled); err != nil || !ok {
		t.Fatalf("reconcile: %v %v", ok, err)
	}
}
