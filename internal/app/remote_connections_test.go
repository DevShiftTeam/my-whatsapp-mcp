package app

import (
	"context"
	"encoding/json"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func remoteConnectionTestApp(t *testing.T) *Application {
	t.Helper()
	a := unixNativeTestApp(t)
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := probe.Addr().String()
	if err = probe.Close(); err != nil {
		t.Fatal(err)
	}
	a.mcpConfig.Remote = &RemoteMCPConfig{Listen: listen, PublicURL: "https://mcp.example.com/mcp", Server: mcpserver.Config{Mode: mcpserver.Bearer, Hosts: []string{"mcp.example.com"}, Origins: []string{"https://mcp.example.com"}}}
	if err = a.startMCP(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRemoteConnectionUsesIndependentGrantAndExplicitCurrentRevisionReveal(t *testing.T) {
	a := remoteConnectionTestApp(t)
	expiry := time.Now().Add(time.Hour)
	request := localui.ConnectionRequest{Client: "codex_cli", Label: "remote scoped", Mode: "remote", AllChats: true, ExpiresAt: &expiry}
	connection, err := a.CreateConnection(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if connection.Mode != "remote" || connection.ID == "" || connection.Revision == 0 || !connection.Scope.AllChats || connection.ExpiresAt == nil || connection.ExpiresAt.After(expiry) {
		t.Fatal("remote grant broadened request or omitted identity")
	}
	if len(a.authState.NativeClients) != 0 || len(a.authState.RemoteClients) != 1 {
		t.Fatal("remote issuance reused native journal")
	}
	record := a.authState.RemoteClients[0]
	if !strings.HasPrefix(record.Principal, archive.RemoteClientPrincipalPrefix) || a.nativeRegistry.PublishedNativeClient(mcpserver.NativeClientBinding{Account: record.Account, Principal: record.Principal, CredentialID: record.CredentialID}) {
		t.Fatal("remote grant crossed native authority")
	}
	secret, err := a.RevealRemoteCredential(context.Background(), localui.ConnectionRevealRequest{ID: connection.ID, Revision: connection.Revision})
	if err != nil || secret.Token == "" || secret.PublicURL != a.mcpConfig.Remote.PublicURL {
		t.Fatal("explicit protected reveal failed")
	}
	encoded, err := json.Marshal(a.authState)
	if err != nil || strings.Contains(string(encoded), secret.Token) {
		t.Fatal("raw token persisted in state")
	}
	snapshot, err := a.Connections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(snapshot)
	if err != nil || strings.Contains(string(encoded), secret.Token) || strings.Contains(string(encoded), record.Principal) {
		t.Fatal("snapshot leaked private authority")
	}
	guide, err := a.ConnectionGuide(context.Background(), localui.ConnectionGuideRequest{ID: connection.ID, Client: "codex_cli", Mode: "remote"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(guide)
	if err != nil || strings.Contains(string(encoded), secret.Token) {
		t.Fatal("inert guide leaked token")
	}
	if err = a.RevokeConnection(context.Background(), localui.ConnectionRevokeRequest{ID: connection.ID, Revision: connection.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.RevealRemoteCredential(context.Background(), localui.ConnectionRevealRequest{ID: connection.ID, Revision: connection.Revision}); err == nil {
		t.Fatal("revoked secret remained revealable")
	}
}

func TestUnsupportedRemoteHeaderClientCreatesNoIntent(t *testing.T) {
	a := remoteConnectionTestApp(t)
	expiry := time.Now().Add(time.Hour)
	_, err := a.CreateConnection(context.Background(), localui.ConnectionRequest{Client: "claude_connectors", Label: "unsupported", Mode: "remote", AllChats: true, ExpiresAt: &expiry})
	if err != localui.ErrConnectionUnsupported || len(a.authState.RemoteClients) != 0 {
		t.Fatal("unsupported client consumed credential authority")
	}
}

func TestActualRemoteListenerAcceptsRemoteGrantAndRejectsNativeToken(t *testing.T) {
	a := remoteConnectionTestApp(t)
	expiry := time.Now().Add(time.Hour)
	connection, err := a.CreateConnection(context.Background(), localui.ConnectionRequest{Client: "codex_cli", Label: "actual remote", Mode: "remote", AllChats: true, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	secret, err := a.RevealRemoteCredential(context.Background(), localui.ConnectionRevealRequest{ID: connection.ID, Revision: connection.Revision})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = a.IssueNativeClientConfig(context.Background(), archive.CredentialCreate{Label: "native separate", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: expiry}, "", mcpserver.NativeAdapterEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	native, _, err := a.readNativeFile(a.authState.NativeClients[0].ConfigName)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	call := func(token string) int {
		request, err := http.NewRequest("POST", "http://"+a.mcpConfig.Remote.Listen+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"bounded-app-test","version":"1"}}}`))
		if err != nil {
			t.Fatal("bounded request construction failed")
		}
		request.Host = "mcp.example.com"
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("bounded local remote-listener request failed")
		}
		_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 65537))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatal("bounded response cleanup failed")
		}
		return response.StatusCode
	}
	if status := call(secret.Token); status != http.StatusOK {
		t.Fatalf("remote listener did not initialize: status %d", status)
	}
	if status := call(native.Token); status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("native token crossed remote listener: status %d", status)
	}
}

func TestPendingRemoteGrantRecoversWithoutDisablingNativeTransport(t *testing.T) {
	a := remoteConnectionTestApp(t)
	expiry := time.Now().Add(time.Hour)
	connection, err := a.CreateConnection(context.Background(), localui.ConnectionRequest{Client: "codex_cli", Label: "interrupted remote", Mode: "remote", AllChats: true, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	record := a.authState.RemoteClients[0]
	a.authState.RemoteClients[0].State = "pending"
	a.remoteRecoveryPending = true
	a.remoteRegistry.replace(nil)
	if err = a.persistPaused(a.paused); err != nil {
		t.Fatal(err)
	}
	if err = a.startMCP(context.Background()); err != nil {
		t.Fatal(err)
	}
	local, remote := a.mcp.status()
	if local != "ready" || remote != "ready" || len(a.authState.RemoteClients) != 0 || a.remoteRecoveryPending {
		t.Fatal("pending recovery hid healthy native or required restart")
	}
	metadata, err := a.listRemote(context.Background(), record.Account, record.Principal)
	if err != nil || len(metadata) != 1 || !metadata[0].Revoked {
		t.Fatal("pending remote grant remained active")
	}
	if _, err = a.RevealRemoteCredential(context.Background(), localui.ConnectionRevealRequest{ID: connection.ID, Revision: connection.Revision}); err == nil {
		t.Fatal("pending recovered secret published")
	}
}

func TestInvalidNativeJournalLeavesReadyRemoteUseAvailable(t *testing.T) {
	a := remoteConnectionTestApp(t)
	expiry := time.Now().Add(time.Hour)
	connection, err := a.CreateConnection(context.Background(), localui.ConnectionRequest{Client: "codex_cli", Label: "independent remote", Mode: "remote", AllChats: true, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	a.authState.NativeInvalid = true
	a.authState.NativeRaw = json.RawMessage(`{"untrusted":"unknown native journal"}`)
	if err = a.startMCP(context.Background()); err != nil {
		t.Fatal("native recovery failure disabled remote listener")
	}
	if _, err = a.RevealRemoteCredential(context.Background(), localui.ConnectionRevealRequest{ID: connection.ID, Revision: connection.Revision}); err != nil {
		t.Fatal("native corruption disabled existing remote use")
	}
	snapshot, err := a.Connections(context.Background())
	if err != nil || snapshot.LocalRecoveryState != "invalid" || snapshot.RemoteRecoveryState != "ready" {
		t.Fatal("family recovery state was broadened")
	}
}
