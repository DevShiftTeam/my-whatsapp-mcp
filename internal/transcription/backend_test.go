package transcription

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/media"
)

const testToken = "synthetic-transcription-token-0123456789"

func testRecipe() recipe {
	return recipe{
		ProtocolVersion: 1, Backend: "faster-whisper", Model: "ivrit-ai/whisper-large-v3-turbo-ct2",
		Revision: strings.Repeat("a", 40), ModelSHA256: strings.Repeat("b", 64),
		ModelFiles: []modelFile{{Path: "config.json", SHA256: strings.Repeat("c", 64)}, {Path: "model.bin", SHA256: strings.Repeat("b", 64)}, {Path: "tokenizer.json", SHA256: strings.Repeat("d", 64)}},
		Runtime:    runtimeRecipe{FasterWhisper: "1.2.1", CTranslate2: "4.8.2", AV: "18.0.0", ONNXRuntime: "1.31.0", Tokenizers: "0.22.1", ComputeType: "int8", CPUThreads: 4, NumWorkers: 1, Language: "he", VADFilter: true},
	}
}

func testInfo(t *testing.T, r recipe) infoResponse {
	t.Helper()
	b, err := canonicalJSON(r)
	if err != nil {
		t.Fatal(err)
	}
	return infoResponse{Recipe: r, Identity: digest(b)}
}

func testBackend(t *testing.T, handle func(http.ResponseWriter, *http.Request, infoResponse)) (*Backend, *httptest.Server) {
	t.Helper()
	info := testInfo(t, testRecipe())
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("missing bearer authentication")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/info" {
			if r.Method != http.MethodGet {
				t.Error("wrong metadata method")
			}
			_ = json.NewEncoder(w).Encode(info)
			return
		}
		if r.URL.Path != "/v1/transcribe" || r.Method != http.MethodPost {
			t.Error("unexpected endpoint")
			w.WriteHeader(404)
			return
		}
		handle(w, r, info)
	}))
	t.Cleanup(s.Close)
	b, err := New(context.Background(), Options{URL: s.URL, Token: testToken, Language: "he"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return b, s
}

type testOriginal struct {
	path     string
	metadata media.OriginalMetadata
	opens    int
	onOpen   func(int)
}

func (o *testOriginal) Metadata() media.OriginalMetadata { return o.metadata }
func (o *testOriginal) Open() (*os.File, error) {
	o.opens++
	if o.onOpen != nil {
		o.onOpen(o.opens)
	}
	return os.Open(o.path)
}

func testInput(t *testing.T, b *Backend) (archive.ExtractionWork, *testOriginal, []byte) {
	t.Helper()
	data := []byte("synthetic verified audio bytes")
	path := filepath.Join(t.TempDir(), "voice.fixture")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	source := archive.SourceRef{Account: "synthetic-account", Version: archive.SourceVersion{MessageID: "voice-message", Revision: 1, PolicyGeneration: 1}, ContentHash: digest([]byte("source"))}
	w := archive.ExtractionWork{ID: "voice-extraction", Lease: archive.JobLease{JobID: "voice-extraction", LeaseID: "lease"}, LeaseUntil: time.Now().Add(time.Minute), Version: b.Version(), Source: source, MediaID: "voice-media", ArtifactID: "voice-artifact", OriginalSHA256: digest(data), ContentHash: digest([]byte("work"))}
	o := &testOriginal{path: path, metadata: media.OriginalMetadata{MediaID: w.MediaID, ArtifactID: w.ArtifactID, Source: source, SHA256: w.OriginalSHA256, Bytes: int64(len(data)), Format: "opus"}}
	return w, o, data
}

func TestValidateOptionsRequiresLiteralLoopbackAndPrivateToken(t *testing.T) {
	valid := Options{URL: "http://127.0.0.1:8096", Token: testToken, Language: "he"}
	if err := ValidateOptions(valid); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://localhost:8096", "https://127.0.0.1:8096", "http://192.0.2.1:8096", "http://127.0.0.1", "http://127.0.0.1:0", "http://127.0.0.1:65536", "http://user@127.0.0.1:8096", "http://127.0.0.1:8096/api", "http://127.0.0.1:8096?token=secret", "http://127.0.0.1:8096#secret"} {
		t.Run(raw, func(t *testing.T) {
			o := valid
			o.URL = raw
			if err := ValidateOptions(o); !errors.Is(err, ErrInvalidRequest) {
				t.Fatal("unsafe URL accepted")
			}
		})
	}
	for _, mutate := range []func(*Options){func(o *Options) { o.Token = "short" }, func(o *Options) { o.Token += "\n" }, func(o *Options) { o.Language = "Auto" }, func(o *Options) { o.Timeout = time.Second }, func(o *Options) { o.Timeout = 31 * time.Minute }} {
		o := valid
		mutate(&o)
		if !errors.Is(ValidateOptions(o), ErrInvalidRequest) {
			t.Fatal("invalid options accepted")
		}
	}
}

