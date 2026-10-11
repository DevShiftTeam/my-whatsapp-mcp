//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func extractionTestVersion(kind JobKind) ExtractionVersion {
	return ExtractionVersion{ID: "independent-" + string(kind), Kind: kind, ProfileID: "synthetic-" + string(kind), ProfileIdentity: strings.Repeat("a", 64), RuntimeSHA256: strings.Repeat("b", 64), ExtractorVersion: "fixture-v1", PromptVersion: "none-v1", Language: "auto"}
}

func extractionTestOriginal(t *testing.T, s *Store, p ArchivePolicy, name string, kind JobKind) (SourceRef, MediaID, MediaReservation) {
	t.Helper()
	if kind != JobTranscription {
		ref, media := extensionTestMedia(t, s, p, name)
		r := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
		extensionPublish(t, s, r, 64)
		return ref, media, r
	}
	e := event(name, "synthetic voice caption", 1, EventOriginal)
	e.BodyKind = MessageVoice
	e.Media = []MediaReference{{Kind: "voice", ProtocolReference: []byte("authenticated synthetic voice"), MimeType: "audio/ogg", Availability: "not_retained"}}
	result := apply(t, s, e, p.Generation)
	ref, err := s.CurrentSource(context.Background(), e.Key)
	if err != nil {
		t.Fatal(err)
	}
	var media MediaID
	if err = s.readDB.QueryRow("SELECT id FROM extension_media_associations WHERE message_id=? AND active=1", result.MessageID).Scan(&media); err != nil {
		t.Fatal(err)
	}
	r := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	if ok, err := s.PublishOriginal(context.Background(), OriginalPublication{Reservation: r, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
		t.Fatalf("voice publication %v %v", ok, err)
	}
	return ref, media, r
}

func extractionTestDocument(work ExtractionWork, id, text string) Document {
	kind := DocumentOCR
	if work.Version.Kind == JobCaption {
		kind = DocumentCaption
	}
	if work.Version.Kind == JobTranscription {
		kind = DocumentTranscript
	}
	return Document{ID: DocumentID(id), Kind: kind, Text: text, Sources: []SourceRef{work.Source}, Provenance: DerivedProvenance{Kind: kind, MediaID: work.MediaID, ProfileID: work.Version.ProfileID, ExtractorVersion: work.Version.ExtractorVersion, PromptVersion: work.Version.PromptVersion, OffsetKind: "none"}}
}

// Isolate the requested association while preparing real, independently
// committed fixture roots. Other associations retain their original flags.
func extractionCompleteImage(t *testing.T, s *Store, media MediaID) map[JobKind]ExtractionWork {
	t.Helper()
	ctx := context.Background()
	rows, err := s.readDB.Query("SELECT id FROM extension_media_associations WHERE active=1 AND id<>?", media)
	if err != nil {
		t.Fatal(err)
	}
	var others []MediaID
	for rows.Next() {
		var id MediaID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		others = append(others, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range others {
		if _, err = s.writeDB.Exec("UPDATE extension_media_associations SET active=0 WHERE id=?", id); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, id := range others {
			if _, err := s.writeDB.Exec("UPDATE extension_media_associations SET active=1 WHERE id=?", id); err != nil {
				t.Fatal(err)
			}
		}
	}()
	works := make(map[JobKind]ExtractionWork)
	for _, kind := range []JobKind{JobOCR, JobCaption} {
		work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(kind), TTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if work.MediaID != media {
			t.Fatalf("wrong processed fixture association: %s != %s", work.MediaID, media)
		}
		d := extractionTestDocument(work, "root-"+string(work.ID), "truthful synthetic processed image text")
		if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{d}}); err != nil || !ok {
			t.Fatalf("processed fixture publication %v %v", ok, err)
		}
		works[kind] = work
	}
	return works
}

func TestExtractionTextIsIndependentOfEncoderAndKeywordVisible(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	ref, media, reservation := extractionTestOriginal(t, s, p, "independent-root", JobOCR)
	version := extractionTestVersion(JobOCR)
	work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: version, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if work.Source != ref || work.MediaID != media || work.ArtifactID != reservation.ArtifactID || work.OriginalSHA256 != strings.Repeat("a", 64) || work.Version != version || work.ContentHash == "" {
		t.Fatalf("unbound extraction work: %#v", work)
	}
	loaded, err := s.LoadExtractionInput(ctx, work.Lease)
	if err != nil || loaded != work {
		t.Fatalf("input mismatch %#v %v", loaded, err)
	}
	doc := extractionTestDocument(work, "independent-ocr-root", "independentruth authoritative generated text")
	result := ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{doc}}
	if ok, err := s.CommitExtraction(ctx, result); err != nil || !ok {
		t.Fatalf("independent commit: %v %v", ok, err)
	}
	var generations, roots int
	if err = s.readDB.QueryRow("SELECT count(*) FROM extension_generations").Scan(&generations); err != nil || generations != 0 {
		t.Fatalf("extraction invented encoder generation: %d %v", generations, err)
	}
	if err = s.readDB.QueryRow("SELECT count(*) FROM extension_roots").Scan(&roots); err != nil || roots != 1 {
		t.Fatalf("roots not committed: %d %v", roots, err)
	}
	pair, err := s.CandidatePair(ctx, ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, CandidatePairRequest{Search: SearchRequest{Query: "independentruth", Limit: 10}, KeywordOnly: true})
	if err != nil || len(pair.Keyword) != 1 || pair.Keyword[0].Ref.ID != doc.ID {
		t.Fatalf("missing-encoder keyword root unavailable: %#v %v", pair.Keyword, err)
	}
	if _, err = s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: version, TTL: time.Minute}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("completed source extracted again: %v", err)
	}
	if ok, err := s.CommitExtraction(ctx, result); err != nil || !ok {
		t.Fatalf("exact successful replay failed: %v %v", ok, err)
	}
	result.Documents[0].Text = "changed root"
	if ok, err := s.CommitExtraction(ctx, result); err != nil || ok {
		t.Fatalf("changed successful replay accepted: %v %v", ok, err)
	}
}

