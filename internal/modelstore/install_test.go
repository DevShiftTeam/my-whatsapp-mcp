package modelstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func component(id string, data []byte, target string) Component {
	h := sha256.Sum256(data)
	return Component{ID: id, Role: id, Filename: id + ".bin", URL: target, SHA256: hex.EncodeToString(h[:]), Bytes: int64(len(data)), License: "test"}
}

func catalogFor(parts ...Component) Catalog {
	return Catalog{SchemaVersion: 1, AllowedHosts: []string{"127.0.0.1"}, Profiles: []Profile{{ID: "test-profile", Revision: "0123456789012345678901234567890123456789", Platform: "any", Selection: "candidate", License: "test", Notices: "THIRD_PARTY_NOTICES", Components: parts}}}
}

func testStore(t *testing.T, c Catalog, server *httptest.Server, options Options) *Store {
	t.Helper()
	if server != nil {
		options.Client = server.Client()
	}
	if options.FreeBytes == nil {
		options.FreeBytes = func() (int64, error) { return 32 << 30, nil }
	}
	s, err := Open(filepath.Join(t.TempDir(), "models"), c, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestHashMismatchNeverPublishes(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "wrong") }))
	defer server.Close()
	s := testStore(t, catalogFor(component("model", []byte("right"), server.URL)), server, Options{})
	if err := s.Install(context.Background(), "test-profile", nil); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Verify(context.Background(), "test-profile"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("active corrupt file: %v", err)
	}
}

func TestTruncatedOrOversizeDownload(t *testing.T) {
	for _, reply := range []string{"abc", "abcdefg"} {
		t.Run(reply, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, reply) }))
			defer server.Close()
			s := testStore(t, catalogFor(component("model", []byte("abcdef"), server.URL)), server, Options{})
			if err := s.Install(context.Background(), "test-profile", nil); !errors.Is(err, ErrSizeMismatch) {
				t.Fatalf("got %v", err)
			}
			if _, err := s.Verify(context.Background(), "test-profile"); !errors.Is(err, ErrNotInstalled) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestRedirectPolicy(t *testing.T) {
	var forbidden atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forbidden.Add(1); _, _ = io.WriteString(w, "abcdef") }))
	defer other.Close()
	otherURL, _ := url.Parse(other.URL)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+otherURL.Host, http.StatusFound)
	}))
	defer server.Close()
	s := testStore(t, catalogFor(component("model", []byte("abcdef"), server.URL)), server, Options{})
	if err := s.Install(context.Background(), "test-profile", nil); !errors.Is(err, ErrURLPolicy) {
		t.Fatalf("got %v", err)
	}
	if forbidden.Load() != 0 {
		t.Fatal("followed rejected redirect")
	}
}

type cdnFixtureTransport func(*http.Request) (*http.Response, error)

