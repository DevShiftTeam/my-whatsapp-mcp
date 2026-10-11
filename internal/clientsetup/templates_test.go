package clientsetup

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const fixtureID = "0123456789abcdef0123456789abcdef"

func localFixture() SetupConfig {
	return SetupConfig{
		ExecutablePath:    "/Applications/WA Indexer's כלי.app/Contents/MacOS/wa-indexer",
		LocalConfigPath:   "/Users/example/WA Indexer's נתונים/native.json",
		LocalCredentialID: fixtureID,
	}
}

func remoteFixture() SetupConfig {
	return SetupConfig{RemoteURL: "https://mcp.example.com/mcp", Auth: Bearer, CredentialID: fixtureID}
}

func TestLocalGuidesKeepPathsAsSeparateArguments(t *testing.T) {
	for _, client := range []Client{CodexCLI, CodexDesktop, ClaudeCode} {
		t.Run(string(client), func(t *testing.T) {
			cfg := localFixture()
			guide, err := Render(client, Local, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if guide.Status != NotYetVerified || guide.Endpoint != "" {
				t.Fatalf("unexpected support or endpoint: %#v", guide)
			}
			if !strings.Contains(guide.Command, "'\"'\"'") || !strings.Contains(guide.Command, "mcp stdio --config") {
				t.Fatalf("unsafe or wrong local command: %s", guide.Command)
			}
			if client == ClaudeCode {
				var decoded struct {
					Servers map[string]struct {
						Type    string   `json:"type"`
						Command string   `json:"command"`
						Args    []string `json:"args"`
					} `json:"mcpServers"`
				}
				if err := json.Unmarshal([]byte(guide.Configuration), &decoded); err != nil {
					t.Fatal(err)
				}
				server := decoded.Servers["wa_indexer"]
				if server.Type != "stdio" || server.Command != cfg.ExecutablePath || len(server.Args) != 4 || server.Args[0] != "mcp" || server.Args[1] != "stdio" || server.Args[2] != "--config" || server.Args[3] != cfg.LocalConfigPath {
					t.Fatalf("JSON lost path boundaries: %#v", server)
				}
			} else if !strings.Contains(guide.Configuration, "[mcp_servers.wa_indexer]") || !strings.Contains(guide.Configuration, "--config") || strings.Contains(guide.Configuration, "env_vars") {
				t.Fatalf("unexpected native config: %s", guide.Configuration)
			}
			if !strings.Contains(guide.CredentialPlacement, "0600") || !strings.Contains(guide.RevokeAction, fixtureID) || guide.TestAction == "" {
				t.Fatalf("missing credential/test/revoke action: %#v", guide)
			}
		})
	}
}

func TestLocalGuidesDoNotExposeNetworkOrCredentialEnvironment(t *testing.T) {
	for _, client := range []Client{CodexCLI, CodexDesktop, ClaudeCode} {
		cfg := localFixture()
		cfg.Token = "synthetic-native-secret-must-remain-in-private-file"
		guide, err := Render(client, Local, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if guide.Endpoint != "" || strings.Contains(guide.Configuration, "http://") || strings.Contains(guide.Configuration, "env_vars") || strings.Contains(guide.Configuration, `"env"`) {
			t.Fatalf("%s exposed a local endpoint or credential environment setup", client)
		}
		data, err := json.Marshal(guide)
		if err != nil || strings.Contains(string(data), cfg.Token) {
			t.Fatalf("%s exposed the native credential in its setup projection", client)
		}
	}
}

func TestLocalConfigPathsMatchTheAdapterCanonicalContract(t *testing.T) {
	for _, client := range []Client{CodexCLI, CodexDesktop, ClaudeCode} {
		for _, configPath := range []string{
			"/private/./native.json", "/private/sub/../native.json",
			"/private//native.json", "/private/native.json/",
		} {
			cfg := localFixture()
			cfg.LocalConfigPath = configPath
			guide, err := Render(client, Local, cfg)
			if !errors.Is(err, ErrInvalidConfig) || guide.Configuration != "" || guide.Command != "" {
				t.Fatalf("%s accepted a config path rejected by the adapter: %#v %v", client, guide, err)
			}
		}
		cfg := localFixture()
		// Executable paths are independent of the adapter's --config guard.
		cfg.ExecutablePath = "/Applications/./WA Indexer/wa-indexer"
		guide, err := Render(client, Local, cfg)
		if err != nil || !strings.Contains(guide.Command, cfg.ExecutablePath) {
			t.Fatalf("%s changed an accepted literal executable path: %#v %v", client, guide, err)
		}
	}
}

func TestBearerGuidesDoNotExposeToken(t *testing.T) {
	const token = "secret-token-must-never-appear"
	for _, client := range []Client{CodexCLI, CodexDesktop, ClaudeCode, ClaudeConnectors, Composio} {
		t.Run(string(client), func(t *testing.T) {
			cfg := remoteFixture()
			cfg.Token = token
			cfg.HeaderAuthAvailable = true
			guide, err := Render(client, Remote, cfg)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(guide)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), token) || strings.Contains(fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg), token) {
				t.Fatal("credential leaked into guide or diagnostics")
			}
			configData, err := json.Marshal(cfg)
			if err != nil || strings.Contains(string(configData), token) {
				t.Fatal("credential leaked into serialized setup")
			}
			if guide.Status != NotYetVerified || guide.Endpoint != cfg.RemoteURL || guide.TestAction == "" || !strings.Contains(guide.RevokeAction, fixtureID) {
				t.Fatalf("missing exact remote actions: %#v", guide)
			}
			switch client {
			case CodexCLI, CodexDesktop:
				if !strings.Contains(guide.Configuration, `bearer_token_env_var = "WA_INDEXER_CLIENT_TOKEN"`) {
					t.Fatal("Codex must use a bearer environment variable")
				}
			case ClaudeCode:
				var decoded map[string]map[string]struct {
					Type    string            `json:"type"`
					Headers map[string]string `json:"headers"`
				}
				if err := json.Unmarshal([]byte(guide.Configuration), &decoded); err != nil {
					t.Fatal(err)
				}
				server := decoded["mcpServers"]["wa_indexer"]
				if server.Type != "http" || server.Headers["Authorization"] != "Bearer ${WA_INDEXER_CLIENT_TOKEN}" {
					t.Fatal("Claude Code must explicitly supply the Bearer scheme")
				}
			case ClaudeConnectors:
				if !strings.Contains(guide.Configuration, "Bearer <REDACTED>") || !strings.Contains(guide.CredentialPlacement, "beta") {
					t.Fatal("connector account gate or full header value missing")
				}
			case Composio:
				if !strings.Contains(guide.Configuration, "Bearer {{generic_api_key}}") || !strings.Contains(guide.Configuration, `"Authorization"`) {
					t.Fatal("Composio registration must map its key to Authorization: Bearer")
				}
			}
		})
	}
}