func TestExtractionVersionIdentityCannotBeRebound(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	extractionTestOriginal(t, s, p, "version-binding", JobOCR)
	v := extractionTestVersion(JobOCR)
	if _, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: v, TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*ExtractionVersion){func(x *ExtractionVersion) { x.Kind = JobCaption }, func(x *ExtractionVersion) { x.ProfileID = "another-profile" }, func(x *ExtractionVersion) { x.ProfileIdentity = strings.Repeat("c", 64) }, func(x *ExtractionVersion) { x.RuntimeSHA256 = strings.Repeat("d", 64) }, func(x *ExtractionVersion) { x.ExtractorVersion = "v2" }, func(x *ExtractionVersion) { x.PromptVersion = "v2" }, func(x *ExtractionVersion) { x.Language = "he" }}
	for i, mutate := range mutations {
		x := v
		mutate(&x)
		if _, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: x, TTL: time.Minute}); !errors.Is(err, ErrInvalidExtension) {
			t.Fatalf("version identity mutation %d accepted: %v", i, err)
		}
	}
	v.ID = "embedding-kind"
	v.Kind = JobDerivedEmbedding
	if _, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: v, TTL: time.Minute}); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("encoder work accepted as extraction: %v", err)
	}
}

func TestExtractionEmptyAndSilentOutcomesAreKindBoundAndAtomic(t *testing.T) {
	for _, kind := range []JobKind{JobOCR, JobCaption, JobTranscription} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			s := testArchive(t)
			p := testPolicy(t, s)
			extractionTestOriginal(t, s, p, "outcome-"+string(kind), kind)
			work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(kind), TTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			valid := ExtractionEmpty
			invalid := ExtractionSilent
			if kind == JobTranscription {
				valid, invalid = ExtractionSilent, ExtractionEmpty
			}
			result := ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: invalid}
			if ok, err := s.CommitExtraction(ctx, result); !errors.Is(err, ErrInvalidExtension) || ok {
				t.Fatalf("wrong kind outcome accepted: %v %v", ok, err)
			}
			result.Outcome = valid
			result.Documents = []Document{extractionTestDocument(work, "unexpected-output", "generated text")}
			if ok, err := s.CommitExtraction(ctx, result); !errors.Is(err, ErrInvalidExtension) || ok {
				t.Fatalf("empty outcome accepted documents: %v %v", ok, err)
			}
			result.Documents = nil
			if ok, err := s.CommitExtraction(ctx, result); err != nil || !ok {
				t.Fatalf("explicit kind-bound outcome rejected: %v %v", ok, err)
			}
			var roots int
			if err = s.readDB.QueryRow("SELECT count(*) FROM extension_roots").Scan(&roots); err != nil || roots != 0 {
				t.Fatalf("empty outcome manufactured roots: %d %v", roots, err)
			}
			if _, err = s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: work.Version, TTL: time.Minute}); !errors.Is(err, ErrNoWork) {
				t.Fatalf("completed empty outcome rerun: %v", err)
			}
		})
	}
}

