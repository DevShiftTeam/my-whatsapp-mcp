package modelstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuiltinCatalogMatchesPackagingBytes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("../..", "packaging/model-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, []byte(builtinCatalogJSON)) {
		t.Fatal("embedded catalog differs from packaging/model-catalog.json")
	}
	embedded, err := os.ReadFile("default_catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, embedded) {
		t.Fatal("default_catalog.json differs from packaging/model-catalog.json")
	}
	if _, err := DefaultCatalog(); err != nil {
		t.Fatalf("trusted catalog is invalid: %v", err)
	}
}

func TestBuiltinCatalogPreservesUnacceptedCandidateState(t *testing.T) {
	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	hosts := []string{"raw.githubusercontent.com", "huggingface.co", "cas-bridge.xethub.hf.co", "cdn-lfs.huggingface.co", "cdn-lfs-us-1.huggingface.co", "us.aws.cdn.hf.co", "us.gcp.cdn.hf.co"}
	if !reflect.DeepEqual(catalog.AllowedHosts, hosts) {
		t.Fatalf("allowed hosts changed: %#v", catalog.AllowedHosts)
	}
	want := map[string]struct {
		selection, gate, platform string
		components                int
	}{
		"darwin-arm64-fp32-feasibility":         {"failed", "memory_gate_failed", "darwin-arm64", 2},
		"darwin-arm64-dynamic-int8-feasibility": {"failed", "numerical_reference_failed", "darwin-arm64", 2},
		"qwen3.5-0.8b-vision":                   {"candidate", "NOT_RUN", "any", 2},
		"ivrit-whisper-large-v3-turbo":          {"candidate", "NOT_RUN", "any", 1},
		"whisper-cpp-small-en-q5_1-v1":          {"candidate", "NOT_RUN", "any", 1},
		"tesseract-fast":                        {"candidate", "NOT_RUN", "any", 2},
		"tesseract-best":                        {"candidate", "NOT_RUN", "any", 2},
	}
	if len(catalog.Profiles) != len(want) {
		t.Fatalf("unexpected profile count: %d", len(catalog.Profiles))
	}
	for _, p := range catalog.Profiles {
		expected, ok := want[p.ID]
		if !ok {
			t.Fatalf("unexpected profile: %s", p.ID)
		}
		if p.Selection != expected.selection || p.GateStatus != expected.gate || p.Platform != expected.platform || len(p.Components) != expected.components {
			t.Errorf("%s candidate state changed: %+v", p.ID, p)
		}
		if len(p.RuntimeRequirements) == 0 {
			t.Errorf("%s omits external runtime requirements", p.ID)
		}
		for _, component := range p.Components {
			if strings.HasSuffix(component.Filename, ".tgz") || strings.HasSuffix(component.Filename, ".tar.gz") || strings.Contains(component.URL, "github.com/") {
				t.Errorf("%s includes an unqualified runtime archive: %+v", p.ID, component)
			}
		}
	}
}

func TestBuiltinCatalogFrozenManifestHashes(t *testing.T) {
	want := map[string]string{
		"packaging/embedding-manifest.json":            "4523985f9b58b813b9337875e8bc4626a94c6b473b79d13d4209a42f441c9410",
		"packaging/embedding-manifest-arm64-int8.json": "a109191ae0bdbe406671efef5a491074f17e3f82594623784c539eb591d1e136",
		"packaging/media-manifest.json":                "d39a2ebce1f5dfa1b83392f168089a00264595228c9aee5ce32f8079e0b7cbea",
		"packaging/media-native-manifest.json":         "cca734091bfb996a5275c9e12f2e4ced749844fb28f82108baf1f2345be3770e",
		"packaging/media-english-manifest.json":        "471636a95258b3e33aa15c60246f5fba0a0514b5a474b70f8d98d0f367fd9b4d",
	}
	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range catalog.Profiles {
		expected, ok := want[p.Manifest]
		if !ok || p.ManifestSHA256 != expected {
			t.Fatalf("%s manifest identity changed: %s %s", p.ID, p.Manifest, p.ManifestSHA256)
		}
		raw, err := os.ReadFile(filepath.Join("../..", p.Manifest))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(raw)
		if hex.EncodeToString(hash[:]) != expected {
			t.Errorf("%s frozen source manifest changed", p.Manifest)
		}
		if _, err := os.Stat(filepath.Join("../..", p.Notices)); err != nil {
			t.Errorf("%s notice reference is missing: %v", p.ID, err)
		}
	}
}

