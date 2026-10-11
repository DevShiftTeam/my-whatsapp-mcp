package modelstore

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func insecureTestTransport() *http.Transport {
	return &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
} // TLS fixture only.

func TestStrictCatalogJSON(t *testing.T) {
	b, _ := json.Marshal(catalogFor(component("model", []byte("x"), "")))
	if _, err := ParseCatalog(append(b, []byte(" junk")...)); !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("trailing junk accepted: %v", err)
	}
}

func TestPrivateRootAncestorSymlinkRejected(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(base, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(link, "models"), catalogFor(component("model", []byte("x"), "")), Options{}); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("ancestor symlink: %v", err)
	}
}

func TestOfflineSourceAncestorSymlinkRejected(t *testing.T) {
	s := testStore(t, catalogFor(component("model", []byte("abcdef"), "")), nil, Options{})
	base := t.TempDir()
	outside := t.TempDir()
	_ = os.Mkdir(filepath.Join(outside, "source"), 0700)
	_ = os.WriteFile(filepath.Join(outside, "source", "model.bin"), []byte("abcdef"), 0600)
	if err := os.Symlink(outside, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if err := s.Import(context.Background(), "test-profile", filepath.Join(base, "link", "source")); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("ancestor symlink: %v", err)
	}
}

func TestAcquiredDescriptorSurvivesPathReplacement(t *testing.T) {
	s := testStore(t, catalogFor(component("model", []byte("abcdef"), "")), nil, Options{})
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "model.bin"), []byte("abcdef"), 0600)
	if err := s.Import(context.Background(), "test-profile", dir); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Acquire(context.Background(), "test-profile")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	path := lease.Profile().Components[0].Path
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(path, []byte("wrong!"), 0400)
	f, err := lease.Open("model")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	if string(b) != "abcdef" {
		t.Fatalf("reopened replacement: %q", b)
	}
	if _, err := s.Acquire(context.Background(), "test-profile"); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("corrupt file admitted: %v", err)
	}
}

func TestInstallRepairsCorruptVersion(t *testing.T) {
	data := []byte("abcdef")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }))
	defer server.Close()
	s := testStore(t, catalogFor(component("model", data, server.URL)), server, Options{})
	if err := s.Install(context.Background(), "test-profile", nil); err != nil {
		t.Fatal(err)
	}
	p, err := s.Verify(context.Background(), "test-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.Components[0].Path, 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(p.Components[0].Path, []byte("broken"), 0400)
	_ = os.Chmod(p.Components[0].Path, 0400)
	if err := s.Install(context.Background(), "test-profile", nil); err != nil {
		t.Fatalf("corrupt version cannot recover: %v", err)
	}
	if _, err := s.Verify(context.Background(), "test-profile"); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogPartFilenameCollisionRejected(t *testing.T) {
	c := catalogFor(component("model", []byte("x"), ""), component("tokenizer", []byte("y"), ""))
	c.Profiles[0].Components[1].Filename = "model.part"
	if _, err := Open(filepath.Join(t.TempDir(), "models"), c, Options{}); !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("partial name collision allowed: %v", err)
	}
}

