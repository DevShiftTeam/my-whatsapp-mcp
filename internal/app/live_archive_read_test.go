package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

func liveReadTestApplication(t *testing.T) *Application {
	t.Helper()
	a, err := newApplication(context.Background(), Config{DataDir: privateTestRoot(t), Offline: true}, func(ingestion.Config, archive.ConnectorStore, ingestion.Callbacks) (connector, error) {
		t.Error("offline live-read test attempted connector construction")
		return nil, archive.ErrSourceUnavailable
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return a
}

func liveReadHasUnknownGap(t *testing.T, value any) bool {
	t.Helper()
	switch page := value.(type) {
	case archive.SearchPage:
		return page.Snapshot.UnknownHistoryGap
	case archive.ContextPage:
		return page.Snapshot.UnknownHistoryGap
	case archive.DiscoveryPage:
		return page.Snapshot.UnknownHistoryGap
	case archive.StatusSnapshot:
		return page.Snapshot.UnknownHistoryGap
	case archive.StatsSnapshot:
		return page.UnknownHistoryGap
	default:
		t.Fatalf("unexpected owner read type %T", value)
		return false
	}
}

func TestLiveArchiveReadGapInvalidatesAllHeldResponseKinds(t *testing.T) {
	ctx := context.Background()
	a := liveReadTestApplication(t)
	policy, err := a.store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	const account = "synthetic-live-read-account"
	_, err = a.store.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: account, Chat: "synthetic-chat", Sender: "synthetic-sender", Message: "synthetic-message"}, Kind: archive.EventOriginal, Body: "preserved live canary", BodyKind: archive.MessageText, OriginalSentAt: time.Now().Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	scope := archive.ReadScope{Account: account, Principal: "owner", AllChats: true, PolicyGeneration: policy.Generation}
	page, err := a.store.Search(ctx, scope, archive.SearchRequest{Query: "canary"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatal("missing synthetic live body", err)
	}
	requests := []localui.ArchiveReadRequest{
		{Kind: "search", Search: archive.SearchRequest{Query: "canary"}},
		{Kind: "context", Context: archive.ContextRequest{MessageID: page.Hits[0].MessageID, Before: 1, After: 1}},
		{Kind: "chats"},
		{Kind: "status"},
		{Kind: "stats"},
	}
	readAll := func(wantGap bool) []localui.ArchiveReadResult {
		t.Helper()
		results := make([]localui.ArchiveReadResult, 0, len(requests))
		for _, request := range requests {
			result, err := a.ReadLiveArchive(ctx, request)
			if err != nil {
				t.Fatalf("owner %s read failed: %v", request.Kind, err)
			}
			t.Cleanup(result.Release)
			if result.Check == nil || result.Release == nil || result.Check(ctx) != nil {
				t.Fatal("new owner read lacks a valid final witness", request.Kind)
			}
			if liveReadHasUnknownGap(t, result.Value) != wantGap {
				t.Fatal("owner response lost current gap metadata", request.Kind)
			}
			results = append(results, result)
		}
		return results
	}
	assertInvalidated := func(results []localui.ArchiveReadResult) {
		t.Helper()
		for i, result := range results {
			if err := result.Check(ctx); !errors.Is(err, archive.ErrCursorStale) && !errors.Is(err, archive.ErrPolicyChanged) {
				t.Fatalf("%s witness survived a new gap: %v", requests[i].Kind, err)
			}
			result.Release()
			result.Release()
			if err := result.Check(ctx); !errors.Is(err, archive.ErrArchiveUnavailable) {
				t.Fatalf("%s witness remained usable after release: %v", requests[i].Kind, err)
			}
		}
	}
	initial := readAll(false)
	if err := a.store.RecordUnknownHistoryGap(ctx, account, 229); err != nil {
		t.Fatal(err)
	}
	assertInvalidated(initial)
	partial := readAll(true)
	if err := a.store.RecordUnknownHistoryGap(ctx, account, 229); err != nil {
		t.Fatal(err)
	}
	assertInvalidated(partial)
	for _, result := range readAll(true) {
		result.Release()
		result.Release()
		if err := result.Check(ctx); !errors.Is(err, archive.ErrArchiveUnavailable) {
			t.Fatal("released fresh response still passed its final check", err)
		}
	}
}

func TestLiveArchiveReadStatusAndStatsRemainAvailableBeforeSetup(t *testing.T) {
	ctx := context.Background()
	a := liveReadTestApplication(t)
	for _, kind := range []string{"status", "stats"} {
		result, err := a.ReadLiveArchive(ctx, localui.ArchiveReadRequest{Kind: kind})
		if err != nil {
			t.Fatal("empty setup owner metadata unavailable", kind, err)
		}
		t.Cleanup(result.Release)
		if err := result.Check(ctx); err != nil {
			t.Fatal("empty setup metadata witness failed", kind, err)
		}
		if liveReadHasUnknownGap(t, result.Value) {
			t.Fatal("empty setup invented an unknown history gap")
		}
		if value, ok := result.Value.(archive.StatsSnapshot); ok && (value.Messages != 0 || value.Chats != 0) {
			t.Fatal("empty setup returned nonempty statistics")
		}
		result.Release()
		result.Release()
		if err := result.Check(ctx); !errors.Is(err, archive.ErrArchiveUnavailable) {
			t.Fatal("released empty-setup witness remained usable", kind, err)
		}
	}
}

func TestLiveArchiveReadEmptyStatsPreservesExistingGap(t *testing.T) {
	ctx := context.Background()
	a := liveReadTestApplication(t)
	if err := a.store.RecordUnknownHistoryGap(ctx, "synthetic-unconfigured-account", 229); err != nil {
		t.Fatal(err)
	}
	result, err := a.ReadLiveArchive(ctx, localui.ArchiveReadRequest{Kind: "stats"})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Release()
	if err := result.Check(ctx); err != nil {
		t.Fatal(err)
	}
	stats, ok := result.Value.(archive.StatsSnapshot)
	if !ok || !stats.UnknownHistoryGap || stats.Messages != 0 {
		t.Fatal("empty dashboard omitted existing partial-history warning")
	}
}
