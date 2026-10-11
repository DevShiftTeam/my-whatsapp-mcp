//go:build native_onnx

package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/daulet/tokenizers"
	ort "github.com/yalue/onnxruntime_go"
)

// ORT's environment and library handle are global. A worker owns one backend;
// the mutex also prevents direct legacy callers from creating a second session.
var nativeEnvironment sync.Mutex

type nativeEncoder struct {
	mu             sync.Mutex
	profile        Profile
	tokenizer      *tokenizers.Tokenizer
	session        *ort.DynamicAdvancedSession
	files          []*os.File
	closed         bool
	legacyTruncate bool
	diagnostics    map[string]float64
}

// NewNativeEncoder constructs a backend for one isolated worker. The caller
// must exit that worker after initialization failure: the pinned ORT binding
// cannot release a loaded library if its environment creation fails early.
func NewNativeEncoder(config NativeConfig) (Backend, error) {
	return newNativeEncoder(config, false)
}

// NewLegacyNativeEncoder preserves the feasibility probe's passage truncation
// behavior. Product workers use NewNativeEncoder and reject passage overflow.
func NewLegacyNativeEncoder(config NativeConfig) (Backend, error) {
	return newNativeEncoder(config, true)
}

func newNativeEncoder(config NativeConfig, legacyTruncate bool) (Backend, error) {
	if err := config.Profile.Validate(); err != nil {
		return nil, err
	}
	files := make([]*os.File, 0, 3)
	paths := make([]string, 0, 3)
	for i, component := range []struct{ path, hash string }{
		{config.Paths.Model, config.Profile.ModelSHA256},
		{config.Paths.Tokenizer, config.Profile.TokenizerSHA256},
		{config.Paths.Runtime, config.Profile.RuntimeSHA256},
	} {
		file, err := openNativeArtifact(component.path, component.hash, i+5)
		if err != nil {
			for _, opened := range files {
				opened.Close()
			}
			return nil, err
		}
		files = append(files, file)
		// Pin the same file identity that was hashed while native loaders reopen
		// it. Native loaders receive no mutable model-store pathnames.
		paths = append(paths, fmt.Sprintf("/dev/fd/%d", file.Fd()))
	}
	ok := false
	defer func() {
		if !ok {
			for _, file := range files {
				file.Close()
			}
		}
	}()
	if !nativeEnvironment.TryLock() {
		return nil, ErrUnavailable
	}
	defer func() {
		if !ok {
			nativeEnvironment.Unlock()
		}
	}()
	diagnostics := map[string]float64{}
	sample := func(name string) {
		if value, err := nativeRSS(); err == nil {
			diagnostics[name] = value
		}
	}
	sample("stage_verified_artifacts_rss_bytes")
	ort.SetSharedLibraryPath(paths[2])
	if err := ort.InitializeEnvironment(); err != nil {
		return nil, ErrUnavailable
	}
	sample("stage_runtime_loaded_rss_bytes")
	environmentOK := false
	defer func() {
		if !environmentOK {
			ort.DestroyEnvironment()
		}
	}()
	// The frozen tokenizer artifact has truncation:null and padding:null. Using
	// FromFile avoids the Go JSON byte slice and preserves a single native copy.
	if _, err := files[1].Seek(0, io.SeekStart); err != nil {
		return nil, ErrArtifact
	}
	tok, err := tokenizers.FromFile(paths[1])
	if err != nil {
		return nil, ErrArtifact
	}
	tokenizerOK := false
	defer func() {
		if !tokenizerOK {
			tok.Close()
		}
	}()
	sample("stage_tokenizer_loaded_rss_bytes")
	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer opts.Destroy()
	configure := []func() error{
		func() error { return opts.SetIntraOpNumThreads(2) },
		func() error { return opts.SetInterOpNumThreads(1) },
		func() error { return opts.SetExecutionMode(ort.ExecutionModeSequential) },
		func() error { return opts.AddSessionConfigEntry("session.intra_op.allow_spinning", "0") },
		func() error { return opts.AddSessionConfigEntry("session.inter_op.allow_spinning", "0") },
	}
	if config.Profile.Allocator == "bounded" {
		configure = append(configure, func() error { return opts.SetCpuMemArena(false) }, func() error { return opts.SetMemPattern(false) })
	}
	for _, apply := range configure {
		if err := apply(); err != nil {
			return nil, err
		}
	}
	if _, err := files[0].Seek(0, io.SeekStart); err != nil {
		return nil, ErrArtifact
	}
	session, err := ort.NewDynamicAdvancedSession(paths[0], []string{"input_ids", "attention_mask", "token_type_ids"}, []string{"last_hidden_state"}, opts)
	if err != nil {
		return nil, err
	}
	ok, environmentOK, tokenizerOK = true, true, true
	sample("stage_session_loaded_rss_bytes")
	return &nativeEncoder{profile: config.Profile, tokenizer: tok, session: session, files: files, diagnostics: diagnostics, legacyTruncate: legacyTruncate}, nil
}

