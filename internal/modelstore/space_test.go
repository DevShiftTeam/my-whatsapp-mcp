package modelstore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResumeAdmitsOnlyRemainingGrowth(t *testing.T) {
	data := strings.Repeat("12345678", 1024)
	var calls atomic.Int32
	var retained atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"same"`)
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "8192")
			_, _ = io.WriteString(w, data[:2048])
			retained.Store(2048)
			return
		}
		w.Header().Set("Content-Range", "bytes 2048-8191/8192")
		w.WriteHeader(206)
		_, _ = io.WriteString(w, data[2048:])
	}))
	defer server.Close()
	s := testStore(t, catalogFor(component("model", []byte(data), server.URL)), server, Options{HeadroomBytes: 1024, FreeBytes: func() (int64, error) {
		return 8192 + 1024 + (1 << 20) - retained.Load(), nil
	}})
	if err := s.Install(context.Background(), "test-profile", nil); err == nil {
		t.Fatal("first truncated request passed")
	}
	if err := s.Install(context.Background(), "test-profile", nil); err != nil {
		t.Fatalf("retained prefix counted twice: %v", err)
	}
}

func TestCompleteUnactivatedVersionDoesNotRedownload(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, "abcdef") }))
	defer server.Close()
	s := testStore(t, catalogFor(component("model", []byte("abcdef"), server.URL)), server, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	err := s.Install(ctx, "test-profile", func(p Progress) {
		if p.Phase == "activating" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interruption: %v", err)
	}
	if err := s.Install(context.Background(), "test-profile", nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("complete version downloaded %d times", calls.Load())
	}
}

func TestSpaceReportsRetainedVersionsAndPartialBytes(t *testing.T) {
	s := testStore(t, catalogFor(component("model", []byte("abcdef"), "")), nil, Options{})
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "model.bin"), []byte("abcdef"), 0600)
	if err := s.Import(context.Background(), "test-profile", dir); err != nil {
		t.Fatal(err)
	}
	plan, err := s.Space(context.Background(), "test-profile")
	if err != nil {
		t.Fatal(err)
	}
	if plan.ExistingBytes < 6 {
		t.Fatalf("retained bytes omitted: %+v", plan)
	}
}

func TestRemoveCleansOwnedPriorVersions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "models")
	opts := Options{FreeBytes: func() (int64, error) { return 32 << 30, nil }}
	s, err := Open(root, catalogFor(component("model", []byte("abcdef"), "")), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "model.bin"), []byte("abcdef"), 0600)
	if err := s.Import(context.Background(), "test-profile", dir); err != nil {
		t.Fatal(err)
	}
	next, err := Open(root, catalogFor(component("model", []byte("ghijkl"), "")), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	_ = os.WriteFile(filepath.Join(dir, "model.bin"), []byte("ghijkl"), 0600)
	if err := next.Import(context.Background(), "test-profile", dir); err != nil {
		t.Fatal(err)
	}
	if err := next.Remove(context.Background(), "test-profile"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "versions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unused prior model remains: %d", len(entries))
	}
}

func TestSparsePartialDoesNotReceiveUnallocatedSpaceCredit(t *testing.T) {
	a := component("model", []byte("x"), "https://127.0.0.1/model")
	a.Bytes = 16 << 20
	c := catalogFor(a)
	s := testStore(t, c, nil, Options{HeadroomBytes: 1024, FreeBytes: func() (int64, error) { return (1 << 20) + 1024 + (1 << 20), nil }})
	d, err := s.staging.child(c.Profiles[0].identity(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	f, err := os.Create(filepath.Join(d.path, "model.part"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(15 << 20); err != nil {
		t.Fatal(err)
	}
	_ = f.Chmod(0600)
	_ = f.Close()
	b, _ := json.Marshal(partialState{URL: a.URL, SHA256: a.SHA256, Bytes: a.Bytes, ETag: `"immutable"`})
	if err := d.atomicWrite("model.meta", b); err != nil {
		t.Fatal(err)
	}
	plan, err := s.Space(context.Background(), "test-profile")
	if err != nil {
		t.Fatal(err)
	}
	if plan.AdditionalBytes < 14<<20 {
		t.Fatalf("sparse bytes treated as allocated: %+v", plan)
	}
}

func TestOfflineImportConsumesPartialWithoutSecondCopy(t *testing.T) {
	data := strings.Repeat("12345678", 1024)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"same"`)
		w.Header().Set("Content-Length", "8192")
		_, _ = io.WriteString(w, data[:2048])
	}))
	defer server.Close()
	c := catalogFor(component("model", []byte(data), server.URL))
	s := testStore(t, c, server, Options{})
	if err := s.Install(context.Background(), "test-profile", nil); err == nil {
		t.Fatal("truncated request accepted")
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "model.bin"), []byte(data), 0600)
	if err := s.Import(context.Background(), "test-profile", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.versions.path, c.Profiles[0].identity(), "model.part")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("offline second copy retained: %v", err)
	}
}

func TestWritableFinalDoesNotReceiveReuseCredit(t *testing.T) {
	c := catalogFor(component("model", []byte("abcdef"), ""))
	s := testStore(t, c, nil, Options{})
	d, err := s.staging.child(c.Profiles[0].identity(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	_ = os.WriteFile(filepath.Join(d.path, "model.bin"), []byte("abcdef"), 0600)
	if _, err := s.Space(context.Background(), "test-profile"); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("writable component credited: %v", err)
	}
}

func TestReadonlyFinishedPartialRecoversWithoutNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, "abcdef") }))
	defer server.Close()
	a := component("model", []byte("abcdef"), server.URL)
	c := catalogFor(a)
	s := testStore(t, c, server, Options{})
	d, err := s.staging.child(c.Profiles[0].identity(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	_ = os.WriteFile(filepath.Join(d.path, "model.part"), []byte("abcdef"), 0400)
	b, _ := json.Marshal(partialState{URL: a.URL, SHA256: a.SHA256, Bytes: a.Bytes, ETag: `"same"`})
	if err := d.atomicWrite("model.meta", b); err != nil {
		t.Fatal(err)
	}
	if err := s.Install(context.Background(), "test-profile", nil); err != nil {
		t.Fatalf("rename-boundary crash not recovered: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("finished readonly partial redownloaded")
	}
}
