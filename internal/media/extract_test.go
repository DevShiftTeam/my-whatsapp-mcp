package media

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/compute"
	"github.com/Bodokh/wa-indexer/internal/diskspace"
	"github.com/Bodokh/wa-indexer/internal/modelstore"
)

func installedMediaProfile(t *testing.T, id string) modelstore.InstalledProfile {
	t.Helper()
	catalog, err := modelstore.DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range catalog.Profiles {
		if profile.ID != id {
			continue
		}
		installed := modelstore.InstalledProfile{ID: profile.ID, Identity: strings.Repeat("a", 64), Revision: profile.Revision, Platform: profile.Platform, Selection: profile.Selection, GateStatus: profile.GateStatus}
		for _, component := range profile.Components {
			installed.Components = append(installed.Components, modelstore.InstalledComponent{ID: component.ID, Role: component.Role, SHA256: component.SHA256, Bytes: component.Bytes, Path: "/diagnostic/" + component.Filename})
		}
		return installed
	}
	t.Fatalf("missing fixture profile %q", id)
	return modelstore.InstalledProfile{}
}

func extractionRuntimeFixture() RuntimePaths {
	return RuntimePaths{
		OCR:     RuntimeSpec{Path: "/private/ocr", Bytes: 1, SHA256: strings.Repeat("c", 64)},
		Caption: RuntimeSpec{Path: "/private/caption", Bytes: 1, SHA256: strings.Repeat("d", 64)},
		Speech:  RuntimeSpec{Path: "/private/speech", Bytes: 1, SHA256: strings.Repeat("a", 64)},
		Decoder: RuntimeSpec{Path: "/private/decoder", Bytes: 1, SHA256: strings.Repeat("b", 64)},
	}
}

func TestNewExtractionVersionOwnsExactRoutesAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		kind             archive.JobKind
		profile          string
		speech           SpeechLanguage
		prompt, language string
	}{
		{archive.JobOCR, "tesseract-fast", "", "none-v1", "heb+eng"},
		{archive.JobOCR, "tesseract-best", "", "none-v1", "heb+eng"},
		{archive.JobCaption, "qwen3.5-0.8b-vision", "", captionPromptVersion, "he"},
		{archive.JobTranscription, "ivrit-whisper-large-v3-turbo", SpeechHebrew, "forced-he-v1", "he"},
		{archive.JobTranscription, EnglishSpeechProfile, SpeechEnglish, "forced-en-v1", "en"},
		{archive.JobTranscription, "ivrit-whisper-large-v3-turbo", SpeechMixed, "mixed-forced-he-candidate-v1", "mixed"},
	} {
		t.Run(tc.profile+"/"+tc.language, func(t *testing.T) {
			profile := installedMediaProfile(t, tc.profile)
			runtimes := extractionRuntimeFixture()
			version, err := NewExtractionVersion(tc.kind, profile, runtimes, tc.speech)
			if err != nil {
				t.Fatal(err)
			}
			fingerprint, err := RuntimeFingerprint(tc.kind, runtimes)
			if err != nil {
				t.Fatal(err)
			}
			if version.Kind != tc.kind || version.ProfileID != profile.ID || version.ProfileIdentity != profile.Identity || version.RuntimeSHA256 != fingerprint || version.ExtractorVersion != ExtractorVersion || version.PromptVersion != tc.prompt || version.Language != tc.language || !strings.HasPrefix(version.ID, "media-v1-") {
				t.Fatalf("producer route drift: %+v", version)
			}
			again, err := NewExtractionVersion(tc.kind, profile, runtimes, tc.speech)
			if err != nil || again != version {
				t.Fatal("version is not deterministic")
			}
			e := &Extractor{options: ExtractorOptions{OCRProfile: tc.profile, EnglishProfile: EnglishSpeechProfile, Language: tc.speech, Runtimes: runtimes}}
			if !e.validPrimaryVersion(version) {
				t.Fatal("producer/consumer version disagreement")
			}
			profile.Identity = strings.Repeat("e", 64)
			changed, err := NewExtractionVersion(tc.kind, profile, runtimes, tc.speech)
			if err != nil || changed.ID == version.ID {
				t.Fatal("whole installed identity not bound")
			}
			profile.Identity = version.ProfileIdentity
			profile.Components[0].Path = "/relocated/diagnostic-only"
			runtimes.OCR.Path, runtimes.Caption.Path, runtimes.Speech.Path, runtimes.Decoder.Path = "/relocated/ocr", "/relocated/caption", "/relocated/speech", "/relocated/decoder"
			relocated, err := NewExtractionVersion(tc.kind, profile, runtimes, tc.speech)
			if err != nil || relocated != version {
				t.Fatal("diagnostic paths changed content identity")
			}
		})
	}
}

