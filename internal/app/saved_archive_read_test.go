package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSavedArchiveApplicationCurrentOnlyAndLifetime(t *testing.T) {
	ctx := context.Background()
	a, e := New(ctx, Config{DataDir: historicalLiveTestRoot(t), Offline: true})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close(ctx)
	result, e := a.ReadSavedArchive(ctx, localui.ArchiveReadRequest{Kind: "browse", Browse: archive.SavedBrowseRequest{Limit: 50}})
	if e != nil {
		t.Fatal(e)
	}
	if result.SavedMetadata == nil || result.SavedMetadata.Source != "saved" || result.SavedMetadata.Historical != nil {
		result.Release()
		t.Fatal("current-only metadata")
	}
	page, ok := result.Value.(archive.SearchPage)
	if !ok || len(page.Hits) != 1 {
		result.Release()
		t.Fatal("current-only initial browse")
	}
	if e = result.Check(ctx); e != nil {
		result.Release()
		t.Fatal(e)
	}
	closed := make(chan error, 1)
	go func() { closed <- a.Close(ctx) }()
	select {
	case <-closed:
		result.Release()
		t.Fatal("close escaped retained saved read")
	case <-time.After(25 * time.Millisecond):
	}
	result.Release()
	result.Release()
	select {
	case e = <-closed:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("saved lifetime failed to drain")
	}
	if e = result.Check(ctx); e == nil {
		t.Fatal("released guard retained authority")
	}
}
func TestSavedArchiveApplicationHistoricalAndPolicyFence(t *testing.T) {
	ctx := context.Background()
	snapshot, captured := historicalFixture(t)
	cfg := Config{DataDir: historicalLiveTestRoot(t), Offline: true}
	configureHistoricalTest(t, &cfg, snapshot, captured)
	a, e := New(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close(ctx)
	result, e := a.ReadSavedArchive(ctx, localui.ArchiveReadRequest{Kind: "search", Search: archive.SearchRequest{Query: "historical", Limit: 50}})
	if e != nil {
		t.Fatal(e)
	}
	defer result.Release()
	page := result.Value.(archive.SearchPage)
	if len(page.Hits) != 1 || !archive.IsHistoricalHandle(string(page.Hits[0].MessageID)) || result.SavedMetadata.Historical == nil {
		t.Fatal("saved historical-only result missing")
	}
	if e = result.Check(ctx); e != nil {
		t.Fatal(e)
	}
	policy, e := a.store.Policy(ctx)
	if e != nil {
		t.Fatal(e)
	}
	policy.DateMode = archive.DateSince
	policy.ImportStart = time.Now()
	if _, e = a.store.SetPolicy(ctx, policy, policy.Generation); e != nil {
		t.Fatal(e)
	}
	if e = result.Check(ctx); e == nil {
		t.Fatal("saved historical response survived narrowed current authority")
	}
}
func TestSavedArchiveApplicationSetupAndErrorsRelease(t *testing.T) {
	ctx := context.Background()
	a, e := New(ctx, Config{DataDir: privateTestRoot(t), Offline: true})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close(ctx)
	result, e := a.ReadSavedArchive(ctx, localui.ArchiveReadRequest{Kind: "stats"})
	if e != nil {
		t.Fatal(e)
	}
	if result.SavedMetadata == nil || result.SavedMetadata.Historical != nil {
		result.Release()
		t.Fatal("setup metadata")
	}
	if e = result.Check(ctx); e != nil {
		result.Release()
		t.Fatal(e)
	}
	result.Release()
	if _, e = a.ReadSavedArchive(ctx, localui.ArchiveReadRequest{Kind: "browse"}); !errors.Is(e, archive.ErrUnauthorized) {
		t.Fatal("setup granted saved content authority")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = a.ReadSavedArchive(cancelled, localui.ArchiveReadRequest{Kind: "browse"}); e == nil {
		t.Fatal("cancelled saved read admitted")
	}
	if e = a.Close(ctx); e != nil {
		t.Fatal("failed query leaked data lock", e)
	}
}

func TestSavedArchiveApplicationStandaloneSnapshot(t *testing.T) {
	ctx := context.Background()
	root, captured := historicalFixture(t)
	path := filepath.Join(root, "archive", "archive.sqlite")
	before, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	a, e := New(ctx, Config{DataDir: root, Snapshot: &SnapshotConfig{CapturedAt: captured}})
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close(ctx)
	for _, q := range []localui.ArchiveReadRequest{
		{Kind: "browse", Browse: archive.SavedBrowseRequest{Limit: 50}},
		{Kind: "search", Search: archive.SearchRequest{Query: "historical", Limit: 50}},
		{Kind: "chats", Chats: archive.ChatListRequest{Limit: 50}},
		{Kind: "stats"},
	} {
		result, err := a.ReadSavedArchive(ctx, q)
		if err != nil {
			t.Fatalf("snapshot saved %s: %v", q.Kind, err)
		}
		if result.SavedMetadata == nil || result.SavedMetadata.Source != "saved" || result.SavedMetadata.Historical != nil {
			result.Release()
			t.Fatal("standalone capture gained supplement metadata")
		}
		switch value := result.Value.(type) {
		case archive.SearchPage:
			if len(value.Hits) != 1 {
				result.Release()
				t.Fatal("standalone saved page missing")
			}
		case archive.StatsSnapshot:
			if value.Messages != 1 {
				result.Release()
				t.Fatal("standalone saved stats missing")
			}
		case archive.ChatPage:
			if len(value.Chats) != 1 {
				result.Release()
				t.Fatal("standalone saved chats missing")
			}
		}
		if err = result.Check(ctx); err != nil {
			result.Release()
			t.Fatalf("snapshot saved %s final: %v", q.Kind, err)
		}
		result.Release()
	}
	if e = a.Pause(ctx); !errors.Is(e, archive.ErrHistoricalSnapshotReadOnly) {
		t.Fatal("saved route changed snapshot mutation fence")
	}
	if e = a.Close(ctx); e != nil {
		t.Fatal(e)
	}
	after, e := os.ReadFile(path)
	if e != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("saved reads changed standalone snapshot archive")
	}
}
