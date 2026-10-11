//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// Roots enter through independent extraction, before any encoder exists.
func derivedTestRoots(t *testing.T, texts []string, clockHooks ...func(time.Time) time.Time) (*Store, *atomic.Int64, string, []Document, MediaID, ArchivePolicy) {
	t.Helper()
	ctx := context.Background()
	s, clock, dir := indexTestStore(t, clockHooks...)
	p := testPolicy(t, s)
	_, media, _ := extractionTestOriginal(t, s, p, "reuse-original", JobOCR)
	work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoadExtractionInput(ctx, work.Lease); err != nil {
		t.Fatal(err)
	}
	var roots []Document
	for i, text := range texts {
		roots = append(roots, extractionTestDocument(work, fmt.Sprintf("reuse-root-%03d", i), text))
	}
	if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: roots}); err != nil || !ok {
		t.Fatalf("independent roots: %v %v", ok, err)
	}
	caption, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobCaption), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoadExtractionInput(ctx, caption.Lease); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: caption.Lease, ContentHash: caption.ContentHash, Outcome: ExtractionEmpty}); err != nil || !ok {
		t.Fatalf("independent empty caption: %v %v", ok, err)
	}
	return s, clock, dir, roots, media, p
}

func derivedTestResult(j Job) JobResult {
	r := JobResult{Lease: j.Lease, Generation: j.Generation, Version: j.Version, Sources: j.Sources, ContentHash: j.ContentHash}
	for _, id := range j.Documents {
		values := make([]float32, j.Version.Dimensions)
		values[0] = 1
		r.Embeddings = append(r.Embeddings, Embedding{DocumentID: id, Values: values})
	}
	return r
}

