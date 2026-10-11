package indexing

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/inference"
)

type fakeIndexStore struct {
	mu                                       sync.Mutex
	messages                                 []archive.WindowMessage
	work                                     archive.DirtyWork
	job                                      archive.Job
	docs                                     []archive.Document
	dirty, claimable, changed, expired       bool
	publication                              archive.WindowPublication
	result                                   archive.JobResult
	retry                                    archive.JobRetry
	currentCalls, renewals, activations      int
	loadErr, errorCommit, renewErr           error
	publishErr                               error
	claimLimits                              []int
	windowPreparing, derivedPreparing        int
	windowProgress                           archive.PublicationProgress
	derivedStart                             int
	derivedResults                           []archive.JobResult
	windowLoads, derivedLoads, dirtyRenewals int
	derivedMutate                            func(*archive.DerivedBatchInput)
}

func testVersion() archive.IndexVersion {
	id := (testTokenizer{}).TokenizerIdentity()
	return archive.IndexVersion{ID: "synthetic-index-v1", EncoderProfile: id.ProfileID, TokenizerSHA256: id.TokenizerSHA256, TokenizerImplementation: id.Implementation, PrefixVersion: id.PrefixVersion, ChunkingVersion: ChunkingVersion, NormalizationVersion: archive.LexicalNormalizationVersion, Dimensions: 384, MaxPassageTokens: 256}
}
func newFakeIndexStore() *fakeIndexStore {
	at := time.Now().UTC().Add(-time.Hour)
	return &fakeIndexStore{messages: []archive.WindowMessage{windowMessage("a", "one", "שלום synthetic", at)}, dirty: true, work: archive.DirtyWork{ID: "dirty", LeaseID: "dirty-lease", LeaseUntil: time.Now().Add(time.Minute), Account: "account", ChatID: "one", PolicyGeneration: 1, Generation: "generation", From: at.Add(-time.Minute), Until: at.Add(time.Minute), DirtyRevision: 1}}
}
func (f *fakeIndexStore) CurrentSource(context.Context, archive.SourceKey) (archive.SourceRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.currentCalls++
	return archive.SourceRef{}, archive.ErrSourceUnavailable
}
func (f *fakeIndexStore) EnsureIndex(_ context.Context, v archive.IndexVersion) (archive.IndexGeneration, error) {
	return archive.IndexGeneration{ID: "generation", Version: v, State: "building"}, nil
}
func (f *fakeIndexStore) ClaimDirty(_ context.Context, c archive.DirtyClaim) (archive.DirtyWork, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimLimits = append(f.claimLimits, c.Limit)
	if !f.dirty {
		return archive.DirtyWork{}, archive.ErrNoWork
	}
	f.dirty = false
	return f.work, nil
}
func (f *fakeIndexStore) LoadWindowMessages(context.Context, archive.DirtyWork) ([]archive.WindowMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]archive.WindowMessage(nil), f.messages...), f.loadErr
}
func (f *fakeIndexStore) PublishWindows(_ context.Context, p archive.WindowPublication) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publication = p
	if f.publishErr != nil {
		return false, f.publishErr
	}
	if f.changed {
		return false, nil
	}
	f.docs = p.Documents
	if len(f.docs) > 0 {
		d := f.docs[0]
		h, e := archive.CanonicalWorkDigest("generation", p.Version, archive.JobTextEmbedding, "", d.Sources, []archive.Document{d})
		if e != nil {
			return false, e
		}
		f.job = archive.Job{ID: "job", Kind: archive.JobTextEmbedding, Lease: archive.JobLease{JobID: "job", LeaseID: "job-lease"}, LeaseUntil: time.Now().Add(time.Minute), Attempts: 1, Generation: "generation", Version: p.Version, Sources: d.Sources, Documents: []archive.DocumentID{d.ID}, ContentHash: h}
		f.claimable = true
	}
	return true, nil
}
func (f *fakeIndexStore) ClaimJob(_ context.Context, c archive.JobClaim) (archive.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.claimable || c.Kind != f.job.Kind {
		return archive.Job{}, archive.ErrNoWork
	}
	f.claimable = false
	return f.job, nil
}

