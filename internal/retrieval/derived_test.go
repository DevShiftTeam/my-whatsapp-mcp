//go:build sqlite_fts5

package retrieval

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// Authored output uses the primary extraction port. Install a synthetic active
// index before publication to prove derived reads do not need a query encoder.
// Legacy extraction-job migration/proof belongs to archive fixtures.
func derivedFixture(t *testing.T, kind archive.DocumentKind) (*fixture, archive.MessageID, archive.DerivedExcerpt) {
	t.Helper()
	return extractionFixtureN(t, kind, 1, true)
}

func TestPrimaryDerivedKeywordSearchWithActiveIndex(t *testing.T) {
	for _, kind := range []archive.DocumentKind{archive.DocumentOCR, archive.DocumentCaption, archive.DocumentTranscript} {
		t.Run(string(kind), func(t *testing.T) {
			f, id, want := derivedFixture(t, kind)
			result, err := service(t, f.config(archive.IndexGeneration{})).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "extraction"})
			if err != nil || result.Mode != "keyword" || len(result.Page.Hits) != 1 {
				t.Fatalf("derived keyword result %#v %v", result, err)
			}
			hit := result.Page.Hits[0]
			if hit.MessageID != id || hit.Snippet != "" || len(hit.Derived) != 1 || hit.Derived[0] != want || len(result.Guard.Sources) != 1 {
				t.Fatalf("derived label/source drift %#v", hit)
			}
		})
	}
}

func TestPrimaryDerivedContextWithActiveIndexWithoutEncoder(t *testing.T) {
	for _, kind := range []archive.DocumentKind{archive.DocumentOCR, archive.DocumentCaption, archive.DocumentTranscript} {
		t.Run(string(kind), func(t *testing.T) {
			f, id, want := derivedFixture(t, kind)
			c := f.config(archive.IndexGeneration{})
			c.Index = func(context.Context) (archive.IndexGeneration, error) {
				t.Fatal("context invoked query planning")
				return archive.IndexGeneration{}, nil
			}
			c.Encoder = queryEncoder{encode: func(context.Context, string) ([]float32, error) { t.Fatal("context loaded encoder"); return nil, nil }}
			page, guard, err := service(t, c).ContextGuarded(context.Background(), f.scope, archive.ContextRequest{MessageID: id})
			if err != nil || len(page.Messages) != 1 {
				t.Fatalf("derived context %v %#v", err, page)
			}
			message := page.Messages[0]
			if message.Body != "original body" || message.Caption != "original caption" || len(message.Derived) != 1 || message.Derived[0] != want || len(guard.Sources) != 1 {
				t.Fatalf("context lost original/derived distinction %#v", message)
			}
		})
	}
}

// Independent roots are authored extractor output with no encoder generation,
// semantic token count, vector or native extraction/quality claim.
func independentDerivedFixture(t *testing.T, kind archive.DocumentKind) (*fixture, archive.MessageID, archive.DerivedExcerpt) {
	t.Helper()
	return independentDerivedFixtureN(t, kind, 1)
}

func independentDerivedFixtureN(t *testing.T, kind archive.DocumentKind, count int) (*fixture, archive.MessageID, archive.DerivedExcerpt) {
	t.Helper()
	return extractionFixtureN(t, kind, count, false)
}