func TestExtractionRejectsInvalidRootWithoutPartialPublication(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	extractionTestOriginal(t, s, p, "invalid-root", JobOCR)
	work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	valid := extractionTestDocument(work, "atomic-root", "valid extraction text")
	mutations := []func(*Document){func(d *Document) { d.Text = "" }, func(d *Document) { d.PassageTokens = 1 }, func(d *Document) { d.Spans = []SourceSpan{{Source: work.Source, Field: "caption"}} }, func(d *Document) { d.Provenance.ProfileID = "wrong-profile" }, func(d *Document) { d.Provenance.MediaID = "wrong-media" }, func(d *Document) {
		d.Sources = []SourceRef{{Account: "another-account", Version: work.Source.Version, ContentHash: work.Source.ContentHash}}
	}, func(d *Document) { d.Kind = DocumentCaption }}
	for i, mutate := range mutations {
		d := valid
		d.ID = DocumentID(fmt.Sprintf("bad-root-%d", i))
		mutate(&d)
		if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{valid, d}}); !errors.Is(err, ErrInvalidExtension) || ok {
			t.Fatalf("invalid root %d accepted: %v %v", i, ok, err)
		}
		var roots int
		if err = s.readDB.QueryRow("SELECT count(*) FROM extension_roots").Scan(&roots); err != nil || roots != 0 {
			t.Fatalf("invalid publication left roots: %d %v", roots, err)
		}
	}
	large := valid
	large.Text = strings.Repeat("x", 64<<10+1)
	if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{large}}); !errors.Is(err, ErrLimitExceeded) || ok {
		t.Fatalf("unbounded root accepted: %v %v", ok, err)
	}
	if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{valid}}); err != nil || !ok {
		t.Fatalf("valid result rejected after invalid attempts: %v %v", ok, err)
	}
}

func TestExtractionExpiredPinRequiresOwnerAckBeforeNewLease(t *testing.T) {
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, err := Open(ctx, dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := testPolicy(t, s)
	_, media, _ := extractionTestOriginal(t, s, p, "expired-extraction-owner", JobOCR)
	claim := ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Second}
	first, err := s.ClaimExtraction(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	now = first.LeaseUntil
	if _, err = s.LoadExtractionInput(ctx, first.Lease); !errors.Is(err, ErrNoWork) {
		t.Fatalf("expired native input granted: %v", err)
	}
	if _, err = s.ClaimExtraction(ctx, claim); !errors.Is(err, ErrNoWork) {
		t.Fatalf("expired pinned owner reclaimed: %v", err)
	}
	if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: first.Lease, ContentHash: first.ContentHash, Outcome: ExtractionText, Documents: []Document{extractionTestDocument(first, "late-root", "late native result")}}); err != nil || ok {
		t.Fatalf("late lease committed: %v %v", ok, err)
	}
	if _, err = s.BeginEviction(ctx, media); !errors.Is(err, ErrNoWork) {
		t.Fatalf("expired native owner evicted: %v", err)
	}
	var pinned bool
	if err = s.readDB.QueryRow("SELECT input_pinned FROM extension_extraction_jobs WHERE id=?", first.ID).Scan(&pinned); err != nil || !pinned {
		t.Fatalf("TTL freed native pin: %v %v", pinned, err)
	}
	retry := JobRetry{Lease: first.Lease, Code: "cancelled", After: time.Second}
	if err = s.RetryExtraction(ctx, retry); err != nil {
		t.Fatal(err)
	}
	if err = s.RetryExtraction(ctx, retry); err != nil {
		t.Fatalf("owner acknowledgement not idempotent: %v", err)
	}
	now = now.Add(time.Second)
	second, err := s.ClaimExtraction(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Lease == first.Lease {
		t.Fatalf("reclaim did not replace settled authority: %#v", second)
	}
	if err = s.RetryExtraction(ctx, retry); !errors.Is(err, ErrNoWork) {
		t.Fatalf("old owner acknowledged replacement lease: %v", err)
	}
	if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: first.Lease, ContentHash: first.ContentHash, Outcome: ExtractionEmpty}); err != nil || ok {
		t.Fatalf("old owner completed replacement: %v %v", ok, err)
	}
	if deadline, err := s.RenewExtraction(ctx, second.Lease, time.Minute); err != nil || !deadline.After(second.LeaseUntil) {
		t.Fatalf("renewal %v %v", deadline, err)
	}
}

