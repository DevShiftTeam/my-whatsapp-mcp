package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestOriginalCacheRecoveryCommandRequiresTwoAbsoluteProfiles(t *testing.T) {
	current := filepath.Join(t.TempDir(), "current")
	backup := filepath.Join(t.TempDir(), "backup")
	opts, err := parseArgs([]string{"recover-original-cache", "--data-dir", current, "--backup-dir", backup})
	if err != nil || opts.command != "recover_original_cache" || opts.dataDir != current || opts.backupDir != backup {
		t.Fatal("offline cache recovery arguments were not accepted")
	}
	for _, args := range [][]string{
		{"recover-original-cache"},
		{"recover-original-cache", "--data-dir", current},
		{"recover-original-cache", "--data-dir", current, "--backup-dir", current},
		{"recover-original-cache", "--data-dir", "relative", "--backup-dir", backup},
		{"recover-original-cache", "--data-dir", current, "--backup-dir", "relative"},
		{"recover-original-cache", "--data-dir", current, "--backup-dir", "/private/../other"},
		{"recover-original-cache", "--data-dir", current, "--backup-dir", backup, "--listen", "127.0.0.1:64165"},
		{"recover-original-cache", "--data-dir", current, "--backup-dir", backup, "--demo"},
		{"recover-original-cache", "--data-dir", current, "--backup-dir", backup, "--originals-budget", "1GiB"},
		{"recover-original-cache", "--data-dir", current, "--backup-dir", backup, "--transcription-env-file", "/private/voice.env"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("cache recovery accepted incomplete or online launch arguments")
		}
	}
}

func TestOriginalCacheRecoveryCommandDoesNotExposeProfilePaths(t *testing.T) {
	current := filepath.Join(t.TempDir(), "SYNTHETIC-CURRENT-PRIVATE-PATH")
	backup := filepath.Join(t.TempDir(), "SYNTHETIC-BACKUP-PRIVATE-PATH")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"recover-original-cache", "--data-dir", current, "--backup-dir", backup}, &stdout, &stderr); code != 1 {
		t.Fatal("cache recovery did not report the refused missing profiles")
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), current) || strings.Contains(stderr.String(), backup) || !strings.Contains(stderr.String(), "recovery") {
		t.Fatal("cache recovery exposed private paths or emitted an unsafe receipt")
	}
}
