package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/compute"
	"github.com/Bodokh/wa-indexer/internal/indexing"
	"github.com/Bodokh/wa-indexer/internal/localui"
	"github.com/Bodokh/wa-indexer/internal/media"
	"github.com/Bodokh/wa-indexer/internal/retrieval"
)

type localAIOwner struct {
	mu             sync.Mutex
	config         Config
	state, code    string
	cancel         context.CancelFunc
	done           chan struct{}
	wake           chan struct{}
	failedRevision uint64
}

func (a *Application) LocalAIState(ctx context.Context) (localui.LocalAISnapshot, error) {
	a.data.RLock()
	defer a.data.RUnlock()
	if err := ctx.Err(); err != nil {
		return localui.LocalAISnapshot{}, err
	}
	if a.closed {
		return localui.LocalAISnapshot{}, localui.ErrLocalAIUnavailable
	}
	record := a.authState.LocalAI
	if record.Revision == 0 {
		record = defaultLocalAIRecord()
	}
	snapshot := localui.LocalAISnapshot{Revision: record.Revision, Choice: record.Choice, State: "off", Code: "local_ai_off", Options: a.localAIOptions()}
	if a.localAI != nil {
		a.localAI.mu.Lock()
		snapshot.State, snapshot.Code = a.localAI.state, a.localAI.code
		a.localAI.mu.Unlock()
	} else if record.Choice.Mode != "off" {
		snapshot.State = "unavailable"
		snapshot.Code = "local_ai_bundle_unavailable"
	}
	return snapshot, nil
}

func (a *Application) SetLocalAI(ctx context.Context, update localui.LocalAIUpdate) (localui.LocalAISnapshot, error) {
	if !validLocalAIChoice(update.Choice) || !knownLocalAIChoice(update.Choice) || update.ExpectedRevision == 0 {
		return localui.LocalAISnapshot{}, localui.ErrLocalAIInvalid
	}
	a.ops.Lock()
	if err := ctx.Err(); err != nil {
		a.ops.Unlock()
		return localui.LocalAISnapshot{}, err
	}
	a.data.Lock()
	if a.closed || a.localAI == nil {
		a.data.Unlock()
		a.ops.Unlock()
		return localui.LocalAISnapshot{}, localui.ErrLocalAIUnavailable
	}
	record := a.authState.LocalAI
	if record.Revision == 0 {
		record = defaultLocalAIRecord()
	}
	if record.Revision != update.ExpectedRevision || record.Revision == math.MaxUint64 {
		a.data.Unlock()
		a.ops.Unlock()
		return localui.LocalAISnapshot{}, localui.ErrLocalAIConflict
	}
	if update.Choice.Mode != "off" && (a.localAI.config.Offline || !a.eligibleLocalAIChoice(update.Choice)) {
		a.data.Unlock()
		a.ops.Unlock()
		return localui.LocalAISnapshot{}, localui.ErrLocalAIUnavailable
	}
	old := a.authState.LocalAI
	a.authState.LocalAI = localAIRecord{Version: 1, Revision: record.Revision + 1, Choice: update.Choice}
	if err := a.persistPaused(a.paused); err != nil {
		a.authState.LocalAI = old
		a.data.Unlock()
		a.ops.Unlock()
		return localui.LocalAISnapshot{}, localui.ErrLocalAIUnavailable
	}
	a.queryCancel()
	service, err := retrieval.NewService(retrieval.Config{Reader: a.store, Candidates: a.store, Index: a.store.ActiveIndex})
	if err == nil {
		a.resources.retrieval.set(service)
	}
	a.replaceQueryContext()
	a.localAI.set("applying", "local_ai_applying")
	if a.localAI.config.Offline {
		a.localAI.set("off", "local_ai_off")
	}
	select {
	case a.localAI.wake <- struct{}{}:
	default:
	}
	a.data.Unlock()
	a.ops.Unlock()
	return a.LocalAIState(ctx)
}

