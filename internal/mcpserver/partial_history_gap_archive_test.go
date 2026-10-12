//go:build sqlite_fts5

package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func recordPartialGap(t *testing.T, store *archive.Store, account string) {
	t.Helper()
	if err := store.RecordUnknownHistoryGap(context.Background(), account, 229); err != nil {
		t.Fatalf("synthetic durable gap not recorded: %v", err)
	}
}

func TestPartialHistoryMCPNewOrRepeatedGapDiscardsBufferedRead(t *testing.T) {
	for _, alreadyPartial := range []bool{false, true} {
		for _, name := range []string{"search_messages", "get_context", "list_chats", "get_index_status"} {
			t.Run(map[bool]string{false: "first", true: "repeated"}[alreadyPartial]+"/"+name, func(t *testing.T) {
				fixture := openActualFixture(t)
				if alreadyPartial {
					recordPartialGap(t, fixture.store, fixture.account)
				}
				gapCommits := make(chan error, 1)
				credentials := &interleavedCredentials{Credentials: fixture.store, mutate: func() error {
					err := fixture.store.RecordUnknownHistoryGap(context.Background(), fixture.account, 229)
					gapCommits <- err
					return err
				}}
				server, _ := httpFixture(t, fixture.store, credentials, nil)
				args := map[string]any{}
				switch name {
				case "search_messages":
					args["query"] = "archive-match"
				case "get_context":
					args["message_id"] = fixture.target
				}
				status, body := rpc(t, server, chatToolRPC(t, name, args), nil)
				select {
				case err := <-gapCommits:
					if err != nil {
						t.Fatalf("synthetic gap commit failed before final epoch check: %v", err)
					}
				default:
					t.Fatal("gap commit hook did not run")
				}
				if status != http.StatusConflict || strings.Contains(body, "permitted original target") || strings.Contains(body, "שָׁלוֹם") || strings.Contains(body, `"chats"`) || strings.Contains(body, `"message_count"`) {
					t.Fatalf("expected final coverage invalidation after successful gap commit: %d %s", status, body)
				}
				if credentials.calls != 2 {
					t.Fatal("coverage change did not occur between construction and delivery")
				}
			})
		}
	}
}

