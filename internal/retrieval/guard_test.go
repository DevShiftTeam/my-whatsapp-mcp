//go:build sqlite_fts5

package retrieval

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
)

type interleavedReader struct {
	archive.CandidateReader
	afterHydrate  func()
	beforeHydrate func()
	beforePair    func() error
}

func (r *interleavedReader) CandidatePair(ctx context.Context, scope archive.ReadScope, request archive.CandidatePairRequest) (archive.CandidatePair, error) {
	if r.beforePair != nil {
		hook := r.beforePair
		r.beforePair = nil
		if err := hook(); err != nil {
			return archive.CandidatePair{}, err
		}
	}
	return r.CandidateReader.CandidatePair(ctx, scope, request)
}

func (r *interleavedReader) HydrateDocuments(ctx context.Context, scope archive.ReadScope, request archive.HydrateRequest) (archive.HydratedPage, error) {
	if r.beforeHydrate != nil {
		hook := r.beforeHydrate
		r.beforeHydrate = nil
		hook()
	}
	page, err := r.CandidateReader.HydrateDocuments(ctx, scope, request)
	if err == nil && r.afterHydrate != nil {
		hook := r.afterHydrate
		r.afterHydrate = nil
		hook()
	}
	return page, err
}
func TestDeletionBetweenCandidateAndHydration(t *testing.T) {
	f := newFixture(t)
	id := f.add(t, "victim", "chat", "keyword old body")
	g := f.index(t, nil)
	c := f.config(g)
	c.Candidates = &interleavedReader{CandidateReader: f.store, beforeHydrate: func() {
		_, e := f.store.ApplySourceEvent(context.Background(), archive.SourceEvent{Key: f.keys[id], Kind: archive.EventDelete, SourceRevision: 2}, f.policy.Generation)
		if e != nil {
			t.Fatal(e)
		}
	}}
	result, e := service(t, c).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "keyword"})
	if e != nil && !errors.Is(e, archive.ErrSourceUnavailable) && !errors.Is(e, archive.ErrCursorStale) {
		t.Fatal(e)
	}
	if e == nil && len(result.Page.Hits) != 0 {
		t.Fatalf("deleted contributor escaped: %#v", result.Page.Hits)
	}
}
func TestUnrelatedInsertBetweenHydrationAndFinalCheck(t *testing.T) {
	f := newFixture(t)
	id := f.add(t, "stable", "chat", "keyword body")
	g := f.index(t, nil)
	c := f.config(g)
	c.Candidates = &interleavedReader{CandidateReader: f.store, afterHydrate: func() { f.add(t, "unrelated", "other-chat", "new insertion") }}
	result, e := service(t, c).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "keyword"})
	if e != nil || len(result.Page.Hits) != 1 || result.Page.Hits[0].MessageID != id {
		t.Fatalf("unrelated insert discarded current result %#v %v", result, e)
	}
}
func TestFinalCheckEnforcesRollingBoundsWithoutRevision(t *testing.T) {
	f := newFixture(t)
	f.add(t, "edge", "chat", "keyword expiring access")
	g := f.index(t, nil)
	c := f.config(g)
	scope := f.scope
	scope.Rolling = time.Minute + time.Second
	hydrated := false
	c.Candidates = &interleavedReader{CandidateReader: f.store, afterHydrate: func() { hydrated = true; f.clock.Add(1001) }}
	result, e := service(t, c).SearchDetails(context.Background(), scope, archive.SearchRequest{Query: "keyword"})
	if !hydrated {
		t.Fatal("rolling test failed before final response check")
	}
	if e != nil && !errors.Is(e, archive.ErrSourceUnavailable) && !errors.Is(e, archive.ErrCursorStale) {
		t.Fatal(e)
	}
	if e == nil && len(result.Page.Hits) != 0 {
		t.Fatal("rolling cutoff crossing returned old contributor")
	}
}
func TestGuardedVariantsPermitFinalCredentialRevocationCheck(t *testing.T) {
	f := newFixture(t)
	f.add(t, "grant", "chat", "keyword body")
	var digest [32]byte
	digest[0] = 1
	credential, e := f.store.CreateCredential(context.Background(), archive.CredentialCreate{Label: "test", Principal: "client", Digest: digest, Policy: archive.CredentialPolicy{Account: "account", AllChats: true}})
	if e != nil {
		t.Fatal(e)
	}
	grant, e := f.store.ResolveCredential(context.Background(), digest)
	if e != nil {
		t.Fatal(e)
	}
	s := service(t, f.config(archive.IndexGeneration{}))
	page, guard, e := s.SearchGuarded(context.Background(), grant.Scope, archive.SearchRequest{Query: "keyword"})
	if e != nil || len(page.Hits) != 1 {
		t.Fatalf("guarded search %v", e)
	}
	if e = f.store.RevokeCredential(context.Background(), credential.ID, credential.Revision); e != nil {
		t.Fatal(e)
	}
	if _, e = s.CheckResponse(context.Background(), grant.Scope, archive.ResponseCheck{Guard: guard, Credential: &grant}); !errors.Is(e, archive.ErrUnauthorized) {
		t.Fatalf("revoked grant survived final check: %v", e)
	}
}
func TestMetadataAndContextDoNotLoadEncoder(t *testing.T) {
	f := newFixture(t)
	id := f.add(t, "context", "chat", "permitted body")
	c := f.config(archive.IndexGeneration{})
	c.Index = func(context.Context) (archive.IndexGeneration, error) {
		t.Fatal("metadata/context invoked query planning")
		return archive.IndexGeneration{}, nil
	}
	c.Encoder = queryEncoder{encode: func(context.Context, string) ([]float32, error) {
		t.Fatal("metadata/context loaded encoder")
		return nil, nil
	}}
	s := service(t, c)
	if page, e := s.Context(context.Background(), f.scope, archive.ContextRequest{MessageID: id}); e != nil || len(page.Messages) != 1 {
		t.Fatalf("context %v", e)
	}
	if page, e := s.ListChats(context.Background(), f.scope, archive.ChatListRequest{}); e != nil || len(page.Chats) != 1 {
		t.Fatalf("chats %v", e)
	}
	if page, e := s.Status(context.Background(), f.scope); e != nil || page.Messages != 1 {
		t.Fatalf("status %v", e)
	}
}
func TestRequestCancellationIsNotKeywordFallback(t *testing.T) {
	f := newFixture(t)
	f.add(t, "cancel", "chat", "keyword body")
	s := service(t, f.config(archive.IndexGeneration{}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, e := s.SearchDetails(ctx, f.scope, archive.SearchRequest{Query: "keyword"})
	if !errors.Is(e, context.Canceled) || len(result.Page.Hits) != 0 {
		t.Fatalf("cancelled query returned fallback: %#v %v", result, e)
	}
}

func TestOriginalMediaKindsSurviveRetrieval(t *testing.T) {
	for _, kind := range []archive.MessageKind{archive.MessageImage, archive.MessageVoice, archive.MessageUnsupported} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFixture(t)
			applied, e := f.store.ApplySourceEvent(context.Background(), archive.SourceEvent{Key: archive.SourceKey{Account: "account", Chat: "chat", Sender: "sender", Message: "media"}, Kind: archive.EventOriginal, SourceRevision: 1, OriginalDateKnown: true, OriginalSentAt: time.UnixMilli(f.clock.Load()).Add(-time.Minute), BodyKind: kind, Caption: "keyword original caption"}, f.policy.Generation)
			if e != nil {
				t.Fatal(e)
			}
			result, e := service(t, f.config(archive.IndexGeneration{})).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "keyword"})
			if e != nil || len(result.Page.Hits) != 1 || result.Page.Hits[0].Kind != kind || result.Page.Hits[0].MessageID != applied.MessageID {
				t.Fatalf("original kind lost %#v %v", result, e)
			}
		})
	}
}

