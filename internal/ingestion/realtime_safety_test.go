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
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// These preservation tests use only delivery, real archive writes and guarded
// reads. They do not require an independent body to overtake any history: a
// conservative ordering fallback is safe. The baseline single worker passes.
type realtimeSafetyGate struct {
	entered, release chan struct{}
	enterOnce, once  sync.Once
}

func (g *realtimeSafetyGate) unblock() { g.once.Do(func() { close(g.release) }) }

type realtimeSafetyStore struct {
	*archive.Store // Keep delivered-history and source-control interfaces intact.
	gates          map[string]*realtimeSafetyGate
	applied        chan string
	directory      string
}

func (s *realtimeSafetyStore) ApplySourceEvent(ctx context.Context, e archive.SourceEvent, generation archive.Generation) (archive.ApplyResult, error) {
	if gate := s.gates[e.Key.Message]; gate != nil && e.Kind == archive.EventOriginal {
		gate.enterOnce.Do(func() { close(gate.entered) })
		select {
		case <-gate.release:
		case <-ctx.Done():
			return archive.ApplyResult{}, ctx.Err()
		}
	}
	result, err := s.Store.ApplySourceEvent(ctx, e, generation)
	if err == nil {
		select {
		case s.applied <- e.Key.Message:
		default:
		}
	}
	return result, err
}

func realtimeSafetyFixture(t *testing.T, delivered bool, heldIDs ...string) (*archive.Store, *Runtime, *realtimeSafetyStore) {
	t.Helper()
	ctx := t.Context()
	store, sourceDir := durableFixture(t)
	directory := sourceDir
	var rebuild *RebuildConfig
	if delivered {
		// The source and destination are small, isolated synthetic archives. No
		// saved session or provider transport participates in this bootstrap.
		if err := store.MarkUnsafe(ctx, durableAccount, archive.UnsafeReason("mutation_metadata_missing")); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(sourceDir, "archive.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(root, "archive")
		directory = destination
		receipt, err := archive.BootstrapRebuild(ctx, sourceDir, destination, archive.RebuildOptions{ExpectedAccount: durableAccount, ExpectedSourceSHA256: hex.EncodeToString(digest[:]), DeviceID: durableAccount, Boundary: time.Unix(1500, 0)})
		if err != nil {
			t.Fatal(err)
		}
		store = durableStore(t, destination)
		selection, err := store.SelectRebuildHistoryPolicy(ctx, archive.RebuildHistorySelectionOptions{Policy: archive.RebuildHistoryDelivered, CommandID: "synthetic-realtime-safety", AcceptDeliveredSnapshot: true})
		if err != nil || selection == nil {
			t.Fatal("synthetic delivered-history selection failed")
		}
		rebuild = &RebuildConfig{ObservationID: receipt.Generation, Account: receipt.Account, DeviceID: receipt.DeviceID, Boundary: receipt.Boundary, Requests: 0}
	}
	barrier := &realtimeSafetyStore{Store: store, gates: make(map[string]*realtimeSafetyGate), applied: make(chan string, 32), directory: directory}
	for _, id := range heldIDs {
		barrier.gates[id] = &realtimeSafetyGate{entered: make(chan struct{}), release: make(chan struct{})}
	}
	r := durableRuntime(t, barrier)
	if rebuild != nil {
		r.config.Rebuild = rebuild
		r.config.RebuildHistoryPolicy = archive.RebuildHistoryDelivered
		if err := configureRebuild(ctx, &r.config, barrier); err != nil {
			t.Fatal(err)
		}
	}
	// Registered AFTER runtime cleanup: failed assertions release every gate
	// before Close joins accepted source work.
	t.Cleanup(func() {
		for _, gate := range barrier.gates {
			gate.unblock()
		}
	})
	return store, r, barrier
}

func realtimeSafetyAwait(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic source did not reach its barrier")
	}
}

func realtimeSafetyApplied(t *testing.T, barrier *realtimeSafetyStore, id string) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case got := <-barrier.applied:
			if got == id {
				return
			}
		case <-timer.C:
			t.Fatal("synthetic source did not finish real writer admission")
		}
	}
}