func TestBuiltinCatalogEnglishSpeechCandidate(t *testing.T) {
	const profileID = "whisper-cpp-small-en-q5_1-v1"
	const revision = "c521a4b02f422512d734391fdf08bb08c0862f68"
	const filename = "ggml-small.en-q5_1.bin"
	const url = "https://huggingface.co/ggerganov/whisper.cpp/resolve/" + revision + "/" + filename
	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var profile Profile
	for _, p := range catalog.Profiles {
		if p.ID == profileID {
			profile = p
		}
		if strings.Contains(p.ID, "whisper-cpp-") && p.ID != profileID {
			t.Errorf("unexpected additional English checkpoint: %s", p.ID)
		}
	}
	if profile.ID != profileID || profile.Revision != revision || profile.Selection != "candidate" || profile.GateStatus != "NOT_RUN" || profile.License != "MIT" || len(profile.Components) != 1 {
		t.Fatalf("English candidate identity/state changed: %+v", profile)
	}
	if profile.Manifest != "packaging/media-english-manifest.json" || profile.Notices != profile.Manifest {
		t.Fatal("English candidate must reference its separate provenance manifest")
	}
	source := sourceCatalogArtifact{URL: url, SHA256: "bfdff4894dcb76bbf647d56263ea2a96645423f1669176f4844a1bf8e478ad30", Bytes: 190098681, License: "MIT"}
	assertCatalogComponent(t, profile, "model", "model", filename, source, url)
	var manifest struct {
		Profile       string `json:"profile"`
		Selection     string `json:"selection"`
		QualityStatus string `json:"quality_status"`
		NoticeStatus  string `json:"notice_status"`
		Speech        struct {
			sourceCatalogArtifact
			Language       string `json:"language"`
			LanguagePolicy string `json:"language_policy"`
			Translate      bool   `json:"translate"`
			LocalStatus    string `json:"local_status"`
			Metadata       struct {
				Status                    string `json:"status"`
				PayloadDownloaded         bool   `json:"payload_downloaded"`
				PayloadHashLocallyChecked bool   `json:"payload_hash_locally_verified"`
			} `json:"metadata_verification"`
		} `json:"speech"`
	}
	readCatalogSource(t, profile.Manifest, &manifest)
	if manifest.Profile != profileID || manifest.Selection != "candidate" || manifest.QualityStatus != "NOT_RUN" || manifest.NoticeStatus != "UPSTREAM_NOTICE_REFERENCES_ONLY_COMPLETE_DISTRIBUTION_AUDIT_NOT_RUN" {
		t.Fatal("English manifest overstates candidate or distribution acceptance")
	}
	if manifest.Speech.Revision != revision || manifest.Speech.Path != filename || manifest.Speech.Language != "en" || manifest.Speech.LanguagePolicy != "forced_en" || manifest.Speech.Translate {
		t.Fatal("English manifest changed its pinned checkpoint or forced English policy")
	}
	if manifest.Speech.LocalStatus != "NOT_DOWNLOADED" || manifest.Speech.Metadata.Status != "PUBLISHED_GIT_LFS_IDENTITY_ONLY" || manifest.Speech.Metadata.PayloadDownloaded || manifest.Speech.Metadata.PayloadHashLocallyChecked {
		t.Fatal("published metadata must not claim locally verified model execution or payload")
	}
	assertCatalogComponent(t, profile, "model", "model", manifest.Speech.Path, manifest.Speech.sourceCatalogArtifact, manifest.Speech.URL)
}

type sourceCatalogArtifact struct {
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	Bytes    int64  `json:"bytes"`
	License  string `json:"license"`
	Path     string `json:"path"`
	Variant  string `json:"variant"`
	Revision string `json:"revision"`
}

