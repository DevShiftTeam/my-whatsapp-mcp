package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxSnippetRunes = 320

const maxLexicalOriginalBytes = 4*maxEventTextBytes + 1
const maxLexicalDocumentBytes = 4 * (64 << 10)

// FTS5 BM25 includes denied corpus statistics. This exact local scorer sees
// only its supplied text and literal terms. Lazy CASE must gate every native
// validation/error path with complete authority, independent of planner order.
func localRank(expression, text, validInput, authority string, authorityArgs []any) (string, []any) {
	terms := strings.Split(expression, " AND ")
	marks := make([]string, 0, len(terms))
	args := append([]any(nil), authorityArgs...)
	for _, quoted := range terms {
		term := strings.TrimSuffix(strings.TrimPrefix(quoted, `"`), `"`)
		marks = append(marks, "?")
		args = append(args, term)
	}
	return `CASE WHEN (` + authority + `) THEN wa_doc_density_v1(CASE WHEN (` + validInput + `) THEN ` + text + ` ELSE NULL END,` + strings.Join(marks, ",") + `) END`, args
}

// Native token relevance uses only the current FTS row's phrase positions and
// indexed token length. Preserve complete lazy authority before any validation
// or FTS API call, including invalid authorized rows below the returned top-K.
func tokenRank(table string, fields []string, validInput, authority string, authorityArgs []any) (string, []any) {
	invalid := make([]string, len(fields))
	for n := range invalid {
		invalid[n] = "NULL"
	}
	call := `wa_doc_token_rank_v1(` + table + `,` + strings.Join(fields, ",") + `)`
	failure := `wa_doc_token_rank_v1(` + table + `,` + strings.Join(invalid, ",") + `)`
	return `CASE WHEN (` + authority + `) THEN CASE WHEN (` + validInput + `) THEN ` + call + ` ELSE ` + failure + ` END END`, append([]any(nil), authorityArgs...)
}

func (f *readFrame) originalRank(expression string) (string, []any) {
	authority := `c.id=m.chat_id AND c.account=m.account AND i.id=m.sender_id AND i.account=m.account
 AND saf.account=m.account AND saf.unsafe=0 AND ap.id=1 AND message_fts.rowid=m.rowid AND (` + f.where + `)`
	valid := `typeof(m.normalized_body)='text' AND typeof(m.normalized_caption)='text'
 AND length(CAST(m.normalized_body AS BLOB))+length(CAST(m.normalized_caption AS BLOB))+1<=` + strconv.Itoa(maxLexicalOriginalBytes)
	return tokenRank("message_fts", []string{"m.normalized_body", "m.normalized_caption"}, valid, authority, f.args)
}

func (f *readFrame) rootRank(expression string, anchor bool) (string, []any) {
	authority := `extension_root_fts.rowid=r.rowid AND extension_root_fts.root_id=r.id AND (` + f.extensionRootPredicate() + `)`
	if anchor {
		authority += ` AND anchor.root_id=r.id AND anchor.ordinal=0 AND anchor_message.id=anchor.message_id AND anchor_message.account=anchor.account`
	}
	valid := `typeof(extension_root_fts.text)='text' AND length(CAST(extension_root_fts.text AS BLOB))<=` + strconv.Itoa(maxLexicalDocumentBytes)
	return tokenRank("extension_root_fts", []string{"extension_root_fts.text"}, valid, authority, f.args)
}

func (f *readFrame) documentRank(expression string, generation IndexGeneration) (string, []any) {
	authority := `d.generation=? AND d.kind='text_window' AND extension_document_fts.rowid=d.rowid AND (` + f.extensionDocumentPredicate() + `)`
	args := append([]any{generation.ID}, f.args...)
	valid := `typeof(d.normalized_text)='text' AND length(CAST(d.normalized_text AS BLOB))<=` + strconv.Itoa(maxLexicalDocumentBytes)
	return tokenRank("extension_document_fts", []string{"d.normalized_text"}, valid, authority, args)
}

// Original text remains in logical order; normalized content is used solely
// by FTS. Returning original excerpts avoids changing niqqud, quotes, or names.
func originalSnippet(body, caption string) string {
	text := body
	if text == "" {
		text = caption
	} else if caption != "" {
		text += "\n" + caption
	}
	if utf8.RuneCountInString(text) <= maxSnippetRunes {
		return text
	}
	runes := []rune(text)
	return string(runes[:maxSnippetRunes])
}

