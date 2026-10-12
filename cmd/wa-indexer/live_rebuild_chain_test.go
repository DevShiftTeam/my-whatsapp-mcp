package main

import "testing"

func TestLiveRebuildChainAncestorDescriptorsAreTrustedAndBounded(t *testing.T) {
	if _, err := parseArgs([]string{"serve", "--data-dir", "/private/destination", "--rebuild-source-lock-fd", "7", "--rebuild-ancestor-lock-fd", "8"}); err != nil {
		t.Fatal("valid ancestor descriptor rejected")
	}
	for _, args := range [][]string{
		{"serve", "--rebuild-ancestor-lock-fd", "8"},
		{"serve", "--data-dir", "/private/destination", "--rebuild-ancestor-lock-fd", "2"},
		{"serve", "--data-dir", "/private/destination", "--rebuild-ancestor-lock-fd", "8", "--rebuild-ancestor-lock-fd", "8"},
		{"serve", "--data-dir", "/private/destination", "--rebuild-source-lock-fd", "8", "--rebuild-ancestor-lock-fd", "8"},
		{"serve", "--data-dir", "/private/destination", "--rebuild-ancestor-lock-fd", "8", "--demo"},
		{"serve", "--data-dir", "/private/destination", "--rebuild-ancestor-lock-fd", "8", "--open-existing"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("invalid ancestor descriptor accepted")
		}
	}
}
