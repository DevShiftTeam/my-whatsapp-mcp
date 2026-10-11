//go:build sqlite_fts5

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/outbound"
)

type delayedBridgeReceipt struct {
	app               *Application
	receipt           outbound.Receipt
	started, released chan struct{}
	captured, checked chan archive.CredentialGrant
}

func (b *delayedBridgeReceipt) response(ctx context.Context, grant archive.CredentialGrant) (outbound.Receipt, error) {
	b.captured <- grant
	close(b.started)
	select {
	case <-b.released:
		return b.receipt, nil
	case <-ctx.Done():
		return outbound.Receipt{}, outbound.ErrUnavailable
	}
}
func (b *delayedBridgeReceipt) Send(ctx context.Context, grant archive.CredentialGrant, _ outbound.Request) (outbound.Receipt, error) {
	return b.response(ctx, grant)
}
func (b *delayedBridgeReceipt) SendStatus(ctx context.Context, grant archive.CredentialGrant, _ string) (outbound.Receipt, error) {
	return b.response(ctx, grant)
}
func (b *delayedBridgeReceipt) CheckSendResponse(ctx context.Context, grant archive.CredentialGrant, receipt outbound.Receipt) error {
	b.checked <- grant
	return b.app.CheckSendResponse(ctx, grant, receipt)
}

func TestRemoteOutboundReceiptPublicationRequiresOriginalGrantAndCurrentSendPermission(t *testing.T) {
	for _, api := range []bool{false, true} {
		for _, status := range []bool{false, true} {
			for _, mutation := range []string{"send_permission", "credential_scope", "archive_policy"} {
				name := map[bool]string{false: "mcp", true: "json_api"}[api] + "/" + map[bool]string{false: "send", true: "status"}[status] + "/" + mutation
				t.Run(name, func(t *testing.T) {
					a, connection, initialGrant, token, chat := outboundBridgeFixture(t)
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					page, err := a.store.ListChats(ctx, archive.ReadScope{Account: demoAccount, Principal: "owner", PolicyGeneration: initialGrant.Scope.PolicyGeneration, AllChats: true}, archive.ChatListRequest{})
					if err != nil {
						t.Fatal("synthetic second chat unavailable")
					}
					var other archive.ChatID
					for _, row := range page.Chats {
						if row.ID != chat {
							other = row.ID
							break
						}
					}
					if other == "" {
						t.Fatal("synthetic second chat missing")
					}
					if _, err = a.store.SetCredentialPolicy(ctx, initialGrant.CredentialID, archive.CredentialPolicy{Account: demoAccount, Chats: []archive.ChatID{chat, other}}, initialGrant.CredentialRevision); err != nil {
						t.Fatal("synthetic two-chat credential unavailable")
					}
					original, err := a.store.ResolveCredentialID(ctx, initialGrant.CredentialID)
					if err != nil {
						t.Fatal("synthetic original grant unavailable")
					}
					if err = a.SetSendPermission(ctx, connection.ID, original.CredentialRevision, []archive.ChatID{chat, other}); err != nil {
						t.Fatal("synthetic two-chat send permission unavailable")
					}
					const requestID = "RECEIPT-REQUEST-SENTINEL"
					const messageID = "RECEIPT-MESSAGE-SENTINEL"
					backend := &delayedBridgeReceipt{app: a, receipt: outbound.Receipt{RequestID: requestID, ChatID: chat, MessageID: messageID, State: "accepted", AcceptedAt: time.Now().UTC().Format(time.RFC3339Nano)}, started: make(chan struct{}), released: make(chan struct{}), captured: make(chan archive.CredentialGrant, 1), checked: make(chan archive.CredentialGrant, 1)}
					var releaseOnce sync.Once
					release := func() { releaseOnce.Do(func() { close(backend.released) }) }
					defer release()
					handler := publishOutboundBridgeHandler(t, a, backend)
					arguments := map[string]any{"request_id": requestID}
					tool, path := "get_send_status", "/api/send/status"
					if !status {
						tool, path = "send_message", "/api/send/message"
						arguments["chat_id"], arguments["text"] = chat, "Synthetic receipt test"
					}
					request := bridgeToolRequest(t, token, tool, arguments)
					if api {
						body, err := json.Marshal(arguments)
						if err != nil {
							t.Fatal("synthetic JSON request unavailable")
						}
						request = borrowedRemoteMCPRequest(token, string(body))
						request.URL.Path = path
					}
					response := httptest.NewRecorder()
					done := make(chan struct{})
					go func() { defer close(done); handler.ServeHTTP(response, request) }()
					t.Cleanup(func() {
						release()
						select {
						case <-done:
						case <-time.After(4 * time.Second):
							t.Error("receipt handler did not join during cleanup")
						}
					})
					select {
					case <-backend.started:
					case <-ctx.Done():
						t.Fatal("real outbound handler did not enter delayed receipt backend")
					}
					if captured := <-backend.captured; !reflect.DeepEqual(captured, original) {
						t.Fatal("outbound backend did not capture the original credential grant")
					}
					switch mutation {
					case "send_permission":
						err = a.SetSendPermission(ctx, connection.ID, original.CredentialRevision, []archive.ChatID{other})
					case "credential_scope":
						_, err = a.store.SetCredentialPolicy(ctx, original.CredentialID, archive.CredentialPolicy{Account: demoAccount, Chats: []archive.ChatID{other}}, original.CredentialRevision)
					case "archive_policy":
						_, err = a.store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsSelective, IncludedChats: []archive.ChatID{other}}, original.Scope.PolicyGeneration)
					}
					if err != nil {
						t.Fatal("synthetic owner mutation failed while receipt was blocked")
					}
					current, err := a.store.ResolveCredentialID(ctx, original.CredentialID)
					if err != nil || current.Scope.Principal != original.Scope.Principal || len(current.Scope.Chats) == 0 {
						t.Fatal("mutation removed the principal instead of invalidating the original receipt")
					}
					// Fresh authentication still succeeds for the same remote principal;
					// only its captured receipt authority has been withdrawn.
					initialize := httptest.NewRecorder()
					handler.ServeHTTP(initialize, borrowedRemoteMCPRequest(token, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"synthetic-receipt-fence","version":"1"}}}`))
					if initialize.Code != http.StatusOK {
						t.Fatal("mutated remote principal no longer authenticates")
					}
					release()
					select {
					case <-done:
					case <-ctx.Done():
						t.Fatal("delayed receipt handler did not return after release")
					}
					wantStatus, wantCode := http.StatusUnauthorized, "mcp_unauthorized"
					if mutation == "send_permission" {
						wantStatus, wantCode = http.StatusConflict, "mcp_response_invalidated"
						select {
						case checked := <-backend.checked:
							if !reflect.DeepEqual(checked, original) {
								t.Fatal("receipt publication refreshed the original grant")
							}
						default:
							t.Fatal("receipt publication skipped the application send-permission fence")
						}
					}
					var denied map[string]string
					if response.Code != wantStatus || json.Unmarshal(response.Body.Bytes(), &denied) != nil || len(denied) != 1 || denied["error"] != wantCode || response.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("withdrawn receipt was not replaced by a fixed denial")
					}
					for _, private := range []string{string(chat), requestID, messageID, "accepted"} {
						if strings.Contains(response.Body.String(), private) {
							t.Fatal("withdrawn receipt leaked a private field")
						}
					}
				})
			}
		}
	}
}