func (f cdnFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBuiltinCDNRedirectPolicy(t *testing.T) {
	// These exact HTTPS443 edges are documented by the primary provider:
	// https://huggingface.co/docs/hub/en/models-downloading
	for _, row := range []struct {
		name, target string
		allowed      bool
	}{
		{"aws", "https://us.aws.cdn.hf.co/model", true},
		{"aws443", "https://us.aws.cdn.hf.co:443/model", true},
		{"gcp", "https://us.gcp.cdn.hf.co/model", true},
		{"gcp443", "https://us.gcp.cdn.hf.co:443/model", true},
		{"http", "http://us.aws.cdn.hf.co/model", false},
		{"http443", "http://us.gcp.cdn.hf.co:443/model", false},
		{"aws8443", "https://us.aws.cdn.hf.co:8443/model", false},
		{"gcp8443", "https://us.gcp.cdn.hf.co:8443/model", false},
		{"suffix", "https://us.aws.cdn.hf.co.evil.example/model", false},
		{"subdomain", "https://evil.us.gcp.cdn.hf.co/model", false},
		{"userinfo", "https://fixture@us.aws.cdn.hf.co/model", false},
		{"fragment", "https://us.gcp.cdn.hf.co/model#fixture", false},
		{"generalAWS", "https://s3.us-east-1.amazonaws.com/model", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			var targetHits, mappedRequests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/start" {
					http.Redirect(w, r, row.target, http.StatusFound)
					return
				}
				targetHits.Add(1)
				_, _ = io.WriteString(w, "abcdef")
			}))
			defer server.Close()
			local, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			base := server.Client().Transport.(*http.Transport).Clone()
			base.Proxy = nil
			defer base.CloseIdleConnections()
			client := &http.Client{Transport: cdnFixtureTransport(func(request *http.Request) (*http.Response, error) {
				original := request
				if request.URL.Host != local.Host {
					// Logical provider authorities use only this owned TLS fixture;
					// its trusted certificate verification remains enabled. Rejected
					// redirects must stop before this transport is called at all.
					host, port := request.URL.Hostname(), request.URL.Port()
					if request.URL.Scheme != "https" || (host != "us.aws.cdn.hf.co" && host != "us.gcp.cdn.hf.co") || (port != "" && port != "443") {
						return nil, errors.New("unexpected fixture redirect reached transport")
					}
					mappedRequests.Add(1)
					request = request.Clone(request.Context())
					request.Host = original.URL.Host
					mappedURL := *request.URL
					mappedURL.Host = local.Host
					request.URL = &mappedURL
				}
				response, err := base.RoundTrip(request)
				if response != nil {
					response.Request = original
				}
				return response, err
			})}
			trusted, err := DefaultCatalog()
			if err != nil {
				t.Fatal(err)
			}
			catalog := catalogFor(component("model", []byte("abcdef"), server.URL+"/start"))
			catalog.AllowedHosts = append(catalog.AllowedHosts, trusted.AllowedHosts...)
			s := testStore(t, catalog, nil, Options{Client: client})
			err = s.Install(context.Background(), "test-profile", nil)
			if !row.allowed {
				if !errors.Is(err, ErrURLPolicy) || targetHits.Load() != 0 || mappedRequests.Load() != 0 {
					t.Fatalf("rejected redirect reached target: error=%v hits=%d mapped=%d", err, targetHits.Load(), mappedRequests.Load())
				}
				if _, err := s.Verify(context.Background(), "test-profile"); !errors.Is(err, ErrNotInstalled) {
					t.Fatalf("rejected redirect activated a profile: %v", err)
				}
				return
			}
			if err != nil || targetHits.Load() != 1 || mappedRequests.Load() != 1 {
				t.Fatalf("allowed pinned redirect failed: error=%v hits=%d mapped=%d", err, targetHits.Load(), mappedRequests.Load())
			}
			lease, err := s.Acquire(context.Background(), "test-profile")
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInsufficientDiskDoesNotStart(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
	defer server.Close()
	s := testStore(t, catalogFor(component("model", []byte("abcdef"), server.URL)), server, Options{FreeBytes: func() (int64, error) { return 1, nil }})
	err := s.Install(context.Background(), "test-profile", nil)
	var disk *SpaceError
	if !errors.As(err, &disk) || disk.AdditionalBytes <= 0 {
		t.Fatalf("got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("network started before disk admission")
	}
}

func TestPairedVisionArtifactsActivateTogether(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/model" {
			_, _ = io.WriteString(w, "model!")
		} else {
			_, _ = io.WriteString(w, "broken")
		}
	}))
	defer server.Close()
	s := testStore(t, catalogFor(component("model", []byte("model!"), server.URL+"/model"), component("projector", []byte("right!"), server.URL+"/projector")), server, Options{})
	if err := s.Install(context.Background(), "test-profile", nil); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("got %v", err)
	}
	if _, err := s.Verify(context.Background(), "test-profile"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("half pair active: %v", err)
	}
}

func TestOfflineSymlinkRejected(t *testing.T) {
	s := testStore(t, catalogFor(component("model", []byte("abcdef"), "")), nil, Options{})
	dir := t.TempDir()
	file := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(file, []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file, filepath.Join(dir, "model.bin")); err != nil {
		t.Fatal(err)
	}
	if err := s.Import(context.Background(), "test-profile", dir); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("got %v", err)
	}
}

func TestOfflineHardlinkRejected(t *testing.T) {
	s := testStore(t, catalogFor(component("model", []byte("abcdef"), "")), nil, Options{})
	dir := t.TempDir()
	file := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(file, []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(file, filepath.Join(dir, "model.bin")); err != nil {
		t.Fatal(err)
	}
	if err := s.Import(context.Background(), "test-profile", dir); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("got %v", err)
	}
}