func realtimeSafetyLive(chat, id, body string) *events.Message {
	e := liveMessage(id, body)
	e.Info.Timestamp = time.Unix(2000, 0) // Fresh under the rebuild boundary.
	jid, err := types.ParseJID(chat)
	if err != nil {
		panic("invalid synthetic chat")
	}
	e.Info.Chat = jid
	e.Info.IsGroup = jid.Server == types.GroupServer
	if !e.Info.IsGroup {
		e.Info.Sender = jid
	}
	return e
}

func realtimeSafetyHistory(t *testing.T, r *Runtime, chat string, records ...*waHistorySync.HistorySyncMsg) *events.Message {
	t.Helper()
	e := deliveredNotification(r)
	e.Message.ProtocolMessage.Type = waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum()
	e.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = compressed(bodylessHistoryWire(t, chat, records...))
	return e
}

func realtimeSafetyMCP(t *testing.T, store *archive.Store, r *Runtime, barrier *realtimeSafetyStore, chat string) (http.Handler, string, archive.SearchHit) {
	t.Helper()
	r.deliver(realtimeSafetyLive(chat, "safety-scope-seed", "safetyscopeseedreceipt"))
	realtimeSafetyApplied(t, barrier, "safety-scope-seed")
	page, err := durableSearch(t, store, "safetyscopeseedreceipt")
	if err != nil || len(page.Hits) != 1 {
		t.Fatal("synthetic scoped seed is not readable")
	}
	seed := page.Hits[0]
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{11}, 32))
	_, err = store.CreateCredential(t.Context(), archive.CredentialCreate{Label: "Synthetic realtime safety", Principal: "client:realtime-safety", Digest: sha256.Sum256([]byte(token)), Policy: archive.CredentialPolicy{Account: durableAccount, Chats: []archive.ChatID{seed.ChatID}}, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := mcpserver.NewHandler(mcpserver.Config{Mode: mcpserver.Bearer, Hosts: []string{"synthetic.invalid"}, RequestTimeout: 5 * time.Second}, store, store)
	if err != nil {
		t.Fatal(err)
	}
	return handler, token, seed
}

func realtimeSafetySearch(t *testing.T, h http.Handler, token, query string, want int) {
	t.Helper()
	var page struct {
		Hits []struct{} `json:"hits"`
	}
	deliveredLiveOutput(t, deliveredLiveRPC(t.Context(), h, token, "search_messages", map[string]any{"query": query, "limit": 20}), &page)
	if len(page.Hits) != want {
		t.Fatalf("guarded synthetic search count=%d want=%d", len(page.Hits), want)
	}
}

func realtimeSafetyNoContent(t *testing.T, h http.Handler, token, query string) {
	t.Helper()
	result := deliveredLiveRPC(t.Context(), h, token, "search_messages", map[string]any{"query": query, "limit": 20})
	if result.err != nil || strings.Contains(result.body, query) {
		t.Fatal("guarded synthetic search exposed withheld content")
	}
	// A grant whose last chat is excluded by current policy is denied at
	// authentication, rather than admitted to a successful empty search.
	if result.status == http.StatusUnauthorized || result.status == http.StatusConflict {
		return
	}
	var page struct {
		Hits []struct{} `json:"hits"`
	}
	deliveredLiveOutput(t, result, &page)
	if len(page.Hits) != 0 {
		t.Fatalf("guarded synthetic search exposed %d withheld sources", len(page.Hits))
	}
}

func realtimeSafetyNoContentWhileHeld(t *testing.T, h http.Handler, token, query string, gate *realtimeSafetyGate) {
	t.Helper()
	// Admission return does not prove consumption. Repeated actual MCP reads
	// observe the privacy boundary for a bounded idle window with the gate held;
	// Disconnect below supplies the authoritative final source join.
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-gate.release:
			t.Fatal("synthetic absence observation lost its source barrier")
		default:
		}
		realtimeSafetyNoContent(t, h, token, query)
		select {
		case <-timer.C:
			return
		case <-tick.C:
		}
	}
}

func realtimeSafetyPlaintextAbsent(t *testing.T, directory string, canaries ...string) {
	t.Helper()
	for i, name := range []string{"archive.sqlite", "archive.sqlite-wal", "archive.sqlite-shm"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if i > 0 && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal("synthetic archive plaintext audit could not read a file")
		}
		for _, canary := range canaries {
			if bytes.Contains(data, []byte(canary)) {
				t.Fatal("excluded pending plaintext reached synthetic archive storage")
			}
		}
	}
}

