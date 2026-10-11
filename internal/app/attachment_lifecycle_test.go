//go:build sqlite_fts5

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestAttachmentMCPPreservesLifetimeAcrossLocalAIChanges(t *testing.T) {
	a := unixNativeTestApp(t)
	ctx := context.Background()
	policy, err := a.store.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := proto.Marshal(&waE2E.DocumentMessage{DirectPath: proto.String("/synthetic-lifetime.enc"), MediaKey: bytes.Repeat([]byte{1}, 32), FileSHA256: bytes.Repeat([]byte{2}, 32), FileEncSHA256: bytes.Repeat([]byte{3}, 32), FileLength: proto.Uint64(32)})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := a.store.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: demoAccount, Chat: "synthetic-document-chat", Sender: "synthetic-document-sender", Message: "synthetic-lifetime-document"}, Kind: archive.EventOriginal, OriginalSentAt: time.Now().Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1, BodyKind: archive.MessageDocument, Media: []archive.MediaReference{{Kind: "document", ProtocolReference: descriptor, Filename: "synthetic-lifetime.pdf", MimeType: "application/pdf", DeclaredBytes: 32, Availability: "not_retained"}}}, policy.Generation)
	if err != nil || len(applied.MediaIDs) != 1 {
		t.Fatal("synthetic document association unavailable", err)
	}
	id, _, err := a.IssueNativeClientConfig(ctx, archive.CredentialCreate{Label: "synthetic lifetime client", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, "ignored", mcpserver.NativeAdapterEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	config, fileID, err := a.readNativeFile(a.authState.NativeClients[0].ConfigName)
	if err != nil || fileID != id {
		t.Fatal("synthetic native publication unavailable", err)
	}
	grant, err := a.store.ResolveCredential(ctx, sha256.Sum256([]byte(config.Token)))
	if err != nil {
		t.Fatal(err)
	}
	service := a.mcp.attachments
	transport := &http.Transport{Proxy: nil, DialContext: func(c context.Context, network, address string) (net.Conn, error) {
		return appdata.DialNativeSocket(c, config.NativeSocketPath)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	check := func() {
		t.Helper()
		page, _, err := service.Search(ctx, grant, archive.AttachmentSearchRequest{FileType: "document", Limit: 10})
		if err != nil || len(page.Attachments) != 1 {
			t.Fatal("attachment search lost its MCP archive lifetime", err)
		}
		delivery, _, err := service.Prepare(ctx, grant, applied.MediaIDs[0], false, false)
		if err != nil || delivery.Attachment.AttachmentID != applied.MediaIDs[0] {
			t.Fatal("attachment retrieval lost its MCP archive lifetime", err)
		}
		request, err := http.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_attachments","arguments":{"file_type":"document","limit":10}}}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("X-WA-Local-Adapter", config.Token)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("dedicated native transport lost its MCP archive lifetime", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 256<<10))
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || strings.Contains(string(body), `"isError":true`) || !strings.Contains(string(body), "synthetic-lifetime.pdf") {
			t.Fatal("dedicated native attachment search stopped after a query-context replacement")
		}
	}
	check()
	oldQuery := a.queryContext
	state, err := a.LocalAIState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetLocalAI(ctx, localui.LocalAIUpdate{Choice: localui.LocalAIChoice{Mode: "off"}, ExpectedRevision: state.Revision}); err != nil {
		t.Fatal(err)
	}
	if oldQuery.Err() == nil {
		t.Fatal("local AI update failed to cancel individual query work")
	}
	check()
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err = service.Search(ctx, grant, archive.AttachmentSearchRequest{}); err == nil {
		t.Fatal("archive close left the captured attachment service published")
	}
	if _, _, err = service.Prepare(ctx, grant, applied.MediaIDs[0], false, false); err == nil {
		t.Fatal("archive close left captured attachment retrieval published")
	}
}
