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

func publicationTestReady(t *testing.T, s *Store, work DirtyWork) WindowBatchInput {
	t.Helper()
	for step := 0; step < 20; step++ {
		input, err := s.LoadWindowBatch(context.Background(), work)
		if err != nil {
			t.Fatal(err)
		}
		if input.Ready {
			if input.Cursor == "" {
				t.Fatal("ready window lacks publish capability")
			}
			return input
		}
		if len(input.Messages) != 0 || input.Cursor != "" {
			t.Fatal("metadata preparation exposed source bodies or publish capability")
		}
	}
	t.Fatal("bounded window preparation did not finish")
	return WindowBatchInput{}
}

// This fixture gives every byte an honest source span. A small tokenizer's
// document count can exceed 256 without dropping the rest of the source page.
func publicationTestPrefix(input WindowBatchInput, v IndexVersion, maxDocs, passageBytes int) WindowBatchPublication {
	p := WindowBatchPublication{Work: input.Work, Version: v, Cursor: input.Cursor}
	ordinal, field, start := input.Start.SourceOrdinal, input.Start.Field, input.Start.StartByte
	for ordinal < len(input.Messages) && len(p.Documents) < maxDocs {
		m := input.Messages[ordinal]
		text := m.Body
		if field == "caption" {
			text = m.Caption
		}
		if start == len(text) {
			start = 0
			if field == "body" {
				field = "caption"
			} else {
				ordinal++
				field = "body"
			}
			continue
		}
		end := start + passageBytes
		if end > len(text) {
			end = len(text)
		}
		p.Documents = append(p.Documents, Document{ID: DocumentID(fmt.Sprintf("%s-%s-%d-%s-%d-%d", input.Work.Generation, input.Work.LeaseID, ordinal, field, start, end)), Kind: DocumentWindow, Text: text[start:end], Sources: []SourceRef{m.Source}, Spans: []SourceSpan{{Source: m.Source, Field: field, StartByte: start, EndByte: end}}, PassageTokens: 1, Provenance: DerivedProvenance{Kind: DocumentWindow, OffsetKind: "none"}})
		start = end
	}
	return p
}

