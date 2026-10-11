package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/diskspace"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
)

type fakeRepository struct {
	mu            sync.Mutex
	source        archive.SourceRef
	reservations  map[archive.ArtifactID]archive.MediaReservation
	artifacts     map[archive.ArtifactID]archive.MediaArtifact
	media         map[archive.MediaID]archive.ArtifactID
	denied        map[archive.MediaID]bool
	quota         map[archive.BudgetKind]int64
	sequence      int
	processed     bool
	onCleanup     func()
	publishErr    error
	inspectErr    error
	cleanupErr    error
	rejectPublish bool
}

func newRepository() *fakeRepository {
	return &fakeRepository{source: archive.SourceRef{Account: "account", ContentHash: "source-hash", Version: archive.SourceVersion{MessageID: "source-id", Revision: 1, PolicyGeneration: 1}}, reservations: map[archive.ArtifactID]archive.MediaReservation{}, artifacts: map[archive.ArtifactID]archive.MediaArtifact{}, media: map[archive.MediaID]archive.ArtifactID{}, denied: map[archive.MediaID]bool{}, quota: map[archive.BudgetKind]int64{archive.BudgetOriginals: 2 << 30, archive.BudgetDecoded: 128 << 20}}
}
func (f *fakeRepository) CurrentSource(context.Context, archive.SourceKey) (archive.SourceRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.source, nil
}
func (f *fakeRepository) AdmitMedia(_ context.Context, q archive.MediaAdmissionRequest) (archive.MediaReservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var used int64
	for _, a := range f.artifacts {
		if a.Budget == q.Budget {
			used += a.ReservedBytes
		}
	}
	if used+q.MaximumBytes > f.quota[q.Budget] {
		return archive.MediaReservation{}, archive.ErrQuotaExceeded
	}
	f.sequence++
	id := archive.ArtifactID(fmt.Sprintf("%032x", f.sequence))
	r := archive.MediaReservation{ID: archive.ReservationID("reservation-" + string(id)), ArtifactID: id, MediaID: q.MediaID, Source: q.Source, Budget: q.Budget, MaximumBytes: q.MaximumBytes, LeaseID: archive.LeaseID("lease-" + string(id)), LeaseUntil: time.Now().Add(q.TTL)}
	f.reservations[id] = r
	f.artifacts[id] = archive.MediaArtifact{ID: id, MediaID: r.MediaID, Source: r.Source, ReservationID: r.ID, CleanupLease: r.LeaseID, Budget: r.Budget, ReservedBytes: r.MaximumBytes, State: "reserved"}
	return r, nil
}
func (f *fakeRepository) RenewMedia(_ context.Context, r archive.MediaReservation, ttl time.Duration) (time.Time, error) {
	return time.Now().Add(ttl), nil
}
func (f *fakeRepository) PublishOriginal(_ context.Context, p archive.OriginalPublication) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rejectPublish {
		return false, f.publishErr
	}
	a := f.artifacts[p.Reservation.ArtifactID]
	a.State = "retained"
	a.Bytes = p.Bytes
	a.SHA256 = p.SHA256
	a.ReservedBytes = p.Bytes
	f.artifacts[a.ID] = a
	f.media[p.Reservation.MediaID] = a.ID
	return true, f.publishErr
}
func (f *fakeRepository) EvictionCandidates(context.Context, int) ([]archive.MediaID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.processed {
		return nil, nil
	}
	var ids []archive.MediaID
	for id := range f.media {
		ids = append(ids, id)
	}
	return ids, nil
}
func (f *fakeRepository) BeginEviction(_ context.Context, id archive.MediaID) (archive.MediaArtifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.processed {
		return archive.MediaArtifact{}, archive.ErrNoWork
	}
	a := f.artifacts[f.media[id]]
	a.State = "evicting"
	a.CleanupLease = "cleanup-lease"
	f.artifacts[a.ID] = a
	return a, nil
}
func (f *fakeRepository) FinishMediaCleanup(_ context.Context, c archive.MediaCleanup) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cleanupErr != nil {
		return f.cleanupErr
	}
	a := f.artifacts[c.ArtifactID]
	if a.State == "retained" {
		return archive.ErrNoWork
	}
	if f.onCleanup != nil {
		f.onCleanup()
	}
	a.ReservedBytes = 0
	f.artifacts[a.ID] = a
	return nil
}
func (f *fakeRepository) ReconcileMedia(context.Context, int) ([]archive.MediaArtifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []archive.MediaArtifact
	for _, a := range f.artifacts {
		if a.State != "retained" && a.ReservedBytes > 0 {
			all = append(all, a)
		}
	}
	return all, nil
}
func (f *fakeRepository) InspectMediaReservation(_ context.Context, r archive.MediaReservation) (archive.MediaArtifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspectErr != nil {
		return archive.MediaArtifact{}, f.inspectErr
	}
	a, ok := f.artifacts[r.ArtifactID]
	if !ok {
		return a, archive.ErrNoWork
	}
	return a, nil
}
func (f *fakeRepository) AuthorizeOriginal(_ context.Context, s archive.ReadScope, id archive.MediaID) (archive.MediaArtifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.artifacts[f.media[id]]
	if f.denied[id] || s.Account != f.source.Account || a.State != "retained" {
		return archive.MediaArtifact{}, archive.ErrSourceUnavailable
	}
	a.MediaID = id
	return a, nil
}
func (f *fakeRepository) charge(b archive.BudgetKind) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, a := range f.artifacts {
		if a.Budget == b {
			n += a.ReservedBytes
		}
	}
	return n
}

