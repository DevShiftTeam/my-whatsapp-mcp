//go:build cgo && sqlite_fts5

package sqlitelex

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tokenRankTable(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("CREATE VIRTUAL TABLE token_docs USING fts5(body,caption,tokenize='unicode61 remove_diacritics 2')"); err != nil {
		t.Fatal(err)
	}
}

// Expected scores deliberately use whole indexed tokens, not character or
// substring density. The unchanged density oracle remains separately tested.
func TestTokenRankLocalInstancesAndOrdering(t *testing.T) {
	db := densityDB(t)
	tokenRankTable(t, db)
	inputs := []struct {
		body, caption string
		score         float64
	}{
		{"alpha alphaa alphaa alphaa alphaa alphaa", "", 1.0 / 6.0},
		{"alpha zzzzzzzzzzzzzzzzzzzz", "", 1.0 / 2.0},
		{"alpha", "alpha beta", 2.0 / 3.0},
		{"alpha\x00beta alpha", "", 2.0 / 3.0},
	}
	for n, input := range inputs {
		if _, err := db.Exec("INSERT INTO token_docs(rowid,body,caption) VALUES(?,?,?)", n+1, input.body, input.caption); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.Query("SELECT rowid,wa_doc_token_rank_v1(token_docs,body,caption) FROM token_docs WHERE token_docs MATCH 'alpha' ORDER BY 2 DESC,rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantOrder := []int{3, 4, 2, 1}
	seen := 0
	for rows.Next() {
		var id int
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			t.Fatal(err)
		}
		if seen >= len(wantOrder) || id != wantOrder[seen] || math.Float64bits(score) != math.Float64bits(inputs[id-1].score) {
			t.Fatalf("token score/order differs at %d: id=%d bits=%016x", seen, id, math.Float64bits(score))
		}
		seen++
	}
	if err := rows.Err(); err != nil || seen != len(inputs) {
		t.Fatalf("token page incomplete: count=%d err=%v", seen, err)
	}
	oldA := compareDensityOracle(t, db, inputs[0].body+" ", []string{"alpha"})
	oldB := compareDensityOracle(t, db, inputs[1].body+" ", []string{"alpha"})
	if oldA <= oldB {
		t.Fatal("fixture must establish intentional token/substr relevance reversal")
	}
	var unary, repeated float64
	if err := db.QueryRow("SELECT wa_doc_token_rank_v1(token_docs,body) FROM token_docs WHERE rowid=2 AND token_docs MATCH 'alpha'").Scan(&unary); err != nil || math.Float64bits(unary) != math.Float64bits(0.5) {
		t.Fatalf("single-field document score: %g %v", unary, err)
	}
	if err := db.QueryRow("SELECT wa_doc_token_rank_v1(token_docs,body,caption) FROM token_docs WHERE rowid=2 AND token_docs MATCH 'alpha AND alpha'").Scan(&repeated); err != nil || math.Float64bits(repeated) != math.Float64bits(1.0) {
		t.Fatalf("duplicate query phrase multiplicity: %g %v", repeated, err)
	}
}

// Validation consumes both fields completely but token relevance comes from
// the current FTS row. Exercise ASCII chunk and poll boundaries without making
// UTF-8 validation depend on token/rune counts or the first NUL.
func TestTokenRankUTF8AcrossASCIIChunks(t *testing.T) {
	db := densityDB(t)
	tokenRankTable(t, db)
	if _, err := db.Exec("INSERT INTO token_docs VALUES('alpha beta','')"); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []int{0, 7, 8, 4091, 4092, 4095, 4096} {
		for _, sequence := range []string{"\u00a2", "\u20ac", "\U0001f4a9"} {
			for _, nul := range []string{"", "\x00"} {
				text := strings.Repeat("a", offset) + nul + sequence + " alpha"
				var score float64
				if err := db.QueryRow("SELECT wa_doc_token_rank_v1(token_docs,?) FROM token_docs WHERE token_docs MATCH 'alpha'", text).Scan(&score); err != nil || math.Float64bits(score) != math.Float64bits(0.5) {
					t.Fatalf("single-field UTF-8 boundary offset=%d width=%d nul=%t: %g %v", offset, len(sequence), nul != "", score, err)
				}
				if err := db.QueryRow("SELECT wa_doc_token_rank_v1(token_docs,'alpha',?) FROM token_docs WHERE token_docs MATCH 'alpha'", text).Scan(&score); err != nil || math.Float64bits(score) != math.Float64bits(0.5) {
					t.Fatalf("caption UTF-8 boundary offset=%d width=%d nul=%t: %g %v", offset, len(sequence), nul != "", score, err)
				}
			}
		}
	}
}

func TestTokenRankMalformedAuthorizedBelowTopKAndDeniedIsolation(t *testing.T) {
	values := []struct {
		name  string
		value any
	}{
		{"null", nil}, {"integer", int64(4)}, {"blob", []byte("alpha")},
		{"nul_invalid_suffix", "alpha\x00" + string([]byte{0xff})},
		{"boundary_truncated_suffix", strings.Repeat("x", 4095) + "\x00" + string([]byte{0xc2})},
		{"ascii_chunk_overlong_suffix", "alpha\x00x" + string([]byte{0xc0, 0xaf})},
		{"ascii_poll_surrogate_suffix", strings.Repeat("x", 4095) + "\x00" + string([]byte{0xed, 0xa0, 0x80})},
		{"ascii_poll_out_of_range_suffix", strings.Repeat("x", 4096) + "\x00" + string([]byte{0xf4, 0x90, 0x80, 0x80})},
		{"ascii_chunk_bad_continuation", strings.Repeat("x", 8) + string([]byte{0xe2, 0x28, 0xa1})},
		{"oversize", strings.Repeat("a", 4*(256<<10)+1)},
	}
	for _, value := range values {
		t.Run(value.name, func(t *testing.T) {
			db := densityDB(t)
			tokenRankTable(t, db)
			if _, err := db.Exec("CREATE TABLE inputs(id INTEGER PRIMARY KEY,allowed INTEGER,body,caption)"); err != nil {
				t.Fatal(err)
			}
			for n := 1; n <= 101; n++ {
				if _, err := db.Exec("INSERT INTO token_docs(rowid,body,caption) VALUES(?,'alpha','')", n); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("INSERT INTO inputs VALUES(?,1,'alpha','')", n); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec("INSERT INTO token_docs(rowid,body,caption) VALUES(1000,'alpha beta beta beta','')"); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("INSERT INTO inputs VALUES(1000,1,?,'')", value.value); err != nil {
				t.Fatal(err)
			}
			query := "SELECT CASE WHEN i.allowed=1 THEN wa_doc_token_rank_v1(token_docs,i.body,i.caption) END AS score FROM token_docs JOIN inputs i ON i.id=token_docs.rowid WHERE token_docs MATCH 'alpha' ORDER BY score DESC,i.id LIMIT 1"
			expectDensityInvalid(t, db, query)
			if _, err := db.Exec("UPDATE inputs SET allowed=0 WHERE id=1000"); err != nil {
				t.Fatal(err)
			}
			var score float64
			if err := db.QueryRow(query).Scan(&score); err != nil || math.Float64bits(score) != math.Float64bits(1.0) {
				t.Fatalf("denied malformed input changed permitted winner: %g %v", score, err)
			}
		})
	}
	db := densityDB(t)
	tokenRankTable(t, db)
	if _, err := db.Exec("INSERT INTO token_docs VALUES('alpha','')"); err != nil {
		t.Fatal(err)
	}
	expectDensityInvalid(t, db, "SELECT wa_doc_token_rank_v1(token_docs,?) FROM token_docs WHERE token_docs MATCH 'alpha'", strings.Repeat("x", 4*(64<<10)+1))
	expectDensityInvalid(t, db, "SELECT wa_doc_token_rank_v1(token_docs) FROM token_docs WHERE token_docs MATCH 'alpha'")
	expectDensityInvalid(t, db, "SELECT wa_doc_token_rank_v1(token_docs,'alpha','','') FROM token_docs WHERE token_docs MATCH 'alpha'")
	expectDensityInvalid(t, db, "SELECT wa_doc_token_rank_v1(NULL,'alpha')")
}

func TestTokenRankReadOnlyConnectionsReopenAndDirectOnly(t *testing.T) {
	if err := Register(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "token.sqlite")
	for round := 0; round < 2; round++ {
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		if round == 0 {
			tokenRankTable(t, db)
			if _, err = db.Exec("INSERT INTO token_docs VALUES('alpha beta',''); CREATE VIEW token_view AS SELECT wa_doc_token_rank_v1(token_docs,body,caption) FROM token_docs WHERE token_docs MATCH 'alpha'"); err != nil {
				t.Fatal(err)
			}
		}
		db.SetMaxOpenConns(3)
		var connections []*sql.Conn
		for n := 0; n < 3; n++ {
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			connections = append(connections, conn)
			if _, err = conn.ExecContext(context.Background(), "PRAGMA query_only=ON"); err != nil {
				t.Fatal(err)
			}
			var score float64
			if err = conn.QueryRowContext(context.Background(), "SELECT wa_doc_token_rank_v1(token_docs,body,caption) FROM token_docs WHERE token_docs MATCH 'alpha'").Scan(&score); err != nil || math.Float64bits(score) != math.Float64bits(0.5) {
				t.Fatalf("reopened/read-only token scorer missing: %g %v", score, err)
			}
			if err = conn.QueryRowContext(context.Background(), "SELECT * FROM token_view").Scan(&score); err == nil {
				t.Fatal("DIRECTONLY token scorer executed through a view")
			}
			if _, err = conn.ExecContext(context.Background(), "INSERT INTO token_docs VALUES('prohibited','')"); err == nil {
				t.Fatal("token registration weakened query_only")
			}
		}
		for _, conn := range connections {
			if err = conn.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// Keep the auxiliary in a nonaggregate SELECT: SQLite rewrites aggregate
// columns before resolving FTS overloads. Materialization preserves its cursor.
func TestTokenRankCancellationAndConnectionReuse(t *testing.T) {
	db := densityDB(t)
	tokenRankTable(t, db)
	if _, err := db.Exec("INSERT INTO token_docs VALUES('alpha beta','')"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{strings.Repeat("a", 4*(256<<10)), "alpha\x00" + strings.Repeat("a", 4*(256<<10)-6)} {
		ctx, cancel := context.WithCancel(context.Background())
		timer := time.AfterFunc(2*time.Millisecond, cancel)
		var score float64
		err := db.QueryRowContext(ctx, "WITH RECURSIVE work(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM work WHERE n<10000), scores AS MATERIALIZED (SELECT wa_doc_token_rank_v1(token_docs,? || substr('',1,n%2),'') AS score FROM work JOIN token_docs ON 1=1 WHERE token_docs MATCH 'alpha') SELECT sum(score) FROM scores", text).Scan(&score)
		timer.Stop()
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled token query returned a score or wrong error: %v", err)
		}
		if err = db.QueryRow("SELECT wa_doc_token_rank_v1(token_docs,body,caption) FROM token_docs WHERE token_docs MATCH 'alpha'").Scan(&score); err != nil || math.Float64bits(score) != math.Float64bits(0.5) {
			t.Fatalf("cancelled token query damaged connection reuse: %g %v", score, err)
		}
		compareDensityOracle(t, db, "fresh alpha", []string{"alpha"})
	}
	// Exercise dense duplicate-phrase position iteration too. Polling between
	// occurrences must discard cancellation and leave the connection reusable.
	if _, err := db.Exec("INSERT INTO token_docs VALUES(?,'')", strings.Repeat("alpha ", 40000)); err != nil {
		t.Fatal(err)
	}
	query := strings.TrimSpace(strings.Repeat("alpha AND ", 31)) + " alpha"
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(time.Millisecond, cancel)
	defer timer.Stop()
	defer cancel()
	var score float64
	err := db.QueryRowContext(ctx, "WITH scores AS MATERIALIZED (SELECT wa_doc_token_rank_v1(token_docs,body,caption) AS score FROM token_docs WHERE token_docs MATCH ?) SELECT sum(score) FROM scores", query).Scan(&score)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("phrase-iteration cancellation returned a score: %v", err)
	}
	if err = db.QueryRow("SELECT wa_doc_token_rank_v1(token_docs,body,caption) FROM token_docs WHERE rowid=1 AND token_docs MATCH 'alpha'").Scan(&score); err != nil || math.Float64bits(score) != math.Float64bits(0.5) {
		t.Fatalf("phrase cancellation damaged connection reuse: %g %v", score, err)
	}
}
