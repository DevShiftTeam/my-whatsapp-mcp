package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnerLaunchConfig(t *testing.T) {
	type launchCase struct {
		name           string
		args           []string
		listen, origin string
		ok             bool
	}
	cases := []launchCase{
		{"native", []string{"serve"}, "127.0.0.1:64165", "", true},
		{"native wildcard", []string{"serve", "--listen", "0.0.0.0:8123"}, "", "", false},
		{"container", []string{"serve", "--container"}, "0.0.0.0:8123", "http://127.0.0.1:8123", true},
		{"custom host port", []string{"serve", "--container", "--browser-origin", "http://127.0.0.1:9000"}, "0.0.0.0:8123", "http://127.0.0.1:9000", true},
		{"explicit loopback", []string{"serve", "--container", "--listen", "127.0.0.1:8124"}, "127.0.0.1:8124", "http://127.0.0.1:8123", true},
		{"IPv6", []string{"serve", "--container", "--listen", "[::]:8123", "--browser-origin", "http://[::1]:8123"}, "[::]:8123", "http://[::1]:8123", true},
	}
	for _, value := range []string{"http://example.com", "http://localhost:8123", "http://192.0.2.1:8123", "https://127.0.0.1:8123", "http://127.0.0.1:0", "http://[::ffff:127.0.0.1]:8123", "http://127.0.0.1:08123", "http://127.0.0.1:8123/", "http://127.0.0.1:8123?", "http://127.0.0.1:8123#", "http://user@127.0.0.1:8123", "http://127.0.0.1:8123\n"} {
		cases = append(cases, launchCase{value, []string{"serve", "--container", "--browser-origin", value}, "", "", false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseArgs(tc.args)
			if err == nil {
				cfg, configErr := ownerLaunchConfig(opts)
				err = configErr
				if err == nil && (opts.listen != tc.listen || cfg.Origin != tc.origin || opts.noOpen != opts.container) {
					t.Fatalf("unexpected composition: %+v %+v", opts, cfg)
				}
				if err == nil && (cfg.Listen != opts.listen || cfg.Container != opts.container) {
					t.Fatal("internal listener not retained separately")
				}
			}
			if (err == nil) != tc.ok {
				t.Fatalf("accepted=%v want=%v err=%v", err == nil, tc.ok, err)
			}
		})
	}
}

func TestContainerOverridesRejectAmbiguityAndNonServerCommands(t *testing.T) {
	cases := [][]string{
		{"serve", "--browser-origin", "http://127.0.0.1:9000"},
		{"serve", "--docker-container-name", "wa-indexer"},
		{"serve", "--container=false", "--browser-origin", "http://127.0.0.1:9000"},
		{"serve", "--container", "--container=false"},
		{"serve", "--container", "--browser-origin", "http://127.0.0.1:8123", "--browser-origin=http://127.0.0.1:9000"},
		{"serve", "--container", "--listen=0.0.0.0:8123", "--listen=127.0.0.1:8123"},
		{"serve", "--container", "--docker-container-name=archive", "--docker-container-name=archive"},
		{"doctor", "--container"}, {"models", "verify", "profile", "--container"}, {"mcp", "stdio", "--config", "/private/config", "--container"},
	}
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "localhost:8123", ":8123", "192.0.2.1:8123", "[::ffff:127.0.0.1]:8123", "[::1%lo0]:8123", "0.0.0.0:08123", "0.0.0.0:+8123"} {
		cases = append(cases, []string{"serve", "--container", "--listen", address})
	}
	for _, name := range []string{"", ".bad", "-bad", "../private-name-SENTINEL", "name with space", strings.Repeat("a", 129)} {
		cases = append(cases, []string{"serve", "--container", "--docker-container-name", name})
	}
	for _, args := range cases {
		_, err := parseArgs(args)
		if err == nil {
			t.Fatalf("ambiguous/unsafe options accepted: %v", args)
		}
		if err.Error() != errArguments.Error() {
			t.Fatal("flag values leaked into rejection")
		}
	}
	for _, name := range []string{"wa-indexer", "project_archive-1", "Archive.1"} {
		opts, err := parseArgs([]string{"serve", "--container", "--docker-container-name", name, "--no-open=false"})
		if err != nil || opts.dockerContainerName != name || !opts.noOpen {
			t.Fatalf("validated container metadata/default no-open lost: %v", err)
		}
	}
}

func TestBindOwnerExplicitContainerAndNativePolicy(t *testing.T) {
	for _, container := range []bool{false, true} {
		probe, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		address := probe.Addr().String()
		probe.Close()
		listener, err := bindOwner(context.Background(), ownerListenerConfig{Listen: address, Container: container})
		if listener != nil {
			defer listener.Close()
		}
		if container && (err != nil || listener.Addr().String() != address) {
			t.Fatalf("explicit wildcard bind failed: %v", err)
		}
		if !container && (err == nil || listener != nil) {
			t.Fatal("native wildcard bind accepted")
		}
	}
}

func TestContainerOccupiedListenerOpensNoApplicationState(t *testing.T) {
	occupied, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	root := filepath.Join(t.TempDir(), "private-data-SENTINEL")
	var stdout, stderr bytes.Buffer
	if run(context.Background(), []string{"serve", "--container", "--listen", occupied.Addr().String(), "--data-dir", root, "--demo"}, &stdout, &stderr) != 1 {
		t.Fatal("occupied container listener did not fail")
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed bind opened application storage")
	}
	if stdout.Len() != 0 || strings.Contains(stderr.String(), root) || strings.Contains(stderr.String(), occupied.Addr().String()) {
		t.Fatal("failed bind leaked private details")
	}
}