type physicalAdmission struct {
	mu         sync.Mutex
	held       int64
	err        error
	releaseErr error
}
type physicalLease struct {
	admission *physicalAdmission
	bytes     int64
	closed    bool
}

func (p *physicalAdmission) Reserve(_ context.Context, r diskspace.Request) (diskspace.Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	p.held += r.MaximumGrowthBytes
	return &physicalLease{admission: p, bytes: r.MaximumGrowthBytes}, nil
}
func (l *physicalLease) Release() error {
	l.admission.mu.Lock()
	defer l.admission.mu.Unlock()
	if l.admission.releaseErr != nil {
		return l.admission.releaseErr
	}
	if !l.closed {
		l.closed = true
		l.admission.held -= l.bytes
	}
	return nil
}
func pngData(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func request(f *fakeRepository, data []byte) DownloadRequest {
	return DownloadRequest{Key: archive.SourceKey{Account: "account", Chat: "chat", Sender: "sender", Message: "message"}, Version: f.source.Version, MediaID: "media-id", Kind: archive.MessageImage, DeclaredBytes: int64(len(data)), ProtocolSHA256: sha256.Sum256(data)}
}
func receipt(q DownloadRequest, data []byte) ingestion.VerifiedMedia {
	return ingestion.VerifiedMedia{Key: q.Key, Version: q.Version, Kind: q.Kind, SHA256: sha256.Sum256(data), Bytes: int64(len(data)), Reader: bytes.NewReader(data)}
}
func testStore(t *testing.T, f *fakeRepository, o Options) (*Store, appdata.Paths, *physicalAdmission) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths, err := appdata.Prepare(filepath.Join(parent, "owned"))
	if err != nil {
		t.Fatal(err)
	}
	p := &physicalAdmission{}
	if o.Admission == nil {
		o.Admission = p
	}
	s, err := Open(paths, f, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, paths, p
}

func TestExpectedDeviceMatchesSharedController(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths, err := appdata.Prepare(filepath.Join(parent, "owned"))
	if err != nil {
		t.Fatal(err)
	}
	broker, err := diskspace.New(paths.Root, diskspace.Options{HeadroomBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	snapshot, err := broker.Snapshot()
	if err != nil || snapshot.Device == 0 {
		t.Fatal("shared controller device unavailable", err)
	}
	s, err := Open(paths, newRepository(), Options{Admission: broker, ExpectedDevice: snapshot.Device})
	if err != nil {
		t.Fatal("matching retained media filesystem rejected", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExpectedDeviceMismatchCreatesNoMediaOwnership(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths, err := appdata.Prepare(filepath.Join(parent, "owned"))
	if err != nil {
		t.Fatal(err)
	}
	broker, err := diskspace.New(paths.Root, diskspace.Options{HeadroomBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	snapshot, err := broker.Snapshot()
	if err != nil || snapshot.Device == 0 {
		t.Fatal("shared controller device unavailable", err)
	}
	if s, err := Open(paths, newRepository(), Options{Admission: broker, ExpectedDevice: snapshot.Device + 1}); !errors.Is(err, ErrFilesystemMismatch) {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("foreign media device accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.MediaDir, "media-owner.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("filesystem mismatch acquired/wrote media ownership", err)
	}
	after, err := broker.Snapshot()
	if err != nil || after.ActiveLeases != 0 {
		t.Fatal("filesystem mismatch admitted growth", err)
	}
}

func TestReservationsCountInflight(t *testing.T) {
	f := newRepository()
	f.quota[archive.BudgetOriginals] = 120
	s, paths, p := testStore(t, f, Options{})
	q := request(f, pngData(t))
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if f.charge(archive.BudgetOriginals) <= q.DeclaredBytes || p.held <= q.DeclaredBytes {
		t.Fatal("ciphertext padding not reserved")
	}
	q.MediaID = "second-media"
	if _, err := s.AdmitDownload(context.Background(), q); !errors.Is(err, ErrWaitingForDiskBudget) {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(paths.MediaDir)
	parts := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".part" {
			parts++
		}
	}
	if parts != 1 {
		t.Fatalf("denied download created bytes: %d", parts)
	}
}
func TestUnknownLengthReservesMaximum(t *testing.T) {
	f := newRepository()
	s, _, _ := testStore(t, f, Options{ImageMaximumBytes: 256})
	q := request(f, pngData(t))
	q.DeclaredBytes = 0
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if f.charge(archive.BudgetOriginals) != 282 {
		t.Fatalf("unknown length charge %d", f.charge(archive.BudgetOriginals))
	}
	if _, err := d.File().Write(make([]byte, 283)); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}
func TestUnknownConnectorCeilingCannotExpandKindLimit(t *testing.T) {
	f := newRepository()
	s, _, _ := testStore(t, f, Options{ImageMaximumBytes: 256})
	q := request(f, pngData(t))
	q.DeclaredBytes = 0
	q.CiphertextMaximum = wireMaximum(25 << 20)
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if f.charge(archive.BudgetOriginals) != wireMaximum(256) {
		t.Fatal("generic limit expanded image reservation")
	}
	if _, err := d.File().Write(make([]byte, wireMaximum(256)+1)); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}
func TestCancelledDownloadReclaimsBytesAfterRemoval(t *testing.T) {
	f := newRepository()
	s, paths, p := testStore(t, f, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	q := request(f, pngData(t))
	d, err := s.AdmitDownload(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.File().Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	f.onCleanup = func() {
		entries, _ := os.ReadDir(paths.MediaDir)
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".part" || filepath.Ext(e.Name()) == ".original" {
				t.Error("quota freed before file removal")
			}
		}
		if p.held == 0 {
			t.Error("physical lease released before cleanup ack")
		}
	}
	cancel()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if f.charge(archive.BudgetOriginals) != 0 || p.held != 0 {
		t.Fatal("reservation leaked")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestCompleteRetainsSameAuthenticatedFile(t *testing.T) {
	f := newRepository()
	s, _, _ := testStore(t, f, Options{})
	data := pngData(t)
	q := request(f, data)
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := d.File().Stat()
	if _, err := d.File().Write(data); err != nil {
		t.Fatal(err)
	}
	if err := d.Complete(context.Background(), receipt(q, data)); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := s.OpenOriginal(context.Background(), archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: 1}, q.MediaID)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	file, err := original.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	after, _ := file.Stat()
	if !os.SameFile(before, after) {
		t.Fatal("original copied outside reservation")
	}
	got, _ := io.ReadAll(file)
	if !bytes.Equal(got, data) {
		t.Fatal("wrong retained bytes")
	}
	hash := sha256.Sum256(data)
	if original.Metadata().SHA256 != hex.EncodeToString(hash[:]) || original.Metadata().Format != "png" {
		t.Fatal("wrong verified metadata")
	}
}
func TestStaleSourceNeverPublishes(t *testing.T) {
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
	f.mu.Lock()
	f.source.Version.Revision++
	f.mu.Unlock()
	if err := d.Complete(context.Background(), receipt(q, data)); !errors.Is(err, ErrSourceChanged) {
		t.Fatal(err)
	}
	if len(f.media) != 0 {
		t.Fatal("stale source published")
	}
}
func TestPublicationErrorAfterCommitPreservesOriginal(t *testing.T) {
	f := newRepository()
	f.publishErr = context.DeadlineExceeded
	s, _, _ := testStore(t, f, Options{})
	data := pngData(t)
	q := request(f, data)
	d, err := s.AdmitDownload(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = d.File().Write(data)
	_ = d.Complete(context.Background(), receipt(q, data))
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := s.OpenOriginal(context.Background(), archive.ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: 1}, q.MediaID)
	if err != nil {
		t.Fatalf("ambiguous committed original lost: %v", err)
	}
	_ = original.Close()
}
func TestDecodedPoolIndependentAndBounded(t *testing.T) {
	f := newRepository()
	f.quota[archive.BudgetDecoded] = 32
	s, _, _ := testStore(t, f, Options{})
	d, err := s.ReserveDecoded(context.Background(), f.source, "media-id", 32)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if f.charge(archive.BudgetDecoded) != 32 || f.charge(archive.BudgetOriginals) != 0 {
		t.Fatal("pools mixed")
	}
	if _, err := d.File().Write(make([]byte, 33)); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if _, err := s.ReserveDecoded(context.Background(), f.source, "media-id", 1); !errors.Is(err, archive.ErrQuotaExceeded) {
		t.Fatal(err)
	}
}
