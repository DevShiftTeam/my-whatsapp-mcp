//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This is the previously shipped SQL scoring algorithm, retained only as an
// independent SQLite oracle for the native replacement and ordered pages.
func searchScoreSQLOracle(expression, text string) (string, []any) {
	pieces := []string{}
	args := []any{}
	for _, quoted := range strings.Split(expression, " AND ") {
		term := strings.TrimSuffix(strings.TrimPrefix(quoted, `"`), `"`)
		pieces = append(pieces, `((length(`+text+`)-length(replace(`+text+`,?,'')))*1.0/max(1,length(?)))`)
		args = append(args, term, term)
	}
	return "(" + strings.Join(pieces, "+") + ")/max(1,length(" + text + "))", args
}

// Independent fixture oracle: FTS5's instance vocabulary exposes indexed
// tokens/positions. These fixture query phrases each tokenize to exactly one
// term. Count matching instances and all row-local indexed tokens in SQLite;
// do not invoke the native auxiliary or use corpus statistics for expectations.
func searchTokenScoreVocabulary(t *testing.T, s *Store, expression string) {
	t.Helper()
	for _, statement := range []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS search_score_message_vocab USING fts5vocab(message_fts,'instance')`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS search_score_root_vocab USING fts5vocab(extension_root_fts,'instance')`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS search_score_query_fts USING fts5(text,tokenize='unicode61 remove_diacritics 2')`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS search_score_query_vocab USING fts5vocab(search_score_query_fts,'instance')`,
		`DELETE FROM search_score_query_fts`,
	} {
		if _, err := s.writeDB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	terms := strings.Split(expression, " AND ")
	text := make([]string, len(terms))
	for n, term := range terms {
		text[n] = strings.TrimSuffix(strings.TrimPrefix(term, `"`), `"`)
	}
	if _, err := s.writeDB.Exec(`INSERT INTO search_score_query_fts(rowid,text) VALUES(1,?)`, strings.Join(text, " ")); err != nil {
		t.Fatal(err)
	}
	var tokens int
	if err := s.writeDB.QueryRow(`SELECT count(*) FROM search_score_query_vocab WHERE doc=1`).Scan(&tokens); err != nil || tokens != len(terms) {
		t.Fatalf("fixture phrase is not one indexed token: %d/%d %v", tokens, len(terms), err)
	}
}

func searchTokenScoreOracle(vocabulary, rowid string) string {
	return `(SELECT count(*)*1.0 FROM ` + vocabulary + ` v JOIN search_score_query_vocab q ON q.term=v.term AND q.doc=1 WHERE v.doc=` + rowid + `)/max(1,(SELECT count(*) FROM ` + vocabulary + ` v WHERE v.doc=` + rowid + `))`
}