func TestNewExtractionVersionRejectsIncompleteOrWrongAssets(t *testing.T) {
	for name, mutate := range map[string]func(*modelstore.InstalledProfile){
		"missing identity":       func(p *modelstore.InstalledProfile) { p.Identity = "" },
		"invalid identity":       func(p *modelstore.InstalledProfile) { p.Identity = strings.Repeat("A", 64) },
		"unknown profile":        func(p *modelstore.InstalledProfile) { p.ID = "custom-ocr" },
		"wrong revision":         func(p *modelstore.InstalledProfile) { p.Revision = "other" },
		"wrong platform":         func(p *modelstore.InstalledProfile) { p.Platform = "other" },
		"missing language asset": func(p *modelstore.InstalledProfile) { p.Components = p.Components[:1] },
		"duplicate component":    func(p *modelstore.InstalledProfile) { p.Components[1] = p.Components[0] },
		"wrong role":             func(p *modelstore.InstalledProfile) { p.Components[0].Role = "model" },
		"wrong asset hash":       func(p *modelstore.InstalledProfile) { p.Components[0].SHA256 = strings.Repeat("f", 64) },
		"wrong asset size":       func(p *modelstore.InstalledProfile) { p.Components[0].Bytes++ },
		"extra component":        func(p *modelstore.InstalledProfile) { p.Components = append(p.Components, p.Components[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			profile := installedMediaProfile(t, "tesseract-fast")
			mutate(&profile)
			version, err := NewExtractionVersion(archive.JobOCR, profile, extractionRuntimeFixture(), "")
			if err == nil || version != (archive.ExtractionVersion{}) {
				t.Fatal("invalid installed snapshot accepted")
			}
		})
	}
	for _, language := range []SpeechLanguage{"", "auto", SpeechEnglish} {
		if _, err := NewExtractionVersion(archive.JobTranscription, installedMediaProfile(t, "ivrit-whisper-large-v3-turbo"), extractionRuntimeFixture(), language); err == nil {
			t.Fatalf("incorrect speech route accepted: %q", language)
		}
	}
	if _, err := NewExtractionVersion(archive.JobOCR, installedMediaProfile(t, "tesseract-fast"), extractionRuntimeFixture(), SpeechHebrew); err == nil {
		t.Fatal("irrelevant speech selector accepted for OCR")
	}
	if _, err := NewExtractionVersion(archive.JobTextEmbedding, installedMediaProfile(t, "tesseract-fast"), extractionRuntimeFixture(), ""); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported kind: %v", err)
	}
	profile := installedMediaProfile(t, "ivrit-whisper-large-v3-turbo")
	runtimes := extractionRuntimeFixture()
	first, err := NewExtractionVersion(archive.JobTranscription, profile, runtimes, SpeechHebrew)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "media-v1-aebfa80b3bbc4bdee927b5f787285977a1ed4d4a0724ff872d19a09388eebabb" {
		t.Fatalf("version framing changed: %s", first.ID)
	}
	mixed, err := NewExtractionVersion(archive.JobTranscription, profile, runtimes, SpeechMixed)
	if err != nil || mixed.ID == first.ID {
		t.Fatal("forced Hebrew and mixed candidate version identities collided")
	}
	runtimes.Decoder.SHA256 = strings.Repeat("e", 64)
	changed, err := NewExtractionVersion(archive.JobTranscription, profile, runtimes, SpeechHebrew)
	if err != nil || changed.ID == first.ID {
		t.Fatal("decoder output authority not bound")
	}
	runtimes.Decoder = RuntimeSpec{}
	if _, err := NewExtractionVersion(archive.JobTranscription, profile, runtimes, SpeechHebrew); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing decoder: %v", err)
	}
	caption := installedMediaProfile(t, "qwen3.5-0.8b-vision")
	caption.Components = caption.Components[:1]
	if _, err := NewExtractionVersion(archive.JobCaption, caption, extractionRuntimeFixture(), ""); err == nil {
		t.Fatal("missing vision projector accepted")
	}
}

