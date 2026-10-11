//go:build sqlite_fts5

package retrieval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/inference"
)

type queryEncoder struct {
	identity inference.TokenizerIdentity
	encode   func(context.Context, string) ([]float32, error)
}

func (q queryEncoder) TokenizerIdentity() inference.TokenizerIdentity { return q.identity }
func (q queryEncoder) EncodeQuery(ctx context.Context, text string) ([]float32, error) {
	return q.encode(ctx, text)
}

type fixture struct {
	store  *archive.Store
	clock  *atomic.Int64
	policy archive.ArchivePolicy
	scope  archive.ReadScope
	keys   map[archive.MessageID]archive.SourceKey
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	clock := new(atomic.Int64)
	clock.Store(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC).UnixMilli())
	store, e := archive.Open(context.Background(), dir, archive.Options{Now: func() time.Time { return time.UnixMilli(clock.Load()) }})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.Close() })
	policy, e := store.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if e != nil {
		t.Fatal(e)
	}
	return &fixture{store: store, clock: clock, policy: policy, scope: archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: policy.Generation}, keys: map[archive.MessageID]archive.SourceKey{}}
}
func (f *fixture) add(t *testing.T, id, chat, body string) archive.MessageID {
	t.Helper()
	key := archive.SourceKey{Account: "account", Chat: chat, Sender: "sender", Message: id}
	r, e := f.store.ApplySourceEvent(context.Background(), archive.SourceEvent{Key: key, Kind: archive.EventOriginal, SourceRevision: 1, OriginalDateKnown: true, OriginalSentAt: time.UnixMilli(f.clock.Load()).Add(-time.Minute), BodyKind: archive.MessageText, Body: body}, f.policy.Generation)
	if e != nil {
		t.Fatal(e)
	}
	f.keys[r.MessageID] = key
	return r.MessageID
}
func (f *fixture) index(t *testing.T, vectors map[archive.MessageID][]float32, combine ...bool) archive.IndexGeneration {
	t.Helper()
	version := archive.IndexVersion{ID: "retrieval-fixture-v1", EncoderProfile: "synthetic", TokenizerSHA256: strings.Repeat("a", 64), TokenizerImplementation: "fixture", PrefixVersion: "fixture-prefix", ChunkingVersion: "fixture-windows", NormalizationVersion: archive.LexicalNormalizationVersion, Dimensions: 2, MaxPassageTokens: 256}
	return f.indexVersion(t, version, vectors, combine...)
}
func (f *fixture) indexVersion(t *testing.T, version archive.IndexVersion, vectors map[archive.MessageID][]float32, combine ...bool) archive.IndexGeneration {
	t.Helper()
	ctx := context.Background()
	generation, e := f.store.EnsureIndex(ctx, version)
	if e != nil {
		t.Fatal(e)
	}
	sequence := 0
	for {
		work, e := f.store.ClaimDirty(ctx, archive.DirtyClaim{Account: "account", Generation: generation.ID, TTL: time.Minute, Limit: 100})
		if errors.Is(e, archive.ErrNoWork) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		messages, e := f.store.LoadWindowMessages(ctx, work)
		if e != nil {
			t.Fatal(e)
		}
		publication := archive.WindowPublication{Work: work, Version: version}
		if len(combine) > 0 && combine[0] && len(messages) > 0 {
			sequence++
			document := archive.Document{ID: archive.DocumentID(fmt.Sprintf("%s-window-%d", version.ID, sequence)), Kind: archive.DocumentWindow, PassageTokens: 5, Provenance: archive.DerivedProvenance{Kind: archive.DocumentWindow, OffsetKind: "none"}}
			var texts []string
			for _, message := range messages {
				text, spans := fixtureWindowParts(message)
				if text == "" {
					continue
				}
				document.Sources = append(document.Sources, message.Source)
				document.Spans = append(document.Spans, spans...)
				texts = append(texts, text)
			}
			document.Text = strings.Join(texts, "\n")
			if document.Text != "" {
				publication.Documents = append(publication.Documents, document)
			}
		} else {
			for _, message := range messages {
				text, spans := fixtureWindowParts(message)
				if text == "" {
					continue
				}
				sequence++
				publication.Documents = append(publication.Documents, archive.Document{ID: archive.DocumentID(fmt.Sprintf("%s-window-%d", version.ID, sequence)), Kind: archive.DocumentWindow, Text: text, Sources: []archive.SourceRef{message.Source}, Spans: spans, PassageTokens: 5, Provenance: archive.DerivedProvenance{Kind: archive.DocumentWindow, OffsetKind: "none"}})
			}
		}
		if ok, e := f.store.PublishWindows(ctx, publication); e != nil || !ok {
			t.Fatalf("publish windows %v %v", ok, e)
		}
	}
	for {
		job, e := f.store.ClaimJob(ctx, archive.JobClaim{Account: "account", Kind: archive.JobTextEmbedding, TTL: time.Minute})
		if errors.Is(e, archive.ErrNoWork) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		input, e := f.store.LoadJobInput(ctx, job.Lease)
		if e != nil {
			t.Fatal(e)
		}
		result := archive.JobResult{Lease: job.Lease, Generation: job.Generation, Version: job.Version, Sources: job.Sources, ContentHash: job.ContentHash}
		for _, document := range input.Documents {
			v := vectors[document.Sources[0].Version.MessageID]
			if v == nil {
				v = []float32{0, 1}
			}
			result.Embeddings = append(result.Embeddings, archive.Embedding{DocumentID: document.ID, Values: v})
		}
		if ok, e := f.store.CommitJob(ctx, result); e != nil || !ok {
			t.Fatalf("commit vector %v %v", ok, e)
		}
	}
	if e := f.store.ActivateIndex(ctx, generation.ID); e != nil {
		t.Fatal(e)
	}
	generation.State = "active"
	return generation
}

