package clientsetup

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeRemoteURLPublishesCanonicalSharedRoute(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"https://MCP.EXAMPLE.COM.:443/mcp", "https://mcp.example.com/mcp"},
		{"https://mcp.example.com:08443/mcp", "https://mcp.example.com:8443/mcp"},
		{"https://8.8.8.8/mcp", "https://8.8.8.8/mcp"},
		{"https://[2606:4700:4700::1111]:443/mcp", "https://[2606:4700:4700::1111]/mcp"},
	} {
		got, err := NormalizeRemoteURL(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("canonical route mismatch: got %q error %v", got, err)
		}
		cfg := remoteFixture()
		cfg.RemoteURL = tc.input
		guide, err := Render(CodexCLI, Remote, cfg)
		if err != nil || guide.Endpoint != got {
			t.Fatal("producer and client guide normalization diverged")
		}
	}
}

func TestNormalizeRemoteURLRejectsUntrustedOrAmbiguousRoutes(t *testing.T) {
	for _, input := range []string{
		"", "http://mcp.example.com/mcp", "https://mcp.example.com/admin",
		"https://mcp.example.com/mcp/", "https://mcp.example.com/m%63p",
		"https://mcp.example.com/mcp%2f", "https://mcp.example.com/mcp?",
		"https://mcp.example.com/mcp?token=private-secret",
		"https://mcp.example.com/mcp#", "https://mcp.example.com/mcp#private-secret",
		"https://user:private-secret@mcp.example.com/mcp",
		"https://localhost/mcp", "https://127.0.0.1/mcp", "https://10.0.0.1/mcp",
		"https://[::1]/mcp", "https://127.1/mcp", "https://0177.0.0.1/mcp",
		"https://0x7f.0.0.1/mcp", "https://2130706433/mcp",
		"https://mcp.example.123/mcp", "https://127%2e0%2e0%2e1/mcp",
		"https://mcp.example.com:/mcp", "https://mcp.example.com:65536/mcp",
		" https://mcp.example.com/mcp", "https://mcp.example.com/mcp\n",
		"https://mcp.example.com/mcp\x00", "https://mcp.example.\xff/mcp",
		"https://" + strings.Repeat("a", 4096) + ".example.com/mcp",
	} {
		endpoint, err := NormalizeRemoteURL(input)
		if endpoint != "" || !errors.Is(err, ErrInvalidConfig) || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("invalid remote route escaped or disclosed input")
		}
	}
}