func searchScoreExpectedSelections(t *testing.T, s *Store, scope ReadScope, query string) []searchSelection {
	t.Helper()
	expression, err := literalQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	searchTokenScoreVocabulary(t, s, expression)
	f, err := s.beginRead(context.Background(), scope, nil, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	rank := searchTokenScoreOracle("search_score_message_vocab", "m.rowid")
	rootRank := searchTokenScoreOracle("search_score_root_vocab", "r.rowid")
	args := append(append([]any(nil), f.args...), expression)
	args = append(args, f.args...)
	args = append(args, expression)
	rows, err := f.tx.QueryContext(f.ctx, `SELECT m.id AS id,0 AS is_root,'' AS kind,1 AS source_count,m.original_sent_at AS sent,`+rank+` AS lexical_score
 FROM `+authorizedTables+` JOIN message_fts ON message_fts.rowid=m.rowid
 WHERE `+f.where+` AND message_fts MATCH ?
 UNION ALL SELECT r.id,1,r.kind,r.source_count,anchor_message.original_sent_at,`+rootRank+`
 FROM extension_roots r JOIN extension_root_fts ON extension_root_fts.rowid=r.rowid AND extension_root_fts.root_id=r.id
 JOIN extension_root_sources anchor ON anchor.root_id=r.id AND anchor.ordinal=0
 JOIN messages anchor_message ON anchor_message.id=anchor.message_id AND anchor_message.account=anchor.account
 WHERE `+f.extensionRootPredicate()+` AND extension_root_fts MATCH ?
 ORDER BY lexical_score DESC,sent DESC,id,is_root`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []searchSelection
	for rows.Next() {
		var row searchSelection
		var sent int64
		if err = rows.Scan(&row.id, &row.root, &row.kind, &row.count, &sent, &row.score); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

// Cursor ciphertext has a fresh nonce on each valid page. Authenticate both
// cursors against the exact fixture authority and pagination view before
// comparing all non-ciphertext fields; never normalize an unchecked cursor.
func searchScoreAssertEquivalentCursors(t *testing.T, s *Store, scope ReadScope, request SearchRequest, before, after SearchPage) {
	t.Helper()
	if request.Limit != 1 || before.NextCursor == "" || after.NextCursor == "" {
		t.Fatal("privacy fixture must retain two nonempty Limit1 cursors")
	}
	expression, err := literalQuery(request.Query)
	if err != nil {
		t.Fatal(err)
	}
	chats, err := canonicalChats(request.Chats)
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.beginRead(context.Background(), scope, request.Chats, request.From, request.Until)
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	horizon, expiry, err := f.cursorHorizon()
	if err != nil {
		t.Fatal(err)
	}
	if before.Snapshot != f.snapshot || after.Snapshot != f.snapshot {
		t.Fatal("denied storage mutation changed authorized snapshot")
	}
	view := struct {
		Expression     string
		Chats          []ChatID
		Limit          int
		RollingHorizon time.Time
	}{expression, chats, request.Limit, horizon}
	identity := cursorIdentity("search", f.scope, view, f.snapshot, f.fixedFrom, f.until, expiry)
	other := f.scope
	other.Principal += "-other"
	otherIdentity := cursorIdentity("search", other, view, f.snapshot, f.fixedFrom, f.until, expiry)
	for _, cursor := range []string{before.NextCursor, after.NextCursor} {
		offset, err := decodeCursor(s.cursorKey, cursor, identity)
		if err != nil || offset != 1 {
			t.Fatalf("cursor lost exact authorized pagination: offset%d %v", offset, err)
		}
		if _, err = decodeCursor(s.cursorKey, cursor, otherIdentity); !errors.Is(err, ErrCursorStale) {
			t.Fatalf("cursor accepted another principal: %v", err)
		}
	}
}

func TestSearchTokenScoreExactMixedPagesAndCursor(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	bodies := []struct{ body, caption string }{
		{"alpha", ""}, {"alpha alpha", ""}, {"alphabet alpha", "alpha"},
		{"abababa aba", ""}, {"ALPHA cafe café", ""}, {"alpha\x00alpha", ""},
		{"\x00alpha", ""}, {"שָׁלוֹם alpha ד״ר 😀", "שלום alpha"}, {"", "alpha"},
		{"alpha 😀 https://example.test/alpha", "alpha"},
	}
	var anchor MessageID
	for n, input := range bodies {
		event := readerEvent(fmt.Sprintf("parity-%d", n), "allowed", input.body, now.Add(-time.Duration(n)*time.Minute))
		event.Caption = input.caption
		id := readerApply(t, s, p, event)
		if n == 0 {
			anchor = id
		}
	}
	candidateRoot(t, s, p, "score-ocr", "alpha", []MessageID{anchor}, derivedContextProvenance(DocumentOCR))
	candidateRoot(t, s, p, "score-caption", "alpha alpha 😀", []MessageID{anchor}, derivedContextProvenance(DocumentCaption))
	scope := readerScope(p, readerChat(t, s, anchor))
	for _, query := range []string{"alpha", "alpha alpha", "aba", "שלום alpha", "cafe", "café", "alpha https"} {
		t.Run(query, func(t *testing.T) {
			expected := searchScoreExpectedSelections(t, s, scope, query)
			request := SearchRequest{Query: query, Limit: 2}
			var actual []SearchHit
			for pages := 0; ; pages++ {
				if pages > len(expected)+1 {
					t.Fatal("cursor did not terminate")
				}
				page, guard, err := s.SearchGuarded(context.Background(), scope, request)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
					t.Fatal(err)
				}
				actual = append(actual, page.Hits...)
				if page.NextCursor == "" {
					break
				}
				request.Cursor = page.NextCursor
			}
			if len(actual) != len(expected) {
				t.Fatalf("result membership got%d want%d", len(actual), len(expected))
			}
			for n, hit := range actual {
				id := string(hit.MessageID)
				if hit.DocumentID != "" {
					id = string(hit.DocumentID)
				}
				if id != expected[n].id || (hit.DocumentID != "") != expected[n].root || math.Float64bits(hit.Score) != math.Float64bits(expected[n].score) {
					t.Fatalf("native score/page ordering differs at%d: got %#v want %#v", n, hit, expected[n])
				}
			}
		})
	}
}

// Native validation must never be reached for denied rows, even when FTS has
// a matching posting and the associated scoring content is malformed.
func TestSearchNativeScoreDeniedMalformedCorpusDoesNotAffectAllowed(t *testing.T) {
	cases := []struct {
		name     string
		value    any
		rootOnly bool
	}{
		{"blob", []byte("needle"), false},
		{"invalid_utf8", string([]byte{'n', 'e', 'e', 'd', 'l', 'e', 0xff}), false},
		{"oversize", "needle" + strings.Repeat("x", 4*maxEventTextBytes+1), false},
		{"integer", int64(42), true}, {"null", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			allowed := readerApply(t, s, p, readerEvent("allowed", "allowed", "needle", now))
			denied := readerApply(t, s, p, readerEvent("denied", "denied", "needle needle", now))
			for n := 0; n < 101; n++ {
				readerApply(t, s, p, readerEvent(fmt.Sprintf("higher-%d", n), "allowed", "needle", now.Add(-time.Duration(n+1)*time.Second)))
			}
			candidateRoot(t, s, p, "allowed-score-root", "needle", []MessageID{allowed}, derivedContextProvenance(DocumentOCR))
			candidateRoot(t, s, p, "denied-score-root", "needle", []MessageID{allowed, denied}, derivedContextProvenance(DocumentCaption))
			generation := candidateIndex(t, s)
			derivedContextDocument(t, s, p, generation, "allowed-score-window", "needle", DocumentWindow, []MessageID{allowed}, DerivedProvenance{Kind: DocumentWindow, OffsetKind: "none"})
			derivedContextDocument(t, s, p, generation, "denied-score-window", "needle", DocumentWindow, []MessageID{allowed, denied}, DerivedProvenance{Kind: DocumentWindow, OffsetKind: "none"})
			scope := readerScope(p, readerChat(t, s, allowed))
			request := SearchRequest{Query: "needle", Limit: 1}
			baseline, guard, err := s.SearchGuarded(context.Background(), scope, request)
			if err != nil || len(baseline.Hits) != 1 {
				t.Fatalf("baseline %#v %v", baseline, err)
			}
			pairBefore, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: request, KeywordOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if !tc.rootOnly {
				if _, err = s.writeDB.Exec(`UPDATE messages SET normalized_body=? WHERE id=?`, tc.value, denied); err != nil {
					t.Fatal(err)
				}
			}
			// The shadow-content-only mutation intentionally preserves the matching
			// FTS posting while corrupting its input. It is fixture-only corruption.
			if _, err = s.writeDB.Exec(`UPDATE extension_root_fts_content SET c1=? WHERE id=(SELECT rowid FROM extension_roots WHERE id='denied-score-root')`, tc.value); err != nil {
				t.Fatal(err)
			}
			if !tc.rootOnly {
				if _, err = s.writeDB.Exec(`UPDATE extension_documents SET normalized_text=? WHERE id='denied-score-window'`, tc.value); err != nil {
					t.Fatal(err)
				}
			}
			after, afterGuard, err := s.SearchGuarded(context.Background(), scope, request)
			if err != nil {
				t.Fatalf("denied native input caused authorized error: %v", err)
			}
			searchScoreAssertEquivalentCursors(t, s, scope, request, baseline, after)
			beforeContent, afterContent := baseline, after
			beforeContent.NextCursor, afterContent.NextCursor = "", ""
			if !reflect.DeepEqual(afterContent, beforeContent) || !reflect.DeepEqual(afterGuard, guard) {
				t.Fatalf("denied native input affected authorized content or guard: %#v", afterContent)
			}
			pairAfter, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: request, KeywordOnly: true})
			if err != nil || !reflect.DeepEqual(pairAfter, pairBefore) {
				t.Fatalf("denied native input affected candidate branch: %#v %v", pairAfter, err)
			}
			if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: afterGuard}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSearchNativeScoreRegisteredOnEveryReadConnectionAndReopen(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	options := Options{Now: func() time.Time { return now }, ReadConnections: 4}
	s, err := Open(context.Background(), dir, options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	p, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	id := readerApply(t, s, p, readerEvent("reopened-score", "chat", "alpha alpha", now))
	scope := readerScope(p, readerChat(t, s, id))
	before, err := s.Search(context.Background(), scope, SearchRequest{Query: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	connections := []*sql.Conn{}
	for n := 0; n < 4; n++ {
		c, e := s.readDB.Conn(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		connections = append(connections, c)
		defer c.Close()
	}
	for _, c := range connections {
		var score float64
		var queryOnly int
		if err = c.QueryRowContext(context.Background(), `SELECT wa_doc_density_v1('alpha alpha','alpha')`).Scan(&score); err != nil || math.Float64bits(score) != math.Float64bits(2.0/11.0) {
			t.Fatalf("new pooled connection missing exact scorer: %v %g", err, score)
		}
		if err = c.QueryRowContext(context.Background(), `SELECT wa_doc_token_rank_v1(message_fts,m.normalized_body,m.normalized_caption) FROM message_fts JOIN messages m ON m.rowid=message_fts.rowid WHERE message_fts MATCH 'alpha'`).Scan(&score); err != nil || math.Float64bits(score) != math.Float64bits(1.0) {
			t.Fatalf("new pooled connection missing token scorer: %v %g", err, score)
		}
		if err = c.QueryRowContext(context.Background(), `PRAGMA query_only`).Scan(&queryOnly); err != nil || queryOnly != 1 {
			t.Fatalf("scorer registration weakened query_only: %v %d", err, queryOnly)
		}
		if _, err = c.ExecContext(context.Background(), `UPDATE messages SET body='unexpected'`); err == nil {
			t.Fatal("registered reader connection permitted write")
		}
	}
	for _, c := range connections {
		if err = c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(context.Background(), dir, options)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.Search(context.Background(), scope, SearchRequest{Query: "alpha"})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("reopen scorer changed result %#v %v", after, err)
	}
}

func TestSearchSelectedHydrationOrderingBudgetAndLookaheadGuard(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	var ids []MessageID
	for n := 0; n < 101; n++ {
		ids = append(ids, readerApply(t, s, p, readerEvent(fmt.Sprintf("selected-%d", n), "chat", "needle "+strings.Repeat("<&", 400), now.Add(-time.Duration(n)*time.Minute))))
	}
	scope := readerScope(p, readerChat(t, s, ids[0]))
	page, guard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "needle", Limit: 100})
	if err != nil || len(page.Hits) != 100 || page.NextCursor == "" || len(guard.Sources) != 100 {
		t.Fatalf("selected hydration/pagination %#v %v", page, err)
	}
	encoded, err := json.Marshal(page)
	if err != nil || len(encoded) > maxContextBytes {
		t.Fatalf("hydration lost exact escaped wire ceiling: %d %v", len(encoded), err)
	}
	for n, hit := range page.Hits {
		if hit.MessageID != ids[n] {
			t.Fatalf("batch did not restore selected order at%d %#v", n, hit)
		}
	}
	for _, source := range guard.Sources {
		if source.Ref.Version.MessageID == ids[100] {
			t.Fatal("undisplayed lookahead source entered guard")
		}
	}
	// Use a small returned page for the unchanged100ms final response guard;
	// the100-hit query above separately verifies maximum materialization bounds.
	small, smallGuard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "needle", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	edit := readerEvent("selected-100", "chat", "UNDISPLAYED changed text", now.Add(-100*time.Minute))
	edit.Kind, edit.SourceRevision = EventEditContent, 2
	readerApply(t, s, p, edit)
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: smallGuard}); err != nil {
		t.Fatalf("unpublished lookahead edit discarded selected source guard: %v", err)
	}
	body, _ := json.Marshal(small)
	if strings.Contains(string(body), "UNDISPLAYED") {
		t.Fatal("unpublished body leaked into selected page")
	}
	if _, err = s.Search(context.Background(), scope, SearchRequest{Query: "needle", Limit: 3, Cursor: small.NextCursor}); !errors.Is(err, ErrCursorStale) {
		t.Fatalf("lookahead membership change accepted old cursor: %v", err)
	}
}

func TestSearchNativeScoreAuthorizedMalformedInputsFailFixed(t *testing.T) {
	for _, kind := range []string{"original", "root", "window"} {
		for _, malformed := range []string{"blob", "invalid_utf8", "oversize", "integer", "null"} {
			if kind != "root" && (malformed == "integer" || malformed == "null") {
				continue
			}
			t.Run(kind+"/"+malformed, func(t *testing.T) {
				now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
				s, p := readerFixture(t, &now)
				id := readerApply(t, s, p, readerEvent("native-error", "chat", "needle needle", now))
				for n := 0; n < 101; n++ {
					readerApply(t, s, p, readerEvent(fmt.Sprintf("higher-valid-%d", n), "chat", "needle", now.Add(-time.Duration(n+1)*time.Second)))
				}
				scope := readerScope(p, readerChat(t, s, id))
				maximum := 4*maxEventTextBytes + 1
				if kind != "original" {
					maximum = 4 * (64 << 10)
				}
				var value any
				switch malformed {
				case "blob":
					value = []byte("needle")
				case "invalid_utf8":
					value = string([]byte{'n', 'e', 'e', 'd', 'l', 'e', 0xff})
				case "oversize":
					value = "needle" + strings.Repeat("x", maximum)
				case "integer":
					value = int64(42)
				case "null":
					value = nil
				}
				if kind == "root" {
					candidateRoot(t, s, p, "malformed-score-root", "needle", []MessageID{id}, derivedContextProvenance(DocumentOCR))
					if _, err := s.writeDB.Exec(`UPDATE extension_root_fts_content SET c1=? WHERE id=(SELECT rowid FROM extension_roots WHERE id='malformed-score-root')`, value); err != nil {
						t.Fatal(err)
					}
				} else if kind == "window" {
					generation := candidateIndex(t, s)
					derivedContextDocument(t, s, p, generation, "malformed-score-window", "needle", DocumentWindow, []MessageID{id}, DerivedProvenance{Kind: DocumentWindow, OffsetKind: "none"})
					if _, err := s.writeDB.Exec(`UPDATE extension_documents SET normalized_text=? WHERE id='malformed-score-window'`, value); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := s.writeDB.Exec(`UPDATE messages SET normalized_body=? WHERE id=?`, value, id); err != nil {
						t.Fatal(err)
					}
				}
				request := SearchRequest{Query: "needle", Limit: 1}
				if kind != "window" {
					page, guard, err := s.SearchGuarded(context.Background(), scope, request)
					if !errors.Is(err, ErrArchiveUnavailable) || len(page.Hits) != 0 || len(guard.Sources) != 0 {
						t.Fatalf("malformed authorized native input exposed partial result: %#v %v", page, err)
					}
				}
				pair, err := s.CandidatePair(context.Background(), scope, CandidatePairRequest{Search: request, KeywordOnly: true})
				if !errors.Is(err, ErrArchiveUnavailable) || len(pair.Keyword) != 0 || len(pair.Semantic) != 0 {
					t.Fatalf("malformed native candidate input exposed partial result: %#v %v", pair, err)
				}
			})
		}
	}
}

func TestSearchNativeScoreMaximumAcceptedNormalizationPreservesOriginal(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	// U+023A lowercases to a wider UTF8 encoding: a valid256KiB original
	// can exceed256KiB in the separate normalized field.
	body := "needle " + strings.Repeat("\u023a", (maxEventTextBytes-7)/2)
	event := readerEvent("maximum-unicode", "chat", body, now)
	event.Caption = "x"
	if len(event.Body)+len(event.Caption) != maxEventTextBytes {
		t.Fatal("fixture no longer reaches accepted ingress boundary")
	}
	id := readerApply(t, s, p, event)
	scope := readerScope(p, readerChat(t, s, id))
	query := strings.TrimSpace(strings.Repeat("needle ", 32))
	expected := searchScoreExpectedSelections(t, s, scope, query)
	page, guard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: query})
	if err != nil || len(expected) != 1 || len(page.Hits) != 1 {
		t.Fatalf("valid transformed maximum rejected: %#v %v", page, err)
	}
	if math.Float64bits(page.Hits[0].Score) != math.Float64bits(expected[0].score) || page.Hits[0].Snippet != originalSnippet(body, event.Caption) {
		t.Fatal("native scorer changed SQL score or logical original")
	}
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
		t.Fatal(err)
	}
}