func TestNewFreezesVerifiedRecipeAndReadyRejectsChange(t *testing.T) {
	var changed atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("metadata lacks authentication")
		}
		rc := testRecipe()
		if changed.Load() {
			rc.Runtime.CPUThreads++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(testInfo(t, rc))
	}))
	defer s.Close()
	b, err := New(context.Background(), Options{URL: s.URL, Token: testToken, Language: "he"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background())
	v := b.Version()
	if v.Kind != archive.JobTranscription || v.Language != "he" || v.ExtractorVersion != "faster-whisper-http-v1" || v.ProfileIdentity != testInfo(t, testRecipe()).Identity || len(v.RuntimeSHA256) != 64 {
		t.Fatal("recipe missing from durable version")
	}
	if err := b.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	changed.Store(true)
	if !errors.Is(b.Ready(context.Background()), ErrModelChanged) {
		t.Fatal("runtime recipe changed silently")
	}
	if b.Version() != v {
		t.Fatal("frozen version changed")
	}
}

func TestNewRejectsMalformedUnverifiedMetadata(t *testing.T) {
	mutations := []struct {
		name  string
		apply func(*infoResponse)
	}{
		{"protocol", func(x *infoResponse) { x.Recipe.ProtocolVersion = 2 }},
		{"backend", func(x *infoResponse) { x.Recipe.Backend = "untrusted" }},
		{"revision", func(x *infoResponse) { x.Recipe.Revision = "main" }},
		{"digest", func(x *infoResponse) { x.Recipe.ModelSHA256 = strings.Repeat("e", 64) }},
		{"missing tokenizer", func(x *infoResponse) { x.Recipe.ModelFiles = x.Recipe.ModelFiles[:2] }},
		{"path traversal", func(x *infoResponse) { x.Recipe.ModelFiles[0].Path = "../config.json" }},
		{"duplicate file", func(x *infoResponse) { x.Recipe.ModelFiles[1].Path = x.Recipe.ModelFiles[0].Path }},
		{"invalid language", func(x *infoResponse) { x.Recipe.Runtime.Language = "Auto" }},
		{"wrong language", func(x *infoResponse) { x.Recipe.Runtime.Language = "en" }},
		{"threads", func(x *infoResponse) { x.Recipe.Runtime.CPUThreads = 128 }},
		{"workers", func(x *infoResponse) { x.Recipe.Runtime.NumWorkers = 2 }},
		{"vad", func(x *infoResponse) { x.Recipe.Runtime.VADFilter = false }},
		{"float compute", func(x *infoResponse) { x.Recipe.Runtime.ComputeType = "float32" }},
		{"identity", func(x *infoResponse) { x.Identity = strings.Repeat("0", 64) }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			x := testInfo(t, testRecipe())
			tc.apply(&x)
			if tc.name != "identity" {
				raw, _ := canonicalJSON(x.Recipe)
				x.Identity = digest(raw)
			}
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(x)
			}))
			defer s.Close()
			b, err := New(context.Background(), Options{URL: s.URL, Token: testToken, Language: "he"})
			if b != nil {
				_ = b.Close(context.Background())
			}
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

