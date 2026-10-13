//go:build sqlite_fts5

package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Preserve every real Store interface, including rebuild admission. Only hold
// one projected positive before writer admission; never hold a SQLite lock or
// fabricate an apply result. Earlier records have already committed normally.
type deliveredLiveApplyBarrier struct {
	*archive.Store
	started chan archive.SourceEvent
	release chan struct{}
	once    sync.Once
}

func (s *deliveredLiveApplyBarrier) ApplySourceEvent(ctx context.Context, e archive.SourceEvent, g archive.Generation) (archive.ApplyResult, error) {
	if e.Key.Message == "seam-held" && e.Kind == archive.EventOriginal && e.DeliveredHistory != nil {
		s.once.Do(func() { s.started <- e })
		select {
		case <-s.release:
		case <-ctx.Done():
			return archive.ApplyResult{}, ctx.Err()
		}
	}
	return s.Store.ApplySourceEvent(ctx, e, g)
}

// A separate reader pauses the second (buffered HTTP delivery) check. The first
// tool-level check and the final check both still use real archive guards.
type deliveredLiveResponseBarrier struct {
	*archive.Store
	mu      sync.Mutex
	checks  int
	started chan archive.ResponseCheck
	release chan struct{}
}

func (s *deliveredLiveResponseBarrier) CheckResponse(ctx context.Context, scope archive.ReadScope, check archive.ResponseCheck) (archive.ReadSnapshot, error) {
	s.mu.Lock()
	s.checks++
	final := s.checks == 2
	s.mu.Unlock()
	if final {
		s.started <- check
		select {
		case <-s.release:
		case <-ctx.Done():
			return archive.ReadSnapshot{}, ctx.Err()
		}
	}
	return s.Store.CheckResponse(ctx, scope, check)
}

type deliveredLiveRPCResult struct {
	status int
	body   string
	err    error
}

// Exercise the shipped MCP HTTP handler/SDK without a listener or any network.
func deliveredLiveRPC(ctx context.Context, h http.Handler, token, tool string, args map[string]any) deliveredLiveRPCResult {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		return deliveredLiveRPCResult{err: err}
	}
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://synthetic.invalid/mcp", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return deliveredLiveRPCResult{status: w.Code, body: w.Body.String()}
}

