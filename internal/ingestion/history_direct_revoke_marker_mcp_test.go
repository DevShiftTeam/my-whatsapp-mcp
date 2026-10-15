//go:build sqlite_fts5

package ingestion

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"go.mau.fi/whatsmeow/proto/waWeb"
)

func TestHistoryDirectRevokeMarkerAuthenticatedCallbackScopedMCP(t *testing.T) {
	store, _ := durableFixture(t)
	r := durableRuntime(t, store)
	if err := r.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.message(t.Context(), liveMessage("marker-callback-target", "markercallbackforbiddenreceipt")); err != nil {
		t.Fatal(err)
	}
	prefix, err := durableSearch(t, store, "markercallbackforbiddenreceipt")
	if err != nil || len(prefix.Hits) != 1 {
		t.Fatal("synthetic prefix not readable")
	}
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{37}, 32))
	_, err = store.CreateCredential(t.Context(), archive.CredentialCreate{Label: "Synthetic marker scope", Principal: "client:direct-marker", Digest: sha256.Sum256([]byte(token)), Policy: archive.CredentialPolicy{Account: durableAccount, Chats: []archive.ChatID{prefix.Hits[0].ChatID}}, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := mcpserver.NewHandler(mcpserver.Config{Mode: mcpserver.Bearer, Hosts: []string{"synthetic.invalid"}, RequestTimeout: time.Second}, store, store)
	if err != nil {
		t.Fatal(err)
	}
	stub := bodylessHistoryStub("200@s.whatsapp.net", "marker-callback-target", waWeb.WebMessageInfo_REVOKE)
	stub.Message = directRevokeMarkerRecord("200@s.whatsapp.net", "marker-callback-target")
	r.deliver(realtimeHistory(r, stub, historyRecord("marker-callback-tail", "markercallbackreadablereceipt")))
	if err := r.Disconnect(t.Context()); err != nil {
		t.Fatal("marker history did not drain", err)
	}
	if r.Status().ReconciliationRequired || r.Status().HistoryChunks != 1 {
		t.Fatal("marker failed authenticated callback ingestion")
	}
	var page struct {
		Hits []struct{} `json:"hits"`
	}
	deliveredLiveOutput(t, deliveredLiveRPC(t.Context(), handler, token, "search_messages", map[string]any{"query": "markercallbackforbiddenreceipt"}), &page)
	if len(page.Hits) != 0 {
		t.Fatal("deleted source remained visible through scoped MCP")
	}
	deliveredLiveOutput(t, deliveredLiveRPC(t.Context(), handler, token, "search_messages", map[string]any{"query": "markercallbackreadablereceipt"}), &page)
	if len(page.Hits) != 1 {
		t.Fatal("marker stopped unrelated scoped MCP reads")
	}
	result := deliveredLiveRPC(t.Context(), handler, token, "get_context", map[string]any{"message_id": prefix.Hits[0].MessageID})
	if result.status != 200 || !strings.Contains(result.body, `"isError":true`) || strings.Contains(result.body, "markercallbackforbiddenreceipt") {
		t.Fatal("deleted context escaped the normal source guard")
	}
	if err := r.projectHistory(t.Context(), historyWire(historyRecord("marker-callback-target", "markercallbackforbiddenreceipt"))); err != nil {
		t.Fatal(err)
	}
	deliveredLiveOutput(t, deliveredLiveRPC(t.Context(), handler, token, "search_messages", map[string]any{"query": "markercallbackforbiddenreceipt"}), &page)
	if len(page.Hits) != 0 {
		t.Fatal("older history resurrected marker-deleted text through MCP")
	}
}
