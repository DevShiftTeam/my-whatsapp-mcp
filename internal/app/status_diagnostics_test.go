//go:build sqlite_fts5

package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
)

func TestStatusPreservesSafeRuntimeFailureCodes(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx, Config{DataDir: privateTestRoot(t), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	if _, err = a.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0); err != nil {
		t.Fatal(err)
	}
	if err = a.StartPairing(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := a.Status(ctx)
	if err != nil || before.Messages == 0 || before.Keyword.Complete == 0 {
		t.Fatal("fixture did not create a readable archive")
	}
	a.statusChanged(ingestion.Status{
		Connection:             "reconciliation_required",
		ReconciliationRequired: true,
		Codes: []string{
			"coverage_unknown", "PRIVATE_ACCOUNT_OR_ERROR_TEXT", "history_limit_exceeded",
			"history_limit_exceeded", "archive_unavailable", "reconciliation_required",
		},
	})
	status, err := a.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.ReconciliationRequired || status.Connection != "reconciliation_required" || status.Messages != 0 || len(status.Chats) != 0 {
		t.Fatal("runtime denial did not mask archive contents")
	}
	if status.Keyword.State != "unavailable" || status.Keyword.Code != "reconciliation_required" {
		t.Fatal("runtime denial falsely reported keyword search as available")
	}
	for _, progress := range []archive.Progress{status.Keyword, status.Semantic, status.Media} {
		if progress.Pending != 0 || progress.Complete != 0 || progress.Failed != 0 {
			t.Fatal("runtime denial exposed archive progress counts")
		}
	}
	want := []string{"reconciliation_required", "history_limit_exceeded", "archive_unavailable", "demo"}
	if !reflect.DeepEqual(status.Codes, want) {
		t.Fatalf("public failure codes = %v, want %v", status.Codes, want)
	}
}
