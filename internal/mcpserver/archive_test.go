//go:build sqlite_fts5

package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type actualFixture struct {
	store                              *archive.Store
	dir                                string
	policy                             archive.ArchivePolicy
	now                                time.Time
	account                            string
	chatA, chatB                       archive.ChatID
	quote, target, hiddenReply, denied archive.MessageID
	quoteKey, targetKey, deniedKey     archive.SourceKey
	remote, native                     archive.Credential
	token                              string
}

func openActualFixture(t *testing.T) *actualFixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &actualFixture{dir: dir, now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC), account: "source-account-canary@s.whatsapp.net", token: fakeToken()}
	f.store, err = archive.Open(context.Background(), dir, archive.Options{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	f.policy, err = f.store.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	apply := func(id, chat, body string, sent time.Time, reply *archive.SourceKey) (archive.MessageID, archive.SourceKey) {
		key := archive.SourceKey{Account: f.account, Chat: chat, Sender: "source-sender-canary@s.whatsapp.net", Message: id}
		r, err := f.store.ApplySourceEvent(context.Background(), archive.SourceEvent{Key: key, Kind: archive.EventOriginal, OriginalSentAt: sent, OriginalDateKnown: true, SourceRevision: 1, Body: body, BodyKind: archive.MessageText, ReplyTo: reply}, f.policy.Generation)
		if err != nil {
			t.Fatal(err)
		}
		return r.MessageID, key
	}
	_, _ = apply("old-protocol-canary", "source-chat-a@g.us", "archive-match old-client-filter-canary", f.now.Add(-2*time.Hour), nil)
	f.quote, f.quoteKey = apply("quote-protocol-canary", "source-chat-a@g.us", "archive-match שָׁלוֹם 1,250 https://example.invalid/demo", f.now.Add(-10*time.Minute), nil)
	f.denied, f.deniedKey = apply("denied-protocol-canary", "source-chat-b@g.us", "archive-match forbidden-body-canary", f.now.Add(-5*time.Minute), nil)
	f.target, f.targetKey = apply("target-protocol-canary", "source-chat-a@g.us", "archive-match permitted original target", f.now.Add(-time.Minute), &f.quoteKey)
	f.hiddenReply, _ = apply("reply-protocol-canary", "source-chat-a@g.us", "archive-match permitted reply", f.now.Add(-30*time.Second), &f.deniedKey)
	scope := archive.ReadScope{Account: f.account, Principal: "owner-fixture", PolicyGeneration: f.policy.Generation, AllChats: true}
	p, err := f.store.Search(context.Background(), scope, archive.SearchRequest{Query: "archive-match", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range p.Hits {
		if h.MessageID == f.target {
			f.chatA = h.ChatID
		}
		if h.MessageID == f.denied {
			f.chatB = h.ChatID
		}
	}
	if f.chatA == "" || f.chatB == "" {
		t.Fatal("fixture missing opaque chat identity")
	}
	f.remote, err = f.store.CreateCredential(context.Background(), archive.CredentialCreate{Label: "Synthetic scoped client", Principal: "client:scoped", Digest: sha256.Sum256([]byte(f.token)), Policy: archive.CredentialPolicy{Account: f.account, Chats: []archive.ChatID{f.chatA}, Rolling: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	nativeRaw := base64.RawURLEncoding.EncodeToString(bytesOf(1, 32))
	f.native, err = f.store.CreateCredential(context.Background(), archive.CredentialCreate{Label: "Synthetic local adapter", Principal: nativeFixturePrincipal, Digest: sha256.Sum256([]byte(nativeRaw)), Policy: archive.CredentialPolicy{Account: f.account, AllChats: true}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func bytesOf(v byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = v
	}
	return b
}

func actualNativeRegistry(f *actualFixture) *fakeNativeRegistry {
	return nativeRegistryFixture(archive.CredentialGrant{CredentialID: f.native.ID, CredentialRevision: f.native.Revision, Scope: archive.ReadScope{Account: f.native.Policy.Account, Principal: f.native.Principal}})
}

func callActual(t *testing.T, session *mcp.ClientSession, ctx context.Context, name string, args any) []byte {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || res.IsError {
		t.Fatalf("synthetic archive tool failed: %v", err)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestActualArchiveToolsScopeCountsRepliesAndRestart(t *testing.T) {
	f := openActualFixture(t)
	for _, reopened := range []bool{false, true} {
		if reopened {
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := archive.Open(context.Background(), f.dir, archive.Options{Now: func() time.Time { return f.now }})
			if err != nil {
				t.Fatal(err)
			}
			f.store = s
		}
		s, _ := httpFixture(t, f.store, f.store, func(c *Config) { c.LocalCredentialID = f.native.ID })
		session, ctx := sdkSession(t, s, f.token)
		search := callActual(t, session, ctx, "search_messages", map[string]any{"query": "archive-match", "limit": 20})
		var hits searchDTO
		if json.Unmarshal(search, &hits) != nil || len(hits.Hits) != 3 || !hits.Untrusted {
			t.Fatal("real FTS search escaped date/chat grant")
		}
		if !strings.Contains(string(search), "שָׁלוֹם 1,250 https://example.invalid/demo") {
			t.Fatal("source text logical order changed")
		}
		status := callActual(t, session, ctx, "get_index_status", map[string]any{})
		var state statusDTO
		if json.Unmarshal(status, &state) != nil || state.MessageCount != 3 || len(state.Chats) != 1 {
			t.Fatal("status counts were global instead of scoped")
		}
		chats := callActual(t, session, ctx, "list_chats", map[string]any{})
		var list chatPageDTO
		if json.Unmarshal(chats, &list) != nil || len(list.Chats) != 1 || list.Chats[0].ChatID != f.chatA || list.Chats[0].MessageCount != 3 {
			t.Fatal("chat metadata escaped scope")
		}
		contextPage := callActual(t, session, ctx, "get_context", map[string]any{"message_id": f.target, "before": 0, "after": 0})
		var messages contextDTO
		if json.Unmarshal(contextPage, &messages) != nil || len(messages.Messages) != 1 || messages.Messages[0].ReplyTo != f.quote || !strings.Contains(messages.Messages[0].ReplyQuote, "שָׁלוֹם") {
			t.Fatal("authorized reply provenance lost")
		}
		hidden := callActual(t, session, ctx, "get_context", map[string]any{"message_id": f.hiddenReply, "before": 0, "after": 0})
		if strings.Contains(string(hidden), "forbidden-body-canary") || strings.Contains(string(hidden), string(f.denied)) {
			t.Fatal("reply borrowed denied source authority")
		}
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_context", Arguments: map[string]any{"message_id": f.denied}})
		if err != nil || !res.IsError {
			t.Fatal("guessed opaque ID accessed denied chat")
		}
		for _, payload := range [][]byte{search, status, chats, contextPage, hidden} {
			for _, canary := range []string{f.account, "source-sender-canary", "source-chat-a@g.us", "source-chat-b@g.us", "protocol-canary", "forbidden-body-canary", "old-client-filter-canary", f.dir, f.token} {
				if strings.Contains(string(payload), canary) {
					t.Fatal("archive response exposed private metadata or excluded content")
				}
			}
		}
		session.Close()
		s.Close()
	}
}

func TestActualTransportDoesNotCacheRevokedOrNarrowedGrant(t *testing.T) {
	for _, operation := range []string{"revoke", "narrow", "archive_policy"} {
		t.Run(operation, func(t *testing.T) {
			f := openActualFixture(t)
			s, _ := httpFixture(t, f.store, f.store, func(c *Config) { c.LocalCredentialID = f.native.ID })
			session, ctx := sdkSession(t, s, f.token)
			_ = callActual(t, session, ctx, "search_messages", map[string]any{"query": "archive-match"})
			switch operation {
			case "revoke":
				if err := f.store.RevokeCredential(context.Background(), f.remote.ID, f.remote.Revision); err != nil {
					t.Fatal(err)
				}
			case "narrow":
				if _, err := f.store.SetCredentialPolicy(context.Background(), f.remote.ID, archive.CredentialPolicy{Account: f.account, Chats: []archive.ChatID{f.chatB}}, f.remote.Revision); err != nil {
					t.Fatal(err)
				}
			case "archive_policy":
				if _, err := f.store.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsSelective, IncludedChats: []archive.ChatID{f.chatB}}, f.policy.Generation); err != nil {
					t.Fatal(err)
				}
			}
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_context", Arguments: map[string]any{"message_id": f.target}})
			if err == nil && (res == nil || !res.IsError) {
				t.Fatal("transport reused old authority after persisted change")
			}
			if res != nil {
				payload, _ := json.Marshal(res)
				if strings.Contains(string(payload), "permitted original target") {
					t.Fatal("stale transport leaked source content")
				}
			}
		})
	}
}

func TestActualPersistedCredentialsCannotReplayBetweenListeners(t *testing.T) {
	f := openActualFixture(t)
	nativeToken := base64.RawURLEncoding.EncodeToString(bytesOf(1, 32))
	for _, mode := range []AuthMode{LocalAdapter, Bearer} {
		s, _ := httpFixture(t, f.store, f.store, func(c *Config) { c.Mode = mode; c.NativeClients = actualNativeRegistry(f) })
		for _, localToken := range []bool{false, true} {
			raw := f.token
			if localToken {
				raw = nativeToken
			}
			status, _ := rpc(t, s, statusRPC, func(r *http.Request) {
				r.Header.Del("Authorization")
				if mode == LocalAdapter {
					r.Host = "localhost"
					r.Header.Set(localAdapterHeader, raw)
				} else {
					r.Header.Set("Authorization", "Bearer "+raw)
				}
			})
			allowed := (mode == LocalAdapter) == localToken
			if (status == 200) != allowed {
				t.Fatal("persisted native grant crossed listener boundary")
			}
		}
	}
}

func TestActualNativeRegistrySeparatesMultipleReadyClientsAndOrphans(t *testing.T) {
	f := openActualFixture(t)
	secondToken := base64.RawURLEncoding.EncodeToString(bytesOf(2, 32))
	second, err := f.store.CreateCredential(context.Background(), archive.CredentialCreate{Label: "Second synthetic native client", Principal: archive.NativeClientPrincipalPrefix + "v1/11111111111111111111111111111111", Digest: sha256.Sum256([]byte(secondToken)), Policy: archive.CredentialPolicy{Account: f.account, Chats: []archive.ChatID{f.chatA}}})
	if err != nil {
		t.Fatal(err)
	}
	orphanToken := base64.RawURLEncoding.EncodeToString(bytesOf(3, 32))
	orphan, err := f.store.CreateCredential(context.Background(), archive.CredentialCreate{Label: "Unpublished synthetic native client", Principal: archive.NativeClientPrincipalPrefix + "v1/22222222222222222222222222222222", Digest: sha256.Sum256([]byte(orphanToken)), Policy: archive.CredentialPolicy{Account: f.account, AllChats: true}})
	if err != nil {
		t.Fatal(err)
	}
	bindingFor := func(credential archive.Credential) NativeClientBinding {
		return NativeClientBinding{Account: credential.Policy.Account, Principal: credential.Principal, CredentialID: credential.ID}
	}
	registry := actualNativeRegistry(f)
	registry.publish(bindingFor(second))
	local, _ := httpFixture(t, f.store, f.store, func(config *Config) {
		config.Mode = LocalAdapter
		config.NativeClients = registry
	})
	for _, fixture := range []struct {
		token string
		count int64
	}{{base64.RawURLEncoding.EncodeToString(bytesOf(1, 32)), 5}, {secondToken, 4}} {
		status, body := rpc(t, local, statusRPC, func(request *http.Request) {
			request.Host = "localhost"
			request.Header.Del("Authorization")
			request.Header.Set(localAdapterHeader, fixture.token)
		})
		var result struct {
			Result struct {
				Status statusDTO `json:"structuredContent"`
			} `json:"result"`
		}
		if status != http.StatusOK || json.Unmarshal([]byte(body), &result) != nil || result.Result.Status.MessageCount != fixture.count {
			t.Fatal("published clients did not retain independent scoped counts")
		}
	}
	status, body := rpc(t, local, statusRPC, func(request *http.Request) {
		request.Host = "localhost"
		request.Header.Del("Authorization")
		request.Header.Set(localAdapterHeader, orphanToken)
	})
	if status != http.StatusUnauthorized || body != `{"error":"mcp_unauthorized"}` {
		t.Fatal("unpublished native grant acquired local authority")
	}
	remote, _ := httpFixture(t, f.store, f.store, nil)
	status, body = rpc(t, remote, statusRPC, func(request *http.Request) {
		request.Header.Set("Authorization", "Bearer "+orphanToken)
	})
	if status != http.StatusUnauthorized || body != `{"error":"mcp_unauthorized"}` {
		t.Fatal("unbound native grant replayed remotely without a known native ID")
	}
	request, _ := http.NewRequest(http.MethodPost, remote.URL+"/mcp", nil)
	request.Header.Set(accessHeader, "synthetic-verified-native-assertion")
	if _, err := authenticate(request, Config{Mode: Access}, f.store, &fixtureAccessVerifier{id: orphan.ID}); err != ErrUnauthorized {
		t.Fatal("Access mapping granted an unpublished native credential")
	}
	registry.publish(bindingFor(orphan))
	status, _ = rpc(t, local, statusRPC, func(request *http.Request) {
		request.Host = "localhost"
		request.Header.Del("Authorization")
		request.Header.Set(localAdapterHeader, orphanToken)
	})
	if status != http.StatusOK {
		t.Fatal("live published binding required rebuilding the native listener")
	}
	if err := f.store.RevokeCredential(context.Background(), second.ID, second.Revision); err != nil {
		t.Fatal(err)
	}
	registry.remove(bindingFor(second))
	status, _ = rpc(t, local, statusRPC, func(request *http.Request) {
		request.Host = "localhost"
		request.Header.Del("Authorization")
		request.Header.Set(localAdapterHeader, secondToken)
	})
	if status != http.StatusUnauthorized {
		t.Fatal("durably revoked native client retained local authority")
	}
}

func TestSDKStdioForwardsActualArchiveWithoutOwningConnector(t *testing.T) {
	f := openActualFixture(t)
	nativeToken := base64.RawURLEncoding.EncodeToString(bytesOf(1, 32))
	handler, err := NewHandler(Config{Mode: LocalAdapter, Hosts: []string{"localhost"}, NativeClients: actualNativeRegistry(f)}, f.store, f.store)
	if err != nil {
		t.Fatal(err)
	}
	daemon := newStdioUnixDaemon(t, handler)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- runStdioTransport(ctx, daemon.adapterConfig(nativeToken), serverTransport)
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "synthetic-native-archive", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	status := callActual(t, session, ctx, "get_index_status", map[string]any{})
	var p statusDTO
	if json.Unmarshal(status, &p) != nil || p.MessageCount != 5 {
		t.Fatal("stdio failed to forward actual native grant")
	}
	if err = f.store.RevokeCredential(context.Background(), f.native.ID, f.native.Revision); err != nil {
		t.Fatal(err)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "search_messages", Arguments: map[string]any{"query": "archive-match"}})
	if err != nil || !res.IsError {
		t.Fatal("native forwarding cached revoked archive authority")
	}
	session.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("native forwarding did not stop")
	}
}

type interleavedCredentials struct {
	Credentials
	calls  int
	mutate func() error
}

func (c *interleavedCredentials) CheckCredentialGrant(ctx context.Context, g archive.CredentialGrant) error {
	c.calls++
	if c.calls == 2 {
		if err := c.mutate(); err != nil {
			return err
		}
	}
	return c.Credentials.CheckCredentialGrant(ctx, g)
}

func TestActualReplyContributorDeletionInvalidatesFinalHTTPResponse(t *testing.T) {
	f := openActualFixture(t)
	mutation := make(chan error, 1)
	credentials := &interleavedCredentials{Credentials: f.store, mutate: func() error {
		_, err := f.store.ApplySourceEvent(context.Background(), archive.SourceEvent{Key: f.quoteKey, Kind: archive.EventDelete, SourceRevision: 2}, f.policy.Generation)
		mutation <- err
		return err
	}}
	s, _ := httpFixture(t, f.store, credentials, func(c *Config) { c.LocalCredentialID = f.native.ID })
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_context","arguments":{"message_id":"` + string(f.target) + `","before":0,"after":0}}}`
	status, payload := rpc(t, s, body, nil)
	select {
	case err := <-mutation:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("reply mutation hook did not execute")
	}
	if status != 409 || strings.Contains(payload, "permitted original target") || strings.Contains(payload, "שָׁלוֹם") {
		t.Fatal("deleted reply contributor escaped final source guard")
	}
}
