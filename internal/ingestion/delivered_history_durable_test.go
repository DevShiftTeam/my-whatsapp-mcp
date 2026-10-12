//go:build sqlite_fts5

package ingestion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func TestDeliveredHistoryDurableAuthenticatedFiftyBodies(t *testing.T) {
	ctx := context.Background()
	source, sourceDir := durableFixture(t)
	if err := source.MarkUnsafe(ctx, durableAccount, archive.UnsafeReason("mutation_metadata_missing")); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(sourceDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "archive")
	receipt, err := archive.BootstrapRebuild(ctx, sourceDir, destination, archive.RebuildOptions{ExpectedAccount: durableAccount, ExpectedSourceSHA256: hex.EncodeToString(digest[:]), DeviceID: durableAccount, Boundary: time.Unix(1500, 0)})
	if err != nil {
		t.Fatal(err)
	}
	store := durableStore(t, destination)
	selection, err := store.SelectRebuildHistoryPolicy(ctx, archive.RebuildHistorySelectionOptions{Policy: archive.RebuildHistoryDelivered, CommandID: "synthetic-delivered-activation", AcceptDeliveredSnapshot: true})
	if err != nil || selection == nil {
		t.Fatalf("select delivered strategy: %v", err)
	}
	r := durableRuntime(t, store)
	r.config.Rebuild = &RebuildConfig{ObservationID: receipt.Generation, Account: receipt.Account, DeviceID: receipt.DeviceID, Boundary: receipt.Boundary, Requests: 32}
	r.config.RebuildHistoryPolicy = selection.Policy
	if err = configureRebuild(ctx, &r.config, store); err != nil {
		t.Fatal(err)
	}
	if err = r.historyFrom(ctx, deliveredNotification(r, deliveredFiftyRecords()...)); err != nil {
		t.Fatal(err)
	}
	status := bodylessHistoryStatus(t, store)
	if status.Messages != 50 || status.ReconciliationRequired {
		t.Fatalf("authenticated chunk not admitted: messages=%d", status.Messages)
	}
	page, err := durableSearch(t, store, "deliveredbodyreceipt")
	if err != nil || len(page.Hits) != 20 {
		t.Fatalf("delivered snapshots missing from FTS: hits=%d err=%v", len(page.Hits), err)
	}
	progress, err := store.RebuildDispatchState(ctx)
	if err != nil || progress.CopySpent != 0 || progress.Accepted != 0 {
		t.Fatal("delivered snapshots fabricated per-key request consumption")
	}
}
