package clientsetup

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const dockerConfigPath = "/var/lib/wa-indexer/data/clients/native-example.json"

func TestDockerStdioCommandUsesLiteralArguments(t *testing.T) {
	command, args, err := DockerStdioCommand("wa-indexer", dockerConfigPath)
	want := []string{"exec", "-i", "wa-indexer", "/usr/local/bin/wa-indexer", "mcp", "stdio", "--config", dockerConfigPath}
	if err != nil || command != "docker" || !reflect.DeepEqual(args, want) {
		t.Fatalf("unexpected bridge: %q %#v %v", command, args, err)
	}
	args[0] = "mutated"
	_, fresh, err := DockerStdioCommand("wa-indexer", dockerConfigPath)
	if err != nil || !reflect.DeepEqual(fresh, want) {
		t.Fatal("caller mutation changed a later command")
	}
}

func TestDockerStdioCommandRejectsUnsafeInputs(t *testing.T) {
	for _, name := range []string{"", "-option", " name", "name ", "two names", "name\n", "name\x00", "name;id", "name$(id)", "${NAME}", "name/other", "שם", strings.Repeat("a", 129)} {
		command, args, err := DockerStdioCommand(name, dockerConfigPath)
		if !errors.Is(err, ErrInvalidConfig) || command != "" || args != nil {
			t.Fatalf("unsafe container produced command: %q %q %#v %v", name, command, args, err)
		}
	}
	for _, path := range []string{"", "relative.json", "/private/../native.json", "/private/./native.json", "/private//native.json", "/private/native.json/", "/private/${NAME}.json", "/private/native\n.json", "/private/native;id.json", "/private/$(id).json"} {
		command, args, err := DockerStdioCommand("wa-indexer", path)
		if !errors.Is(err, ErrInvalidConfig) || command != "" || args != nil {
			t.Fatalf("unsafe path produced command: %q %q %#v %v", path, command, args, err)
		}
	}
	for _, name := range []string{"a", "WA_Indexer.preview-1", strings.Repeat("a", 128)} {
		if _, _, err := DockerStdioCommand(name, dockerConfigPath); err != nil {
			t.Fatalf("rejected valid bounded container: %q %v", name, err)
		}
	}
}

func TestDockerGuidesParseAsExplicitClientCommands(t *testing.T) {
	for _, client := range []Client{CodexCLI, CodexDesktop, ClaudeCode} {
		t.Run(string(client), func(t *testing.T) {
			cfg := SetupConfig{LocalConfigPath: dockerConfigPath, LocalCredentialID: fixtureID, Docker: &DockerRunner{Container: "wa-indexer"}, Token: "synthetic-private-token"}
			guide, err := Render(client, Local, cfg)
			if err != nil {
				t.Fatal(err)
			}
			var command string
			var args []string
			if client == ClaudeCode {
				var decoded struct {
					Servers map[string]struct {
						Command string   `json:"command"`
						Args    []string `json:"args"`
						Type    string   `json:"type"`
					} `json:"mcpServers"`
				}
				if err := json.Unmarshal([]byte(guide.Configuration), &decoded); err != nil {
					t.Fatal(err)
				}
				server := decoded.Servers["wa_indexer"]
				if server.Type != "stdio" {
					t.Fatal("missing stdio transport")
				}
				command, args = server.Command, server.Args
			} else {
				// Parse the generated TOML's basic-string/array subset independently.
				lines := strings.Split(strings.TrimSpace(guide.Configuration), "\n")
				if len(lines) != 3 || lines[0] != "[mcp_servers.wa_indexer]" {
					t.Fatal("invalid TOML table")
				}
				command, err = strconv.Unquote(strings.TrimPrefix(lines[1], "command = "))
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[2], "args = ")), &args); err != nil {
					t.Fatal(err)
				}
			}
			want := []string{"exec", "-i", "wa-indexer", "/usr/local/bin/wa-indexer", "mcp", "stdio", "--config", dockerConfigPath}
			if command != "docker" || !reflect.DeepEqual(args, want) {
				t.Fatalf("client command lost boundaries: %q %#v", command, args)
			}
			if guide.Status != NotYetVerified || guide.SupportLabel != "Not yet verified" || !strings.Contains(guide.CredentialPlacement, "inside the container") || !strings.Contains(strings.Join(guide.Warnings, " "), "PATH") {
				t.Fatalf("missing container instructions/evidence boundary: %#v", guide)
			}
			encoded, err := json.Marshal(guide)
			if err != nil || strings.Contains(string(encoded), cfg.Token) || strings.Contains(guide.Configuration, "env") || strings.Contains(guide.Command, " -t ") {
				t.Fatal("bridge leaked credentials or allocated a TTY")
			}
		})
	}
}

func TestDockerGuideRejectsAmbiguousOrTaintedComposition(t *testing.T) {
	for _, cfg := range []SetupConfig{
		{LocalConfigPath: dockerConfigPath, LocalCredentialID: fixtureID, Docker: &DockerRunner{Container: "-option"}},
		{ExecutablePath: "/host/alternative", LocalConfigPath: dockerConfigPath, LocalCredentialID: fixtureID, Docker: &DockerRunner{Container: "wa-indexer"}},
		{LocalConfigPath: dockerConfigPath, LocalCredentialID: fixtureID, Docker: &DockerRunner{Container: "secret-marker"}, Token: "secret-marker"},
	} {
		guide, err := Render(CodexCLI, Local, cfg)
		if !errors.Is(err, ErrInvalidConfig) || guide.Configuration != "" {
			t.Fatal("invalid Docker composition rendered")
		}
		encoded, err := json.Marshal(cfg)
		if err != nil || cfg.Token != "" && strings.Contains(string(encoded), cfg.Token) {
			t.Fatal("Docker metadata leaked token in diagnostics")
		}
	}
	cfg := remoteFixture()
	cfg.Docker = &DockerRunner{Container: "wa-indexer"}
	if _, err := Render(CodexCLI, Remote, cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("remote accepted local Docker runner")
	}
}
