//go:build darwin || linux

package app

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestBundledAssetsRetainVerifiedDescriptorAndCloseAuthority(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "Contents", "Resources"), 0700); err != nil {
		t.Fatal(err)
	}
	name := "Contents/Resources/profile.json"
	bytes := []byte("verified immutable fixture")
	if err := os.WriteFile(filepath.Join(dir, name), bytes, 0400); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	assets := &bundledAIAssets{root: root, files: make(map[string]*os.File)}
	defer assets.Close()
	digest := sha256.Sum256(bytes)
	artifact := bundleArtifact{SizeBytes: int64(len(bytes)), SHA256: hex.EncodeToString(digest[:])}
	file, err := assets.open(name, &artifact, 1024)
	if err != nil {
		t.Fatal(err)
	}
	assets.files[name] = file
	if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, name+".old")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("unverified replacement"), 0400); err != nil {
		t.Fatal(err)
	}
	if reopened, err := assets.open(name, &artifact, 1024); err == nil {
		reopened.Close()
		t.Fatal("replacement bypassed artifact hash")
	}
	duplicate, err := assets.duplicate(name)
	if err != nil {
		t.Fatal(err)
	}
	defer duplicate.Close()
	got, err := io.ReadAll(duplicate)
	if err != nil || string(got) != string(bytes) {
		t.Fatal("retained FD changed to replacement pathname")
	}
	if err := assets.Close(); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := assets.duplicate(name); err == nil {
		duplicate.Close()
		t.Fatal("closed authority still issued descriptor")
	}
}
