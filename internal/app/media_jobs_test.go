package app

import (
	"context"
	"errors"
	"testing"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/media"
)

type trackedOriginal struct {
	closed int
	busy   bool
}

func (o *trackedOriginal) Close() error {
	o.closed++
	if o.busy {
		return media.ErrActiveUse
	}
	return nil
}
func TestExtractionEarlyCleanupRetainsOriginalWithoutPublishing(t *testing.T) {
	original := &trackedOriginal{busy: true}
	runner := &mediaJobs{
		open: func(context.Context, archive.ReadScope, archive.MediaID) (originalOwner, error) { return original, nil },
		extract: func(context.Context, archive.Job, originalOwner) ([]archive.Document, error) {
			return nil, media.ErrCleanup
		},
	}
	err := runner.extractJob(context.Background(), archive.Job{MediaID: "media", Sources: []archive.SourceRef{{Account: "account"}}})
	if !errors.Is(err, media.ErrCleanup) {
		t.Fatal(err)
	}
	if len(runner.pending) != 1 || original.closed != 0 {
		t.Fatal("native-owned original was prematurely closed")
	}
	if err = runner.closeOriginals(); err == nil {
		t.Fatal("busy original cleanup acknowledged")
	}
	original.busy = false
	if err = runner.closeOriginals(); err != nil {
		t.Fatal(err)
	}
	if len(runner.pending) != 0 {
		t.Fatal("clean original owner was retained")
	}
}