func (a *Application) replaceQueryContext() {
	a.queryContext, a.queryCancel = context.WithCancel(context.Background())
}
func (o *localAIOwner) set(state, code string) {
	o.mu.Lock()
	o.state, o.code = state, code
	o.mu.Unlock()
}

func (a *Application) startLocalAI(cfg Config) {
	// Explicit internal experimental fixtures retain their existing lifecycle;
	// they cannot become owner-GUI preview eligibility without bundle evidence.
	if cfg.Native != nil && cfg.Native.Experimental && cfg.Native.bundle == nil || cfg.Media != nil && cfg.Media.Experimental && cfg.Media.bundle == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	owner := &localAIOwner{config: copyApplicationConfig(cfg), cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), state: "off", code: "local_ai_off"}
	a.localAI = owner
	if cfg.Offline {
		if a.authState.LocalAI.Choice.Mode != "" && a.authState.LocalAI.Choice.Mode != "off" {
			owner.set("unavailable", "local_ai_unavailable")
		}
		close(owner.done)
		return
	}
	go func() {
		defer close(owner.done)
		defer cancel()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var applied uint64
		retry := true
		for {
			if ctx.Err() != nil {
				return
			}
			a.data.RLock()
			revision := a.authState.LocalAI.Revision
			if revision == 0 {
				revision = 1
			}
			a.data.RUnlock()
			if retry || revision != applied {
				applied, retry = a.applyLocalAI(ctx, owner)
			}
			select {
			case <-ctx.Done():
				return
			case <-owner.wake:
			case <-ticker.C:
			}
		}
	}()
}

