package main

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestDemoAcceptanceFlagRequiresExplicitDemo(t *testing.T) {
	for _, args := range [][]string{{"demo", "--demo-acceptance-fixtures"}, {"serve", "--demo", "--demo-acceptance-fixtures"}} {
		opts, err := parseArgs(args)
		if err != nil || !opts.demo || !opts.demoAcceptance {
			t.Fatalf("explicit demo refused: %v %v", args, err)
		}
	}
	for _, args := range [][]string{{"serve", "--demo-acceptance-fixtures"}, {"doctor", "--demo-acceptance-fixtures"}} {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("real-mode fixture accepted: %v", args)
		}
	}
}

// These tests catch accidentally broadening the listener or printing supplied
// flags when an argument fails, before any application data can be opened.
func TestParseArgsDefaults(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}} {
		got, err := parseArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		if got.listen != "127.0.0.1:64165" || got.dataDir != "" || got.demo || got.noOpen || got.help {
			t.Fatalf("unexpected default options: %+v", got)
		}
	}
}

func TestParseArgsExplicitEphemeralListener(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "[::1]:0"} {
		got, err := parseArgs([]string{"serve", "--listen", address})
		if err != nil || got.listen != address {
			t.Fatal("explicit ephemeral listener was not retained")
		}
	}
}

func TestParseArgsPrivateOptions(t *testing.T) {
	got, err := parseArgs([]string{"serve", "--listen", "[::1]:0", "--data-dir", "/private/owner archive", "--demo", "--no-open"})
	if err != nil {
		t.Fatal(err)
	}
	if got.listen != "[::1]:0" || got.dataDir != "/private/owner archive" || !got.demo || !got.noOpen {
		t.Fatal("private launch options were not retained")
	}
}

func TestParseArgsRejectsWithoutLeakingValues(t *testing.T) {
	const secret = "private-bootstrap-SENTINEL"
	cases := [][]string{
		{secret}, {"serve", secret}, {"serve", "--" + secret},
		{"serve", "--no-open=" + secret}, {"serve", "--demo=" + secret},
		{"serve", "--listen"}, {"serve", "--data-dir"}, {"serve", "--data-dir="},
		{"serve", "--listen", "127.0.0.1:0", secret},
	}
	for _, args := range cases {
		if _, err := parseArgs(args); err == nil {
			t.Error("invalid command was accepted")
		} else if strings.Contains(err.Error(), secret) {
			t.Fatal("error exposed a supplied argument")
		}
	}
}

func TestNativeSubcommandsAndDoubleClickDefaults(t *testing.T) {
	for _, tc := range []struct {
		args                     []string
		command, profile, config string
		demo                     bool
	}{
		{nil, "serve", "", "", false},
		{[]string{"demo", "--no-open"}, "serve", "", "", true},
		{[]string{"doctor", "--data-dir", "/private/local archive"}, "doctor", "", "", false},
		{[]string{"models", "install", "encoder-profile", "--data-dir", "/private/local archive"}, "models_install", "encoder-profile", "", false},
		{[]string{"models", "verify", "encoder-profile"}, "models_verify", "encoder-profile", "", false},
		{[]string{"mcp", "stdio", "--config", "/private/local client.json"}, "mcp_stdio", "", "/private/local client.json", false},
	} {
		got, err := parseArgs(tc.args)
		if err != nil {
			t.Fatalf("subcommand unavailable: %v", err)
		}
		if got.command != tc.command || got.profile != tc.profile || got.config != tc.config || got.demo != tc.demo {
			t.Fatalf("incorrect subcommand routing: %+v", got)
		}
	}
}

func TestStdioRequiresOnlyAbsolutePrivateConfigArgument(t *testing.T) {
	for _, args := range [][]string{
		{"mcp"}, {"mcp", "stdio"}, {"mcp", "stdio", "--config", "relative.json"},
		{"mcp", "stdio", "--config", "/private/client.json", "--data-dir", "/private/app"},
		{"mcp", "stdio", "--config", "/private/client.json", "extra"},
		{"models", "install"}, {"models", "remove", "profile"},
		{"doctor", "--demo"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatalf("invalid isolated command accepted: %v", args)
		}
	}
}

func TestParseArgsHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"serve", "--help"}} {
		got, err := parseArgs(args)
		if err != nil || !got.help {
			t.Fatal("help was not accepted")
		}
	}
}

func TestBindLoopbackRejectsPublicWildcardAndNames(t *testing.T) {
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "localhost:0", ":0", "192.0.2.1:0", "[::ffff:127.0.0.1]:0", "127.0.0.1:+1", "127.0.0.1:65536", "127.0.0.1:-1", "127.0.0.1", "[::1%lo0]:0"} {
		listener, err := bindLoopback(context.Background(), address)
		if listener != nil {
			listener.Close()
			t.Fatal("unsafe listener was opened")
		}
		if err == nil || strings.Contains(err.Error(), address) {
			t.Fatal("listener rejection must use a fixed private error")
		}
	}
}

func TestBindLoopbackUsesEphemeralPrivatePort(t *testing.T) {
	listener, err := bindLoopback(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.IsLoopback() || address.Port == 0 {
		t.Fatal("listener was not an assigned loopback port")
	}
}

func TestBindLoopbackHidesOccupiedAddress(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	address := occupied.Addr().String()
	listener, err := bindLoopback(context.Background(), address)
	if listener != nil {
		listener.Close()
		t.Fatal("occupied listener was reused")
	}
	if err == nil || strings.Contains(err.Error(), address) {
		t.Fatal("bind failure exposed its supplied address")
	}
}

func TestBareLaunchFlagsUseServeDefault(t *testing.T) {
	opts, err := parseArgs([]string{"--no-open"})
	if err != nil || opts.command != "serve" || !opts.noOpen {
		t.Fatalf("default flag launch failed: %v", err)
	}
}
