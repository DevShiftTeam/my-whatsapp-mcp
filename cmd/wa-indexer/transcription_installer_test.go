package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exercise the Python installer's actual private env serialization through the
// Go launch loader. No dependencies, weights, service or archive are provisioned.
func TestHebrewInstallerSharedEnvLoadsInArchive(t *testing.T) {
	clearTranscriptionEnv(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is needed for the installer configuration smoke check")
	}
	service, err := filepath.Abs(filepath.Join("..", "..", "services", "transcription"))
	if err != nil {
		t.Fatal("synthetic service path unavailable")
	}
	dataDir := filepath.Join(t.TempDir(), "Voice תמלול (ivrit)")
	if err := os.Mkdir(dataDir, 0700); err != nil {
		t.Fatal("synthetic private directory unavailable")
	}
	const program = `import sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
from setup import configure_environment
configure_environment(Path(sys.argv[2]), language="he", profile_language="he")
`
	command := exec.Command(python, "-B", "-c", program, service, dataDir)
	// The test never prints the generated credential or subprocess diagnostics.
	if output, err := command.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatal("synthetic installer configuration failed or printed unexpected output")
	}
	path := filepath.Join(dataDir, "transcription.env")
	cfg, err := transcriptionLaunchConfig(options{transcriptionEnvFile: path})
	if err != nil || cfg == nil || cfg.Language != "he" || cfg.URL != "http://127.0.0.1:6971" {
		t.Fatal("installer Hebrew configuration refused by the archive launch loader")
	}
	body, err := readPrivateEnvFile(path)
	if err != nil {
		t.Fatal("synthetic shared env unreadable")
	}
	values, err := parseTranscriptionEnv(body)
	if err != nil || values["WA_TRANSCRIPTION_LANGUAGE"] != "he" || cfg.Token == "" || cfg.Token != values["WA_TRANSCRIPTION_TOKEN"] || cfg.Token != values[transcriptionTokenEnv] {
		t.Fatal("installer service and archive language or credentials disagree")
	}
}
