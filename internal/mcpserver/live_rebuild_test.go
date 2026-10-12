package mcpserver

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"strings"
	"testing"
)

type liveRebuildReader struct{ fakeReader }

func (f *liveRebuildReader) StatusGuarded(ctx context.Context, scope archive.ReadScope) (archive.StatusSnapshot, archive.ResponseGuard, error) {
	status, guard, err := f.fakeReader.StatusGuarded(ctx, scope)
	status.Connection = "connected"
	status.Coverage = "partial"
	status.Rebuild = &archive.RebuildStatus{Mode: "live_rebuild", Coverage: "partial", CurrentPhoneState: "unknown"}
	return status, guard, err
}
func TestLiveRebuildMCPReportsPartialLiveCoverageWithoutSnapshotMode(t *testing.T) {
	reader := &liveRebuildReader{}
	server, _ := httpFixture(t, reader, credentialsFixture(), func(c *Config) { c.Outbound = &sendBackend{} })
	code, body := rpc(t, server, statusRPC, nil)
	if code != 200 || !strings.Contains(body, `"live_rebuild"`) || !strings.Contains(body, `"connected"`) || !strings.Contains(body, `"unknown"`) || strings.Contains(body, `"historical_snapshot"`) {
		t.Fatal("live recovery lost current connection and uncertainty labels")
	}
	if reader.checks == 0 {
		t.Fatal("status bypassed response authorization")
	}
	code, body = rpc(t, server, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil)
	if code != 200 || !strings.Contains(body, "send_message") || !strings.Contains(body, "get_context") {
		t.Fatal("live recovery removed normal tools")
	}
}