// Publish every nonempty original field in source/field order. The fixture uses
// synthetic token counts/vectors, while archive activation checks real coverage.
func fixtureWindowParts(message archive.WindowMessage) (string, []archive.SourceSpan) {
	var parts []string
	var spans []archive.SourceSpan
	for _, field := range []struct{ name, text string }{{"body", message.Body}, {"caption", message.Caption}} {
		if field.text != "" {
			parts = append(parts, field.text)
			spans = append(spans, archive.SourceSpan{Source: message.Source, Field: field.name, EndByte: len(field.text)})
		}
	}
	return strings.Join(parts, "\n"), spans
}

func (f *fixture) config(g archive.IndexGeneration) Config {
	c := Config{Reader: f.store, Candidates: f.store}
	if g.ID != "" {
		c.Index = func(context.Context) (archive.IndexGeneration, error) { return g, nil }
		c.Encoder = queryEncoder{identity: inference.TokenizerIdentity{ProfileID: g.Version.EncoderProfile, TokenizerSHA256: g.Version.TokenizerSHA256, Implementation: g.Version.TokenizerImplementation, PrefixVersion: g.Version.PrefixVersion}, encode: func(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil }}
	}
	return c
}
func service(t *testing.T, c Config) *Service {
	t.Helper()
	s, e := NewService(c)
	if e != nil {
		t.Fatal(e)
	}
	return s
}

// Dropping the keyword-only branch or its explicit status loses offline search.
func TestKeywordFallbackExplicit(t *testing.T) {
	f := newFixture(t)
	id := f.add(t, "fallback", "chat", "שָׁלוֹם עולם")
	s := service(t, f.config(archive.IndexGeneration{}))
	result, e := s.SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "שלום"})
	if e != nil || result.Mode != "keyword" || result.Code != "model_missing" || len(result.Page.Hits) != 1 || result.Page.Hits[0].MessageID != id || result.Page.Hits[0].Snippet != "שָׁלוֹם עולם" {
		t.Fatalf("fallback result %#v error %v", result, e)
	}
	if len(result.Documents) != 1 || len(result.Page.Hits[0].Derived) != 0 {
		t.Fatal("original provenance or ordered details lost")
	}
}

// Requiring lexical overlap would discard the only relevant semantic source.
func TestSemanticHitWithoutKeywordOverlap(t *testing.T) {
	f := newFixture(t)
	id := f.add(t, "semantic", "chat", "חשבונית לתשלום")
	g := f.index(t, map[archive.MessageID][]float32{id: {1, 0}})
	result, e := service(t, f.config(g)).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "invoice", Limit: 1})
	if e != nil || result.Mode != "hybrid" || len(result.Page.Hits) != 1 || result.Page.Hits[0].MessageID != id || result.Page.Hits[0].Snippet != "חשבונית לתשלום" {
		t.Fatalf("semantic result %#v error %v", result, e)
	}
	if len(result.Page.Hits[0].Sources) != 1 || len(result.Documents) != 1 {
		t.Fatal("exact source membership missing")
	}
}

// Global nearest-neighbor selection followed by filtering would lose this hit.
func TestScopeAppliedBeforeTopK(t *testing.T) {
	f := newFixture(t)
	allowed := f.add(t, "allowed", "allowed-chat", "permitted source")
	vectors := map[archive.MessageID][]float32{allowed: {0, 1}}
	for i := 0; i < 101; i++ {
		id := f.add(t, fmt.Sprintf("denied-%d", i), "denied-chat", "private nearer source")
		vectors[id] = []float32{1, 0}
	}
	g := f.index(t, vectors)
	contextPage, e := f.store.Context(context.Background(), f.scope, archive.ContextRequest{MessageID: allowed})
	if e != nil {
		t.Fatal(e)
	}
	scope := f.scope
	scope.AllChats = false
	scope.Chats = []archive.ChatID{contextPage.Messages[0].ChatID}
	result, e := service(t, f.config(g)).SearchDetails(context.Background(), scope, archive.SearchRequest{Query: "concept", Limit: 1})
	if e != nil || len(result.Page.Hits) != 1 || result.Page.Hits[0].MessageID != allowed {
		t.Fatalf("scoped top-k %#v %v", result, e)
	}
}

