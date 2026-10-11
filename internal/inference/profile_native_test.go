//go:build native_onnx

package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/workerlife"
	"github.com/daulet/tokenizers"
	ort "github.com/yalue/onnxruntime_go"
	"golang.org/x/sys/unix"
)

const profileFixtureSHA256 = "f3538d0d5393ed59ac7ad9b14e2b02ac3177d7b002c2d1d24a3b8fbcdf6a8c33"

// profileFixtureLease is an explicit developer fixture lease, never a model
// store or release acquisition path. Each Open returns a readonly owned file.
type profileFixtureLease struct {
	mu     sync.Mutex
	paths  ComponentPaths
	closed bool
	guard  *os.File
}

func (l *profileFixtureLease) Open(name string) (*os.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrClosed
	}
	path := ""
	switch name {
	case "model":
		path = l.paths.Model
	case "tokenizer":
		path = l.paths.Tokenizer
	case "runtime":
		path = l.paths.Runtime
	default:
		return nil, ErrArtifact
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, ErrArtifact
	}
	file := os.NewFile(uintptr(fd), "private-profile-component")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, ErrArtifact
	}
	return file, nil
}

func (l *profileFixtureLease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.guard != nil {
		err := l.guard.Close()
		l.guard = nil
		return err
	}
	return nil
}

func (l *profileFixtureLease) WorkerGuard() (*os.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.guard == nil {
		return nil, ErrClosed
	}
	fd, err := unix.FcntlInt(l.guard.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, ErrArtifact
	}
	return os.NewFile(uintptr(fd), "private-profile-guard"), nil
}

func profileFixtureGuard(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, ErrArtifact
	}
	if unix.Flock(int(file.Fd()), unix.LOCK_SH) != nil {
		file.Close()
		return nil, ErrArtifact
	}
	return file, nil
}

func profileSupervisor(t *testing.T, profile Profile) *Supervisor {
	t.Helper()
	worker := os.Getenv("WA_INDEXER_NATIVE_WORKER")
	if worker == "" || !filepath.IsAbs(worker) {
		t.Fatal("explicit absolute native worker required")
	}
	config := nativeTestConfig(t)
	guardRoot := t.TempDir()
	instance, err := profileFixtureGuard(filepath.Join(guardRoot, "instance.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	supervisor, err := NewSupervisor(Config{Executable: worker, Profile: profile, AllowExperimental: true,
		WorkerLifetime: workerlife.Config{Executable: worker, ExecutableProvider: testExecutableNamespace(t)}, InstanceGuard: func() (*os.File, error) {
			fd, err := unix.FcntlInt(instance.Fd(), unix.F_DUPFD_CLOEXEC, 0)
			if err != nil {
				return nil, ErrArtifact
			}
			return os.NewFile(uintptr(fd), "private-profile-instance"), nil
		},
		Acquire: func(ctx context.Context, actual Profile) (ComponentLease, error) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if actual != profile {
				return nil, ErrArtifact
			}
			guard, err := profileFixtureGuard(filepath.Join(guardRoot, "model.lock"))
			if err != nil {
				return nil, err
			}
			return &profileFixtureLease{paths: config.Paths, guard: guard}, nil
		}, RequestTimeout: 30 * time.Second, IdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := supervisor.Close(); err != nil {
			t.Error(err)
		}
	})
	return supervisor
}