func TestExtractionRejectsDifferentSourceBeforeWorker(t *testing.T) {
	source := archive.SourceRef{Account: "account", ContentHash: "hash", Version: archive.SourceVersion{MessageID: "message", Revision: 2, PolicyGeneration: 1}}
	original := &Original{metadata: OriginalMetadata{MediaID: "media", Source: source}}
	job := archive.Job{Kind: archive.JobOCR, MediaID: "media", Sources: []archive.SourceRef{source}}
	job.Sources[0].Version.Revision = 1
	e := &Extractor{}
	if _, err := e.Extract(context.Background(), job, original); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("stale source escaped: %v", err)
	}
	job.Sources[0] = source
	job.Kind = archive.JobTextEmbedding
	if _, err := e.Extract(context.Background(), job, original); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported job escaped: %v", err)
	}
	job.Kind = archive.JobOCR
	source.Version.ExpiresAt = time.Now().Add(-time.Second)
	job.Sources[0] = source
	original.metadata.Source = source
	if _, err := e.Extract(context.Background(), job, original); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("expired original started extraction: %v", err)
	}
}

func TestSpeechRoutesAndHonestProvenance(t *testing.T) {
	e := &Extractor{options: ExtractorOptions{Language: SpeechHebrew}}
	id, lang, err := e.speechRoute(0, 30000)
	if err != nil || id != "ivrit-whisper-large-v3-turbo" || lang != "he" {
		t.Fatal("Hebrew not forced")
	}
	e.options.Language = SpeechEnglish
	if _, _, err = e.speechRoute(0, 30000); !errors.Is(err, ErrModelMissing) {
		t.Fatal("English silently uses Hebrew model")
	}
	e.options.EnglishProfile = "whisper-small.en-q5_1"
	id, lang, err = e.speechRoute(0, 30000)
	if err != nil || lang != "en" || id != "whisper-small.en-q5_1" {
		t.Fatal("English route missing")
	}
	e.options.Language = SpeechMixed
	id, lang, err = e.speechRoute(0, 30000)
	if err != nil || lang != "he" || id != "ivrit-whisper-large-v3-turbo" {
		t.Fatal("mixed auto-detect substitution")
	}
	doc := e.document(archive.Job{ID: "job"}, OriginalMetadata{MediaID: "media"}, archive.DocumentTranscript, "שלום $()", strings.Repeat("a", 64), 30000, 60000, "30000")
	if doc.Text != "שלום $()" || doc.Provenance.OffsetKind != "slice" || doc.Provenance.StartMS != 30000 || doc.Provenance.EndMS != 60000 || doc.Provenance.PromptVersion != "mixed-forced-he-candidate-v1" {
		t.Fatalf("dishonest provenance %+v", doc)
	}
}

func TestCleanupRetainsComputeUntilEveryDependencyCloses(t *testing.T) {
	scheduler, _ := compute.NewThreadBudgetScheduler(2)
	guard, err := scheduler.Acquire(context.Background(), compute.HeavyMedia, 2)
	if err != nil {
		t.Fatal(err)
	}
	own := newOwnership()
	own.add(func() error { guard.Release(); return nil })
	pending := make(chan struct{})
	own.pending = pending
	if err := own.finish(); !errors.Is(err, ErrCleanup) {
		t.Fatal(err)
	}
	if scheduler.Snapshot().Heavy != 1 {
		t.Fatal("cleanup released model guard before process tree exit")
	}
	close(pending)
	select {
	case <-own.done:
	case <-time.After(time.Second):
		t.Fatal("cleanup never acknowledged")
	}
	if scheduler.Snapshot().Threads != 0 || scheduler.Snapshot().Heavy != 0 {
		t.Fatal("guard leak")
	}
}