func TestExtractionRevokedSourceAndUnsafeRecoveryRetainPinUntilAck(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	_, media, r := extractionTestOriginal(t, s, p, "revoked-extraction", JobOCR)
	work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	apply(t, s, event("revoked-extraction", "", 2, EventDelete), p.Generation)
	if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{extractionTestDocument(work, "revoked-root", "obsolete generated text")}}); err != nil || ok {
		t.Fatalf("revoked source published: %v %v", ok, err)
	}
	if items, err := s.ReconcileMedia(ctx, 10); err != nil || len(items) != 0 {
		t.Fatalf("revoked native pin entered cleanup: %#v %v", items, err)
	}
	if _, err = s.InspectMediaReservation(ctx, r); !errors.Is(err, ErrNoWork) {
		t.Fatalf("inspector exposed pinned cleanup: %v", err)
	}
	if _, err = s.BeginEviction(ctx, media); !errors.Is(err, ErrNoWork) {
		t.Fatalf("source invalidation erased pin: %v", err)
	}
	var lease LeaseID
	if err = s.readDB.QueryRow("SELECT cleanup_lease FROM extension_media_artifacts WHERE id=?", r.ArtifactID).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	if lease == "" {
		lease = "fixture-cleanup-lease"
		if _, err = s.writeDB.Exec("UPDATE extension_media_artifacts SET cleanup_lease=? WHERE id=?", lease, r.ArtifactID); err != nil {
			t.Fatal(err)
		}
	}
	cleanup := MediaCleanup{ReservationID: r.ID, ArtifactID: r.ArtifactID, LeaseID: lease, Code: "removed"}
	if err = s.FinishMediaCleanup(ctx, cleanup); !errors.Is(err, ErrNoWork) {
		t.Fatalf("cleanup acknowledged active native input: %v", err)
	}
	s.safetyMu.Lock()
	s.unsafe["account"] = true
	s.unclean = true
	s.safetyMu.Unlock()
	if err = s.RetryExtraction(ctx, JobRetry{Lease: JobLease{JobID: work.ID, LeaseID: "wrong"}, Code: "cancelled", Terminal: true}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("wrong owner released pin: %v", err)
	}
	if err = s.RetryExtraction(ctx, JobRetry{Lease: work.Lease, Code: "cancelled", Terminal: true}); err != nil {
		t.Fatalf("unsafe source blocked body-free owner ack: %v", err)
	}
	items, err := s.ReconcileMedia(ctx, 10)
	if err != nil || len(items) != 1 || items[0].ID != r.ArtifactID {
		t.Fatalf("settled invalid source not recoverable: %#v %v", items, err)
	}
	if err = s.FinishMediaCleanup(ctx, cleanup); err != nil {
		t.Fatal(err)
	}
	if extensionMediaCharge(t, s, BudgetOriginals) != 0 {
		t.Fatal("stopped-owner cleanup retained charged bytes")
	}
	if _, err = s.Search(ctx, ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "synthetic", Limit: 10}); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("owner cleanup cleared source denial: %v", err)
	}
}

