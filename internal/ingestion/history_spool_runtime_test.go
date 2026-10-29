//go:build sqlite_fts5

package ingestion

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func spoolRuntimeFixture(t *testing.T) (*Runtime, string) {
	t.Helper()
	store, dir := durableFixture(t)
	r := durableRuntime(t, store)
	path := filepath.Join(dir, "history-spool")
	spool, err := openHistorySpool(path)
	if err != nil {
		t.Fatal(err)
	}
	r.historySpool = spool
	return r, path
}

func retainedHistoryBatchIDs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".batch") {
			ids = append(ids, strings.TrimSuffix(entry.Name(), ".batch"))
		}
	}
	return ids
}

func TestHistorySpoolCapturesWholeFailedBatchBeforeProjectionAndReopens(t *testing.T) {
	r, dir := spoolRuntimeFixture(t)
	const chat = "200@s.whatsapp.net"
	notice := bodylessHistoryStub(chat, "spool-unhandled-notice", waWeb.WebMessageInfo_BIZ_COEX_PRIVACY_TRANSITION)
	notice.Message.Participant = nil
	notification := realtimeHistory(r, historyRecord("spool-prefix", "spoolprefixreceipt"), notice, historyRecord("spool-tail", "spooltailreceipt"))
	compressed := append([]byte(nil), notification.Message.ProtocolMessage.HistorySyncNotification.GetInitialHistBootstrapInlinePayload()...)
	if err := r.historyFrom(t.Context(), notification); err == nil {
		t.Fatal("fixture did not hit the real strict history failure")
	}
	ids := retainedHistoryBatchIDs(t, dir)
	if len(ids) != 1 {
		t.Fatal("failed history did not retain its complete raw batch")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	spool, err := openHistorySpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	_, raw, account, err := spool.read(ids[0])
	if err != nil || !bytes.Equal(raw, compressed) || account != durableAccount {
		t.Fatal("restart lost the authenticated raw prefix or unvisited suffix")
	}
	clear(raw)
}

func TestHistorySpoolPreservesDecodeFailureDownloadedBytes(t *testing.T) {
	r, dir := spoolRuntimeFixture(t)
	notification := realtimeHistory(r)
	notification.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = nil
	canary := []byte("SYNTHETIC_PRIVATE_INVALID_COMPRESSED_HISTORY")
	r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, file whatsmeow.File) error {
		_, err := file.Write(canary)
		return err
	}
	if err := r.historyFrom(t.Context(), notification); err == nil {
		t.Fatal("fixture did not fail decoding")
	}
	ids := retainedHistoryBatchIDs(t, dir)
	if len(ids) != 1 {
		t.Fatal("decode failure discarded downloaded bytes")
	}
	_, raw, _, err := r.historySpool.read(ids[0])
	if err != nil || !bytes.Equal(raw, canary) {
		t.Fatal("downloaded raw bytes were changed or lost before decode")
	}
	clear(raw)
}

func TestHistorySpoolKeepsCompletedDownloadWhenProcessingIsCanceled(t *testing.T) {
	r, dir := spoolRuntimeFixture(t)
	notification := realtimeHistory(r)
	notification.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = nil
	compressed := compressed(historyWire(historyRecord("spool-canceled", "spoolcanceledreceipt")))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, file whatsmeow.File) error {
		_, err := file.Write(compressed)
		cancel() // Stop intent after SDK-style authenticated completion.
		return err
	}
	if err := r.historyFrom(ctx, notification); err == nil {
		t.Fatal("canceled processing unexpectedly imported history")
	}
	ids := retainedHistoryBatchIDs(t, dir)
	if len(ids) != 1 {
		t.Fatal("cancellation discarded the completed download before capture")
	}
	_, raw, _, err := r.historySpool.read(ids[0])
	if err != nil || !bytes.Equal(raw, compressed) {
		t.Fatal("canceled processing lost the raw bytes")
	}
	clear(raw)
}

func TestHistorySpoolRemovesOnlyCompletelyProcessedBatch(t *testing.T) {
	r, dir := spoolRuntimeFixture(t)
	if err := r.historyFrom(t.Context(), realtimeHistory(r, historyRecord("spool-success", "spoolsuccessreceipt"))); err != nil {
		t.Fatal(err)
	}
	if len(retainedHistoryBatchIDs(t, dir)) != 0 {
		t.Fatal("fully committed history retained its raw payload")
	}
}

func TestHistorySpoolRetainsDiscoveryOnlyBodiesUntilChatsAreChosen(t *testing.T) {
	r, dir := spoolRuntimeFixture(t)
	store := r.store.(*archive.Store)
	policy, err := store.Policy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetPolicy(t.Context(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsSelective}, policy.Generation); err != nil {
		t.Fatal(err)
	}
	if err := r.historyFrom(t.Context(), realtimeHistory(r, historyRecord("spool-discovery-only", "spooldiscoveryreceipt"))); err != nil {
		t.Fatal(err)
	}
	if len(retainedHistoryBatchIDs(t, dir)) != 1 {
		t.Fatal("first-run discovery discarded raw bodies before chat choices were committed")
	}
	queueBurstAssertSearch(t, store, "spooldiscoveryreceipt", 0)
}