func TestHeartbeatCleanupCancelsBlockedRenewWithoutCancellingTranscript(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	own := newOwnership()
	own.cancel = cancel
	entered := make(chan struct{})
	trackRenewal(ctx, own, time.Millisecond, func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() })
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("renewal not started")
	}
	result := make(chan error, 1)
	go func() { result <- own.finish() }()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup waited on an uncancelled renewal")
	}
	if ctx.Err() != nil {
		t.Fatal("successful slice cleanup cancelled entire transcription")
	}
}

func TestCaptionFramingAndDecoderProtocols(t *testing.T) {
	text, err := captionText([]byte("diagnostic\nWA_MEDIA_CAPTION_BEGIN\nשלום\nWA_MEDIA_CAPTION_END\n"))
	if err != nil || string(text) != "שלום" {
		t.Fatal(err)
	}
	if _, err := captionText([]byte("unframed invented path /tmp/secret")); !errors.Is(err, ErrCorrupt) {
		t.Fatal("unframed response accepted")
	}
	for _, format := range []string{"https://attacker.invalid/x", "concat", "hls", "mp4"} {
		if _, err := decoderArguments(format); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unsupported protocol/demux %s", format)
		}
	}
	args, err := decoderArguments("ogg")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i, arg := range args {
		if arg == "-protocol_whitelist" && i+1 < len(args) && args[i+1] == "pipe" {
			found = true
		}
	}
	if !found {
		t.Fatal("network decoder protocols enabled")
	}
}

func TestExtractionOutcomesDoNotLeakNativeErrors(t *testing.T) {
	for _, entry := range []struct {
		err  error
		want string
	}{{archive.ErrQuotaExceeded, "waiting_for_disk_budget"}, {diskspace.ErrInsufficientSpace, "waiting_for_disk_budget"}, {ErrModelMissing, "model_missing"}, {ErrUnsupported, "unsupported"}, {context.Canceled, "cancelled"}, {context.DeadlineExceeded, "timeout"}, {errors.New("native stdout secret /private/path"), "failed"}} {
		if got := ExtractionCode(entry.err); got != entry.want {
			t.Fatalf("outcome %q want %q", got, entry.want)
		}
	}
}

func TestGeneratedStagingPreservesEveryUTF8ByteAndProvenance(t *testing.T) {
	e := &Extractor{options: ExtractorOptions{Language: SpeechMixed}}
	source := archive.SourceRef{Account: "account", ContentHash: "hash", Version: archive.SourceVersion{MessageID: "message", Revision: 2}}
	meta := OriginalMetadata{MediaID: "media", Source: source}
	job := archive.Job{ID: "job"}
	text := "  " + strings.Repeat("א🙂b\n", 8000) + "  "
	accepted, err := generatedText([]byte(text))
	if err != nil || accepted != text {
		t.Fatalf("generated bytes changed: %v", err)
	}
	docs := e.documents(job, meta, archive.DocumentTranscript, accepted, strings.Repeat("a", 64), 30000, 60000, "30000")
	if len(docs) < 2 {
		t.Fatal("large output was not partitioned")
	}
	var joined strings.Builder
	ids := map[archive.DocumentID]bool{}
	for _, d := range docs {
		if len(d.Text) == 0 || len(d.Text) > 16<<10 || !utf8.ValidString(d.Text) {
			t.Fatal("invalid staging envelope")
		}
		if ids[d.ID] {
			t.Fatal("duplicate partition identity")
		}
		ids[d.ID] = true
		if len(d.Sources) != 1 || d.Sources[0] != source || d.Provenance.StartMS != 30000 || d.Provenance.EndMS != 60000 || d.Provenance.OffsetKind != "slice" || d.Provenance.PromptVersion != "mixed-forced-he-candidate-v1" {
			t.Fatal("partition invented provenance")
		}
		joined.WriteString(d.Text)
	}
	if joined.String() != text {
		t.Fatal("staging dropped or duplicated bytes")
	}
	again := e.documents(job, meta, archive.DocumentTranscript, accepted, strings.Repeat("a", 64), 30000, 60000, "30000")
	for i := range docs {
		if docs[i].ID != again[i].ID {
			t.Fatal("unstable partition identity")
		}
	}
}

