package app

import (
	"context"
	"errors"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func TestOwnerStatsBeforeLinkingDoesNotExpireSession(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx, Config{DataDir: privateTestRoot(t), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	assertEmpty := func() {
		t.Helper()
		page, err := a.Stats(ctx, archive.StatsRequest{})
		if err != nil || page.Messages != 0 || page.Chats != 0 || page.SampledAt == 0 || page.Daily == nil || page.TopChats == nil {
			t.Fatalf("first-run stats: %#v, %v", page, err)
		}
	}
	assertEmpty()
	if _, err = a.Stats(ctx, archive.StatsRequest{Kind: "video"}); !errors.Is(err, archive.ErrInvalidQuery) {
		t.Fatalf("unlinked invalid query: %v", err)
	}
	if _, err = a.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0); err != nil {
		t.Fatal(err)
	}
	assertEmpty()
	if err = a.StartPairing(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := a.Stats(ctx, archive.StatsRequest{})
	if err != nil || page.Messages == 0 {
		t.Fatalf("linked demo stats: %#v, %v", page, err)
	}
	account, err := a.store.BoundAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.MarkUnsafe(ctx, account, archive.UnsafeReason("mutation_metadata_missing")); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Stats(ctx, archive.StatsRequest{}); !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatalf("unsafe bound archive hidden: %v", err)
	}
}
