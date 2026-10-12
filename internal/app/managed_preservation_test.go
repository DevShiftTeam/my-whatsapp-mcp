//go:build sqlite_fts5

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestManagedMCPPreservesFullToolCatalogAndNativeAttachmentService(t *testing.T) {
	a := unixNativeTestApp(t)
	service := a.mcp.attachments
	if service == nil || service.hasPublicURL() {
		t.Fatal("native fixture has unexpected attachment authority")
	}
	if err := a.ConfigureManagedMCP(context.Background(), &ManagedMCPConfig{PublicURL: "https://mcp.example.com/mcp", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if a.mcp.attachments != service || !service.hasPublicURL() {
		t.Fatal("managed route stranded native attachment service")
	}
	_, token := managedGrant(t, a)
	request := httptest.NewRequest("POST", "https://mcp.example.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response := httptest.NewRecorder()
	a.ManagedMCPHandler().ServeHTTP(response, request)
	var result struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatal("managed catalog unavailable", response.Code, response.Body.String())
	}
	var names []string
	for _, tool := range result.Result.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "get_attachment,get_context,get_index_status,get_send_status,list_chats,search_attachments,search_contacts,search_groups,search_messages,send_media,send_message,send_voice_note" {
		t.Fatal("managed catalog lost main tools", names)
	}
	// Native handlers retain the exact same object through disconnect and reconnect.
	service.mu.Lock()
	service.tickets["stale"] = attachmentTicket{}
	service.mu.Unlock()
	if err := a.ClearManagedMCP(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if service.hasPublicURL() {
		t.Fatal("disabled route retained download authority")
	}
	if err := a.ConfigureManagedMCP(context.Background(), &ManagedMCPConfig{PublicURL: "https://new.example.com/mcp", Generation: 2}); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	current, count := service.publicURL, len(service.tickets)
	service.mu.Unlock()
	if a.mcp.attachments != service || current != "https://new.example.com/mcp" || count != 0 {
		t.Fatal("reconnect retained stale authority or replaced native service")
	}
}

type managedSendProvider struct {
	bridgeUploadProvider
	calls atomic.Int32
	fail  bool
}

func (p *managedSendProvider) SendMessage(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	p.calls.Add(1)
	if p.fail {
		return whatsmeow.SendResponse{}, errors.New("synthetic provider uncertainty")
	}
	return whatsmeow.SendResponse{ID: "SYNTHETIC-BRIDGE-SEND", Timestamp: time.Now()}, nil
}
func TestManagedMCPSendJournalReplayConflictAndUnknown(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "unknown"}[uncertain], func(t *testing.T) {
			a, connection, _, token, chat := outboundBridgeFixtureWithApp(t, managedTestApp(t))
			provider := &managedSendProvider{fail: uncertain}
			a.runtime = &bridgeOutboundConnector{syntheticConnector: &syntheticConnector{account: demoAccount, snapshot: ingestion.Status{Connection: "connected"}}, client: provider}
			h := a.ManagedMCPHandler()
			args := map[string]any{"chat_id": chat, "request_id": "managed-durable", "text": "Synthetic managed message"}
			want := "accepted"
			if uncertain {
				want = "outcome_unknown"
			}
			for i := 0; i < 2; i++ {
				w := managedCall(h, token, "send_message", args)
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
					t.Fatal("managed send/replay failed", w.Code, w.Body.String())
				}
			}
			if provider.calls.Load() != 1 {
				t.Fatal("same request replay reached provider twice")
			}
			args["text"] = "Conflicting payload"
			w := managedCall(h, token, "send_message", args)
			if strings.Contains(w.Body.String(), `"state":"accepted"`) || provider.calls.Load() != 1 {
				t.Fatal("request conflict reached provider")
			}
			if err := a.SetSendPermission(context.Background(), connection.ID, connection.Revision, nil); err != nil {
				t.Fatal(err)
			}
			args["request_id"] = "managed-denied"
			w = managedCall(h, token, "send_message", args)
			if provider.calls.Load() != 1 || strings.Contains(w.Body.String(), `"state":"accepted"`) {
				t.Fatal("read grant implied send authority")
			}
		})
	}
}

