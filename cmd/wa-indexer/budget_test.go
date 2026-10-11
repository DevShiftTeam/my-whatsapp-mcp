package main

import "testing"

func TestOriginalBudgetArgumentsUseCheckedIntegerUnits(t *testing.T) {
	for input, want := range map[string]int64{"64MiB": 64 << 20, "2GiB": 2 << 30, "1024GiB": 1 << 40, "1048576MiB": 1 << 40} {
		value, err := parseOriginalBudget(input)
		if err != nil || value != want {
			t.Fatalf("valid budget %q: %v", input, err)
		}
	}
	for _, input := range []string{"", "64", "64MB", "63MiB", "1025GiB", "1.5GiB", "-64MiB", "+64MiB", "0x40MiB", "18446744073709551615GiB", " 64MiB"} {
		if _, err := parseOriginalBudget(input); err == nil {
			t.Fatalf("unsafe budget accepted: %q", input)
		}
	}
	for _, command := range [][]string{{"serve", "--originals-budget", "3GiB"}, {"doctor", "--originals-budget", "64MiB"}, {"demo", "--originals-budget", "2GiB"}} {
		opts, err := parseArgs(command)
		if err != nil || opts.originalBudget == nil {
			t.Fatal("explicit action lost")
		}
	}
	opts, err := parseArgs([]string{"serve"})
	if err != nil || opts.originalBudget != nil {
		t.Fatal("omission overwrote persisted authority")
	}
	if _, err := parseArgs([]string{"mcp", "stdio", "--config", "/private/file.json", "--originals-budget", "2GiB"}); err == nil {
		t.Fatal("stdio accepted archive action")
	}
}
