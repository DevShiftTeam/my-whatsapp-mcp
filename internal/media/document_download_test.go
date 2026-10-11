package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/diskspace"
)

func documentRequest(f *fakeRepository, data []byte) DownloadRequest {
	q := request(f, data)
	q.Kind = archive.MessageKind("document")
	return q
}

func TestDocumentUnknownLengthReservesOnlyDocumentCeiling(t *testing.T) {
	f := newRepository()
	s, _, physical := testStore(t, f, Options{})
	q := documentRequest(f, documentPDFFixture())
	q.DeclaredBytes = 0
	q.CiphertextMaximum = wireMaximum(25 << 20)
	download, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer download.Close()
	if f.charge(archive.BudgetOriginals) != wireMaximum(8<<20) || physical.held != wireMaximum(8<<20)+metadataGrowth {
		t.Fatal("document reservation escaped its kind-specific ciphertext budget")
	}
	if _, err := download.File().WriteAt([]byte{1}, wireMaximum(8<<20)); !errors.Is(err, ErrLimit) {
		t.Fatalf("document extent exceeded reservation: %v", err)
	}
	if err := download.Close(); err != nil {
		t.Fatal(err)
	}
	if f.charge(archive.BudgetOriginals) != 0 || physical.held != 0 {
		t.Fatal("document reservation leaked after close")
	}
}

func TestDocumentAdmissionDenialsDoNotRetainFilesOrCharges(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"declared size", ErrLimit},
		{"stale source", ErrSourceChanged},
		{"logical budget", ErrWaitingForDiskBudget},
		{"physical budget", ErrWaitingForDiskBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepository()
			physical := &physicalAdmission{}
			s, paths, _ := testStore(t, f, Options{Admission: physical})
			q := documentRequest(f, documentPDFFixture())
			switch tc.name {
			case "declared size":
				q.DeclaredBytes = (8 << 20) + 1
			case "stale source":
				q.Version.Revision++
			case "logical budget":
				f.quota[archive.BudgetOriginals] = 1
			case "physical budget":
				physical.err = diskspace.ErrInsufficientSpace
			}
			if download, err := s.AdmitDownload(context.Background(), q); !errors.Is(err, tc.want) {
				if download != nil {
					_ = download.Close()
				}
				t.Fatalf("denial %q: %v", tc.name, err)
			}
			if f.charge(archive.BudgetOriginals) != 0 || physical.held != 0 {
				t.Fatal("denied document left a quota charge")
			}
			entries, err := os.ReadDir(paths.MediaDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if ext := filepath.Ext(entry.Name()); ext == ".part" || ext == ".original" {
					t.Fatal("denied document left file bytes")
				}
			}
		})
	}
}

func TestDocumentCompletionRejectsChangedSourceAndReclaimsDownload(t *testing.T) {
	f := newRepository()
	s, _, physical := testStore(t, f, Options{})
	data := documentPDFFixture()
	q := documentRequest(f, data)
	download, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer download.Close()
	if _, err := download.File().Write(data); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.source.Version.Revision++
	f.mu.Unlock()
	if err := download.Complete(context.Background(), receipt(q, data)); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("changed source document published: %v", err)
	}
	if len(f.media) != 0 {
		t.Fatal("stale document became retained")
	}
	if err := download.Close(); err != nil {
		t.Fatal(err)
	}
	if f.charge(archive.BudgetOriginals) != 0 || physical.held != 0 {
		t.Fatal("stale document cleanup leaked its reservation")
	}
}

func TestDocumentOptionsAndSmallerConfiguredAdmissionCap(t *testing.T) {
	f := newRepository()
	s, _, _ := testStore(t, f, Options{DocumentMaximumBytes: 1024})
	q := documentRequest(f, documentPDFFixture())
	q.DeclaredBytes = 1025
	if download, err := s.AdmitDownload(context.Background(), q); !errors.Is(err, ErrLimit) {
		if download != nil {
			_ = download.Close()
		}
		t.Fatal("configured document cap ignored", err)
	}
	q.DeclaredBytes = 0
	q.CiphertextMaximum = wireMaximum(25 << 20)
	download, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer download.Close()
	if f.charge(archive.BudgetOriginals) != wireMaximum(1024) {
		t.Fatal("unknown document reserved beyond configured cap")
	}
	for _, invalid := range []int64{-1, (8 << 20) + 1} {
		parent, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		paths, err := appdata.Prepare(filepath.Join(parent, "owned"))
		if err != nil {
			t.Fatal(err)
		}
		store, err := Open(paths, newRepository(), Options{Admission: &physicalAdmission{}, DocumentMaximumBytes: invalid})
		if store != nil {
			_ = store.Close()
		}
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatal("invalid document option accepted", err)
		}
		if _, err := os.Stat(filepath.Join(paths.MediaDir, "media-owner.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid document option acquired ownership", err)
		}
	}
}

func TestDocumentCompletionAuthenticatesOpaqueBytesAndReceipt(t *testing.T) {
	for _, change := range []string{"owned bytes", "receipt hash", "receipt size"} {
		t.Run(change, func(t *testing.T) {
			f := newRepository()
			s, _, physical := testStore(t, f, Options{})
			data := []byte("opaque synthetic document")
			q := documentRequest(f, data)
			download, err := s.AdmitDownload(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			defer download.Close()
			if _, err := download.File().Write(data); err != nil {
				t.Fatal(err)
			}
			verified := receipt(q, data)
			switch change {
			case "owned bytes":
				if _, err := download.File().WriteAt([]byte("O"), 0); err != nil {
					t.Fatal(err)
				}
			case "receipt hash":
				verified.SHA256[0] ^= 1
			case "receipt size":
				verified.Bytes--
			}
			if err := download.Complete(context.Background(), verified); !errors.Is(err, ErrAuthentication) {
				t.Fatal("unauthenticated opaque document published", err)
			}
			if len(f.media) != 0 {
				t.Fatal("unauthenticated document became retained")
			}
			if err := download.Close(); err != nil {
				t.Fatal(err)
			}
			if f.charge(archive.BudgetOriginals) != 0 || physical.held != 0 {
				t.Fatal("unauthenticated document leaked reservation")
			}
		})
	}
}
