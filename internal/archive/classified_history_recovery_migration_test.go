//go:build sqlite_fts5

package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Build genuine historical schemas from their immutable SQL, never from a
// latest Open with a forged migration receipt.
func classifiedVersionFixture(t *testing.T, version int, variant rebuildSourceVariant) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Unlike Open's fixtures, this builder intentionally bypasses
	// preparePrivateDir; set its required ownership boundary explicitly.
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "archive.sqlite")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("PRAGMA journal_mode=WAL; CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,checksum TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < version; i++ {
		b, err := migrationFiles.ReadFile("migrations/" + entries[i].Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(b)); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if _, err = db.Exec("INSERT INTO schema_migrations VALUES(?,?)", i+1, hex.EncodeToString(sum[:])); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec("INSERT INTO archive_meta(id,archive_revision,index_generation,normalization_version,cursor_key) VALUES(1,0,1,?,zeroblob(32)); INSERT INTO accounts VALUES('account'); INSERT INTO account_safety(account) VALUES('account'); INSERT INTO archive_policy(id,generation,date_mode,import_start,chat_mode,rolling_retention) VALUES(1,1,'all_available',0,'all',0)", LexicalNormalizationVersion); err != nil {
		t.Fatal(err)
	}
	if variant != rebuildSourceOrdinary {
		if _, err = db.Exec(rebuildSchema); err != nil {
			t.Fatal(err)
		}
	}
	if variant == rebuildSourceContinuation {
		if _, err = db.Exec(rebuildRequestUpgradeDDL + rebuildOperationSchema); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, credentialFenceMarker), []byte("v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = originalCacheExistingArchive(dir, true); err != nil {
		t.Fatal("synthetic archive filesystem precondition", err)
	}
	return dir
}

func TestClassifiedMigrationPreservesOrdinaryAuditAndConstraints(t *testing.T) {
	dir := classifiedVersionFixture(t, 14, rebuildSourceOrdinary)
	ctx := context.Background()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "archive.sqlite")+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	columns := "recovery_epoch,account,prior_unsafe_epoch,prior_unsafe_reason,prior_archive_revision,archive_revision,observation_sha256,unsafe_sha256,observation_file_identity,unsafe_file_identity,phase,created_at,completed_at"
	if _, err = db.Exec("INSERT INTO snapshot_recovery_audit(" + columns + ") VALUES(1,'account',1,'drain_failed',0,1,'','','','','complete',123,124)"); err != nil {
		t.Fatal(err)
	}
	before := rebuildRows(t, &Store{readDB: db}, "snapshot_recovery_audit", columns)
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if after := rebuildRows(t, &Store{readDB: db}, "snapshot_recovery_audit", columns); after != before {
		t.Fatal("migration changed ordinary audit")
	}
	if _, err = db.Exec("INSERT INTO snapshot_recovery_audit(recovery_epoch,account,prior_unsafe_epoch,prior_unsafe_reason,prior_archive_revision,archive_revision,phase,created_at) VALUES(2,'account',2,'history_corrupt',1,2,'prepared',125)"); err == nil {
		t.Fatal("history reason accepted without typed witness")
	}
}

func TestClassifiedCompatibilityHistorical14Through16RemainReadonly(t *testing.T) {
	for _, version := range []int{14, 15, 16} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			dir := classifiedVersionFixture(t, version, rebuildSourceOrdinary)
			path := filepath.Join(dir, "archive.sqlite")
			before, err := rebuildFileHash(path)
			if err != nil {
				t.Fatal(err)
			}
			s, err := OpenHistoricalSnapshot(context.Background(), dir)
			if err != nil {
				t.Fatal("historical open", err)
			}
			var got int
			if err = s.readDB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&got); err != nil || got != version {
				t.Fatal("historical schema mutated", got, err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			after, err := rebuildFileHash(path)
			if err != nil || after != before {
				t.Fatal("historical bytes changed")
			}
			if version < 16 {
				if err = validateOriginalCacheRecoverySchema(context.Background(), path); err == nil {
					t.Fatal("writable original-cache accepted old schema")
				}
			}
		})
	}
}

func TestClassifiedCompatibilityForwardVersionSchemaPairing(t *testing.T) {
	for _, version := range []int{14, 15, 16} {
		for _, variant := range []rebuildSourceVariant{rebuildSourceOrdinary, rebuildSourceLegacy, rebuildSourceContinuation} {
			t.Run(fmt.Sprintf("%d_%d", version, variant), func(t *testing.T) {
				dir := classifiedVersionFixture(t, version, variant)
				source, err := sql.Open(queryOnlyDriverName, filepath.Join(dir, "archive.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				defer source.Close()
				src, err := source.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				defer src.Rollback()
				dst := testArchive(t)
				tx, err := dst.writeDB.BeginTx(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				got, err := classifyRebuildSourceSchema(context.Background(), src, tx)
				if err != nil || got != variant {
					t.Fatal("supported exact schema rejected", got, err)
				}
			})
		}
	}
	for _, tamper := range []string{"receipt_mismatch", "receipt_16_over_15", "extra_ddl"} {
		t.Run(tamper, func(t *testing.T) {
			version := 14
			if tamper == "receipt_16_over_15" {
				version = 15
			}
			dir := classifiedVersionFixture(t, version, rebuildSourceOrdinary)
			source, err := sql.Open("sqlite3", filepath.Join(dir, "archive.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			if tamper == "receipt_mismatch" || tamper == "receipt_16_over_15" {
				name := "migrations/015_classified_history_recovery.sql"
				if version == 15 {
					name = "migrations/016_classified_history_username.sql"
				}
				b, _ := migrationFiles.ReadFile(name)
				sum := sha256.Sum256(b)
				_, err = source.Exec("INSERT INTO schema_migrations VALUES(?,?)", version+1, hex.EncodeToString(sum[:]))
			} else {
				_, err = source.Exec("CREATE TABLE unexpected_object(id INTEGER)")
			}
			if err != nil {
				t.Fatal(err)
			}
			src, err := source.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Rollback()
			dst := testArchive(t)
			tx, err := dst.writeDB.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err = classifyRebuildSourceSchema(context.Background(), src, tx); err == nil {
				t.Fatal("accepted mismatched receipt/schema")
			}
		})
	}
}

func TestClassifiedRecoveryPreMigrationCallbackOwnsArchiveLock(t *testing.T) {
	source, w := classifiedRecoveryFixture(t)
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	dir := classifiedVersionFixture(t, 14, rebuildSourceOrdinary)
	path := filepath.Join(dir, "archive.sqlite")
	before, _ := rebuildFileHash(path)
	called := false
	_, err := OpenClassifiedHistoryRecovery(context.Background(), dir, w, func() error {
		called = true
		if lock, err := acquireArchiveLock(context.Background(), dir); err == nil {
			lock.Close()
			t.Fatal("callback lacked archive ownership")
		}
		return ErrInvalidEvent
	})
	if !called || err != ErrInvalidEvent {
		t.Fatal("callback contract failed", called, err)
	}
	after, _ := rebuildFileHash(path)
	if before != after {
		t.Fatal("SQLite mutated before validated evidence callback")
	}
}
