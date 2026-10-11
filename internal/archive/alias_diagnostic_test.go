//go:build sqlite_fts5

package archive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func captureAliasDiagnostics(t *testing.T, run func()) []map[string]string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "alias-diagnostic-")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	previous := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = previous }()
	run()
	os.Stderr = previous
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	encoded, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(encoded) > 4096 {
		t.Fatal("alias diagnostic stream exceeded its finite capture bound")
	}
	if strings.Contains(string(encoded), "SYNTHETIC_PRIVATE_ALIAS") {
		t.Fatal("alias diagnostic exposed a source identifier")
	}
	var records []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(encoded)), "\n") {
		if line == "" {
			continue
		}
		var record map[string]string
		if err = json.Unmarshal([]byte(line), &record); err != nil || len(record) != 4 || record["event"] != "archive_failure" {
			t.Fatal("alias diagnostic lost its closed four-field schema")
		}
		records = append(records, record)
	}
	return records
}

func requireAliasDiagnostic(t *testing.T, records []map[string]string, stage, code, class string) {
	t.Helper()
	for _, record := range records {
		if record["stage"] == stage && record["code"] == code && record["class"] == class {
			return
		}
	}
	t.Fatalf("missing fixed alias diagnostic for %s/%s/%s", stage, code, class)
}

func TestAliasDiagnosticDistinguishesLookupDeadline(t *testing.T) {
	s, _, a, _ := aliasNoopFixture(t)
	s.readDB.SetMaxOpenConns(1)
	conn, err := s.readDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s.options.ReadTimeout = 20 * time.Millisecond
	ctx := context.Background()
	records := captureAliasDiagnostics(t, func() {
		if err = s.ApplyAlias(ctx, a); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("exhausted alias read pool did not preserve its deadline error")
		}
	})
	if ctx.Err() != nil {
		t.Fatal("lookup diagnostic expired the source parent context")
	}
	if _, err = s.readSafety(a.Account); err != nil {
		t.Fatal("diagnostic changed read-only alias safety")
	}
	if len(records) != 1 {
		t.Fatal("lookup failure did not emit exactly one boundary diagnostic")
	}
	requireAliasDiagnostic(t, records, "alias_lookup", "alias_operation_failed", "deadline")
}

func TestAliasDiagnosticDistinguishesWriterDeadlineAfterAdmission(t *testing.T) {
	s, _, a, _ := aliasNoopFixture(t)
	a.PhoneJID, a.LID = "SYNTHETIC_PRIVATE_ALIAS@s.whatsapp.net", "SYNTHETIC_PRIVATE_ALIAS@lid"
	s.options.AliasDeadline = 30 * time.Millisecond
	barrier := aliasNoopBarrierCommand{entered: make(chan struct{}), release: make(chan struct{})}
	blocked := make(chan error, 1)
	var once sync.Once
	release := func() { once.Do(func() { close(barrier.release) }) }
	defer release()
	go func() { _, e := s.submitExtension(context.Background(), barrier, 0); blocked <- e }()
	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not enter the alias diagnostic barrier")
	}
	ctx := context.Background()
	records := captureAliasDiagnostics(t, func() {
		if err := s.ApplyAlias(ctx, a); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("accepted alias command did not preserve its owned writer deadline")
		}
		if _, err := s.readSafety(a.Account); !errors.Is(err, ErrReconciliationRequired) {
			t.Fatal("writer deadline diagnostic preceded authoritative denial")
		}
		select {
		case queued := <-s.controls:
			command, ok := queued.payload.(aliasCommand)
			if !ok || command.alias != a {
				t.Fatal("deadline did not retain the exact accepted alias command")
			}
			s.controls <- queued
		default:
			t.Fatal("alias deadline occurred before control admission")
		}
		release()
		if err := <-blocked; err != nil {
			t.Fatal(err)
		}
		if err := s.Drain(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	if ctx.Err() != nil {
		t.Fatal("writer diagnostic expired the source parent context")
	}
	requireAliasDiagnostic(t, records, "alias_write", "alias_operation_failed", "deadline")
	requireAliasDiagnostic(t, records, "alias_writer_result", "alias_parent_deadline", "deadline")
}

func TestAliasDiagnosticPreservesAuthoritativeConflictError(t *testing.T) {
	s, _, a, _ := aliasNoopFixture(t)
	a.PhoneJID = "SYNTHETIC_PRIVATE_ALIAS@s.whatsapp.net"
	records := captureAliasDiagnostics(t, func() {
		if err := s.ApplyAlias(context.Background(), a); err != ErrIdentityAliasConflict {
			t.Fatal("alias diagnostic wrapped or replaced the authoritative conflict error")
		}
		if _, err := s.readSafety(a.Account); !errors.Is(err, ErrReconciliationRequired) {
			t.Fatal("alias conflict diagnostic preceded authoritative denial")
		}
	})
	requireAliasDiagnostic(t, records, "alias_write", "alias_operation_failed", "identity_alias_conflict")
	requireAliasDiagnostic(t, records, "alias_writer_result", "alias_operation_failed", "identity_alias_conflict")
}

type opaqueAliasDiagnosticError struct{}

func (opaqueAliasDiagnosticError) Error() string {
	panic("alias diagnostic formatted a private error")
}

func TestAliasFailureDiagnosticUsesClosedValuesAndParentState(t *testing.T) {
	for _, test := range []struct {
		name, stage, code, class string
		ctx                      context.Context
		err                      error
	}{
		{"nested_deadline", "alias_lookup", "alias_operation_failed", "deadline", context.Background(), context.DeadlineExceeded},
		{"private_error", "SYNTHETIC_PRIVATE_ALIAS", "alias_operation_failed", "unknown", context.Background(), opaqueAliasDiagnosticError{}},
		{"nil_error", "alias_write", "alias_operation_failed", "none", context.Background(), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded := aliasFailureDiagnostic(test.ctx, test.stage, test.err)
			var record map[string]string
			if err := json.Unmarshal(encoded, &record); err != nil || len(record) != 4 || record["event"] != "archive_failure" || record["code"] != test.code || record["class"] != test.class {
				t.Fatal("alias failure diagnostic lost its closed schema or error classification")
			}
			stage := test.stage
			if test.name == "private_error" {
				stage = "unknown"
			}
			if record["stage"] != stage || strings.Contains(string(encoded), "SYNTHETIC_PRIVATE_ALIAS") {
				t.Fatal("alias diagnostic admitted an untrusted stage")
			}
		})
	}
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		code, class := "alias_parent_canceled", "canceled"
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			code, class = "alias_parent_deadline", "deadline"
		}
		cancel()
		var record map[string]string
		if err := json.Unmarshal(aliasFailureDiagnostic(ctx, "alias_write", ctx.Err()), &record); err != nil || record["code"] != code || record["class"] != class {
			t.Fatal("alias diagnostic lost the expired operation-parent state")
		}
	}
	if records := captureAliasDiagnostics(t, func() {
		recordAliasFailure(context.Background(), "alias_write", nil)
	}); len(records) != 0 {
		t.Fatal("successful alias operation emitted a failure")
	}
}