func openNativeArtifact(path, expected string, inheritedFD int) (*os.File, error) {
	if len(expected) != 64 || !filepath.IsAbs(path) {
		return nil, ErrArtifact
	}
	var file *os.File
	var err error
	if path == fmt.Sprintf("/dev/fd/%d", inheritedFD) {
		// These are worker descriptors supplied by the supervisor under an
		// active component lease, not arbitrary symlink paths from IPC.
		file, err = os.Open(path)
	} else {
		if filepath.Clean(path) != path || len(path) >= 8 && path[:8] == "/dev/fd/" {
			return nil, ErrArtifact
		}
		var fd int
		fd, err = syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err == nil {
			file = os.NewFile(uintptr(fd), path)
		}
	}
	if err != nil || file == nil {
		return nil, ErrArtifact
	}
	ok := false
	defer func() {
		if !ok {
			file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, ErrArtifact
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, ErrArtifact
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil || hex.EncodeToString(hash.Sum(nil)) != expected {
		return nil, ErrArtifact
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, ErrArtifact
	}
	ok = true
	return file, nil
}

func (e *nativeEncoder) Diagnostics() map[string]float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	copy := make(map[string]float64, len(e.diagnostics))
	for name, value := range e.diagnostics {
		copy[name] = value
	}
	return copy
}

func (e *nativeEncoder) Count(ctx context.Context, text string) (int, error) {
	if err := validateNativeText(text); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.countLocked(ctx, text)
}

func (e *nativeEncoder) countLocked(ctx context.Context, text string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if e.closed {
		return 0, ErrClosed
	}
	encoding, err := e.tokenizer.EncodeWithOptionsErr(e.profile.PassagePrefix+text, true)
	if err != nil {
		return 0, err
	}
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	if len(encoding.IDs) < 2 {
		return 0, ErrProtocol
	}
	return len(encoding.IDs), nil
}

func (e *nativeEncoder) Split(ctx context.Context, text string, maxTokens int) ([]string, error) {
	if maxTokens > e.profile.MaximumTokens {
		return nil, ErrLimit
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrClosed
	}
	return splitNativeText(ctx, text, maxTokens, e.countLocked)
}

func (e *nativeEncoder) EncodeQuery(ctx context.Context, text string) ([]float32, error) {
	return e.encode(ctx, e.profile.QueryPrefix, text)
}

func (e *nativeEncoder) EncodePassages(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) > 128 {
		return nil, ErrLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vector, err := e.encode(ctx, e.profile.PassagePrefix, text)
		if err != nil {
			return nil, err
		}
		vectors[i] = vector
	}
	return vectors, nil
}

func truncateNativeEncoding(encoding tokenizers.Encoding, maximum int) (tokenizers.Encoding, error) {
	size := len(encoding.IDs)
	if maximum < 2 || size < 2 || len(encoding.AttentionMask) != size || len(encoding.TypeIDs) != size || encoding.IDs[0] != 0 || encoding.IDs[size-1] != 2 {
		return tokenizers.Encoding{}, ErrProtocol
	}
	if size <= maximum {
		return encoding, nil
	}
	// This frozen single-sequence E5 template wraps BOS and EOS around the
	// content. Keep the EOS added after truncation, matching HF right truncation.
	truncate := func(values []uint32) []uint32 {
		result := make([]uint32, maximum)
		copy(result, values[:maximum-1])
		result[maximum-1] = values[size-1]
		return result
	}
	return tokenizers.Encoding{IDs: truncate(encoding.IDs), AttentionMask: truncate(encoding.AttentionMask), TypeIDs: truncate(encoding.TypeIDs)}, nil
}

func (e *nativeEncoder) encode(ctx context.Context, prefix, text string) ([]float32, error) {
	if err := validateNativeText(text); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.closed {
		return nil, ErrClosed
	}
	encoding, err := e.tokenizer.EncodeWithOptionsErr(prefix+text, true, tokenizers.WithReturnAttentionMask(), tokenizers.WithReturnTypeIDs())
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if !e.legacyTruncate && prefix == e.profile.PassagePrefix && len(encoding.IDs) > e.profile.MaximumTokens {
		return nil, ErrLimit
	}
	encoding, err = truncateNativeEncoding(encoding, e.profile.MaximumTokens)
	if err != nil {
		return nil, err
	}
	size := len(encoding.IDs)
	ids, mask, types := make([]int64, size), make([]int64, size), make([]int64, size)
	for i := range ids {
		ids[i], mask[i], types[i] = int64(encoding.IDs[i]), int64(encoding.AttentionMask[i]), int64(encoding.TypeIDs[i])
	}
	values := make([]ort.Value, 3)
	defer func() {
		for _, value := range values {
			if value != nil {
				value.Destroy()
			}
		}
	}()
	for i, data := range [][]int64{ids, mask, types} {
		tensor, err := ort.NewTensor(ort.NewShape(1, int64(size)), data)
		if err != nil {
			return nil, err
		}
		values[i] = tensor
	}
	outputs := []ort.Value{nil}
	defer func() {
		if outputs[0] != nil {
			outputs[0].Destroy()
		}
	}()
	run, err := ort.NewRunOptions()
	if err != nil {
		return nil, err
	}
	defer run.Destroy()
	done, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		select {
		case <-ctx.Done():
			run.Terminate()
		case <-done:
		}
	}()
	err = e.session.RunWithOptions(values, outputs, run)
	close(done)
	<-joined
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	hidden, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, ErrProtocol
	}
	shape := hidden.GetShape()
	if len(shape) != 3 || shape[0] != 1 || shape[1] != int64(size) || shape[2] != int64(e.profile.Dimension) {
		return nil, ErrProtocol
	}
	return PoolEmbedding(hidden.GetData(), mask, e.profile.Dimension)
}

func (e *nativeEncoder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	errorsFound := []error{e.session.Destroy(), e.tokenizer.Close(), ort.DestroyEnvironment()}
	for _, file := range e.files {
		errorsFound = append(errorsFound, file.Close())
	}
	nativeEnvironment.Unlock()
	return errors.Join(errorsFound...)
}
