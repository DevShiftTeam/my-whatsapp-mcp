package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clearTranscriptionEnv(t *testing.T) {
	t.Helper()
	for _, key := range transcriptionAppEnv {
		t.Setenv(key, "")
	}
}

func testTranscriptionEnv() string {
	return transcriptionURLEnv + "=http://127.0.0.1:6971\n" + transcriptionTokenEnv + "=" + strings.Repeat("a", 43) + "\n"
}

func TestTranscriptionIsOptionalAndDefaultsToAutomaticLanguage(t *testing.T) {
	clearTranscriptionEnv(t)
	if cfg, err := transcriptionLaunchConfig(options{}); cfg != nil || err != nil {
		t.Fatal("unconfigured voice service activated or failed startup")
	}
	path := privateAdminTestEnv(t, testTranscriptionEnv())
	cfg, err := transcriptionLaunchConfig(options{transcriptionEnvFile: path})
	if err != nil || cfg == nil || cfg.Language != "auto" || cfg.Timeout != 600*time.Second {
		t.Fatal("private service configuration did not use documented defaults")
	}
}

func TestTranscriptionSharedEnvAndExplicitLanguage(t *testing.T) {
	clearTranscriptionEnv(t)
	body := testTranscriptionEnv() + transcriptionLanguageEnv + "=auto\n" + transcriptionTimeoutEnv + "=1800\n" +
		"WA_TRANSCRIPTION_TOKEN=" + strings.Repeat("a", 43) + "\nWA_TRANSCRIPTION_LISTEN=127.0.0.1:6971\n" +
		"WA_TRANSCRIPTION_MODEL_DIR=/private/Model Cache/model\nWA_TRANSCRIPTION_LANGUAGE=auto\n" +
		"WA_TRANSCRIPTION_CPU_THREADS=4\nWA_TRANSCRIPTION_BODY_TIMEOUT_SECONDS=30\n"
	path := privateAdminTestEnv(t, body)
	cfg, err := transcriptionLaunchConfig(options{transcriptionEnvFile: path})
	if err != nil || cfg == nil || cfg.Language != "auto" || cfg.Timeout != 30*time.Minute {
		t.Fatal("installer shared env or explicit language refused")
	}
	t.Setenv(transcriptionLanguageEnv, "auto")
	if _, err := transcriptionLaunchConfig(options{transcriptionEnvFile: path}); err != nil {
		t.Fatal("identical ambient value conflicted with private config")
	}
	t.Setenv(transcriptionLanguageEnv, "he")
	if _, err := transcriptionLaunchConfig(options{transcriptionEnvFile: path}); !errors.Is(err, errTranscriptionConfig) {
		t.Fatal("conflicting ambient language was silently selected")
	}
}

func TestTranscriptionEnvRefusesUnknownOrExecutableData(t *testing.T) {
	clearTranscriptionEnv(t)
	for _, body := range []string{
		"", testTranscriptionEnv() + "UNKNOWN=PRIVATE_CANARY\n",
		testTranscriptionEnv() + transcriptionLanguageEnv + "=he\n" + transcriptionLanguageEnv + "=en\n",
		testTranscriptionEnv() + "export WA_TRANSCRIPTION_TOKEN=PRIVATE_CANARY\n",
		testTranscriptionEnv() + "WA_TRANSCRIPTION_MODEL_DIR='PRIVATE_CANARY'\n",
		testTranscriptionEnv() + "WA_TRANSCRIPTION_MODEL_DIR=$(PRIVATE_CANARY)\n",
		testTranscriptionEnv() + "WA_TRANSCRIPTION_MODEL_DIR=`PRIVATE_CANARY`\n",
		testTranscriptionEnv() + "WA_TRANSCRIPTION_MODEL_DIR=PRIVATE_CANARY\x00\n",
		testTranscriptionEnv() + transcriptionTimeoutEnv + "=9\n",
		testTranscriptionEnv() + transcriptionTimeoutEnv + "=1801\n",
		testTranscriptionEnv() + transcriptionTimeoutEnv + "=+600\n",
		transcriptionURLEnv + "=http://127.0.0.1:6971\n",
		transcriptionTokenEnv + "=" + strings.Repeat("a", 43) + "\n",
		strings.Repeat("A", maxAdminEnvBytes+1),
	} {
		path := privateAdminTestEnv(t, body)
		if _, err := transcriptionLaunchConfig(options{transcriptionEnvFile: path}); !errors.Is(err, errTranscriptionConfig) || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("malformed config accepted or private input exposed")
		}
	}
}

func TestTranscriptionEnvRefusesUnsafeFiles(t *testing.T) {
	clearTranscriptionEnv(t)
	for _, entry := range []string{"public-file", "public-parent", "leaf-symlink", "hardlink"} {
		t.Run(entry, func(t *testing.T) {
			path := privateAdminTestEnv(t, testTranscriptionEnv())
			switch entry {
			case "public-file":
				if os.Chmod(path, 0644) != nil {
					t.Fatal("fixture permissions")
				}
			case "public-parent":
				if os.Chmod(filepath.Dir(path), 0755) != nil {
					t.Fatal("fixture permissions")
				}
			case "leaf-symlink":
				link := filepath.Join(filepath.Dir(path), "link.env")
				if os.Symlink(path, link) != nil {
					t.Fatal("fixture symlink")
				}
				path = link
			case "hardlink":
				if os.Link(path, filepath.Join(filepath.Dir(path), "linked.env")) != nil {
					t.Fatal("fixture hardlink")
				}
			}
			if _, err := transcriptionLaunchConfig(options{transcriptionEnvFile: path}); !errors.Is(err, errTranscriptionConfig) {
				t.Fatal("unsafe private file accepted")
			}
		})
	}
}

func TestTranscriptionMissingTokenFailsBeforeProfileOrListener(t *testing.T) {
	clearTranscriptionEnv(t)
	t.Setenv(transcriptionURLEnv, "http://127.0.0.1:6971")
	root := filepath.Join(t.TempDir(), "private-not-created")
	var output, diagnostic bytes.Buffer
	code := run(context.Background(), []string{"demo", "--listen", "127.0.0.1:0", "--data-dir", root, "--no-open"}, &output, &diagnostic)
	if code != 1 || !strings.Contains(diagnostic.String(), "transcription_token_missing") {
		t.Fatal("incomplete voice config did not fail with fixed code")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid configuration opened a profile")
	}
	if output.Len() != 0 || strings.Contains(diagnostic.String(), root) {
		t.Fatal("invalid configuration exposed private values")
	}
}

func TestTranscriptionEnvFlagRequiresLaunchAndPrivateAbsolutePath(t *testing.T) {
	for _, args := range [][]string{
		{"serve", "--transcription-env-file", ""},
		{"serve", "--transcription-env-file", "relative.env"},
		{"serve", "--transcription-env-file", "/private/../config.env"},
		{"serve", "--open-existing", "--transcription-env-file", "/private/config.env"},
		{"doctor", "--transcription-env-file", "/private/config.env"},
		{"mcp", "stdio", "--config", "/private/client.json", "--transcription-env-file", "/private/config.env"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("invalid voice launch flag accepted")
		}
	}
	if _, err := parseArgs([]string{"serve", "--transcription-env-file", "/private/config.env"}); err != nil {
		t.Fatal("canonical voice env argument refused")
	}
}