func TestQueryValidatedBeforeEncoder(t *testing.T) {
	f := newFixture(t)
	f.add(t, "validation", "chat", "valid body")
	g := f.index(t, nil)
	c := f.config(g)
	encoder := c.Encoder.(queryEncoder)
	encoder.encode = func(context.Context, string) ([]float32, error) {
		t.Fatal("invalid query invoked encoder")
		return nil, nil
	}
	c.Encoder = encoder
	s := service(t, c)
	for _, query := range []string{"", "   ", string([]byte{0xff}), strings.Repeat("x", 1025), "***"} {
		if _, e := s.SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: query}); !errors.Is(e, archive.ErrInvalidQuery) {
			t.Fatalf("invalid query accepted: %v", e)
		}
	}
}

func TestEncoderFailureAndProfileMismatchFallback(t *testing.T) {
	for _, scenario := range []string{"failed", "dimension", "nonfinite", "identity"} {
		t.Run(scenario, func(t *testing.T) {
			f := newFixture(t)
			id := f.add(t, "fallback", "chat", "keyword body")
			g := f.index(t, nil)
			c := f.config(g)
			encoder := c.Encoder.(queryEncoder)
			switch scenario {
			case "failed":
				encoder.encode = func(context.Context, string) ([]float32, error) { return nil, inference.ErrUnavailable }
			case "dimension":
				encoder.encode = func(context.Context, string) ([]float32, error) { return []float32{1}, nil }
			case "nonfinite":
				encoder.encode = func(context.Context, string) ([]float32, error) { return []float32{float32(math.NaN()), 0}, nil }
			case "identity":
				encoder.identity.ProfileID = "different-model"
			}
			c.Encoder = encoder
			result, e := service(t, c).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "keyword"})
			if e != nil || result.Mode != "keyword" || result.Code == "" || len(result.Page.Hits) != 1 || result.Page.Hits[0].MessageID != id {
				t.Fatalf("fallback %s %#v %v", scenario, result, e)
			}
		})
	}
}

// The query must finish before the archive read snapshot is opened.
func TestEncodePrecedesCandidateSnapshot(t *testing.T) {
	f := newFixture(t)
	id := f.add(t, "first", "chat", "original source")
	g := f.index(t, map[archive.MessageID][]float32{id: {1, 0}})
	c := f.config(g)
	encoder := c.Encoder.(queryEncoder)
	encoder.encode = func(context.Context, string) ([]float32, error) {
		f.add(t, "during-encode", "chat", "new keyword source")
		return []float32{1, 0}, nil
	}
	c.Encoder = encoder
	result, e := service(t, c).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "keyword"})
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, hit := range result.Page.Hits {
		if hit.Snippet == "new keyword source" {
			found = true
		}
	}
	if !found {
		t.Fatal("candidate snapshot predates encoder completion")
	}
}

func TestSearchBoundsValidatedBeforeEncoder(t *testing.T) {
	f := newFixture(t)
	f.add(t, "validation", "chat", "valid body")
	g := f.index(t, nil)
	c := f.config(g)
	encoder := c.Encoder.(queryEncoder)
	encoder.encode = func(context.Context, string) ([]float32, error) {
		t.Fatal("invalid bounds invoked encoder")
		return nil, nil
	}
	c.Encoder = encoder
	s := service(t, c)
	badTime := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"scope-date", "scope-chats", "request-date", "request-order", "request-chats", "request-chat-cap"} {
		t.Run(scenario, func(t *testing.T) {
			scope := f.scope
			request := archive.SearchRequest{Query: "valid"}
			want := archive.ErrInvalidQuery
			switch scenario {
			case "scope-date":
				scope.From = badTime
				want = archive.ErrUnauthorized
			case "scope-chats":
				scope.Chats = []archive.ChatID{""}
				want = archive.ErrUnauthorized
			case "request-date":
				request.Until = badTime
			case "request-order":
				request.From = time.Unix(2, 0)
				request.Until = time.Unix(1, 0)
			case "request-chats":
				request.Chats = []archive.ChatID{""}
			case "request-chat-cap":
				request.Chats = make([]archive.ChatID, 4097)
			}
			if _, err := s.SearchDetails(context.Background(), scope, request); !errors.Is(err, want) {
				t.Fatalf("bounds accepted: %v", err)
			}
		})
	}
}

func TestEncoderTimeoutKeepsKeywordAvailable(t *testing.T) {
	f := newFixture(t)
	id := f.add(t, "timeout", "chat", "keyword body")
	g := f.index(t, nil)
	c := f.config(g)
	c.QueryTimeout = time.Millisecond
	encoder := c.Encoder.(queryEncoder)
	encoder.encode = func(ctx context.Context, _ string) ([]float32, error) { <-ctx.Done(); return nil, ctx.Err() }
	c.Encoder = encoder
	result, err := service(t, c).SearchDetails(context.Background(), f.scope, archive.SearchRequest{Query: "keyword"})
	if err != nil || result.Mode != "keyword" || result.Code != "encoder_timeout" || len(result.Page.Hits) != 1 || result.Page.Hits[0].MessageID != id {
		t.Fatalf("timeout lost keyword result %#v %v", result, err)
	}
}
