package main

import "testing"

func TestHistoricalViewTrustedLaunchArguments(t *testing.T) {
	base := []string{"serve", "--data-dir", "/private/synthetic/live", "--historical-view-dir", "/private/synthetic/captured", "--historical-view-id", "0123456789abcdef", "--historical-view-at", "2026-01-02T03:04:05Z"}
	if _, err := parseArgs(base); err != nil {
		t.Fatalf("trusted historical view options: %v", err)
	}
	for _, extra := range [][]string{{"--demo"}, {"--open-existing"}, {"--historical-snapshot-at", "2026-01-02T03:04:05Z"}} {
		if _, err := parseArgs(append(append([]string(nil), base...), extra...)); err == nil {
			t.Fatal("conflicting launch accepted")
		}
	}
	for _, args := range [][]string{
		{"serve", "--historical-view-dir", "/private/synthetic/captured"},
		{"serve", "--historical-view-id", "0123456789abcdef"},
		{"serve", "--historical-view-at", "2026-01-02T03:04:05Z"},
		{"serve", "--historical-view-dir", "relative", "--historical-view-id", "0123456789abcdef", "--historical-view-at", "2026-01-02T03:04:05Z"},
		{"serve", "--historical-view-dir", "/private/synthetic/captured", "--historical-view-id", "not-a-view", "--historical-view-at", "2026-01-02T03:04:05Z"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("incomplete or invalid trusted source accepted")
		}
	}
}
