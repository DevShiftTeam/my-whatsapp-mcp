package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"net"
	"os"
	"reflect"
	"testing"
)

func TestUnknownHistoryTrustedLaunchPolicy(t *testing.T) {
	for _, policy := range []string{"strict", "partial"} {
		opts, err := parseArgs([]string{"serve", "--data-dir", "/private/synthetic/profile", "--unknown-history-policy", policy})
		if err != nil {
			t.Fatalf("explicit trusted policy %s rejected: %v", policy, err)
		}
		field := reflect.ValueOf(opts).FieldByName("unknownHistoryPolicy")
		if !field.IsValid() || field.Kind() != reflect.String || field.String() != policy {
			t.Fatal("trusted policy was not retained")
		}
	}
	for _, args := range [][]string{
		{"serve", "--unknown-history-policy", ""},
		{"serve", "--unknown-history-policy", "unknown"},
		{"serve", "--unknown-history-policy", "PARTIAL"},
		{"health", "--unknown-history-policy", "partial"},
		{"recover-classified-history", "--config", "/private/synthetic/config.json", "--unknown-history-policy", "partial"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("invalid or wrong-command policy accepted")
		}
	}
	opts, err := parseArgs([]string{"serve"})
	if err != nil {
		t.Fatal(err)
	}
	field := reflect.ValueOf(opts).FieldByName("unknownHistoryPolicy")
	if !field.IsValid() || field.String() != "strict" {
		t.Fatal("normal launch did not default strict")
	}
}

func TestUnknownHistoryExplicitPolicyRequiresRestartBeforeAttach(t *testing.T) {
	for _, lockedProfile := range []bool{false, true} {
		t.Run(map[bool]string{false: "occupied_listener", true: "locked_profile"}[lockedProfile], func(t *testing.T) {
			root := privateCLIDataRoot(t)
			listen := "127.0.0.1:0"
			if lockedProfile {
				paths, err := appdata.Prepare(root)
				if err != nil {
					t.Fatal(err)
				}
				lease, err := appdata.Acquire(paths)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Close()
			} else {
				l, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
				listen = l.Addr().String()
			}
			opts, err := parseArgs([]string{"serve", "--listen", listen, "--data-dir", root, "--unknown-history-policy", "partial", "--no-open"})
			if err != nil {
				t.Fatal(err)
			}
			var output, diagnostic bytes.Buffer
			err = serve(context.Background(), opts, &output, &diagnostic)
			if !errors.Is(err, errRestartRequired) {
				t.Fatalf("explicit policy silently attached or failed without restart signal: %v", err)
			}
			if !lockedProfile {
				if _, err = os.Stat(root); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("occupied listener policy override touched profile")
				}
			}
			if output.Len() != 0 || diagnostic.Len() != 0 {
				t.Fatal("private attach configuration exposed")
			}
		})
	}
	if _, err := parseArgs([]string{"serve", "--open-existing", "--unknown-history-policy", "partial"}); err == nil {
		t.Fatal("open-existing accepted a policy override")
	}
}