func TestExtractPrimaryCopiesVerifiedAudioAndReturnsSourceBoundHebrew(t *testing.T) {
	b, _ := testBackend(t, func(w http.ResponseWriter, r *http.Request, info infoResponse) {
		data, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("X-Audio-Format") != "opus" || r.Header.Get("X-Audio-SHA256") != digest(data) || r.Header.Get("X-Transcription-Identity") != info.Identity {
			t.Error("audio request lacks integrity metadata")
		}
		_ = json.NewEncoder(w).Encode(transcribeResponse{Identity: info.Identity, AudioSHA256: digest(data), Language: "he", Text: "שלום בדיקת תמלול בעברית", End: 2.5})
	})
	work, original, _ := testInput(t, b)
	result, err := b.extractPrimary(context.Background(), work, original)
	if err != nil {
		t.Fatal(err)
	}
	if result.Lease != work.Lease || result.ContentHash != work.ContentHash || result.Outcome != archive.ExtractionText || len(result.Documents) != 1 {
		t.Fatal("wrong extraction result")
	}
	d := result.Documents[0]
	if d.Kind != archive.DocumentTranscript || d.Text != "שלום בדיקת תמלול בעברית" || len(d.Sources) != 1 || d.Sources[0] != work.Source || d.Provenance.ProfileID != work.Version.ProfileID || d.Provenance.ExtractorVersion != work.Version.ExtractorVersion || d.Provenance.PromptVersion != work.Version.PromptVersion || d.Provenance.MediaID != work.MediaID || d.Provenance.OffsetKind != "slice" || d.Provenance.EndMS != 2500 {
		t.Fatal("transcript lost provenance")
	}
	again, err := b.extractPrimary(context.Background(), work, original)
	if err != nil || len(again.Documents) != 1 || again.Documents[0].ID != d.ID {
		t.Fatal("document identity is not deterministic")
	}
}