func TestBuiltinCatalogMatchesFrozenArtifactIdentities(t *testing.T) {
	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	profiles := make(map[string]Profile, len(catalog.Profiles))
	for _, p := range catalog.Profiles {
		profiles[p.ID] = p
	}
	for _, path := range []string{"packaging/embedding-manifest.json", "packaging/embedding-manifest-arm64-int8.json"} {
		var source struct {
			Profile       string                           `json:"profile"`
			ModelRevision string                           `json:"model_revision"`
			Artifacts     map[string]sourceCatalogArtifact `json:"artifacts"`
		}
		readCatalogSource(t, path, &source)
		p := profiles[source.Profile]
		if p.Revision != source.ModelRevision || p.License != "MIT" || p.Notices != "packaging/embedding-notices/README.md" {
			t.Errorf("%s embedding identity changed", source.Profile)
		}
		assertCatalogComponent(t, p, "model", "model", "model.onnx", source.Artifacts["model"], source.Artifacts["model"].URL)
		assertCatalogComponent(t, p, "tokenizer", "tokenizer", "tokenizer.json", source.Artifacts["tokenizer"], source.Artifacts["tokenizer"].URL)
	}
	var media struct {
		OCRWeights []sourceCatalogArtifact `json:"ocr_weights"`
		VisionPair struct {
			Conversion string                `json:"conversion"`
			Revision   string                `json:"revision"`
			License    string                `json:"license"`
			Model      sourceCatalogArtifact `json:"model"`
			Projector  sourceCatalogArtifact `json:"projector"`
		} `json:"vision_pair"`
		Speech struct {
			sourceCatalogArtifact
			Publisher string `json:"publisher"`
		} `json:"speech"`
	}
	readCatalogSource(t, "packaging/media-manifest.json", &media)
	vision := profiles["qwen3.5-0.8b-vision"]
	if vision.Revision != media.VisionPair.Revision || vision.License != media.VisionPair.License {
		t.Error("vision pair identity changed")
	}
	visionBase := "https://huggingface.co/" + media.VisionPair.Conversion + "/resolve/" + media.VisionPair.Revision + "/"
	for id, source := range map[string]sourceCatalogArtifact{"model": media.VisionPair.Model, "projector": media.VisionPair.Projector} {
		source.License = media.VisionPair.License
		assertCatalogComponent(t, vision, id, id, source.Path, source, visionBase+source.Path)
	}
	speech := profiles["ivrit-whisper-large-v3-turbo"]
	if speech.Revision != media.Speech.Revision || speech.License != media.Speech.License {
		t.Error("speech identity changed")
	}
	assertCatalogComponent(t, speech, "model", "model", media.Speech.Path, media.Speech.sourceCatalogArtifact, "https://huggingface.co/"+media.Speech.Publisher+"/resolve/"+media.Speech.Revision+"/"+media.Speech.Path)
	for _, source := range media.OCRWeights {
		p := profiles["tesseract-"+source.Variant]
		if p.Revision != source.Revision || p.License != source.License {
			t.Errorf("%s OCR identity changed", p.ID)
		}
		url := "https://raw.githubusercontent.com/tesseract-ocr/tessdata_" + source.Variant + "/" + source.Revision + "/" + source.Path
		assertCatalogComponent(t, p, strings.TrimSuffix(source.Path, ".traineddata"), "ocr_language", source.Path, source, url)
	}
}

func readCatalogSource(t *testing.T, path string, target any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("../..", path))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}

func assertCatalogComponent(t *testing.T, profile Profile, id, role, filename string, source sourceCatalogArtifact, url string) {
	t.Helper()
	want := Component{ID: id, Role: role, Filename: filename, URL: url, SHA256: source.SHA256, Bytes: source.Bytes, License: source.License}
	for _, component := range profile.Components {
		if component.ID == id {
			if !reflect.DeepEqual(component, want) {
				t.Errorf("%s/%s artifact identity changed: got %+v, want %+v", profile.ID, id, component, want)
			}
			return
		}
	}
	t.Errorf("%s component %s is missing", profile.ID, id)
}

