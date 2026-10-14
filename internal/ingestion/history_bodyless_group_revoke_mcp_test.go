//go:build sqlite_fts5

package ingestion

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
)

func TestHistoryBodylessGroupRevokeAuthenticatedCallbackScopedMCP(t *testing.T) {
	store, dir := durableFixture(t)
	r := durableRuntime(t, store)
	if err := r.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	const chat, target = "300@g.us", "callback-senderless-revoke"
	stub := bodylessHistoryStub(chat, target, waWeb.WebMessageInfo_REVOKE)
	stub.Message = groupRevokeReviewStub(chat, target, true)
	tail := bodylessHistoryRecord(chat, "callback-group-tail", "callbackgroupreadablereceipt")
	notification := realtimeHistory(r)
	notification.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = compressed(bodylessHistoryWire(t, chat, stub, tail))
	r.deliver(notification)
	r.deliver(liveMessage("callback-direct-tail", "callbackdirectreadablereceipt"))
	if err := r.Disconnect(t.Context()); err != nil {
		t.Fatal("authenticated callback history did not drain safely", err)
	}
	if r.Status().HistoryChunks != 1 || r.Status().ReconciliationRequired {
		t.Fatal("valid senderless revoke globally fenced admitted history")
	}
	group, err := durableSearch(t, store, "callbackgroupreadablereceipt")
	if err != nil || len(group.Hits) != 1 {
		t.Fatal("unrelated group history was not readable", err)
	}
	direct, err := durableSearch(t, store, "callbackdirectreadablereceipt")
	if err != nil || len(direct.Hits) != 1 {
		t.Fatal("subsequent direct text was not readable", err)
	}
	handler, err := mcpserver.NewHandler(mcpserver.Config{Mode: mcpserver.Bearer, Hosts: []string{"synthetic.invalid"}, RequestTimeout: time.Second}, store, store)
	if err != nil {
		t.Fatal(err)
	}
	for index, tc := range []struct {
		name  string
		chat  archive.ChatID
		query string
		gap   bool
	}{
		{"group", group.Hits[0].ChatID, "callbackgroupreadablereceipt", true},
		{"direct", direct.Hits[0].ChatID, "callbackdirectreadablereceipt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{byte(index + 21)}, 32))
			_, err := store.CreateCredential(t.Context(), archive.CredentialCreate{Label: "Synthetic revoke scope", Principal: "client:" + tc.name, Digest: sha256.Sum256([]byte(token)), Policy: archive.CredentialPolicy{Account: durableAccount, Chats: []archive.ChatID{tc.chat}}, ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			var status struct {
				Codes                  []string `json:"codes"`
				ReconciliationRequired bool     `json:"reconciliation_required"`
			}
			deliveredLiveOutput(t, deliveredLiveRPC(t.Context(), handler, token, "get_index_status", map[string]any{}), &status)
			if status.ReconciliationRequired || slices.Contains(status.Codes, "unresolved_deletion") != tc.gap {
				t.Fatal("MCP gap escaped its chat scope or blocked unrelated reads")
			}
			var page struct {
				Hits []struct{} `json:"hits"`
			}
			deliveredLiveOutput(t, deliveredLiveRPC(t.Context(), handler, token, "search_messages", map[string]any{"query": tc.query}), &page)
			if len(page.Hits) != 1 {
				t.Fatal("scoped MCP lost an unrelated readable message")
			}
		})
	}
	original := bodylessHistoryRecord(chat, target, "callbackrevokeforbiddenreceipt")
	if err := r.projectHistory(t.Context(), bodylessHistoryWire(t, chat, original)); err != nil {
		t.Fatal(err)
	}
	queueBurstAssertSearch(t, store, "callbackrevokeforbiddenreceipt", 0)
	durableAssertAbsent(t, dir, "callbackrevokeforbiddenreceipt")
}

func TestHistoryBodylessGroupRevokePeerCannotInstallUnknownGuard(t *testing.T) {
	for _, fromMe := range []bool{false, true} {
		t.Run(map[bool]string{false: "incoming_peer", true: "wrong_self_identity"}[fromMe], func(t *testing.T) {
			store, _ := durableFixture(t)
			r := durableRuntime(t, store)
			const chat, target = "300@g.us", "peer-untrusted-revoke"
			stub := bodylessHistoryStub(chat, target, waWeb.WebMessageInfo_REVOKE)
			stub.Message = groupRevokeReviewStub(chat, target, true)
			notification := realtimeHistory(r)
			notification.Info.Sender = types.NewJID("777", types.DefaultUserServer)
			notification.Info.IsFromMe = fromMe
			notification.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = compressed(bodylessHistoryWire(t, chat, stub))
			if err := r.historyFrom(t.Context(), notification); !errors.Is(err, archive.ErrInvalidEvent) {
				t.Fatal("peer notification bypassed authenticated history admission", err)
			}
			original := bodylessHistoryRecord(chat, target, "peernoguardreadablereceipt")
			if err := r.projectHistory(t.Context(), bodylessHistoryWire(t, chat, original)); err != nil {
				t.Fatal(err)
			}
			queueBurstAssertSearch(t, store, "peernoguardreadablereceipt", 1)
			if slices.Contains(bodylessHistoryStatus(t, store).Codes, "unresolved_deletion") {
				t.Fatal("untrusted history installed an unresolved guard")
			}
		})
	}
}