func TestExtractPrimaryRejectsChangedSourceVersionAndExpiredLeaseBeforeUpload(t *testing.T) {
	var calls atomic.Int64
	b, _ := testBackend(t, func(w http.ResponseWriter, r *http.Request, info infoResponse) { calls.Add(1); w.WriteHeader(500) })
	mutations := []func(*archive.ExtractionWork, *testOriginal){
		func(w *archive.ExtractionWork, o *testOriginal) { w.Version.Language = "en" },
		func(w *archive.ExtractionWork, o *testOriginal) { w.Source.Version.PolicyGeneration++ },
		func(w *archive.ExtractionWork, o *testOriginal) { w.Source.Version.Revision++ },
		func(w *archive.ExtractionWork, o *testOriginal) { w.MediaID = "other" },
		func(w *archive.ExtractionWork, o *testOriginal) { w.ArtifactID = "other" },
		func(w *archive.ExtractionWork, o *testOriginal) { w.OriginalSHA256 = strings.Repeat("0", 64) },
		func(w *archive.ExtractionWork, o *testOriginal) { w.LeaseUntil = time.Now().Add(-time.Second) },
		func(w *archive.ExtractionWork, o *testOriginal) { w.Lease.JobID = "other" },
		func(w *archive.ExtractionWork, o *testOriginal) {
			w.Source.Version.ExpiresAt = time.Now().Add(-time.Second)
			o.metadata.Source = w.Source
		},
		func(w *archive.ExtractionWork, o *testOriginal) { o.metadata.Bytes = 25<<20 + 1 },
		func(w *archive.ExtractionWork, o *testOriginal) { o.metadata.Format = "webm" },
	}
	for i, mutate := range mutations {
		work, original, _ := testInput(t, b)
		mutate(&work, original)
		if _, err := b.extractPrimary(context.Background(), work, original); err == nil {
			t.Fatalf("invalid work %d accepted", i)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid work uploaded")
	}
}

func TestExtractPrimaryRejectsHashMismatchAndChangedOriginalAfterResponse(t *testing.T) {
	var calls atomic.Int64
	b, _ := testBackend(t, func(w http.ResponseWriter, r *http.Request, info infoResponse) {
		calls.Add(1)
		data, _ := io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(transcribeResponse{Identity: info.Identity, AudioSHA256: digest(data), Language: "he", Text: "בדיקה", End: 1})
	})
	work, original, _ := testInput(t, b)
	if err := os.WriteFile(original.path, []byte("changed audio bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.extractPrimary(context.Background(), work, original); !errors.Is(err, media.ErrSourceChanged) {
		t.Fatal("changed audio accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("changed audio uploaded")
	}
	work, original, _ = testInput(t, b)
	original.onOpen = func(n int) {
		if n == 2 {
			if err := os.WriteFile(original.path, []byte("changed after response"), 0600); err != nil {
				t.Error(err)
			}
		}
	}
	if _, err := b.extractPrimary(context.Background(), work, original); !errors.Is(err, media.ErrSourceChanged) {
		t.Fatal("original changed during inference accepted")
	}
}

func TestExtractPrimaryValidatesBoundedTranscriptResponse(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*transcribeResponse)
		raw    string
		want   error
		silent bool
	}{
		{name: "silent", mutate: func(x *transcribeResponse) { x.Text = ""; x.Silent = true }, silent: true},
		{name: "identity", mutate: func(x *transcribeResponse) { x.Identity = strings.Repeat("f", 64) }, want: ErrModelChanged},
		{name: "audio hash", mutate: func(x *transcribeResponse) { x.AudioSHA256 = strings.Repeat("f", 64) }, want: ErrInvalidResponse},
		{name: "language", mutate: func(x *transcribeResponse) { x.Language = "en" }, want: ErrInvalidResponse},
		{name: "empty", mutate: func(x *transcribeResponse) { x.Text = "" }, want: ErrInvalidResponse},
		{name: "silent with text", mutate: func(x *transcribeResponse) { x.Silent = true }, want: ErrInvalidResponse},
		{name: "nul", mutate: func(x *transcribeResponse) { x.Text = "a\x00b" }, want: ErrInvalidResponse},
		{name: "long text", mutate: func(x *transcribeResponse) { x.Text = strings.Repeat("x", 64<<10+1) }, want: ErrInvalidResponse},
		{name: "negative time", mutate: func(x *transcribeResponse) { x.Start = -1 }, want: ErrInvalidResponse},
		{name: "duration", mutate: func(x *transcribeResponse) { x.End = 1201 }, want: ErrInvalidResponse},
		{name: "no duration", mutate: func(x *transcribeResponse) { x.End = 0 }, want: ErrInvalidResponse},
		{name: "duplicate", raw: `{"identity":"a","identity":"b"}`, want: ErrInvalidResponse},
		{name: "unknown", raw: `{"unexpected":true}`, want: ErrInvalidResponse},
		{name: "oversized", raw: strings.Repeat(" ", 72<<10+1), want: ErrInvalidResponse},
		{name: "invalid utf8", raw: "{\"text\":\"\xff\"}", want: ErrInvalidResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := testBackend(t, func(w http.ResponseWriter, r *http.Request, info infoResponse) {
				data, _ := io.ReadAll(r.Body)
				response := transcribeResponse{Identity: info.Identity, AudioSHA256: digest(data), Language: "he", Text: "שלום", End: 1}
				if tc.mutate != nil {
					tc.mutate(&response)
				}
				if tc.raw != "" {
					_, _ = io.WriteString(w, tc.raw)
				} else {
					_ = json.NewEncoder(w).Encode(response)
				}
			})
			work, original, _ := testInput(t, b)
			result, err := b.extractPrimary(context.Background(), work, original)
			if tc.silent {
				if err != nil || result.Outcome != archive.ExtractionSilent || len(result.Documents) != 0 {
					t.Fatal("silence was not preserved")
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("invalid response accepted: %v", err)
			}
		})
	}
}

func TestBackendRejectsRedirectAndNeverLeaksRemoteErrorBody(t *testing.T) {
	var targetCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(302)
		_, _ = io.WriteString(w, "synthetic private transcript and token")
	}))
	defer s.Close()
	_, err := New(context.Background(), Options{URL: s.URL, Token: testToken, Language: "he"})
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "private") || targetCalls.Load() != 0 {
		t.Fatal("redirect followed or response body leaked")
	}
}

