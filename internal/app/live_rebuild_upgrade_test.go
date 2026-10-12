package app

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
)

// This is a full profile upgrade, not a second bootstrap. The saved session
// intentionally differs from the original backup before either restart.
func TestLiveRebuildLegacyUpgradeNeverRecopiesEvolvedSavedDevice(t *testing.T) {
	ctx := context.Background()
	noNetwork := func(ingestion.Config, archive.ConnectorStore, ingestion.Callbacks) (connector, error) {
		t.Error("offline saved-device upgrade attempted connector construction")
		return nil, archive.ErrSourceUnavailable
	}
	cfg, original := syntheticLiveRebuild(t)
	receipt, err := BootstrapLiveRebuild(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(cfg.DestinationProfileDir, "session/session.sqlite")
	address := url.URL{Scheme: "file", Path: sessionPath}
	address.RawQuery = url.Values{"mode": {"rw"}, "_foreign_keys": {"on"}}.Encode()
	session, err := sql.Open("sqlite3", address.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.ExecContext(ctx, "CREATE TABLE synthetic_evolved_device_state(epoch INTEGER NOT NULL); INSERT INTO synthetic_evolved_device_state VALUES(2)"); err != nil {
		session.Close()
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	evolved, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(evolved, original["session/session.sqlite"]) {
		t.Fatal("fixture failed to evolve the destination device")
	}
	if err := liveRebuildSession(ctx, sessionPath, cfg.ExpectedAccountID, cfg.ExpectedDeviceID); err != nil {
		t.Fatal("evolved device identity invalid")
	}
	app, err := newApplication(ctx, Config{DataDir: cfg.DestinationProfileDir, Offline: true}, noNetwork)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close(ctx) })
	progress, err := app.RebuildProgress(ctx)
	if err != nil || progress == nil {
		t.Fatal("missing operation")
	}
	anchors, err := app.store.NextRebuildCandidates(ctx, 1)
	if err != nil || len(anchors) != 1 {
		t.Fatal("missing exact candidate")
	}
	requestID := "legacy-device-upgrade-pending"
	if err := app.store.PendingRebuildRequest(ctx, archive.RebuildRequest{ID: requestID, Generation: progress.Generation, OperationID: progress.OperationID, OperationRevision: progress.Revision, Key: anchors[0].Key, PolicyGeneration: progress.PolicyGeneration, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	beforeProgress, err := app.RebuildProgress(ctx)
	if err != nil || beforeProgress.CopySpent != 1 {
		t.Fatal("request spend was not durable")
	}
	provenance, err := app.store.RebuildProvenance(ctx)
	if err != nil || provenance == nil || !reflect.DeepEqual(*provenance, receipt.Archive.RebuildProvenance) {
		t.Fatal("unexpected original provenance")
	}
	clientBytes := map[string][]byte{}
	for _, record := range app.authState.NativeClients {
		data, err := os.ReadFile(filepath.Join(app.paths.ClientDir, record.ConfigName))
		if err != nil {
			t.Fatal(err)
		}
		clientBytes[record.ConfigName] = data
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// All application/session owners are stopped before this synthetic downgrade.
	address.Path = filepath.Join(cfg.DestinationProfileDir, "archive/archive.sqlite")
	db, err := sql.Open("sqlite3", address.String())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	for _, statement := range []string{
		"ALTER TABLE rebuild_requests RENAME TO continuation_requests",
		"CREATE TABLE rebuild_requests(id TEXT PRIMARY KEY,generation TEXT NOT NULL,account TEXT NOT NULL,chat TEXT NOT NULL,sender TEXT NOT NULL,message TEXT NOT NULL,from_me INTEGER NOT NULL,policy_generation INTEGER NOT NULL,created_at INTEGER NOT NULL,state TEXT NOT NULL CHECK(state IN('pending','consumed')),consumed_at INTEGER NOT NULL DEFAULT 0)",
		"INSERT INTO rebuild_requests SELECT id,generation,account,chat,sender,message,from_me,policy_generation,created_at,state,consumed_at FROM continuation_requests",
		"DROP TABLE continuation_requests", "DROP TABLE rebuild_operation", "DROP TABLE rebuild_commands", "DROP TABLE rebuild_discovery_attempts", "DROP TABLE rebuild_candidate_deferrals",
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			tx.Rollback()
			db.Close()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for restart := 0; restart < 2; restart++ {
		upgraded, err := newApplication(ctx, Config{DataDir: cfg.DestinationProfileDir, Offline: true}, noNetwork)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { upgraded.Close(ctx) })
		got, err := os.ReadFile(sessionPath)
		if err != nil || !bytes.Equal(got, evolved) {
			t.Fatal("upgrade replaced evolving saved session with original backup")
		}
		after, err := upgraded.store.RebuildProvenance(ctx)
		if err != nil || !reflect.DeepEqual(after, provenance) {
			t.Fatal("upgrade replaced observation identity")
		}
		p, err := upgraded.RebuildProgress(ctx)
		if err != nil || p.CopySpent != 1 || p.Pending != 1 || p.Generation != beforeProgress.Generation {
			t.Fatal("upgrade lost or refunded legacy request")
		}
		matched, err := upgraded.store.MatchRebuildRequest(ctx, requestID, anchors[0].Key, p.Generation, p.PolicyGeneration)
		if err != nil || !matched {
			t.Fatal("upgrade lost exact pending authority")
		}
		for name, want := range clientBytes {
			got, err := os.ReadFile(filepath.Join(upgraded.paths.ClientDir, name))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatal("upgrade rewrote saved native configuration")
			}
		}
		if err := upgraded.Close(ctx); err != nil {
			t.Fatal(err)
		}
		for _, root := range []string{cfg.SourceProfileDir, cfg.CurrentBackupDir} {
			for name, want := range original {
				got, err := os.ReadFile(filepath.Join(root, name))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatal("upgrade modified original source or backup")
				}
			}
		}
	}
}
