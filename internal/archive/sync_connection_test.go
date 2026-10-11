//go:build sqlite_fts5

package archive

import (
	"context"
	"reflect"
	"testing"
)

func TestSyncStatusOmittedConnectionPreservesStateAndGapAcrossRestart(t *testing.T) {
	ctx := context.Background()
	for _, connection := range []ConnectionState{"connected", "pairing", "paused"} {
		t.Run(string(connection), func(t *testing.T) {
			s := testArchive(t)
			p := testPolicy(t, s)
			apply(t, s, event("sync-connection", "readable archived message", 1, EventOriginal), p.Generation)
			scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
			if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", Connection: connection}); err != nil {
				t.Fatal(err)
			}
			// Independent history diagnostics omit socket state. They must not
			// turn a known connection into the reader's disconnected fallback.
			if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", GapCode: "app_state_sync_error", CoverageUnknown: true}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", Received: 2, Applied: 1, CoverageUnknown: true}); err != nil {
				t.Fatal(err)
			}
			check := func(store *Store) {
				t.Helper()
				status, err := store.Status(ctx, scope)
				if err != nil {
					t.Fatal(err)
				}
				if status.Connection != connection || status.Paused != (connection == "paused") {
					t.Fatalf("gap-only update changed connection: got %q (paused %t), want %q", status.Connection, status.Paused, connection)
				}
				if status.ReconciliationRequired || status.Messages != 1 || status.Coverage != "partial" || !reflect.DeepEqual(status.Codes, []string{"app_state_sync_error"}) {
					t.Fatalf("connection preservation changed durable coverage or readable archive: %+v", status)
				}
			}
			check(s)
			dir := s.privateDir
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			check(reopened)
		})
	}
}

func TestSyncStatusExplicitDisconnectPersistsAndRetainsGap(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
	if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", Connection: "connected", GapCode: "undecryptable_message", CoverageUnknown: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", Connection: "disconnected"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", CoverageUnknown: true}); err != nil {
		t.Fatal(err)
	}
	check := func(store *Store) {
		t.Helper()
		status, err := store.Status(ctx, scope)
		if err != nil || status.Connection != "disconnected" || status.Paused || status.Coverage != "partial" || !reflect.DeepEqual(status.Codes, []string{"undecryptable_message"}) {
			t.Fatalf("explicit disconnect or retained gap changed: %+v, %v", status, err)
		}
	}
	check(s)
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
}

func TestSyncStatusInitialOmittedConnectionDefaultsDisconnected(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
	if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", GapCode: "undecryptable_message", CoverageUnknown: true}); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx, scope)
	if err != nil || status.Connection != "disconnected" || status.Paused || status.Coverage != "partial" || !reflect.DeepEqual(status.Codes, []string{"undecryptable_message"}) {
		t.Fatalf("first gap-only update fabricated a connection or lost coverage: %+v, %v", status, err)
	}
}