func TestPartialHistoryMCPGapDoesNotWeakenChatDateOrDeletionControls(t *testing.T) {
	fixture := openActualFixture(t)
	recordPartialGap(t, fixture.store, fixture.account)
	server, _ := httpFixture(t, fixture.store, fixture.store, nil)
	_, body := rpc(t, server, chatToolRPC(t, "search_messages", map[string]any{"query": "archive-match"}), nil)
	payload := partialGapPayload(t, body)
	assertPartialGapLabel(t, payload)
	var hits []hitDTO
	if json.Unmarshal(payload["hits"], &hits) != nil || len(hits) != 3 || strings.Contains(body, "forbidden-body-canary") || strings.Contains(body, "old-client-filter-canary") {
		t.Fatal("partial-history flag broadened chat or date authority")
	}
	_, body = rpc(t, server, chatToolRPC(t, "get_context", map[string]any{"message_id": fixture.denied}), nil)
	if !strings.Contains(body, `"isError":true`) || strings.Contains(body, "forbidden-body-canary") {
		t.Fatal("partial flag admitted a denied chat")
	}
	_, err := fixture.store.ApplySourceEvent(context.Background(), archive.SourceEvent{Key: fixture.quoteKey, Kind: archive.EventDelete, SourceRevision: 2}, fixture.policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	_, body = rpc(t, server, chatToolRPC(t, "get_context", map[string]any{"message_id": fixture.quote}), nil)
	if !strings.Contains(body, `"isError":true`) || strings.Contains(body, "שָׁלוֹם") {
		t.Fatal("partial flag revived deleted source text")
	}
}

func TestPartialHistoryMCPDeletionStillInvalidatesReplyContributor(t *testing.T) {
	fixture := openActualFixture(t)
	recordPartialGap(t, fixture.store, fixture.account)
	credentials := &interleavedCredentials{Credentials: fixture.store, mutate: func() error {
		_, err := fixture.store.ApplySourceEvent(context.Background(), archive.SourceEvent{Key: fixture.quoteKey, Kind: archive.EventDelete, SourceRevision: 2}, fixture.policy.Generation)
		return err
	}}
	server, _ := httpFixture(t, fixture.store, credentials, nil)
	status, body := rpc(t, server, chatToolRPC(t, "get_context", map[string]any{"message_id": fixture.target}), nil)
	if status != http.StatusConflict || strings.Contains(body, "permitted original target") || strings.Contains(body, "שָׁלוֹם") {
		t.Fatal("partial gap disabled final deletion contributor fence")
	}
}

type partialGapHistoryFixture struct {
	live, snapshot *archive.Store
	factory        *archive.HistoricalView
	account        string
}

func newPartialGapHistoryFixture(t *testing.T) partialGapHistoryFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	account := "synthetic-partial-account"
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
	writer, dir, historyPolicy := open()
	apply := func(store *archive.Store, policy archive.ArchivePolicy, id, body string) {
		_, err := store.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: account, Chat: "synthetic-partial-chat", Sender: "synthetic-sender", Message: id}, Kind: archive.EventOriginal, OriginalSentAt: now.Add(-time.Hour), OriginalDateKnown: true, SourceRevision: 1, Body: body, BodyKind: archive.MessageText}, policy.Generation)
		if err != nil {
			t.Fatal(err)
		}
	}
	apply(live, livePolicy, "live-message", "LIVE_SYNTHETIC")
	apply(writer, historyPolicy, "history-message", "HISTORICAL_SYNTHETIC")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := archive.OpenHistoricalSnapshot(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { snapshot.Close() })
	metadata := archive.HistoricalViewMetadata{Source: "historical", SnapshotID: "0123456789abcdef", CapturedAt: now, ReadOnly: true, Coverage: "partial", CurrentPhoneState: "unknown"}
	factory, err := archive.NewHistoricalView(ctx, live, snapshot, metadata)
	if err != nil {
		t.Fatal(err)
	}
	_, err = live.CreateCredential(ctx, archive.CredentialCreate{Label: "synthetic current partial grant", Principal: "synthetic-client", Digest: sha256.Sum256([]byte(fakeToken())), Policy: archive.CredentialPolicy{Account: account, AllChats: true}})
	if err != nil {
		t.Fatal(err)
	}
	return partialGapHistoryFixture{live: live, snapshot: snapshot, factory: factory, account: account}
}

func TestPartialHistoryMCPSelectedHistoricalReadsInheritCurrentLiveGap(t *testing.T) {
	fixture := newPartialGapHistoryFixture(t)
	recordPartialGap(t, fixture.live, fixture.account)
	server, _ := httpFixture(t, fixture.live, fixture.live, func(c *Config) { c.HistoricalReads = fixture.factory })
	_, body := rpc(t, server, chatToolRPC(t, "search_messages", map[string]any{"source": "historical", "query": "HISTORICAL_SYNTHETIC"}), nil)
	payload := partialGapPayload(t, body)
	assertPartialGapLabel(t, payload)
	if !strings.Contains(body, "HISTORICAL_SYNTHETIC") || strings.Contains(body, "LIVE_SYNTHETIC") {
		t.Fatal("live gap metadata crossed selected source content")
	}
	// The readonly capture contains no new gap; the warning must come from the
	// current live authority, never its frozen metadata or credential table.
	status, err := fixture.snapshot.Status(context.Background(), archive.ReadScope{Account: fixture.account, Principal: "synthetic-snapshot-owner", PolicyGeneration: 1, AllChats: true})
	if err != nil || status.Snapshot.UnknownHistoryGap {
		t.Fatal("synthetic snapshot itself acquired live gap authority")
	}
}