func realtimeSafetyContextDenied(t *testing.T, h http.Handler, token string, id archive.MessageID, canary string) {
	t.Helper()
	result := deliveredLiveRPC(t.Context(), h, token, "get_context", map[string]any{"message_id": id, "before": 0, "after": 0})
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if result.err != nil || result.status != http.StatusOK || json.Unmarshal([]byte(result.body), &envelope) != nil || !envelope.Result.IsError || strings.Contains(result.body, canary) {
		t.Fatal("guarded context exposed an unavailable synthetic source")
	}
}

func realtimeSafetyDrain(t *testing.T, r *Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := r.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	status := r.Status()
	if status.ReconciliationRequired || status.Dropped != 0 || !r.Quiesced() {
		t.Fatal("synthetic source did not drain cleanly")
	}
}

func TestRealtimeSafetyHistoryTailControlPreventsFreshPublication(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		for _, admin := range []bool{false, true} {
			name := map[bool]string{false: "standard", true: "delivered"}[delivered] + "/" + map[bool]string{false: "terminal_revoke", true: "unresolved_admin_revoke"}[admin]
			t.Run(name, func(t *testing.T) {
				const target = "safety-tail-target"
				const canary = "safetytailtargetreceipt"
				chat := "200@s.whatsapp.net"
				if admin {
					chat = "300@g.us"
				}
				store, r, barrier := realtimeSafetyFixture(t, delivered, "safety-history-held")
				if err := r.Connect(t.Context()); err != nil {
					t.Fatal(err)
				}
				handler, token, _ := realtimeSafetyMCP(t, store, r, barrier, chat)
				control := bodylessHistoryStub(chat, target, waWeb.WebMessageInfo_REVOKE)
				if admin {
					control.Message.MessageStubType = waWeb.WebMessageInfo_ADMIN_REVOKE.Enum()
					control.Message.Participant = proto.String("999@s.whatsapp.net") // Actor, not original author.
					control.Message.Key.Participant = nil
				}
				r.deliver(realtimeSafetyHistory(t, r, chat, bodylessHistoryRecord(chat, "safety-history-held", "safetyhistoryheldreceipt"), control))
				gate := barrier.gates["safety-history-held"]
				realtimeSafetyAwait(t, gate.entered)
				r.deliver(realtimeSafetyLive(chat, target, canary))
				realtimeSafetyNoContentWhileHeld(t, handler, token, canary, gate)
				if r.Status().HistoryChunks != 0 {
					t.Fatal("held history completed before its terminal suffix")
				}
				gate.unblock()
				realtimeSafetyDrain(t, r)
				realtimeSafetySearch(t, handler, token, canary, 0)
				realtimeSafetySearch(t, handler, token, "safetyhistoryheldreceipt", 1)
				key := bodylessHistoryKey(chat, target)
				metadata, err := store.SourceMetadata(t.Context(), key)
				if err != nil || !metadata.Version.Deleted {
					t.Fatal("terminal historical control was lost during the source join")
				}
				if metadata.Version.MessageID != "" {
					realtimeSafetyContextDenied(t, handler, token, metadata.Version.MessageID, canary)
				}
			})
		}
	}
}

