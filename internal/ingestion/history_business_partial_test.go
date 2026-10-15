//go:build sqlite_fts5

package ingestion

import (
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow/proto/waWeb"
)

// A business privacy transition is not established as benign by the SDK enum.
// Explicit partial mode must record that uncertainty, not silently classify it
// as a notice or discard the supported deletion that follows it.
func TestExplicitPartialBusinessPrivacyTransitionKeepsGapAndDeletion(t *testing.T) {
	store, dir := durableFixture(t)
	r := durableRuntime(t, store)
	r.config.UnknownHistoryPolicy = UnknownHistoryPartial
	if err := r.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.message(t.Context(), liveMessage("partial-biz-target", "partialbizforbiddenreceipt")); err != nil {
		t.Fatal(err)
	}
	notice := bodylessHistoryStub("200@s.whatsapp.net", "partial-biz-transition", waWeb.WebMessageInfo_BIZ_COEX_PRIVACY_TRANSITION)
	notice.Message.Participant = nil
	if historyPresence(notice.Message) != 155 {
		t.Fatal("fixture lost the phone-delivered notice shape")
	}
	deletion := bodylessHistoryStub("200@s.whatsapp.net", "partial-biz-target", waWeb.WebMessageInfo_REVOKE)
	r.deliver(realtimeHistory(r, notice, deletion, historyRecord("partial-biz-tail", "partialbizreadablereceipt")))
	if err := r.Disconnect(t.Context()); err != nil {
		t.Fatal("explicit partial history did not drain", err)
	}
	queueBurstAssertSearch(t, store, "partialbizforbiddenreceipt", 0)
	queueBurstAssertSearch(t, store, "partialbizreadablereceipt", 1)
	policy, err := store.Policy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scope := archive.ReadScope{Account: durableAccount, Principal: "local_owner", AllChats: true, PolicyGeneration: policy.Generation}
	status, err := store.Status(t.Context(), scope)
	if err != nil || status.ReconciliationRequired || status.Coverage != "partial" || !status.Snapshot.UnknownHistoryGap {
		t.Fatal("unsupported business transition lost its explicit uncertainty warning", err)
	}
	metadata, err := store.SourceMetadata(t.Context(), bodylessHistoryKey("200@s.whatsapp.net", "partial-biz-transition"))
	if err != nil || metadata.Version.MessageID != "" || metadata.OriginalDateKnown {
		t.Fatal("discarded notice created source content or an original date")
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := durableStore(t, dir)
	status, err = reopened.Status(t.Context(), scope)
	if err != nil || !status.Snapshot.UnknownHistoryGap {
		t.Fatal("partial-history uncertainty disappeared on reopen", err)
	}
	queueBurstAssertSearch(t, reopened, "partialbizforbiddenreceipt", 0)
	queueBurstAssertSearch(t, reopened, "partialbizreadablereceipt", 1)
}