func TestExtractClassifiesInvalidAudioWithoutLeakingErrorBodies(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusBadRequest, media.ErrInvalidRequest},
		{http.StatusRequestEntityTooLarge, media.ErrLimit},
		{http.StatusUnsupportedMediaType, media.ErrUnsupported},
		{http.StatusUnprocessableEntity, media.ErrCorrupt},
		{http.StatusTooManyRequests, ErrUnavailable},
		{http.StatusServiceUnavailable, ErrUnavailable},
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrUnauthorized},
		{http.StatusConflict, ErrModelChanged},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			b, _ := testBackend(t, func(w http.ResponseWriter, r *http.Request, info infoResponse) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "synthetic private transcript and credential detail")
			})
			work, original, _ := testInput(t, b)
			result, err := b.extractPrimary(context.Background(), work, original)
			if !errors.Is(err, tc.want) || len(result.Documents) != 0 || strings.Contains(err.Error(), "private") {
				t.Fatal("wrong status classification or leaked service error")
			}
		})
	}
}

func TestExtractCancellationAndClosedBackend(t *testing.T) {
	started := make(chan struct{})
	b, _ := testBackend(t, func(w http.ResponseWriter, r *http.Request, info infoResponse) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	})
	work, original, _ := testInput(t, b)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := b.extractPrimary(ctx, work, original); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not begin")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("cancellation lost")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled request did not return")
	}
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.extractPrimary(context.Background(), work, original); !errors.Is(err, ErrUnavailable) {
		t.Fatal("closed backend accepts work")
	}
}

func TestCanonicalRecipeMatchesPythonSortedCompactASCIIJSON(t *testing.T) {
	raw, err := canonicalJSON(map[string]any{"z": "x", "a": map[string]any{"b": true, "a": 1}})
	if err != nil || string(raw) != `{"a":{"a":1,"b":true},"z":"x"}` {
		t.Fatal("canonical recipe differs from Python contract")
	}
	sum := sha256.Sum256(raw)
	if digest(raw) != hex.EncodeToString(sum[:]) {
		t.Fatal("wrong recipe digest")
	}
}

func TestAutomaticLanguageKeepsRecipeSelectionAndAcceptsDetectedLanguages(t *testing.T) {
	for _, language := range []string{"he", "en", "fr"} {
		t.Run(language, func(t *testing.T) {
			rc := testRecipe()
			rc.Runtime.Language = "auto"
			info := testInfo(t, rc)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/info" {
					_ = json.NewEncoder(w).Encode(info)
					return
				}
				data, _ := io.ReadAll(r.Body)
				_ = json.NewEncoder(w).Encode(transcribeResponse{Identity: info.Identity, AudioSHA256: digest(data), Language: language, Text: "synthetic multilingual transcript", End: 1})
			}))
			defer s.Close()
			b, err := New(context.Background(), Options{URL: s.URL, Token: testToken})
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close(context.Background())
			work, original, _ := testInput(t, b)
			result, err := b.extractPrimary(context.Background(), work, original)
			if err != nil || result.Outcome != archive.ExtractionText || b.Version().Language != "auto" {
				t.Fatal("automatic language lost recipe or transcript")
			}
		})
	}
}

