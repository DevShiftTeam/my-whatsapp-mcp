package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"reflect"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
)

func TestRebuildHistoryPolicyTrustedLaunchSelection(t *testing.T) {
	for _, selection := range []struct {
		value    string
		explicit bool
	}{{"selected-original", false}, {"selected-original", true}, {"delivered-history", true}} {
		args := []string{"serve"}
		if selection.explicit {
			args = append(args, "--rebuild-history-policy", selection.value)
		}
		opts, err := parseArgs(args)
		if err != nil {
			t.Fatal("trusted history selection rejected", err)
		}
		value := reflect.ValueOf(opts)
		policy := value.FieldByName("rebuildHistoryPolicy")
		explicit := value.FieldByName("rebuildHistoryPolicyExplicit")
		if !policy.IsValid() || policy.Kind() != reflect.String || policy.String() != selection.value || !explicit.IsValid() || explicit.Bool() != selection.explicit {
			t.Fatal("history selection lost its value or explicit-launch distinction")
		}
	}
	for _, args := range [][]string{
		{"serve", "--rebuild-history-policy", ""},
		{"serve", "--rebuild-history-policy", "delivered"},
		{"serve", "--rebuild-history-policy", "DELIVERED-HISTORY"},
		{"serve", "--rebuild-history-policy", " delivered-history"},
		{"serve", "--demo", "--rebuild-history-policy", "delivered-history"},
		{"serve", "--open-existing", "--rebuild-history-policy", "selected-original"},
		{"serve", "--data-dir", "/synthetic/profile", "--historical-snapshot-at", "2026-01-01T00:00:00Z", "--rebuild-history-policy", "delivered-history"},
		{"health", "--rebuild-history-policy", "delivered-history"},
		{"rebuild-live", "--config", "/synthetic/config.json", "--rebuild-history-policy", "delivered-history"},
		{"recover-partial-history", "--config", "/synthetic/config.json", "--rebuild-history-policy", "delivered-history"},
		{"mcp", "stdio", "--config", "/synthetic/config.json", "--rebuild-history-policy", "delivered-history"},
	} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("invalid or non-startup history selection accepted")
		}
	}
}

func TestRebuildHistoryPolicyExplicitSelectionRequiresRestartBeforeAttach(t *testing.T) {
	for _, policy := range []string{"selected-original", "delivered-history"} {
		for _, locked := range []bool{false, true} {
			name := "occupied_listener"
			if locked {
				name = "locked_profile"
			}
			t.Run(policy+"/"+name, func(t *testing.T) {
				root := privateCLIDataRoot(t)
				listen := "127.0.0.1:0"
				if locked {
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
					listener, err := net.Listen("tcp4", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Close()
					listen = listener.Addr().String()
				}
				opts, err := parseArgs([]string{"serve", "--listen", listen, "--data-dir", root, "--rebuild-history-policy", policy, "--no-open"})
				if err != nil {
					t.Fatal(err)
				}
				var output, diagnostic bytes.Buffer
				if err := serve(context.Background(), opts, &output, &diagnostic); !errors.Is(err, errRestartRequired) {
					t.Fatal("running owner accepted a launch-only strategy change", err)
				}
				if !locked {
					if _, err := os.Stat(root); !os.IsNotExist(err) {
						t.Fatal("occupied-listener attach touched the profile", err)
					}
				}
				if output.Len() != 0 || diagnostic.Len() != 0 {
					t.Fatal("attach rejection disclosed private configuration")
				}
			})
		}
	}
}