func (f *fakeIndexStore) LoadWindowBatch(_ context.Context, w archive.DirtyWork) (archive.WindowBatchInput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.windowLoads++
	input := archive.WindowBatchInput{Work: w}
	if f.loadErr != nil {
		return input, f.loadErr
	}
	if f.windowPreparing > 0 {
		f.windowPreparing--
		return input, nil
	}
	input.Ready, input.Cursor, input.Messages, input.Start = true, "window-cursor", append([]archive.WindowMessage(nil), f.messages...), archive.WindowPosition{Field: "body"}
	return input, nil
}
func (f *fakeIndexStore) RenewDirty(ctx context.Context, w archive.DirtyWork, _ time.Duration) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirtyRenewals++
	if f.changed || f.expired {
		return time.Time{}, archive.ErrNoWork
	}
	return w.LeaseUntil, nil
}
func (f *fakeIndexStore) PublishWindowBatch(ctx context.Context, p archive.WindowBatchPublication) (archive.PublicationProgress, error) {
	ok, err := f.PublishWindows(ctx, archive.WindowPublication{Work: p.Work, Version: p.Version, Documents: p.Documents})
	if err != nil || !ok {
		return archive.PublicationProgress{}, err
	}
	if f.windowProgress.Accepted {
		return f.windowProgress, nil
	}
	return archive.PublicationProgress{Accepted: true, Complete: true}, nil
}
func fakeOrigin(d archive.Document) archive.DerivedOrigin {
	hash := sha256.Sum256([]byte(d.Text))
	return archive.DerivedOrigin{ID: d.ID, TextSHA256: fmt.Sprintf("%x", hash), Bytes: len(d.Text), Provenance: d.Provenance, Sources: d.Sources}
}
func (f *fakeIndexStore) LoadDerivedBatch(_ context.Context, _ archive.JobLease) (archive.DerivedBatchInput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.derivedLoads++
	input := archive.DerivedBatchInput{Job: f.job, Origin: fakeOrigin(f.docs[0])}
	if f.loadErr != nil {
		return input, f.loadErr
	}
	if f.derivedPreparing > 0 {
		f.derivedPreparing--
		return input, nil
	}
	end := f.derivedStart + inference.MaxTextBytes
	if end > len(f.docs[0].Text) {
		end = len(f.docs[0].Text)
	}
	for end < len(f.docs[0].Text) && !utf8.RuneStart(f.docs[0].Text[end]) {
		end--
	}
	input.Ready, input.Cursor, input.StartByte, input.EndByte, input.Text = true, archive.PublicationCursor(fmt.Sprintf("derived-cursor-%d", f.derivedStart)), f.derivedStart, end, f.docs[0].Text[f.derivedStart:end]
	if f.derivedMutate != nil {
		f.derivedMutate(&input)
	}
	return input, nil
}
func (f *fakeIndexStore) CommitDerivedBatch(ctx context.Context, p archive.DerivedBatchPublication) (archive.PublicationProgress, error) {
	ok, err := f.CommitJob(ctx, p.Result)
	if err != nil || !ok {
		return archive.PublicationProgress{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.derivedResults = append(f.derivedResults, p.Result)
	for _, d := range p.Result.Documents {
		f.derivedStart += len(d.Text)
	}
	complete := f.derivedStart == len(f.docs[0].Text)
	progress := archive.PublicationProgress{Accepted: true, Complete: complete}
	if !complete {
		progress.Next = archive.PublicationCursor(fmt.Sprintf("derived-cursor-%d", f.derivedStart))
	}
	return progress, nil
}
func (f *fakeIndexStore) LoadJobInput(context.Context, archive.JobLease) (archive.JobInput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return archive.JobInput{Job: f.job, Documents: f.docs[:len(f.job.Documents)]}, f.loadErr
}
func (f *fakeIndexStore) RenewJob(ctx context.Context, _ archive.JobLease, _ time.Duration) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewals++
	if f.renewErr != nil {
		return time.Time{}, f.renewErr
	}
	if f.expired {
		return time.Time{}, archive.ErrNoWork
	}
	return time.Now().Add(time.Minute), nil
}
func (f *fakeIndexStore) CommitJob(_ context.Context, r archive.JobResult) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errorCommit != nil {
		return false, f.errorCommit
	}
	if f.changed || f.expired {
		return false, nil
	}
	f.result = r
	return true, nil
}
func (f *fakeIndexStore) RetryJob(_ context.Context, r archive.JobRetry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retry = r
	return nil
}
func (f *fakeIndexStore) ActivateIndex(context.Context, archive.IndexGenerationID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.publishErr != nil {
		return archive.ErrNoWork
	}
	f.activations++
	return nil
}

type fakeEncoder struct {
	before    func()
	err       error
	malformed bool
	calls     int
	texts     []string
	batches   []int
}