func TestRealtimeSafetySameMessageIDAndAliasKeepHistoricalOrder(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		for _, alias := range []bool{false, true} {
			name := map[bool]string{false: "standard", true: "delivered"}[delivered] + "/" + map[bool]string{false: "same_key", true: "mapped_lid"}[alias]
			t.Run(name, func(t *testing.T) {
				store, r, barrier := realtimeSafetyFixture(t, delivered, "safety-history-held")
				if err := r.Connect(t.Context()); err != nil {
					t.Fatal(err)
				}
				handler, token, _ := realtimeSafetyMCP(t, store, r, barrier, "200@s.whatsapp.net")
				chat := "200@s.whatsapp.net"
				if alias {
					chat = "900@lid"
				}
				target := bodylessHistoryRecord(chat, "safety-overlap", "safetyoverlapreceipt")
				target.Message.MessageTimestamp = proto.Uint64(2000)
				chunk := &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum(), Conversations: []*waHistorySync.Conversation{{ID: proto.String(chat), Messages: []*waHistorySync.HistorySyncMsg{bodylessHistoryRecord(chat, "safety-history-held", "safetyhistoryheldreceipt"), target}}}}
				if alias {
					chunk.PhoneNumberToLidMappings = []*waHistorySync.PhoneNumberToLIDMapping{{PnJID: proto.String("200@s.whatsapp.net"), LidJID: proto.String(chat)}}
				}
				raw, err := proto.Marshal(chunk)
				if err != nil {
					t.Fatal(err)
				}
				notification := realtimeSafetyHistory(t, r, chat)
				notification.Message.ProtocolMessage.HistorySyncNotification.InitialHistBootstrapInlinePayload = compressed(raw)
				r.deliver(notification)
				gate := barrier.gates["safety-history-held"]
				realtimeSafetyAwait(t, gate.entered)
				// Identical content avoids fabricating a revision conflict. It still
				// must not be published ahead of the earlier record of this ID.
				r.deliver(realtimeSafetyLive("200@s.whatsapp.net", "safety-overlap", "safetyoverlapreceipt"))
				realtimeSafetyNoContentWhileHeld(t, handler, token, "safetyoverlapreceipt", gate)
				gate.unblock()
				realtimeSafetyDrain(t, r)
				realtimeSafetySearch(t, handler, token, "safetyoverlapreceipt", 1)
				pn, err := store.SourceMetadata(t.Context(), bodylessHistoryKey("200@s.whatsapp.net", "safety-overlap"))
				if err != nil || pn.Version.MessageID == "" {
					t.Fatal("ordered duplicate lost its real source identity")
				}
				if alias {
					lid, err := store.SourceMetadata(t.Context(), bodylessHistoryKey("900@lid", "safety-overlap"))
					if err != nil || lid.Version.MessageID != pn.Version.MessageID {
						t.Fatal("transport aliases split an ordered source identity")
					}
				}
			})
		}
	}
}

func TestRealtimeSafetyLiveFIFOWithAvailableAdmissionRoom(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		t.Run(map[bool]string{false: "standard", true: "delivered"}[delivered], func(t *testing.T) {
			store, r, barrier := realtimeSafetyFixture(t, delivered, "safety-live-held")
			if err := r.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			handler, token, _ := realtimeSafetyMCP(t, store, r, barrier, "200@s.whatsapp.net")
			r.deliver(realtimeSafetyLive("200@s.whatsapp.net", "safety-live-held", "safetyfirstlivereceipt"))
			gate := barrier.gates["safety-live-held"]
			realtimeSafetyAwait(t, gate.entered)
			// Unlike the saturated-ledger case, both ordinary live callbacks are
			// admitted. A goroutine per callback must not reorder their commits.
			r.deliver(realtimeSafetyLive("200@s.whatsapp.net", "safety-live-second", "safetysecondlivereceipt"))
			realtimeSafetyNoContentWhileHeld(t, handler, token, "safetysecondlivereceipt", gate)
			gate.unblock()
			realtimeSafetyDrain(t, r)
			realtimeSafetySearch(t, handler, token, "safetyfirstlivereceipt", 1)
			realtimeSafetySearch(t, handler, token, "safetysecondlivereceipt", 1)
		})
	}
}

