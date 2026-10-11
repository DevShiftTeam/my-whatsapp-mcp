package app

import (
	"context"
	"errors"
	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/media"
	"testing"
	"time"
)

type primaryOriginalFixture struct {
	metadata     media.OriginalMetadata
	busy, closed bool
	calls        int
}

func (o *primaryOriginalFixture) Metadata() media.OriginalMetadata { return o.metadata }
func (o *primaryOriginalFixture) Close() error {
	o.calls++
	if o.busy {
		return media.ErrActiveUse
	}
	o.closed = true
	return nil
}

type primaryStoreFixture struct {
	archive.ExtractionStore
	original *primaryOriginalFixture
	commits  int
	retries  []archive.JobRetry
	retryErr error
}

func (s *primaryStoreFixture) CommitExtraction(_ context.Context, result archive.ExtractionResult) (bool, error) {
	if !s.original.closed {
		return false, errors.New("commit_before_physical_close")
	}
	s.commits++
	return true, nil
}
func (s *primaryStoreFixture) RenewExtraction(context.Context, archive.JobLease, time.Duration) (time.Time, error) {
	return time.Now().Add(time.Minute), nil
}
func (s *primaryStoreFixture) RetryExtraction(_ context.Context, request archive.JobRetry) error {
	if !s.original.closed {
		return errors.New("retry_before_physical_close")
	}
	s.retries = append(s.retries, request)
	return s.retryErr
}
func primaryFixture(t *testing.T) (*primaryMediaJobs, *primaryStoreFixture, *primaryOriginalFixture, archive.ExtractionWork) {
	t.Helper()
	work := archive.ExtractionWork{Lease: archive.JobLease{JobID: "job", LeaseID: "lease"}, MediaID: "media", ArtifactID: "artifact", OriginalSHA256: "verified-sha", ContentHash: "content", Source: archive.SourceRef{Account: "account"}}
	original := &primaryOriginalFixture{metadata: media.OriginalMetadata{ArtifactID: work.ArtifactID, SHA256: work.OriginalSHA256, Source: work.Source, MediaID: work.MediaID}}
	store := &primaryStoreFixture{original: original}
	runner := &primaryMediaJobs{store: store, cancel: func() {}, done: make(chan struct{}), open: func(context.Context, archive.ReadScope, archive.MediaID) (primaryOriginal, error) {
		return original, nil
	}}
	close(runner.done)
	runner.produce = func(context.Context, archive.ExtractionWork, primaryOriginal) (archive.ExtractionResult, error) {
		return archive.ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: archive.ExtractionEmpty}, nil
	}
	return runner, store, original, work
}
func TestPrimaryInputIdentityIsCheckedBeforeNative(t *testing.T) {
	for _, field := range []string{"artifact", "sha", "source", "media"} {
		t.Run(field, func(t *testing.T) {
			runner, store, original, work := primaryFixture(t)
			switch field {
			case "artifact":
				original.metadata.ArtifactID = "other"
			case "sha":
				original.metadata.SHA256 = "other"
			case "source":
				original.metadata.Source.Account = "other"
			case "media":
				original.metadata.MediaID = "other"
			}
			launched := false
			runner.produce = func(context.Context, archive.ExtractionWork, primaryOriginal) (archive.ExtractionResult, error) {
				launched = true
				return archive.ExtractionResult{}, nil
			}
			if err := runner.extract(context.Background(), work); !errors.Is(err, media.ErrInvalidRequest) {
				t.Fatal(err)
			}
			if launched || store.commits != 0 || !original.closed {
				t.Fatal("mismatched physical input reached native or publication")
			}
		})
	}
}
func TestPrimarySuccessPublishesOnlyAfterPhysicalClose(t *testing.T) {
	runner, store, original, work := primaryFixture(t)
	if err := runner.extract(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if !original.closed || store.commits != 1 || len(runner.pending) != 0 {
		t.Fatal("clean result not published at exact boundary")
	}
}

type primaryExtractorFixture struct{ ready bool }

func (e *primaryExtractorFixture) Close(context.Context) error {
	if !e.ready {
		return media.ErrCleanup
	}
	return nil
}
func TestPrimaryCleanupRetainsInputPinRenewalUntilEveryAcknowledgement(t *testing.T) {
	runner, store, original, work := primaryFixture(t)
	original.busy = true
	runner.produce = func(context.Context, archive.ExtractionWork, primaryOriginal) (archive.ExtractionResult, error) {
		return archive.ExtractionResult{}, media.ErrCleanup
	}
	if err := runner.extract(context.Background(), work); !errors.Is(err, media.ErrCleanup) {
		t.Fatal(err)
	}
	if original.calls != 0 || store.commits != 0 || len(runner.pending) != 1 {
		t.Fatal("early native return settled input")
	}
	renewed := runner.pending[0].renewed
	select {
	case <-renewed:
		t.Fatal("lease renewal stopped before physical cleanup")
	default:
	}
	extractor := &primaryExtractorFixture{}
	resources := &resources{primaryJobs: runner, extraction: extractor}
	if err := resources.close(context.Background()); !errors.Is(err, media.ErrCleanup) {
		t.Fatal(err)
	}
	if original.calls != 0 || len(store.retries) != 0 {
		t.Fatal("unacknowledged native cleanup released input pin")
	}
	extractor.ready = true
	original.busy = false
	store.retryErr = errors.New("ack_uncertain")
	if err := resources.close(context.Background()); err == nil {
		t.Fatal("uncertain durable cleanup acknowledged")
	}
	if len(runner.pending) != 1 {
		t.Fatal("old exact lease was forgotten")
	}
	select {
	case <-renewed:
		t.Fatal("uncertain durable cleanup stopped renewal")
	default:
	}
	store.retryErr = nil
	if err := resources.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-renewed:
	default:
		t.Fatal("renewal owner did not join")
	}
	if len(store.retries) != 2 || store.retries[1].Lease != work.Lease || store.retries[1].Terminal || store.retries[1].Code != "cancelled" {
		t.Fatal("cleanup used a different lease or terminal failure")
	}
}
