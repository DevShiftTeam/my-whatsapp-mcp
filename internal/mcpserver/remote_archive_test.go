//go:build sqlite_fts5

package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestActualRemoteReadyFileScopeAndCredentialRestart(t *testing.T) {
	f := openActualFixture(t)
	root, _ := privateTestRoot(t)
	credential, err := IssueRemoteCredentialFile(context.Background(), f.store, archive.CredentialCreate{Label: "synthetic ready remote", Principal: remoteFixturePrincipal, Policy: archive.CredentialPolicy{Account: f.account, Chats: []archive.ChatID{f.chatA}, Rolling: time.Hour}, ExpiresAt: f.now.Add(time.Hour)}, root, "remote.json", remoteFileEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	config, id, err := ReadRemoteCredentialFileRoot(root, "remote.json")
	if err != nil || id != credential.ID {
		t.Fatal("persistent remote handoff identity mismatch")
	}
	grant, err := f.store.ResolveCredential(context.Background(), sha256.Sum256([]byte(config.Token)))
	if err != nil || grant.CredentialID != id || grant.Scope.Account != f.account || grant.Scope.Principal != remoteFixturePrincipal {
		t.Fatal("file token did not resolve exact persisted remote grant")
	}
	registry := remoteRegistryFixture()
	for _, reopened := range []bool{false, true} {
		if reopened {
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			f.store, err = archive.Open(context.Background(), f.dir, archive.Options{Now: func() time.Time { return f.now }})
			if err != nil {
				t.Fatal(err)
			}
		}
		server, _ := httpFixture(t, f.store, f.store, func(config *Config) { config.RemoteClients = registry })
		if !reopened {
			status, _ := rpc(t, server, statusRPC, func(request *http.Request) { request.Header.Set("Authorization", "Bearer "+config.Token) })
			if status != http.StatusUnauthorized {
				t.Fatal("pending persistent remote grant was accepted")
			}
			registry.publish(remoteBinding(grant))
		}
		for _, mode := range []AuthMode{LocalAdapter, Access} {
			request, _ := http.NewRequest(http.MethodPost, "http://localhost/mcp", nil)
			if mode == LocalAdapter {
				request.Header.Set(localAdapterHeader, config.Token)
			} else {
				request.Header.Set(accessHeader, "synthetic-verified-assertion")
			}
			if _, err := authenticate(request, Config{Mode: mode, NativeClients: nativeRegistryFixture(grant), RemoteClients: registry}, f.store, &fixtureAccessVerifier{id: id}); err != ErrUnauthorized {
				t.Fatal("persisted remote credential crossed authentication family")
			}
		}
		session, ctx := sdkSession(t, server, config.Token)
		var state statusDTO
		if json.Unmarshal(callActual(t, session, ctx, "get_index_status", map[string]any{}), &state) != nil || state.MessageCount != 3 || len(state.Chats) != 1 {
			t.Fatal("remote status escaped persisted finite/date scope")
		}
		search := callActual(t, session, ctx, "search_messages", map[string]any{"query": "archive-match", "limit": 20})
		var hits searchDTO
		if json.Unmarshal(search, &hits) != nil || len(hits.Hits) != 3 || !strings.Contains(string(search), "שָׁלוֹם 1,250 https://example.invalid/demo") {
			t.Fatal("remote keyword result lost original logical text/scope")
		}
		for _, canary := range []string{f.account, "forbidden-body-canary", "old-client-filter-canary", config.Token, config.PublicURL, f.dir} {
			if strings.Contains(string(search), canary) {
				t.Fatal("remote result leaked excluded source/credential metadata")
			}
		}
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_context", Arguments: map[string]any{"message_id": f.denied}})
		if err != nil || !res.IsError {
			t.Fatal("remote context guessed denied opaque message")
		}
		session.Close()
		server.Close()
	}
}

func TestActualRemoteTransportRechecksExpiryRevokeAndScope(t *testing.T) {
	for _, operation := range []string{"expiry", "revoke", "narrow"} {
		t.Run(operation, func(t *testing.T) {
			f := openActualFixture(t)
			token := base64.RawURLEncoding.EncodeToString(bytesOf(9, 32))
			credential, err := f.store.CreateCredential(context.Background(), archive.CredentialCreate{Label: "synthetic remote expiry", Principal: remoteFixturePrincipal, Digest: sha256.Sum256([]byte(token)), Policy: archive.CredentialPolicy{Account: f.account, Chats: []archive.ChatID{f.chatA}}, ExpiresAt: f.now.Add(time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			grant, err := f.store.ResolveCredential(context.Background(), sha256.Sum256([]byte(token)))
			if err != nil {
				t.Fatal(err)
			}
			server, _ := httpFixture(t, f.store, f.store, func(config *Config) { config.RemoteClients = remoteRegistryFixture(grant) })
			session, ctx := sdkSession(t, server, token)
			_ = callActual(t, session, ctx, "get_context", map[string]any{"message_id": f.target})
			switch operation {
			case "expiry":
				f.now = f.now.Add(2 * time.Minute)
			case "revoke":
				if f.store.RevokeCredential(context.Background(), credential.ID, credential.Revision) != nil {
					t.Fatal("persistent remote revocation failed")
				}
			case "narrow":
				if _, err := f.store.SetCredentialPolicy(context.Background(), credential.ID, archive.CredentialPolicy{Account: f.account, Chats: []archive.ChatID{f.chatB}}, credential.Revision); err != nil {
					t.Fatal(err)
				}
			}
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_context", Arguments: map[string]any{"message_id": f.target}})
			if err == nil && (res == nil || !res.IsError) {
				t.Fatal("remote session reused expired/revoked/broadened authority")
			}
			if res != nil {
				data, _ := json.Marshal(res)
				if strings.Contains(string(data), "permitted original target") {
					t.Fatal("stale remote transport leaked original")
				}
			}
			session.Close()
		})
	}
}
