package archive

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"github.com/mattn/go-sqlite3"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const queryOnlyDriverName = "wa_indexer_archive_reader"

func init() {
	sql.Register(queryOnlyDriverName, &sqlite3.SQLiteDriver{ConnectHook: func(c *sqlite3.SQLiteConn) error {
		if err := c.RegisterFunc("wa_indexer_name_key", chatNameKey, true); err != nil {
			return err
		}
		_, err := c.Exec("PRAGMA query_only=ON", nil)
		return err
	}})
}
func migrate(ctx context.Context, db *sql.DB) error {
	// Check actual compiled capability; no keyword-only silent fallback.
	if _, err := db.ExecContext(ctx, "CREATE VIRTUAL TABLE temp.fts_capability USING fts5(text)"); err != nil {
		return ErrFTS5Unavailable
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE temp.fts_capability"); err != nil {
		return ErrArchiveUnavailable
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return ErrArchiveUnavailable
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY,checksum TEXT NOT NULL)"); err != nil {
		return ErrArchiveUnavailable
	}

	const latest = 16
	contents := make([][]byte, latest)
	checksums := make([]string, latest)
	names := []string{"001_archive.sql", "002_indexing.sql", "003_media.sql", "004_credentials.sql", "005_media_identity.sql", "006_index_reuse.sql", "007_fts_secure_delete.sql", "008_publication_extraction.sql", "009_original_downloads.sql", "010_unresolved_deletions.sql", "011_snapshot_recovery.sql", "012_outbound.sql", "013_send_permission_modes.sql", "014_documents.sql", "015_classified_history_recovery.sql", "016_classified_history_username.sql"}
	for i, name := range names {
		contents[i], err = migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return ErrArchiveUnavailable
		}
		sum := sha256.Sum256(contents[i])
		checksums[i] = hex.EncodeToString(sum[:])
	}
	rows, err := tx.QueryContext(ctx, "SELECT version,checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return ErrArchiveUnavailable
	}
	applied := 0
	for rows.Next() {
		var version int
		var stored string
		if err = rows.Scan(&version, &stored); err != nil {
			rows.Close()
			return ErrArchiveUnavailable
		}
		if version > latest {
			rows.Close()
			return ErrSchemaTooNew
		}
		if version != applied+1 || stored != checksums[version-1] {
			rows.Close()
			return ErrSchemaChanged
		}
		applied++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ErrArchiveUnavailable
	}
	for i := applied; i < latest; i++ {
		if _, err = tx.ExecContext(ctx, string(contents[i])); err != nil {
			return ErrArchiveUnavailable
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,checksum) VALUES(?,?)", i+1, checksums[i]); err != nil {
			return ErrArchiveUnavailable
		}
		if i == 0 {
			key := make([]byte, 32)
			if _, err = rand.Read(key); err != nil {
				return ErrArchiveUnavailable
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO archive_meta(id,archive_revision,index_generation,normalization_version,cursor_key) VALUES(1,0,1,?,?)", LexicalNormalizationVersion, key); err != nil {
				return ErrArchiveUnavailable
			}
		}
	}
	var norm string
	if err = tx.QueryRowContext(ctx, "SELECT normalization_version FROM archive_meta WHERE id=1").Scan(&norm); err != nil || strings.TrimSpace(norm) != LexicalNormalizationVersion {
		return ErrSchemaChanged
	}
	if applied < latest {
		if err = finishMigrationForeignKeys(ctx, tx); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return ErrArchiveUnavailable
	}
	return nil
}

func finishMigrationForeignKeys(ctx context.Context, tx *sql.Tx) error {
	// The parent rebuild defers NO ACTION checks while its same-name table is
	// replaced. SQLite retains transient drop counters even after that table
	// is restored; reset those counters only after checking the actual graph.
	var enabled int
	if err := tx.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
		return ErrArchiveUnavailable
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return ErrArchiveUnavailable
	}
	broken := rows.Next()
	err = rows.Err()
	closeErr := rows.Close()
	if broken || err != nil || closeErr != nil {
		return ErrArchiveUnavailable
	}
	if _, err = tx.ExecContext(ctx, "PRAGMA defer_foreign_keys=OFF"); err != nil {
		return ErrArchiveUnavailable
	}
	return nil
}