func publicationTestVectors(t *testing.T, s *Store) {
	t.Helper()
	for {
		j, err := s.ClaimJob(context.Background(), JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
		if errors.Is(err, ErrNoWork) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := s.CommitJob(context.Background(), derivedTestResult(j)); err != nil || !ok {
			t.Fatalf("window vector: %v %v", ok, err)
		}
	}
}

func TestWindowPublicationDense256KiBSourceCompletesAcrossBatches(t *testing.T) {
	ctx := context.Background()
	s, clock, _ := indexTestStore(t)
	p := testPolicy(t, s)
	body := strings.Repeat("a", 256<<10)
	apply(t, s, event("dense-original", body, 1, EventOriginal), p.Generation)
	v := indexTestVersion("dense-original-v2")
	v.MaxPassageTokens = 1
	g, err := s.EnsureIndex(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.ClaimDirty(ctx, DirtyClaim{Account: "account", Generation: g.ID, TTL: time.Minute, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	first := publicationTestReady(t, s, w)
	if len(first.Messages) != 1 || first.Messages[0].Body != body {
		t.Fatal("frozen dense source input was truncated")
	}
	var last WindowBatchPublication
	for batch := 0; batch < 4; batch++ {
		input := publicationTestReady(t, s, w)
		if input.Start.SourceOrdinal != 0 || input.Start.Field != "body" || input.Start.StartByte != batch*(64<<10) || len(input.Messages) != 1 || input.Messages[0].Body != body {
			t.Fatalf("full ordered source page or server prefix changed: %#v", input.Start)
		}
		last = publicationTestPrefix(input, g.Version, 256, 256)
		progress, err := s.PublishWindowBatch(ctx, last)
		if err != nil || !progress.Accepted || progress.Complete != (batch == 3) {
			t.Fatalf("batch %d: %#v %v", batch, progress, err)
		}
		var count, bytes, fields int
		if err = s.readDB.QueryRow(`SELECT count(*),coalesce(sum(length(CAST(text AS BLOB))),0),(SELECT count(*) FROM extension_window_coverage WHERE generation=?) FROM extension_documents WHERE generation=? AND kind='text_window'`, g.ID, g.ID).Scan(&count, &bytes, &fields); err != nil || count != (batch+1)*256 || bytes != (batch+1)*(64<<10) || fields != func() int {
			if batch == 3 {
				return 2
			}
			return 0
		}() {
			t.Fatalf("durable prefix/EOF: %d/%d/%d %v", count, bytes, fields, err)
		}
		if batch != 3 {
			publicationTestVectors(t, s)
			if err = s.ActivateIndex(ctx, g.ID); !errors.Is(err, ErrNoWork) {
				t.Fatalf("incomplete dense source activated: %v", err)
			}
			clock.Add(int64((20 * time.Second) / time.Millisecond))
			until, err := s.RenewDirty(ctx, w, time.Minute)
			if err != nil || !until.After(w.LeaseUntil) {
				t.Fatalf("renewal trusted caller old expiry: %v %v", until, err)
			}
		}
	}
	if replay, err := s.PublishWindowBatch(ctx, last); err != nil || !replay.Accepted || !replay.Complete {
		t.Fatalf("EOF receipt after dirty ack: %#v %v", replay, err)
	}
	publicationTestVectors(t, s)
	if err = s.extensionIndexCoverageAt(ctx, s.readDB, g.ID, s.now()); err != nil {
		t.Fatalf("completed fixture has unrelated incomplete work: %v", err)
	}
	if _, err = s.writeDB.Exec(`UPDATE extension_window_coverage SET bytes=bytes-1 WHERE generation=? AND field='body'`, g.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateIndex(ctx, g.ID); !errors.Is(err, ErrNoWork) {
		t.Fatalf("incorrect whole-field EOF proof activated: %v", err)
	}
	if _, err = s.writeDB.Exec(`UPDATE extension_window_coverage SET bytes=bytes+1 WHERE generation=? AND field='body'`, g.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateIndex(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
}

func TestWindowPublicationFinalFenceRejectsElapsedLeaseAndSource(t *testing.T) {
	for _, fence := range []string{"lease", "source"} {
		t.Run(fence, func(t *testing.T) {
			ctx := context.Background()
			var armed atomic.Bool
			var calls atomic.Int64
			s, clock, _ := indexTestStore(t, func(now time.Time) time.Time {
				if armed.Load() && calls.Add(1) >= 5 {
					return now.Add(2 * time.Second)
				}
				return now
			})
			p := testPolicy(t, s)
			e := event("window-final-"+fence, "prefix|remaining", 1, EventOriginal)
			ttl := time.Second
			if fence == "source" {
				e.ExpiresAt = time.UnixMilli(clock.Load()).Add(time.Second)
				ttl = time.Minute
			}
			apply(t, s, e, p.Generation)
			g, err := s.EnsureIndex(ctx, indexTestVersion("window-final-"+fence))
			if err != nil {
				t.Fatal(err)
			}
			w, err := s.ClaimDirty(ctx, DirtyClaim{Account: "account", Generation: g.ID, TTL: ttl, Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			input := publicationTestReady(t, s, w)
			batch := publicationTestPrefix(input, g.Version, 1, len("prefix|"))
			armed.Store(true)
			result, err := s.PublishWindowBatch(ctx, batch)
			armed.Store(false)
			if calls.Load() < 5 || err != nil || result.Accepted {
				t.Fatalf("elapsed final %s published: %#v %v calls=%d", fence, result, err, calls.Load())
			}
			var rows, coverage int
			if err = s.readDB.QueryRow(`SELECT (SELECT count(*) FROM extension_documents WHERE generation=?),(SELECT count(*) FROM extension_window_coverage WHERE generation=?)`, g.ID, g.ID).Scan(&rows, &coverage); err != nil || rows != 0 || coverage != 0 {
				t.Fatalf("late final fence retained partial rows: %d/%d %v", rows, coverage, err)
			}
			reloaded := publicationTestReady(t, s, w)
			if reloaded.Cursor != input.Cursor || reloaded.Start != input.Start {
				t.Fatal("final fence failure advanced durable cursor")
			}
		})
	}
}

func TestWindowPublicationReclaimPreservesPrefixAndRejectsOldCursor(t *testing.T) {
	ctx := context.Background()
	s, clock, dir := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("window-reclaim", "first|second|third", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(ctx, indexTestVersion("window-reclaim-v2"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.ClaimDirty(ctx, DirtyClaim{Account: "account", Generation: g.ID, TTL: time.Minute, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	first := publicationTestPrefix(publicationTestReady(t, s, w), g.Version, 1, len("first|"))
	progress, err := s.PublishWindowBatch(ctx, first)
	if err != nil || !progress.Accepted || progress.Complete {
		t.Fatalf("first prefix: %#v %v", progress, err)
	}
	if replay, err := s.PublishWindowBatch(ctx, first); err != nil || replay != progress {
		t.Fatalf("exact replay: %#v %v", replay, err)
	}
	changed := first
	changed.Documents = extensionCloneDocuments(first.Documents)
	changed.Documents[0].Text = "forged"
	if _, err = s.PublishWindowBatch(ctx, changed); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("changed replay: %v", err)
	}
	clock.Add(int64((2 * time.Minute) / time.Millisecond))
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir, Options{Now: func() time.Time { return time.UnixMilli(clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recovered, err := s.ClaimDirty(ctx, DirtyClaim{Account: "account", Generation: g.ID, TTL: time.Minute, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != w.ID || recovered.LeaseID == w.LeaseID || recovered.DirtyRevision != w.DirtyRevision {
		t.Fatal("reclaim upgraded captured work or reused lease")
	}
	next := publicationTestReady(t, s, recovered)
	if next.Start.StartByte != len("first|") || next.Cursor == first.Cursor {
		t.Fatal("accepted prefix or capability rotation lost")
	}
	if stale, err := s.PublishWindowBatch(ctx, first); err != nil || stale.Accepted {
		t.Fatalf("stale receipt: %#v %v", stale, err)
	}
	gap := publicationTestPrefix(next, g.Version, 2, 16)
	gap.Documents[0].Spans[0].StartByte++
	if _, err = s.PublishWindowBatch(ctx, gap); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("gap accepted: %v", err)
	}
	last := publicationTestPrefix(next, g.Version, 2, 16)
	if result, err := s.PublishWindowBatch(ctx, last); err != nil || !result.Complete {
		t.Fatalf("recovered EOF: %#v %v", result, err)
	}
	var bytes int
	if err = s.readDB.QueryRow(`SELECT sum(length(CAST(text AS BLOB))) FROM extension_documents WHERE generation=?`, g.ID).Scan(&bytes); err != nil || bytes != len("first|second|third") {
		t.Fatalf("earlier prefix lost or duplicated: %d %v", bytes, err)
	}
}

func TestWindowPublicationSourceEditRevokesCursorAndAllPrefixCoverage(t *testing.T) {
	ctx := context.Background()
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("window-edit", "first|remaining", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(ctx, indexTestVersion("window-edit-v2"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.ClaimDirty(ctx, DirtyClaim{Account: "account", Generation: g.ID, TTL: time.Minute, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	first := publicationTestPrefix(publicationTestReady(t, s, w), g.Version, 1, len("first|"))
	if progress, err := s.PublishWindowBatch(ctx, first); err != nil || !progress.Accepted {
		t.Fatalf("prefix: %#v %v", progress, err)
	}
	late := publicationTestPrefix(publicationTestReady(t, s, w), g.Version, 1, 16)
	apply(t, s, event("window-edit", "changed authoritative source", 2, EventEditContent), p.Generation)
	if progress, err := s.PublishWindowBatch(ctx, late); err != nil || progress.Accepted {
		t.Fatalf("late edited source: %#v %v", progress, err)
	}
	var residual int
	if err = s.readDB.QueryRow(`SELECT (SELECT count(*) FROM extension_publications WHERE work_id=?)+(SELECT count(*) FROM extension_documents WHERE generation=?)+(SELECT count(*) FROM extension_window_coverage WHERE generation=?)`, w.ID, g.ID, g.ID).Scan(&residual); err != nil || residual != 0 {
		t.Fatalf("source edit retained receipt, prefix or coverage: %d %v", residual, err)
	}
}

func TestWindowPublicationSQLFailurePreservesEarlierPrefixAndCursor(t *testing.T) {
	ctx := context.Background()
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("window-rollback", "accepted|remaining", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(ctx, indexTestVersion("window-rollback-v2"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.ClaimDirty(ctx, DirtyClaim{Account: "account", Generation: g.ID, TTL: time.Minute, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	first := publicationTestPrefix(publicationTestReady(t, s, w), g.Version, 1, len("accepted|"))
	if result, err := s.PublishWindowBatch(ctx, first); err != nil || !result.Accepted || result.Complete {
		t.Fatalf("first prefix: %#v %v", result, err)
	}
	input := publicationTestReady(t, s, w)
	last := publicationTestPrefix(input, g.Version, 1, 16)
	if _, err = s.writeDB.Exec(`CREATE TEMP TRIGGER reject_window_batch BEFORE INSERT ON extension_documents BEGIN SELECT RAISE(ABORT,'synthetic window failure'); END`); err != nil {
		t.Fatal(err)
	}
	if result, err := s.PublishWindowBatch(ctx, last); err == nil || result.Accepted {
		t.Fatalf("failed SQL batch accepted: %#v %v", result, err)
	}
	if _, err = s.writeDB.Exec(`DROP TRIGGER reject_window_batch`); err != nil {
		t.Fatal(err)
	}
	reloaded := publicationTestReady(t, s, w)
	if reloaded.Cursor != input.Cursor || reloaded.Start != input.Start {
		t.Fatal("SQL rollback advanced server cursor")
	}
	var bytes, fields int
	if err = s.readDB.QueryRow(`SELECT coalesce(sum(length(CAST(text AS BLOB))),0),(SELECT count(*) FROM extension_window_coverage WHERE generation=?) FROM extension_documents WHERE generation=?`, g.ID, g.ID).Scan(&bytes, &fields); err != nil || bytes != len("accepted|") || fields != 0 {
		t.Fatalf("SQL rollback lost earlier prefix or fabricated EOF: %d/%d %v", bytes, fields, err)
	}
	if result, err := s.PublishWindowBatch(ctx, last); err != nil || !result.Complete {
		t.Fatalf("same capability retry: %#v %v", result, err)
	}
}

func TestWindowPublicationPreparationDeletesAtMost256MetadataRowsPerLoad(t *testing.T) {
	ctx := context.Background()
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("window-preparation", strings.Repeat("a", 1024), 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(ctx, indexTestVersion("window-preparation-v2"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.ClaimDirty(ctx, DirtyClaim{Account: "account", Generation: g.ID, TTL: time.Minute, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	for {
		batch := publicationTestPrefix(publicationTestReady(t, s, w), g.Version, 256, 1)
		progress, err := s.PublishWindowBatch(ctx, batch)
		if err != nil || !progress.Accepted {
			t.Fatalf("old publication: %#v %v", progress, err)
		}
		if progress.Complete {
			break
		}
	}
	var revision ArchiveRevision
	if err = s.readDB.QueryRow(`SELECT archive_revision FROM archive_meta WHERE id=1`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec(`INSERT INTO extension_dirty(id,account,chat_id,generation,policy_generation,from_time,until_time,dirty_revision) VALUES('prepare-again',?,?,?,?,?,?,?)`, w.Account, w.ChatID, g.ID, p.Generation, unixms(w.From), unixms(w.Until), revision); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.ClaimDirty(ctx, DirtyClaim{Account: "account", Generation: g.ID, TTL: time.Minute, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	remaining := 1024
	for step := 0; step < 4; step++ {
		input, err := s.LoadWindowBatch(ctx, replacement)
		if err != nil || input.Ready || len(input.Messages) != 0 {
			t.Fatalf("cleanup was not metadata-only: %#v %v", input, err)
		}
		var count, coverage int
		remaining -= 256
		if err = s.readDB.QueryRow(`SELECT (SELECT count(*) FROM extension_documents WHERE generation=?),(SELECT count(*) FROM extension_window_coverage WHERE generation=?)`, g.ID, g.ID).Scan(&count, &coverage); err != nil || count != remaining || coverage != 0 {
			t.Fatalf("cleanup step %d: %d/%d %v", step, count, coverage, err)
		}
	}
	input := publicationTestReady(t, s, replacement)
	first := publicationTestPrefix(input, g.Version, 256, 1)
	if progress, err := s.PublishWindowBatch(ctx, first); err != nil || !progress.Accepted || progress.Complete {
		t.Fatalf("new prefix: %#v %v", progress, err)
	}
	if input = publicationTestReady(t, s, replacement); input.Start.StartByte != 256 {
		t.Fatal("later load restarted obsolete cleanup")
	}
	var retained int
	if err = s.readDB.QueryRow(`SELECT count(*) FROM extension_documents WHERE generation=?`, g.ID).Scan(&retained); err != nil || retained != 256 {
		t.Fatalf("preparation deleted successful new prefix: %d %v", retained, err)
	}
}

func TestWindowPublicationCombinedReferenceLimitRejectsBeforeWriterAdmission(t *testing.T) {
	ctx := context.Background()
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	apply(t, s, event("combined-reference-limit", "bounded body", 1, EventOriginal), p.Generation)
	g, err := s.EnsureIndex(ctx, indexTestVersion("combined-reference-limit"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.ClaimDirty(ctx, DirtyClaim{Account: "account", Generation: g.ID, TTL: time.Minute, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	input := publicationTestReady(t, s, w)
	ref := input.Messages[0].Source
	documents := make([]Document, 256)
	for i := range documents {
		d := Document{ID: DocumentID(fmt.Sprintf("%s-reference-%d", g.ID, i)), Kind: DocumentWindow, Text: "bounded body", PassageTokens: 1}
		for member := 0; member < 4; member++ {
			d.Sources = append(d.Sources, ref)
			d.Spans = append(d.Spans, SourceSpan{Source: ref, Field: "body", EndByte: len(d.Text)})
		}
		documents[i] = d
	}
	// Sources=1024 and spans=1024 meet the combined boundary. This call
	// checks DTO bounds only, before chronological/source-set validation.
	if _, err = extensionDocumentBounds(documents, extensionMaxInputBytes); err != nil {
		t.Fatalf("exact combined boundary rejected: %v", err)
	}
	documents[len(documents)-1].Spans = append(documents[len(documents)-1].Spans, documents[0].Spans[0])
	if _, err = extensionDocumentBounds(documents, extensionMaxInputBytes); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("combined2049 admitted: %v", err)
	}
	conn, err := s.writeDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if progress, err := s.PublishWindowBatch(bounded, WindowBatchPublication{Work: w, Version: g.Version, Cursor: input.Cursor, Documents: documents}); !errors.Is(err, ErrLimitExceeded) || progress.Accepted {
		t.Fatalf("oversized DTO reached blocked writer: %#v %v", progress, err)
	}
	if _, err := CanonicalWorkDigest(g.ID, g.Version, JobTextEmbedding, "", []SourceRef{ref}, documents); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("digest admitted oversized combined references: %v", err)
	}
}