// Token relevance is an intentional ranking change. Prove both directions
// against the retained SQLite density oracle rather than silently updating
// expected order, and retain the same public response/guard path.
func TestSearchTokenRelevanceIntentionallyDiffersFromDensity(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	a := readerApply(t, s, p, readerEvent("substring-dense", "chat", "alpha alphaa alphaa alphaa alphaa alphaa", now))
	b := readerApply(t, s, p, readerEvent("token-dense", "chat", "alpha zzzzzzzzzzzzzzzzzzzz", now.Add(-time.Second)))
	scope := readerScope(p, readerChat(t, s, a))
	page, guard, err := s.SearchGuarded(context.Background(), scope, SearchRequest{Query: "alpha", Limit: 2})
	if err != nil || len(page.Hits) != 2 || page.Hits[0].MessageID != b || page.Hits[1].MessageID != a {
		t.Fatalf("intentional token order: %#v %v", page, err)
	}
	if math.Float64bits(page.Hits[0].Score) != math.Float64bits(1.0/2.0) || math.Float64bits(page.Hits[1].Score) != math.Float64bits(1.0/6.0) {
		t.Fatal("token score is not local phrase instances/indexed token count")
	}
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
		t.Fatal(err)
	}
	expression, err := literalQuery("alpha")
	if err != nil {
		t.Fatal(err)
	}
	oracle, args := searchScoreSQLOracle(expression, `(normalized_body || ' ' || normalized_caption)`)
	var oldA, oldB float64
	if err = s.readDB.QueryRowContext(context.Background(), `SELECT `+oracle+` FROM messages WHERE id=?`, append(append([]any(nil), args...), a)...).Scan(&oldA); err != nil {
		t.Fatal(err)
	}
	if err = s.readDB.QueryRowContext(context.Background(), `SELECT `+oracle+` FROM messages WHERE id=?`, append(append([]any(nil), args...), b)...).Scan(&oldB); err != nil || oldA <= oldB {
		t.Fatalf("old density relevance reversal not established: %g/%g %v", oldA, oldB, err)
	}
}