func TestExtractionCancellationAndBudgetWaitNeverQuarantineKeyword(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	ref, media, _ := extractionTestOriginal(t, s, p, "background-extraction", JobOCR)
	work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	checks := []func() error{func() error {
		_, e := s.ClaimExtraction(cancelled, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobCaption), TTL: time.Minute})
		return e
	}, func() error { _, e := s.LoadExtractionInput(cancelled, work.Lease); return e }, func() error { _, e := s.RenewExtraction(cancelled, work.Lease, time.Minute); return e }, func() error {
		_, e := s.CommitExtraction(cancelled, ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionEmpty})
		return e
	}, func() error { return s.RetryExtraction(cancelled, JobRetry{Lease: work.Lease, Code: "cancelled"}) }}
	for i, check := range checks {
		if err = check(); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel operation %d: %v", i, err)
		}
	}
	extensionAdmit(t, s, ref, media, BudgetDecoded, extensionDecodedQuota)
	if _, err = s.AdmitMedia(ctx, MediaAdmissionRequest{Source: ref, MediaID: media, Budget: BudgetDecoded, MaximumBytes: 1, TTL: time.Minute}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("decoded competition: %v", err)
	}
	if err = s.RetryExtraction(ctx, JobRetry{Lease: work.Lease, Code: "waiting_for_disk_budget", After: time.Minute}); err != nil {
		t.Fatal(err)
	}
	var outcome string
	var pinned bool
	if err = s.readDB.QueryRow("SELECT outcome,input_pinned FROM extension_extraction_jobs WHERE id=?", work.ID).Scan(&outcome, &pinned); err != nil || outcome != "" || pinned {
		t.Fatalf("budget wait invented completion or retained stopped pin: %q %v %v", outcome, pinned, err)
	}
	if page, err := s.Search(ctx, ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "synthetic", Limit: 10}); err != nil || len(page.Hits) == 0 {
		t.Fatalf("background failure quarantined keyword: %#v %v", page, err)
	}
}

func TestExtractionTranscriptTimingBoundedBeforeEncoderExists(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	extractionTestOriginal(t, s, p, "long-timing", JobTranscription)
	work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobTranscription), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	doc := extractionTestDocument(work, "timing-root", "bounded generated transcript")
	doc.Provenance.OffsetKind = "slice"
	doc.Provenance.EndMS = int64(24*time.Hour/time.Millisecond) + 1
	if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{doc}}); !errors.Is(err, ErrInvalidExtension) || ok {
		t.Fatalf("poisonous future semantic provenance accepted: %v %v", ok, err)
	}
	var roots int
	if err = s.readDB.QueryRow("SELECT count(*) FROM extension_roots").Scan(&roots); err != nil || roots != 0 {
		t.Fatalf("invalid timing persisted: %d %v", roots, err)
	}
}

type extractionFinalClockCommand struct {
	result            ExtractionResult
	advance, prepared *atomic.Bool
}

