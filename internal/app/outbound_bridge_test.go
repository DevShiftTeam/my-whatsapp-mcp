//go:build sqlite_fts5

package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"github.com/Bodokh/my-whatsapp-mcp/internal/outbound"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

type bridgeOutboundConnector struct {
	*syntheticConnector
	client outbound.Client
}

func (c *bridgeOutboundConnector) SendOutbound(ctx context.Context, store outbound.Store, grant archive.CredentialGrant, request outbound.Request) (outbound.Receipt, error) {
	return outbound.Send(ctx, store, c.client, grant, request)
}
func (*bridgeOutboundConnector) Disconnect(context.Context) error { return nil }
func (*bridgeOutboundConnector) Close() error                     { return nil }
func (*bridgeOutboundConnector) Quiesced() bool                   { return true }

type bridgeUploadProvider struct {
	started, released chan struct{}
	sends             atomic.Int32
}

func (*bridgeUploadProvider) IsConnected() bool                  { return true }
func (*bridgeUploadProvider) IsLoggedIn() bool                   { return true }
func (*bridgeUploadProvider) GenerateMessageID() types.MessageID { return "SYNTHETIC-BRIDGE-SEND" }
func (p *bridgeUploadProvider) Upload(ctx context.Context, data []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	close(p.started)
	select {
	case <-p.released:
		return whatsmeow.UploadResponse{URL: "https://upload.example.invalid/synthetic", FileLength: uint64(len(data))}, nil
	case <-ctx.Done():
		return whatsmeow.UploadResponse{}, ctx.Err()
	}
}
func (p *bridgeUploadProvider) SendMessage(_ context.Context, _ types.JID, _ *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	p.sends.Add(1)
	return whatsmeow.SendResponse{ID: "SYNTHETIC-BRIDGE-SEND", Timestamp: time.Now()}, nil
}

func outboundBridgeFixture(t *testing.T) (*Application, localui.Connection, archive.CredentialGrant, string, archive.ChatID) {
	t.Helper()
	a := remoteConnectionTestApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	policy, err := a.store.Policy(ctx)
	if err != nil {
		t.Fatal("synthetic archive policy unavailable")
	}
	key := archive.SourceKey{Account: demoAccount, Chat: "12345@s.whatsapp.net", Sender: "12345@s.whatsapp.net", Message: "outbound-bridge-source"}
	if err = a.store.SetSourceNames(ctx, archive.SourceNames{Account: demoAccount, Chat: key.Chat, Sender: key.Sender, ChatName: "Synthetic outbound bridge"}); err != nil {
		t.Fatal("synthetic source names unavailable")
	}
	if _, err = a.store.ApplySourceEvent(ctx, archive.SourceEvent{Key: key, Kind: archive.EventOriginal, OriginalSentAt: time.Now(), OriginalDateKnown: true, SourceRevision: 1, Body: "Synthetic outbound bridge", BodyKind: archive.MessageText}, policy.Generation); err != nil {
		t.Fatal("synthetic source unavailable")
	}
	page, err := a.store.ListChats(ctx, archive.ReadScope{Account: demoAccount, Principal: "owner", PolicyGeneration: policy.Generation, AllChats: true}, archive.ChatListRequest{})
	if err != nil {
		t.Fatal("synthetic chats unavailable")
	}
	var chat archive.ChatID
	for _, row := range page.Chats {
		if row.Name == "Synthetic outbound bridge" {
			chat = row.ID
		}
	}
	if chat == "" {
		t.Fatal("synthetic outbound chat missing")
	}
	expiry := time.Now().Add(time.Hour)
	connection, err := a.CreateConnection(ctx, localui.ConnectionRequest{Client: "codex_cli", Label: "Synthetic outbound bridge", Mode: "remote", Chats: []archive.ChatID{chat}, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal("synthetic remote connection unavailable")
	}
	secret, err := a.RevealRemoteCredential(ctx, localui.ConnectionRevealRequest{ID: connection.ID, Revision: connection.Revision})
	if err != nil {
		t.Fatal("synthetic remote credential unavailable")
	}
	grant, err := a.store.ResolveCredentialID(ctx, a.authState.RemoteClients[0].CredentialID)
	if err != nil {
		t.Fatal("synthetic grant unavailable")
	}
	a.demo = false
	t.Cleanup(func() {
		a.data.Lock()
		defer a.data.Unlock()
		a.demo = true
		a.runtime = nil
	})
	if err = a.SetSendPermission(ctx, connection.ID, connection.Revision, []archive.ChatID{chat}); err != nil {
		t.Fatal("synthetic send permission unavailable")
	}
	return a, connection, grant, secret.Token, chat
}

func publishOutboundBridgeHandler(t *testing.T, a *Application, backend mcpserver.Outbound) http.Handler {
	t.Helper()
	cfg := a.mcpConfig.Remote.Server
	cfg.Outbound = backend
	cfg.RemoteClients = &a.remoteRegistry
	cfg.NativeClients = &a.nativeRegistry
	cfg.RequestTimeout = 3 * time.Second
	handler, err := mcpserver.NewHandler(cfg, a.resources.retrieval, a.store)
	if err != nil {
		t.Fatal("real outbound MCP handler unavailable")
	}
	a.data.Lock()
	a.mcp.remoteHandler = handler
	a.data.Unlock()
	return a.RemoteMCPHandler()
}

func bridgeToolRequest(t *testing.T, token, tool string, arguments any) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": arguments}})
	if err != nil {
		t.Fatal("synthetic tool request unavailable")
	}
	return borrowedRemoteMCPRequest(token, string(body))
}