type searchSelection struct {
	id    string
	root  bool
	kind  DocumentKind
	count int
	score float64
}

// Search materializes one policy row and one granted-account safety row.
// Mode projections retain only their exact equality results; numeric policy
// fields were already decoded by beginRead. All candidate authority remains
// in the original JOINs, WHERE and
// lazy scorer CASE; complete source/root verification uses its current tables.
const searchAuthorizedTables = `messages m JOIN chats c ON c.id=m.chat_id AND c.account=m.account
 JOIN identities i ON i.id=m.sender_id AND i.account=m.account
 JOIN search_safety saf ON saf.account=m.account AND saf.unsafe=0
 JOIN search_policy ap ON ap.id=1`

// Merge both keyword corpora while each branch still has its complete scope
// predicate. The extra metadata row proves pagination without loading or
// pinning a source that contributes no published plaintext.
func (f *readFrame) searchSelections(expression string, limit, offset int) ([]searchSelection, error) {
	rank, rankArgs := f.originalRank(expression)
	rootRank, rootRankArgs := f.rootRank(expression, true)
	args := append([]any{f.scope.Account}, rankArgs...)
	args = append(args, f.args...)
	args = append(args, expression)
	args = append(args, rootRankArgs...)
	args = append(args, f.args...)
	args = append(args, expression, limit+1, offset)
	rows, err := f.tx.QueryContext(f.ctx, `WITH search_policy AS MATERIALIZED (
 SELECT id,generation,CASE WHEN date_mode='all_available' THEN 'all_available' ELSE '' END AS date_mode,
 import_start,CASE WHEN chat_mode='all' THEN 'all' ELSE '' END AS chat_mode FROM archive_policy WHERE id=1
 ),search_safety AS MATERIALIZED (
 SELECT account,unsafe FROM account_safety WHERE account=? AND unsafe=0
 )
 SELECT m.id AS id,0 AS is_root,'' AS kind,1 AS source_count,m.original_sent_at AS sent,`+rank+` AS lexical_score
 FROM `+searchAuthorizedTables+` JOIN message_fts ON message_fts.rowid=m.rowid
 WHERE `+f.where+` AND message_fts MATCH ?
 UNION ALL
 SELECT r.id,1,r.kind,r.source_count,anchor_message.original_sent_at,`+rootRank+`
 FROM extension_roots r JOIN extension_root_fts ON extension_root_fts.rowid=r.rowid AND extension_root_fts.root_id=r.id
 JOIN extension_root_sources anchor ON anchor.root_id=r.id AND anchor.ordinal=0
 JOIN messages anchor_message ON anchor_message.id=anchor.message_id AND anchor_message.account=anchor.account
 WHERE `+f.extensionRootPredicate()+` AND extension_root_fts MATCH ?
 ORDER BY lexical_score DESC,sent DESC,id,is_root LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	defer rows.Close()
	selected := make([]searchSelection, 0, limit+1)
	for rows.Next() {
		var row searchSelection
		var sent int64
		if err = rows.Scan(&row.id, &row.root, &row.kind, &row.count, &sent, &row.score); err != nil {
			return nil, readError(f.ctx, err)
		}
		selected = append(selected, row)
	}
	if err = rows.Err(); err != nil {
		return nil, readError(f.ctx, err)
	}
	return selected, nil
}

// Hydrate only displayed originals. A single bounded authorized query keeps
// original text/name/source pins in the selection transaction; metadata order
// and scores come from selected, never from the database's IN-result order.
func (f *readFrame) originalSearchHits(selected []searchSelection) (map[MessageID]SearchHit, error) {
	if len(selected) > 100 {
		return nil, ErrLimitExceeded
	}
	scores := make(map[MessageID]float64, len(selected))
	marks := make([]string, 0, len(selected))
	args := append([]any(nil), f.args...)
	for _, row := range selected {
		if row.root {
			continue
		}
		id := MessageID(row.id)
		if row.count != 1 || !validReadID(row.id) {
			return nil, ErrSourceUnavailable
		}
		if _, duplicate := scores[id]; duplicate {
			return nil, ErrSourceUnavailable
		}
		scores[id] = row.score
		marks = append(marks, "?")
		args = append(args, id)
	}
	hits := make(map[MessageID]SearchHit, len(scores))
	if len(scores) == 0 {
		return hits, nil
	}
	rows, err := f.tx.QueryContext(f.ctx, `SELECT m.id,m.chat_id,substr(c.name,1,256),substr(i.name,1,256),m.original_sent_at,m.body_kind,
 substr(m.body,1,4096),substr(m.caption,1,4096),m.expires_at,m.local_revision,m.account,m.content_hash,m.sender_id
 FROM `+authorizedTables+` WHERE `+f.where+` AND m.id IN (`+strings.Join(marks, ",")+`)`, args...)
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var hit SearchHit
		var sent, expiry int64
		var revision uint64
		var body, caption, account, hash, sender string
		if err = rows.Scan(&hit.MessageID, &hit.ChatID, &hit.ChatName, &hit.SenderName, &sent, &hit.Kind, &body, &caption, &expiry, &revision, &account, &hash, &sender); err != nil {
			return nil, readError(f.ctx, err)
		}
		score, expected := scores[hit.MessageID]
		if _, duplicate := hits[hit.MessageID]; duplicate || !expected {
			return nil, ErrSourceUnavailable
		}
		hit.Score, hit.SentAt = score, fromMillis(sent)
		hit.ChatName, hit.SenderName = boundedOriginal(hit.ChatName, 256), boundedOriginal(hit.SenderName, 256)
		hit.Snippet = originalSnippet(body, caption)
		f.note(hit.SentAt, fromMillis(expiry))
		f.pin(hit.MessageID, revision)
		source := SourceReadVersion{Ref: SourceRef{Account: account, Version: SourceVersion{MessageID: hit.MessageID, Revision: revision, PolicyGeneration: f.snapshot.PolicyGeneration, ExpiresAt: fromMillis(expiry)}, ContentHash: hash}, ChatID: hit.ChatID, SenderID: sender, OriginalSentAt: hit.SentAt, ExpiresAt: fromMillis(expiry)}
		if err = f.captureSource(source, hit.ChatName, hit.SenderName); err != nil {
			return nil, err
		}
		hits[hit.MessageID] = hit
	}
	if err = rows.Err(); err != nil {
		return nil, readError(f.ctx, err)
	}
	if len(hits) != len(scores) {
		return nil, ErrSourceUnavailable
	}
	return hits, nil
}

func (f *readFrame) rootSearchHit(selected searchSelection) (SearchHit, error) {
	ref, err := f.extensionRootRef(DocumentID(selected.id), selected.kind)
	if err != nil {
		return SearchHit{}, err
	}
	if len(ref.ReadSources) != selected.count {
		return SearchHit{}, ErrSourceUnavailable
	}
	root, err := f.s.extensionLoadRoot(f.ctx, f.tx, ref.ID)
	if err != nil {
		return SearchHit{}, err
	}
	if root.Kind != ref.Kind || !extensionSameRootMembers(root.Sources, ref.Sources) {
		return SearchHit{}, ErrSourceUnavailable
	}
	anchor := ref.ReadSources[0]
	hit := SearchHit{DocumentID: ref.ID, MessageID: anchor.Ref.Version.MessageID, ChatID: anchor.ChatID, SentAt: anchor.OriginalSentAt, Score: selected.score, Snippet: originalSnippet(root.Text, "")}
	args := append(append([]any(nil), f.args...), hit.MessageID)
	err = f.tx.QueryRowContext(f.ctx, `SELECT m.body_kind,substr(c.name,1,256),substr(i.name,1,256) FROM `+authorizedTables+` WHERE `+f.where+` AND m.id=?`, args...).Scan(&hit.Kind, &hit.ChatName, &hit.SenderName)
	if errors.Is(err, sql.ErrNoRows) {
		return SearchHit{}, ErrSourceUnavailable
	}
	if err != nil {
		return SearchHit{}, readError(f.ctx, err)
	}
	hit.ChatName, hit.SenderName = boundedOriginal(hit.ChatName, 256), boundedOriginal(hit.SenderName, 256)
	for _, source := range ref.ReadSources {
		hit.Sources = append(hit.Sources, source.Ref.Version)
	}
	hit.Derived = []DerivedExcerpt{{DocumentID: ref.ID, Source: anchor.Ref.Version, Text: root.Text, Provenance: root.Provenance}}
	return hit, nil
}

// Preserve complete provenance and member metadata while assigning the wire
// budget to excerpts in result order. JSON escaping counts toward the ceiling.
func boundSearch(page *SearchPage) error {
	snippets := make([]string, len(page.Hits))
	generated := make([][]string, len(page.Hits))
	for i := range page.Hits {
		hit := &page.Hits[i]
		snippets[i], hit.Snippet = hit.Snippet, ""
		generated[i] = make([]string, len(hit.Derived))
		for j := range hit.Derived {
			generated[i][j], hit.Derived[j].Text = hit.Derived[j].Text, ""
		}
	}
	metadata, err := json.Marshal(page)
	if err != nil || len(metadata) > maxContextBytes {
		return ErrLimitExceeded
	}
	remaining := maxContextBytes - len(metadata)
	for i := range page.Hits {
		hit := &page.Hits[i]
		hit.Snippet = reserveJSONText(snippets[i], &remaining)
		for j := range hit.Derived {
			hit.Derived[j].Text = reserveJSONText(generated[i][j], &remaining)
		}
	}
	return nil
}

func (s *Store) Search(ctx context.Context, scope ReadScope, request SearchRequest) (SearchPage, error) {
	return s.searchPage(ctx, scope, request, nil)
}
func (s *Store) SearchGuarded(ctx context.Context, scope ReadScope, request SearchRequest) (SearchPage, ResponseGuard, error) {
	var guard ResponseGuard
	page, err := s.searchPage(ctx, scope, request, &guard)
	if err != nil {
		return SearchPage{}, ResponseGuard{}, err
	}
	return page, guard, nil
}
func (s *Store) searchPage(ctx context.Context, scope ReadScope, request SearchRequest, guardOut *ResponseGuard) (SearchPage, error) {
	expression, err := literalQuery(request.Query)
	if err != nil {
		return SearchPage{}, err
	}
	limit, err := readLimit(request.Limit)
	if err != nil {
		return SearchPage{}, err
	}
	f, err := s.beginRead(ctx, scope, request.Chats, request.From, request.Until)
	if err != nil {
		return SearchPage{}, err
	}
	defer f.close()
	f.captureSources = guardOut != nil
	horizon, expiry, err := f.cursorHorizon()
	if err != nil {
		return SearchPage{}, err
	}
	chats, err := canonicalChats(request.Chats)
	if err != nil {
		return SearchPage{}, err
	}
	identity := cursorIdentity("search", f.scope, struct {
		Expression     string
		Chats          []ChatID
		Limit          int
		RollingHorizon time.Time
	}{expression, chats, limit, horizon}, f.snapshot, f.fixedFrom, f.until, expiry)
	offset := 0
	if request.Cursor != "" {
		offset, err = decodeCursor(s.cursorKey, request.Cursor, identity)
		if err != nil {
			return SearchPage{}, err
		}
	}
	selected, err := f.searchSelections(expression, limit, offset)
	if err != nil {
		return SearchPage{}, err
	}
	page := SearchPage{Hits: make([]SearchHit, 0, limit), Snapshot: f.snapshot}
	if len(selected) > limit {
		selected = selected[:limit]
		page.NextCursor, err = encodeCursor(s.cursorKey, identity, offset+limit)
		if err != nil {
			return SearchPage{}, err
		}
	}
	members := f.refSourceCopies
	for _, row := range selected {
		if row.count < 1 || row.count > maxResponseSources || row.count > maxResponseSources-members {
			return SearchPage{}, ErrLimitExceeded
		}
		members += row.count
	}
	originals, err := f.originalSearchHits(selected)
	if err != nil {
		return SearchPage{}, err
	}
	for _, row := range selected {
		var hit SearchHit
		if row.root {
			hit, err = f.rootSearchHit(row)
		} else {
			hit = originals[MessageID(row.id)]
		}
		if err != nil {
			return SearchPage{}, err
		}
		page.Hits = append(page.Hits, hit)
	}
	if err = boundSearch(&page); err != nil {
		return SearchPage{}, err
	}
	if guardOut != nil {
		*guardOut, err = f.responseGuard()
		if err != nil {
			return SearchPage{}, err
		}
	}
	if err = f.finish(); err != nil {
		return SearchPage{}, err
	}
	return page, nil
}
