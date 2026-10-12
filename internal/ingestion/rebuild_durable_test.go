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
	"google.golang.org/protobuf/proto"
)

func TestRebuildDurablePhoneResponseAndFTS(t *testing.T) {
	ctx := context.Background()
	source, sourceDir := durableFixture(t)
	original := liveMessage("rebuild-source", "OLD_BOOTSTRAP_BODY_CANARY")
	sr := testRuntime(fixtureStore())
	sr.store = source
	if err := sr.message(ctx, original); err != nil {
		t.Fatal(err)
	}
	if err := source.MarkUnsafe(ctx, durableAccount, archive.UnsafeReason("mutation_metadata_missing")); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(sourceDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(before)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "archive")
	receipt, err := archive.BootstrapRebuild(ctx, sourceDir, destination, archive.RebuildOptions{ExpectedAccount: durableAccount, ExpectedSourceSHA256: hex.EncodeToString(hash[:]), DeviceID: durableAccount, Boundary: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	target := durableStore(t, destination)
	r := durableRuntime(t, target)
	r.config.Rebuild = &RebuildConfig{ObservationID: receipt.Generation, Account: receipt.Account, Boundary: receipt.Boundary, Requests: 32}
	// Cached delivery and uncorrelated history must never contribute searchable
	// bodies or plaintext DB/WAL bytes to the new physical namespace.
	if err = r.message(ctx, liveMessage("rebuild-source", "UNREQUESTED_BODY_CANARY")); err != nil {
		t.Fatal(err)
	}
	if err = r.projectHistory(ctx, historyWire(historyRecord("rebuild-source", "UNMATCHED_HISTORY_CANARY"))); err != nil {
		t.Fatal(err)
	}
	durableAssertAbsent(t, destination, "OLD_BOOTSTRAP_BODY_CANARY", "UNREQUESTED_BODY_CANARY", "UNMATCHED_HISTORY_CANARY")
	fresh, err := r.client.ParseWebMessage(original.Info.Chat, historyRecord("rebuild-source", "freshphonecanary").Message)
	if err != nil {
		t.Fatal(err)
	}
	key, err := r.envelopeKey(ctx, fresh.Info)
	if err != nil {
		t.Fatal(err)
	}
	p, err := target.SourcePolicy(ctx, durableAccount)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := target.RebuildProgress(ctx)
	if err != nil || operation == nil {
		t.Fatal("missing rebuild operation")
	}
	q := archive.RebuildRequest{ID: "fresh-phone-request", Generation: receipt.Generation, OperationID: operation.OperationID, OperationRevision: operation.Revision, Key: key, PolicyGeneration: p.Generation, CreatedAt: time.Now()}
	if err = target.PendingRebuildRequest(ctx, q); err != nil {
		t.Fatal(err)
	}
	fresh.UnavailableRequestID = q.ID
	if err = r.message(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	page, err := durableSearch(t, target, "freshphonecanary")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 1 {
		t.Fatal("fresh correlated phone body not searchable")
	}
	if matched, err := target.MatchRebuildRequest(ctx, q.ID, key, q.Generation, p.Generation); err != nil || matched {
		t.Fatal("request was not consumed with source transaction")
	}
	// A replay cannot republish an altered payload using a consumed receipt.
	fresh.Message.Conversation = proto.String("REPLAY_BODY_CANARY")
	if err = r.message(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	durableAssertAbsent(t, destination, "REPLAY_BODY_CANARY")

	// A withheld older copy can still tighten security for a previously admitted
	// source. Preserve the future expiry/version without ever storing its body.
	prior, err := target.SourceMetadata(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).Truncate(time.Second)
	restricted := liveMessage(key.Message, "WITHHELD_RESTRICTION_CANARY")
	restricted.SourceWebMsg = historyRecord(key.Message, "WITHHELD_RESTRICTION_CANARY").Message
	restricted.SourceWebMsg.EphemeralExpirationTimestamp = proto.Uint64(uint64(future.Unix()))
	if err = r.message(ctx, restricted); err != nil {
		t.Fatal(err)
	}
	tightened, err := target.SourceMetadata(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !tightened.Version.ExpiresAt.Equal(future) || tightened.Version.Revision <= prior.Version.Revision {
		t.Fatal("withheld copy lost tighter expiry or source version")
	}
	// View-once is terminal for searchable content even when no positive body is
	// accepted. A later copy omitting that fact cannot obtain a pending request.
	restricted.IsViewOnce = true
	if err = r.message(ctx, restricted); err != nil {
		t.Fatal(err)
	}
	guard, err := target.RebuildGuard(ctx, key)
	if err != nil || !guard.ViewOnce {
		t.Fatal("withheld view-once fact not durable")
	}
	page, err = durableSearch(t, target, "freshphonecanary")
	if err != nil || len(page.Hits) != 0 {
		t.Fatal("view-once restriction left active FTS body")
	}
	q.ID = "fresh-request-after-restriction"
	q.CreatedAt = time.Now()
	if err = target.PendingRebuildRequest(ctx, q); err == nil {
		t.Fatal("later phone copy bypassed observed view-once restriction")
	}
	durableAssertAbsent(t, destination, "WITHHELD_RESTRICTION_CANARY")
	after, err := os.ReadFile(filepath.Join(sourceDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(after) != hash {
		t.Fatal("preserved source archive changed")
	}
}
