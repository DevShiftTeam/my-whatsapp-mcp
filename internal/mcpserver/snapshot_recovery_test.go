package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type recoveredSnapshotReader struct {
	fakeReader
	snapshot archive.ReadSnapshot
}

func (r *recoveredSnapshotReader) SearchGuarded(ctx context.Context, scope archive.ReadScope, request archive.SearchRequest) (archive.SearchPage, archive.ResponseGuard, error) {
	page, guard, err := r.fakeReader.SearchGuarded(ctx, scope, request)
	page.Snapshot = r.snapshot
	return page, guard, err
}

func (r *recoveredSnapshotReader) ContextGuarded(ctx context.Context, scope archive.ReadScope, request archive.ContextRequest) (archive.ContextPage, archive.ResponseGuard, error) {
	page, guard, err := r.fakeReader.ContextGuarded(ctx, scope, request)
	page.Snapshot = r.snapshot
	return page, guard, err
}

func (r *recoveredSnapshotReader) ListChatsGuarded(ctx context.Context, scope archive.ReadScope, request archive.ChatListRequest) (archive.ChatPage, archive.ResponseGuard, error) {
	page, guard, err := r.fakeReader.ListChatsGuarded(ctx, scope, request)
	page.Snapshot = r.snapshot
	return page, guard, err
}

func (r *recoveredSnapshotReader) StatusGuarded(ctx context.Context, scope archive.ReadScope) (archive.StatusSnapshot, archive.ResponseGuard, error) {
	page, guard, err := r.fakeReader.StatusGuarded(ctx, scope)
	page.Snapshot = r.snapshot
	return page, guard, err
}

func TestMCPEveryToolLabelsItsSelectedRecoverySnapshot(t *testing.T) {
	reader := &recoveredSnapshotReader{snapshot: archive.ReadSnapshot{RecoveryCode: "owner_restored_snapshot", RecoveryEpoch: 7}}
	server, _ := httpFixture(t, reader, credentialsFixture(), nil)
	session, ctx := sdkSession(t, server, fakeToken())
	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{"search_messages", map[string]any{"query": "synthetic"}},
		{"get_context", map[string]any{"message_id": "opaque-message"}},
		{"list_chats", map[string]any{}},
		{"get_index_status", map[string]any{}},
	} {
		before := reader.reads
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.name, Arguments: tool.args})
		if err != nil || result.IsError || len(result.Content) != 1 {
			t.Fatalf("%s failed to return its selected archive: %v", tool.name, err)
		}
		content, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatal("missing text content")
		}
		var label struct {
			RecoveryCode      string `json:"recovery_code"`
			RecoveryEpoch     uint64 `json:"recovery_epoch"`
			ArchiveState      string `json:"archive_state"`
			CurrentPhoneState string `json:"current_phone_state"`
			CoverageWarning   string `json:"coverage_warning"`
		}
		if err := json.Unmarshal([]byte(content.Text), &label); err != nil {
			t.Fatal(err)
		}
		if label.RecoveryCode != "owner_restored_snapshot" || label.RecoveryEpoch != 7 || label.ArchiveState != "stored_snapshot" || label.CurrentPhoneState != "unverified" || !strings.Contains(label.CoverageWarning, "interruption") || !strings.Contains(label.CoverageWarning, "deletions") {
			t.Fatalf("%s omitted its recovery identity or missing-change limitation", tool.name)
		}
		if reader.reads != before+1 {
			t.Fatalf("%s queried a different snapshot after selecting content", tool.name)
		}
	}
}