func TestManagedMCPUploadRevocationAndShutdown(t *testing.T) {
	for _, action := range []string{"permission", "revoke", "disable"} {
		t.Run(action, func(t *testing.T) {
			a, connection, _, token, chat := outboundBridgeFixtureWithApp(t, managedTestApp(t))
			provider := &bridgeUploadProvider{started: make(chan struct{}), released: make(chan struct{})}
			a.runtime = &bridgeOutboundConnector{syntheticConnector: &syntheticConnector{account: demoAccount, snapshot: ingestion.Status{Connection: "connected"}}, client: provider}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- managedCall(a.ManagedMCPHandler(), token, "send_media", map[string]any{"chat_id": chat, "request_id": "managed-upload", "kind": "document", "mime_type": "application/pdf", "filename": "synthetic.pdf", "data_base64": base64.StdEncoding.EncodeToString([]byte("%PDF-1.7 synthetic"))})
			}()
			defer close(provider.released)
			select {
			case <-provider.started:
			case <-time.After(3 * time.Second):
				t.Fatal("managed send did not enter upload")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var err error
			switch action {
			case "permission":
				err = a.SetSendPermission(ctx, connection.ID, connection.Revision, nil)
			case "revoke":
				err = a.RevokeConnection(ctx, localui.ConnectionRevokeRequest{ID: connection.ID, Revision: connection.Revision})
			case "disable":
				err = a.ClearManagedMCP(ctx, 1)
			}
			if err != nil {
				t.Fatal(err)
			}
			if action != "disable" {
				provider.released <- struct{}{}
			}
			select {
			case response := <-done:
				if provider.sends.Load() != 0 || strings.Contains(response.Body.String(), `"state":"accepted"`) {
					t.Fatal("withdrawn upload sent message")
				}
			case <-ctx.Done():
				t.Fatal("managed request failed to join")
			}
		})
	}
}

func TestAttachmentAuthorityChangeInvalidatesTicketsAndKeepsServiceUsable(t *testing.T) {
	s, grant, id, _ := attachmentRetainedFixture(t, 128, "https://archive.example/mcp")
	old, _, err := s.Prepare(context.Background(), grant, id, false, false)
	if err != nil || old.DownloadURL == "" {
		t.Fatal("retained fixture did not mint ticket", err)
	}
	if err := s.setPublicURL("https://new.example/mcp"); err != nil {
		t.Fatal(err)
	}
	if w := attachmentGET(s, strings.TrimPrefix(old.DownloadURL, "https://archive.example/mcp/attachments/")); w.Code != http.StatusNotFound {
		t.Fatal("old-authority ticket survived")
	}
	delivery, _, err := s.Prepare(context.Background(), grant, id, false, false)
	if err != nil || !strings.HasPrefix(delivery.DownloadURL, "https://new.example/mcp/attachments/") {
		t.Fatal("new authority not used", err, delivery)
	}
	token := strings.TrimPrefix(delivery.DownloadURL, "https://new.example/mcp/attachments/")
	if w := attachmentGET(s, token); w.Code != http.StatusOK {
		t.Fatal("rebound service cannot download", w.Code)
	}
	if err := s.setPublicURL(""); err != nil {
		t.Fatal(err)
	}
	if w := attachmentGET(s, token); w.Code != http.StatusNotFound {
		t.Fatal("disabled authority retained ticket")
	}
}