func TestHistorySpoolKeepsExplicitPartialUnknownRecord(t *testing.T) {
	r, dir := spoolRuntimeFixture(t)
	r.config.UnknownHistoryPolicy = UnknownHistoryPartial
	if err := r.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	notice := bodylessHistoryStub("200@s.whatsapp.net", "spool-partial-notice", waWeb.WebMessageInfo_BIZ_COEX_PRIVACY_TRANSITION)
	notice.Message.Participant = nil
	if err := r.historyFrom(t.Context(), realtimeHistory(r, notice, historyRecord("spool-partial-tail", "spoolpartialtailreceipt"))); err != nil {
		t.Fatal(err)
	}
	if len(retainedHistoryBatchIDs(t, dir)) != 1 {
		t.Fatal("partial-mode skipped record was falsely treated as fully imported and deleted")
	}
}

func TestHistorySpoolRetainsPartialGroupNoticeAndImportsTail(t *testing.T) {
	for _, kind := range []waWeb.WebMessageInfo_StubType{waWeb.WebMessageInfo_GROUP_CREATE, waWeb.WebMessageInfo_GROUP_PARTICIPANT_ADD} {
		t.Run(kind.String(), func(t *testing.T) {
			r, dir := spoolRuntimeFixture(t)
			r.config.UnknownHistoryPolicy = UnknownHistoryPartial
			if err := r.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			notice := bodylessHistoryStub("300@g.us", "retained-group-notice", kind)
			notice.Message.Key.Participant = proto.String("900@lid")
			notice.Message.MessageStubParameters = []string{"discarded-notice-metadata"}
			tail := bodylessHistoryRecord("300@g.us", "group-notice-tail", "groupnoticetailreceipt")
			notification := realtimeHistory(r)
			notification.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = compressed(unknownHistoryWire(t, unknownHistoryChunk(notice, tail)))
			if err := r.historyFrom(t.Context(), notification); err != nil {
				t.Fatalf("bodyless notice blocked authenticated partial history: %v", err)
			}
			if len(retainedHistoryBatchIDs(t, dir)) != 1 {
				t.Fatal("group notice gap discarded its raw evidence")
			}
			store := r.store.(*archive.Store)
			queueBurstAssertSearch(t, store, "groupnoticetailreceipt", 1)
			status := bodylessHistoryStatus(t, store)
			if !status.Snapshot.UnknownHistoryGap || status.Coverage != "partial" || r.Status().ReconciliationRequired {
				t.Fatal("partial group notice did not expose its persistent coverage warning")
			}
		})
	}
}

func TestHistorySpoolFailureStopsBeforeImport(t *testing.T) {
	r, dir := spoolRuntimeFixture(t)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if err := r.historyFrom(t.Context(), realtimeHistory(r, historyRecord("spool-refused", "spoolrefusedreceipt"))); err == nil {
		t.Fatal("import continued without a safe durable raw capture")
	}
}

func TestHistorySpoolKeepsIgnoredWireFieldsEvenWhenProjectionReturnsSuccess(t *testing.T) {
	for _, mode := range []UnknownHistoryPolicy{UnknownHistoryStrict, UnknownHistoryPartial} {
		for _, layer := range []string{"envelope", "conversation", "record"} {
			t.Run(map[UnknownHistoryPolicy]string{UnknownHistoryStrict: "strict", UnknownHistoryPartial: "partial"}[mode]+"/"+layer, func(t *testing.T) {
				r, dir := spoolRuntimeFixture(t)
				r.config.UnknownHistoryPolicy = mode
				if err := r.Connect(t.Context()); err != nil {
					t.Fatal(err)
				}
				record := historyRecord("spool-unknown-wire", "spoolunknownwirereceipt")
				conversation := &waHistorySync.Conversation{ID: proto.String("200@s.whatsapp.net"), Messages: []*waHistorySync.HistorySyncMsg{record}}
				envelope := &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum(), Conversations: []*waHistorySync.Conversation{conversation}}
				unknown := protowire.AppendVarint(protowire.AppendTag(nil, 777, protowire.VarintType), 0)
				switch layer {
				case "envelope":
					envelope.ProtoReflect().SetUnknown(unknown)
				case "conversation":
					conversation.ProtoReflect().SetUnknown(unknown)
				case "record":
					record.Message.ProtoReflect().SetUnknown(unknown)
				}
				raw, err := proto.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				notification := realtimeHistory(r)
				notification.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = compressed(raw)
				if err := r.historyFrom(t.Context(), notification); err != nil {
					t.Fatal("retention audit changed existing source handling", err)
				}
				if len(retainedHistoryBatchIDs(t, dir)) != 1 {
					t.Fatal("ignored metadata was falsely marked completely imported and discarded")
				}
			})
		}
	}
}

func TestHistorySpoolKeepsUnresolvedDeletionGap(t *testing.T) {
	for _, kind := range []waWeb.WebMessageInfo_StubType{waWeb.WebMessageInfo_REVOKE, waWeb.WebMessageInfo_ADMIN_REVOKE} {
		t.Run(kind.String(), func(t *testing.T) {
			r, dir := spoolRuntimeFixture(t)
			stub := bodylessHistoryStub("300@g.us", "spool-unresolved-target", kind)
			stub.Message.Participant = nil
			notification := realtimeHistory(r)
			notification.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = compressed(bodylessHistoryWire(t, "300@g.us", stub))
			if err := r.historyFrom(t.Context(), notification); err != nil {
				t.Fatal(err)
			}
			if len(retainedHistoryBatchIDs(t, dir)) != 1 {
				t.Fatal("unresolved deletion gap discarded its raw evidence")
			}
		})
	}
}