func extractionFixtureN(t *testing.T, kind archive.DocumentKind, count int, activeIndex bool) (*fixture, archive.MessageID, archive.DerivedExcerpt) {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	mediaKind, mime, format, bodyKind, jobKind := "image", "image/png", "png", archive.MessageImage, archive.JobOCR
	switch kind {
	case archive.DocumentCaption:
		jobKind = archive.JobCaption
	case archive.DocumentTranscript:
		mediaKind, mime, format, bodyKind, jobKind = "voice", "audio/ogg", "ogg", archive.MessageVoice, archive.JobTranscription
	}
	key := archive.SourceKey{Account: "account", Chat: "chat", Sender: "sender", Message: "independent-root-source"}
	applied, err := f.store.ApplySourceEvent(ctx, archive.SourceEvent{Key: key, Kind: archive.EventOriginal, SourceRevision: 1, OriginalDateKnown: true, OriginalSentAt: time.UnixMilli(f.clock.Load()).Add(-time.Minute), BodyKind: bodyKind, Body: "original body", Caption: "original caption", Media: []archive.MediaReference{{Kind: mediaKind, MimeType: mime, ProtocolReference: []byte("synthetic authenticated reference"), Availability: "not_retained"}}}, f.policy.Generation)
	if err != nil || len(applied.MediaIDs) != 1 {
		t.Fatalf("independent source media %v %#v", err, applied)
	}
	f.keys[applied.MessageID] = key
	if activeIndex {
		f.index(t, nil)
	}
	source, err := f.store.CurrentSource(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := f.store.AdmitMedia(ctx, archive.MediaAdmissionRequest{Source: source, MediaID: applied.MediaIDs[0], Budget: archive.BudgetOriginals, MaximumBytes: 1, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.store.PublishOriginal(ctx, archive.OriginalPublication{Reservation: reservation, SHA256: strings.Repeat("b", 64), Bytes: 1, ActualFormat: format}); err != nil || !ok {
		t.Fatalf("independent original ledger %v %v", ok, err)
	}
	extraction, ok := any(f.store).(archive.ExtractionStore)
	if !ok {
		t.Fatal("archive does not implement the published independent extraction contract")
	}
	version := archive.ExtractionVersion{ID: "retrieval-root-" + string(kind) + "-v1", Kind: jobKind, ProfileID: "synthetic-extractor", ProfileIdentity: strings.Repeat("a", 64), RuntimeSHA256: strings.Repeat("c", 64), ExtractorVersion: "fixture-v1", PromptVersion: "fixture-prompt-v1", Language: "he+en"}
	work, err := extraction.ClaimExtraction(ctx, archive.ExtractionClaim{Account: "account", Version: version, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	provenance := archive.DerivedProvenance{Kind: kind, MediaID: work.MediaID, ProfileID: version.ProfileID, ExtractorVersion: version.ExtractorVersion, PromptVersion: version.PromptVersion, OffsetKind: "none"}
	if kind == archive.DocumentTranscript {
		provenance.OffsetKind, provenance.StartMS, provenance.EndMS = "slice", 1250, 3500
	}
	if count < 1 {
		t.Fatal("independent root fixture needs a nonempty output inventory")
	}
	documents := make([]archive.Document, count)
	for i := range documents {
		documents[i] = archive.Document{ID: archive.DocumentID("independent-" + string(kind) + "-" + strconv.Itoa(i)), Kind: kind, Text: "authorized immutable extraction", Sources: []archive.SourceRef{work.Source}, Provenance: provenance}
	}
	if ok, err := extraction.CommitExtraction(ctx, archive.ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: archive.ExtractionText, Documents: documents}); err != nil || !ok {
		t.Fatalf("independent extraction commit %v %v", ok, err)
	}
	return f, applied.MessageID, archive.DerivedExcerpt{DocumentID: documents[0].ID, Source: work.Source.Version, Text: documents[0].Text, Provenance: provenance}
}

func TestIndependentDerivedKeywordSearchWithoutIndex(t *testing.T) {
	for _, kind := range []archive.DocumentKind{archive.DocumentOCR, archive.DocumentCaption, archive.DocumentTranscript} {
		t.Run(string(kind), func(t *testing.T) {
			f, id, want := independentDerivedFixture(t, kind)
			s := service(t, f.config(archive.IndexGeneration{}))
			result, err := s.SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "immutable"})
			if err != nil || result.Mode != "keyword" || len(result.Page.Hits) != 1 {
				t.Fatalf("independent keyword result %#v %v", result, err)
			}
			hit := result.Page.Hits[0]
			if hit.MessageID != id || hit.Snippet != "" || len(hit.Derived) != 1 || hit.Derived[0] != want || len(result.Guard.Sources) != 1 {
				t.Fatalf("independent keyword label/source drift %#v", hit)
			}
			key := f.keys[id]
			if _, err := f.store.ApplySourceEvent(context.Background(), archive.SourceEvent{Key: key, Kind: archive.EventDelete, SourceRevision: 2}, f.policy.Generation); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CheckResponse(context.Background(), f.scope, archive.ResponseCheck{Guard: result.Guard}); err == nil {
				t.Fatal("independent root response survived source revocation")
			}
		})
	}
}

func TestIndependentDerivedContextWithoutIndex(t *testing.T) {
	for _, kind := range []archive.DocumentKind{archive.DocumentOCR, archive.DocumentCaption, archive.DocumentTranscript} {
		t.Run(string(kind), func(t *testing.T) {
			f, id, want := independentDerivedFixture(t, kind)
			c := f.config(archive.IndexGeneration{})
			c.Index = func(context.Context) (archive.IndexGeneration, error) {
				t.Fatal("independent context invoked query planning")
				return archive.IndexGeneration{}, nil
			}
			c.Encoder = queryEncoder{encode: func(context.Context, string) ([]float32, error) {
				t.Fatal("independent context loaded encoder")
				return nil, nil
			}}
			page, guard, err := service(t, c).ContextGuarded(context.Background(), f.scope, archive.ContextRequest{MessageID: id})
			if err != nil || len(page.Messages) != 1 {
				t.Fatalf("independent context %v %#v", err, page)
			}
			message := page.Messages[0]
			if message.Body != "original body" || message.Caption != "original caption" || len(message.Derived) != 1 || message.Derived[0] != want || len(guard.Sources) != 1 {
				t.Fatalf("independent context lost originals/provenance %#v", message)
			}
		})
	}
}

func TestIndependentRootAppendPreservesGuardAndStalesCursor(t *testing.T) {
	f, _, _ := independentDerivedFixtureN(t, archive.DocumentOCR, 2)
	ctx := context.Background()
	s := service(t, f.config(archive.IndexGeneration{}))
	request := archive.SearchRequest{Query: "immutable", Limit: 1}
	first, err := s.SearchDetails(ctx, f.scope, request)
	if err != nil || len(first.Page.Hits) != 1 || first.Page.NextCursor == "" {
		t.Fatalf("independent root first page %#v %v", first, err)
	}
	extraction := any(f.store).(archive.ExtractionStore)
	version := archive.ExtractionVersion{ID: "retrieval-root-caption-v1", Kind: archive.JobCaption, ProfileID: "synthetic-extractor", ProfileIdentity: strings.Repeat("a", 64), RuntimeSHA256: strings.Repeat("c", 64), ExtractorVersion: "fixture-v1", PromptVersion: "fixture-prompt-v1", Language: "he+en"}
	work, err := extraction.ClaimExtraction(ctx, archive.ExtractionClaim{Account: "account", Version: version, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	provenance := archive.DerivedProvenance{Kind: archive.DocumentCaption, MediaID: work.MediaID, ProfileID: version.ProfileID, ExtractorVersion: version.ExtractorVersion, PromptVersion: version.PromptVersion, OffsetKind: "none"}
	document := archive.Document{ID: "independent-caption-append", Kind: archive.DocumentCaption, Text: "additional immutable extraction", Sources: []archive.SourceRef{work.Source}, Provenance: provenance}
	if ok, err := extraction.CommitExtraction(ctx, archive.ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: archive.ExtractionText, Documents: []archive.Document{document}}); err != nil || !ok {
		t.Fatalf("append independent root %v %v", ok, err)
	}
	if _, err := s.CheckResponse(ctx, f.scope, archive.ResponseCheck{Guard: first.Guard}); err != nil {
		t.Fatalf("unrelated root append discarded unchanged first page: %v", err)
	}
	request.Cursor = first.Page.NextCursor
	if next, err := s.SearchDetails(ctx, f.scope, request); !errors.Is(err, archive.ErrCursorStale) || len(next.Page.Hits) != 0 {
		t.Fatalf("root append resumed old ranking: %#v %v", next, err)
	}
}