func TestManagedAuthorityReservationRejectsBrowserOffAndPreservesOtherOrigins(t *testing.T) {
	a := unixNativeTestApp(t)
	a.reservedRemoteOrigins = []string{"https://admin.example.com"}
	if err := a.ConfigureManagedMCP(context.Background(), &ManagedMCPConfig{PublicURL: "https://admin.example.com/mcp", Generation: 1}); err == nil {
		t.Fatal("MCP-only route claimed reserved legacy authority")
	}
	if a.ManagedMCPReady() || a.mcpConfig.Remote != nil {
		t.Fatal("conflicting authority was published")
	}
	if err := a.ConfigureManagedMCP(context.Background(), &ManagedMCPConfig{PublicURL: "https://mcp.example.com/mcp", Generation: 2}); err != nil || !a.ManagedMCPReady() {
		t.Fatal("independent managed authority refused", err)
	}
	original := []string{"https://admin.example.com"}
	copied := copyApplicationConfig(Config{ReservedRemoteOrigins: original})
	original[0] = "https://changed.example.com"
	if copied.ReservedRemoteOrigins[0] != "https://admin.example.com" {
		t.Fatal("reserved authority borrowed mutable caller configuration")
	}
}

func TestAttachmentInFlightAuthorityChangeDeniesOldBytes(t *testing.T) {
	s, a, _, _, token := attachmentServiceFixture(t)
	a.checkHook = func() {
		if err := s.setPublicURL("https://changed.example/mcp"); err != nil {
			t.Fatal(err)
		}
	}
	if response := attachmentGET(s, token); response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "%PDF") {
		t.Fatal("authority changed before response but old bytes escaped")
	}
}

// Exercise the handlers captured before provider discovery, then redeem both
// native- and remote-issued tickets through the real managed bearer route.
func TestManagedRetainedDocumentFromNativeAndRemoteHandlers(t *testing.T) {
	a := managedTestApp(t)
	ctx := context.Background()
	policy, err := a.store.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := prepareDemoAcceptance(ctx, a, policy.Generation)
	if owner != nil {
		defer owner.Close(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	_, token := managedGrant(t, a)
	grant, err := a.store.ResolveCredentialID(ctx, a.authState.RemoteClients[0].CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	page, _, err := a.mcp.attachments.Search(ctx, grant, archive.AttachmentSearchRequest{Query: demoDocumentCanary})
	if err != nil || len(page.Attachments) != 1 {
		t.Fatal("synthetic retained document missing", err)
	}
	id := page.Attachments[0].AttachmentID
	_, _, err = a.IssueNativeClientConfig(ctx, archive.CredentialCreate{Label: "native document", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, "", mcpserver.NativeAdapterEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	native, _, err := a.readNativeFile(a.authState.NativeClients[0].ConfigName)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return appdata.DialNativeSocket(ctx, a.mcp.native.Path())
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	for _, local := range []bool{false, true} {
		var payload []byte
		if local {
			body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "get_attachment", "arguments": map[string]any{"attachment_id": id}}})
			request, _ := http.NewRequest("POST", mcpserver.NativeAdapterEndpoint, bytes.NewReader(body))
			request.Header.Set("X-WA-Local-Adapter", native.Token)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			payload, err = io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != 200 {
				t.Fatal("native attachment unavailable", err, response.StatusCode)
			}
		} else {
			response := managedCall(a.ManagedMCPHandler(), token, "get_attachment", map[string]any{"attachment_id": id})
			payload = response.Body.Bytes()
		}
		var result struct {
			Result struct {
				Content struct {
					URL string `json:"download_url"`
				} `json:"structuredContent"`
			} `json:"result"`
		}
		if json.Unmarshal(payload, &result) != nil || !strings.HasPrefix(result.Result.Content.URL, "https://mcp.example.com/mcp/attachments/") {
			t.Fatal("handler lost managed attachment authority", local, string(payload))
		}
		request := httptest.NewRequest("GET", result.Result.Content.URL, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		a.ManagedMCPHandler().ServeHTTP(response, request)
		if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), demoAcceptanceDocument()) {
			t.Fatal("managed download lost retained bytes", local, response.Code, response.Body.String())
		}
	}
}
