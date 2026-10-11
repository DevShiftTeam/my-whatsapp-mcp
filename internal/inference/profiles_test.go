package inference

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestProfileFrozenContract(t *testing.T) {
	p := ReferenceProfile()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.ReleaseSelected {
		t.Fatal("failed memory profile selected")
	}
	for _, mutate := range []func(*Profile){func(p *Profile) { p.Dimension = 383 }, func(p *Profile) { p.MaximumTokens = 257 }, func(p *Profile) { p.QueryPrefix = "" }, func(p *Profile) { p.TokenizerSHA256 = "bad" }, func(p *Profile) { p.ModelSHA256 = INT8ModelSHA256 }, func(p *Profile) { p.Allocator = "unbounded" }} {
		q := p
		mutate(&q)
		if q.Validate() == nil {
			t.Fatal("invalid profile accepted")
		}
	}
	p.ReleaseSelected = true
	if p.Validate() == nil {
		t.Fatal("failed release profile accepted")
	}
}

func TestProfileIDBindsModelSpace(t *testing.T) {
	profiles := []Profile{ReferenceProfile(), ReferenceProfile(), ReferenceProfile()}
	profiles[1].ID = "darwin-arm64-fp32-bounded-experimental"
	profiles[1].Allocator = "bounded"
	profiles[2].ID = "darwin-arm64-dynamic-int8-feasibility"
	profiles[2].Precision = "int8"
	profiles[2].ModelSHA256 = INT8ModelSHA256
	for _, profile := range profiles {
		if err := profile.Validate(); err != nil {
			t.Fatalf("frozen profile %s: %v", profile.ID, err)
		}
		for _, other := range profiles {
			if profile.ID == other.ID {
				continue
			}
			forged := profile
			forged.ID = other.ID
			if forged.Validate() != ErrArtifact {
				t.Fatalf("profile label %s accepted another model space", other.ID)
			}
		}
	}
	unknown := ReferenceProfile()
	unknown.ID = "syntactically-valid-unregistered-profile"
	if unknown.Validate() != ErrArtifact {
		t.Fatal("unknown profile label accepted")
	}
}

func TestLoadProfilePreservesFrozenIdentities(t *testing.T) {
	for _, path := range []string{"../../packaging/embedding-manifest.json", "../../packaging/embedding-manifest-arm64-int8.json", "../../packaging/embedding-manifest-arm64-fp32-bounded.json"} {
		if _, err := LoadProfile(path); err != nil {
			t.Fatalf("frozen manifest %s: %v", path, err)
		}
	}
}

func TestPoolEmbeddingFrozenMath(t *testing.T) {
	hidden := make([]float32, 2*384)
	hidden[0] = 2
	hidden[385] = 4
	v, err := PoolEmbedding(hidden, []int64{1, 0}, 384)
	if err != nil || len(v) != 384 || v[0] != 1 {
		t.Fatalf("pool contract %v", err)
	}
	hidden[0] = float32(math.Inf(1))
	if _, err = PoolEmbedding(hidden, []int64{1, 0}, 384); err == nil {
		t.Fatal("nonfinite accepted")
	}
}

func TestProfileRejectsOversizeManifest(t *testing.T) {
	body, err := os.ReadFile("../../packaging/embedding-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, bytes.Repeat([]byte(" "), (1<<20)-len(body))...)
	body = append(body, []byte("invalid")...)
	path := filepath.Join(t.TempDir(), "profile.json")
	if os.WriteFile(path, body, 0600) != nil {
		t.Fatal("write")
	}
	if _, err = LoadProfile(path); err == nil {
		t.Fatal("oversize suffix accepted")
	}
}