func TestUnsupportedCombinationsExplainTheirGate(t *testing.T) {
	for _, tc := range []struct {
		client Client
		mode   Mode
		cfg    SetupConfig
	}{
		{ClaudeConnectors, Local, localFixture()},
		{Composio, Local, localFixture()},
		{ClaudeConnectors, Remote, remoteFixture()},
		{ClaudeConnectors, Remote, SetupConfig{RemoteURL: "https://oauth.example.com/mcp", Auth: OAuth}},
		{Composio, Remote, SetupConfig{RemoteURL: "https://oauth.example.com/mcp", Auth: OAuth, OAuthAvailable: true}},
	} {
		guide, err := Render(tc.client, tc.mode, tc.cfg)
		if !errors.Is(err, ErrUnsupported) || guide.Status != Unsupported || guide.Explanation == "" || guide.Configuration != "" || guide.Command != "" {
			t.Fatalf("misleading unsupported guide for %s/%s: %#v %v", tc.client, tc.mode, guide, err)
		}
	}
}

func TestVerifiedOAuthRouteStillNeedsActualClientEvidence(t *testing.T) {
	for _, client := range []Client{CodexCLI, CodexDesktop, ClaudeCode, ClaudeConnectors} {
		cfg := SetupConfig{RemoteURL: "https://oauth.example.com/mcp", Auth: OAuth, OAuthAvailable: true}
		guide, err := Render(client, Remote, cfg)
		if err != nil || guide.Status != NotYetVerified || guide.Configuration == "" || guide.TestAction == "" || guide.RevokeAction == "" {
			t.Fatalf("OAuth candidate incorrectly advertised: %#v %v", guide, err)
		}
		if strings.Contains(guide.Configuration, "bearer_token_env_var") || strings.Contains(guide.Configuration, "Authorization") {
			t.Fatal("OAuth guide must not inject a static bearer credential")
		}
	}
}

func TestModesAndEndpointsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client Client
		mode   Mode
		cfg    SetupConfig
	}{
		{"unknown client", Client("unknown"), Local, localFixture()},
		{"unknown mode", CodexCLI, Mode("both"), localFixture()},
		{"remote in local", CodexCLI, Local, SetupConfig{ExecutablePath: "/app/wa-indexer", LocalConfigPath: "/private/native.json", LocalCredentialID: fixtureID, RemoteURL: "https://mcp.example.com/mcp"}},
		{"local in remote", CodexCLI, Remote, SetupConfig{ExecutablePath: "/app/wa-indexer", RemoteURL: "https://mcp.example.com/mcp", Auth: Bearer, CredentialID: fixtureID}},
		{"missing persisted local ID", CodexCLI, Local, SetupConfig{ExecutablePath: "/app/wa-indexer", LocalConfigPath: "/private/native.json"}},
		{"relative config", CodexCLI, Local, SetupConfig{ExecutablePath: "/app/wa-indexer", LocalConfigPath: "private/native.json", LocalCredentialID: fixtureID}},
		{"shell newline", CodexCLI, Local, SetupConfig{ExecutablePath: "/app/wa-indexer\nwhoami", LocalConfigPath: "/private/native.json", LocalCredentialID: fixtureID}},
		{"missing remote ID", CodexCLI, Remote, SetupConfig{RemoteURL: "https://mcp.example.com/mcp", Auth: Bearer}},
		{"missing auth", CodexCLI, Remote, SetupConfig{RemoteURL: "https://mcp.example.com/mcp", CredentialID: fixtureID}},
		{"HTTP", CodexCLI, Remote, SetupConfig{RemoteURL: "http://mcp.example.com/mcp", Auth: Bearer, CredentialID: fixtureID}},
		{"userinfo", CodexCLI, Remote, SetupConfig{RemoteURL: "https://user:secret@mcp.example.com/mcp", Auth: Bearer, CredentialID: fixtureID}},
		{"query token", CodexCLI, Remote, SetupConfig{RemoteURL: "https://mcp.example.com/mcp?token=secret", Auth: Bearer, CredentialID: fixtureID}},
		{"fragment", CodexCLI, Remote, SetupConfig{RemoteURL: "https://mcp.example.com/mcp#secret", Auth: Bearer, CredentialID: fixtureID}},
		{"empty fragment", CodexCLI, Remote, SetupConfig{RemoteURL: "https://mcp.example.com/mcp#", Auth: Bearer, CredentialID: fixtureID}},
		{"wrong route", CodexCLI, Remote, SetupConfig{RemoteURL: "https://mcp.example.com/admin", Auth: Bearer, CredentialID: fixtureID}},
		{"loopback", ClaudeConnectors, Remote, SetupConfig{RemoteURL: "https://127.0.0.1/mcp", Auth: Bearer, CredentialID: fixtureID, HeaderAuthAvailable: true}},
		{"localhost", Composio, Remote, SetupConfig{RemoteURL: "https://localhost/mcp", Auth: Bearer, CredentialID: fixtureID}},
		{"private IP", ClaudeCode, Remote, SetupConfig{RemoteURL: "https://192.168.1.7/mcp", Auth: Bearer, CredentialID: fixtureID}},
		{"secret environment name", CodexCLI, Remote, SetupConfig{RemoteURL: "https://mcp.example.com/mcp", Auth: Bearer, CredentialID: fixtureID, BearerTokenEnv: "OPENAI_API_KEY"}},
		{"injected environment name", ClaudeCode, Remote, SetupConfig{RemoteURL: "https://mcp.example.com/mcp", Auth: Bearer, CredentialID: fixtureID, BearerTokenEnv: "TOKEN} $(whoami)"}},
		{"dual host same case", CodexCLI, Remote, SetupConfig{RemoteURL: "https://MCP.example.com/mcp", Auth: Bearer, CredentialID: fixtureID, BearerURL: "https://mcp.example.com/mcp", OAuthURL: "https://MCP.EXAMPLE.COM:443/mcp"}},
		{"dual host same different ports", CodexCLI, Remote, SetupConfig{RemoteURL: "https://mcp.example.com/mcp", Auth: Bearer, CredentialID: fixtureID, BearerURL: "https://mcp.example.com/mcp", OAuthURL: "https://mcp.example.com:8443/mcp"}},
		{"selected OAuth host in bearer mode", CodexCLI, Remote, SetupConfig{RemoteURL: "https://oauth.example.com/mcp", Auth: Bearer, CredentialID: fixtureID, BearerURL: "https://bearer.example.com/mcp", OAuthURL: "https://oauth.example.com/mcp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guide, err := Render(tc.client, tc.mode, tc.cfg)
			if !errors.Is(err, ErrInvalidConfig) || guide.Configuration != "" || guide.Command != "" || strings.Contains(fmt.Sprint(err), "secret") {
				t.Fatalf("did not reject invalid configuration safely: %#v %v", guide, err)
			}
		})
	}
}