func TestRealtimeSafetyCombinedLaneBudgetsKeepControls(t *testing.T) {
	for _, byBytes := range []bool{false, true} {
		t.Run(map[bool]string{false: "count_two", true: "combined_bytes"}[byBytes], func(t *testing.T) {
			store, r, barrier := realtimeSafetyFixture(t, false, "safety-live-held", "safety-history-held")
			first := realtimeSafetyLive("200@s.whatsapp.net", "safety-live-held", "safetyfirstlivereceipt "+strings.Repeat("a", 400))
			history := realtimeSafetyHistory(t, r, "200@s.whatsapp.net", historyRecord("safety-history-held", "safetybudgethistoryreceipt"))
			second := realtimeSafetyLive("200@s.whatsapp.net", "safety-live-second", "safetysecondlivereceipt "+strings.Repeat("b", 400))
			r.config.BodyQueueCount = 2
			if byBytes {
				r.config.BodyQueueCount = 8
				r.config.BodyQueueBytes = 128 + retainedBytes(first) + retainedBytes(history)
			}
			if err := r.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			handler, token, seed := realtimeSafetyMCP(t, store, r, barrier, "200@s.whatsapp.net")
			r.deliver(first)
			gate := barrier.gates["safety-live-held"]
			realtimeSafetyAwait(t, gate.entered)
			// The second gate keeps this envelope charged even if a safe future
			// scheduler can run independent history ahead of the held live body.
			// The baseline worker leaves it queued instead; both count equally.
			r.deliver(history)
			started, done := make(chan struct{}), make(chan struct{})
			go func() {
				close(started)
				r.deliver(second)
				close(done)
			}()
			realtimeSafetyAwait(t, started)
			realtimeSafetyNoContentWhileHeld(t, handler, token, "safetysecondlivereceipt", gate)
			select {
			case <-done:
				t.Fatal("live and history borrowed separate count or byte budgets")
			default:
			}
			realtimeSafetySearch(t, handler, token, "safetybudgethistoryreceipt", 0)
			r.deliver(&events.DeleteForMe{ChatJID: types.NewJID("200", types.DefaultUserServer), SenderJID: types.NewJID("200", types.DefaultUserServer), MessageID: "safety-scope-seed", Timestamp: time.Unix(2100, 0)})
			realtimeSafetySearch(t, handler, token, "safetyscopeseedreceipt", 0)
			realtimeSafetyContextDenied(t, handler, token, seed.MessageID, "safetyscopeseedreceipt")
			if status := r.Status(); status.Controls != 1 || status.ReconciliationRequired || status.Dropped != 0 {
				t.Fatal("combined body pressure delayed a received source control")
			}
			gate.unblock()
			barrier.gates["safety-history-held"].unblock()
			realtimeSafetyAwait(t, done)
			realtimeSafetyDrain(t, r)
			for _, query := range []string{"safetyfirstlivereceipt", "safetybudgethistoryreceipt", "safetysecondlivereceipt"} {
				realtimeSafetySearch(t, handler, token, query, 1)
			}
			realtimeSafetySearch(t, handler, token, "safetyscopeseedreceipt", 0)
		})
	}
}

