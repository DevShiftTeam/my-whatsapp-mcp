package mcpserver

import (
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHistoricalSnapshotMCPDeniesSendsBeforeBackendAndLabelsStatus(t *testing.T) {
	backend := &sendBackend{}
	server, _ := httpFixture(t, &fakeReader{}, credentialsFixture(), func(c *Config) {
		c.Outbound = backend
		c.HistoricalSnapshot = &archive.HistoricalSnapshot{CapturedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), ReadOnly: true, PhoneState: "unknown"}
	})
	code, body := rpc(t, server, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if code != 200 || !strings.Contains(body, "send_message") || !strings.Contains(body, "get_send_status") {
		t.Fatal("snapshot removed tools")
	}
	code, body = rpc(t, server, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send_message","arguments":{"chat_id":"chat","request_id":"r1","text":"canary"}}}`, nil)
	if code != 200 || !strings.Contains(body, "historical_snapshot_read_only") || backend.callCount() != 0 {
		t.Fatalf("snapshot send reached backend: %d %s", code, body)
	}
	req, _ := http.NewRequest("POST", server.URL+"/api/send/media", strings.NewReader(`{"invalid":"must not matter"}`))
	req.Header.Set("Authorization", "Bearer "+fakeToken())
	req.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 || backend.callCount() != 0 {
		t.Fatal("snapshot REST send reached backend")
	}
	code, body = rpc(t, server, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_index_status","arguments":{}}}`, nil)
	if code != 200 || !strings.Contains(body, "historical_snapshot") || !strings.Contains(body, "2026-01-02T03:04:05Z") || !strings.Contains(body, "partial") {
		t.Fatalf("MCP freshness omitted: %d %s", code, body)
	}
}
