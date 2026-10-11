//go:build cgo

package sqlitelex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/textnorm"
	"github.com/mattn/go-sqlite3"
)

type densityQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func densityDB(t *testing.T) *sql.DB {
	t.Helper()
	if err := Register(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// This is the pre-optimization SQL itself, not a Go restatement of the scorer.
// Wrong case folding, overlap handling, rune length or arithmetic fails against
// the actual bundled SQLite length/replace evaluator.
func densityOracleExpression(text string, terms int) string {
	parts := make([]string, terms)
	for i := range parts {
		parts[i] = "((length(" + text + ")-length(replace(" + text + ",?,'')))*1.0/max(1,length(?)))"
	}
	return "(" + strings.Join(parts, "+") + ")/max(1,length(" + text + "))"
}

func densityCall(text string, terms int) string {
	return "wa_doc_density_v1(" + text + strings.Repeat(",?", terms) + ")"
}

func compareDensityOracle(t *testing.T, db densityQuerier, text string, terms []string) float64 {
	t.Helper()
	args := []any{text}
	for _, term := range terms {
		args = append(args, term)
	}
	for _, term := range terms {
		args = append(args, term, term)
	}
	query := "WITH input AS (SELECT ? AS text) SELECT " + densityCall("text", len(terms)) + "," + densityOracleExpression("text", len(terms)) + " FROM input"
	var native, oracle float64
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&native, &oracle); err != nil {
		t.Fatal(err)
	}
	if math.Float64bits(native) != math.Float64bits(oracle) {
		t.Fatalf("density bits differ: native=%016x oracle=%016x", math.Float64bits(native), math.Float64bits(oracle))
	}
	if math.IsNaN(native) || math.IsInf(native, 0) || math.Signbit(native) {
		t.Fatalf("density is not finite/nonnegative: %016x", math.Float64bits(native))
	}
	return native
}

func TestDensityExactSQLiteFloatBits(t *testing.T) {
	db := densityDB(t)
	cases := []struct {
		name, text string
		terms      []string
	}{
		{"ascii_case_verbatim", "Hello HELLO hello", []string{"hello", "HELLO"}},
		{"no_match_positive_zero", "HELLO", []string{"hello"}},
		{"substring_nonoverlap", "aaaaa", []string{"aa"}},
		{"duplicate_terms", "aaaaa", []string{"aa", "aa", "a"}},
		{"prefix_fallback", "ababababac ababaca", []string{"ababaca", "abac"}},
		{"hebrew_marks_verbatim", "שָׁלוֹם שלום שָׁלוֹם ד״ר ד\"ר", []string{"שלום", "שָׁלוֹם", "ד״ר"}},
		{"hebrew_substring", "אבאבאב", []string{"אב"}},
		{"unicode_case_no_fold", "éÉ e Æ æ ⱥ Ⱥ", []string{"é", "É", "ⱥ", "Æ"}},
		{"supplementary_and_combining", "😀𐐀😀a\u0301𐐀", []string{"😀", "𐐀", "a\u0301"}},
		{"empty_input", "", []string{"a"}},
		{"empty_fields_join_space", " ", []string{"a"}},
		{"body_caption_boundary", "foo bar", []string{"o b", "foo", "bar"}},
		{"nul_before_hits", "\x00alpha alpha", []string{"alpha"}},
		{"nul_inside_hit", "al\x00pha alpha", []string{"alpha", "al"}},
		{"nul_after_hits", "alpha alpha\x00alpha", []string{"alpha"}},
		{"nul_with_multibyte_prefix", "שלום 😀\x00שלום 😀", []string{"שלום", "😀"}},
		{"max_term_width", strings.Repeat("𐐀", 128) + " x", []string{strings.Repeat("𐐀", 128)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { compareDensityOracle(t, db, tc.text, tc.terms) })
	}
	if got := compareDensityOracle(t, db, "HELLO", []string{"hello"}); math.Float64bits(got) != 0 {
		t.Fatalf("zero score is not positive zero: %016x", math.Float64bits(got))
	}
}

func TestDensityShortLiteralExactSQLiteFloatBits(t *testing.T) {
	db := densityDB(t)
	cases := []struct {
		name, text string
		terms      []string
	}{
		{"empty_prefix", "", []string{"alpha"}},
		{"shorter_prefix", "alph", []string{"alpha"}},
		{"single_byte_dense", strings.Repeat("a", 8195), []string{"a"}},
		{"single_byte_no_hit", strings.Repeat("x", 8195), []string{"a"}},
		{"short_dense", strings.Repeat("alpha", 1641), []string{"alpha"}},
		{"short_no_hit_same_first_byte", strings.Repeat("a", 8195), []string{"alpha"}},
		{"greedy_overlap", strings.Repeat("a", 8195), []string{"aa", "aaa", "aaaaaaaa"}},
		{"short_prefix_fallback", strings.Repeat("ababab", 1400) + "abac", []string{"ababac", "abac"}},
		{"hebrew_eight_bytes", strings.Repeat("שלום", 1027), []string{"שלום"}},
		{"supplementary_eight_bytes", strings.Repeat("😀😀", 1027), []string{"😀😀", "😀"}},
		{"combining_literal", strings.Repeat("a\u0301", 2731), []string{"a\u0301", "\u0301"}},
		{"long_adversarial_fallback", strings.Repeat("a", 8195) + "b" + strings.Repeat("a", 257) + "b", []string{"aaaaaaaab", strings.Repeat("a", 127) + "b"}},
		{"long_multibyte_fallback", strings.Repeat("שלוםא", 1000) + "שלום", []string{"שלוםא", "שלום"}},
		{"mixed_lengths_and_duplicate_terms", strings.Repeat("alpha שלום aa 😀😀 a\u0301 ", 400), []string{"alpha", "שלום", "aa", "😀😀", "a\u0301", "alpha", "aaaaaaaab"}},
		{"single_mixed_utf8_literal", strings.Repeat("שלום😀a\u0301|", 600), []string{"😀a\u0301"}},
		{"single_long_prefix_fallback", strings.Repeat("a", 8195) + "b", []string{strings.Repeat("a", 127) + "b"}},
		{"single_maximum_utf8_term", strings.Repeat("𐐀", 256) + "|𐐀", []string{strings.Repeat("𐐀", 128)}},
		{"single_partial_match_before_nul", "ababac|ababa\x00cababac", []string{"ababac"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { compareDensityOracle(t, db, tc.text, tc.terms) })
	}
}

func TestDensityLiteralWindowBoundariesExactSQLiteFloatBits(t *testing.T) {
	db := densityDB(t)
	terms := []struct {
		name, text string
	}{
		{"two_bytes", "aa"},
		{"five_bytes", "alpha"},
		{"eight_ascii_bytes", "abcdefgh"},
		{"eight_hebrew_bytes", "שלום"},
		{"eight_supplementary_bytes", "😀😀"},
		{"nine_bytes", "aaaaaaaab"},
	}
	// Place complete literals immediately before, across and after both byte
	// boundaries. Each case also has a second hit, so a boundary hit cannot be
	// dropped or counted twice while still matching the SQLite oracle.
	for _, term := range terms {
		for _, boundary := range []int{4096, 8192} {
			for _, delta := range []int{-len(term.text), -1, 0, 1, len(term.text)} {
				at := boundary + delta
				text := strings.Repeat("x", at) + term.text + "|" + term.text + strings.Repeat("x", 17)
				t.Run(fmt.Sprintf("%s_at_%d", term.name, at), func(t *testing.T) {
					compareDensityOracle(t, db, text, []string{term.text})
				})
			}
		}
	}
	cases := []struct {
		name, text string
		terms      []string
	}{
		{"nul_inside_crossing_literal", strings.Repeat("x", 4094) + "al\x00pha" + strings.Repeat("alpha", 820), []string{"alpha", "al"}},
		{"nul_at_boundary_before_literal", strings.Repeat("x", 4096) + "\x00alpha" + strings.Repeat("alpha", 820), []string{"alpha"}},
		{"nul_after_crossing_hebrew_literal", strings.Repeat("x", 4095) + "שלום\x00" + strings.Repeat("שלום", 600), []string{"שלום", "missing"}},
		{"undersized_prefix_valid_full_suffix", "al\x00" + strings.Repeat("שלום", 1024), []string{"alpha", "שלום"}},
		{"multibyte_prefix_crossing_hebrew_literal", strings.Repeat("א", 2047) + "xשלום|שלוםא", []string{"שלום", "א"}},
		{"multibyte_prefix_crossing_supplementary_literal", strings.Repeat("😀", 1023) + "xxx😀😀|😀😀", []string{"😀😀", "😀"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { compareDensityOracle(t, db, tc.text, tc.terms) })
	}
}

func TestDensityLiteralWindowRejectsMalformedSuffix(t *testing.T) {
	db := densityDB(t)
	cases := []struct {
		name string
		text []byte
		term string
	}{
		{"empty_prefix_invalid_suffix", append([]byte("\x00"+strings.Repeat("alpha", 820)), 0xff), "alpha"},
		{"short_prefix_truncated_suffix", append([]byte("al\x00"+strings.Repeat("x", 4096)), 0xc2), "alpha"},
		{"short_hit_surrogate_suffix", append([]byte(strings.Repeat("x", 4095)+"alpha\x00"+strings.Repeat("x", 4096)), 0xed, 0xa0, 0x80), "alpha"},
		{"hebrew_hit_out_of_range_suffix", append([]byte(strings.Repeat("x", 4095)+"שלום\x00"+strings.Repeat("x", 4096)), 0xf4, 0x90, 0x80, 0x80), "שלום"},
		{"crossing_truncated_sequence", append([]byte(strings.Repeat("x", 4095)), 0xe2, 0x82), "alpha"},
		{"crossing_invalid_continuation", append([]byte(strings.Repeat("x", 4095)), 0xf0, 0x90, 0x41, 0x80), "alpha"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expectDensityInvalid(t, db, "SELECT wa_doc_density_v1(CAST(? AS TEXT),?)", tc.text, tc.term)
		})
	}
	for _, tc := range []struct {
		name string
		term []byte
	}{
		{"later_term_invalid_utf8", []byte{'a', 0xff}},
		{"later_term_nul", []byte{'a', 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectDensityInvalid(t, db, "SELECT wa_doc_density_v1(?,'alpha',CAST(? AS TEXT))", strings.Repeat("x", 8193), tc.term)
		})
	}
}

func TestDensityNormalizationWidthAndAcceptedBoundaries(t *testing.T) {
	db := densityDB(t)
	// U+023A grows from two bytes to three under the existing lexical lowercasing.
	// Applying the original ingress byte cap to normalized text would reject it.
	for _, ingress := range []int{256 << 10, 64 << 10} {
		original := strings.Repeat("Ⱥ", ingress/2)
		normalized := textnorm.NormalizeLexical(original)
		if len(normalized) <= ingress {
			t.Fatal("fixture did not exercise normalization width growth")
		}
		compareDensityOracle(t, db, normalized+" ", []string{"ⱥ"})
	}
	compareDensityOracle(t, db, strings.Repeat("a", 4*(256<<10)+1), []string{"a"})
	terms := make([]string, 32)
	for i := range terms {
		terms[i] = "aa"
	}
	compareDensityOracle(t, db, "aaaaa", terms)
}

func TestDensityPreservesSQLiteCompleteOrderingAndPages(t *testing.T) {
	db := densityDB(t)
	if _, err := db.Exec(`CREATE TABLE documents(id TEXT,text TEXT,sent INTEGER,is_root INTEGER,PRIMARY KEY(id,is_root));
 INSERT INTO documents VALUES('a','alpha',10,0),('b','alpha',10,1),('c','alpha alpha beta',20,0),
 ('d','ALPHA',40,0),('e','alpha beta',30,1),('f','alpha',5,0),
 ('same','alpha',10,0),('same','alpha',10,1)`); err != nil {
		t.Fatal(err)
	}
	type ranked struct {
		id   string
		root bool
		bits uint64
	}
	read := func(expression string, args []any, offset int) []ranked {
		t.Helper()
		args = append(append([]any(nil), args...), 2, offset)
		rows, err := db.Query("SELECT id,is_root,"+expression+" AS score FROM documents ORDER BY score DESC,sent DESC,id,is_root LIMIT ? OFFSET ?", args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []ranked
		for rows.Next() {
			var id string
			var root bool
			var score float64
			if err = rows.Scan(&id, &root, &score); err != nil {
				t.Fatal(err)
			}
			out = append(out, ranked{id, root, math.Float64bits(score)})
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for offset := 0; offset <= 8; offset += 2 {
		native := read(densityCall("text", 2), []any{"alpha", "alpha"}, offset)
		oracle := read(densityOracleExpression("text", 2), []any{"alpha", "alpha", "alpha", "alpha"}, offset)
		if fmt.Sprint(native) != fmt.Sprint(oracle) {
			t.Fatalf("page at %d differs: native=%v oracle=%v", offset, native, oracle)
		}
	}
}

func expectDensityInvalid(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	var ignored float64
	err := db.QueryRow(query, args...).Scan(&ignored)
	var sqliteError sqlite3.Error
	if !errors.As(err, &sqliteError) || sqliteError.Code != sqlite3.ErrError || err.Error() != "sqlitelex_invalid_input" {
		t.Fatalf("wanted fixed malformed-input error, got %v", err)
	}
}

func TestDensityRejectsInvalidTypesUTF8ExtentsAndArity(t *testing.T) {
	db := densityDB(t)
	for _, value := range []any{nil, int64(123), float64(1.25), []byte("alpha")} {
		expectDensityInvalid(t, db, "SELECT wa_doc_density_v1(?,'alpha')", value)
		expectDensityInvalid(t, db, "SELECT wa_doc_density_v1('alpha',?)", value)
	}
	for _, value := range [][]byte{
		{0x80}, {0xc0, 0xaf}, {0xc2}, {0xe0, 0x80, 0x80}, {0xed, 0xa0, 0x80},
		{0xf0, 0x80, 0x80, 0x80}, {0xf4, 0x90, 0x80, 0x80}, {0xf5, 0x80, 0x80, 0x80},
		{'a', 0, 0xff},
		[]byte(strings.Repeat("a", 4095) + "\x00" + string([]byte{0xff})),
	} {
		expectDensityInvalid(t, db, "SELECT wa_doc_density_v1(CAST(? AS TEXT),'a')", value)
		expectDensityInvalid(t, db, "SELECT wa_doc_density_v1('alpha',CAST(? AS TEXT))", value)
	}
	expectDensityInvalid(t, db, "SELECT wa_doc_density_v1(?,'a')", strings.Repeat("a", 4*(256<<10)+2))
	expectDensityInvalid(t, db, "SELECT wa_doc_density_v1('alpha',?)", strings.Repeat("a", 129))
	expectDensityInvalid(t, db, "SELECT wa_doc_density_v1('alpha',?)", strings.Repeat("𐐀", 129))
	expectDensityInvalid(t, db, "SELECT wa_doc_density_v1('alpha','')")
	expectDensityInvalid(t, db, "SELECT wa_doc_density_v1('alpha',?)", "al\x00pha")
	expectDensityInvalid(t, db, "SELECT wa_doc_density_v1()")
	expectDensityInvalid(t, db, "SELECT wa_doc_density_v1('alpha')")
	args := []any{"alpha"}
	for i := 0; i < 33; i++ {
		args = append(args, "alpha")
	}
	expectDensityInvalid(t, db, "SELECT "+densityCall("?", 33), args...)
}

func TestDensityLazyCaseSkipsDeniedMalformedInputs(t *testing.T) {
	db := densityDB(t)
	if _, err := db.Exec("CREATE TABLE inputs(id INTEGER PRIMARY KEY,authorized INTEGER,text)"); err != nil {
		t.Fatal(err)
	}
	values := []any{nil, int64(123), []byte("alpha"), string([]byte{'a', 0, 0xff}), strings.Repeat("a", 4*(256<<10)+2)}
	for i, value := range values {
		if _, err := db.Exec("INSERT INTO inputs VALUES(?,0,?)", i+1, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO inputs VALUES(100,1,'alpha')"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT id,CASE WHEN authorized=1 THEN wa_doc_density_v1(text,'alpha') END FROM inputs ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var id int
		var score sql.NullFloat64
		if err = rows.Scan(&id, &score); err != nil {
			t.Fatal(err)
		}
		if id == 100 {
			if !score.Valid || math.Float64bits(score.Float64) != math.Float64bits(0.2) {
				t.Fatalf("authorized score differs: %v", score)
			}
		} else if score.Valid {
			t.Fatal("denied branch returned a score")
		}
		seen++
	}
	if err = rows.Err(); err != nil || seen != len(values)+1 {
		t.Fatalf("denied validation affected result: seen=%d err=%v", seen, err)
	}
}

func TestDensityDirectOnlyAndMultipleReadOnlyConnectionsReopen(t *testing.T) {
	if err := Register(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "lexical.sqlite")
	for round := 0; round < 2; round++ {
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(3)
		connections := make([]*sql.Conn, 0, 3)
		for i := 0; i < 3; i++ {
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			connections = append(connections, conn)
			if _, err = conn.ExecContext(context.Background(), "PRAGMA query_only=ON"); err != nil {
				t.Fatal(err)
			}
			compareDensityOracle(t, conn, "alpha alpha", []string{"alpha"})
			var queryOnly int
			if err = conn.QueryRowContext(context.Background(), "PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
				t.Fatalf("scoring changed read-only connection: %d %v", queryOnly, err)
			}
			if _, err = conn.ExecContext(context.Background(), "CREATE TABLE prohibited(a)"); err == nil {
				t.Fatal("read-only connection wrote after scoring")
			}
		}
		for _, conn := range connections {
			conn.Close()
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	db := densityDB(t)
	if _, err := db.Exec("CREATE VIEW prohibited AS SELECT wa_doc_density_v1('alpha','alpha')"); err != nil {
		t.Fatal(err)
	}
	var score float64
	if err := db.QueryRow("SELECT * FROM prohibited").Scan(&score); err == nil {
		t.Fatal("DIRECTONLY scorer ran inside a view")
	}
}

func TestDensityCancellationAndConnectionReuse(t *testing.T) {
	db := densityDB(t)
	for _, tc := range []struct {
		name, text string
		terms      []string
		arity      int
	}{
		{"long_kmp", strings.Repeat("a", 4*(256<<10)+1), []string{strings.Repeat("a", 127) + "b"}, 32},
		{"short_repetitive_hit_and_no_hit", strings.Repeat("a", 8193), []string{"aaaaaaab", "aaaaaaaa"}, 32},
		{"single_long_prefix_fallback", strings.Repeat("a", 4*(256<<10)+1), []string{strings.Repeat("a", 127) + "b"}, 1},
		{"single_valid_suffix_after_nul", "a\x00" + strings.Repeat("a", 4*(256<<10)-1), []string{"a"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []any{tc.text}
			for i := 0; i < tc.arity; i++ {
				args = append(args, tc.terms[i%len(tc.terms)])
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timer := time.AfterFunc(2*time.Millisecond, cancel)
			defer timer.Stop()
			// One bounded worst-case scalar is repeated by SQLite; cancellation must
			// discard the query, not publish a partly scanned density, and release it.
			query := "WITH RECURSIVE work(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM work WHERE n<10000) SELECT sum(" + densityCall("? || substr('',1,n%2)", tc.arity) + ") FROM work"
			var score float64
			err := db.QueryRowContext(ctx, query, args...).Scan(&score)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation returned a score or wrong error: %v", err)
			}
			compareDensityOracle(t, db, "fresh alpha", []string{"alpha"})
		})
	}
}