func (e *fakeEncoder) EncodeQuery(context.Context, string) ([]float32, error) {
	return nil, errors.New("unexpected query")
}
func (e *fakeEncoder) EncodePassages(ctx context.Context, texts []string) ([][]float32, error) {
	e.calls++
	e.texts = append(e.texts, texts...)
	e.batches = append(e.batches, len(texts))
	if e.before != nil {
		e.before()
	}
	if e.err != nil {
		return nil, e.err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.malformed {
		return [][]float32{{1}}, nil
	}
	v := make([][]float32, len(texts))
	for i := range v {
		v[i] = make([]float32, 384)
		v[i][0] = 1
	}
	return v, nil
}
func (*fakeEncoder) Close() error { return nil }
func testService(t *testing.T, f *fakeIndexStore, e *fakeEncoder) *Service {
	t.Helper()
	s, err := NewService(ServiceConfig{Store: f, Account: "account", Version: testVersion(), Tokenizer: testTokenizer{}, Encoder: e})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func stagedDerivedStore(t *testing.T, kind archive.DocumentKind, text string) *fakeIndexStore {
	t.Helper()
	f := newFakeIndexStore()
	f.dirty = false
	source := f.messages[0].Source
	d := archive.Document{ID: "immutable-origin", Kind: kind, Text: text, Sources: []archive.SourceRef{source}, Provenance: archive.DerivedProvenance{Kind: kind, MediaID: "media", ProfileID: "extractor-profile", ExtractorVersion: "extractor-v1", PromptVersion: "prompt-v1", OffsetKind: "none"}}
	if kind == archive.DocumentTranscript {
		d.Provenance.OffsetKind, d.Provenance.StartMS, d.Provenance.EndMS = "slice", 120, 2900
	}
	f.docs = []archive.Document{d}
	f.job = archive.Job{ID: "derived-job", Kind: archive.JobDerivedEmbedding, Lease: archive.JobLease{JobID: "derived-job", LeaseID: "derived-lease"}, LeaseUntil: time.Now().Add(time.Minute), Attempts: 1, Generation: "generation", Version: testVersion(), Sources: d.Sources, Documents: []archive.DocumentID{d.ID}, MediaID: "media"}
	var err error
	f.job.ContentHash, err = archive.CanonicalOriginWorkDigest(f.job.Generation, f.job.Version, fakeOrigin(d))
	if err != nil {
		t.Fatal(err)
	}
	f.claimable = true
	return f
}

func TestStagedDerivedTextRetokenizesEveryByteWithExactAuthority(t *testing.T) {
	text := "  " + strings.Repeat("שלום🙂\n", 180) + " \t"
	for _, kind := range []archive.DocumentKind{archive.DocumentOCR, archive.DocumentCaption, archive.DocumentTranscript} {
		t.Run(string(kind), func(t *testing.T) {
			f := stagedDerivedStore(t, kind, text)
			origin, claimed := f.docs[0], f.job
			e := &fakeEncoder{}
			s := testService(t, f, e)
			r, err := s.Step(context.Background())
			if err != nil || r.Embedded < 2 || len(f.result.Documents) != r.Embedded || len(f.result.Embeddings) != r.Embedded {
				t.Fatalf("derived partition: %#v %v", r, err)
			}
			if f.currentCalls != 0 || f.result.Lease != claimed.Lease || f.result.Generation != claimed.Generation || f.result.Version != claimed.Version || f.result.ContentHash != claimed.ContentHash || !reflect.DeepEqual(f.result.Sources, claimed.Sources) {
				t.Fatal("derived partition upgraded claimed authority")
			}
			var joined strings.Builder
			for i, d := range f.result.Documents {
				start := joined.Len()
				joined.WriteString(d.Text)
				id := archive.DocumentID(fmt.Sprintf("%s-%d-%d", claimed.ID, start, joined.Len()))
				count, countErr := (testTokenizer{}).CountPassage(context.Background(), d.Text)
				if d.ID != id || d.Kind != origin.Kind || d.Provenance != origin.Provenance || !reflect.DeepEqual(d.Sources, origin.Sources) || len(d.Spans) != 0 || !utf8.ValidString(d.Text) || len(d.Text) > inference.MaxTextBytes || countErr != nil || count != d.PassageTokens || count < 1 || count > claimed.Version.MaxPassageTokens || f.result.Embeddings[i].DocumentID != id {
					t.Fatal("fragment lost exact bytes, tokens, provenance, or vector identity")
				}
			}
			if joined.String() != text || strings.Join(e.texts, "") != text || !reflect.DeepEqual(f.docs[0], origin) {
				t.Fatal("derived text was truncated, normalized, or mutated")
			}
			for _, batch := range e.batches {
				if batch < 1 || batch > inference.MaxBatchItems {
					t.Fatal("unbounded embedding batch")
				}
			}
		})
	}
}

type fragmentTokenizer struct {
	testTokenizer
	split func(string) []inference.TextSpan
}

func (t fragmentTokenizer) SplitPassage(_ context.Context, text string, _ int) ([]inference.TextSpan, error) {
	return t.split(text), nil
}

func TestStagedDerivedMalformedPartitionsNeverEncodeOrCommit(t *testing.T) {
	text := strings.Repeat("שלום🙂", 90)
	for _, test := range []struct {
		name  string
		split func(string) []inference.TextSpan
	}{
		{"gap", func(s string) []inference.TextSpan {
			return []inference.TextSpan{{Start: 2, End: len(s), Text: s[2:], Tokens: 3}}
		}},
		{"partial", func(s string) []inference.TextSpan {
			return []inference.TextSpan{{Start: 0, End: 2, Text: s[:2], Tokens: 3}}
		}},
		{"changed_text", func(s string) []inference.TextSpan {
			return []inference.TextSpan{{Start: 0, End: 2, Text: "xx", Tokens: 4}}
		}},
		{"false_count", func(s string) []inference.TextSpan {
			return []inference.TextSpan{{Start: 0, End: 2, Text: s[:2], Tokens: 1}}
		}},
		{"broken_utf8", func(s string) []inference.TextSpan {
			return []inference.TextSpan{{Start: 0, End: 1, Text: s[:1], Tokens: 3}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := stagedDerivedStore(t, archive.DocumentOCR, text)
			e := &fakeEncoder{}
			s, err := NewService(ServiceConfig{Store: f, Account: "account", Version: testVersion(), Tokenizer: fragmentTokenizer{split: test.split}, Encoder: e})
			if err != nil {
				t.Fatal(err)
			}
			r, err := s.Step(context.Background())
			if err != nil || r.Code != "invalid_input" || !f.retry.Terminal || e.calls != 0 || len(f.result.Documents) != 0 || len(f.result.Embeddings) != 0 || f.activations != 0 {
				t.Fatalf("malformed partition admitted: %#v %v", r, err)
			}
		})
	}
}

func TestStagedDerivedExcessFragmentsPublishLosslessAdaptivePrefixes(t *testing.T) {
	f := stagedDerivedStore(t, archive.DocumentCaption, strings.Repeat("a", 300))
	e := &fakeEncoder{}
	tokenizer := fragmentTokenizer{split: func(text string) []inference.TextSpan {
		spans := make([]inference.TextSpan, len(text))
		for i := range text {
			spans[i] = inference.TextSpan{Start: i, End: i + 1, Text: text[i : i+1], Tokens: 3}
		}
		return spans
	}}
	s, err := NewService(ServiceConfig{Store: f, Account: "account", Version: testVersion(), Tokenizer: tokenizer, Encoder: e})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Step(context.Background())
	if err != nil || r.Code != "publication_pending" || r.Embedded != 1 || f.derivedStart != 150 || f.activations != 0 {
		t.Fatalf("adaptive first prefix: %#v %v", r, err)
	}
	r, err = s.Step(context.Background())
	if err != nil || r.Code != "" || r.Embedded != 1 || len(f.derivedResults) != 2 || f.derivedStart != 300 || strings.Join(e.texts, "") != f.docs[0].Text {
		t.Fatalf("adaptive complete prefix: %#v %v", r, err)
	}
}

type nativeCapTokenizer struct{ testTokenizer }

func (t nativeCapTokenizer) SplitPassage(ctx context.Context, text string, max int) ([]inference.TextSpan, error) {
	if utf8.RuneCountInString(text) > inference.MaxSplitChunks {
		return nil, inference.ErrLimit
	}
	return t.testTokenizer.SplitPassage(ctx, text, max)
}
func TestStagedDerivedNativeFragmentCapHalvesAtUTF8Boundary(t *testing.T) {
	text := strings.Repeat("🙂", 300)
	f := stagedDerivedStore(t, archive.DocumentOCR, text)
	e := &fakeEncoder{}
	s, err := NewService(ServiceConfig{Store: f, Account: "account", Version: testVersion(), Tokenizer: nativeCapTokenizer{}, Encoder: e})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Step(context.Background())
	if err != nil || r.Code != "publication_pending" || f.derivedStart != 600 || len(f.result.Documents) != 1 || !utf8.ValidString(f.result.Documents[0].Text) {
		t.Fatalf("native cap prefix: %#v %v", r, err)
	}
	r, err = s.Step(context.Background())
	if err != nil || r.Code != "" || f.derivedStart != len(text) || strings.Join(e.texts, "") != text || len(f.derivedResults) != 2 {
		t.Fatalf("native cap full text: %#v %v", r, err)
	}
}

func TestStagedDerivedLateSourceChangeRejectsWholePartition(t *testing.T) {
	f := stagedDerivedStore(t, archive.DocumentTranscript, strings.Repeat("שלום🙂", 100))
	e := &fakeEncoder{before: func() { f.mu.Lock(); f.changed = true; f.mu.Unlock() }}
	r, err := testService(t, f, e).Step(context.Background())
	if err != nil || r.Code != "superseded" || r.Embedded != 0 || len(f.result.Documents) != 0 || len(f.result.Embeddings) != 0 || f.activations != 0 {
		t.Fatalf("stale derived partition published: %#v %v", r, err)
	}
}

func TestStagedDerivedAdmissionAndBoundedTokenizerEnvelope(t *testing.T) {
	t.Run("full envelope", func(t *testing.T) {
		text := strings.Repeat(" ", inference.MaxTextBytes)
		f := stagedDerivedStore(t, archive.DocumentOCR, text)
		e := &fakeEncoder{}
		largest := 0
		tokenizer := boundedRequestTokenizer{testTokenizer: testTokenizer{cheap: true}, largest: &largest}
		s, err := NewService(ServiceConfig{Store: f, Account: "account", Version: testVersion(), Tokenizer: tokenizer, Encoder: e})
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.Step(context.Background())
		if err != nil || r.Embedded != 1 || len(f.result.Documents) != 1 || f.result.Documents[0].Text != text || largest != inference.MaxTextBytes {
			t.Fatalf("full bounded envelope lost: %#v %v", r, err)
		}
	})
	for _, test := range []struct {
		name   string
		mutate func(*fakeIndexStore)
	}{
		{"text_window_zero", func(f *fakeIndexStore) {
			f.job.Kind = archive.JobTextEmbedding
			f.docs[0].Kind = archive.DocumentWindow
		}},
		{"derived_window", func(f *fakeIndexStore) { f.docs[0].Provenance.Kind = archive.DocumentWindow }},
		{"two_origins", func(f *fakeIndexStore) {
			d := f.docs[0]
			d.ID = "second-origin"
			f.docs = append(f.docs, d)
			f.job.Documents = append(f.job.Documents, d.ID)
		}},
		{"oversize", func(f *fakeIndexStore) { f.docs[0].Text = strings.Repeat("a", (64<<10)+1) }},
		{"bad_utf8", func(f *fakeIndexStore) { f.docs[0].Text = "\xff" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := stagedDerivedStore(t, archive.DocumentOCR, "text")
			test.mutate(f)
			e := &fakeEncoder{}
			r, err := testService(t, f, e).Step(context.Background())
			if err != nil || r.Code != "invalid_input" || !f.retry.Terminal || e.calls != 0 || len(f.result.Documents) != 0 || f.activations != 0 {
				t.Fatalf("invalid staging admitted: %#v %v", r, err)
			}
		})
	}
}

func TestPublicationPreparationRetainsAndRenewsExactWork(t *testing.T) {
	t.Run("window", func(t *testing.T) {
		f := newFakeIndexStore()
		f.windowPreparing = 2
		e := &fakeEncoder{}
		s := testService(t, f, e)
		for i := 0; i < 2; i++ {
			r, err := s.Step(context.Background())
			if err != nil || r.Code != "preparing" || f.windowLoads != i+1 || f.dirtyRenewals != i+1 || len(f.claimLimits) != 1 || f.activations != 0 || e.calls != 0 {
				t.Fatalf("window prep: %#v %v", r, err)
			}
		}
		r, err := s.Step(context.Background())
		if err != nil || r.Published != 1 || r.Embedded != 1 || !reflect.DeepEqual(f.publication.Work, f.work) {
			t.Fatalf("prepared window: %#v %v", r, err)
		}
	})
	t.Run("derived", func(t *testing.T) {
		f := stagedDerivedStore(t, archive.DocumentOCR, "complete root")
		f.derivedPreparing = 2
		e := &fakeEncoder{}
		s := testService(t, f, e)
		for i := 0; i < 2; i++ {
			r, err := s.Step(context.Background())
			if err != nil || r.Code != "preparing" || f.derivedLoads != i+1 || f.renewals != i+1 || s.derived == nil || f.activations != 0 || e.calls != 0 {
				t.Fatalf("derived prep: %#v %v", r, err)
			}
		}
		r, err := s.Step(context.Background())
		if err != nil || r.Embedded != 1 || s.derived != nil || f.result.ContentHash != f.job.ContentHash {
			t.Fatalf("prepared root: %#v %v", r, err)
		}
	})
}

func TestDerivedWhole64KiBRootPublishesGlobalExactRanges(t *testing.T) {
	text := strings.Repeat("שלום🙂", (64<<10)/12)
	f := stagedDerivedStore(t, archive.DocumentTranscript, text)
	e := &fakeEncoder{}
	s, err := NewService(ServiceConfig{Store: f, Account: "account", Version: testVersion(), Tokenizer: testTokenizer{cheap: true}, Encoder: e})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		r, err := s.Step(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if r.Activated {
			break
		}
		if i == 7 {
			t.Fatal("root did not finish")
		}
	}
	if len(f.derivedResults) < 4 || strings.Join(e.texts, "") != text || f.derivedStart != len(text) {
		t.Fatal("full root lost across bounded slices")
	}
	start := 0
	for _, result := range f.derivedResults {
		if result.Lease != f.job.Lease || result.ContentHash != f.job.ContentHash || result.Version != f.job.Version || !reflect.DeepEqual(result.Sources, f.job.Sources) {
			t.Fatal("batch upgraded root authority")
		}
		for _, d := range result.Documents {
			end := start + len(d.Text)
			if d.ID != archive.DocumentID(fmt.Sprintf("%s-%d-%d", f.job.ID, start, end)) || d.Text != text[start:end] || !utf8.ValidString(d.Text) || d.Provenance != f.docs[0].Provenance {
				t.Fatal("global root byte ranges changed")
			}
			start = end
		}
	}
}

func TestDerivedLoadedRangeAndWholeOriginFences(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*archive.DerivedBatchInput)
	}{
		{"version", func(i *archive.DerivedBatchInput) { i.Job.Version.ID = "newer-claimed-version" }},
		{"policy", func(i *archive.DerivedBatchInput) {
			i.Job.Sources = append([]archive.SourceRef(nil), i.Job.Sources...)
			i.Job.Sources[0].Version.PolicyGeneration++
		}},
		{"origin_hash", func(i *archive.DerivedBatchInput) { i.Origin.TextSHA256 = strings.Repeat("c", 64) }},
		{"origin_provenance", func(i *archive.DerivedBatchInput) { i.Origin.Provenance.ProfileID = "different-extractor" }},
		{"range", func(i *archive.DerivedBatchInput) { i.EndByte++ }},
		{"cursor", func(i *archive.DerivedBatchInput) { i.Cursor = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := stagedDerivedStore(t, archive.DocumentOCR, "root text")
			f.derivedMutate = test.mutate
			e := &fakeEncoder{}
			r, err := testService(t, f, e).Step(context.Background())
			if err != nil || r.Code != "invalid_input" || !f.retry.Terminal || e.calls != 0 || len(f.derivedResults) != 0 {
				t.Fatalf("changed origin authority: %#v %v", r, err)
			}
		})
	}
}

type cleanupTokenizer struct {
	testTokenizer
	entered, cleanup, release chan struct{}
	started                   bool
}

func (t *cleanupTokenizer) CountPassage(ctx context.Context, text string) (int, error) {
	if !t.started {
		t.started = true
		close(t.entered)
		<-ctx.Done()
		close(t.cleanup)
		<-t.release
	}
	return t.testTokenizer.CountPassage(ctx, text)
}
func TestWindowCancellationRenewsThroughTokenizerCleanup(t *testing.T) {
	f := newFakeIndexStore()
	tokenizer := &cleanupTokenizer{entered: make(chan struct{}), cleanup: make(chan struct{}), release: make(chan struct{})}
	s, err := NewService(ServiceConfig{Store: f, Account: "account", Version: testVersion(), Tokenizer: tokenizer, Encoder: &fakeEncoder{}, LeaseTTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := s.Step(ctx); done <- err }()
	<-tokenizer.entered
	cancel()
	<-tokenizer.cleanup
	time.Sleep(400 * time.Millisecond)
	f.mu.Lock()
	renewals := f.dirtyRenewals
	f.mu.Unlock()
	close(tokenizer.release)
	if err = <-done; !errors.Is(err, context.Canceled) || renewals < 2 || len(f.publication.Documents) != 0 {
		t.Fatalf("dirty lease abandoned before native tokenizer returned: renewals%d %v", renewals, err)
	}
}

func TestUnsupportedTextStaysExplicitAndNeverDropsNUL(t *testing.T) {
	f := stagedDerivedStore(t, archive.DocumentOCR, "before\x00after")
	e := &fakeEncoder{}
	r, err := testService(t, f, e).Step(context.Background())
	if err != nil || r.Code != "unsupported" || f.retry.Terminal || e.calls != 0 || len(f.derivedResults) != 0 || f.activations != 0 {
		t.Fatalf("NUL text silently transformed or completed: %#v %v", r, err)
	}
}

func TestDirtyMetadataLimitShrinksOnlyFutureReclaimedPages(t *testing.T) {
	f := newFakeIndexStore()
	f.publishErr = archive.ErrLimitExceeded
	s := testService(t, f, &fakeEncoder{})
	for want := MaxInputMessages; want >= 1; want /= 2 {
		if _, err := s.Step(context.Background()); !errors.Is(err, archive.ErrLimitExceeded) {
			t.Fatalf("limit result: %v", err)
		}
		if got := f.claimLimits[len(f.claimLimits)-1]; got != want || !reflect.DeepEqual(f.publication.Work, f.work) || f.activations != 0 {
			t.Fatalf("limit changed authority or page: %d want %d", got, want)
		}
		if want == 1 {
			break
		}
		// The store alone controls expiry/reclaim. No work can be claimed before
		// it exposes the lease again, even though the next page limit is smaller.
		if _, err := s.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.dirty = true
	}
	f.dirty = true
	if _, err := s.Step(context.Background()); !errors.Is(err, archive.ErrLimitExceeded) || f.claimLimits[len(f.claimLimits)-1] != 1 {
		t.Fatalf("minimum page: %v", err)
	}
}

func TestDirtyBuilderDocumentLimitPublishesContiguousBoundedPrefix(t *testing.T) {
	f := newFakeIndexStore()
	f.messages = nil
	for i := 0; i < MaxInputMessages; i++ {
		f.messages = append(f.messages, windowMessage(fmt.Sprintf("source-%03d", i), "one", strings.Repeat("x", 300), f.work.From.Add(time.Second)))
	}
	s := testService(t, f, &fakeEncoder{})
	f.windowProgress = archive.PublicationProgress{Accepted: true, Next: "next-window-cursor"}
	r, err := s.Step(context.Background())
	if err != nil || r.Published != MaxDocuments || r.Code != "publication_pending" || s.dirtyLimit != MaxInputMessages || f.publication.Work.ID != f.work.ID || f.activations != 0 {
		t.Fatalf("bounded window prefix: %#v %v", r, err)
	}
}

func TestServicePublishesAndEmbedsExactClaim(t *testing.T) {
	f := newFakeIndexStore()
	e := &fakeEncoder{}
	s := testService(t, f, e)
	r, err := s.Step(context.Background())
	if err != nil || r.Published != 1 || r.Embedded != 1 {
		t.Fatalf("step: %#v %v", r, err)
	}
	if f.currentCalls != 0 {
		t.Fatal("consumer replaced expected source with current authority")
	}
	if f.renewals == 0 {
		t.Fatal("job lease not renewed")
	}
	if f.result.Lease != f.job.Lease || f.result.Generation != f.job.Generation || f.result.Version != f.job.Version || f.result.ContentHash != f.job.ContentHash || !reflect.DeepEqual(f.result.Sources, f.job.Sources) {
		t.Fatal("committed authority differs from leased job")
	}
	if len(f.result.Embeddings) != 1 || f.result.Embeddings[0].DocumentID != f.docs[0].ID {
		t.Fatal("embedding source identity lost")
	}
	if len(f.result.Documents) != 0 {
		t.Fatal("ordinary text embedding replaced immutable window rows")
	}
	if !reflect.DeepEqual(f.publication.Work, f.work) {
		t.Fatal("dirty snapshot changed")
	}
}
func TestPolicyNarrowingRejectsLateResult(t *testing.T) {
	f := newFakeIndexStore()
	e := &fakeEncoder{before: func() { f.mu.Lock(); f.changed = true; f.mu.Unlock() }}
	s := testService(t, f, e)
	r, err := s.Step(context.Background())
	if err != nil || r.Embedded != 0 || r.Code != "superseded" || len(f.result.Embeddings) != 0 {
		t.Fatalf("late result: %#v %v", r, err)
	}
}
func TestExpiredLeaseCannotCommit(t *testing.T) {
	f := newFakeIndexStore()
	e := &fakeEncoder{before: func() { f.mu.Lock(); f.expired = true; f.mu.Unlock() }}
	s := testService(t, f, e)
	r, err := s.Step(context.Background())
	if err != nil || r.Embedded != 0 || len(f.result.Embeddings) != 0 {
		t.Fatalf("expired result: %#v %v", r, err)
	}
}
func TestMissingModelRetriesWithoutTerminalOrKeywordAuthority(t *testing.T) {
	f := newFakeIndexStore()
	s, err := NewService(ServiceConfig{Store: f, Account: "account", Version: testVersion(), Tokenizer: testTokenizer{}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Step(context.Background())
	if err != nil || r.Code != "model_unavailable" || f.retry.Code != "model_unavailable" || f.retry.Terminal || f.retry.After < 30*time.Second || f.retry.After > 5*time.Minute {
		t.Fatalf("missing model: %#v %#v %v", r, f.retry, err)
	}
	if f.currentCalls != 0 {
		t.Fatal("background retries touched source authority")
	}
}
func TestInvalidEmbeddingAndDigestNeverPublish(t *testing.T) {
	t.Run("malformed vector", func(t *testing.T) {
		f := newFakeIndexStore()
		s := testService(t, f, &fakeEncoder{malformed: true})
		r, e := s.Step(context.Background())
		if e != nil || r.Code != "invalid_result" || !f.retry.Terminal || len(f.result.Embeddings) > 0 {
			t.Fatalf("invalid result: %#v %v", r, e)
		}
	})
	t.Run("digest mismatch", func(t *testing.T) {
		f := newFakeIndexStore()
		s := testService(t, f, &fakeEncoder{})
		_, e := s.Step(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		f.mu.Lock()
		f.claimable = true
		f.job.ContentHash = strings.Repeat("c", 64)
		f.result = archive.JobResult{}
		f.mu.Unlock()
		r, e := s.Step(context.Background())
		if e != nil || r.Code != "invalid_input" || !f.retry.Terminal || len(f.result.Embeddings) > 0 {
			t.Fatalf("digest mismatch: %#v %v", r, e)
		}
	})
}
func TestServicePauseAndFixedErrorBoundary(t *testing.T) {
	f := newFakeIndexStore()
	s := testService(t, f, &fakeEncoder{})
	s.SetPaused(true)
	r, e := s.Step(context.Background())
	if e != nil || r.State != "paused" || !f.dirty {
		t.Fatalf("paused: %#v %v", r, e)
	}
	s.SetPaused(false)
	f.loadErr = errors.New("SECRET PRIVATE SOURCE BODY /private/path")
	_, e = s.Step(context.Background())
	if !errors.Is(e, ErrStore) || strings.Contains(e.Error(), "PRIVATE") {
		t.Fatalf("unsafe error: %v", e)
	}
}
func TestPauseCancelsAdmittedEmbeddingWithoutPublication(t *testing.T) {
	f := newFakeIndexStore()
	e := &fakeEncoder{}
	s := testService(t, f, e)
	e.before = func() { s.SetPaused(true) }
	r, err := s.Step(context.Background())
	if err != nil || r.Embedded != 0 || f.retry.Code != "cancelled" || len(f.result.Embeddings) != 0 || s.Snapshot().State != "paused" {
		t.Fatalf("pause active work: %#v %#v %v", r, s.Snapshot(), err)
	}
}
func TestIncompatibleGenerationRemainsPending(t *testing.T) {
	f := newFakeIndexStore()
	s := testService(t, f, &fakeEncoder{})
	if _, e := s.Step(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.claimable = true
	f.job.Generation = "another-profile-generation"
	f.job.Attempts = 100
	f.result = archive.JobResult{}
	f.mu.Unlock()
	r, e := s.Step(context.Background())
	if e != nil || r.Code != "model_unavailable" || f.retry.Terminal || len(f.result.Embeddings) > 0 {
		t.Fatalf("incompatible generation destroyed: %#v %#v %v", r, f.retry, e)
	}
}
func TestBackgroundRunIsBoundedPausedAndCancelable(t *testing.T) {
	f := newFakeIndexStore()
	s, e := NewBackgroundService(ServiceConfig{Store: f, Account: "account", Version: testVersion(), Tokenizer: testTokenizer{}, Paused: true, PollInterval: 10 * time.Millisecond})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for !s.Snapshot().Running {
		if time.Now().After(deadline) {
			t.Fatal("run did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if e = s.Run(context.Background()); !errors.Is(e, ErrBusy) {
		t.Fatalf("second run: %v", e)
	}
	cancel()
	if e = <-done; !errors.Is(e, context.Canceled) {
		t.Fatalf("run shutdown: %v", e)
	}
	if s.Snapshot().Running || !f.dirty {
		t.Fatal("paused run admitted work or retained running state")
	}
	if e = s.Run(nil); !errors.Is(e, ErrInvalidInput) {
		t.Fatalf("nil run context: %v", e)
	}
}
func TestServiceStepRejectsConcurrentAdmission(t *testing.T) {
	f := newFakeIndexStore()
	started, released := make(chan struct{}), make(chan struct{})
	e := &fakeEncoder{before: func() { close(started); <-released }}
	s := testService(t, f, e)
	done := make(chan error, 1)
	go func() { _, err := s.Step(context.Background()); done <- err }()
	<-started
	if _, err := s.Step(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent step: %v", err)
	}
	close(released)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestRenewalFailureCancelsNativeWorkBeforeCommit(t *testing.T) {
	f := newFakeIndexStore()
	e := &fakeEncoder{before: func() {
		f.mu.Lock()
		f.renewErr = archive.ErrSourceUnavailable
		f.mu.Unlock()
		time.Sleep(400 * time.Millisecond)
	}}
	s, err := NewService(ServiceConfig{Store: f, Account: "account", Version: testVersion(), Tokenizer: testTokenizer{}, Encoder: e, LeaseTTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Step(context.Background())
	if err != nil || r.Code != "superseded" || f.renewals < 2 || len(f.result.Embeddings) > 0 {
		t.Fatalf("renewal failure: %#v %v", r, err)
	}
}
func TestCancellationRetainsJobRenewalThroughEncoderCleanup(t *testing.T) {
	f := newFakeIndexStore()
	ctx, cancel := context.WithCancel(context.Background())
	entered, cleanup, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	e := &fakeEncoder{before: func() { close(entered); <-ctx.Done(); close(cleanup); <-release }}
	s, err := NewService(ServiceConfig{Store: f, Account: "account", Version: testVersion(), Tokenizer: testTokenizer{}, Encoder: e, LeaseTTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := s.Step(ctx); done <- e }()
	<-entered
	cancel()
	<-cleanup
	time.Sleep(400 * time.Millisecond)
	f.mu.Lock()
	renewals := f.renewals
	f.mu.Unlock()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if renewals < 2 || len(f.result.Embeddings) > 0 {
		t.Fatalf("lease renewal abandoned during native cleanup: %d", renewals)
	}
}
func TestNativeFailuresRemainTypedAndAttemptCapped(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		code     string
		terminal bool
	}{{"worker", errors.New("PRIVATE BODY /secret/path"), "worker_failed", false}, {"deadline", context.DeadlineExceeded, "timeout", false}, {"missing", inference.ErrUnavailable, "model_unavailable", false}, {"oversize", inference.ErrLimit, "invalid_input", true}} {
		t.Run(test.name, func(t *testing.T) {
			f := newFakeIndexStore()
			s := testService(t, f, &fakeEncoder{err: test.err})
			r, e := s.Step(context.Background())
			if e != nil || r.Code != test.code || f.retry.Terminal != test.terminal || len(f.result.Embeddings) != 0 {
				t.Fatalf("native failure: %#v %#v %v", r, f.retry, e)
			}
		})
	}
	f := newFakeIndexStore()
	s := testService(t, f, &fakeEncoder{})
	if _, e := s.Step(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.job.Attempts = 8
	f.claimable = true
	s.config.Encoder = &fakeEncoder{err: inference.ErrWorkerFailed}
	r, e := s.Step(context.Background())
	if e != nil || r.State != "failed" || !f.retry.Terminal {
		t.Fatalf("attempt cap: %#v %v", r, e)
	}
}
func TestTokenizerAndProfileBinding(t *testing.T) {
	f := newFakeIndexStore()
	version := testVersion()
	version.TokenizerSHA256 = strings.Repeat("c", 64)
	if _, e := NewService(ServiceConfig{Store: f, Account: "account", Version: version, Tokenizer: testTokenizer{}}); !errors.Is(e, ErrInvalidConfiguration) {
		t.Fatalf("profile mismatch: %v", e)
	}
	s, e := NewService(ServiceConfig{Store: f, Account: "account", Version: testVersion()})
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.Step(context.Background())
	if e != nil || r.Code != "tokenizer_unavailable" || !f.dirty {
		t.Fatalf("missing tokenizer admitted work: %#v %v", r, e)
	}
}