func TestNativeInheritedDescriptorSmoke(t *testing.T) {
	if os.Getenv("WA_INDEXER_NATIVE_PROFILE_SMOKE") != "1" {
		t.Skip("explicit isolated native profile smoke gate required")
	}
	supervisor := profileSupervisor(t, ReferenceProfile())
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	if _, err := supervisor.EncodeQuery(ctx, "hello שלום"); err != nil {
		t.Fatal(err)
	}
	if snapshot := supervisor.Snapshot(); snapshot.State != Ready || snapshot.PID == 0 {
		t.Fatal("native worker did not become ready")
	}
	if err := supervisor.Unload(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshot := supervisor.Snapshot(); snapshot.State != Unloaded || snapshot.PID != 0 {
		t.Fatal("native worker did not unload")
	}
}

type profileMeasurement struct {
	RunID                 string             `json:"run_id"`
	SchemaVersion         int                `json:"schema_version"`
	Stage                 string             `json:"stage"`
	Profile               Profile            `json:"profile"`
	FixtureSHA256         string             `json:"fixture_sha256"`
	Passages              int                `json:"passages"`
	Queries               int                `json:"queries"`
	Vectors               [][]float32        `json:"vectors,omitempty"`
	ColdLoadMS            float64            `json:"cold_load_ms"`
	WarmQueries           int                `json:"warm_queries"`
	WarmQueryP95MS        float64            `json:"warm_query_p95_ms"`
	BatchPassages         int                `json:"batch_passages"`
	BatchTokens           int                `json:"batch_tokens"`
	BatchMS               float64            `json:"batch_ms"`
	PassagesPerSecond     float64            `json:"passages_per_second"`
	TokensPerSecond       float64            `json:"tokens_per_second"`
	UnloadedPID           int                `json:"unloaded_pid"`
	WorkerPID             int                `json:"worker_pid"`
	TokenizerGoldenParity bool               `json:"tokenizer_golden_parity"`
	NativeDiagnostics     map[string]float64 `json:"native_diagnostics,omitempty"`
}

func profileRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func profileOutputDirectory(t *testing.T) string {
	t.Helper()
	root := filepath.Join(profileRoot(t), ".artifacts", "acceptance", "encoder-profile")
	directory := os.Getenv("WA_INDEXER_NATIVE_PROFILE_OUTPUT")
	relative, err := filepath.Rel(root, directory)
	if err != nil || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		t.Fatal("private ignored profile output directory required")
	}
	// Reject symlinks in every output directory component.
	for current := directory; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || current == directory && info.Mode().Perm()&0077 != 0 {
			t.Fatal("private profile directory invalid")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != uint32(os.Getuid()) && stat.Uid != 0) || info.Mode().Perm()&0022 != 0 {
			t.Fatal("profile directory ancestor not owner controlled")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return directory
}

func writeProfileJSON(t *testing.T, name string, value any) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal("profile evidence encoding failed")
	}
	file, err := os.OpenFile(filepath.Join(profileOutputDirectory(t), name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal("profile evidence creation failed")
	}
	if _, err = file.Write(append(body, '\n')); err != nil {
		file.Close()
		t.Fatal("profile evidence write failed")
	}
	if err = file.Close(); err != nil {
		t.Fatal("profile evidence close failed")
	}
}

func profilePhase(t *testing.T, phase string, workerPID ...int) {
	t.Helper()
	// The directory is private and validated; phase values never include text.
	directory := profileOutputDirectory(t)
	value := map[string]any{"phase": phase}
	if len(workerPID) > 0 {
		value["worker_pid"] = workerPID[0]
	}
	body, _ := json.Marshal(value)
	file, err := os.CreateTemp(directory, ".phase-")
	if err != nil {
		t.Fatal("profile phase creation failed")
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(body); err != nil {
		file.Close()
		t.Fatal("profile phase write failed")
	}
	if file.Close() != nil || os.Rename(file.Name(), filepath.Join(directory, "phase.json")) != nil {
		t.Fatal("profile phase write failed")
	}
}

func loadProfileFixture(t *testing.T) ([]string, []string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(profileRoot(t), "internal/inference/testdata/encoder-retrieval-smoke.json"))
	if err != nil {
		t.Fatal("frozen profile fixture unavailable")
	}
	hash := sha256.Sum256(body)
	var fixture struct {
		Version   string
		Documents []string
		Queries   []struct{ Text string }
	}
	if hex.EncodeToString(hash[:]) != profileFixtureSHA256 || json.Unmarshal(body, &fixture) != nil || fixture.Version != "synthetic_bilingual_smoke_v1" || len(fixture.Documents) != 30 || len(fixture.Queries) != 10 {
		t.Fatal("frozen 40-vector fixture identity mismatch")
	}
	queries := make([]string, len(fixture.Queries))
	for i := range queries {
		queries[i] = fixture.Queries[i].Text
	}
	return fixture.Documents, queries
}

