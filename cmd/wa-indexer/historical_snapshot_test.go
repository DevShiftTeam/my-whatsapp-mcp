package main

import "testing"

func TestHistoricalSnapshotLaunchRequiresExplicitCompatibleOptions(t *testing.T) {
	base := []string{"serve", "--data-dir", "/synthetic/profile", "--historical-snapshot-at", "2026-01-02T03:04:05Z"}
	if _, err := parseArgs(base); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{{"--demo"}, {"--open-existing"}, {"--originals-budget", "64MiB"}, {"--data-dir", "relative"}, {"--historical-snapshot-at", "not-a-time"}} {
		args := append(append([]string{}, base...), extra...)
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("incompatible options accepted: %v", extra)
		}
	}
}