func deliveredLiveOutput(t *testing.T, result deliveredLiveRPCResult, out any) {
	t.Helper()
	var envelope struct {
		Result struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	if result.err != nil || result.status != http.StatusOK || json.Unmarshal([]byte(result.body), &envelope) != nil || envelope.Result.IsError || len(envelope.Result.StructuredContent) == 0 {
		t.Fatalf("synthetic MCP call failed: status=%d err=%v", result.status, result.err)
	}
	if err := json.Unmarshal(envelope.Result.StructuredContent, out); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveredHistoryLiveControlAndScopedMCPDuringApply(t *testing.T) {
	// This is a deadlock escape, not an import-latency assertion. Ordering below
	// comes exclusively from barriers, delivery return, and the owned drain.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	source, sourceDir := durableFixture(t)
	policy, err := source.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	knownKey := archive.SourceKey{Account: durableAccount, Chat: "200@s.whatsapp.net", Sender: "200@s.whatsapp.net", Message: "seam-known-deleted"}
	if _, err = source.ApplySourceEvent(ctx, archive.SourceEvent{Key: knownKey, Kind: archive.EventDelete, SourceRevision: 2000000, BodyKind: archive.MessageUnsupported}, policy.Generation); err != nil {
		t.Fatal(err)
	}
	if err = source.MarkUnsafe(ctx, durableAccount, archive.UnsafeReason("mutation_metadata_missing")); err != nil {
		t.Fatal(err)
	}
	if err = source.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(sourceDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(before)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := archive.BootstrapRebuild(ctx, sourceDir, filepath.Join(root, "archive"), archive.RebuildOptions{ExpectedAccount: durableAccount, ExpectedSourceSHA256: hex.EncodeToString(digest[:]), DeviceID: durableAccount, Boundary: time.Unix(1500, 0)})
	if err != nil {
		t.Fatal(err)
	}
	store := durableStore(t, filepath.Join(root, "archive"))
	selection, err := store.SelectRebuildHistoryPolicy(ctx, archive.RebuildHistorySelectionOptions{Policy: archive.RebuildHistoryDelivered, CommandID: "synthetic-live-control-seam", AcceptDeliveredSnapshot: true})
	if err != nil || selection == nil {
		t.Fatalf("select delivered history: %v", err)
	}
	apply := &deliveredLiveApplyBarrier{Store: store, started: make(chan archive.SourceEvent, 1), release: make(chan struct{})}
	r := durableRuntime(t, apply)
	r.config.Rebuild = &RebuildConfig{ObservationID: receipt.Generation, Account: receipt.Account, DeviceID: receipt.DeviceID, Boundary: receipt.Boundary, Requests: 0}
	r.config.RebuildHistoryPolicy = selection.Policy
	if err = configureRebuild(ctx, &r.config, apply); err != nil {
		t.Fatal(err)
	}
	var releaseApply sync.Once
	unblockApply := func() { releaseApply.Do(func() { close(apply.release) }) }
	t.Cleanup(unblockApply) // release before durableRuntime's Close, even on failure
	if err = r.Connect(ctx); err != nil {
		t.Fatal(err)
	}

	const query = "syncseamreceipt"
	const targetText = query + " targetreceipt שלום 👋"
	outsideChat := historyRecord("seam-outside-chat", query+" outsidechatcanary")
	outsideChat.Message.Key.RemoteJID = proto.String("300@s.whatsapp.net")
	outsideDate := historyRecord("seam-outside-date", query+" outsidedatecanary")
	outsideDate.Message.MessageTimestamp = proto.Uint64(800)
	held := historyRecord("seam-held", query+" heldreceipt")
	held.Message.MessageTimestamp = proto.Uint64(990)
	tail := historyRecord("seam-tail", query+" tailreceipt")
	tail.Message.MessageTimestamp = proto.Uint64(980)
	raw, err := proto.Marshal(&waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum(), Conversations: []*waHistorySync.Conversation{
		{ID: proto.String("300@s.whatsapp.net"), Messages: []*waHistorySync.HistorySyncMsg{outsideChat}},
		{ID: proto.String("200@s.whatsapp.net"), Messages: []*waHistorySync.HistorySyncMsg{outsideDate, historyRecord("seam-target", targetText), held, historyRecord("seam-target", targetText), historyRecord(knownKey.Message, query+" knownnegativecanary"), tail}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	notification := deliveredNotification(r)
	notification.Message.ProtocolMessage.Type = waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum()
	notification.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = compressed(raw)
	r.deliver(notification) // actual queue worker, authentication and delivered admission
	select {
	case e := <-apply.started:
		if e.DeliveredHistory.StrategyEpoch != selection.StrategyEpoch || !e.OriginalSentAt.Before(receipt.Boundary) || e.Body != query+" heldreceipt" {
			t.Fatal("barrier did not intercept a projected older delivered positive")
		}
	case <-ctx.Done():
		t.Fatal("history did not reach the apply barrier")
	}

	owner, err := durableSearch(t, store, query)
	if err != nil || len(owner.Hits) != 3 {
		t.Fatalf("history prefix not committed: hits=%d err=%v", len(owner.Hits), err)
	}
	var target archive.SearchHit
	var excluded []archive.MessageID
	for _, hit := range owner.Hits {
		if hit.Snippet == targetText {
			target = hit
		} else {
			excluded = append(excluded, hit.MessageID)
		}
	}
	if target.MessageID == "" || len(excluded) != 2 {
		t.Fatal("fixture lacks permitted and excluded real opaque identities")
	}
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	credential, err := store.CreateCredential(ctx, archive.CredentialCreate{Label: "Synthetic history seam", Principal: "client:history-seam", Digest: sha256.Sum256([]byte(token)), Policy: archive.CredentialPolicy{Account: durableAccount, Chats: []archive.ChatID{target.ChatID}, From: time.Unix(950, 0)}, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	config := mcpserver.Config{Mode: mcpserver.Bearer, Hosts: []string{"synthetic.invalid"}, RequestTimeout: 15 * time.Second}
	handler, err := mcpserver.NewHandler(config, store, store)
	if err != nil {
		t.Fatal(err)
	}
	assertSearch := func(want ...string) {
		t.Helper()
		result := deliveredLiveRPC(ctx, handler, token, "search_messages", map[string]any{"query": query, "limit": 20})
		var page struct {
			Delivered bool `json:"delivered_history_snapshot"`
			Hits      []struct {
				ChatID  archive.ChatID        `json:"chat_id"`
				Excerpt struct{ Text string } `json:"excerpt"`
			} `json:"hits"`
		}
		deliveredLiveOutput(t, result, &page)
		remaining := make(map[string]bool)
		for _, text := range want {
			remaining[text] = true
		}
		if !page.Delivered || len(page.Hits) != len(want) {
			t.Fatalf("scoped delivered search: hits=%d want=%d snapshot=%t", len(page.Hits), len(want), page.Delivered)
		}
		for _, hit := range page.Hits {
			if hit.ChatID != target.ChatID || !remaining[hit.Excerpt.Text] {
				t.Fatal("MCP escaped its chat/date scope or returned stale content")
			}
			delete(remaining, hit.Excerpt.Text)
		}
		for _, canary := range []string{"outsidechatcanary", "outsidedatecanary", "knownnegativecanary", durableAccount, "200@s.whatsapp.net", "300@s.whatsapp.net", token} {
			if strings.Contains(result.body, canary) {
				t.Fatal("MCP leaked excluded content or private authority")
			}
		}
	}
	assertStatus := func(want int) {
		t.Helper()
		var status struct {
			Messages int `json:"message_count"`
			Chats    []struct {
				ID       archive.ChatID `json:"chat_id"`
				Messages int            `json:"message_count"`
			} `json:"chats"`
		}
		deliveredLiveOutput(t, deliveredLiveRPC(ctx, handler, token, "get_index_status", map[string]any{}), &status)
		if status.Messages != want || want > 0 && (len(status.Chats) != 1 || status.Chats[0].ID != target.ChatID || status.Chats[0].Messages != want) || want == 0 && len(status.Chats) != 0 {
			t.Fatalf("MCP counts escaped scope: messages=%d chats=%d want=%d", status.Messages, len(status.Chats), want)
		}
	}
	assertSearch(targetText)
	assertStatus(1)
	for _, id := range excluded {
		result := deliveredLiveRPC(ctx, handler, token, "get_context", map[string]any{"message_id": id, "before": 0, "after": 0})
		var envelope struct {
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if result.err != nil || result.status != http.StatusOK || json.Unmarshal([]byte(result.body), &envelope) != nil || !envelope.Result.IsError || strings.Contains(result.body, "canary") {
			t.Fatal("guessed context ID bypassed the chat/date grant")
		}
	}

	response := &deliveredLiveResponseBarrier{Store: store, started: make(chan archive.ResponseCheck, 1), release: make(chan struct{})}
	var releaseResponse sync.Once
	unblockResponse := func() { releaseResponse.Do(func() { close(response.release) }) }
	t.Cleanup(unblockResponse)
	guardedHandler, err := mcpserver.NewHandler(config, response, store)
	if err != nil {
		t.Fatal(err)
	}
	pending := make(chan deliveredLiveRPCResult, 1)
	go func() {
		pending <- deliveredLiveRPC(ctx, guardedHandler, token, "search_messages", map[string]any{"query": query})
	}()
	var captured archive.ResponseCheck
	select {
	case captured = <-response.started:
		if captured.Credential == nil || captured.Credential.CredentialID != credential.ID || len(captured.Guard.Sources) != 1 || captured.Guard.Sources[0].Ref.Version.MessageID != target.MessageID {
			t.Fatal("buffered response did not retain the admitted source and exact scoped grant")
		}
	case <-ctx.Done():
		t.Fatal("MCP did not reach its final buffered delivery check")
	}

	live := liveMessage("seam-live", query+" livereceipt")
	live.Info.Timestamp = time.Unix(2000, 0)
	r.deliver(live) // admitted while history is held; bodies share one worker
	deletion := &events.DeleteForMe{ChatJID: types.NewJID("200", types.DefaultUserServer), SenderJID: types.NewJID("200", types.DefaultUserServer), MessageID: "seam-target", Timestamp: time.Unix(2100, 0)}
	r.deliver(deletion)
	if status := r.Status(); status.Controls != 1 || status.ReconciliationRequired || status.Dropped != 0 || status.HistoryChunks != 0 {
		t.Fatal("received control did not complete while delivered history remained held")
	}
	if _, err = store.CheckResponse(ctx, captured.Credential.Scope, captured); !errors.Is(err, archive.ErrSourceUnavailable) {
		t.Fatalf("real source guard blessed the deleted buffered excerpt: %v", err)
	}
	unblockResponse()
	select {
	case result := <-pending:
		if result.err != nil || result.status != http.StatusConflict || result.body != `{"error":"mcp_response_invalidated"}` {
			t.Fatalf("MCP flushed stale buffered content: status=%d err=%v", result.status, result.err)
		}
	case <-ctx.Done():
		t.Fatal("invalidated MCP delivery did not finish")
	}
	assertSearch()
	assertStatus(0)

	unblockApply()
	if err = r.Disconnect(ctx); err != nil { // joins history suffix and queued live body
		t.Fatal(err)
	}
	if status := r.Status(); status.HistoryChunks != 1 || status.Controls != 1 || status.ReconciliationRequired || status.Dropped != 0 || !r.Quiesced() {
		t.Fatal("history suffix and queued live body did not drain cleanly")
	}
	assertSearch(query+" heldreceipt", query+" tailreceipt", query+" livereceipt")
	assertStatus(3)
	key := archive.SourceKey{Account: durableAccount, Chat: "200@s.whatsapp.net", Sender: "200@s.whatsapp.net", Message: "seam-target"}
	metadata, err := store.SourceMetadata(ctx, key)
	if err != nil || !metadata.Version.Deleted {
		t.Fatalf("stale history replay lost the live terminal deletion: %v", err)
	}
	guard, err := store.RebuildGuard(ctx, knownKey)
	if err != nil || !guard.Deleted {
		t.Fatalf("delivered positive bypassed an inherited negative: %v", err)
	}
	owner, err = durableSearch(t, store, query)
	if err != nil || len(owner.Hits) != 5 {
		t.Fatalf("real archive lost independent bodies or revived negatives: hits=%d err=%v", len(owner.Hits), err)
	}
	for _, hit := range owner.Hits {
		if hit.Snippet == targetText || strings.Contains(hit.Snippet, "knownnegativecanary") {
			t.Fatal("delivered suffix revived a known deleted body")
		}
	}
	progress, err := store.RebuildDispatchState(ctx)
	if err != nil || progress == nil || progress.CopySpent != 0 || progress.DiscoverySpent != 0 || progress.Accepted != 0 {
		t.Fatal("synthetic delivery performed network work or fabricated exact-copy proof")
	}
	after, err := os.ReadFile(filepath.Join(sourceDir, "archive.sqlite"))
	if err != nil || sha256.Sum256(after) != digest {
		t.Fatal("preserved synthetic source archive changed")
	}
}
