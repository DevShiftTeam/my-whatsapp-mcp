package main

import "testing"

func TestClassifiedHistoryRecoveryRequiresPrivateConfigOnly(t *testing.T) {
	opts, err := parseArgs([]string{"recover-classified-history", "--config", "/synthetic/private/recovery.json"})
	if err != nil || opts.command != "recover_classified_history" || opts.config != "/synthetic/private/recovery.json" {
		t.Fatal("typed offline recovery config unavailable")
	}
	for _, args := range [][]string{{"recover-classified-history"}, {"recover-classified-history", "--config", "relative.json"}, {"recover-classified-history", "--data-dir", "/synthetic"}, {"recover-classified-history", "--config", "/synthetic/config", "--force"}, {"recover-snapshot", "--config", "/synthetic/config"}} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("recovery accepted unbounded or generic bypass flags")
		}
	}
}