func (c extractionFinalClockCommand) priority() bool                    { return false }
func (c extractionFinalClockCommand) account() string                   { return "account" }
func (c extractionFinalClockCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c extractionFinalClockCommand) run(s *Store, ctx context.Context) (any, error) {
	err := s.extensionTransaction(ctx, c.account(), func(tx *sql.Tx) error {
		ok, err := s.extensionCommitExtraction(ctx, tx, c.result)
		if err != nil {
			return err
		}
		if !ok {
			return ErrInvalidExtension
		}
		id := c.result.Documents[0].ID
		var roots, sources, fts, outputs, origins int
		if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM extension_roots WHERE id=?),(SELECT count(*) FROM extension_root_sources WHERE root_id=?),(SELECT count(*) FROM extension_root_fts WHERE root_id=?),(SELECT count(*) FROM extension_extraction_outputs WHERE root_id=?),(SELECT count(*) FROM extension_job_origins WHERE root_id=?)`, id, id, id, id, id).Scan(&roots, &sources, &fts, &outputs, &origins); err != nil {
			return fixedSQLError(err)
		}
		if roots != 1 || sources != 1 || fts != 1 || outputs != 1 || origins != 1 {
			return ErrInvalidExtension
		}
		c.prepared.Store(true)
		c.advance.Store(true)
		return nil
	})
	return nil, err
}

func TestExtractionLateFinalSourceClockRollsBackWholePublication(t *testing.T) {
	ctx := context.Background()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var advance atomic.Bool
	var prepared atomic.Bool
	s, err := Open(ctx, dir, Options{Now: func() time.Time {
		if advance.Load() {
			return base.Add(2 * time.Second)
		}
		return base
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := testPolicy(t, s)
	e := event("final-extraction-clock", "synthetic clock image", 1, EventOriginal)
	e.BodyKind = MessageImage
	e.ExpiresAt = base.Add(time.Second)
	e.Media = []MediaReference{{Kind: "image", ProtocolReference: []byte("authenticated synthetic reference"), Availability: "not_retained"}}
	r := apply(t, s, e, p.Generation)
	ref, err := s.CurrentSource(ctx, e.Key)
	if err != nil {
		t.Fatal(err)
	}
	var media MediaID
	if err = s.readDB.QueryRow("SELECT id FROM extension_media_associations WHERE message_id=? AND active=1", r.MessageID).Scan(&media); err != nil {
		t.Fatal(err)
	}
	extensionPublish(t, s, extensionAdmit(t, s, ref, media, BudgetOriginals, 64), 32)
	if _, err = s.EnsureIndex(ctx, indexTestVersion("final-extraction-clock-encoder")); err != nil {
		t.Fatal(err)
	}
	work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var beforeRevision ArchiveRevision
	if err = s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&beforeRevision); err != nil {
		t.Fatal(err)
	}
	result := ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{extractionTestDocument(work, "late-clock-root", "obsolete clock text")}}
	if _, err = extensionExtractionBounds(result); err != nil {
		t.Fatal(err)
	}
	// Advance only after the real transaction has prepared every root and its
	// origin-backed semantic job. Capture the raw final source error rather
	// than accepting another reason mapped by the public port to false,nil.
	if _, err = s.submitExtension(ctx, extractionFinalClockCommand{result: result, advance: &advance, prepared: &prepared}, 4096); !errors.Is(err, ErrSourceUnavailable) || !prepared.Load() {
		t.Fatalf("publication did not reach the final source fence: prepared%v err%v", prepared.Load(), err)
	}
	for _, table := range []string{"extension_roots", "extension_root_sources", "extension_root_fts", "extension_extraction_outputs", "extension_job_origins"} {
		var count int
		if err = s.readDB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial late commit in %s: %d %v", table, count, err)
		}
	}
	var state, outcome, digest string
	var lease LeaseID
	var pinned bool
	if err = s.readDB.QueryRow("SELECT state,outcome,input_pinned,completion_digest,lease_id FROM extension_extraction_jobs WHERE id=?", work.ID).Scan(&state, &outcome, &pinned, &digest, &lease); err != nil || state != "leased" || outcome != "" || !pinned || digest != "" || lease != work.Lease.LeaseID {
		t.Fatalf("late guard altered completion/pin: %s %s %v %s %s %v", state, outcome, pinned, digest, lease, err)
	}
	var afterRevision ArchiveRevision
	if err = s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&afterRevision); err != nil || afterRevision != beforeRevision {
		t.Fatalf("late guard altered archive revision %d/%d %v", beforeRevision, afterRevision, err)
	}
}

func TestExtractionBLOBLeaseAndIgnoredCASCannotReportCompletion(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	extractionTestOriginal(t, s, p, "corrupt-lease-cas", JobOCR)
	work, err := s.ClaimExtraction(context.Background(), ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	result := ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionEmpty}
	if _, err = s.writeDB.Exec("UPDATE extension_extraction_jobs SET lease_id=CAST(? AS BLOB) WHERE id=?", work.Lease.LeaseID, work.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoadExtractionInput(context.Background(), work.Lease); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("BLOB lease loaded native input %v", err)
	}
	if _, err = s.RenewExtraction(context.Background(), work.Lease, time.Minute); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("BLOB lease renewed %v", err)
	}
	if ok, err := s.CommitExtraction(context.Background(), result); ok || !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("BLOB lease reported empty quality %v %v", ok, err)
	}
	if err = s.RetryExtraction(context.Background(), JobRetry{Lease: work.Lease, Code: "cancelled", After: time.Second}); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("BLOB lease reported pin settlement %v", err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_extraction_jobs SET lease_id=? WHERE id=?", work.Lease.LeaseID, work.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec(`CREATE TEMP TRIGGER ignore_extraction_cas BEFORE UPDATE ON extension_extraction_jobs BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RenewExtraction(context.Background(), work.Lease, time.Minute); !errors.Is(err, ErrNoWork) {
		t.Fatalf("zero-row renewal reported success %v", err)
	}
	if ok, err := s.CommitExtraction(context.Background(), result); ok || err != nil {
		t.Fatalf("zero-row completion reported accepted quality %v %v", ok, err)
	}
	if err = s.RetryExtraction(context.Background(), JobRetry{Lease: work.Lease, Code: "cancelled", After: time.Second}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("zero-row pin settlement reported success %v", err)
	}
	var state, outcome, digest string
	var pinned bool
	if err = s.readDB.QueryRow("SELECT state,outcome,completion_digest,input_pinned FROM extension_extraction_jobs WHERE id=?", work.ID).Scan(&state, &outcome, &digest, &pinned); err != nil || state != "leased" || outcome != "" || digest != "" || !pinned {
		t.Fatalf("failed CAS mutated completion %s %s %s %v %v", state, outcome, digest, pinned, err)
	}
	if _, err = s.writeDB.Exec("DROP TRIGGER ignore_extraction_cas"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CommitExtraction(context.Background(), result); err != nil || !ok {
		t.Fatalf("restored exact lease failed %v %v", ok, err)
	}
}

func TestExtractionRootsAndOutcomesSurviveEvictionAndReuseSameSHA(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	ref, media, first := extractionTestOriginal(t, s, p, "redownload-extraction", JobOCR)
	epoch := func() uint64 { s.safetyMu.RLock(); defer s.safetyMu.RUnlock(); return s.safetyEpoch }
	beforeRoots := epoch()
	var beforeRevision, afterRevision ArchiveRevision
	if err := s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&beforeRevision); err != nil {
		t.Fatal(err)
	}
	works := extractionCompleteImage(t, s, media)
	if epoch() != beforeRoots {
		t.Fatal("initial immutable roots invalidated privacy epoch")
	}
	if err := s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&afterRevision); err != nil || afterRevision <= beforeRevision {
		t.Fatalf("root append did not advance archive revision: %v %v", afterRevision, err)
	}
	_, unrelated, _ := extractionTestOriginal(t, s, p, "unrelated-root-append", JobOCR)
	beforeAppend := epoch()
	extractionCompleteImage(t, s, unrelated)
	if epoch() != beforeAppend {
		t.Fatal("unrelated immutable root append invalidated privacy epoch")
	}
	root := DocumentID("root-" + string(works[JobOCR].ID))
	cleanupOriginal := func() {
		t.Helper()
		artifact, err := s.BeginEviction(ctx, media)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.FinishMediaCleanup(ctx, MediaCleanup{ReservationID: artifact.ReservationID, ArtifactID: artifact.ID, LeaseID: artifact.CleanupLease, Code: "removed"}); err != nil {
			t.Fatal(err)
		}
	}
	cleanupOriginal()
	if _, err := s.extensionLoadRoot(ctx, s.readDB, root); err != nil {
		t.Fatalf("ordinary file eviction removed authoritative text: %v", err)
	}
	second := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	if second.ArtifactID == first.ArtifactID {
		t.Fatal("redownload reused file ownership identity")
	}
	beforeSameSHA := epoch()
	extensionPublish(t, s, second, 64)
	if epoch() != beforeSameSHA {
		t.Fatal("same-SHA redownload invalidated live root readers")
	}
	if _, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute}); !errors.Is(err, ErrNoWork) {
		t.Fatalf("same verified SHA reran native extraction: %v", err)
	}
	if got, err := s.extensionLoadRoot(ctx, s.readDB, root); err != nil || got.ID != root || got.PassageTokens != 0 || len(got.Spans) != 0 {
		t.Fatalf("same-SHA root changed: %#v %v", got, err)
	}
	cleanupOriginal()
	third := extensionAdmit(t, s, ref, media, BudgetOriginals, 128)
	different := OriginalPublication{Reservation: third, SHA256: strings.Repeat("c", 64), Bytes: 64, ActualFormat: "png"}
	beforeRollback := epoch()
	if _, err := s.submitExtension(ctx, extractionRollbackPublication{publication: different}, 1024); !errors.Is(err, ErrNoWork) {
		t.Fatalf("forced publication rollback %v", err)
	}
	if epoch() != beforeRollback {
		t.Fatal("rolled-back SHA invalidation advanced privacy epoch")
	}
	if _, err := s.extensionLoadRoot(ctx, s.readDB, root); err != nil {
		t.Fatalf("rollback destroyed old root authority: %v", err)
	}
	beforeChangedSHA := epoch()
	if ok, err := s.PublishOriginal(ctx, OriginalPublication{Reservation: third, SHA256: strings.Repeat("c", 64), Bytes: 64, ActualFormat: "png"}); err != nil || !ok {
		t.Fatalf("changed SHA publication %v %v", ok, err)
	}
	if epoch() <= beforeChangedSHA {
		t.Fatal("successful changed SHA did not invalidate prior reader epoch")
	}
	if _, err := s.extensionLoadRoot(ctx, s.readDB, root); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("older original SHA root reactivated: %v", err)
	}
	var roots int
	if err := s.readDB.QueryRow("SELECT count(*) FROM extension_roots WHERE media_id=?", media).Scan(&roots); err != nil || roots != 0 {
		t.Fatalf("changed SHA left obsolete text: %d %v", roots, err)
	}
	work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil || work.ArtifactID != third.ArtifactID || work.OriginalSHA256 != strings.Repeat("c", 64) {
		t.Fatalf("changed SHA did not require new native work: %#v %v", work, err)
	}
}

