package media

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"testing"
)

type ownerLeaseRepository struct {
	*fakeRepository
	admissions []archive.MediaAdmissionRequest
}

func (r *ownerLeaseRepository) AdmitMedia(ctx context.Context, q archive.MediaAdmissionRequest) (archive.MediaReservation, error) {
	r.admissions = append(r.admissions, q)
	if len(r.admissions) == 1 {
		return archive.MediaReservation{}, archive.ErrQuotaExceeded
	}
	return r.fakeRepository.AdmitMedia(ctx, q)
}
func (r *ownerLeaseRepository) EvictionCandidates(context.Context, int) ([]archive.MediaID, error) {
	return []archive.MediaID{"already-cleaned"}, nil
}

func TestOwnerVoiceLeaseSurvivesMediaAdapterQuotaRetry(t *testing.T) {
	f := newRepository()
	s, _, _ := testStore(t, f, Options{})
	wrapped := &ownerLeaseRepository{fakeRepository: f}
	s.repo = wrapped
	q := request(f, pngData(t))
	owner := archive.OriginalDownloadLease{IntentID: "new-intent", MediaID: q.MediaID, LeaseID: "owner-lease"}
	admission, err := s.MediaAdmitter(context.Background(), ingestion.MediaDownloadRequest{Key: q.Key, Version: q.Version, MediaID: q.MediaID, Kind: q.Kind, DeclaredBytes: q.DeclaredBytes, ProtocolSHA256: q.ProtocolSHA256, OwnerDownloadLease: owner})
	if err != nil {
		t.Fatal(err)
	}
	defer admission.Close()
	if len(wrapped.admissions) != 2 {
		t.Fatalf("expected quota retry: %d", len(wrapped.admissions))
	}
	for _, attempt := range wrapped.admissions {
		if attempt.OwnerDownloadLease != owner {
			t.Fatal("quota retry lost exact owner lease")
		}
	}
}