func TestBoundaryWindowSkippedKeepsAllowedKeywordSource(t *testing.T) {
	f := newFixture(t)
	old := f.add(t, "before", "chat", "keyword old source")
	f.clock.Add(time.Minute.Milliseconds())
	allowed := f.add(t, "inside", "chat", "keyword allowed source")
	g := f.index(t, map[archive.MessageID][]float32{old: {1, 0}, allowed: {1, 0}}, true)
	scope := f.scope
	scope.From = time.UnixMilli(f.clock.Load()).Add(-time.Minute)
	result, e := service(t, f.config(g)).SearchDetails(context.Background(), scope, archive.SearchRequest{Query: "keyword"})
	if e != nil || len(result.Page.Hits) != 1 || result.Page.Hits[0].MessageID != allowed || result.Page.Hits[0].Snippet != "keyword allowed source" || len(result.Page.Hits[0].Sources) != 1 {
		t.Fatalf("boundary vector escaped or allowed lexical source lost %#v %v", result, e)
	}
}

func TestResponseByteCapFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.add(t, "large", "chat", "keyword body")
	c := f.config(archive.IndexGeneration{})
	c.MaxOutputBytes = 1
	result, err := service(t, c).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "keyword"})
	if !errors.Is(err, archive.ErrLimitExceeded) || len(result.Page.Hits) != 0 {
		t.Fatalf("output cap returned plaintext %#v %v", result, err)
	}
}