func primaryFixture(t *testing.T) (*Extractor, archive.ExtractionWork, *Original) {
	t.Helper()
	runtime := RuntimeSpec{Path: "/private/bundle/ocr", Bytes: 123, SHA256: strings.Repeat("b", 64)}
	e := &Extractor{options: ExtractorOptions{OCRProfile: "tesseract-fast", Language: SpeechHebrew, Runtimes: RuntimePaths{OCR: runtime}}}
	fingerprint, err := RuntimeFingerprint(archive.JobOCR, e.options.Runtimes)
	if err != nil {
		t.Fatal(err)
	}
	v := archive.ExtractionVersion{ID: "primary-ocr", Kind: archive.JobOCR, ProfileID: "tesseract-fast", ProfileIdentity: strings.Repeat("a", 64), RuntimeSHA256: fingerprint, ExtractorVersion: extractorVersion, PromptVersion: "none-v1", Language: "heb+eng"}
	e.options.ExtractionVersions = []archive.ExtractionVersion{v}
	source := archive.SourceRef{Account: "account", ContentHash: strings.Repeat("c", 64), Version: archive.SourceVersion{MessageID: "message", Revision: 2, PolicyGeneration: 1}}
	work := archive.ExtractionWork{ID: "work", Lease: archive.JobLease{JobID: "work", LeaseID: "lease"}, LeaseUntil: time.Now().Add(time.Minute), Version: v, Source: source, MediaID: "media", ArtifactID: "artifact", OriginalSHA256: strings.Repeat("d", 64), ContentHash: strings.Repeat("e", 64)}
	original := &Original{metadata: OriginalMetadata{MediaID: work.MediaID, ArtifactID: work.ArtifactID, SHA256: work.OriginalSHA256, Source: source}}
	return e, work, original
}

func TestPrimaryRequiresExactIndependentVersionAndOriginal(t *testing.T) {
	e, work, original := primaryFixture(t)
	if _, err := e.primaryInput(work, original); err != nil {
		t.Fatal(err)
	}
	changes := []func(*archive.ExtractionWork){
		func(w *archive.ExtractionWork) { w.ArtifactID = "other" },
		func(w *archive.ExtractionWork) { w.OriginalSHA256 = strings.Repeat("f", 64) },
		func(w *archive.ExtractionWork) { w.Source.Version.Revision++ },
		func(w *archive.ExtractionWork) { w.Version.ProfileIdentity = strings.Repeat("f", 64) },
		func(w *archive.ExtractionWork) { w.Version.RuntimeSHA256 = strings.Repeat("f", 64) },
		func(w *archive.ExtractionWork) { w.Version.PromptVersion = "other" },
		func(w *archive.ExtractionWork) { w.Version.Language = "en" },
		func(w *archive.ExtractionWork) { w.Version.ExtractorVersion = "other" },
		func(w *archive.ExtractionWork) { w.Lease.JobID = "other" },
	}
	for i, change := range changes {
		copy := work
		change(&copy)
		if _, err := e.primaryInput(copy, original); err == nil {
			t.Fatalf("mutation %d escaped", i)
		}
	}
}

func TestPrimaryRootIsCompleteAndHasNoEncoderAuthority(t *testing.T) {
	e, work, original := primaryFixture(t)
	input, err := e.primaryInput(work, original)
	if err != nil {
		t.Fatal(err)
	}
	text := "  " + strings.Repeat("אב🙂", 7000) + "  "
	docs := e.resultDocuments(input, original.Metadata(), archive.DocumentOCR, text, work.Version.ProfileIdentity, 0, 0, "")
	if len(docs) != 1 || docs[0].Text != text || docs[0].PassageTokens != 0 || len(docs[0].Spans) != 0 {
		t.Fatal("primary root truncated or gained encoder authority")
	}
	p := docs[0].Provenance
	if p.ProfileID != work.Version.ProfileID || p.ExtractorVersion != work.Version.ExtractorVersion || p.PromptVersion != work.Version.PromptVersion || p.OffsetKind != "none" {
		t.Fatal("root provenance differs from extraction version")
	}
	newer := input
	changed := *input.Version
	changed.RuntimeSHA256 = strings.Repeat("f", 64)
	newer.Version = &changed
	if e.resultDocuments(newer, original.Metadata(), archive.DocumentOCR, text, work.Version.ProfileIdentity, 0, 0, "")[0].ID == docs[0].ID {
		t.Fatal("root identity failed to bind extraction version")
	}
}

