package media

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/diskspace"
)

func TestCompleteUsesOwnedFileRatherThanBorrowedReceiptReader(t *testing.T) {
	f := newRepository()
	s, _, _ := testStore(t, f, Options{})
	data := pngData(t)
	q := request(f, data)
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	mutated := bytes.Clone(data)
	mutated[0] ^= 1
	_, _ = d.File().Write(mutated)
	if err := d.Complete(context.Background(), receipt(q, data)); err == nil {
		t.Fatal("borrowed receipt masked corrupt owned bytes")
	}
	if len(f.media) != 0 {
		t.Fatal("corrupt original published")
	}
}
func TestDeclaredPlaintextMustMatchAuthenticatedCount(t *testing.T) {
	f := newRepository()
	s, _, _ := testStore(t, f, Options{})
	data := pngData(t)
	q := request(f, data)
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, _ = d.File().Write(data)
	r := receipt(q, data)
	r.Bytes--
	if err := d.Complete(context.Background(), r); !errors.Is(err, ErrAuthentication) {
		t.Fatal(err)
	}
	if _, err := d.File().WriteAt([]byte("late writer"), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatal("completion did not stop writes", err)
	}
}
func TestPhysicalAdmissionDenialCreatesNoPartialOrLeakedCharge(t *testing.T) {
	f := newRepository()
	p := &physicalAdmission{err: diskspace.ErrInsufficientSpace}
	s, paths, _ := testStore(t, f, Options{Admission: p})
	if _, err := s.AdmitDownload(context.Background(), request(f, pngData(t))); !errors.Is(err, ErrWaitingForDiskBudget) {
		t.Fatal(err)
	}
	if f.charge(archive.BudgetOriginals) != 0 || p.held != 0 {
		t.Fatal("denied reservation leaked")
	}
	entries, err := os.ReadDir(paths.MediaDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "media-owner.lock" {
			t.Fatalf("physical denial created entry %s", e.Name())
		}
	}
}
func TestStoreInstanceLeaseAndShutdownAdmission(t *testing.T) {
	f := newRepository()
	s, paths, p := testStore(t, f, Options{})
	if other, err := Open(paths, f, Options{Admission: p}); !errors.Is(err, ErrActiveUse) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatal("second media owner admitted", err)
	}
	d, err := s.AdmitDownload(context.Background(), request(f, pngData(t)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); !errors.Is(err, ErrActiveUse) {
		t.Fatal(err)
	}
	if _, err := s.AdmitDownload(context.Background(), request(f, pngData(t))); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