func TestRemoteURLNormalizationAndSeparateHosts(t *testing.T) {
	cfg := remoteFixture()
	cfg.RemoteURL = "https://MCP.EXAMPLE.COM:443/mcp"
	cfg.BearerURL = "https://mcp.example.com/mcp"
	cfg.OAuthURL = "https://oauth.example.com/mcp"
	cfg.BearerTokenEnv = "WA_INDEXER_CODEX_TOKEN"
	guide, err := Render(CodexCLI, Remote, cfg)
	if err != nil || guide.Endpoint != "https://mcp.example.com/mcp" || !strings.Contains(guide.Configuration, "WA_INDEXER_CODEX_TOKEN") {
		t.Fatalf("endpoint normalization failed: %#v %v", guide, err)
	}
}

func TestTokenCannotBeSmuggledIntoGuideFields(t *testing.T) {
	cfg := remoteFixture()
	cfg.Token = "secret-marker"
	cfg.RemoteURL = "https://secret-marker.example.com/mcp"
	guide, err := Render(CodexCLI, Remote, cfg)
	if !errors.Is(err, ErrInvalidConfig) || strings.Contains(fmt.Sprintf("%#v %v", guide, err), cfg.Token) {
		t.Fatal("secret echoed from an invalid noncredential field")
	}
	data, err := json.Marshal(cfg)
	if err != nil || strings.Contains(string(data), cfg.Token) {
		t.Fatal("tainted field leaked from setup diagnostics")
	}
}