func TestRuntimeFingerprintBindsEveryRequiredRole(t *testing.T) {
	r := RuntimePaths{Speech: RuntimeSpec{Path: "/private/speech", Bytes: 1, SHA256: strings.Repeat("a", 64)}, Decoder: RuntimeSpec{Path: "/private/decoder", Bytes: 1, SHA256: strings.Repeat("b", 64)}}
	first, err := RuntimeFingerprint(archive.JobTranscription, r)
	if err != nil {
		t.Fatal(err)
	}
	if first != "d09dd632f5bbbb0676ba350a5ab635162fb44a75ba184e984bf2eaa2d56be4db" {
		t.Fatal("runtime fingerprint framing changed")
	}
	changed := r
	changed.Decoder.SHA256 = strings.Repeat("c", 64)
	second, err := RuntimeFingerprint(archive.JobTranscription, changed)
	if err != nil || first == second {
		t.Fatal("decoder digest not bound")
	}
	changed = r
	changed.Speech, changed.Decoder = changed.Decoder, changed.Speech
	second, err = RuntimeFingerprint(archive.JobTranscription, changed)
	if err != nil || first == second {
		t.Fatal("executable role not bound")
	}
	r.Decoder = RuntimeSpec{}
	if _, err = RuntimeFingerprint(archive.JobTranscription, r); err == nil {
		t.Fatal("missing decoder accepted")
	}
}

func TestPrimaryCannotConvertAnyFailureIntoEmptySuccess(t *testing.T) {
	_, work, _ := primaryFixture(t)
	empty := extractionOutput{outcome: archive.ExtractionEmpty}
	for _, candidate := range []struct {
		kind    archive.JobKind
		outcome archive.ExtractionOutcome
	}{{archive.JobOCR, archive.ExtractionEmpty}, {archive.JobTranscription, archive.ExtractionSilent}} {
		candidateWork := work
		candidateWork.Version.Kind = candidate.kind
		for _, failure := range []error{ErrNoText, ErrSilent, ErrCleanup, ErrUnsupported, ErrCorrupt, context.Canceled, context.DeadlineExceeded, errors.Join(ErrNoText, ErrCleanup), errors.Join(ErrSilent, ErrCleanup)} {
			result, err := primaryResult(candidateWork, extractionOutput{outcome: candidate.outcome}, failure)
			if err == nil || result.Outcome != "" || len(result.Documents) != 0 || result.Lease.JobID != "" {
				t.Fatalf("failure fabricated success: %v", failure)
			}
		}
	}
	result, err := primaryResult(work, empty, nil)
	if err != nil || result.Outcome != archive.ExtractionEmpty || result.Lease != work.Lease || result.ContentHash != work.ContentHash {
		t.Fatal("verified empty lost exact work identity")
	}
	work.Version.Kind = archive.JobTranscription
	if _, err = primaryResult(work, empty, nil); err == nil {
		t.Fatal("image empty accepted for ASR")
	}
	if result, err = primaryResult(work, extractionOutput{outcome: archive.ExtractionSilent}, nil); err != nil || result.Outcome != archive.ExtractionSilent {
		t.Fatal("verified silence not retained")
	}
}

type mismatchedPrimaryModel struct{ opens, guards, closes int }

func (m *mismatchedPrimaryModel) Profile() modelstore.InstalledProfile {
	return modelstore.InstalledProfile{ID: "tesseract-fast", Identity: strings.Repeat("f", 64)}
}
func (m *mismatchedPrimaryModel) Open(string) (*os.File, error) {
	m.opens++
	return nil, ErrModelMissing
}
func (m *mismatchedPrimaryModel) WorkerGuard() (*os.File, error) {
	m.guards++
	return nil, ErrUnavailable
}
func (m *mismatchedPrimaryModel) Close() error { m.closes++; return nil }

