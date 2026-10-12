package main

import (
	"strings"
	"testing"
)

func TestAcceptedPartialHistoryRecoveryRequiresPrivateConfigOnly(t *testing.T) {
	const config = "/synthetic/private/partial-history.json"
	opts, err := parseArgs([]string{"recover-partial-history", "--config", config})
	if err != nil || opts.command != "recover_partial_history" || opts.config != config {
		t.Fatal("explicit offline partial-history recovery command unavailable", err)
	}
}

func TestAcceptedPartialHistoryRecoveryRejectsBypassesAndMalformedPaths(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{"missing_config", nil},
		{"empty_config", []string{"--config", ""}},
		{"relative_config", []string{"--config", "private/partial-history.json"}},
		{"unclean_config", []string{"--config", "/synthetic/private/../partial-history.json"}},
		{"nul_config", []string{"--config", "/synthetic/private\x00/partial-history.json"}},
		{"force", []string{"--config", "/synthetic/private/partial-history.json", "--force"}},
		{"runtime_policy", []string{"--config", "/synthetic/private/partial-history.json", "--unknown-history-policy", "partial"}},
		{"generic_policy", []string{"--config", "/synthetic/private/partial-history.json", "--policy", "partial"}},
		{"data_directory", []string{"--config", "/synthetic/private/partial-history.json", "--data-dir", "/synthetic/private/profile"}},
		{"positional_path", []string{"/synthetic/private/partial-history.json"}},
		{"extra_argument", []string{"--config", "/synthetic/private/partial-history.json", "extra"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"recover-partial-history"}, test.args...)
			if _, err := parseArgs(args); err == nil {
				t.Fatal("partial-history recovery accepted invalid arguments")
			} else if strings.Contains(err.Error(), "synthetic") {
				t.Fatal("argument error disclosed a private supplied path")
			}
		})
	}
}