func TestSetupDiagnosticsSuppressEveryURLBeforeValidation(t *testing.T) {
	for _, token := range []string{"", "different-designated-secret"} {
		cfg := remoteFixture()
		cfg.Token = token
		cfg.RemoteURL = "https://user:embedded-userinfo-secret@mcp.example.com/mcp"
		cfg.BearerURL = "https://bearer.example.com/mcp?token=embedded-query-secret"
		cfg.OAuthURL = "not-a-url#embedded-fragment-secret"
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"embedded-userinfo-secret", "embedded-query-secret", "embedded-fragment-secret", token} {
			if secret != "" && strings.Contains(string(data), secret) {
				t.Fatal("prevalidation URL secret leaked through diagnostics")
			}
		}
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"remote_url", "bearer_url", "oauth_url"} {
			if _, present := decoded[field]; present {
				t.Fatalf("diagnostics retained tainted %s", field)
			}
		}
	}
}

func TestTokensAreRecheckedAfterNormalizationAndDefaulting(t *testing.T) {
	for _, tc := range []struct{ token, endpoint string }{
		{"secret-marker", "https://SECRET-MARKER.example.com/mcp"},
		{"WA_INDEXER_CLIENT_TOKEN", "https://mcp.example.com/mcp"},
	} {
		cfg := remoteFixture()
		cfg.Token, cfg.RemoteURL = tc.token, tc.endpoint
		guide, err := Render(CodexCLI, Remote, cfg)
		if !errors.Is(err, ErrInvalidConfig) || guide.Configuration != "" || strings.Contains(fmt.Sprint(guide, err), tc.token) {
			t.Fatal("normalization/defaulting introduced a secret into the guide")
		}
	}
}

func TestRemoteURLsRejectLegacyIPv4Aliases(t *testing.T) {
	for _, host := range []string{"127.1", "10.1", "0x7f.0.0.1", "0177.0.0.1", "127.0.01", "2130706433", "0x7f000001", "mcp.example.123", "mcp.example.0x7f"} {
		cfg := remoteFixture()
		cfg.RemoteURL = "https://" + host + "/mcp"
		guide, err := Render(CodexCLI, Remote, cfg)
		if !errors.Is(err, ErrInvalidConfig) || guide.Configuration != "" {
			t.Fatalf("legacy numeric host reached a copyable configuration: %s", host)
		}
	}
	for _, host := range []string{"8.8.8.8", "mcp.example.com", "mcp.example.0x1g"} {
		cfg := remoteFixture()
		cfg.RemoteURL = "https://" + host + "/mcp"
		if _, err := Render(CodexCLI, Remote, cfg); err != nil {
			t.Fatalf("rejected ordinary DNS/canonical public IP: %s", host)
		}
	}
}

func TestClaudeCodeRejectsEnvironmentExpansionInLiteralPaths(t *testing.T) {
	for _, path := range []string{"/app/${NAME}/wa-indexer", "/app/${NAME:-default}/wa-indexer"} {
		for _, executable := range []bool{true, false} {
			cfg := localFixture()
			if executable {
				cfg.ExecutablePath = path
			} else {
				cfg.LocalConfigPath = path
			}
			guide, err := Render(ClaudeCode, Local, cfg)
			if !errors.Is(err, ErrUnsupported) || guide.Status != Unsupported || guide.Explanation == "" || guide.Configuration != "" || guide.Command != "" {
				t.Fatal("Claude Code environment expansion changed a literal path")
			}
			if _, err := Render(CodexCLI, Local, cfg); err != nil {
				t.Fatal("Claude-specific expansion restriction leaked into Codex")
			}
		}
	}
}

func TestRemoteHeadersRejectVariablesClaudeExpandsToEmpty(t *testing.T) {
	for _, name := range []string{"AWS_BEARER_TOKEN_BEDROCK", "HTTPS_PROXY", "NPM_TOKEN"} {
		cfg := remoteFixture()
		cfg.BearerTokenEnv = name
		guide, err := Render(ClaudeCode, Remote, cfg)
		if !errors.Is(err, ErrInvalidConfig) || guide.Configuration != "" {
			t.Fatalf("unusable reserved header variable accepted: %s", name)
		}
	}
}