type extractionRollbackPublication struct{ publication OriginalPublication }

func (c extractionRollbackPublication) sensitivity() extensionSensitivity { return extensionBackground }
func (c extractionRollbackPublication) priority() bool                    { return false }
func (c extractionRollbackPublication) account() string {
	return c.publication.Reservation.Source.Account
}
func (c extractionRollbackPublication) run(s *Store, ctx context.Context) (any, error) {
	err := s.extensionTransaction(ctx, c.account(), func(tx *sql.Tx) error {
		ok, err := s.extensionPublishOriginal(ctx, tx, c.publication)
		if err != nil {
			return err
		}
		if !ok {
			return ErrInvalidExtension
		}
		return ErrNoWork
	})
	return nil, err
}

func TestExtractionRemappedSameSHAArtifactRejectsOldPhysicalLease(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	_, media, r := extractionTestOriginal(t, s, p, "remapped-extraction", JobOCR)
	work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec(`INSERT INTO extension_media_artifacts(id,reservation_id,media_id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,budget,maximum_bytes,charged_bytes,lease_id,lease_until,state,sha256,bytes,actual_format,created_at,updated_at) SELECT 'remapped-artifact','remapped-reservation',media_id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,budget,maximum_bytes,charged_bytes,'remapped-download-lease',lease_until,'retained',sha256,bytes,actual_format,created_at,updated_at FROM extension_media_artifacts WHERE id=?`, r.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_media_associations SET artifact_id='remapped-artifact' WHERE id=?", media); err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec("UPDATE extension_media_artifacts SET state='evicting' WHERE id=?", r.ArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoadExtractionInput(ctx, work.Lease); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("same hash granted different physical input: %v", err)
	}
	if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionEmpty}); err != nil || ok {
		t.Fatalf("old physical lease completed replacement: %v %v", ok, err)
	}
	if items, err := s.ReconcileMedia(ctx, 10); err != nil || len(items) != 0 {
		t.Fatalf("remap discarded old native ownership: %#v %v", items, err)
	}
	if err = s.RetryExtraction(ctx, JobRetry{Lease: work.Lease, Code: "source_unavailable", Terminal: true}); err != nil {
		t.Fatal(err)
	}
	if items, err := s.ReconcileMedia(ctx, 10); err != nil || len(items) != 1 || items[0].ID != r.ArtifactID {
		t.Fatalf("settled old artifact remained pinned: %#v %v", items, err)
	}
}

func TestExtractionUnsupportedFailureNeverManufacturesProcessedCoverage(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	_, media, _ := extractionTestOriginal(t, s, p, "unsupported-extraction", JobOCR)
	for _, kind := range []JobKind{JobOCR, JobCaption} {
		work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(kind), TTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.RetryExtraction(ctx, JobRetry{Lease: work.Lease, Code: "unsupported", Terminal: true}); err != nil {
			t.Fatal(err)
		}
	}
	if candidates, err := s.EvictionCandidates(ctx, 10); err != nil || len(candidates) != 0 {
		t.Fatalf("failed extraction manufactured successful coverage: %#v %v", candidates, err)
	}
	if _, err := s.BeginEviction(ctx, media); !errors.Is(err, ErrNoWork) {
		t.Fatalf("unsupported input called verified complete: %v", err)
	}
	var outcomes int
	if err := s.readDB.QueryRow("SELECT count(*) FROM extension_extraction_jobs WHERE outcome<>''").Scan(&outcomes); err != nil || outcomes != 0 {
		t.Fatalf("unsupported status fabricated empty outcome: %d %v", outcomes, err)
	}
}
