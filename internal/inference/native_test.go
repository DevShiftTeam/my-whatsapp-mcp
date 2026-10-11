//go:build native_onnx

package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/daulet/tokenizers"
)

func TestNativeArtifactHashAndPinnedIdentity(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "model")
	data := []byte("verified native artifact")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	expected := hex.EncodeToString(hash[:])
	file, err := openNativeArtifact(path, expected, 5)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replaced"), 0600); err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(file)
	if err != nil || string(actual) != string(data) {
		t.Fatalf("verified file identity changed: %q %v", actual, err)
	}
	if other, err := openNativeArtifact(path, expected, 5); !errors.Is(err, ErrArtifact) || other != nil {
		if other != nil {
			other.Close()
		}
		t.Fatalf("hash mismatch accepted: %v", err)
	}
	symlink := filepath.Join(root, "link")
	if err := os.Symlink(path+".old", symlink); err != nil {
		t.Fatal(err)
	}
	if other, err := openNativeArtifact(symlink, expected, 5); !errors.Is(err, ErrArtifact) || other != nil {
		if other != nil {
			other.Close()
		}
		t.Fatalf("symlink accepted: %v", err)
	}
	if other, err := openNativeArtifact("/dev/fd/6", expected, 5); !errors.Is(err, ErrArtifact) || other != nil {
		if other != nil {
			other.Close()
		}
		t.Fatalf("unexpected inherited descriptor accepted: %v", err)
	}
}

func nativeTestConfig(t *testing.T) NativeConfig {
	t.Helper()
	if os.Getenv("WA_INDEXER_NATIVE_TEST") != "1" {
		t.Skip("explicit native execution lane and verified local artifacts required")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	artifacts := filepath.Join(root, ".artifacts/native")
	return NativeConfig{Profile: ReferenceProfile(), Paths: ComponentPaths{
		Model: filepath.Join(artifacts, "model.onnx"), Tokenizer: filepath.Join(artifacts, "tokenizer.json"),
		Runtime: filepath.Join(artifacts, "onnxruntime-osx-arm64-1.23.2/lib/libonnxruntime.1.23.2.dylib"),
	}}
}

func TestNativeTokenizerIndependentGoldenParity(t *testing.T) {
	config := nativeTestConfig(t)
	backend, err := NewNativeEncoder(config)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	native := backend.(*nativeEncoder)
	data, err := os.ReadFile("../feasibility/testdata/encoder-tokenizer-reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Reference       string
		TokenizerSHA256 string `json:"tokenizer_sha256"`
		Fixtures        []struct {
			Name, Text    string
			IDs           []uint32
			AttentionMask []uint32 `json:"attention_mask"`
			TypeIDs       []uint32 `json:"type_ids"`
		}
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Reference != "huggingface/tokenizers-python-0.22.1" || fixture.TokenizerSHA256 != config.Profile.TokenizerSHA256 || len(fixture.Fixtures) != 8 {
		t.Fatal("independent golden identity mismatch")
	}
	for _, test := range fixture.Fixtures {
		t.Run(test.Name, func(t *testing.T) {
			encoding, err := native.tokenizer.EncodeWithOptionsErr(test.Text, true, tokenizers.WithReturnAttentionMask(), tokenizers.WithReturnTypeIDs())
			if err != nil {
				t.Fatal(err)
			}
			actual, err := truncateNativeEncoding(encoding, config.Profile.MaximumTokens)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual.IDs, test.IDs) || !reflect.DeepEqual(actual.AttentionMask, test.AttentionMask) || !reflect.DeepEqual(actual.TypeIDs, test.TypeIDs) {
				t.Fatal("independent tokenizer golden mismatch")
			}
			if strings.HasPrefix(test.Text, config.Profile.PassagePrefix) {
				count, err := backend.Count(context.Background(), strings.TrimPrefix(test.Text, config.Profile.PassagePrefix))
				if err != nil || count != len(encoding.IDs) {
					t.Fatalf("full prefixed count mismatch: %d != %d %v", count, len(encoding.IDs), err)
				}
				if test.Name == "truncated" && count <= config.Profile.MaximumTokens {
					t.Fatal("Count silently truncated long text")
				}
			}
		})
	}
}

func TestNativeSplitUsesActualTokenizerAndOriginalText(t *testing.T) {
	backend, err := NewNativeEncoder(nativeTestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	for _, text := range []string{"", "  \t\n\r\n", strings.Repeat("שָׁלוֹם  team 👩\u200d💻 \u200f123\u200e\t\r\n", 120)} {
		parts, err := backend.Split(context.Background(), text, 32)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(parts, "") != text {
			t.Fatal("native split changed original UTF-8")
		}
		for _, part := range parts {
			count, err := backend.Count(context.Background(), part)
			if err != nil || count > 32 || !utf8.ValidString(part) {
				t.Fatalf("native split invalid: tokens=%d valid_utf8=%t error=%v", count, utf8.ValidString(part), err)
			}
		}
	}
}

func TestNativeEncoderNormalizedAndReusableAfterCancellation(t *testing.T) {
	config := nativeTestConfig(t)
	backend, err := NewNativeEncoder(config)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if second, secondErr := NewNativeEncoder(config); !errors.Is(secondErr, ErrUnavailable) || second != nil {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second native owner accepted: %v", secondErr)
	}
	if _, err = backend.EncodePassages(context.Background(), []string{strings.Repeat("שלום world ", 300)}); !errors.Is(err, ErrLimit) {
		t.Fatalf("over-limit passage silently truncated: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = backend.Count(cancelled, "hello"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Count: %v", err)
	}
	ctx, stop := context.WithTimeout(context.Background(), time.Millisecond)
	defer stop()
	if _, err = backend.EncodeQuery(ctx, strings.Repeat("שלום world ", 300)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request deadline: %v", err)
	}
	vector, err := backend.EncodeQuery(context.Background(), "hello שלום")
	if err != nil || len(vector) != 384 {
		t.Fatalf("encoder reusable after cancellation: %v dimension=%d", err, len(vector))
	}
	var norm float64
	for _, x := range vector {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			t.Fatal("nonfinite embedding")
		}
		norm += float64(x) * float64(x)
	}
	if math.Abs(norm-1) > 1e-5 {
		t.Fatalf("vector not L2 normalized: %.9f", norm)
	}
	if err = backend.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = backend.Count(context.Background(), "hello"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Count accepted closed backend: %v", err)
	}
	if _, err = backend.EncodePassages(context.Background(), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("empty passage batch accepted closed backend: %v", err)
	}
}

func TestNativeLegacyPassageRetainsGoldenTruncation(t *testing.T) {
	backend, err := NewLegacyNativeEncoder(nativeTestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	text := strings.Repeat("שלום world ", 300)
	count, err := backend.Count(context.Background(), text)
	if err != nil || count <= 256 {
		t.Fatalf("legacy count must remain untruncated: %d %v", count, err)
	}
	vectors, err := backend.EncodePassages(context.Background(), []string{text})
	if err != nil || len(vectors) != 1 || len(vectors[0]) != 384 {
		t.Fatalf("legacy long passage no longer encodes with EOS truncation: %v", err)
	}
	var norm float64
	for _, x := range vectors[0] {
		norm += float64(x) * float64(x)
	}
	if math.Abs(norm-1) > 1e-5 {
		t.Fatalf("legacy passage not normalized: %.9f", norm)
	}
}