func TestChangedActiveIndexUsesExplicitKeywordFallback(t *testing.T) {
	f := newFixture(t)
	id := f.add(t, "changed", "chat", "keyword body")
	g := f.index(t, nil)
	c := f.config(g)
	c.Candidates = &interleavedReader{CandidateReader: f.store, beforePair: func() error { return archive.ErrCursorStale }}
	result, err := service(t, c).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "keyword"})
	if err != nil || result.Mode != "keyword" || result.Code != "index_changed" || len(result.Page.Hits) != 1 || result.Page.Hits[0].MessageID != id {
		t.Fatalf("changed index fallback %#v %v", result, err)
	}
}

func TestIndexActivationBeforeHydrationStillReturnsKeywordFallback(t *testing.T) {
	f := newFixture(t)
	id := f.add(t, "late-change", "chat", "keyword body")
	g := f.index(t, nil)
	c := f.config(g)
	c.Candidates = &interleavedReader{CandidateReader: f.store, beforeHydrate: func() {
		replacement := g.Version
		replacement.ID = "replacement-v1"
		replacement.EncoderProfile = "replacement-profile"
		f.indexVersion(t, replacement, nil)
	}}
	result, err := service(t, c).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "keyword"})
	if err != nil || result.Mode != "keyword" || result.Code != "index_changed" || len(result.Page.Hits) != 1 || result.Page.Hits[0].MessageID != id {
		t.Fatalf("late activation exhausted keyword fallback %#v %v", result, err)
	}
}

func TestIndexActivationBeforeFinalCheckStillReturnsKeywordFallback(t *testing.T) {
	f := newFixture(t)
	id := f.add(t, "response-change", "chat", "keyword body")
	g := f.index(t, nil)
	c := f.config(g)
	c.Candidates = &interleavedReader{CandidateReader: f.store, afterHydrate: func() {
		replacement := g.Version
		replacement.ID = "response-replacement-v1"
		replacement.EncoderProfile = "replacement-profile"
		f.indexVersion(t, replacement, nil)
	}}
	result, err := service(t, c).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "keyword"})
	if err != nil || result.Mode != "keyword" || result.Code != "index_changed" || len(result.Page.Hits) != 1 || result.Page.Hits[0].MessageID != id {
		t.Fatalf("final check activation lost fallback %#v %v", result, err)
	}
}

func TestCandidateSourceRaceRetriesFreshSelection(t *testing.T) {
	f := newFixture(t)
	id := f.add(t, "race", "chat", "keyword body")
	c := f.config(archive.IndexGeneration{})
	c.Candidates = &interleavedReader{CandidateReader: f.store, beforePair: func() error { return archive.ErrSourceUnavailable }}
	result, err := service(t, c).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "keyword"})
	if err != nil || len(result.Page.Hits) != 1 || result.Page.Hits[0].MessageID != id {
		t.Fatalf("candidate source race lost fresh result %#v %v", result, err)
	}
}
