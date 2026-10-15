//go:build sqlite_fts5

package ingestion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"google.golang.org/protobuf/proto"
)

func rebuildConflictHistoryRecord(id string, kind waE2E.ProtocolMessage_Type, revision int64) *waHistorySync.HistorySyncMsg {
	record := historyRecord("synthetic-control-"+id, "")
	record.Message.MessageTimestamp = proto.Uint64(uint64(revision / 1000))
	record.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: kind.Enum(), Key: &waCommon.MessageKey{ID: proto.String(id), RemoteJID: proto.String("200@s.whatsapp.net"), FromMe: proto.Bool(false)}, TimestampMS: proto.Int64(revision)}}
	if kind == waE2E.ProtocolMessage_MESSAGE_EDIT {
		record.Message.Message.ProtocolMessage.EditedMessage = &waE2E.Message{Conversation: proto.String("WITHHELD_REBUILD_EDIT_CANARY")}
	}
	return record
}

func TestRebuildCommittedRevisionConflictContinuesHistory(t *testing.T) {
	ctx := context.Background()
	source, sourceDir := durableFixture(t)
	sr := durableRuntime(t, source)
	if err := sr.message(ctx, liveMessage("source-seed", "SOURCE_BODY_CANARY")); err != nil {
		t.Fatal(err)
	}
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
	hash := sha256.Sum256(data)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "archive")
	receipt, err := archive.BootstrapRebuild(ctx, sourceDir, destination, archive.RebuildOptions{ExpectedAccount: durableAccount, ExpectedSourceSHA256: hex.EncodeToString(hash[:]), DeviceID: durableAccount, Boundary: time.Unix(1500, 0)})
	if err != nil {
		t.Fatal(err)
	}
	store := durableStore(t, destination)
	r := durableRuntime(t, store)
	r.config.Rebuild = &RebuildConfig{ObservationID: receipt.Generation, Account: receipt.Account, Boundary: receipt.Boundary, Requests: 32}
	for _, id := range []string{"conflict", "revocable", "editable"} {
		event := liveMessage(id, "rebuild"+id+"originalreceipt")
		event.Info.Timestamp = time.Unix(2000, 0)
		if err = r.message(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	key := bodylessHistoryKey("200@s.whatsapp.net", "conflict")
	before, err := store.SourceMetadata(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := store.SourcePolicy(ctx, durableAccount)
	if err != nil {
		t.Fatal(err)
	}
	// Establish a REAL committed row quarantine, not a fabricated adapter error.
	_, err = store.ApplySourceEvent(ctx, archive.SourceEvent{Key: key, Kind: archive.EventOriginal, SourceRevision: before.ProtocolRevision, OriginalSentAt: time.Unix(2000, 0), OriginalDateKnown: true, BodyKind: archive.MessageText, Body: "CONFLICT_REPLACEMENT_CANARY"}, policy.Generation)
	if err != archive.ErrSourceRevisionConflict {
		t.Fatalf("fixture did not commit exact conflict: %v", err)
	}
	queueBurstAssertSearch(t, store, "rebuildconflictoriginalreceipt", 0)
	spentBefore, err := store.RebuildDispatchState(ctx)
	if err != nil || spentBefore == nil {
		t.Fatal("missing durable rebuild operation")
	}
	conflictEdit := rebuildConflictHistoryRecord("conflict", waE2E.ProtocolMessage_MESSAGE_EDIT, before.ProtocolRevision)
	discovery := historyRecord("following-discovery", "WITHHELD_REBUILD_DISCOVERY_CANARY")
	revoke := rebuildConflictHistoryRecord("revocable", waE2E.ProtocolMessage_REVOKE, 3000000)
	edit := rebuildConflictHistoryRecord("editable", waE2E.ProtocolMessage_MESSAGE_EDIT, 3000000)
	retention := &historyRawRetention{}
	historyCtx := context.WithValue(ctx, historyRawRetentionKey{}, retention)
	if err = r.projectHistory(historyCtx, historyWire(conflictEdit, discovery, revoke, edit)); err != nil {
		t.Fatalf("committed quarantine stopped rebuild history suffix: %v", err)
	}
	if r.Status().ReconciliationRequired {
		t.Fatal("per-message conflict became an account fence")
	}
	if !retention.incomplete {
		t.Fatal("quarantined history edit was falsely eligible for raw deletion")
	}
	guard, err := store.RebuildGuard(ctx, key)
	if err != nil || guard.RevisionBarrier != string(archive.ErrSourceRevisionConflict) {
		t.Fatal("committed conflict barrier lost")
	}
	revoked, err := store.SourceMetadata(ctx, bodylessHistoryKey("200@s.whatsapp.net", "revocable"))
	if err != nil || !revoked.Version.Deleted {
		t.Fatal("following revoke was dropped")
	}
	edited, err := store.SourceMetadata(ctx, bodylessHistoryKey("200@s.whatsapp.net", "editable"))
	if err != nil || edited.ProtocolRevision != 3000000 || !edited.OriginalSentAt.Equal(time.Unix(2000, 0)) {
		t.Fatal("following edit invalidation lost revision/date")
	}
	found := false
	anchors, err := store.BackfillAnchors(ctx, durableAccount, 32)
	if err != nil {
		t.Fatal(err)
	}
	for _, anchor := range anchors {
		found = found || anchor.Key.Message == "following-discovery"
	}
	if !found {
		t.Fatal("following metadata discovery was dropped")
	}
	for _, term := range []string{"rebuildconflictoriginalreceipt", "rebuildrevocableoriginalreceipt", "rebuildeditableoriginalreceipt"} {
		queueBurstAssertSearch(t, store, term, 0)
	}
	status := bodylessHistoryStatus(t, store)
	if status.ReconciliationRequired || status.Coverage != "partial" || !slices.Contains(status.Codes, "source_revision_conflict") {
		t.Fatal("durable scoped gap was lost or made global")
	}
	replay := liveMessage("conflict", "rebuildconflictoriginalreceipt")
	replay.Info.Timestamp = time.Unix(2000, 0)
	if err = r.messageWithMediaOutcome(ctx, replay); err != nil {
		t.Fatal(err)
	}
	if err = r.projectHistory(ctx, historyWire(conflictEdit)); err != nil {
		t.Fatal("equal revision replay stopped history")
	}
	queueBurstAssertSearch(t, store, "rebuildconflictoriginalreceipt", 0)
	spentAfter, err := store.RebuildDispatchState(ctx)
	if err != nil || spentAfter.CopySpent != spentBefore.CopySpent || spentAfter.DiscoverySpent != spentBefore.DiscoverySpent {
		t.Fatal("handling conflict changed request spend")
	}
	durableAssertAbsent(t, destination, "SOURCE_BODY_CANARY", "CONFLICT_REPLACEMENT_CANARY", "WITHHELD_REBUILD_DISCOVERY_CANARY", "WITHHELD_REBUILD_EDIT_CANARY")
	bad := rebuildConflictHistoryRecord("editable", waE2E.ProtocolMessage_MESSAGE_EDIT, 4000000)
	bad.Message.Message.ProtocolMessage.EditedMessage = ambiguousProjectionMessage().Message
	if err = r.projectHistory(ctx, historyWire(bad)); err == nil || !r.Status().ReconciliationRequired {
		t.Fatal("malformed edit lost strict fence")
	}
	if _, err = durableSearch(t, store, "rebuildconflictoriginalreceipt"); !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatal("malformed edit left archive readable")
	}
}

func TestRebuildRevisionConflictDoesNotSwallowOtherErrors(t *testing.T) {
	for _, cause := range []error{fmt.Errorf("wrapped fixture: %w", archive.ErrSourceRevisionConflict), archive.ErrArchiveUnavailable, archive.ErrMutationMetadataMissing} {
		t.Run(cause.Error(), func(t *testing.T) {
			r, store := rebuildFixture()
			store.failApply = cause
			record := rebuildConflictHistoryRecord("synthetic-conflict", waE2E.ProtocolMessage_MESSAGE_EDIT, 2000000)
			if err := r.projectHistory(context.Background(), historyWire(record)); err == nil || !r.Status().ReconciliationRequired {
				t.Fatal("non-exact store error was swallowed")
			}
		})
	}
}