func profileEncodePassages(t *testing.T, supervisor *Supervisor, texts []string) [][]float32 {
	t.Helper()
	vectors := make([][]float32, 0, len(texts))
	for first := 0; first < len(texts); first += MaxBatchItems {
		last := min(first+MaxBatchItems, len(texts))
		batch, err := supervisor.EncodePassages(context.Background(), texts[first:last])
		if err != nil {
			t.Fatal(err)
		}
		vectors = append(vectors, batch...)
	}
	return vectors
}

// This helper executes only in an explicitly gated fresh process. Combined
// measurements allocate all model/tokenizer/runtime state in the worker.
func TestNativeProfileMeasurementProcess(t *testing.T) {
	if os.Getenv("WA_INDEXER_NATIVE_PROFILE_CHILD") != "1" {
		t.Skip("fresh isolated measurement child required")
	}
	profileOutputDirectory(t)
	profile := ReferenceProfile()
	allocator := os.Getenv("WA_INDEXER_NATIVE_PROFILE_ALLOCATOR")
	if allocator == "bounded" {
		var err error
		profile, err = LoadProfile(filepath.Join(profileRoot(t), "packaging/embedding-manifest-arm64-fp32-bounded.json"))
		if err != nil {
			t.Fatal("bounded profile manifest invalid")
		}
	} else if allocator != "default" {
		t.Fatal("profile allocator invalid")
	}
	stage := os.Getenv("WA_INDEXER_NATIVE_PROFILE_STAGE")
	record := profileMeasurement{RunID: filepath.Base(profileOutputDirectory(t)), SchemaVersion: 1, Stage: stage, Profile: profile, FixtureSHA256: profileFixtureSHA256}
	profilePhase(t, "loading")
	if stage != "combined" {
		profileMeasureComponent(t, &record)
		writeProfileJSON(t, stage+"-"+allocator+"-result.json", record)
		return
	}
	passages, queries := loadProfileFixture(t)
	supervisor := profileSupervisor(t, profile)
	start := time.Now()
	if _, err := supervisor.CountPassage(context.Background(), passages[0]); err != nil {
		t.Fatal(err)
	}
	record.ColdLoadMS = float64(time.Since(start)) / float64(time.Millisecond)
	if snapshot := supervisor.Snapshot(); snapshot.PID == 0 || snapshot.State != Ready {
		t.Fatal("combined native worker unavailable")
	}
	record.WorkerPID = supervisor.Snapshot().PID
	profilePhase(t, "loaded", record.WorkerPID)
	record.Vectors = profileEncodePassages(t, supervisor, passages)
	for _, query := range queries {
		vector, err := supervisor.EncodeQuery(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		record.Vectors = append(record.Vectors, vector)
	}
	record.Passages, record.Queries = len(passages), len(queries)
	profilePhase(t, "warm", record.WorkerPID)
	latencies := make([]float64, 20)
	for i := range latencies {
		start := time.Now()
		if _, err := supervisor.EncodeQuery(context.Background(), queries[i%len(queries)]); err != nil {
			t.Fatal(err)
		}
		latencies[i] = float64(time.Since(start)) / float64(time.Millisecond)
	}
	sort.Float64s(latencies)
	record.WarmQueries, record.WarmQueryP95MS = len(latencies), latencies[int(math.Ceil(.95*float64(len(latencies))))-1]
	for _, passage := range passages {
		count, err := supervisor.CountPassage(context.Background(), passage)
		if err != nil {
			t.Fatal(err)
		}
		record.BatchTokens += count
	}
	profilePhase(t, "batch", record.WorkerPID)
	start = time.Now()
	profileEncodePassages(t, supervisor, passages)
	elapsed := time.Since(start)
	record.BatchPassages, record.BatchMS = len(passages), float64(elapsed)/float64(time.Millisecond)
	record.PassagesPerSecond = float64(len(passages)) / elapsed.Seconds()
	record.TokensPerSecond = float64(record.BatchTokens) / elapsed.Seconds()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := supervisor.Unload(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshot := supervisor.Snapshot(); snapshot.PID != 0 || snapshot.State != Unloaded {
		t.Fatal("combined native worker did not fully unload")
	}
	record.UnloadedPID = supervisor.Snapshot().PID
	profilePhase(t, "unloaded", 0)
	time.Sleep(250 * time.Millisecond)
	writeProfileJSON(t, stage+"-"+allocator+"-result.json", record)
}

func profileMeasureComponent(t *testing.T, record *profileMeasurement) {
	t.Helper()
	config := nativeTestConfig(t)
	start := time.Now()
	var closeComponent func()
	switch record.Stage {
	case "tokenizer":
		file, err := openNativeArtifact(config.Paths.Tokenizer, record.Profile.TokenizerSHA256, 6)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		tok, err := tokenizers.FromFile(fmt.Sprintf("/dev/fd/%d", file.Fd()))
		if err != nil {
			t.Fatal("component tokenizer initialization failed")
		}
		closeComponent = func() {
			if err := tok.Close(); err != nil {
				t.Error("component tokenizer close failed")
			}
		}
		defer closeComponent()
		checkProfileTokenizerGoldens(t, tok)
		record.TokenizerGoldenParity = true
	case "runtime", "model":
		file, err := openNativeArtifact(config.Paths.Runtime, record.Profile.RuntimeSHA256, 7)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		ort.SetSharedLibraryPath(fmt.Sprintf("/dev/fd/%d", file.Fd()))
		if err := ort.InitializeEnvironment(); err != nil {
			t.Fatal("component runtime initialization failed")
		}
		defer ort.DestroyEnvironment()
		if record.Stage == "model" {
			model, err := openNativeArtifact(config.Paths.Model, record.Profile.ModelSHA256, 5)
			if err != nil {
				t.Fatal(err)
			}
			defer model.Close()
			opts, err := ort.NewSessionOptions()
			if err != nil {
				t.Fatal("component session options unavailable")
			}
			defer opts.Destroy()
			configure := []func() error{func() error { return opts.SetIntraOpNumThreads(2) }, func() error { return opts.SetInterOpNumThreads(1) }, func() error { return opts.SetExecutionMode(ort.ExecutionModeSequential) }, func() error { return opts.AddSessionConfigEntry("session.intra_op.allow_spinning", "0") }, func() error { return opts.AddSessionConfigEntry("session.inter_op.allow_spinning", "0") }}
			if record.Profile.Allocator == "bounded" {
				configure = append(configure, func() error { return opts.SetCpuMemArena(false) }, func() error { return opts.SetMemPattern(false) })
			}
			for _, apply := range configure {
				if apply() != nil {
					t.Fatal("component session options rejected")
				}
			}
			session, err := ort.NewDynamicAdvancedSession(fmt.Sprintf("/dev/fd/%d", model.Fd()), []string{"input_ids", "attention_mask", "token_type_ids"}, []string{"last_hidden_state"}, opts)
			if err != nil {
				t.Fatal("component model initialization failed")
			}
			defer session.Destroy()
		}
	default:
		t.Fatal("profile component stage invalid")
	}
	record.ColdLoadMS = float64(time.Since(start)) / float64(time.Millisecond)
	profilePhase(t, "loaded")
	time.Sleep(350 * time.Millisecond)
}

func checkProfileTokenizerGoldens(t *testing.T, tok *tokenizers.Tokenizer) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(profileRoot(t), "internal/inference/testdata/encoder-tokenizer-reference.json"))
	if err != nil {
		t.Fatal("independent tokenizer golden unavailable")
	}
	var golden struct {
		Reference       string
		TokenizerSHA256 string `json:"tokenizer_sha256"`
		Fixtures        []struct {
			Text          string
			IDs           []uint32
			AttentionMask []uint32 `json:"attention_mask"`
			TypeIDs       []uint32 `json:"type_ids"`
		}
	}
	if json.Unmarshal(body, &golden) != nil || golden.Reference != "huggingface/tokenizers-python-0.22.1" || golden.TokenizerSHA256 != TokenizerSHA256 || len(golden.Fixtures) != 8 {
		t.Fatal("independent tokenizer golden identity mismatch")
	}
	for _, fixture := range golden.Fixtures {
		encoded, err := tok.EncodeWithOptionsErr(fixture.Text, true, tokenizers.WithReturnAttentionMask(), tokenizers.WithReturnTypeIDs())
		if err != nil {
			t.Fatal("independent tokenizer encoding failed")
		}
		actual, err := truncateNativeEncoding(encoded, EncoderTokenLimit)
		if err != nil || !reflect.DeepEqual(actual.IDs, fixture.IDs) || !reflect.DeepEqual(actual.AttentionMask, fixture.AttentionMask) || !reflect.DeepEqual(actual.TypeIDs, fixture.TypeIDs) {
			t.Fatal("FromFile independent tokenizer golden mismatch")
		}
	}
}