func TestPartialHistoryMCPSelectedHistoricalGapRevisionDiscardsBufferedResult(t *testing.T) {
	for _, alreadyPartial := range []bool{false, true} {
		t.Run(map[bool]string{false: "first", true: "repeated"}[alreadyPartial], func(t *testing.T) {
			fixture := newPartialGapHistoryFixture(t)
			if alreadyPartial {
				recordPartialGap(t, fixture.live, fixture.account)
			}
			gapCommits := make(chan error, 1)
			credentials := &interleavedCredentials{Credentials: fixture.live, mutate: func() error {
				err := fixture.live.RecordUnknownHistoryGap(context.Background(), fixture.account, 229)
				gapCommits <- err
				return err
			}}
			server, _ := httpFixture(t, fixture.live, credentials, func(c *Config) { c.HistoricalReads = fixture.factory })
			status, body := rpc(t, server, chatToolRPC(t, "search_messages", map[string]any{"source": "historical", "query": "HISTORICAL_SYNTHETIC"}), nil)
			select {
			case err := <-gapCommits:
				if err != nil {
					t.Fatalf("synthetic current gap commit failed before historical final epoch check: %v", err)
				}
			default:
				t.Fatal("current gap commit hook did not run")
			}
			if status != http.StatusConflict || strings.Contains(body, "HISTORICAL_SYNTHETIC") || strings.Contains(body, `"archive_view"`) {
				t.Fatalf("expected historical ticket current coverage invalidation after successful gap commit: %d %s", status, body)
			}
			if credentials.calls != 2 {
				t.Fatal("selected historical gap mutation did not follow serialization")
			}
		})
	}
}

// This synthetic service returns archive-issued status-frame guards. The MCP
// boundary must retain those guards through attachment metadata serialization.
type partialGapGuardedAttachments struct {
	fakeAttachments
	store *archive.Store
}

func (a *partialGapGuardedAttachments) Search(ctx context.Context, g archive.CredentialGrant, q archive.AttachmentSearchRequest) (archive.AttachmentPage, archive.ResponseGuard, error) {
	page, _, err := a.fakeAttachments.Search(ctx, g, q)
	if err != nil {
		return page, archive.ResponseGuard{}, err
	}
	status, guard, err := a.store.StatusGuarded(ctx, g.Scope)
	page.Snapshot = status.Snapshot
	return page, guard, err
}
func (a *partialGapGuardedAttachments) Prepare(ctx context.Context, g archive.CredentialGrant, id archive.MediaID, fetch, inline bool) (AttachmentDelivery, archive.ResponseGuard, error) {
	delivery, _, err := a.fakeAttachments.Prepare(ctx, g, id, fetch, inline)
	if err != nil {
		return delivery, archive.ResponseGuard{}, err
	}
	status, guard, err := a.store.StatusGuarded(ctx, g.Scope)
	delivery.Snapshot = status.Snapshot
	return delivery, guard, err
}
func TestPartialHistoryMCPAttachmentMetadataGapRevisionDiscardsBufferedResult(t *testing.T) {
	for _, alreadyPartial := range []bool{false, true} {
		for _, name := range []string{"search_attachments", "get_attachment"} {
			t.Run(map[bool]string{false: "first", true: "repeated"}[alreadyPartial]+"/"+name, func(t *testing.T) {
				fixture := openActualFixture(t)
				if alreadyPartial {
					recordPartialGap(t, fixture.store, fixture.account)
				}
				gapCommits := make(chan error, 1)
				credentials := &interleavedCredentials{Credentials: fixture.store, mutate: func() error {
					err := fixture.store.RecordUnknownHistoryGap(context.Background(), fixture.account, 229)
					gapCommits <- err
					return err
				}}
				service := &partialGapGuardedAttachments{store: fixture.store}
				server, _ := httpFixture(t, fixture.store, credentials, func(c *Config) { c.Attachments = service })
				args := map[string]any{}
				if name == "get_attachment" {
					args["attachment_id"] = "opaque-attachment"
				}
				status, body := rpc(t, server, chatToolRPC(t, name, args), nil)
				select {
				case err := <-gapCommits:
					if err != nil {
						t.Fatalf("synthetic gap commit failed before final epoch check: %v", err)
					}
				default:
					t.Fatal("gap commit hook did not run")
				}
				if status != http.StatusConflict || strings.Contains(body, "opaque-document") || strings.Contains(body, "Proposal") || strings.Contains(body, `"download_url"`) {
					t.Fatalf("expected final attachment coverage invalidation after successful gap commit: %d %s", status, body)
				}
				if credentials.calls != 2 {
					t.Fatal("attachment coverage mutation did not follow construction")
				}
			})
		}
	}
}
