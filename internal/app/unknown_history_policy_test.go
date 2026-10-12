package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
)

// Reflection keeps the baseline test compilable before the trusted app field
// exists, so its initial failure identifies missing policy propagation.
func unknownHistoryPolicyConfig(t *testing.T, root string, value uint64) Config {
	t.Helper()
	cfg := Config{DataDir: root}
	field := reflect.ValueOf(&cfg).Elem().FieldByName("UnknownHistoryPolicy")
	if !field.IsValid() || !field.CanSet() || field.Kind() != reflect.Uint8 {
		t.Fatal("app Config lacks the trusted UnknownHistoryPolicy enum")
	}
	field.SetUint(value)
	return cfg
}

func unknownHistoryFactoryPolicy(t *testing.T, cfg ingestion.Config) uint64 {
	t.Helper()
	field := reflect.ValueOf(cfg).FieldByName("UnknownHistoryPolicy")
	if !field.IsValid() || field.Kind() != reflect.Uint8 {
		t.Fatal("ingestion Config lacks the trusted UnknownHistoryPolicy enum")
	}
	return field.Uint()
}

func TestUnknownHistoryPolicyPropagatesThroughRuntimeRecreationAndRestart(t *testing.T) {
	for _, test := range []struct {
		name  string
		value uint64
	}{{"strict_zero", 0}, {"explicit_partial", 1}} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			cfg := unknownHistoryPolicyConfig(t, privateTestRoot(t), test.value)
			var received []uint64
			factory := func(c ingestion.Config, _ archive.ConnectorStore, _ ingestion.Callbacks) (connector, error) {
				received = append(received, unknownHistoryFactoryPolicy(t, c))
				return &connectionRecoveryFixture{}, nil
			}
			a, err := newApplication(ctx, cfg, factory)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if a != nil {
					a.Close(ctx)
				}
			})
			if len(received) != 1 || received[0] != test.value {
				t.Fatal("startup discarded the trusted policy", received)
			}
			// Exercise the same runtime factory path used when a stopped
			// application recreates its connector; this fixture owns no SDK.
			recreated, err := a.makeRuntime()
			if err != nil {
				t.Fatal(err)
			}
			if err := recreated.Close(); err != nil {
				t.Fatal(err)
			}
			if len(received) != 2 || received[1] != test.value {
				t.Fatal("runtime recreation discarded the trusted policy", received)
			}
			if err := a.Close(ctx); err != nil {
				t.Fatal(err)
			}
			a, err = newApplication(ctx, cfg, factory)
			if err != nil {
				t.Fatal(err)
			}
			if len(received) != 3 || received[2] != test.value {
				t.Fatal("same-config restart discarded the policy", received)
			}
			if err := a.Close(ctx); err != nil {
				t.Fatal(err)
			}
			// Partial mode is a trusted launch choice, not authority persisted
			// by settings or inherited from the profile on a later launch.
			a, err = newApplication(ctx, Config{DataDir: cfg.DataDir}, factory)
			if err != nil {
				t.Fatal(err)
			}
			if len(received) != 4 || received[3] != 0 {
				t.Fatal("default launch inherited partial mode from profile state", received)
			}
		})
	}
}

func TestUnknownHistoryPolicyRejectsInvalidBeforeProfileOrSessionMutation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "absent_profile"
		if existing {
			name = "existing_session"
		}
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(privateTestRoot(t), "profile")
			before := map[string][]byte{}
			if existing {
				paths, err := appdata.Prepare(root)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(paths.SessionDir, "session.sqlite"), []byte("synthetic session must not be opened"), 0600); err != nil {
					t.Fatal(err)
				}
				before = classifiedFollowupFiles(t, root)
			}
			cfg := unknownHistoryPolicyConfig(t, root, 255)
			calls := 0
			factory := func(ingestion.Config, archive.ConnectorStore, ingestion.Callbacks) (connector, error) {
				calls++
				return &connectionRecoveryFixture{}, nil
			}
			a, err := newApplication(context.Background(), cfg, factory)
			if a != nil {
				a.Close(context.Background())
			}
			if err == nil || calls != 0 {
				t.Fatal("invalid policy reached runtime construction", err, calls)
			}
			if existing {
				if !reflect.DeepEqual(before, classifiedFollowupFiles(t, root)) {
					t.Fatal("invalid policy changed an existing profile or session")
				}
			} else if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Fatal("invalid policy created a profile", err)
			}
		})
	}
}
