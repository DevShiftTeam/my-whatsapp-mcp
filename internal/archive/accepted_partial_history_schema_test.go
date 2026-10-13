//go:build sqlite_fts5

package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestPartialHistorySchemaCompatibilityExact14Through19(t *testing.T) {
	latest := testArchive(t)
	var version int
	if err := latest.readDB.QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil || version != 19 {
		t.Fatal("partial uncertainty migration missing", version, err)
	}
	for _, version := range []int{14, 15, 16, 17, 18, 19} {
		for _, variant := range []rebuildSourceVariant{rebuildSourceOrdinary, rebuildSourceLegacy, rebuildSourceContinuation} {
			t.Run(fmt.Sprintf("%d_%d", version, variant), func(t *testing.T) {
				dir := classifiedVersionFixture(t, version, variant)
				path := filepath.Join(dir, "archive.sqlite")
				before, err := rebuildFileHash(path)
				if err != nil {
					t.Fatal(err)
				}
				s, err := OpenHistoricalSnapshot(context.Background(), dir)
				if err != nil {
					t.Fatal("exact readonly version rejected", err)
				}
				var snapshot ReadSnapshot
				if err = unknownHistoryGapSnapshot(context.Background(), s.readDB, "account", &snapshot); err != nil || snapshot.UnknownHistoryGap || snapshot.CoverageEpoch != 0 {
					t.Fatal("legacy/current empty uncertainty projection", snapshot, err)
				}
				diagnostics, diagnosticErr := s.HistoryUncertaintyDiagnostics(context.Background(), "account")
				if diagnosticErr != nil || diagnostics.Epoch != 0 || len(diagnostics.Reasons) != 0 {
					t.Fatal("legacy/current empty uncertainty diagnostics", diagnostics, diagnosticErr)
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				after, err := rebuildFileHash(path)
				if err != nil || before != after {
					t.Fatal("readonly source changed", err)
				}
				if version < 19 && validateOriginalCacheRecoverySchema(context.Background(), path) == nil {
					t.Fatal("current writable recovery accepted old schema")
				}
			})
		}
	}
}

func TestPartialHistorySchemaCompatibilityRejectsForged17DDL(t *testing.T) {
	dir := classifiedVersionFixture(t, 16, rebuildSourceOrdinary)
	db, err := sql.Open("sqlite3", filepath.Join(dir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ddl, err := migrationFiles.ReadFile("migrations/017_unknown_history_gaps.sql")
	if err != nil {
		t.Fatal("missing compiled gap migration", err)
	}
	// An authentic migration receipt cannot authorize a different table CHECK.
	tampered := strings.Replace(string(ddl), "raw_stub<=2147483647", "raw_stub<=2147483646", 1)
	if tampered == string(ddl) {
		t.Fatal("test CHECK not present")
	}
	if _, err = db.Exec(tampered); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(ddl)
	if _, err = db.Exec("INSERT INTO schema_migrations(version,checksum) VALUES(17,?)", hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenHistoricalSnapshot(context.Background(), dir); err == nil {
		t.Fatal("authentic receipt admitted tampered full DDL")
	}
}

func TestPartialHistoryForwardCarriesStickyGlobalUncertainty(t *testing.T) {
	s, _, _ := rebuildFixture(t)
	if _, err := s.writeDB.Exec("INSERT INTO unknown_history_coverage VALUES('account',9,12); INSERT INTO unknown_history_codes VALUES('account',229,12,-10,-5); INSERT INTO sync_gaps VALUES('account','','unknown_history_notice')"); err != nil {
		t.Fatal("missing sticky gap storage", err)
	}
	before := acceptedPartialGapRows(t, s)
	_, dest, err := forwardBootstrapTest(t, s)
	if err != nil {
		t.Fatal(err)
	}
	next, err := Open(context.Background(), dest, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if before != acceptedPartialGapRows(t, next) {
		t.Fatal("forward bootstrap reset gap epoch or evidence")
	}
	var gap, unknown, bodies int
	if err = next.readDB.QueryRow("SELECT count(*) FROM sync_gaps WHERE account='account' AND chat='' AND code='unknown_history_notice'").Scan(&gap); err != nil || gap != 1 {
		t.Fatal("lost global warning", gap, err)
	}
	if err = next.readDB.QueryRow("SELECT coverage_unknown FROM sync_state WHERE account='account' AND chat=''").Scan(&unknown); err != nil || unknown != 1 {
		t.Fatal("forward coverage claimed complete", unknown, err)
	}
	if err = next.readDB.QueryRow("SELECT count(*) FROM messages").Scan(&bodies); err != nil || bodies != 0 {
		t.Fatal("forward imported old bodies", bodies, err)
	}
}

func TestPartialHistoryMigrationPreservesPriorAuditBytesAndStrictGapDDL(t *testing.T) {
	dir := classifiedVersionFixture(t, 16, rebuildSourceOrdinary)
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "archive.sqlite")+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const columns = "recovery_epoch,account,prior_unsafe_epoch,prior_unsafe_reason,prior_archive_revision,archive_revision,phase,created_at,completed_at,classification,witness_json,witness_sha256"
	for i, stub := range []int{33, 193, 217} {
		class := ClassifiedHistoryChangeUsername
		if stub == 33 {
			class = ClassifiedHistoryGroupParticipantChangeNumber
		}
		witness := fmt.Sprintf("{ \"diagnostic\" : { \"raw_stub\" : %d } }", stub)
		if _, err = db.Exec("INSERT INTO snapshot_recovery_audit("+columns+") VALUES(?,'account',?,'history_corrupt',?,?,'complete',1,2,?,?,?)", i+1, i+1, i, i+1, class, witness, strings.Repeat("a", 64)); err != nil {
			t.Fatal(err)
		}
	}
	before := rebuildRows(t, &Store{readDB: db}, "snapshot_recovery_audit", columns)
	if err = migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if before != rebuildRows(t, &Store{readDB: db}, "snapshot_recovery_audit", columns) {
		t.Fatal("017 reserialized prior witnesses")
	}
	for _, invalid := range []string{
		"INSERT INTO unknown_history_coverage VALUES(NULL,1,1)",
		"INSERT INTO unknown_history_coverage VALUES('account',0,1)",
		"INSERT INTO unknown_history_coverage VALUES('account',1.5,1)",
		"INSERT INTO unknown_history_codes VALUES('account',0,1,1,2)",
		"INSERT INTO unknown_history_codes VALUES('account',2147483648,1,1,2)",
		"INSERT INTO unknown_history_codes VALUES('account',229,0,1,2)",
		"INSERT INTO unknown_history_codes VALUES('account',229,1.5,1,2)",
		"INSERT INTO unknown_history_codes VALUES('account',229,1,2,1)",
		"INSERT INTO unknown_history_codes VALUES('account',229,1,1.5,2)",
	} {
		if _, err = db.Exec(invalid); err == nil {
			t.Fatal("gap DDL admitted invalid value", invalid)
		}
	}
}

func TestAcceptedPartialHistoryOpenSealsBeforeMigrationAndRequiresAcceptance(t *testing.T) {
	_, w := acceptedPartialRecoveryFixture(t)
	dir := classifiedVersionFixture(t, 16, rebuildSourceOrdinary)
	path := filepath.Join(dir, "archive.sqlite")
	before, err := rebuildFileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = OpenAcceptedPartialHistoryRecovery(context.Background(), dir, w, func() error {
		called = true
		current, err := rebuildFileHash(path)
		if err != nil || current != before {
			t.Fatal("SQLite changed bytes before seal", err)
		}
		return ErrArchiveUnavailable
	})
	if err != ErrArchiveUnavailable || !called {
		t.Fatal("seal callback not respected", err, called)
	}
	after, err := rebuildFileHash(path)
	if err != nil || before != after {
		t.Fatal("failed seal migrated source", err)
	}
	called = false
	w.AcceptUnknownHistoryGaps = false
	if _, err = OpenAcceptedPartialHistoryRecovery(context.Background(), dir, w, func() error { called = true; return nil }); err == nil || called {
		t.Fatal("missing acceptance reached mutable opening", err, called)
	}
}
