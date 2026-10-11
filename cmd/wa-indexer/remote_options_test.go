package main

import "testing"

func TestRemoteLaunchOptionsRequireTrustedCompleteRoute(t *testing.T) {
	valid := []string{"serve", "--mcp-listen", "127.0.0.1:8123", "--mcp-public-url", "https://mcp.example.com/mcp", "--mcp-header-auth-available"}
	opts, err := parseArgs(valid)
	if err != nil || opts.mcpListen != "127.0.0.1:8123" || opts.mcpPublicURL != "https://mcp.example.com/mcp" || !opts.mcpHeaderAuth {
		t.Fatal("valid trusted route rejected")
	}
	for _, args := range [][]string{
		{"serve", "--mcp-listen", "127.0.0.1:8123"},
		{"serve", "--mcp-public-url", "https://mcp.example.com/mcp"},
		{"serve", "--mcp-header-auth-available"},
		{"serve", "--mcp-listen", "127.0.0.1:0", "--mcp-public-url", "https://mcp.example.com/mcp"},
		{"serve", "--mcp-listen", "localhost:8123", "--mcp-public-url", "https://mcp.example.com/mcp"},
		{"serve", "--mcp-listen", "127.0.0.1:08123", "--mcp-public-url", "https://mcp.example.com/mcp"},
		{"serve", "--mcp-listen", "0.0.0.0:8123", "--mcp-public-url", "https://mcp.example.com/mcp"},
		{"serve", "--mcp-listen", "127.0.0.1:8123", "--mcp-public-url", "https://127.0.0.1/mcp"},
		{"serve", "--mcp-listen", "127.0.0.1:8123", "--mcp-public-url", "https://mcp.example.com/mcp?token=private"},
		{"doctor", "--mcp-listen", "127.0.0.1:8123"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("unsafe or incomplete remote route accepted")
		}
	}
}
