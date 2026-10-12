//go:build sqlite_fts5

package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func TestHistoricalSourceCurrentCredentialMutationsDiscardSerializedResponse(t *testing.T) {
	for _, mutation := range []string{"revoke", "narrow", "expire"} {
		t.Run(mutation, func(t *testing.T) {
			ctx := context.Background()
			start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			var clock atomic.Int64
			clock.Store(start.UnixNano())
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			store, err := archive.Open(ctx, dir, archive.Options{Now: func() time.Time { return time.Unix(0, clock.Load()).UTC() }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			policy, err := store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: "synthetic-account", Chat: "synthetic-chat", Sender: "synthetic-sender", Message: "synthetic-message"}, Kind: archive.EventOriginal, OriginalSentAt: start.Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1, Body: "synthetic message", BodyKind: archive.MessageText}, policy.Generation)
			if err != nil {
				t.Fatal(err)
			}
			credential, err := store.CreateCredential(ctx, archive.CredentialCreate{Label: "synthetic current authority", Principal: "synthetic-client", Digest: sha256.Sum256([]byte(fakeToken())), Policy: archive.CredentialPolicy{Account: "synthetic-account", AllChats: true}, ExpiresAt: start.Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			live := &sourceTestReader{generation: 1, body: "LIVE_SYNTHETIC"}
			historical := &sourceTestReader{generation: 73, body: "HISTORY_SYNTHETIC"}
			credentials := &interleavedCredentials{Credentials: store, mutate: func() error {
				switch mutation {
				case "revoke":
					return store.RevokeCredential(ctx, credential.ID, credential.Revision)
				case "narrow":
					_, e := store.SetCredentialPolicy(ctx, credential.ID, archive.CredentialPolicy{Account: "synthetic-account", AllChats: true, From: start}, credential.Revision)
					return e
				case "expire":
					clock.Store(start.Add(2 * time.Hour).UnixNano())
				}
				return nil
			}}
			server, _ := httpFixture(t, live, credentials, func(c *Config) { configureTestHistorical(t, c, &sourceTestHistoricalFactory{reader: historical}) })
			status, body := rpc(t, server, chatToolRPC(t, "search_messages", map[string]any{"query": "synthetic", "source": "historical"}), nil)
			if status != http.StatusUnauthorized || strings.Contains(body, "HISTORY_SYNTHETIC") || strings.Contains(body, "LIVE_SYNTHETIC") {
				t.Fatalf("current %s failed final serialization fence: %d %s", mutation, status, body)
			}
			if credentials.calls != 2 {
				t.Fatal("credential mutation did not occur between construction and delivery")
			}
			reads, checks := historical.counts()
			if reads != 1 || checks != 1 {
				t.Fatal("historical content was not constructed before current credential invalidation")
			}
		})
	}
}

// Independent synthetic stores exercise the real bridge with differing opaque
// chat IDs, current selective policy, and no credential in the historical DB.
func TestHistoricalSourceActualBridgeScopesCountsAndSourceBoundHandles(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	account := "synthetic-history-account"
	open := func() (*archive.Store, string, archive.ArchivePolicy) {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		store, err := archive.Open(ctx, dir, archive.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		policy, err := store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return store, dir, policy
	}
	live, _, livePolicy := open()
	writer, historyDir, historyPolicy := open()
	apply := func(store *archive.Store, policy archive.ArchivePolicy, id, chat, body string) archive.MessageID {
		result, err := store.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: account, Chat: chat, Sender: "synthetic-sender", Message: id}, Kind: archive.EventOriginal, OriginalSentAt: now.Add(-2 * time.Hour), OriginalDateKnown: true, SourceRevision: 1, Body: body, BodyKind: archive.MessageText}, policy.Generation)
		if err != nil {
			t.Fatal(err)
		}
		return result.MessageID
	}
	liveMessage := apply(live, livePolicy, "live-a", "synthetic-a", "LIVE_SYNTHETIC")
	apply(live, livePolicy, "live-b", "synthetic-b", "LIVE_SYNTHETIC")
	apply(writer, historyPolicy, "history-a1", "synthetic-a", "historical synthetic ALLOWED_ONE")
	apply(writer, historyPolicy, "history-a2", "synthetic-a", "historical synthetic ALLOWED_TWO")
	apply(writer, historyPolicy, "history-b", "synthetic-b", "historical synthetic FORBIDDEN_HISTORICAL")
	page, err := live.Search(ctx, archive.ReadScope{Account: account, Principal: "synthetic-owner", AllChats: true, PolicyGeneration: livePolicy.Generation}, archive.SearchRequest{Query: "LIVE_SYNTHETIC"})
	if err != nil {
		t.Fatal(err)
	}
	var selectedChat archive.ChatID
	for _, hit := range page.Hits {
		if hit.MessageID == liveMessage {
			selectedChat = hit.ChatID
		}
	}
	if selectedChat == "" {
		t.Fatal("synthetic chat mapping missing")
	}
	livePolicy, err = live.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsSelective, IncludedChats: []archive.ChatID{selectedChat}}, livePolicy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	historical, err := archive.OpenHistoricalSnapshot(ctx, historyDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { historical.Close() })
	metadata := archive.HistoricalViewMetadata{Source: "historical", SnapshotID: "0123456789abcdef", CapturedAt: now.Add(-time.Hour), ReadOnly: true, Coverage: "partial", CurrentPhoneState: "unknown"}
	factory, err := archive.NewHistoricalView(ctx, live, historical, metadata)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := live.CreateCredential(ctx, archive.CredentialCreate{Label: "new synthetic current credential", Principal: "synthetic-client", Digest: sha256.Sum256([]byte(fakeToken())), Policy: archive.CredentialPolicy{Account: account, AllChats: true}})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := live.ResolveCredentialID(ctx, credential.ID)
	if err != nil || !grant.Scope.AllChats {
		t.Fatal("fixture lost AllChats grant")
	}
	server, _ := httpFixture(t, live, live, func(c *Config) { c.HistoricalReads = factory })
	call := func(name string, args map[string]any) string {
		status, body := rpc(t, server, chatToolRPC(t, name, args), nil)
		if status != 200 || strings.Contains(body, `"isError":true`) {
			t.Fatalf("real selected bridge failed: %d %s", status, body)
		}
		if strings.Contains(body, "FORBIDDEN_HISTORICAL") {
			t.Fatal("current selective policy exposed a deselected historical chat")
		}
		return body
	}
	body := call("search_messages", map[string]any{"query": "historical", "source": "historical", "limit": 1})
	var result struct {
		Result struct {
			StructuredContent searchDTO `json:"structuredContent"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(body), &result) != nil {
		t.Fatal("invalid selected search")
	}
	hits := result.Result.StructuredContent
	if len(hits.Hits) != 1 || hits.NextCursor == "" || !strings.HasPrefix(string(hits.Hits[0].MessageID), "h1.0123456789abcdef.m.") || !strings.HasPrefix(string(hits.Hits[0].ChatID), "h1.0123456789abcdef.c.") {
		t.Fatal("historical result or pagination lost source binding")
	}
	call("get_context", map[string]any{"source": "historical", "message_id": hits.Hits[0].MessageID})
	call("search_messages", map[string]any{"source": "historical", "query": "historical", "chat_ids": []archive.ChatID{hits.Hits[0].ChatID}})
	statusBody := call("get_index_status", map[string]any{"source": "historical"})
	if !strings.Contains(statusBody, `"message_count":2`) || strings.Contains(statusBody, `"live_rebuild"`) || !strings.Contains(statusBody, `"connection":"historical_snapshot"`) {
		t.Fatalf("scoped captured status inaccurate: %s", statusBody)
	}
	chatBody := call("list_chats", map[string]any{"source": "historical"})
	var chats struct {
		Result struct {
			StructuredContent chatPageDTO `json:"structuredContent"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(chatBody), &chats) != nil || len(chats.Result.StructuredContent.Chats) != 1 || chats.Result.StructuredContent.Chats[0].MessageCount != 2 {
		t.Fatal("historical chat counts mixed current policy")
	}
	for _, bad := range []struct {
		name string
		args map[string]any
	}{
		{"get_context", map[string]any{"message_id": hits.Hits[0].MessageID}},
		{"get_context", map[string]any{"source": "historical", "message_id": liveMessage}},
		{"get_context", map[string]any{"source": "historical", "message_id": strings.Replace(string(hits.Hits[0].MessageID), "0123456789abcdef", "fedcba9876543210", 1)}},
		{"search_messages", map[string]any{"query": "historical", "cursor": hits.NextCursor}},
		{"search_messages", map[string]any{"source": "historical", "query": "historical", "cursor": strings.Replace(hits.NextCursor, "0123456789abcdef", "fedcba9876543210", 1)}},
	} {
		_, body := rpc(t, server, chatToolRPC(t, bad.name, bad.args), nil)
		if !strings.Contains(body, `"isError":true`) || strings.Contains(body, "ALLOWED_") {
			t.Fatal("foreign source/view identity returned content")
		}
	}
	// A new revision must not replay an old historical cursor under new scope.
	_, err = live.SetCredentialPolicy(ctx, credential.ID, archive.CredentialPolicy{Account: account, Chats: []archive.ChatID{selectedChat}}, credential.Revision)
	if err != nil {
		t.Fatal(err)
	}
	_, body = rpc(t, server, chatToolRPC(t, "search_messages", map[string]any{"source": "historical", "query": "historical", "cursor": hits.NextCursor}), nil)
	if !strings.Contains(body, `"isError":true`) || strings.Contains(body, "ALLOWED_") {
		t.Fatal("historical cursor survived current grant revision")
	}
}