func (a *Application) stopLocalAI(ctx context.Context) error {
	a.data.RLock()
	owner := a.localAI
	a.data.RUnlock()
	if owner == nil {
		return nil
	}
	owner.cancel()
	select {
	case <-owner.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Application) applyLocalAI(ctx context.Context, owner *localAIOwner) (uint64, bool) {
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	record := a.authState.LocalAI
	if record.Revision == 0 {
		record = defaultLocalAIRecord()
	}
	if a.closed || ctx.Err() != nil {
		a.data.Unlock()
		return record.Revision, false
	}
	r := a.resources
	service, err := retrieval.NewService(retrieval.Config{Reader: a.store, Candidates: a.store, Index: a.store.ActiveIndex})
	if err != nil {
		a.data.Unlock()
		owner.set("unavailable", "local_ai_operation_failed")
		return record.Revision, false
	}
	r.retrieval.set(service)
	a.data.Unlock()
	owner.set("applying", "local_ai_applying")
	if err := r.drainAI(ctx); err != nil {
		owner.set("cleanup_pending", "local_ai_cleanup_pending")
		return record.Revision, true
	}
	a.data.Lock()
	r.finishAIDrain()
	r.nativeConfigured = false
	r.mediaConfigured = false
	a.data.Unlock()
	if record.Choice.Mode == "off" {
		owner.set("off", "local_ai_off")
		return record.Revision, false
	}
	if owner.failedRevision == record.Revision {
		owner.set("unavailable", "local_ai_operation_failed")
		return record.Revision, false
	}
	if r.recoveryPending.Load() {
		owner.set("unavailable", "local_ai_model_missing")
		return record.Revision, true
	}
	cfg, code := a.selectedLocalAIConfig(ctx, owner.config, record.Choice)
	if code != "" {
		owner.set("unavailable", code)
		return record.Revision, false
	}
	if cfg.Native != nil {
		if err := r.prepareBundledWorker(ctx, cfg.Native); err != nil {
			owner.failedRevision = record.Revision
			if r.workerExecutable != nil {
				owner.set("cleanup_pending", "local_ai_cleanup_pending")
				return record.Revision, true
			}
			owner.set("unavailable", "local_ai_operation_failed")
			return record.Revision, false
		}
	}
	a.data.Lock()
	err = r.initializeRuntime(ctx, a.store, cfg)
	if err == nil && cfg.Native != nil && r.native == nil {
		err = localui.ErrLocalAIUnavailable
	}
	if err == nil {
		if cfg.Native != nil {
			cfg.Native.IndexVersion = localAIIndexVersion(r.native)
		}
		r.nativeConfigured = cfg.Native != nil
		r.mediaConfigured = cfg.Media != nil
		r.startBackground(a.store, cfg)
	}
	a.data.Unlock()
	if err != nil {
		owner.failedRevision = record.Revision
		owner.set("cleanup_pending", "local_ai_cleanup_pending")
		return record.Revision, true
	}
	owner.set("active", "local_ai_active")
	return record.Revision, false
}

func localAIIndexVersion(native nativeOwner) *archive.IndexVersion {
	identity := native.TokenizerIdentity()
	version := archive.IndexVersion{EncoderProfile: identity.ProfileID, TokenizerSHA256: identity.TokenizerSHA256, TokenizerImplementation: identity.Implementation, PrefixVersion: identity.PrefixVersion, ChunkingVersion: indexing.ChunkingVersion, NormalizationVersion: archive.LexicalNormalizationVersion, Dimensions: 384, MaxPassageTokens: 256}
	encoded, _ := json.Marshal(version)
	digest := sha256.Sum256(encoded)
	version.ID = "local-ai-index-v1-" + hex.EncodeToString(digest[:])
	return &version
}

func knownLocalAIChoice(choice localui.LocalAIChoice) bool {
	if choice.Mode == "off" {
		return true
	}
	if choice.EncoderProfile != "" && !knownAIProfile(aiProfileMapping{Kind: "encoder", ProfileID: choice.EncoderProfile, ModelProfileID: encoderModelProfile(choice.EncoderProfile)}) {
		return false
	}
	return choice.OCRProfile == "" || choice.OCRProfile == "tesseract-fast" || choice.OCRProfile == "tesseract-best"
}
func encoderModelProfile(profile string) string {
	if profile == "darwin-arm64-fp32-bounded-experimental" {
		return "darwin-arm64-fp32-feasibility"
	}
	return profile
}
func speechModelProfile(language string) string {
	if language == "en" {
		return media.EnglishSpeechProfile
	}
	return "ivrit-whisper-large-v3-turbo"
}

func (a *Application) localAIBundle(kind string) *bundledAI {
	if a.localAI == nil {
		return nil
	}
	if kind == "encoder" {
		if a.localAI.config.Native != nil {
			return a.localAI.config.Native.bundle
		}
		return nil
	}
	if a.localAI.config.Media != nil {
		return a.localAI.config.Media.bundle
	}
	return nil
}
func (a *Application) localAIMapping(kind, profile, language string) (*bundledAI, aiProfileMapping, bool) {
	bundle := a.localAIBundle(kind)
	if bundle == nil {
		return nil, aiProfileMapping{}, false
	}
	bundle.assets.mu.Lock()
	alive := bundle.assets.root != nil
	bundle.assets.mu.Unlock()
	mapping, exists := bundle.entries[aiMappingKey(kind, profile, language)]
	return bundle, mapping, alive && exists
}
func (a *Application) eligibleLocalAIChoice(choice localui.LocalAIChoice) bool {
	if choice.Mode != "preview" {
		return false
	} // compiled accepted allowlist is empty
	for _, item := range choiceComponents(choice) {
		if _, _, ok := a.localAIMapping(item.kind, item.profile, item.language); !ok {
			return false
		}
	}
	return true
}

type aiChoiceComponent struct{ kind, profile, language string }

func choiceComponents(choice localui.LocalAIChoice) []aiChoiceComponent {
	items := []aiChoiceComponent{}
	if choice.EncoderProfile != "" {
		items = append(items, aiChoiceComponent{"encoder", choice.EncoderProfile, ""})
	}
	if choice.OCRProfile != "" {
		items = append(items, aiChoiceComponent{"ocr", choice.OCRProfile, ""})
	}
	if choice.Caption {
		items = append(items, aiChoiceComponent{"caption", "qwen3.5-0.8b-vision", ""})
	}
	if choice.SpeechLanguage != "" {
		items = append(items, aiChoiceComponent{"speech", speechModelProfile(choice.SpeechLanguage), choice.SpeechLanguage})
	}
	return items
}

func (a *Application) localAIOptions() []localui.LocalAIOption {
	items := []aiChoiceComponent{{"encoder", "darwin-arm64-fp32-feasibility", ""}, {"encoder", "darwin-arm64-fp32-bounded-experimental", ""}, {"encoder", "darwin-arm64-dynamic-int8-feasibility", ""}, {"ocr", "tesseract-fast", ""}, {"ocr", "tesseract-best", ""}, {"caption", "qwen3.5-0.8b-vision", ""}, {"speech", "ivrit-whisper-large-v3-turbo", "he"}, {"speech", media.EnglishSpeechProfile, "en"}, {"speech", "ivrit-whisper-large-v3-turbo", "mixed"}}
	options := make([]localui.LocalAIOption, 0, len(items))
	for _, item := range items {
		selector := item.profile
		if item.kind == "speech" {
			selector = item.language
		}
		model := item.profile
		if item.kind == "encoder" {
			model = encoderModelProfile(item.profile)
		}
		option := localui.LocalAIOption{Kind: item.kind, ProfileID: selector, ModelProfileID: model, State: "unavailable", Code: "local_ai_bundle_unavailable"}
		if _, _, eligible := a.localAIMapping(item.kind, item.profile, item.language); eligible {
			option.State = "preview"
			option.Code = ""
		}
		options = append(options, option)
	}
	return options
}

func (a *Application) selectedLocalAIConfig(ctx context.Context, base Config, choice localui.LocalAIChoice) (Config, string) {
	cfg := copyApplicationConfig(base)
	cfg.Native = nil
	cfg.Media = nil
	if !a.eligibleLocalAIChoice(choice) {
		return cfg, "local_ai_bundle_unavailable"
	}
	if a.resources.models == nil {
		return cfg, "local_ai_model_missing"
	}
	for _, item := range choiceComponents(choice) {
		bundle, mapping, _ := a.localAIMapping(item.kind, item.profile, item.language)
		installed, err := a.resources.models.Verify(ctx, mapping.ModelProfileID)
		if err != nil {
			return cfg, "local_ai_model_missing"
		}
		if item.kind == "encoder" {
			cfg.Native = &NativeConfig{Profile: bundle.profiles[aiMappingKey(item.kind, item.profile, item.language)], ModelProfileID: mapping.ModelProfileID, Experimental: true, bundle: bundle, RuntimeLease: bundle.runtimeLease(mapping.RuntimeArtifacts["ort"]), WorkerLifetime: bundle.workerLifetime()}
			continue
		}
		if cfg.Media == nil {
			cfg.Media = &MediaConfig{Experimental: true, Residency: compute.SerialModels, bundle: bundle, WorkerLifetime: bundle.workerLifetime()}
		}
		kind := archive.JobOCR
		switch item.kind {
		case "ocr":
			cfg.Media.OCRProfile = item.profile
			cfg.Media.Runtimes.OCR = bundle.runtimeSpec(mapping.RuntimeArtifacts["ocr"])
		case "caption":
			kind = archive.JobCaption
			cfg.Media.Runtimes.Caption = bundle.runtimeSpec(mapping.RuntimeArtifacts["caption"])
		case "speech":
			kind = archive.JobTranscription
			cfg.Media.Language = media.SpeechLanguage(item.language)
			cfg.Media.Runtimes.Speech = bundle.runtimeSpec(mapping.RuntimeArtifacts["speech"])
			cfg.Media.Runtimes.Decoder = bundle.runtimeSpec(mapping.RuntimeArtifacts["decoder"])
		}
		language := media.SpeechLanguage("")
		if item.kind == "speech" {
			language = cfg.Media.Language
		}
		version, err := media.NewExtractionVersion(kind, installed, cfg.Media.Runtimes, language)
		if err != nil {
			return cfg, "local_ai_profile_unqualified"
		}
		cfg.Media.ExtractionVersions = append(cfg.Media.ExtractionVersions, version)
	}
	return cfg, ""
}
