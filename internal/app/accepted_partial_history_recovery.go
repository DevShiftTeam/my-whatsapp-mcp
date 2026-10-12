package app

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// RecoverAcceptedPartialHistorySnapshot requires separate explicit owner
// acceptance of unknown semantics. This entry point never enables ingestion.
func RecoverAcceptedPartialHistorySnapshot(ctx context.Context, cfg ClassifiedHistoryRecoveryConfig) (archive.SnapshotRecoveryReceipt, error) {
	return recoverClassifiedHistorySnapshot(ctx, cfg, true)
}
