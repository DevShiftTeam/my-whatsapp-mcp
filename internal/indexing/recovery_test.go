//go:build sqlite_fts5

package indexing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/inference"
)

func integrationStore(t *testing.T) (*archive.Store, *atomic.Int64, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clock := new(atomic.Int64)
	clock.Store(time.Now().UnixMilli())
	s, err := archive.Open(context.Background(), dir, archive.Options{Now: func() time.Time { return time.UnixMilli(clock.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, clock, dir
}
func integrationEvent(id, body string, at time.Time) archive.SourceEvent {
	return archive.SourceEvent{Key: archive.SourceKey{Account: "account", Chat: "synthetic-chat", Sender: "synthetic-sender", Message: id}, Kind: archive.EventOriginal, OriginalSentAt: at, OriginalDateKnown: true, SourceRevision: 1, Body: body, BodyKind: archive.MessageText}
}
func integrationPolicy(t *testing.T, s *archive.Store) archive.ArchivePolicy {
	t.Helper()
	p, e := s.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func integrationApply(t *testing.T, s *archive.Store, p archive.ArchivePolicy, e archive.SourceEvent) archive.ApplyResult {
	t.Helper()
	r, err := s.ApplySourceEvent(context.Background(), e, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func integrationService(t *testing.T, s *archive.Store, e *fakeEncoder) *Service {
	t.Helper()
	svc, err := NewService(ServiceConfig{Store: s, Account: "account", Version: testVersion(), Tokenizer: testTokenizer{}, Encoder: e})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func integrationStep(t *testing.T, s *Service) (StepResult, error) {
	t.Helper()
	for i := 0; i < 64; i++ {
		r, err := s.Step(context.Background())
		if err != nil || r.Code != "preparing" {
			return r, err
		}
	}
	t.Fatal("bounded publication preparation did not finish")
	return StepResult{}, ErrLimit
}

type recordingPublicationStore struct {
	*archive.Store
	documents []archive.Document
}

func (s *recordingPublicationStore) PublishWindowBatch(ctx context.Context, p archive.WindowBatchPublication) (archive.PublicationProgress, error) {
	progress, err := s.Store.PublishWindowBatch(ctx, p)
	if err == nil && progress.Accepted {
		s.documents = append(s.documents, p.Documents...)
	}
	return progress, err
}
func TestSQLiteWindowPublishesWhole256KiBSourceAcrossBatches(t *testing.T) {
	ctx := context.Background()
	s, _, _ := integrationStore(t)
	p := integrationPolicy(t, s)
	e := integrationEvent("full-256k-source", strings.Repeat("x", MaxSourceFieldBytes), time.Now().UTC().Add(-time.Hour))
	integrationApply(t, s, p, e)
	ref, err := s.CurrentSource(ctx, e.Key)
	if err != nil {
		t.Fatal(err)
	}
	recording := &recordingPublicationStore{Store: s}
	svc, err := NewService(ServiceConfig{Store: recording, Account: "account", Version: testVersion(), Tokenizer: testTokenizer{}, Encoder: &fakeEncoder{}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		r, err := svc.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r.Published > MaxDocuments || r.Activated {
			t.Fatal("bounded windows were prematurely active")
		}
		if svc.dirty == nil && len(recording.documents) > 0 {
			break
		}
		if i == 15 {
			t.Fatal("full source coverage did not finish across bounded batches")
		}
	}
	if len(recording.documents) <= MaxDocuments {
		t.Fatal("fixture did not cross publication boundary")
	}
	checkSourceCoverage(t, []archive.WindowMessage{{Source: ref, ChatID: "synthetic", SentAt: e.OriginalSentAt, Body: e.Body}}, recording.documents)
}
func semanticCandidates(t *testing.T, s *archive.Store, p archive.ArchivePolicy) []archive.Candidate {
	t.Helper()
	g, e := s.EnsureIndex(context.Background(), testVersion())
	if e != nil {
		t.Fatal(e)
	}
	q := make([]float32, 384)
	q[0] = 1
	pair, e := s.CandidatePair(context.Background(), archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, archive.CandidatePairRequest{Search: archive.SearchRequest{Query: "needle", Limit: 100}, Generation: g.ID, Version: g.Version, Query: q})
	if e != nil {
		t.Fatal(e)
	}
	return pair.Semantic
}

type identityTokenizer struct {
	testTokenizer
	identity inference.TokenizerIdentity
}

func (t identityTokenizer) TokenizerIdentity() inference.TokenizerIdentity { return t.identity }

func TestSQLiteStagedDerivedFirstFlowAndEvictedOriginalReplacement(t *testing.T) {
	ctx := context.Background()
	s, _, _ := integrationStore(t)
	p := integrationPolicy(t, s)
	e := integrationEvent("derived-image", "synthetic source caption", time.Now().Add(-time.Hour))
	e.BodyKind = archive.MessageImage
	e.Media = []archive.MediaReference{{Kind: "image", ProtocolReference: []byte("synthetic authenticated key"), MimeType: "image/png", Availability: "not_retained"}}
	applied := integrationApply(t, s, p, e)
	if len(applied.MediaIDs) != 1 {
		t.Fatal("fixture requires exact committed media association")
	}
	ref, err := s.CurrentSource(ctx, e.Key)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := s.AdmitMedia(ctx, archive.MediaAdmissionRequest{Source: ref, MediaID: applied.MediaIDs[0], Budget: archive.BudgetOriginals, MaximumBytes: 32, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.PublishOriginal(ctx, archive.OriginalPublication{Reservation: reservation, SHA256: strings.Repeat("a", 64), Bytes: 16, ActualFormat: "png"}); err != nil || !ok {
		t.Fatalf("original fixture: %t %v", ok, err)
	}
	version := archive.ExtractionVersion{ID: "synthetic-primary-ocr-v1", Kind: archive.JobOCR, ProfileID: "synthetic-ocr", ProfileIdentity: "synthetic-model-sha", RuntimeSHA256: strings.Repeat("a", 64), ExtractorVersion: "1", PromptVersion: "synthetic-prompt-v1", Language: "auto"}
	ocr, err := s.ClaimExtraction(ctx, archive.ExtractionClaim{Account: "account", Version: version, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	text := "  " + strings.Repeat("שלום🙂\n", 100) + " needle \t"
	provenance := archive.DerivedProvenance{Kind: archive.DocumentOCR, MediaID: applied.MediaIDs[0], ProfileID: version.ProfileID, ExtractorVersion: version.ExtractorVersion, PromptVersion: version.PromptVersion, OffsetKind: "none"}
	origin := archive.Document{ID: "synthetic-independent-root", Kind: archive.DocumentOCR, Text: text, Sources: []archive.SourceRef{ocr.Source}, Provenance: provenance}
	if ok, err := s.CommitExtraction(ctx, archive.ExtractionResult{Lease: ocr.Lease, ContentHash: ocr.ContentHash, Outcome: archive.ExtractionText, Documents: []archive.Document{origin}}); err != nil || !ok {
		t.Fatalf("independent extraction: %t %v", ok, err)
	}
	keyword, err := s.Search(ctx, archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, archive.SearchRequest{Query: "needle", Limit: 10})
	if err != nil || len(keyword.Hits) == 0 {
		t.Fatalf("independent root unavailable before encoder/index: %#v %v", keyword, err)
	}
	old, err := s.EnsureIndex(ctx, testVersion())
	if err != nil {
		t.Fatal(err)
	}
	first := integrationService(t, s, &fakeEncoder{})
	for i := 0; i < 16; i++ {
		r, stepErr := first.Step(ctx)
		if stepErr != nil {
			t.Fatal(stepErr)
		}
		if r.Activated {
			break
		}
		if i == 15 {
			t.Fatal("first generated fragments did not finish bounded work")
		}
	}
	readDerived := func(g archive.IndexGeneration, tokenizer inference.PassageTokenizer) int {
		t.Helper()
		query := make([]float32, inference.ProtocolDimension)
		query[0] = 1
		scope := archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
		pair, readErr := s.CandidatePair(ctx, scope, archive.CandidatePairRequest{Search: archive.SearchRequest{Query: "needle", Limit: 100}, Generation: g.ID, Version: g.Version, Query: query})
		if readErr != nil {
			t.Fatal(readErr)
		}
		var refs []archive.DocumentRef
		for _, candidate := range pair.Semantic {
			if candidate.Ref.Kind == archive.DocumentOCR {
				refs = append(refs, candidate.Ref)
			}
		}
		page, readErr := s.HydrateDocuments(ctx, scope, archive.HydrateRequest{Documents: refs, Snapshot: pair.Snapshot, MaxBytes: 64 << 10})
		if readErr != nil {
			t.Fatal(readErr)
		}
		type fragment struct {
			start, end int
			text       string
		}
		var fragments []fragment
		for _, document := range page.Documents {
			for _, excerpt := range document.Excerpts {
				id := string(excerpt.DocumentID)
				last := strings.LastIndexByte(id, '-')
				if last <= 0 {
					t.Fatal("fragment ID missing byte range")
				}
				previous := strings.LastIndexByte(id[:last], '-')
				if previous <= 0 {
					t.Fatal("fragment ID missing byte range")
				}
				var start, end int
				if _, scanErr := fmt.Sscanf(id[previous+1:], "%d-%d", &start, &end); scanErr != nil {
					t.Fatal("fragment ID does not retain byte range")
				}
				count, countErr := tokenizer.CountPassage(ctx, excerpt.Text)
				if countErr != nil || count < 1 || count > g.Version.MaxPassageTokens || excerpt.Provenance != provenance || excerpt.Source != ref.Version {
					t.Fatal("hydrated fragment authority/provenance/tokens changed")
				}
				fragments = append(fragments, fragment{start, end, excerpt.Text})
			}
		}
		sort.Slice(fragments, func(i, j int) bool { return fragments[i].start < fragments[j].start })
		var joined strings.Builder
		for _, fragment := range fragments {
			if fragment.start != joined.Len() || fragment.end != fragment.start+len(fragment.text) {
				t.Fatal("hydrated fragment byte coverage has gap or overlap")
			}
			joined.WriteString(fragment.text)
		}
		if joined.String() != text {
			t.Fatal("generated UTF8 text lost after archive publication")
		}
		return len(fragments)
	}
	if count := readDerived(old, testTokenizer{}); count < 2 {
		t.Fatal("first tokenizer did not partition full generated text")
	}
	if _, err := s.BeginEviction(ctx, applied.MediaIDs[0]); !errors.Is(err, archive.ErrNoWork) {
		t.Fatalf("OCR alone authorized original eviction: %v", err)
	}
	captionVersion := version
	captionVersion.ID, captionVersion.Kind, captionVersion.ProfileID = "synthetic-primary-caption-v1", archive.JobCaption, "synthetic-caption"
	caption, err := s.ClaimExtraction(ctx, archive.ExtractionClaim{Account: "account", Version: captionVersion, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if caption.Source != ocr.Source || caption.MediaID != applied.MediaIDs[0] || caption.OriginalSHA256 != ocr.OriginalSHA256 || caption.Version != captionVersion {
		t.Fatal("caption fixture changed original source, media, or extraction authority")
	}
	if ok, err := s.CommitExtraction(ctx, archive.ExtractionResult{Lease: caption.Lease, ContentHash: caption.ContentHash, Outcome: archive.ExtractionEmpty}); err != nil || !ok {
		t.Fatalf("independent empty caption: %t %v", ok, err)
	}
	artifact, err := s.BeginEviction(ctx, applied.MediaIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishMediaCleanup(ctx, archive.MediaCleanup{ReservationID: artifact.ReservationID, ArtifactID: artifact.ID, LeaseID: artifact.CleanupLease, Code: "removed"}); err != nil {
		t.Fatal(err)
	}
	v := testVersion()
	v.ID, v.EncoderProfile, v.TokenizerSHA256, v.TokenizerImplementation = "synthetic-index-v2", "synthetic-replacement", strings.Repeat("c", 64), "synthetic-cheap-v2"
	tokenizer := identityTokenizer{testTokenizer: testTokenizer{cheap: true}, identity: inference.TokenizerIdentity{ProfileID: v.EncoderProfile, TokenizerSHA256: v.TokenizerSHA256, Implementation: v.TokenizerImplementation, PrefixVersion: v.PrefixVersion}}
	next, err := s.EnsureIndex(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ActivateIndex(ctx, next.ID); !errors.Is(err, archive.ErrNoWork) {
		t.Fatalf("incomplete generation activated: %v", err)
	}
	replacement, err := NewService(ServiceConfig{Store: s, Account: "account", Version: v, Tokenizer: tokenizer, Encoder: &fakeEncoder{}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		r, stepErr := replacement.Step(ctx)
		if stepErr != nil {
			t.Fatal(stepErr)
		}
		if r.Activated {
			break
		}
		if i == 15 {
			t.Fatal("evicted-original generated reuse did not finish bounded work")
		}
	}
	if count := readDerived(next, tokenizer); count != 1 {
		t.Fatal("replacement reused old fragments instead of retokenizing original text")
	}
}

func TestLateEditRebuildsAffectedRange(t *testing.T) {
	s, _, _ := integrationStore(t)
	p := integrationPolicy(t, s)
	at := time.Now().UTC().Add(-time.Hour)
	a := integrationEvent("a", strings.Repeat("a", 110)+" needleone", at)
	b := integrationEvent("b", strings.Repeat("b", 110)+" needletwo", at.Add(time.Minute))
	integrationApply(t, s, p, a)
	integrationApply(t, s, p, b)
	svc := integrationService(t, s, &fakeEncoder{})
	first, e := integrationStep(t, svc)
	if e != nil || first.Published != 2 {
		t.Fatalf("initial boundary: %#v %v", first, e)
	}
	if _, e = integrationStep(t, svc); e != nil {
		t.Fatal(e)
	}
	if got := semanticCandidates(t, s, p); len(got) != 2 {
		t.Fatalf("initial vectors: %d", len(got))
	}
	invalidation := a
	invalidation.Kind = archive.EventEditInvalidation
	invalidation.SourceRevision = 2
	invalidation.Body = ""
	integrationApply(t, s, p, invalidation)
	edit := a
	edit.Kind = archive.EventEditContent
	edit.SourceRevision = 2
	edit.Body = "newneedle"
	current := integrationApply(t, s, p, edit)
	rebuilt, e := integrationStep(t, svc)
	if e != nil || rebuilt.Published != 1 || rebuilt.Embedded != 1 {
		t.Fatalf("rebuilt neighboring boundaries: %#v %v", rebuilt, e)
	}
	got := semanticCandidates(t, s, p)
	if len(got) != 1 || len(got[0].Ref.Sources) != 2 {
		t.Fatalf("old boundary retained: %#v", got)
	}
	found := false
	for _, source := range got[0].Ref.Sources {
		if source.Version.MessageID == current.MessageID {
			found = true
			if source.Version.Revision != current.Version.Revision {
				t.Fatal("late edit used stale source revision")
			}
		}
	}
	if !found {
		t.Fatal("edited source omitted")
	}
}
func TestSQLitePolicyNarrowingRejectsLateEmbedding(t *testing.T) {
	s, _, _ := integrationStore(t)
	p := integrationPolicy(t, s)
	integrationApply(t, s, p, integrationEvent("a", "privacyneedle", time.Now().Add(-time.Hour)))
	var changed archive.ArchivePolicy
	e := &fakeEncoder{before: func() {
		var err error
		changed, err = s.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsSelective}, p.Generation)
		if err != nil {
			t.Error(err)
		}
	}}
	svc := integrationService(t, s, e)
	r, err := integrationStep(t, svc)
	if err != nil || r.Embedded != 0 || r.Code != "superseded" {
		t.Fatalf("late policy completion: %#v %v", r, err)
	}
	page, err := s.Search(context.Background(), archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: changed.Generation}, archive.SearchRequest{Query: "privacyneedle", Limit: 10})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("excluded source readable: %#v %v", page, err)
	}
}
func TestSQLiteExpiredLeaseCannotCommit(t *testing.T) {
	s, clock, _ := integrationStore(t)
	p := integrationPolicy(t, s)
	integrationApply(t, s, p, integrationEvent("a", "expireneedle", time.Now().Add(-time.Hour)))
	e := &fakeEncoder{before: func() { clock.Add((3 * time.Minute).Milliseconds()) }}
	svc := integrationService(t, s, e)
	r, err := integrationStep(t, svc)
	if err != nil || r.Embedded != 0 || r.Code != "superseded" {
		t.Fatalf("expired lease completion: %#v %v", r, err)
	}
	e.before = nil
	retried, err := integrationStep(t, svc)
	if err != nil || retried.Embedded != 1 {
		t.Fatalf("expired lease not reclaimed: %#v %v", retried, err)
	}
}
func TestMissingModelKeepsKeywordAndDurableRetryAfterReopen(t *testing.T) {
	s, clock, dir := integrationStore(t)
	p := integrationPolicy(t, s)
	integrationApply(t, s, p, integrationEvent("a", "keywordneedle", time.Now().Add(-time.Hour)))
	svc := integrationService(t, s, nil)
	r, e := integrationStep(t, svc)
	if e != nil || r.Code != "model_unavailable" {
		t.Fatalf("missing model: %#v %v", r, e)
	}
	scope := archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
	page, e := s.Search(context.Background(), scope, archive.SearchRequest{Query: "keywordneedle", Limit: 10})
	if e != nil || len(page.Hits) != 1 {
		t.Fatalf("keyword fallback: %#v %v", page, e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	reopened, e := archive.Open(context.Background(), dir, archive.Options{Now: func() time.Time { return time.UnixMilli(clock.Load()) }})
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	if _, e = reopened.ClaimJob(context.Background(), archive.JobClaim{Account: "account", Kind: archive.JobTextEmbedding, TTL: time.Minute}); !errors.Is(e, archive.ErrNoWork) {
		t.Fatalf("retry loop was not delayed: %v", e)
	}
	clock.Add((2 * time.Minute).Milliseconds())
	resumed := integrationService(t, reopened, &fakeEncoder{})
	done, e := integrationStep(t, resumed)
	if e != nil || done.Embedded != 1 {
		t.Fatalf("durable pending job lost: %#v %v", done, e)
	}
}

func TestCrashRecoversDirtyWindows(t *testing.T) {
	for _, phase := range []string{"dirty_claimed", "windows_published"} {
		t.Run(phase, func(t *testing.T) {
			dir, e := filepath.EvalSymlinks(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			marker := filepath.Join(dir, "helper-ready")
			cmd := exec.Command(os.Args[0], "-test.run=^TestIndexingCrashHelper$")
			cmd.Env = append(os.Environ(), "WA_INDEXING_CRASH_DIR="+dir, "WA_INDEXING_CRASH_PHASE="+phase, "WA_INDEXING_CRASH_READY="+marker)
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, e = os.Stat(marker); e == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("synthetic crash helper did not reach boundary")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if e = cmd.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			_ = cmd.Wait()
			s, e := archive.Open(context.Background(), dir, archive.Options{Now: func() time.Time { return time.Now().Add(2 * time.Second) }})
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			svc := integrationService(t, s, &fakeEncoder{})
			r, e := integrationStep(t, svc)
			if e != nil || r.Embedded != 1 {
				t.Fatalf("crash recovery: %#v %v", r, e)
			}
			if phase == "dirty_claimed" && r.Published != 1 {
				t.Fatal("claimed dirty work was lost")
			}
			if phase == "windows_published" && r.Published != 0 {
				t.Fatal("committed publication was unnecessarily recreated")
			}
			p, e := s.Policy(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			if got := semanticCandidates(t, s, p); len(got) != 1 {
				t.Fatalf("recovery created missing/orphan vectors: %d", len(got))
			}
		})
	}
}
func TestIndexingCrashHelper(t *testing.T) {
	dir := os.Getenv("WA_INDEXING_CRASH_DIR")
	if dir == "" {
		return
	}
	s, err := archive.Open(context.Background(), dir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p := integrationPolicy(t, s)
	integrationApply(t, s, p, integrationEvent("crash", "crashneedle synthetic", time.Now().Add(-time.Hour)))
	g, err := s.EnsureIndex(context.Background(), testVersion())
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.ClaimDirty(context.Background(), archive.DirtyClaim{Account: "account", Generation: g.ID, TTL: time.Second, Limit: 256})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("WA_INDEXING_CRASH_PHASE") == "windows_published" {
		var input archive.WindowBatchInput
		for i := 0; i < 16; i++ {
			input, err = s.LoadWindowBatch(context.Background(), w)
			if err != nil {
				t.Fatal(err)
			}
			if input.Ready {
				break
			}
			if _, err = s.RenewDirty(context.Background(), w, time.Second); err != nil {
				t.Fatal(err)
			}
		}
		if !input.Ready {
			t.Fatal("crash fixture preparation not ready")
		}
		documents, err := BuildWindowBatch(context.Background(), input, testTokenizer{})
		if err != nil {
			t.Fatal(err)
		}
		progress, err := s.PublishWindowBatch(context.Background(), archive.WindowBatchPublication{Work: w, Version: g.Version, Cursor: input.Cursor, Documents: documents})
		if err != nil || !progress.Accepted || !progress.Complete {
			t.Fatalf("publication: %#v %v", progress, err)
		}
	}
	if err = os.WriteFile(os.Getenv("WA_INDEXING_CRASH_READY"), []byte("synthetic-ready\n"), 0600); err != nil {
		t.Fatal(err)
	}
	select {}
}