func TestRemoteMCPOutboundOwnerWithdrawalCommitsBeforeBlockedUploadReturns(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "send_permission", true: "connection"}[revoke], func(t *testing.T) {
			a, connection, grant, token, chat := outboundBridgeFixture(t)
			if err := a.SetSendPermissionMode(context.Background(), connection.ID, connection.Revision, archive.SendPermission{Mode: "all"}, 1); err != nil {
				t.Fatal("synthetic all-target send permission unavailable")
			}
			provider := &bridgeUploadProvider{started: make(chan struct{}), released: make(chan struct{})}
			a.runtime = &bridgeOutboundConnector{syntheticConnector: &syntheticConnector{account: demoAccount, snapshot: ingestion.Status{Connection: "connected"}}, client: provider}
			handler := publishOutboundBridgeHandler(t, a, a)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(provider.released) }) }
			defer release()
			request := bridgeToolRequest(t, token, "send_media", map[string]any{"chat_id": chat, "request_id": "bridge-blocked-upload", "kind": "document", "mime_type": "application/pdf", "filename": "synthetic.pdf", "data_base64": base64.StdEncoding.EncodeToString([]byte("%PDF-1.7 synthetic"))})
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); handler.ServeHTTP(response, request) }()
			t.Cleanup(func() {
				release()
				select {
				case <-done:
				case <-time.After(4 * time.Second):
					t.Error("blocked upload handler did not join during cleanup")
				}
			})
			select {
			case <-provider.started:
			case <-time.After(time.Second):
				t.Fatal("real MCP send did not reach the synthetic upload")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			withdrawn := make(chan error, 1)
			withdrawalDone := make(chan struct{})
			go func() {
				defer close(withdrawalDone)
				if revoke {
					withdrawn <- a.RevokeConnection(ctx, localui.ConnectionRevokeRequest{ID: connection.ID, Revision: connection.Revision})
				} else {
					withdrawn <- a.SetSendPermissionMode(ctx, connection.ID, connection.Revision, archive.SendPermission{Mode: "none"}, 2)
				}
			}()
			t.Cleanup(func() {
				cancel()
				release()
				select {
				case <-withdrawalDone:
				case <-time.After(4 * time.Second):
					t.Error("owner withdrawal did not join during cleanup")
				}
			})
			select {
			case err := <-withdrawn:
				if err != nil {
					t.Fatal("owner withdrawal failed while upload was blocked")
				}
			case <-ctx.Done():
				release()
				t.Fatal("owner withdrawal waited behind the borrowed MCP request")
			}
			if _, err := a.store.SendTarget(context.Background(), grant, chat); err == nil {
				t.Fatal("completed owner withdrawal left send authority active")
			}
			select {
			case <-done:
				t.Fatal("synthetic upload completed before it was released")
			default:
			}
			release()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("withdrawn MCP send did not finish after upload release")
			}
			if provider.sends.Load() != 0 || strings.Contains(response.Body.String(), `"state":"accepted"`) {
				t.Fatal("withdrawn authority reached the synthetic message send")
			}
		})
	}
}

// The gate pauses inside the real MCP tool after authentication, but before the
// Application method. It deliberately acknowledges cancellation only after the
// test permits handler return, like a bounded provider cleanup owner.
type bridgeLifecycleGate struct {
	app                             *Application
	entered, released, acknowledged chan struct{}
	callDone                        chan struct{}
	result                          chan error
}

type bridgeResourceCleanup struct{ closed atomic.Bool }

func (p *bridgeResourceCleanup) Close(context.Context) error {
	p.closed.Store(true)
	return nil
}

func (g *bridgeLifecycleGate) invoke(ctx context.Context, operation func() error) (outbound.Receipt, error) {
	close(g.entered)
	<-g.released
	go func() { defer close(g.callDone); g.result <- operation() }()
	select {
	case <-g.callDone:
	case <-ctx.Done():
	}
	<-g.acknowledged
	return outbound.Receipt{}, outbound.ErrUnavailable
}
func (g *bridgeLifecycleGate) Send(ctx context.Context, grant archive.CredentialGrant, request outbound.Request) (outbound.Receipt, error) {
	return g.invoke(ctx, func() error { _, err := g.app.Send(ctx, grant, request); return err })
}
func (g *bridgeLifecycleGate) SendStatus(ctx context.Context, grant archive.CredentialGrant, id string) (outbound.Receipt, error) {
	return g.invoke(ctx, func() error { _, err := g.app.SendStatus(ctx, grant, id); return err })
}
func (g *bridgeLifecycleGate) CheckSendResponse(ctx context.Context, grant archive.CredentialGrant, receipt outbound.Receipt) error {
	return g.app.CheckSendResponse(ctx, grant, receipt)
}

