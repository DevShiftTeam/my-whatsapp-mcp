package app

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"time"
)

var errExtractionRecovery = errors.New("extraction_recovery_pending")

type extractionRecoveryPort interface {
	archive.ExtractionRecoveryStore
	RetryExtraction(context.Context, archive.JobRetry) error
}
type extractionRecovery struct {
	account  string
	after    archive.JobID
	complete bool
}

// Only startup under the newly acquired SAME instance lease calls this port.
// Exclusive acquisition proves prior trusted worker holders are gone; TTL never
// provides physical cleanup authority. Each step is a bounded metadata page.
func (r *extractionRecovery) step(ctx context.Context, store extractionRecoveryPort) (bool, error) {
	if r.complete {
		return true, nil
	}
	if r.account == "" {
		r.complete = true
		return true, nil
	}
	page, err := store.ListPinnedExtractions(ctx, archive.ExtractionRecoveryRequest{Account: r.account, After: r.after, Limit: 32})
	if err != nil || len(page.Items) > 32 {
		return false, errExtractionRecovery
	}
	seen := make(map[archive.JobLease]bool, len(page.Items))
	for _, item := range page.Items {
		if item.Account != r.account || item.Lease.JobID == "" || item.Lease.LeaseID == "" || item.MediaID == "" || item.ArtifactID == "" || seen[item.Lease] {
			return false, errExtractionRecovery
		}
		seen[item.Lease] = true
		if err := store.RetryExtraction(ctx, archive.JobRetry{Lease: item.Lease, Code: "cancelled", After: time.Second, Terminal: false}); err != nil {
			return false, errExtractionRecovery
		}
	}
	if page.NextAfter != "" {
		if len(page.NextAfter) > 128 || page.NextAfter <= r.after || len(page.Items) == 0 {
			return false, errExtractionRecovery
		}
		r.after = page.NextAfter
		return false, nil
	}
	// Confirm an empty full inventory before admitting new work. No caller cursor
	// or apparent page EOF can substitute for this ordered server recheck.
	final, err := store.ListPinnedExtractions(ctx, archive.ExtractionRecoveryRequest{Account: r.account, Limit: 1})
	if err != nil || len(final.Items) > 1 {
		return false, errExtractionRecovery
	}
	if len(final.Items) != 0 || final.NextAfter != "" {
		r.after = ""
		return false, nil
	}
	r.complete = true
	return true, nil
}
func recoverExtractionPins(ctx context.Context, store extractionRecoveryPort, account string) (*extractionRecovery, error) {
	recovery := &extractionRecovery{account: account}
	for pages := 0; pages < 32; pages++ {
		done, err := recovery.step(ctx, store)
		if err != nil {
			return recovery, err
		}
		if done {
			return recovery, nil
		}
	}
	return recovery, nil
}