func TestSelectedProfileFrozenReferenceParity(t *testing.T) {
	if os.Getenv("WA_INDEXER_NATIVE_PROFILE") != "1" {
		t.Skip("explicit native profile reference gate required")
	}
	if os.Getenv("WA_INDEXER_NATIVE_TEST") != "1" || os.Getenv("WA_INDEXER_NATIVE_PROFILE_PRECOMPUTED") != "1" {
		t.Fatal("explicit native fixtures and serial cleanup-owning measurement driver required")
	}
	directory := profileOutputDirectory(t)
	profile, err := LoadProfile(filepath.Join(profileRoot(t), "packaging/embedding-manifest-arm64-fp32-bounded.json"))
	if err != nil || profile.Allocator != "bounded" || profile.Precision != "fp32" || profile.ReleaseSelected {
		t.Fatal("bounded experimental profile manifest unavailable")
	}
	loadProfileFixture(t)
	results := make([]profileMeasurement, 2)
	for i, allocator := range []string{"default", "bounded"} {
		body, err := os.ReadFile(filepath.Join(directory, "combined-"+allocator+"-result.json"))
		if err != nil || json.Unmarshal(body, &results[i]) != nil {
			t.Fatal("private native reference result unavailable")
		}
		result := results[i]
		if result.RunID != filepath.Base(directory) || result.SchemaVersion != 1 || result.Stage != "combined" || result.Profile.Validate() != nil || result.Profile.Allocator != allocator || result.Profile.Precision != "fp32" || result.Profile.ModelSHA256 != FP32ModelSHA256 || result.FixtureSHA256 != profileFixtureSHA256 || result.Passages != 30 || result.Queries != 10 || len(result.Vectors) != 40 || result.UnloadedPID != 0 {
			t.Fatal("frozen native reference result identity mismatch")
		}
	}
	minCos, maxDelta := 1., 0.
	for i, reference := range results[0].Vectors {
		candidate := results[1].Vectors[i]
		if len(reference) != EmbeddingDimension || len(candidate) != EmbeddingDimension {
			t.Fatal("frozen native reference vector dimension mismatch")
		}
		dot, normA, normB := 0., 0., 0.
		for j, a := range reference {
			b := candidate[j]
			if math.IsNaN(float64(a)) || math.IsInf(float64(a), 0) || math.IsNaN(float64(b)) || math.IsInf(float64(b), 0) {
				t.Fatal("nonfinite frozen reference vector")
			}
			dot += float64(a) * float64(b)
			normA += float64(a) * float64(a)
			normB += float64(b) * float64(b)
			maxDelta = math.Max(maxDelta, math.Abs(float64(a)-float64(b)))
		}
		if math.Abs(normA-1) > 1e-5 || math.Abs(normB-1) > 1e-5 {
			t.Fatal("frozen reference normalization mismatch")
		}
		minCos = math.Min(minCos, dot/math.Sqrt(normA*normB))
	}
	writeProfileJSON(t, "parity.json", map[string]any{"schema_version": 1, "reference_profile": results[0].Profile.ID, "candidate_profile": results[1].Profile.ID, "fixture_sha256": profileFixtureSHA256, "reference_model_sha256": FP32ModelSHA256, "compared_vectors": 40, "passages": 30, "queries": 10, "minimum_cosine": minCos, "maximum_component_delta": maxDelta, "minimum_cosine_threshold": .98, "maximum_component_delta_threshold": .03, "pass": minCos >= .98 && maxDelta <= .03, "release_selected": false, "scope": "numerical parity only; synthetic fixture does not establish relevance or release quality"})
	if minCos < .98 || maxDelta > .03 {
		t.Fatal("frozen native reference parity thresholds failed")
	}
}
