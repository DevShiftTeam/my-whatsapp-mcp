//go:build cgo

package sqlitelex

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/sqlitelex/internal/testsqlite"
)

func TestDensityReleasesStatementAndConnectionMemory(t *testing.T) {
	if err := Register(); err != nil {
		t.Fatal(err)
	}
	cycle := func() {
		t.Helper()
		db, err := sql.Open("sqlite3", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		stmt, err := db.Prepare("SELECT wa_doc_density_v1(?,?)")
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		for i := 0; i < 16; i++ {
			text := strings.Repeat("alpha ", 10000) + strings.Repeat("β", i)
			term := "alpha"
			if i%2 != 0 {
				term = "missing"
			}
			var score float64
			if err = stmt.QueryRow(text, term).Scan(&score); err != nil {
				stmt.Close()
				db.Close()
				t.Fatal(err)
			}
			if (i%2 == 0 && score <= 0) || (i%2 != 0 && score != 0) {
				t.Fatal("statement reused stale input or term")
			}
		}
		if err = stmt.Close(); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Warm SQLite's one-time registry allocations before comparing cycles.
	cycle()
	baseline := testsqlite.MemoryUsed()
	for i := 0; i < 8; i++ {
		cycle()
	}
	if retained := testsqlite.MemoryUsed() - baseline; retained > 4096 {
		t.Fatalf("repeated scorer connections retained SQLite memory: %d bytes", retained)
	}
}
