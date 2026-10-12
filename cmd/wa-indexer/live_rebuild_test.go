package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveRebuildRequiresPrivateConfigurationOnly(t *testing.T) {
	opts, err := parseArgs([]string{"rebuild-live", "--config", "/private/rebuild.json"})
	if err != nil || opts.command != "rebuild_live" || opts.config != "/private/rebuild.json" {
		t.Fatal("explicit offline rebuild unavailable")
	}
	for _, args := range [][]string{
		{"rebuild-live"}, {"rebuild-live", "--config", "relative.json"},
		{"rebuild-live", "--config", "/private/rebuild.json", "--data-dir", "/private/profile"},
		{"rebuild-live", "--config", "/private/rebuild.json", "--listen", "127.0.0.1:0"},
		{"rebuild-live", "--config", "/private/rebuild.json", "--originals-budget", "1GiB"},
		{"rebuild-live", "--config", "/private/rebuild.json", "--demo"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("accepted implicit or unrelated rebuild options")
		}
	}
}

func TestLiveRebuildConfigurationFailuresStayPrivate(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "PRIVATE-SENTINEL.json")
	for _, content := range []string{`{"unknown":"PRIVATE-SENTINEL"}`, `{}`, `{} {}`, `not json PRIVATE-SENTINEL`} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), []string{"rebuild-live", "--config", path}, &stdout, &stderr); code != 1 {
			t.Fatal("invalid bootstrap configuration accepted")
		}
		if stdout.Len() != 0 || strings.Contains(stderr.String(), "PRIVATE-SENTINEL") {
			t.Fatal("private rebuild configuration leaked")
		}
	}
}

func TestLiveRebuildInheritedDescriptorIsTrustedLaunchOnly(t *testing.T) {
	opts, err := parseArgs([]string{"serve", "--data-dir", "/private/destination", "--rebuild-source-lock-fd", "7"})
	if err != nil || opts.rebuildSourceLockFD == nil || *opts.rebuildSourceLockFD != 7 {
		t.Fatal("inherited source ownership descriptor lost")
	}
	for _, args := range [][]string{
		{"serve", "--rebuild-source-lock-fd", "2"},
		{"serve", "--rebuild-source-lock-fd", "7"},
		{"serve", "--data-dir", "/private/destination", "--rebuild-source-lock-fd", "7", "--demo"},
		{"serve", "--data-dir", "/private/destination", "--rebuild-source-lock-fd", "7", "--open-existing"},
		{"doctor", "--data-dir", "/private/destination", "--rebuild-source-lock-fd", "7"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("unqualified descriptor launch accepted")
		}
	}
}