// This checks runtime provenance only. Candidate and quality gates stay unaccepted.
func TestBuiltinCatalogVisionRuntimeMatchesNativeManifest(t *testing.T) {
	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var profile Profile
	for _, candidate := range catalog.Profiles {
		if candidate.ID == "qwen3.5-0.8b-vision" {
			profile = candidate
		}
	}
	if profile.ID == "" || profile.Selection != "candidate" || profile.GateStatus != "NOT_RUN" || profile.Manifest != "packaging/media-native-manifest.json" {
		t.Fatal("vision provenance must remain an unaccepted candidate bound to the native manifest")
	}
	var native struct {
		CaptionPatch       string            `json:"caption_patch"`
		CaptionPatchSHA256 string            `json:"caption_patch_sha256"`
		RuntimePins        map[string]string `json:"runtime_pins"`
		CaptionPolicy      struct {
			Version string `json:"version"`
			Quality string `json:"quality"`
		} `json:"caption_policy"`
	}
	readCatalogSource(t, profile.Manifest, &native)
	if native.CaptionPatch != "packaging/media-worker-patches/llama-cpu-caption-v2.patch" || native.CaptionPolicy.Version != "he-grounded-canvas-v2" || !strings.HasPrefix(native.CaptionPolicy.Quality, "NOT_RUN;") {
		t.Fatal("caption runtime policy or its unaccepted quality state changed")
	}
	patch, err := os.ReadFile(filepath.Join("../..", native.CaptionPatch))
	if err != nil {
		t.Fatal(err)
	}
	patchHash := sha256.Sum256(patch)
	if hex.EncodeToString(patchHash[:]) != native.CaptionPatchSHA256 {
		t.Fatal("caption patch is not the exact native manifest artifact")
	}
	wantRuntime := []string{
		"llama.cpp revision=" + native.RuntimePins["llama.cpp"] + "; CPU vision worker",
		"llama CPU caption patch sha256=" + native.CaptionPatchSHA256,
	}
	if native.RuntimePins["llama.cpp"] != "4f5406761517648c23dbd60ea5ade37f77a316c9" || !reflect.DeepEqual(profile.RuntimeRequirements, wantRuntime) {
		t.Fatal("vision catalog runtime does not match native source and patch provenance")
	}
	legacy := profile
	legacy.Manifest = "packaging/media-manifest.json"
	legacy.ManifestSHA256 = "d39a2ebce1f5dfa1b83392f168089a00264595228c9aee5ce32f8079e0b7cbea"
	legacy.RuntimeRequirements = []string{wantRuntime[0], "llama CPU caption patch sha256=a785f4029aba430ba36626489db49339fbcb8eb5c2451f99db0aaa2af35e2403"}
	if legacy.identity() == profile.identity() {
		t.Fatal("old installed profile identity must not silently acquire the new caption runtime provenance")
	}
}

// Full-profile goldens keep historical speech/OCR identities and old receipts distinct.
func TestBuiltinCatalogImageRebasePreservesProfileIdentities(t *testing.T) {
	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"darwin-arm64-fp32-feasibility":         "beab7573806c73d30193aac877db5d1b7ac1a9b10622df73a356214c9a46fe22",
		"darwin-arm64-dynamic-int8-feasibility": "645f26a38862a9cf7d04264dc512745742c30b521da70ed9b5d53c28c31fd7da",
		"qwen3.5-0.8b-vision":                   "ec1bcc0126f10346be5ea2291625aac31b09b8eb10f962089fe72d95d09125eb",
		"ivrit-whisper-large-v3-turbo":          "e873b9e31d61b303d2f9a6ddab0b37351b9971b22bd8e14c5d67e41ca7c4083c",
		"tesseract-fast":                        "2c43e3d94923fa019f68bfa46db298b05a4d3ad116dc25643391703497b9ff7e",
		"tesseract-best":                        "690c85ccb61b33e303e036d2abb58646992f165e8db1d3981cdcc86c197fb114",
		"whisper-cpp-small-en-q5_1-v1":          "e812a8fbf2fa897e2d95504d24a9c33aa1f14a2b39a8c60461e89f02b70658ec",
	}
	if len(catalog.Profiles) != len(want) {
		t.Fatal("catalog image provenance rebase changed the profile set")
	}
	for _, profile := range catalog.Profiles {
		if profile.identity() != want[profile.ID] {
			t.Fatalf("%s full-profile identity changed beyond the reviewed image provenance delta", profile.ID)
		}
		if profile.ID == "qwen3.5-0.8b-vision" {
			legacy := profile
			legacy.Manifest = "packaging/media-manifest.json"
			legacy.ManifestSHA256 = "d39a2ebce1f5dfa1b83392f168089a00264595228c9aee5ce32f8079e0b7cbea"
			legacy.RuntimeRequirements = []string{profile.RuntimeRequirements[0], "llama CPU caption patch sha256=a785f4029aba430ba36626489db49339fbcb8eb5c2451f99db0aaa2af35e2403"}
			if legacy.identity() != "11184fd65c28a334b0dab94d031fb510ad7183a53636984f76913d1112df51c8" || legacy.identity() == profile.identity() {
				t.Fatal("paired component records or old whole-profile identity were relabeled")
			}
		}
	}
}
