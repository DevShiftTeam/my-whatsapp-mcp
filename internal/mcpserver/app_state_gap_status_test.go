//go:build sqlite_fts5

package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Hosted clients must exercise a registered remote bearer grant, not an
// unreserved fixture principal accepted through the compatibility path.
func actualGapRemoteServer(t *testing.T, f *actualFixture, reader archive.Reader) *httptest.Server {
	t.Helper()
	f.token = base64.RawURLEncoding.EncodeToString(bytesOf(23, 32))
	var err error
	f.remote, err = f.store.CreateCredential(context.Background(), archive.CredentialCreate{
		Label: "Synthetic remote readable snapshot", Principal: remoteFixturePrincipal,
		Digest:    sha256.Sum256([]byte(f.token)),
		Policy:    archive.CredentialPolicy{Account: f.account, Chats: []archive.ChatID{f.chatA}, Rolling: time.Hour},
		ExpiresAt: f.now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := f.store.ResolveCredential(context.Background(), sha256.Sum256([]byte(f.token)))
	if err != nil {
		t.Fatal(err)
	}
	server, _ := httpFixture(t, reader, f.store, func(c *Config) {
		c.LocalCredentialID = f.native.ID
		c.RemoteClients = remoteRegistryFixture(grant)
	})
	return server
}

func actualGapRemoteSession(t *testing.T, f *actualFixture, reader archive.Reader) (*mcp.ClientSession, context.Context) {
	t.Helper()
	server := actualGapRemoteServer(t, f, reader)
	return sdkSession(t, server, f.token)
}

func actualGapRecord(t *testing.T, f *actualFixture, connection archive.ConnectionState, codes ...string) {
	t.Helper()
	if err := f.store.SetSyncStatus(context.Background(), archive.SyncUpdate{Account: f.account, Connection: connection}); err != nil {
		t.Fatal(err)
	}
	for _, code := range codes {
		if err := f.store.SetSyncStatus(context.Background(), archive.SyncUpdate{Account: f.account, GapCode: code, CoverageUnknown: true}); err != nil {
			t.Fatal(err)
		}
	}
}

func actualGapNoPrivateMetadata(t *testing.T, f *actualFixture, payload []byte) {
	t.Helper()
	for _, canary := range []string{f.account, "source-sender-canary", "source-chat-a@g.us", "source-chat-b@g.us", "protocol-canary", "forbidden-body-canary", "old-client-filter-canary", f.dir, f.token} {
		if strings.Contains(string(payload), canary) {
			t.Fatal("readable snapshot escaped its chat/date grant or exposed private authority")
		}
	}
}

func assertArchiveSnapshotDisclosure(t *testing.T, payload []byte) {
	t.Helper()
	var label struct {
		Untrusted         bool   `json:"untrusted"`
		Instruction       string `json:"instruction"`
		ArchiveState      string `json:"archive_state"`
		CurrentPhoneState string `json:"current_phone_state"`
		CoverageTool      string `json:"coverage_tool"`
		CoverageWarning   string `json:"coverage_warning"`
	}
	if err := json.Unmarshal(payload, &label); err != nil {
		t.Fatal(err)
	}
	if !label.Untrusted || !strings.Contains(label.Instruction, "untrusted") || label.ArchiveState != "stored_snapshot" || label.CurrentPhoneState != "unverified" || label.CoverageTool != "get_index_status" || !strings.Contains(label.CoverageWarning, "incomplete") || !strings.Contains(label.CoverageWarning, "current WhatsApp state") {
		t.Fatal("MCP reply omitted its stored-snapshot/current-state limitation or coverage guidance")
	}
}

func TestMCPArchiveSnapshotLabelRequiresCoverageDisclosure(t *testing.T) {
	wire, err := json.Marshal(sourceLabel())
	if err != nil {
		t.Fatal(err)
	}
	assertArchiveSnapshotDisclosure(t, wire)
}

func TestActualRemoteAppStateGapReadsScopedStoredSnapshot(t *testing.T) {
	for _, connection := range []archive.ConnectionState{"connected", "disconnected"} {
		t.Run(string(connection), func(t *testing.T) {
			f := openActualFixture(t)
			const chatName = "synthetic-state-gap-readable-chat"
			if err := f.store.SetSourceNames(context.Background(), archive.SourceNames{Account: f.account, Chat: f.targetKey.Chat, ChatName: chatName}); err != nil {
				t.Fatal(err)
			}
			actualGapRecord(t, f, connection, "app_state_sync_error", "undecryptable_message")
			session, ctx := actualGapRemoteSession(t, f, f.store)
			statusWire := callActual(t, session, ctx, "get_index_status", map[string]any{})
			var status statusDTO
			if err := json.Unmarshal(statusWire, &status); err != nil {
				t.Fatal(err)
			}
			if status.Connection != connection || status.Paused || status.ReconciliationRequired || status.MessageCount != 3 || status.Keyword.Complete != 3 || status.Coverage != "partial" || len(status.Chats) != 1 || status.Chats[0].Name != chatName {
				t.Fatalf("diagnostic-only gap lost scoped readable status: %#v", status)
			}
			for _, code := range []string{"app_state_sync_error", "undecryptable_message"} {
				if !strings.Contains(string(statusWire), `"`+code+`"`) {
					t.Fatalf("readable snapshot concealed its coverage gap: %s", code)
				}
			}
			searchWire := callActual(t, session, ctx, "search_messages", map[string]any{"query": "archive-match", "limit": 20})
			var search searchDTO
			if json.Unmarshal(searchWire, &search) != nil || len(search.Hits) != 3 || !search.Untrusted || !strings.Contains(string(searchWire), "שָׁלוֹם 1,250 https://example.invalid/demo") {
				t.Fatal("diagnostic gap blocked readable Hebrew search or broadened its grant")
			}
			chatsWire := callActual(t, session, ctx, "list_chats", map[string]any{})
			var chats chatPageDTO
			// Account-wide sync gaps are reported by get_index_status, not
			// attributed to individual chats without a chat-specific witness.
			if json.Unmarshal(chatsWire, &chats) != nil || len(chats.Chats) != 1 || chats.Chats[0].ChatID != f.chatA || chats.Chats[0].Name != chatName || chats.Chats[0].MessageCount != 3 || chats.Chats[0].Coverage != "unknown" {
				t.Fatal("diagnostic gap blocked scoped chat metadata or invented chat-specific coverage")
			}
			contextWire := callActual(t, session, ctx, "get_context", map[string]any{"message_id": f.target, "before": 0, "after": 0})
			var messages contextDTO
			if json.Unmarshal(contextWire, &messages) != nil || len(messages.Messages) != 1 || messages.Messages[0].MessageID != f.target || messages.Messages[0].ReplyTo != f.quote || !strings.Contains(messages.Messages[0].ReplyQuote, "שָׁלוֹם") || !messages.Untrusted {
				t.Fatal("diagnostic gap blocked a permitted stored original or authorized reply quote")
			}
			for _, payload := range [][]byte{statusWire, searchWire, chatsWire, contextWire} {
				actualGapNoPrivateMetadata(t, f, payload)
				assertArchiveSnapshotDisclosure(t, payload)
			}
			emptyWire := callActual(t, session, ctx, "search_messages", map[string]any{"query": "absent synthetic keyword"})
			var empty searchDTO
			if json.Unmarshal(emptyWire, &empty) != nil || len(empty.Hits) != 0 {
				t.Fatal("readable snapshot failed an empty scoped search")
			}
			assertArchiveSnapshotDisclosure(t, emptyWire)
			actualGapNoPrivateMetadata(t, f, emptyWire)
			denied, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_context", Arguments: map[string]any{"message_id": f.denied}})
			if err != nil || denied == nil || !denied.IsError {
				t.Fatal("partial-read policy authorized a guessed message in a denied chat")
			}
			payload, _ := json.Marshal(denied)
			actualGapNoPrivateMetadata(t, f, payload)
		})
	}
}

func TestActualRemoteAppStateGapWithholdsDeletedAndConflictedSources(t *testing.T) {
	f := openActualFixture(t)
	actualGapRecord(t, f, "connected", "app_state_sync_error", "undecryptable_message")
	if err := f.store.RecordUnresolvedDeletion(context.Background(), f.account, f.quoteKey.Chat, f.quoteKey.Message); err != nil {
		t.Fatal(err)
	}
	conflict := archive.SourceEvent{Key: f.targetKey, Kind: archive.EventOriginal, OriginalSentAt: f.now.Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1, Body: "conflicting replacement private canary", BodyKind: archive.MessageText, ReplyTo: &f.quoteKey}
	if _, err := f.store.ApplySourceEvent(context.Background(), conflict, f.policy.Generation); !errors.Is(err, archive.ErrSourceRevisionConflict) {
		t.Fatalf("source conflict fixture did not commit its exclusion: %v", err)
	}
	session, ctx := actualGapRemoteSession(t, f, f.store)
	searchWire := callActual(t, session, ctx, "search_messages", map[string]any{"query": "archive-match", "limit": 20})
	var search searchDTO
	if json.Unmarshal(searchWire, &search) != nil || len(search.Hits) != 1 || search.Hits[0].MessageID != f.hiddenReply {
		t.Fatal("mixed gaps exposed deleted/conflicted originals or blocked an unrelated readable message")
	}
	statusWire := callActual(t, session, ctx, "get_index_status", map[string]any{})
	var status statusDTO
	if json.Unmarshal(statusWire, &status) != nil || status.MessageCount != 1 || status.Coverage != "partial" || status.ReconciliationRequired || status.Paused {
		t.Fatal("per-source conflicts became an archive-wide content block")
	}
	for _, code := range []string{"app_state_sync_error", "undecryptable_message", "unresolved_deletion", "source_revision_conflict"} {
		if !strings.Contains(string(statusWire), `"`+code+`"`) {
			t.Fatalf("mixed coverage warning omitted %s", code)
		}
	}
	for _, id := range []archive.MessageID{f.quote, f.target, f.denied} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_context", Arguments: map[string]any{"message_id": id}})
		if err != nil || result == nil || !result.IsError {
			t.Fatal("partial-read policy exposed deleted/conflicted/out-of-scope context")
		}
		wire, _ := json.Marshal(result)
		actualGapNoPrivateMetadata(t, f, wire)
		if strings.Contains(string(wire), "permitted original target") || strings.Contains(string(wire), "שָׁלוֹם") || strings.Contains(string(wire), "conflicting replacement") {
			t.Fatal("excluded source content leaked through a tool error")
		}
	}
	contextWire := callActual(t, session, ctx, "get_context", map[string]any{"message_id": f.hiddenReply, "before": 0, "after": 0})
	var messages contextDTO
	if json.Unmarshal(contextWire, &messages) != nil || len(messages.Messages) != 1 || messages.Messages[0].MessageID != f.hiddenReply || messages.Messages[0].ReplyTo != "" || messages.Messages[0].ReplyQuote != "" {
		t.Fatal("partial-read policy borrowed reply authority from an excluded chat")
	}
	for _, payload := range [][]byte{searchWire, statusWire, contextWire} {
		actualGapNoPrivateMetadata(t, f, payload)
		assertArchiveSnapshotDisclosure(t, payload)
		if strings.Contains(string(payload), string(f.quote)) || strings.Contains(string(payload), string(f.target)) || strings.Contains(string(payload), "conflicting replacement") {
			t.Fatal("withheld source identity or replacement body escaped the remote reply")
		}
	}
}

func TestActualRemoteAppStateGapDoesNotDowngradeUnsafeArchive(t *testing.T) {
	for _, reason := range []archive.UnsafeReason{"app_state_sync_error", "undecryptable_message", "unsafe_mutation_projection", "control_admission_failed", "queue_limit_exceeded"} {
		t.Run(string(reason), func(t *testing.T) {
			f := openActualFixture(t)
			actualGapRecord(t, f, "connected", "app_state_sync_error")
			server := actualGapRemoteServer(t, f, f.store)
			if err := f.store.MarkUnsafe(context.Background(), f.account, reason); err != nil {
				t.Fatal(err)
			}
			for _, request := range []mcp.CallToolParams{
				{Name: "search_messages", Arguments: map[string]any{"query": "archive-match"}},
				{Name: "get_context", Arguments: map[string]any{"message_id": f.target}},
				{Name: "list_chats", Arguments: map[string]any{}},
			} {
				body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": request})
				if err != nil {
					t.Fatal(err)
				}
				status, wire := rpc(t, server, string(body), func(r *http.Request) {
					r.Header.Set("Authorization", "Bearer "+f.token)
				})
				// Unsafe archives deny credential resolution at HTTP admission,
				// before the SDK can return any archive metadata or content.
				if status != http.StatusUnauthorized || wire != `{"error":"mcp_unauthorized"}` {
					t.Fatalf("diagnostic gap downgraded true unsafe authority for %s: status=%d", request.Name, status)
				}
				if strings.Contains(wire, "permitted original target") || strings.Contains(wire, string(f.chatA)) || strings.Contains(wire, "שָׁלוֹם") {
					t.Fatal("unsafe archive exposed a content or metadata response")
				}
			}
		})
	}
}

type actualGapFinalCheckReader struct {
	archive.Reader
	store       *archive.Store
	mu          sync.Mutex
	checks      int
	mutate      func() error
	mutationErr error
}

func (r *actualGapFinalCheckReader) setMutation(mutate func() error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mutate = mutate
}

func (r *actualGapFinalCheckReader) mutationState() (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checks, r.mutationErr
}

func (r *actualGapFinalCheckReader) SearchGuarded(ctx context.Context, scope archive.ReadScope, request archive.SearchRequest) (archive.SearchPage, archive.ResponseGuard, error) {
	return r.store.SearchGuarded(ctx, scope, request)
}
func (r *actualGapFinalCheckReader) ContextGuarded(ctx context.Context, scope archive.ReadScope, request archive.ContextRequest) (archive.ContextPage, archive.ResponseGuard, error) {
	return r.store.ContextGuarded(ctx, scope, request)
}
func (r *actualGapFinalCheckReader) ListChatsGuarded(ctx context.Context, scope archive.ReadScope, request archive.ChatListRequest) (archive.ChatPage, archive.ResponseGuard, error) {
	return r.store.ListChatsGuarded(ctx, scope, request)
}
func (r *actualGapFinalCheckReader) StatusGuarded(ctx context.Context, scope archive.ReadScope) (archive.StatusSnapshot, archive.ResponseGuard, error) {
	return r.store.StatusGuarded(ctx, scope)
}
func (r *actualGapFinalCheckReader) CheckResponse(ctx context.Context, scope archive.ReadScope, check archive.ResponseCheck) (archive.ReadSnapshot, error) {
	r.mu.Lock()
	r.checks++
	checks, mutate := r.checks, r.mutate
	r.mu.Unlock()
	// The second check runs after SDK serialization, before HTTP response flush.
	if checks == 2 {
		err := mutate()
		r.mu.Lock()
		r.mutationErr = err
		r.mu.Unlock()
		if err != nil {
			return archive.ReadSnapshot{}, err
		}
	}
	return r.store.CheckResponse(ctx, scope, check)
}

func TestActualRemoteAppStateGapStillRechecksBeforeResponseBytes(t *testing.T) {
	for _, mutation := range []string{"delete", "conflict", "revoke", "unsafe"} {
		t.Run(mutation, func(t *testing.T) {
			f := openActualFixture(t)
			actualGapRecord(t, f, "connected", "app_state_sync_error")
			reader := &actualGapFinalCheckReader{Reader: f.store, store: f.store}
			server := actualGapRemoteServer(t, f, reader)
			reader.setMutation(func() error {
				switch mutation {
				case "delete":
					return f.store.RecordUnresolvedDeletion(context.Background(), f.account, f.targetKey.Chat, f.targetKey.Message)
				case "conflict":
					e := archive.SourceEvent{Key: f.targetKey, Kind: archive.EventOriginal, OriginalSentAt: f.now.Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1, Body: "late private conflict canary", BodyKind: archive.MessageText, ReplyTo: &f.quoteKey}
					_, err := f.store.ApplySourceEvent(context.Background(), e, f.policy.Generation)
					if errors.Is(err, archive.ErrSourceRevisionConflict) {
						return nil
					}
					if err == nil {
						return errors.New("late source conflict fixture did not invalidate the source")
					}
					return err
				case "revoke":
					return f.store.RevokeCredential(context.Background(), f.remote.ID, f.remote.Revision)
				default:
					return f.store.MarkUnsafe(context.Background(), f.account, "unsafe_mutation_projection")
				}
			})
			request, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": "get_context", "arguments": map[string]any{"message_id": f.target, "before": 0, "after": 0}},
			})
			if err != nil {
				t.Fatal(err)
			}
			status, wire := rpc(t, server, string(request), func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+f.token)
			})
			if status != http.StatusConflict || wire != `{"error":"mcp_response_invalidated"}` {
				t.Fatalf("late mutation did not withhold serialized HTTP bytes: status=%d", status)
			}
			checks, mutationErr := reader.mutationState()
			if checks != 2 || mutationErr != nil {
				t.Fatalf("late mutation fixture did not reach the final HTTP response check: checks=%d err=%v", checks, mutationErr)
			}
			if strings.Contains(wire, "permitted original target") || strings.Contains(wire, "late private conflict") || strings.Contains(wire, string(f.target)) {
				t.Fatal("invalidated original escaped serialized response withholding")
			}
		})
	}
}