func TestRemoteMCPOutboundLifecycleWriterRejectsInnerCallAndJoinsBorrower(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		for _, status := range []bool{false, true} {
			name := map[bool]string{false: "close", true: "delete"}[deleting] + "/" + map[bool]string{false: "send", true: "status"}[status]
			t.Run(name, func(t *testing.T) {
				a, _, _, token, chat := outboundBridgeFixture(t)
				a.runtime = &bridgeOutboundConnector{syntheticConnector: &syntheticConnector{account: demoAccount, snapshot: ingestion.Status{Connection: "connected"}}}
				store, resources, query, paths := a.store, a.resources, a.queryContext, a.paths
				cleanup := &bridgeResourceCleanup{}
				resources.extraction = cleanup
				gate := &bridgeLifecycleGate{app: a, entered: make(chan struct{}), released: make(chan struct{}), acknowledged: make(chan struct{}), callDone: make(chan struct{}), result: make(chan error, 1)}
				var releaseOnce, acknowledgeOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(gate.released) }) }
				acknowledge := func() { acknowledgeOnce.Do(func() { close(gate.acknowledged) }) }
				defer acknowledge()
				defer release()
				handler := publishOutboundBridgeHandler(t, a, gate)
				tool := "send_message"
				arguments := map[string]any{"chat_id": chat, "request_id": "bridge-lifecycle", "text": "Synthetic text"}
				if status {
					tool, arguments = "get_send_status", map[string]any{"request_id": "bridge-lifecycle"}
				}
				request := bridgeToolRequest(t, token, tool, arguments)
				requestDone := make(chan struct{})
				go func() { defer close(requestDone); handler.ServeHTTP(httptest.NewRecorder(), request) }()
				t.Cleanup(func() {
					release()
					acknowledge()
					select {
					case <-requestDone:
					case <-time.After(4 * time.Second):
						t.Error("lifecycle-gated handler did not join during cleanup")
					}
				})
				select {
				case <-gate.entered:
				case <-time.After(time.Second):
					t.Fatal("real MCP tool did not reach lifecycle gate")
				}
				lifecycleDone := make(chan error, 1)
				lifecycleJoined := make(chan struct{})
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				go func() {
					defer close(lifecycleJoined)
					if deleting {
						lifecycleDone <- a.DeleteLocalData(ctx, true)
					} else {
						lifecycleDone <- a.Close(ctx)
					}
				}()
				t.Cleanup(func() {
					cancel()
					release()
					acknowledge()
					for _, owner := range []<-chan struct{}{lifecycleJoined, gate.callDone} {
						select {
						case <-owner:
						case <-time.After(4 * time.Second):
							t.Error("outbound lifecycle owner did not join during cleanup")
						}
					}
				})
				select {
				case <-query.Done():
				case <-time.After(time.Second):
					t.Fatal("lifecycle did not cancel the admitted query owner")
				}
				deadline := time.Now().Add(time.Second)
				for a.data.TryRLock() {
					a.data.RUnlock()
					if time.Now().After(deadline) {
						t.Fatal("lifecycle writer did not take or queue the resource lock")
					}
					time.Sleep(time.Millisecond)
				}
				release()
				select {
				case err := <-gate.result:
					if !errors.Is(err, outbound.ErrUnavailable) {
						t.Fatal("inner outbound call crossed the lifecycle writer")
					}
				case <-time.After(time.Second):
					t.Fatal("inner outbound call blocked behind lifecycle teardown")
				}
				select {
				case <-lifecycleDone:
					t.Fatal("lifecycle finished before the borrowed handler acknowledged return")
				default:
				}
				if account, err := store.BoundAccount(context.Background()); err != nil || account != demoAccount {
					t.Fatal("borrowed handler lost its archive before acknowledgement")
				}
				if cleanup.closed.Load() {
					t.Fatal("borrowed handler lost its resources before acknowledgement")
				}
				if lease, err := appdata.Acquire(paths); err == nil {
					_ = lease.Close()
					t.Fatal("borrowed handler lost instance ownership before acknowledgement")
				}
				acknowledge()
				select {
				case <-requestDone:
				case <-time.After(time.Second):
					t.Fatal("borrowed handler did not finish after acknowledgement")
				}
				select {
				case err := <-lifecycleDone:
					if err != nil {
						t.Fatal("lifecycle failed after the borrowed handler joined")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("lifecycle did not finish after the borrowed handler joined")
				}
				if !cleanup.closed.Load() {
					t.Fatal("joined lifecycle did not close the synthetic resource owner")
				}
			})
		}
	}
}