func TestPrimaryRejectsActualModelMismatchBeforeNativeAndReleasesOwnership(t *testing.T) {
	repo := newRepository()
	store, _, _ := testStore(t, repo, Options{})
	data := pngData(t)
	request := request(repo, data)
	download, err := store.AdmitDownload(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := download.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err = download.File().Write(data); err != nil {
		t.Fatal(err)
	}
	if err = download.Complete(context.Background(), receipt(request, data)); err != nil {
		t.Fatal(err)
	}
	if err = download.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := store.OpenOriginal(context.Background(), archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: 1}, request.MediaID)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	e, work, _ := primaryFixture(t)
	meta := original.Metadata()
	work.Source = meta.Source
	work.ArtifactID = meta.ArtifactID
	work.MediaID = meta.MediaID
	work.OriginalSHA256 = meta.SHA256
	scheduler, err := compute.NewThreadBudgetScheduler(2)
	if err != nil {
		t.Fatal(err)
	}
	lease := &mismatchedPrimaryModel{}
	e.options.Store = store
	e.options.Scheduler = scheduler
	e.options.Acquire = func(context.Context, string) (MediaModelLease, error) { return lease, nil }
	result, err := e.ExtractPrimary(context.Background(), work, original)
	if !errors.Is(err, ErrModelMissing) || result.Outcome != "" || lease.opens != 0 || lease.guards != 0 || lease.closes != 1 {
		t.Fatalf("actual model mismatch escaped: %v %+v %+v", err, result, lease)
	}
	if scheduler.Snapshot().Threads != 0 {
		t.Fatal("identity rejection leaked compute admission")
	}
	e.options.BeforeHeavy = func(context.Context) error { return ErrNoText }
	result, err = e.ExtractPrimary(context.Background(), work, original)
	if !errors.Is(err, ErrNoText) || result.Outcome != "" || len(result.Documents) != 0 {
		t.Fatal("pre-native failure became no_text success")
	}
	closing, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err = e.Close(closing); err != nil {
		t.Fatal(err)
	}
	if err = original.Close(); err != nil {
		t.Fatal("rejected extraction kept original active")
	}
}

func TestCaptionCanvasVersionInvalidatesOnlyCaptionPolicy(t *testing.T) {
	runtimes := extractionRuntimeFixture()
	caption, err := NewExtractionVersion(archive.JobCaption, installedMediaProfile(t, "qwen3.5-0.8b-vision"), runtimes, "")
	if err != nil {
		t.Fatal(err)
	}
	if caption.PromptVersion != "he-grounded-canvas-v2" || caption.ExtractorVersion != "native-media-v1" || caption.ID == "media-v1-3a3979e27db22edebdd47355f81d9c998f4321fefc891ccfd78138b15ee52d0d" {
		t.Fatal("caption preprocessing/template policy not versioned")
	}
	extractor := &Extractor{options: ExtractorOptions{Runtimes: runtimes, OCRProfile: "tesseract-fast", Language: SpeechHebrew}}
	if !extractor.validPrimaryVersion(caption) {
		t.Fatal("new caption policy rejected")
	}
	old := caption
	old.ID, old.PromptVersion = "media-v1-3a3979e27db22edebdd47355f81d9c998f4321fefc891ccfd78138b15ee52d0d", "he-grounded-v1"
	if extractor.validPrimaryVersion(old) {
		t.Fatal("old caption work accepted under new preprocessing")
	}
	ocr, err := NewExtractionVersion(archive.JobOCR, installedMediaProfile(t, "tesseract-fast"), runtimes, "")
	if err != nil || ocr.ID != "media-v1-cf1b9db7e6449a06a418e1a59f538ee7109c408c632a3289df6a54970c5ede71" || !extractor.validPrimaryVersion(ocr) {
		t.Fatal("OCR identity changed with caption policy")
	}
	speech, err := NewExtractionVersion(archive.JobTranscription, installedMediaProfile(t, "ivrit-whisper-large-v3-turbo"), runtimes, SpeechHebrew)
	if err != nil || speech.ID != "media-v1-aebfa80b3bbc4bdee927b5f787285977a1ed4d4a0724ff872d19a09388eebabb" || !extractor.validPrimaryVersion(speech) {
		t.Fatal("speech identity changed with caption policy")
	}
}