func TestLeasePreventsRemoval(t *testing.T) {
	c := catalogFor(component("model", []byte("abcdef"), ""))
	s := testStore(t, c, nil, Options{})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model.bin"), []byte("abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Import(context.Background(), "test-profile", dir); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Acquire(context.Background(), "test-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(context.Background(), "test-profile"); !errors.Is(err, ErrActiveLease) {
		t.Fatalf("got %v", err)
	}
	f, err := lease.Open("model")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	_ = f.Close()
	if string(got) != "abcdef" {
		t.Fatal(string(got))
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(context.Background(), "test-profile"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(context.Background(), "test-profile"); !errors.Is(err, ErrNotInstalled) {
		t.Fatal(err)
	}
}

func TestCatalogRejectsArbitraryURLAndPath(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1/a", "https://evil.example/a", "https://127.0.0.1/a#x", "https://u:p@127.0.0.1/a"} {
		c := catalogFor(component("model", []byte("x"), target))
		if _, err := Open(filepath.Join(t.TempDir(), "models"), c, Options{}); !errors.Is(err, ErrInvalidCatalog) {
			t.Fatalf("%s: %v", target, err)
		}
	}
	for _, name := range []string{"../model", "/model", "a/b", ".", "a\\b"} {
		c := catalogFor(component("model", []byte("x"), ""))
		c.Profiles[0].Components[0].Filename = name
		if _, err := Open(filepath.Join(t.TempDir(), "models"), c, Options{}); !errors.Is(err, ErrInvalidCatalog) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestInterruptedInstallPreservesOldProfile(t *testing.T) {
	old := []byte("old-version")
	root := filepath.Join(t.TempDir(), "models")
	c := catalogFor(component("model", old, ""))
	s, err := Open(root, c, Options{FreeBytes: func() (int64, error) { return 32 << 30, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "model.bin"), old, 0600)
	if err := s.Import(context.Background(), "test-profile", dir); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "bad-version") }))
	defer server.Close()
	newCatalog := catalogFor(component("model", []byte("new-version"), server.URL))
	newStore, err := Open(root, newCatalog, Options{Client: server.Client(), FreeBytes: func() (int64, error) { return 32 << 30, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer newStore.Close()
	if err := newStore.Install(context.Background(), "test-profile", nil); !errors.Is(err, ErrHashMismatch) {
		t.Fatal(err)
	}
	if _, err := s.Verify(context.Background(), "test-profile"); err != nil {
		t.Fatalf("old good version lost: %v", err)
	}
}

func TestResumeVerifiedRange(t *testing.T) {
	data := strings.Repeat("12345678", 1024)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set("ETag", `"immutable"`)
		if n == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			_, _ = io.WriteString(w, data[:2048])
			return
		}
		if r.Header.Get("Range") != "bytes=2048-" || r.Header.Get("If-Range") != `"immutable"` {
			t.Errorf("unverified resume headers: %v", r.Header)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 2048-%d/%d", len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, data[2048:])
	}))
	defer server.Close()
	s := testStore(t, catalogFor(component("model", []byte(data), server.URL)), server, Options{})
	if err := s.Install(context.Background(), "test-profile", nil); err == nil {
		t.Fatal("first truncated download passed")
	}
	if err := s.Install(context.Background(), "test-profile", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(context.Background(), "test-profile"); err != nil {
		t.Fatal(err)
	}
}

func TestServerIgnoresRangeRestartsSafely(t *testing.T) {
	data := strings.Repeat("12345678", 1024)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"immutable"`)
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if requests.Add(1) == 1 {
			_, _ = io.WriteString(w, data[:2048])
			return
		}
		_, _ = io.WriteString(w, data)
	}))
	defer server.Close()
	s := testStore(t, catalogFor(component("model", []byte(data), server.URL)), server, Options{})
	if err := s.Install(context.Background(), "test-profile", nil); err == nil {
		t.Fatal("first request passed")
	}
	if err := s.Install(context.Background(), "test-profile", nil); err != nil {
		t.Fatal(err)
	}
}
