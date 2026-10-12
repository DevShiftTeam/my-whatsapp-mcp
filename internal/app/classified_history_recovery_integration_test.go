package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

type classifiedIntegrationFixture struct {
	bootstrap LiveRebuildBootstrapConfig
	config    ClassifiedHistoryRecoveryConfig
	evidence  ClassifiedHistoryEvidence
	paths     appdata.Paths
	preserved map[string][]byte
}

// All fixtures are stopped synthetic profiles. No application constructor or
// network connector is used by these recovery tests.
func classifiedIntegrationProfile(t *testing.T, legacy bool) classifiedIntegrationFixture {
	t.Helper()
	ctx := context.Background()
	bootstrap, _ := syntheticLiveRebuild(t)
	receipt, err := BootstrapLiveRebuild(ctx, bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := appdata.ExistingPaths(bootstrap.DestinationProfileDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := archive.Open(ctx, paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.BeginObservation(ctx, receipt.Archive.Account); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err = store.MarkUnsafe(ctx, receipt.Archive.Account, "history_corrupt"); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	db := classifiedIntegrationDB(t, paths)
	if legacy {
		// Recreate the exact pre-classification audit table on this disposable,
		// stopped destination, so recovery genuinely migrates its archive bytes.
		raw, err := os.ReadFile("../archive/migrations/011_snapshot_recovery.sql")
		if err != nil {
			t.Fatal(err)
		}
		start := strings.Index(string(raw), "CREATE TABLE snapshot_recovery_audit")
		if start < 0 {
			t.Fatal("missing legacy audit schema")
		}
		if _, err = db.Exec("DROP TABLE snapshot_recovery_audit;" + string(raw[start:]) + "; DELETE FROM schema_migrations WHERE version=15"); err != nil {
			t.Fatal(err)
		}
	}
	cfg, e := syntheticClassifiedEvidence(t)
	cfg.DataDir = paths.Root
	e.Witness.RebuildGeneration = receipt.Archive.Generation
	if err = db.QueryRow("SELECT epoch FROM account_safety WHERE account=?", receipt.Archive.Account).Scan(&e.Witness.UnsafeEpoch); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT archive_revision,recovery_epoch FROM archive_meta WHERE id=1").Scan(&e.Witness.ArchiveRevision, &e.Witness.RecoveryEpoch); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	e.Witness.OriginalArchiveSHA256, err = classifiedArchiveHash(ctx, filepath.Join(paths.ArchiveDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, ready, err := liveRebuildLoadReady(paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	e.ReadySHA256 = liveRebuildDigest(ready)
	e.Witness.UnsafeMarkerSHA256, e.Witness.UnsafeMarkerIdentity = classifiedIntegrationMarker(t, filepath.Join(paths.ArchiveDir, "unsafe.marker"))
	e.Witness.ObservationMarkerSHA256, e.Witness.ObservationMarkerIdentity = classifiedIntegrationMarker(t, filepath.Join(paths.ArchiveDir, "observation.marker"))
	classifiedIntegrationResign(t, &cfg, &e)
	preserved := map[string][]byte{}
	for _, root := range []string{bootstrap.SourceProfileDir, bootstrap.CurrentBackupDir, paths.Root} {
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if root == paths.Root && path == paths.ArchiveDir {
				return filepath.SkipDir
			}
			if entry.Type().IsRegular() {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				preserved[path] = data
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return classifiedIntegrationFixture{bootstrap, cfg, e, paths, preserved}
}

func classifiedIntegrationDB(t *testing.T, paths appdata.Paths) *sql.DB {
	t.Helper()
	address := url.URL{Scheme: "file", Path: filepath.Join(paths.ArchiveDir, "archive.sqlite")}
	address.RawQuery = url.Values{"mode": {"rw"}, "_foreign_keys": {"on"}}.Encode()
	db, err := sql.Open("sqlite3", address.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func classifiedIntegrationMarker(t *testing.T, path string) (string, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := reflect.Indirect(reflect.ValueOf(info.Sys()))
	stamp := stat.FieldByName("Ctimespec")
	if !stamp.IsValid() {
		stamp = stat.FieldByName("Ctim")
	}
	if !stamp.IsValid() {
		t.Skip("marker identity requires Darwin or Linux stat")
	}
	number := func(v reflect.Value) uint64 {
		if v.Kind() >= reflect.Int && v.Kind() <= reflect.Int64 {
			return uint64(v.Int())
		}
		return v.Uint()
	}
	identity := fmt.Sprintf("%x:%x:%x:%x", number(stat.FieldByName("Dev")), number(stat.FieldByName("Ino")), number(stamp.FieldByName("Sec")), number(stamp.FieldByName("Nsec")))
	return liveRebuildDigest(raw), identity
}

// Re-sign the complete owner evidence chain after changing fixture witnesses.
// The source helper supplies the unchanged process-read and joined-exit files.
func classifiedIntegrationResign(t *testing.T, cfg *ClassifiedHistoryRecoveryConfig, e *ClassifiedHistoryEvidence) {
	t.Helper()
	write := func(path string, value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return liveRebuildDigest(raw)
	}
	raw, err := os.ReadFile(e.LaunchEvidencePath)
	var launch ClassifiedHistoryLaunchEvidence
	if err != nil || json.Unmarshal(raw, &launch) != nil {
		t.Fatal("invalid synthetic launch bundle")
	}
	launch.DataDir, launch.ReadySHA256 = cfg.DataDir, e.ReadySHA256
	e.Witness.LaunchEvidenceSHA256 = write(e.LaunchEvidencePath, launch)
	w := &e.Witness
	w.EvidenceSHA256 = ""
	w.OperatorReceiptSHA256 = write(e.OperatorReceiptPath, ClassifiedHistoryOperatorReceipt{Version: 1, AcceptedPartial: true, AttestedCurrentFence: true, LaunchSHA256: w.LaunchEvidenceSHA256, DiagnosticLogSHA256: w.DiagnosticLogSHA256, OriginalArchiveSHA256: w.OriginalArchiveSHA256, RebuildGeneration: w.RebuildGeneration, UnsafeEpoch: w.UnsafeEpoch, ArchiveRevision: w.ArchiveRevision, RecoveryEpoch: w.RecoveryEpoch})
	cfg.ExpectedEvidenceSHA256 = write(cfg.EvidencePath, e)
}

func (f classifiedIntegrationFixture) assertPreserved(t *testing.T) {
	t.Helper()
	for path, want := range f.preserved {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("recovery changed preserved synthetic file %s: %v", path, err)
		}
	}
}

func TestClassifiedHistoryProfileRecoveryPreservesPrivateStateAndRepeats(t *testing.T) {
	f := classifiedIntegrationProfile(t, false)
	ctx := context.Background()
	got, err := RecoverClassifiedHistorySnapshot(ctx, f.config)
	if err != nil {
		t.Fatal("classified profile recovery:", err)
	}
	if got.Mode != "committed_snapshot" || got.Code != "owner_restored_snapshot" || got.RecoveryEpoch != f.evidence.Witness.RecoveryEpoch+1 || uint64(got.ArchiveRevision) != f.evidence.Witness.ArchiveRevision+1 {
		t.Fatal("invalid partial recovery receipt", got)
	}
	f.assertPreserved(t)
	for _, name := range []string{"unsafe.marker", "observation.marker"} {
		if _, err := os.Lstat(filepath.Join(f.paths.ArchiveDir, name)); !os.IsNotExist(err) {
			t.Fatal("recovery left the accepted fence marker", name, err)
		}
	}
	again, err := RecoverClassifiedHistorySnapshot(ctx, f.config)
	if err != nil || again != got {
		t.Fatal("identical profile replay failed", again, err)
	}
	f.assertPreserved(t)
}

func TestClassifiedHistoryProfileRecoveryRequiresExclusiveProfileAndAncestor(t *testing.T) {
	for _, owner := range []string{"current", "ancestor"} {
		t.Run(owner, func(t *testing.T) {
			f := classifiedIntegrationProfile(t, false)
			paths := f.paths
			if owner == "ancestor" {
				var err error
				paths, err = appdata.ExistingPaths(f.bootstrap.SourceProfileDir)
				if err != nil {
					t.Fatal(err)
				}
			}
			lease, err := appdata.AcquireExisting(paths)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if _, err := RecoverClassifiedHistorySnapshot(context.Background(), f.config); err == nil {
				t.Fatal("recovery competed with an existing owner")
			}
			classifiedIntegrationAssertUnprepared(t, f)
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := RecoverClassifiedHistorySnapshot(context.Background(), f.config); err != nil {
				t.Fatal("recovery failed after owner released lease", err)
			}
			f.assertPreserved(t)
		})
	}
}

func TestClassifiedHistoryProfileRecoveryRejectsBindingDrift(t *testing.T) {
	for _, kind := range []string{"ready", "evidence", "current_archive", "generation"} {
		t.Run(kind, func(t *testing.T) {
			f := classifiedIntegrationProfile(t, false)
			switch kind {
			case "ready":
				f.evidence.ReadySHA256 = strings.Repeat("0", 64)
			case "current_archive":
				f.evidence.Witness.OriginalArchiveSHA256 = strings.Repeat("0", 64)
			case "generation":
				f.evidence.Witness.RebuildGeneration = strings.Repeat("0", 32)
			}
			classifiedIntegrationResign(t, &f.config, &f.evidence)
			if kind == "evidence" {
				f.config.ExpectedEvidenceSHA256 = strings.Repeat("0", 64)
			}
			if _, err := RecoverClassifiedHistorySnapshot(context.Background(), f.config); err == nil {
				t.Fatal("accepted changed binding", kind)
			}
			classifiedIntegrationAssertUnprepared(t, f)
		})
	}
}

func classifiedIntegrationAssertUnprepared(t *testing.T, f classifiedIntegrationFixture) {
	t.Helper()
	f.assertPreserved(t)
	if _, err := os.Lstat(filepath.Join(f.paths.WorkingDir, classifiedHistoryBindingFile)); !os.IsNotExist(err) {
		t.Fatal("rejected recovery persisted an evidence binding", err)
	}
	digest, identity := classifiedIntegrationMarker(t, filepath.Join(f.paths.ArchiveDir, "unsafe.marker"))
	if digest != f.evidence.Witness.UnsafeMarkerSHA256 || identity != f.evidence.Witness.UnsafeMarkerIdentity {
		t.Fatal("rejected recovery changed the unsafe marker")
	}
}

func TestClassifiedHistoryProfileRecoveryRetriesAfterBindingAndMigration(t *testing.T) {
	f := classifiedIntegrationProfile(t, true)
	ctx := context.Background()
	e, err := readClassifiedHistoryEvidence(f.config)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.AcquireExisting(f.paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	guards, err := AcquireLiveRebuildSourceGuard(f.paths.Root, nil)
	if err != nil || guards == nil {
		t.Fatal("missing ancestor leases", err)
	}
	defer guards.Close()
	store, err := archive.OpenClassifiedHistoryRecovery(ctx, f.paths.ArchiveDir, e.Witness, func() error {
		return prepareClassifiedHistoryBinding(ctx, f.paths, e)
	})
	if err != nil {
		t.Fatal("migration after durable evidence binding failed", err)
	}
	// Simulate stopping immediately after open/migration, before recovery prepare.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := guards.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := classifiedArchiveHash(ctx, filepath.Join(f.paths.ArchiveDir, "archive.sqlite"))
	if err != nil || after == e.Witness.OriginalArchiveSHA256 {
		t.Fatal("fixture did not migrate archive bytes", err)
	}
	db := classifiedIntegrationDB(t, f.paths)
	var version, audits int
	if err := db.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM snapshot_recovery_audit").Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if version != 15 || audits != 0 {
		t.Fatal("interruption was not between migration and recovery prepare", version, audits)
	}
	if _, err := os.Stat(filepath.Join(f.paths.WorkingDir, classifiedHistoryBindingFile)); err != nil {
		t.Fatal("missing durable pre-migration binding", err)
	}
	changed := f.evidence
	changed.Witness.ArchiveRevision++
	classifiedIntegrationResign(t, &f.config, &changed)
	if _, err := RecoverClassifiedHistorySnapshot(ctx, f.config); err == nil {
		t.Fatal("changed evidence resumed a bound recovery")
	}
	classifiedIntegrationResign(t, &f.config, &f.evidence)
	if _, err := RecoverClassifiedHistorySnapshot(ctx, f.config); err != nil {
		t.Fatal("exact evidence could not resume after migration", err)
	}
	f.assertPreserved(t)
}