func TestRealtimeSafetyNarrowedPolicyWithholdsPendingLiveAndBufferedMCP(t *testing.T) {
	for _, selective := range []bool{false, true} {
		t.Run(map[bool]string{false: "date", true: "chat"}[selective], func(t *testing.T) {
			store, r, barrier := realtimeSafetyFixture(t, false, "safety-live-held")
			if err := r.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			handler, token, seed := realtimeSafetyMCP(t, store, r, barrier, "200@s.whatsapp.net")
			response := &deliveredLiveResponseBarrier{Store: store, started: make(chan archive.ResponseCheck, 1), release: make(chan struct{})}
			var once sync.Once
			unblockResponse := func() { once.Do(func() { close(response.release) }) }
			t.Cleanup(unblockResponse)
			guardedHandler, err := mcpserver.NewHandler(mcpserver.Config{Mode: mcpserver.Bearer, Hosts: []string{"synthetic.invalid"}, RequestTimeout: 5 * time.Second}, response, store)
			if err != nil {
				t.Fatal(err)
			}
			pending := make(chan deliveredLiveRPCResult, 1)
			go func() {
				pending <- deliveredLiveRPC(t.Context(), guardedHandler, token, "search_messages", map[string]any{"query": "safetyscopeseedreceipt"})
			}()
			select {
			case check := <-response.started:
				if check.Credential == nil || len(check.Guard.Sources) != 1 || check.Guard.Sources[0].Ref.Version.MessageID != seed.MessageID {
					t.Fatal("buffered MCP read lost its actual credential or source guard")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("synthetic response did not reach final delivery check")
			}
			r.deliver(realtimeSafetyLive("200@s.whatsapp.net", "safety-live-held", "safetyexcludedheldreceipt"))
			gate := barrier.gates["safety-live-held"]
			realtimeSafetyAwait(t, gate.entered)
			r.deliver(realtimeSafetyLive("200@s.whatsapp.net", "safety-excluded-pending", "safetyexcludedpendingreceipt"))
			current, err := store.Policy(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			narrow := archive.ArchivePolicy{DateMode: archive.DateSince, ImportStart: time.Unix(2500, 0), ChatMode: archive.ChatsAll}
			if selective {
				narrow = archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsSelective}
			}
			if _, err := store.SetPolicy(t.Context(), narrow, current.Generation); err != nil {
				t.Fatal(err)
			}
			unblockResponse()
			select {
			case result := <-pending:
				if result.err != nil || result.status != http.StatusConflict || strings.Contains(result.body, "safetyscopeseedreceipt") {
					t.Fatal("policy change flushed a stale scoped MCP excerpt")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("invalidated synthetic response did not finish")
			}
			realtimeSafetyNoContentWhileHeld(t, handler, token, "safetyexcludedpendingreceipt", gate)
			gate.unblock()
			realtimeSafetyDrain(t, r)
			for _, query := range []string{"safetyexcludedheldreceipt", "safetyexcludedpendingreceipt", "safetyscopeseedreceipt"} {
				realtimeSafetyNoContent(t, handler, token, query)
			}
			contextResult := deliveredLiveRPC(t.Context(), handler, token, "get_context", map[string]any{"message_id": seed.MessageID, "before": 0, "after": 0})
			if contextResult.status != http.StatusUnauthorized {
				realtimeSafetyContextDenied(t, handler, token, seed.MessageID, "safetyscopeseedreceipt")
			} else if contextResult.err != nil || strings.Contains(contextResult.body, "safetyscopeseedreceipt") {
				t.Fatal("excluded chat remained readable through context")
			}
			realtimeSafetyPlaintextAbsent(t, barrier.directory, "safetyexcludedheldreceipt", "safetyexcludedpendingreceipt")
			for _, id := range []string{"safety-live-held", "safety-excluded-pending"} {
				metadata, err := store.SourceMetadata(t.Context(), bodylessHistoryKey("200@s.whatsapp.net", id))
				if err != nil || !metadata.OriginalDateKnown || !metadata.OriginalSentAt.Equal(time.Unix(2000, 0)) {
					t.Fatal("policy exclusion lost the bodyless source anchor")
				}
			}
		})
	}
}

func TestRealtimeSafetyMalformedHistorySuffixRemainsStrict(t *testing.T) {
	store, r, barrier := realtimeSafetyFixture(t, false, "safety-history-held")
	if err := r.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	handler, token, _ := realtimeSafetyMCP(t, store, r, barrier, "200@s.whatsapp.net")
	bad := historyRecord("safety-malformed-control", "")
	bad.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum()}}
	r.deliver(realtimeSafetyHistory(t, r, "200@s.whatsapp.net", historyRecord("safety-history-held", "safetyhistoryheldreceipt"), bad))
	gate := barrier.gates["safety-history-held"]
	// Preparation may reject before applying the prefix. Wait for either that
	// strict refusal or the legacy apply gate, never for an unreachable gate.
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !r.Status().ReconciliationRequired {
		select {
		case <-gate.entered:
			goto observed
		case <-tick.C:
		case <-timer.C:
			t.Fatal("malformed source reached neither preparation refusal nor apply gate")
		}
	}
observed:
	r.deliver(realtimeSafetyLive("200@s.whatsapp.net", "safety-after-malformed", "safetymalformedlivereceipt"))
	realtimeSafetyNoContentWhileHeld(t, handler, token, "safetymalformedlivereceipt", gate)
	// Complete preparation is allowed to fence before the positive reaches the
	// gate; legacy ordered projection reaches it first. Neither may publish.
	gate.unblock()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := r.Disconnect(ctx); !errors.Is(err, archive.ErrReconciliationRequired) || !r.Status().ReconciliationRequired || !r.Quiesced() {
		t.Fatal("malformed source suffix lost strict lifecycle protection")
	}
	page, err := durableSearch(t, store, "safetymalformedlivereceipt")
	if !errors.Is(err, archive.ErrReconciliationRequired) || len(page.Hits) != 0 {
		t.Fatal("strict source failure left fresh archive content authorized")
	}
	result := deliveredLiveRPC(t.Context(), handler, token, "search_messages", map[string]any{"query": "safetymalformedlivereceipt"})
	if result.err != nil || strings.Contains(result.body, "safetymalformedlivereceipt") || result.status == http.StatusOK {
		t.Fatal("strict source failure left the scoped MCP credential readable")
	}
}