func derivedTestGeneration(t *testing.T, s *Store, name string, maxTokens int) IndexGeneration {
	t.Helper()
	ctx := context.Background()
	v := indexTestVersion(name)
	v.Dimensions, v.MaxPassageTokens = 3, maxTokens
	v.TokenizerSHA256 = strings.Repeat("b", 64)
	if name == "reuse-old" {
		v.Dimensions, v.TokenizerSHA256 = 2, strings.Repeat("a", 64)
	} else if name == "reuse-third" {
		v.Dimensions, v.TokenizerSHA256 = 4, strings.Repeat("c", 64)
	}
	g, err := s.EnsureIndex(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	p := indexTestPublication(t, s, g, 256)
	for i := range p.Documents {
		p.Documents[i].PassageTokens = 1
	}
	if ok, err := s.PublishWindows(ctx, p); err != nil || !ok {
		t.Fatalf("windows: %v %v", ok, err)
	}
	for {
		j, err := s.ClaimJob(ctx, JobClaim{Account: "account", Kind: JobTextEmbedding, TTL: time.Minute})
		if errors.Is(err, ErrNoWork) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := s.CommitJob(ctx, derivedTestResult(j)); err != nil || !ok {
			t.Fatalf("text embedding: %v %v", ok, err)
		}
	}
	return g
}

func derivedTestClaim(t *testing.T, s *Store) Job {
	t.Helper()
	j, err := s.ClaimJob(context.Background(), JobClaim{Account: "account", Kind: JobDerivedEmbedding, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(j.Documents) != 1 {
		t.Fatalf("origin capability: %#v", j.Documents)
	}
	if _, err = s.LoadJobInput(context.Background(), j.Lease); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("origin accepted legacy input API: %v", err)
	}
	return j
}

func derivedTestReady(t *testing.T, s *Store, j Job) DerivedBatchInput {
	t.Helper()
	for step := 0; step < 20; step++ {
		input, err := s.LoadDerivedBatch(context.Background(), j.Lease)
		if err != nil {
			t.Fatal(err)
		}
		if input.Ready {
			if input.Cursor == "" || input.Origin.ID != j.Documents[0] || input.EndByte-input.StartByte != len(input.Text) || len(input.Text) > extensionMaxPassageBytes || !utf8.ValidString(input.Text) {
				t.Fatalf("invalid ready range: %#v", input)
			}
			hash, err := CanonicalOriginWorkDigest(j.Generation, j.Version, input.Origin)
			if err != nil || hash != j.ContentHash {
				t.Fatalf("whole origin digest: %s %v", hash, err)
			}
			return input
		}
		if input.Text != "" || input.Cursor != "" {
			t.Fatal("preparation exposed body or publish capability")
		}
	}
	t.Fatal("bounded preparation did not finish")
	return DerivedBatchInput{}
}

func derivedTestBatch(input DerivedBatchInput, texts []string) DerivedBatchPublication {
	j := input.Job
	r := JobResult{Lease: j.Lease, Generation: j.Generation, Version: j.Version, Sources: j.Sources, ContentHash: j.ContentHash}
	start := input.StartByte
	for _, text := range texts {
		end := start + len(text)
		d := Document{ID: DocumentID(fmt.Sprintf("%s-%d-%d", j.ID, start, end)), Kind: input.Origin.Provenance.Kind, Text: text, Sources: input.Origin.Sources, PassageTokens: 1, Provenance: input.Origin.Provenance}
		values := make([]float32, j.Version.Dimensions)
		values[0] = 1
		r.Documents = append(r.Documents, d)
		r.Embeddings = append(r.Embeddings, Embedding{DocumentID: d.ID, Values: values})
		start = end
	}
	return DerivedBatchPublication{Result: r, Cursor: input.Cursor}
}

func derivedTestFinish(t *testing.T, s *Store, j Job) {
	t.Helper()
	for step := 0; step < 8; step++ {
		input := derivedTestReady(t, s, j)
		progress, err := s.CommitDerivedBatch(context.Background(), derivedTestBatch(input, []string{input.Text}))
		if err != nil || !progress.Accepted {
			t.Fatalf("derived prefix: %#v %v", progress, err)
		}
		if progress.Complete {
			return
		}
	}
	t.Fatal("whole root did not finish")
}

func derivedTestSnapshot(t *testing.T, s *Store, g IndexGenerationID, root DocumentID) map[DocumentID]string {
	t.Helper()
	rows, err := s.readDB.Query(`SELECT d.id,d.origin_start_byte,d.origin_end_byte,f.normalized_text,v.values_blob FROM extension_documents d LEFT JOIN extension_document_fts f ON f.rowid=d.rowid LEFT JOIN extension_vectors v ON v.document_id=d.id WHERE d.generation=? AND d.origin_document_id=? ORDER BY d.id LIMIT 257`, g, root)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	snapshot := make(map[DocumentID]string)
	for rows.Next() {
		var id DocumentID
		var start, end int
		var lexical sql.NullString
		var vector []byte
		if err = rows.Scan(&id, &start, &end, &lexical, &vector); err != nil {
			t.Fatal(err)
		}
		snapshot[id] = fmt.Sprintf("%d/%d/%t/%s/%x", start, end, lexical.Valid, lexical.String, vector)
		if len(snapshot) > 256 {
			t.Fatal("snapshot fixture exceeds one bounded page")
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestDerivedReuseEvictedOriginalPreservesRootAndOldVectors(t *testing.T) {
	ctx := context.Background()
	s, _, _, roots, media, _ := derivedTestRoots(t, []string{"truthful αβ and ישראל extraction\nsecond passage"})
	var noEncoder, visible, directVector int
	if err := s.readDB.QueryRow(`SELECT (SELECT count(*) FROM extension_generations),(SELECT count(*) FROM extension_root_fts WHERE root_id=?),(SELECT count(*) FROM extension_vectors WHERE document_id=?)`, roots[0].ID, roots[0].ID).Scan(&noEncoder, &visible, &directVector); err != nil || noEncoder != 0 || visible != 1 || directVector != 0 {
		t.Fatalf("independent lexical root: %d/%d/%d %v", noEncoder, visible, directVector, err)
	}
	old := derivedTestGeneration(t, s, "reuse-old", 256)
	derivedTestFinish(t, s, derivedTestClaim(t, s))
	if err := s.ActivateIndex(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	oldSnapshot := derivedTestSnapshot(t, s, old.ID, roots[0].ID)
	artifact, err := s.BeginEviction(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishMediaCleanup(ctx, MediaCleanup{ReservationID: artifact.ReservationID, ArtifactID: artifact.ID, LeaseID: artifact.CleanupLease, Code: "removed"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"reuse-new", "reuse-third"} {
		g := derivedTestGeneration(t, s, name, 128)
		if g.Version.Dimensions == old.Version.Dimensions || g.Version.TokenizerSHA256 == old.Version.TokenizerSHA256 {
			t.Fatal("replacement fixture did not change dimensions and tokenizer identity")
		}
		if err = s.ActivateIndex(ctx, g.ID); !errors.Is(err, ErrNoWork) {
			t.Fatalf("incomplete reuse activated: %v", err)
		}
		active, err := s.ActiveIndex(ctx)
		if err != nil || active.ID != old.ID {
			t.Fatalf("old active generation lost: %#v %v", active, err)
		}
		j := derivedTestClaim(t, s)
		var copied int
		if err = s.readDB.QueryRow(`SELECT count(*) FROM extension_vectors WHERE generation=? AND document_id IN(SELECT id FROM extension_documents WHERE origin_document_id=?)`, g.ID, roots[0].ID).Scan(&copied); err != nil || copied != 0 {
			t.Fatalf("replacement copied old semantic vectors: %d %v", copied, err)
		}
		input := derivedTestReady(t, s, j)
		if input.Origin.Provenance != roots[0].Provenance || !extensionSameSources(input.Origin.Sources, roots[0].Sources) || input.Text != roots[0].Text {
			t.Fatal("replacement changed authoritative extraction")
		}
		derivedTestFinish(t, s, j)
		if !reflect.DeepEqual(derivedTestSnapshot(t, s, old.ID, roots[0].ID), oldSnapshot) {
			t.Fatal("replacement altered old vector or lexical bytes")
		}
		if err = s.ActivateIndex(ctx, g.ID); err != nil {
			t.Fatal(err)
		}
		old, oldSnapshot = g, derivedTestSnapshot(t, s, g.ID, roots[0].ID)
	}
}

func TestDerivedPublicationAdaptivePrefixCompletesMoreThan256Fragments(t *testing.T) {
	ctx := context.Background()
	s, _, _, roots, _, _ := derivedTestRoots(t, []string{strings.Repeat("α", 300)})
	g := derivedTestGeneration(t, s, "dense-derived", 1)
	j := derivedTestClaim(t, s)
	first := derivedTestReady(t, s, j)
	if first.StartByte != 0 || first.EndByte != 600 || first.Origin.Bytes != 600 {
		t.Fatalf("range was artificially truncated: %#v", first)
	}
	fragments := make([]string, 256)
	for i := range fragments {
		fragments[i] = "α"
	}
	batch := derivedTestBatch(first, fragments)
	progress, err := s.CommitDerivedBatch(ctx, batch)
	if err != nil || !progress.Accepted || progress.Complete || progress.Next == "" {
		t.Fatalf("first adaptive prefix: %#v %v", progress, err)
	}
	if err = s.ActivateIndex(ctx, g.ID); !errors.Is(err, ErrNoWork) {
		t.Fatalf("partial root activated: %v", err)
	}
	second := derivedTestReady(t, s, j)
	if second.StartByte != 512 || second.EndByte != 600 || second.Job.ContentHash != first.Job.ContentHash || second.Origin.TextSHA256 != first.Origin.TextSHA256 {
		t.Fatal("continuation lost whole-origin identity or exact prefix")
	}
	fragments = make([]string, 44)
	for i := range fragments {
		fragments[i] = "α"
	}
	last := derivedTestBatch(second, fragments)
	progress, err = s.CommitDerivedBatch(ctx, last)
	if err != nil || !progress.Accepted || !progress.Complete || progress.Next != "" {
		t.Fatalf("EOF prefix: %#v %v", progress, err)
	}
	if replay, err := s.CommitDerivedBatch(ctx, last); err != nil || replay != progress {
		t.Fatalf("EOF exact replay: %#v %v", replay, err)
	}
	var count, bytes, direct int
	if err = s.readDB.QueryRow(`SELECT count(*),sum(length(CAST(text AS BLOB))),(SELECT count(*) FROM extension_vectors WHERE document_id=?) FROM extension_documents WHERE generation=? AND origin_document_id=?`, roots[0].ID, g.ID, roots[0].ID).Scan(&count, &bytes, &direct); err != nil || count != 300 || bytes != 600 || direct != 0 {
		t.Fatalf("whole partition: %d/%d/%d %v", count, bytes, direct, err)
	}
	if err = s.ActivateIndex(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDerivedPublicationLoadsBoundedRangesOf64KiBRoot(t *testing.T) {
	s, _, _, _, _, _ := derivedTestRoots(t, []string{strings.Repeat("a", 64<<10)})
	g := derivedTestGeneration(t, s, "large-origin", 256)
	j := derivedTestClaim(t, s)
	for start := 0; start < 64<<10; start += 16 << 10 {
		input := derivedTestReady(t, s, j)
		if input.StartByte != start || input.EndByte != start+(16<<10) || input.Origin.Bytes != 64<<10 {
			t.Fatalf("unbounded or wrong range: %d..%d", input.StartByte, input.EndByte)
		}
		progress, err := s.CommitDerivedBatch(context.Background(), derivedTestBatch(input, []string{input.Text}))
		if err != nil || !progress.Accepted || progress.Complete != (start == 48<<10) {
			t.Fatalf("range publication: %#v %v", progress, err)
		}
	}
	if err := s.ActivateIndex(context.Background(), g.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDerivedPublicationActivationRejectsOverlapAndCompensatingGap(t *testing.T) {
	ctx := context.Background()
	s, _, _, _, _, _ := derivedTestRoots(t, []string{"abcdef"})
	g := derivedTestGeneration(t, s, "partition-adjacency", 256)
	j := derivedTestClaim(t, s)
	batch := derivedTestBatch(derivedTestReady(t, s, j), []string{"ab", "cd", "ef"})
	if result, err := s.CommitDerivedBatch(ctx, batch); err != nil || !result.Complete {
		t.Fatalf("honest partition: %#v %v", result, err)
	}
	if err := s.extensionIndexCoverageAt(ctx, s.readDB, g.ID, s.now()); err != nil {
		t.Fatalf("fixture has unrelated incomplete work: %v", err)
	}
	// Total bytes, range widths, first start, and final EOF stay unchanged.
	// Only ordered adjacency distinguishes this overlap plus cancelling gap.
	middle := batch.Result.Documents[1].ID
	if _, err := s.writeDB.Exec(`UPDATE extension_documents SET origin_start_byte=1,origin_end_byte=3 WHERE id=?`, middle); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateIndex(ctx, g.ID); !errors.Is(err, ErrNoWork) {
		t.Fatalf("overlap/gap partition activated: %v", err)
	}
	if _, err := s.writeDB.Exec(`UPDATE extension_documents SET origin_start_byte=2,origin_end_byte=4 WHERE id=?`, middle); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateIndex(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDerivedPublicationFinalFenceRollsBackElapsedLease(t *testing.T) {
	ctx := context.Background()
	var armed atomic.Bool
	var calls atomic.Int64
	s, _, _, roots, _, _ := derivedTestRoots(t, []string{"prefix|remaining"}, func(now time.Time) time.Time {
		if armed.Load() && calls.Add(1) >= 7 {
			return now.Add(2 * time.Second)
		}
		return now
	})
	g := derivedTestGeneration(t, s, "final-derived-lease", 256)
	j := derivedTestClaim(t, s)
	if _, err := s.RenewJob(ctx, j.Lease, time.Second); err != nil {
		t.Fatal(err)
	}
	input := derivedTestReady(t, s, j)
	batch := derivedTestBatch(input, []string{"prefix|"})
	armed.Store(true)
	result, err := s.CommitDerivedBatch(ctx, batch)
	armed.Store(false)
	if calls.Load() < 7 || err != nil || result.Accepted {
		t.Fatalf("elapsed final lease published: %#v %v calls=%d", result, err, calls.Load())
	}
	var rows int
	if err = s.readDB.QueryRow(`SELECT count(*) FROM extension_documents WHERE generation=? AND origin_document_id=?`, g.ID, roots[0].ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("late final fence retained partial fragments: %d %v", rows, err)
	}
	reloaded := derivedTestReady(t, s, j)
	if reloaded.Cursor != input.Cursor || reloaded.StartByte != 0 {
		t.Fatal("final lease failure advanced durable cursor")
	}
}

func TestDerivedPublicationRejectsForgeryAndRollsBackOnlyCurrentBatch(t *testing.T) {
	ctx := context.Background()
	s, _, _, roots, _, _ := derivedTestRoots(t, []string{"first part\nsecond part"})
	g := derivedTestGeneration(t, s, "prefix-atomic", 256)
	j := derivedTestClaim(t, s)
	input := derivedTestReady(t, s, j)
	first := derivedTestBatch(input, []string{"first part\n"})
	progress, err := s.CommitDerivedBatch(ctx, first)
	if err != nil || !progress.Accepted || progress.Complete {
		t.Fatalf("first prefix: %#v %v", progress, err)
	}
	snapshot := derivedTestSnapshot(t, s, g.ID, roots[0].ID)
	if replay, err := s.CommitDerivedBatch(ctx, first); err != nil || replay != progress {
		t.Fatalf("prefix replay: %#v %v", replay, err)
	}
	changed := first
	changed.Result.Documents = extensionCloneDocuments(first.Result.Documents)
	changed.Result.Documents[0].Text = "forged"
	if _, err = s.CommitDerivedBatch(ctx, changed); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("changed replay accepted: %v", err)
	}
	next := derivedTestReady(t, s, j)
	valid := derivedTestBatch(next, []string{next.Text})
	for _, mutation := range []string{"text", "provenance", "source", "global-id", "vector"} {
		t.Run(mutation, func(t *testing.T) {
			bad := valid
			bad.Result.Documents = extensionCloneDocuments(valid.Result.Documents)
			bad.Result.Embeddings = append([]Embedding(nil), valid.Result.Embeddings...)
			switch mutation {
			case "text":
				bad.Result.Documents[0].Text = "fabrication"
			case "provenance":
				bad.Result.Documents[0].Provenance.ProfileID = "different-profile"
			case "source":
				bad.Result.Documents[0].Sources[0].Version.Revision++
			case "global-id":
				bad.Result.Documents[0].ID = DocumentID(fmt.Sprintf("%s-0-%d", j.ID, len(next.Text)))
			case "vector":
				bad.Result.Embeddings[0].Values = []float32{0, 0, 0}
			}
			if _, err := s.CommitDerivedBatch(ctx, bad); !errors.Is(err, ErrInvalidExtension) {
				t.Fatalf("forgery accepted: %v", err)
			}
			if !reflect.DeepEqual(derivedTestSnapshot(t, s, g.ID, roots[0].ID), snapshot) {
				t.Fatal("failed current batch changed earlier prefix")
			}
		})
	}
	if _, err = s.writeDB.Exec(`CREATE TEMP TRIGGER reject_derived_vector BEFORE INSERT ON extension_vectors BEGIN SELECT RAISE(ABORT,'synthetic vector failure'); END`); err != nil {
		t.Fatal(err)
	}
	if result, err := s.CommitDerivedBatch(ctx, valid); err == nil || result.Accepted {
		t.Fatalf("vector failure was accepted: %#v %v", result, err)
	}
	if !reflect.DeepEqual(derivedTestSnapshot(t, s, g.ID, roots[0].ID), snapshot) {
		t.Fatal("SQL rollback removed earlier accepted prefix")
	}
	if _, err = s.writeDB.Exec(`DROP TRIGGER reject_derived_vector`); err != nil {
		t.Fatal(err)
	}
	if result, err := s.CommitDerivedBatch(ctx, valid); err != nil || !result.Complete {
		t.Fatalf("retry after rollback: %#v %v", result, err)
	}
	if result, err := s.CommitDerivedBatch(ctx, first); err != nil || result.Accepted {
		t.Fatalf("older receipt revived: %#v %v", result, err)
	}
}

func TestDerivedPublicationReopenRecoversPrefixWithNewLease(t *testing.T) {
	ctx := context.Background()
	s, clock, dir, roots, _, _ := derivedTestRoots(t, []string{"accepted prefix|remaining bytes"})
	g := derivedTestGeneration(t, s, "prefix-reopen", 256)
	j := derivedTestClaim(t, s)
	first := derivedTestBatch(derivedTestReady(t, s, j), []string{"accepted prefix|"})
	if result, err := s.CommitDerivedBatch(ctx, first); err != nil || !result.Accepted || result.Complete {
		t.Fatalf("prefix: %#v %v", result, err)
	}
	snapshot := derivedTestSnapshot(t, s, g.ID, roots[0].ID)
	clock.Add(int64((2 * time.Minute) / time.Millisecond))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, dir, Options{Now: func() time.Time { return time.UnixMilli(clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	recovered := derivedTestClaim(t, s2)
	if recovered.ID != j.ID || recovered.ContentHash != j.ContentHash || recovered.Lease.LeaseID == j.Lease.LeaseID || recovered.Attempts != 2 {
		t.Fatal("recovery upgraded identity or reused capability")
	}
	input := derivedTestReady(t, s2, recovered)
	if input.StartByte != len("accepted prefix|") || input.Cursor == first.Cursor || input.Text != "remaining bytes" {
		t.Fatal("durable prefix or cursor was lost")
	}
	if stale, err := s2.CommitDerivedBatch(ctx, first); err != nil || stale.Accepted {
		t.Fatalf("stale lease replay: %#v %v", stale, err)
	}
	if !reflect.DeepEqual(derivedTestSnapshot(t, s2, g.ID, roots[0].ID), snapshot) {
		t.Fatal("reclaim erased accepted prefix")
	}
	derivedTestFinish(t, s2, recovered)
	if err = s2.ActivateIndex(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDerivedPublicationSourceEditPurgesPrefixAndRejectsLateCursor(t *testing.T) {
	ctx := context.Background()
	s, _, _, roots, _, policy := derivedTestRoots(t, []string{"accepted prefix|remaining bytes"})
	g := derivedTestGeneration(t, s, "source-edit-prefix", 256)
	j := derivedTestClaim(t, s)
	first := derivedTestBatch(derivedTestReady(t, s, j), []string{"accepted prefix|"})
	if result, err := s.CommitDerivedBatch(ctx, first); err != nil || !result.Accepted {
		t.Fatalf("prefix: %#v %v", result, err)
	}
	next := derivedTestReady(t, s, j)
	late := derivedTestBatch(next, []string{next.Text})
	apply(t, s, event("reuse-original", "edited source", 2, EventEditContent), policy.Generation)
	if result, err := s.CommitDerivedBatch(ctx, late); err != nil || result.Accepted {
		t.Fatalf("late revoked prefix: %#v %v", result, err)
	}
	var residual int
	if err := s.readDB.QueryRow(`SELECT (SELECT count(*) FROM extension_roots WHERE id=?)+(SELECT count(*) FROM extension_documents WHERE origin_document_id=?)+(SELECT count(*) FROM extension_publications WHERE work_id=?)+(SELECT count(*) FROM extension_root_fts WHERE root_id=?)`, roots[0].ID, roots[0].ID, j.ID, roots[0].ID).Scan(&residual); err != nil || residual != 0 {
		t.Fatalf("revoked roots/prefix/receipt resurrected: %d %v", residual, err)
	}
	if err := s.ActivateIndex(ctx, g.ID); !errors.Is(err, ErrNoWork) {
		t.Fatalf("edited generation activated: %v", err)
	}
}

func TestDerivedOriginJobRepairRestoresCascadedOriginLink(t *testing.T) {
	ctx := context.Background()
	s, _, _, roots, _, _ := derivedTestRoots(t, []string{"truthful repair input"})
	g := derivedTestGeneration(t, s, "origin-link-repair", 256)
	j := derivedTestClaim(t, s)
	if _, err := s.writeDB.Exec(`UPDATE extension_jobs SET state='cancelled',lease_id='',lease_until=0 WHERE id=?`, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec(`DELETE FROM extension_job_origins WHERE job_id=?`, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.extensionTransaction(ctx, "account", func(tx *sql.Tx) error {
		origin, err := s.extensionAuthorizeRoot(ctx, tx, roots[0].ID)
		if err != nil {
			return err
		}
		return extensionEnqueueOriginJob(ctx, tx, g, origin)
	}); err != nil {
		t.Fatal(err)
	}
	repaired := derivedTestClaim(t, s)
	if repaired.ID != j.ID || len(repaired.Documents) != 1 || repaired.Documents[0] != roots[0].ID || repaired.Lease.LeaseID == j.Lease.LeaseID {
		t.Fatal("exact origin repair remained unclaimable")
	}
	derivedTestFinish(t, s, repaired)
}

func TestDerivedPublicationScopesSafetyToAuthoritativeJobAccount(t *testing.T) {
	s, _, _, _, _, _ := derivedTestRoots(t, []string{"safe account extraction"})
	derivedTestGeneration(t, s, "scoped-origin-publication", 256)
	j := derivedTestClaim(t, s)
	s.denyImmediate("unrelated-account", "synthetic unrelated unsafe state")
	derivedTestFinish(t, s, j)
	var state string
	if err := s.readDB.QueryRow(`SELECT state FROM extension_jobs WHERE id=?`, j.ID).Scan(&state); err != nil || state != "complete" {
		t.Fatalf("unrelated unsafe account blocked exact root publication: %s %v", state, err)
	}
}

func TestDerivedRootSeedingIsBoundedAndExistingWorkClaimsFirst(t *testing.T) {
	texts := make([]string, 17)
	for i := range texts {
		texts[i] = fmt.Sprintf("truthful root %d", i)
	}
	s, _, _, _, _, _ := derivedTestRoots(t, texts)
	g := derivedTestGeneration(t, s, "bounded-root-seed", 256)
	count := func() int {
		t.Helper()
		var n int
		if err := s.readDB.QueryRow(`SELECT count(*) FROM extension_job_origins jo JOIN extension_jobs j ON j.id=jo.job_id WHERE j.generation=?`, g.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 16 {
		t.Fatalf("Ensure seed exceeded metadata batch: %d", n)
	}
	for i := 0; i < 16; i++ {
		j := derivedTestClaim(t, s)
		if n := count(); n != 16 {
			t.Fatalf("claim seeded while eligible inventory remained: %d", n)
		}
		derivedTestFinish(t, s, j)
	}
	if err := s.ActivateIndex(context.Background(), g.ID); !errors.Is(err, ErrNoWork) {
		t.Fatalf("unseeded root coverage disappeared: %v", err)
	}
	derivedTestFinish(t, s, derivedTestClaim(t, s))
	if n := count(); n != 17 {
		t.Fatalf("later bounded seed did not progress: %d", n)
	}
	if err := s.ActivateIndex(context.Background(), g.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDerivedLegacyRecoverySkipsUnverifiableRowsAndKeepsOneBatchBound(t *testing.T) {
	ctx := context.Background()
	s, _, _ := indexTestStore(t)
	p := testPolicy(t, s)
	ref, media, _ := extractionTestOriginal(t, s, p, "legacy-root", JobOCR)
	g, err := s.EnsureIndex(ctx, indexTestVersion("legacy-recovery"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.extensionTransaction(ctx, "account", func(tx *sql.Tx) error {
		for i := 0; i < 33; i++ {
			id, currentMedia := fmt.Sprintf("z-truthful-%03d", i), media
			if i < 16 {
				id, currentMedia = fmt.Sprintf("a-unverifiable-%03d", i), MediaID("missing-association")
			}
			d := Document{ID: DocumentID(id), Kind: DocumentOCR, Text: strings.Repeat("a", 4096), Sources: []SourceRef{ref}, PassageTokens: 1, Provenance: DerivedProvenance{Kind: DocumentOCR, MediaID: currentMedia, ProfileID: "legacy-profile", ExtractorVersion: "legacy-v1", OffsetKind: "none"}}
			if err := extensionSaveDocument(ctx, tx, g, d); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var before, after ArchiveRevision
	if err = s.readDB.QueryRow(`SELECT archive_revision FROM archive_meta WHERE id=1`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	claim := derivedTestClaim(t, s)
	var roots, bytes, invented int
	if err = s.readDB.QueryRow(`SELECT count(*),coalesce(sum(text_bytes),0),(SELECT count(*) FROM extension_extraction_jobs) FROM extension_roots`).Scan(&roots, &bytes, &invented); err != nil || roots != 16 || bytes != 64<<10 || invented != 0 {
		t.Fatalf("legacy verification batch/proof: %d/%d/%d %v", roots, bytes, invented, err)
	}
	if err = s.readDB.QueryRow(`SELECT archive_revision FROM archive_meta WHERE id=1`).Scan(&after); err != nil || after <= before {
		t.Fatalf("legacy root insertion did not invalidate search cursors: %v", err)
	}
	input := derivedTestReady(t, s, claim)
	if !strings.HasPrefix(string(input.Origin.ID), "z-truthful-") || input.Origin.Provenance.ProfileID != "legacy-profile" {
		t.Fatal("legacy verification invented identity or starved truthful later rows")
	}
}
