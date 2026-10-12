package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/clientsetup"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

func TestDockerConnectionGuideUsesTrustedCompositionAndPrivateGrant(t *testing.T) {
	ctx := context.Background()
	cfg := Config{DataDir: shortNativeRoot(t), Demo: true, Offline: true, ExecutablePath: "/unselected/${HOST_EXECUTABLE}", DockerContainerName: "trusted-wa-indexer"}
	a, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(ctx) })
	// Changing caller composition after construction cannot redirect a guide.
	cfg.DockerContainerName = "caller-mutated"
	policy, err := a.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.seedDemo(ctx, policy.Generation); err != nil {
		t.Fatal(err)
	}
	a.mcpEnabled = true
	if err := a.startMCP(ctx); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour)
	connection, err := a.CreateConnection(ctx, localui.ConnectionRequest{Client: "claude_code", Label: "scoped Docker client", Mode: "local", AllChats: true, ExpiresAt: &expiry})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.authState.NativeClients) != 1 || connection.Revision == 0 {
		t.Fatal("missing independently persisted grant")
	}
	record := a.authState.NativeClients[0]
	private, credentialID, err := a.readNativeFile(record.ConfigName)
	if err != nil || credentialID != record.CredentialID || private.Token == "" {
		t.Fatal("missing private credential file")
	}
	guide, err := a.ConnectionGuide(ctx, localui.ConnectionGuideRequest{ID: connection.ID, Client: "claude_code", Mode: "local"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(guide.Configuration), &decoded); err != nil {
		t.Fatal(err)
	}
	server := decoded.Servers["wa_indexer"]
	want := []string{"exec", "-i", "trusted-wa-indexer", "/usr/local/bin/wa-indexer", "mcp", "stdio", "--config", filepath.Join(a.paths.ClientDir, record.ConfigName)}
	if server.Command != "docker" || !reflect.DeepEqual(server.Args, want) {
		t.Fatalf("untrusted or wrong bridge: %q %#v", server.Command, server.Args)
	}
	encoded, err := json.Marshal(guide)
	if err != nil || strings.Contains(string(encoded), private.Token) || strings.Contains(string(encoded), "HOST_EXECUTABLE") || strings.Contains(string(encoded), cfg.DockerContainerName) || guide.Status != clientsetup.NotYetVerified {
		t.Fatal("guide leaked private credential or changed trusted composition/evidence status")
	}
	if err := a.RevokeConnection(ctx, localui.ConnectionRevokeRequest{ID: connection.ID, Revision: connection.Revision}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.ResolveCredentialID(ctx, credentialID); !errors.Is(err, archive.ErrUnauthorized) {
		t.Fatalf("revoked Docker grant still resolved: %v", err)
	}
	if _, err := a.ConnectionGuide(ctx, localui.ConnectionGuideRequest{ID: connection.ID, Client: "claude_code", Mode: "local"}); err == nil {
		t.Fatal("revoked private bridge still rendered")
	}
}