func TestCustomClientHonorsDownloadDeadline(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	s := testStore(t, catalogFor(component("model", []byte("abcdef"), server.URL)), server, Options{MaxDownloadTime: 25 * time.Millisecond})
	start := time.Now()
	if err := s.Install(context.Background(), "test-profile", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
	if time.Since(start) > time.Second/2 {
		t.Fatal("deadline not enforced")
	}
}

func TestHeadroomOverflowRejected(t *testing.T) {
	if s, err := Open(filepath.Join(t.TempDir(), "models"), catalogFor(component("model", []byte("x"), "")), Options{HeadroomBytes: 1<<63 - 1}); err == nil {
		_ = s.Close()
		t.Fatal("overflow headroom allowed")
	}
}

func TestFIFORejectedWithoutBlocking(t *testing.T) {
	s := testStore(t, catalogFor(component("model", []byte("x"), "")), nil, Options{})
	if err := unix.Mkfifo(filepath.Join(s.root.path, "test-profile.active"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Verify(context.Background(), "test-profile"); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnsafeFile) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		// Unblock the rejected implementation so test cleanup cannot deadlock.
		fd, _ := unix.Open(filepath.Join(s.root.path, "test-profile.active"), unix.O_WRONLY|unix.O_NONBLOCK, 0)
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		<-done
		t.Fatal("FIFO opened synchronously before type validation")
	}
}

func TestCrossProcessLeasePreventsRemoval(t *testing.T) {
	if root := os.Getenv("WA_MODELSTORE_LEASE_HELPER"); root != "" {
		s, err := Open(root, catalogFor(component("model", []byte("abcdef"), "")), Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := s.Remove(context.Background(), "test-profile"); !errors.Is(err, ErrActiveLease) {
			t.Fatalf("crossprocess removed: %v", err)
		}
		return
	}
	s := testStore(t, catalogFor(component("model", []byte("abcdef"), "")), nil, Options{})
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "model.bin"), []byte("abcdef"), 0600)
	if err := s.Import(context.Background(), "test-profile", dir); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Acquire(context.Background(), "test-profile")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrossProcessLeasePreventsRemoval$")
	cmd.Env = append(os.Environ(), "WA_MODELSTORE_LEASE_HELPER="+s.root.path)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
}

func TestResumeRejectsBadRangeAndChangedETag(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "range", true: "etag"}[changed], func(t *testing.T) {
			data := strings.Repeat("12345678", 1024)
			first := true
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"same"`)
				if first {
					first = false
					w.Header().Set("Content-Length", "8192")
					_, _ = io.WriteString(w, data[:2048])
					return
				}
				w.Header().Set("Content-Range", "bytes 2048-8191/8193")
				if changed {
					w.Header().Set("ETag", `"changed"`)
					w.Header().Set("Content-Range", "bytes 2048-8191/8192")
				}
				w.WriteHeader(206)
				_, _ = io.WriteString(w, data[2048:])
			}))
			defer server.Close()
			s := testStore(t, catalogFor(component("model", []byte(data), server.URL)), server, Options{})
			_ = s.Install(context.Background(), "test-profile", nil)
			if err := s.Install(context.Background(), "test-profile", nil); !errors.Is(err, ErrRange) {
				t.Fatalf("unsafe resume: %v", err)
			}
		})
	}
}

func TestCrashBeforeActivationIsInertAndRecoverable(t *testing.T) {
	if root := os.Getenv("WA_MODELSTORE_CRASH_HELPER"); root != "" {
		s, err := Open(root, catalogFor(component("model", []byte("abcdef"), os.Getenv("WA_MODELSTORE_CRASH_URL"))), Options{Client: &http.Client{Transport: insecureTestTransport()}, FreeBytes: func() (int64, error) { return 32 << 30, nil }})
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Install(context.Background(), "test-profile", func(p Progress) {
			if p.Phase == "activating" {
				os.Exit(23)
			}
		})
		t.Fatal("did not crash")
	}
	data := []byte("abcdef")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(data) }))
	defer server.Close()
	s := testStore(t, catalogFor(component("model", data, server.URL)), server, Options{})
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashBeforeActivationIsInertAndRecoverable$")
	cmd.Env = append(os.Environ(), "WA_MODELSTORE_CRASH_HELPER="+s.root.path, "WA_MODELSTORE_CRASH_URL="+server.URL)
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("crash helper: %v", err)
	}
	if _, err := s.Verify(context.Background(), "test-profile"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("orphan active: %v", err)
	}
	if err := s.Install(context.Background(), "test-profile", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(context.Background(), "test-profile"); err != nil {
		t.Fatal(err)
	}
}