func TestReadJSONRequiresEveryTranscriptFieldWithoutNulls(t *testing.T) {
	complete := map[string]any{"identity": strings.Repeat("a", 64), "audio_sha256": strings.Repeat("b", 64), "language": "he", "text": "שלום", "start": 0, "end": 1, "silent": false}
	for name := range complete {
		for _, null := range []bool{false, true} {
			fields := make(map[string]any, len(complete))
			for key, value := range complete {
				fields[key] = value
			}
			if null {
				fields[name] = nil
			} else {
				delete(fields, name)
			}
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			response := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}
			var transcript transcribeResponse
			if !errors.Is(readJSON(response, maximumResponseBytes, &transcript), ErrInvalidResponse) {
				t.Fatal("missing or null transcript field accepted")
			}
			_ = response.Body.Close()
		}
	}
}

func TestReadJSONRejectsTagAliasesAndLossySurrogates(t *testing.T) {
	response := transcribeResponse{Identity: strings.Repeat("a", 64), AudioSHA256: strings.Repeat("b", 64), Language: "he", Text: "synthetic", End: 1}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, text string
		valid      bool
	}{
		{"uppercase alias", strings.Replace(string(raw), `"text":"synthetic"`, `"Text":"synthetic"`, 1), false},
		{"alias override", strings.Replace(string(raw), `"text":"synthetic"`, `"text":"synthetic","Text":"changed"`, 1), false},
		{"lone high surrogate", strings.Replace(string(raw), `"synthetic"`, `"\ud800"`, 1), false},
		{"lone low surrogate", strings.Replace(string(raw), `"synthetic"`, `"\udfff"`, 1), false},
		{"surrogate pair", strings.Replace(string(raw), `"synthetic"`, `"\ud83d\ude00"`, 1), true},
		{"literal replacement", strings.Replace(string(raw), `"synthetic"`, `"�"`, 1), true},
		{"escaped literal", strings.Replace(string(raw), `"synthetic"`, `"\\ud800"`, 1), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.text))}
			defer r.Body.Close()
			var transcript transcribeResponse
			err := readJSON(r, maximumResponseBytes, &transcript)
			if tc.valid {
				if err != nil {
					t.Fatal("valid Unicode rejected")
				}
			} else if !errors.Is(err, ErrInvalidResponse) {
				t.Fatal("ambiguous or lossy JSON accepted")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestExtractJoinsDelayedRequestBodyClosureBeforeReleasingAudio(t *testing.T) {
	for _, transportError := range []bool{false, true} {
		t.Run(strconv.FormatBool(transportError), func(t *testing.T) {
			b, _ := testBackend(t, func(w http.ResponseWriter, r *http.Request, info infoResponse) {
				t.Error("unexpected ordinary transport request")
			})
			work, original, data := testInput(t, b)
			consumed := make(chan string, 1)
			allowClose := make(chan struct{})
			b.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				go func() {
					<-r.Context().Done()
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error("delayed upload could not read bytes")
					}
					consumed <- digest(body)
					<-allowClose
					_ = r.Body.Close()
				}()
				if transportError {
					return nil, errors.New("synthetic private transport detail")
				}
				output := transcribeResponse{Identity: b.identity, AudioSHA256: digest(data), Language: "he", Text: "שלום", End: 1}
				raw, _ := json.Marshal(output)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
			})
			done := make(chan error, 1)
			go func() { _, err := b.extractPrimary(context.Background(), work, original); done <- err }()
			select {
			case got := <-consumed:
				if got != digest(data) {
					t.Error("audio was wiped before transport read it")
				}
			case <-time.After(time.Second):
				close(allowClose)
				t.Fatal("transport did not reach delayed closure")
			}
			select {
			case <-done:
				t.Error("extract returned before request body closure")
			default:
			}
			close(allowClose)
			select {
			case err := <-done:
				if transportError {
					if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "private") {
						t.Error("unsafe transport error")
					}
				} else if err != nil {
					t.Error("successful extraction failed")
				}
			case <-time.After(time.Second):
				t.Fatal("request body closure was not joined")
			}
		})
	}
}
