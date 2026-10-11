package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/ingestion"
	"go.mau.fi/whatsmeow"
)

func TestDemoAcceptanceOwnsRealReservationsAndIndependentRoots(t *testing.T) {
	a := unixNativeTestApp(t)
	policy, err := a.store.Policy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		id   string
		kind archive.MessageKind
	}{{"acceptance-ocr", archive.MessageImage}, {"acceptance-transcript", archive.MessageVoice}} {
		event := demoAcceptanceEvent(fixture.id, fixture.kind, 1)
		if len(event.Media) != 1 || !strings.HasPrefix(string(event.Media[0].ProtocolReference), "wa-indexer/synthetic-demo-only/v1/") {
			t.Fatal("synthetic fixture has no explicit association reference")
		}
	}
	voice := demoAcceptanceEvent("acceptance-transcript", archive.MessageVoice, int64(len(demoAcceptanceVoice())))
	associated, err := a.store.ApplySourceEvent(context.Background(), voice, policy.Generation)
	if err != nil || len(associated.MediaIDs) != 1 {
		t.Fatalf("public synthetic media association missing: %v", err)
	}
	owner, err := prepareDemoAcceptance(context.Background(), a, policy.Generation)
	if owner != nil {
		t.Cleanup(func() {
			if err := owner.Close(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	budget, err := a.store.GetOriginalBudget(context.Background())
	if err != nil || !budget.Pressure || budget.MaximumBytes != archive.MinimumOriginalBudgetBytes || budget.ChargedBytes < 3*26214426 {
		t.Fatalf("actual pressure missing: %+v %v", budget, err)
	}
	for _, text := range []string{demoOCRCanary, demoTranscriptCanary} {
		page, err := a.Search(context.Background(), archive.SearchRequest{Query: text, Limit: 10})
		if err != nil || len(page.Hits) == 0 {
			t.Fatalf("independent synthetic root unavailable: %s %v", text, err)
		}
	}
	if err := owner.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	budget, err = a.store.GetOriginalBudget(context.Background())
	if err != nil || budget.Pressure {
		t.Fatalf("cleanup did not acknowledge physical reservations: %+v %v", budget, err)
	}
}

func TestDemoAcceptanceRefusesRealApplicationBeforeSourceWrites(t *testing.T) {
	a := &Application{}
	owner, err := prepareDemoAcceptance(context.Background(), a, 1)
	if owner != nil || err == nil {
		t.Fatal("real-mode fixture admission was allowed")
	}
}

func TestDemoAcceptanceConfigRefusesRealBeforeDataPreparation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-created")
	if a, err := New(context.Background(), Config{DataDir: path, DemoAcceptanceFixtures: true}); a != nil || err == nil {
		t.Fatal("real fixture flag admitted")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal touched data root: %v", err)
	}
}

type demoPendingDownload struct {
	failure bool
	closes  int
}

func (d *demoPendingDownload) File() whatsmeow.File                                    { return nil }
func (d *demoPendingDownload) Complete(context.Context, ingestion.VerifiedMedia) error { return nil }
func (d *demoPendingDownload) Close() error {
	d.closes++
	if d.failure {
		return errDemoAcceptance
	}
	return nil
}

func TestDemoAcceptanceCleanupRetainsFailedPhysicalOwnerForRetry(t *testing.T) {
	first, second := &demoPendingDownload{failure: true}, &demoPendingDownload{}
	owner := &demoAcceptanceOwner{downloads: []ingestion.DownloadAdmission{first, second}}
	if err := owner.Close(context.Background()); err == nil || len(owner.downloads) != 2 || second.closes != 0 {
		t.Fatal("cleanup forgot an unacknowledged physical owner")
	}
	first.failure = false
	if err := owner.Close(context.Background()); err != nil || len(owner.downloads) != 0 || first.closes != 2 || second.closes != 1 {
		t.Fatal("retained physical cleanup could not finish")
	}
}
