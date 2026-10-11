package modelstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPinnedPublicHebrewAsset(t *testing.T) {
	if os.Getenv("WA_INDEXER_MODELSTORE_PUBLIC") != "1" {
		t.Skip("explicit small public-asset integration not enabled")
	}
	c, err := DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var profile Profile
	for _, p := range c.Profiles {
		if p.ID == "tesseract-fast" {
			profile = p
			break
		}
	}
	var heb Component
	for _, a := range profile.Components {
		if a.ID == "heb" {
			heb = a
			break
		}
	}
	// This isolated acquisition proves the pinned public transport and hash; it
	// does not activate an OCR worker or select a release profile.
	profile.ID = "tesseract-fast-public-verification"
	profile.Components = []Component{heb}
	c.Profiles = []Profile{profile}
	s, err := Open(filepath.Join(t.TempDir(), "models"), c, Options{MaxDownloadTime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.Install(ctx, profile.ID, nil); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Acquire(ctx, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := lease.Open("heb")
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if n != 961404 {
		t.Fatalf("size %d", n)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(ctx, profile.ID); err != nil {
		t.Fatal(err)
	}
	t.Logf("pinned public HTTPS asset verified by Store.Install+Acquire: bytes=%d sha256=%s selection=%s", n, heb.SHA256, profile.Selection)
}

type testReservation struct {
	released *atomic.Int32
	err      error
}

func (r testReservation) Release() error { r.released.Add(1); return r.err }

func TestSharedSpaceAdmissionPrecedesNetworkAndReleases(t *testing.T) {
	var calls, releases atomic.Int32
	denied := errors.New("shared physical admission denied")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, "abcdef") }))
	defer server.Close()
	c := catalogFor(component("model", []byte("abcdef"), server.URL))
	s := testStore(t, c, server, Options{ReserveSpace: func(context.Context, int64) (SpaceReservation, error) { return nil, denied }})
	if err := s.Install(context.Background(), "test-profile", nil); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("network before shared admission")
	}
	s2 := testStore(t, c, server, Options{ReserveSpace: func(ctx context.Context, growth int64) (SpaceReservation, error) {
		if growth != 6+(1<<20) {
			t.Errorf("wrong shared reservation %d", growth)
		}
		return testReservation{&releases, nil}, nil
	}})
	if err := s2.Install(context.Background(), "test-profile", nil); err != nil {
		t.Fatal(err)
	}
	if releases.Load() != 1 {
		t.Fatal("reservation leaked")
	}
}

func TestLeaseDescriptorCloseOnExec(t *testing.T) {
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
	f, err := lease.Open("model")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("descriptor exec leak flags=%d err=%v", flags, err)
	}
}
