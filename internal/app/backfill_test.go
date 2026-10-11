package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

func TestBackfillOnlyNewSelectedChatsAndNeverImplicitAll(t *testing.T) {
	previous := archive.ArchivePolicy{Generation: 1, ChatMode: archive.ChatsSelective, IncludedChats: []archive.ChatID{"old"}}
	current := archive.ArchivePolicy{Generation: 2, ChatMode: archive.ChatsSelective, IncludedChats: []archive.ChatID{"old", "new", "new"}}
	request, needed := selectedBackfillRequest(previous, current)
	if !needed || request.AllSelected || len(request.Chats) != 1 || request.Chats[0] != "new" || request.PolicyGeneration != 2 || request.Limit != 32 {
		t.Fatalf("broadened request: %+v", request)
	}
	current.IncludedChats = nil
	if request, needed := selectedBackfillRequest(previous, current); needed || request.AllSelected {
		t.Fatal("empty selection became all chats")
	}
	current.ChatMode = archive.ChatsAll
	if request, needed := selectedBackfillRequest(previous, current); !needed || !request.AllSelected || len(request.Chats) != 0 {
		t.Fatal("explicit all selection was not preserved")
	}
}

func TestBackfillDateExpansionStaysWithinExplicitSelectedChats(t *testing.T) {
	previous := archive.ArchivePolicy{Generation: 1, DateMode: archive.DateSince, ImportStart: time.Unix(200, 0), ChatMode: archive.ChatsSelective, IncludedChats: []archive.ChatID{"selected"}}
	current := previous
	current.Generation = 2
	current.ImportStart = time.Unix(100, 0)
	request, needed := selectedBackfillRequest(previous, current)
	if !needed || request.AllSelected || len(request.Chats) != 1 || request.Chats[0] != "selected" {
		t.Fatal("date expansion lost or broadened selected scope")
	}
	current.ImportStart = time.Unix(300, 0)
	if _, needed := selectedBackfillRequest(previous, current); needed {
		t.Fatal("date narrowing requested backfill")
	}
}

func TestBackfillRollingRetentionExpansionAndRemovalRemainSelected(t *testing.T) {
	previous := archive.ArchivePolicy{Generation: 1, DateMode: archive.DateAllAvailable, RollingRetention: 24 * time.Hour, ChatMode: archive.ChatsSelective, IncludedChats: []archive.ChatID{"selected"}}
	for _, horizon := range []time.Duration{7 * 24 * time.Hour, 0} {
		current := previous
		current.Generation = 2
		current.RollingRetention = horizon
		request, needed := selectedBackfillRequest(previous, current)
		if !needed || request.AllSelected || len(request.Chats) != 1 || request.Chats[0] != "selected" {
			t.Fatal("retention widening lost or broadened selected scope")
		}
	}
	current := previous
	current.Generation = 2
	current.RollingRetention = 12 * time.Hour
	if _, needed := selectedBackfillRequest(previous, current); needed {
		t.Fatal("retention narrowing requested backfill")
	}
}

type downloadWakeConnector struct {
	connector
	wakes int
}

func (c *downloadWakeConnector) WakeOriginalDownloads() { c.wakes++ }

func TestBudgetOnlyWakesDownloadsAfterSuccessfulIncrease(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	runtime := &downloadWakeConnector{}
	a.runtime = runtime
	defer func() { a.runtime = nil }()
	budget, err := a.OriginalBudget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	changed, err := a.SetOriginalBudget(context.Background(), localui.OriginalBudgetRequest{MaximumBytes: 3 << 30, ExpectedRevision: budget.Revision})
	if err != nil || runtime.wakes != 1 {
		t.Fatalf("increase did not wake: %v %d", err, runtime.wakes)
	}
	if _, err := a.SetOriginalBudget(context.Background(), localui.OriginalBudgetRequest{MaximumBytes: 4 << 30, ExpectedRevision: budget.Revision}); err == nil || runtime.wakes != 1 {
		t.Fatal("stale CAS woke downloads")
	}
	if _, err := a.SetOriginalBudget(context.Background(), localui.OriginalBudgetRequest{MaximumBytes: 2 << 30, ExpectedRevision: changed.Revision}); err != nil || runtime.wakes != 1 {
		t.Fatal("lowering woke downloads")
	}
	a.resources.recoveryPending.Store(true)
	a.wakeOriginalDownloads()
	if runtime.wakes != 1 {
		t.Fatal("unresolved pins admitted download wake")
	}
}

func TestBackfillSelectedTransitionIsBoundedAndExcludedChatsStayExcluded(t *testing.T) {
	current := archive.ArchivePolicy{Generation: 7, ChatMode: archive.ChatsSelective}
	for i := 0; i < 100; i++ {
		current.IncludedChats = append(current.IncludedChats, archive.ChatID(fmt.Sprintf("chat-%d", i)))
	}
	request, needed := selectedBackfillRequest(archive.ArchivePolicy{}, current)
	if !needed || len(request.Chats) != 32 || request.AllSelected {
		t.Fatalf("unbounded transition: %+v", request)
	}
	request, needed = selectedBackfillRequest(archive.ArchivePolicy{Generation: 6, ChatMode: archive.ChatsAll}, current)
	if needed || len(request.Chats) != 0 {
		t.Fatal("policy narrowing requested previously selected chats again")
	}
}
