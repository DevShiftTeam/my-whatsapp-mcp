package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxResponseSources = 2048
	maxDerivedBytes    = 64 << 10
	responseDeadline   = 100 * time.Millisecond
)

var _ CandidateReader = (*Store)(nil)
var _ ActiveIndexReader = (*Store)(nil)

// ActiveIndex is a bounded, body-free planning read. Its result can change
// during query encoding; CandidatePair still checks the complete identity in
// its own authorized snapshot before selecting any candidate.
func (s *Store) ActiveIndex(ctx context.Context) (IndexGeneration, error) {
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	tx, err := s.readDB.BeginTx(bounded, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return IndexGeneration{}, readError(bounded, err)
	}
	defer tx.Rollback()
	generation, err := extensionActiveGeneration(bounded, tx)
	if err != nil {
		return IndexGeneration{}, err
	}
	if err = tx.Commit(); err != nil {
		return IndexGeneration{}, readError(bounded, err)
	}
	return generation, nil
}

// This identity describes static authority. In particular, it contains the
// rolling duration, never the effective cutoff at an earlier clock sample.
func extensionScopeIdentity(scope ReadScope) ([32]byte, error) {
	if scope.Account == "" || len(scope.Account) > 512 || !utf8.ValidString(scope.Account) || scope.Principal == "" || len(scope.Principal) > 256 || !utf8.ValidString(scope.Principal) || scope.Rolling < 0 || !validReadTime(scope.From) || !validReadTime(scope.Until) || (!scope.AllChats && len(scope.Chats) == 0) {
		return [32]byte{}, ErrUnauthorized
	}
	chats, err := canonicalChats(scope.Chats)
	if err != nil {
		return [32]byte{}, ErrUnauthorized
	}
	scope.Chats = chats
	scope.From, scope.Until = scope.From.UTC(), scope.Until.UTC()
	encoded, err := json.Marshal(scope)
	if err != nil {
		return [32]byte{}, ErrUnauthorized
	}
	return sha256.Sum256(encoded), nil
}

func extensionActiveGeneration(ctx context.Context, q sqlQuerier) (IndexGeneration, error) {
	var generation IndexGeneration
	var encoded string
	var bytes int
	// Guard the SQL payload before it crosses into Go. This also works for a
	// single-query caller without a transaction or a separate preflight race.
	err := q.QueryRowContext(ctx, `SELECT id,CASE WHEN length(CAST(version_json AS BLOB))<=4096 THEN version_json ELSE '' END,state,length(CAST(version_json AS BLOB)) FROM extension_generations WHERE state='active'`).Scan(&generation.ID, &encoded, &generation.State, &bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return generation, nil
	}
	if err != nil {
		return IndexGeneration{}, readError(ctx, err)
	}
	if bytes > 4096 || !validString(string(generation.ID), 256) || json.Unmarshal([]byte(encoded), &generation.Version) != nil || !extensionValidVersion(generation.Version) {
		return IndexGeneration{}, ErrArchiveUnavailable
	}
	return generation, nil
}

func extensionQueryVector(values []float32, dimensions int) ([]byte, error) {
	if dimensions < 1 || dimensions > 4096 || len(values) != dimensions {
		return nil, ErrInvalidQuery
	}
	var norm float64
	for _, value := range values {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, ErrInvalidQuery
		}
		norm += float64(value) * float64(value)
	}
	if norm == 0 || math.Abs(norm-1) > .01 {
		return nil, ErrInvalidQuery
	}
	encoded := make([]byte, dimensions*4)
	for i, value := range values {
		binary.LittleEndian.PutUint32(encoded[i*4:], math.Float32bits(value))
	}
	return encoded, nil
}

// Count plus anti-denial preserves the complete membership set. A join alone
// would quietly drop an inaccessible member and admit a partially visible
// document. MATERIALIZED eligible vectors below keeps top-k after this gate.
func (f *readFrame) extensionDocumentPredicate() string {
	return `d.embedding_input=0 AND d.source_count BETWEEN 1 AND 2048
 AND ` + extensionDocumentRootPredicate + `
 AND d.source_count=(SELECT count(*) FROM extension_document_sources ds WHERE ds.document_id=d.id)
 AND NOT EXISTS(SELECT 1 FROM extension_document_sources ds WHERE ds.document_id=d.id AND NOT EXISTS(
 SELECT 1 FROM ` + authorizedTables + ` WHERE ` + f.where + `
 AND m.account=ds.account AND m.id=ds.message_id AND m.local_revision=ds.local_revision
 AND m.content_hash=ds.content_hash AND ap.generation=ds.policy_generation AND m.expires_at=ds.expires_at))`
}

func scanExtensionSource(rows *sql.Rows, generation Generation) (SourceReadVersion, error) {
	var source SourceReadVersion
	var sent, expiry int64
	var chatName, senderName string
	if err := rows.Scan(&source.Ref.Account, &source.Ref.Version.MessageID, &source.Ref.Version.Revision, &source.Ref.ContentHash, &source.ChatID, &source.SenderID, &sent, &expiry, &chatName, &senderName); err != nil {
		return source, err
	}
	var err error
	source.MetadataHash, err = sourceMetadataHash(boundedOriginal(chatName, 256), boundedOriginal(senderName, 256))
	if err != nil {
		return SourceReadVersion{}, err
	}
	source.Ref.Version.PolicyGeneration = generation
	source.OriginalSentAt, source.ExpiresAt = fromMillis(sent), fromMillis(expiry)
	source.Ref.Version.ExpiresAt = source.ExpiresAt
	return source, nil
}

const extensionSourceColumns = `m.account,m.id,m.local_revision,m.content_hash,m.chat_id,m.sender_id,m.original_sent_at,m.expires_at,substr(c.name,1,256),substr(i.name,1,256)`

func (f *readFrame) extensionDocumentRef(id DocumentID, kind DocumentKind) (DocumentRef, error) {
	ref := DocumentRef{ID: id, Kind: kind}
	args := append([]any{id, kind}, f.args...)
	var count int
	err := f.tx.QueryRowContext(f.ctx, `SELECT d.source_count FROM extension_documents d WHERE d.id=? AND d.kind=? AND `+f.extensionDocumentPredicate(), args...).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return DocumentRef{}, ErrSourceUnavailable
	}
	if err != nil {
		return DocumentRef{}, readError(f.ctx, err)
	}
	if err = f.extensionReserveRefSources(count); err != nil {
		return DocumentRef{}, err
	}
	args = append(args, f.args...)
	rows, err := f.tx.QueryContext(f.ctx, `WITH readable AS MATERIALIZED(SELECT d.id FROM extension_documents d WHERE d.id=? AND d.kind=? AND `+f.extensionDocumentPredicate()+`)
 SELECT `+extensionSourceColumns+` FROM `+authorizedTables+`
 JOIN extension_document_sources ds ON ds.message_id=m.id AND ds.account=m.account
 JOIN readable ON readable.id=ds.document_id
 WHERE `+f.where+` AND ds.local_revision=m.local_revision AND ds.content_hash=m.content_hash
 AND ds.policy_generation=ap.generation AND ds.expires_at=m.expires_at ORDER BY ds.ordinal,m.id`, args...)
	if err != nil {
		return ref, readError(f.ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		source, err := scanExtensionSource(rows, f.snapshot.PolicyGeneration)
		if err != nil {
			return DocumentRef{}, readError(f.ctx, err)
		}
		ref.Sources = append(ref.Sources, source.Ref)
		ref.ReadSources = append(ref.ReadSources, source)
		f.note(source.OriginalSentAt, source.ExpiresAt)
		f.pin(source.Ref.Version.MessageID, source.Ref.Version.Revision)
	}
	if err = rows.Err(); err != nil {
		return DocumentRef{}, readError(f.ctx, err)
	}
	if err = rows.Close(); err != nil {
		return DocumentRef{}, readError(f.ctx, err)
	}
	if len(ref.Sources) == 0 || len(ref.Sources) > maxResponseSources {
		return DocumentRef{}, ErrSourceUnavailable
	}
	if kind != DocumentWindow {
		var origin DocumentID
		if err = f.tx.QueryRowContext(f.ctx, `SELECT origin_document_id FROM extension_documents WHERE id=?`, id).Scan(&origin); err != nil {
			return DocumentRef{}, readError(f.ctx, err)
		}
		root, err := f.extensionRootRef(origin, kind)
		if err != nil {
			return DocumentRef{}, err
		}
		if !extensionSameOrderedRootMembers(ref.Sources, root.Sources) {
			return DocumentRef{}, ErrSourceUnavailable
		}
		canonical, err := f.extensionCanonicalLegacyRoot(ref, root)
		if err != nil {
			return DocumentRef{}, err
		}
		if canonical {
			return root, nil
		}
		ref.root = root.root
	}
	return ref, nil
}

func (f *readFrame) extensionKeywordCandidates(expression string, limit int, generation IndexGeneration) ([]Candidate, error) {
	rank, rankArgs := f.originalRank(expression)
	args := append(append(rankArgs, f.args...), expression, limit)
	rows, err := f.tx.QueryContext(f.ctx, `SELECT `+extensionSourceColumns+`,`+rank+` AS lexical_score FROM `+authorizedTables+`
 JOIN message_fts ON message_fts.rowid=m.rowid WHERE `+f.where+` AND message_fts MATCH ?
 ORDER BY lexical_score DESC,m.original_sent_at DESC,m.id LIMIT ?`, args...)
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	candidates := []Candidate{}
	for rows.Next() {
		if err = f.extensionReserveRefSources(1); err != nil {
			rows.Close()
			return nil, err
		}
		var source SourceReadVersion
		var sent, expiry int64
		var score float64
		var chatName, senderName string
		if err = rows.Scan(&source.Ref.Account, &source.Ref.Version.MessageID, &source.Ref.Version.Revision, &source.Ref.ContentHash, &source.ChatID, &source.SenderID, &sent, &expiry, &chatName, &senderName, &score); err != nil {
			rows.Close()
			return nil, readError(f.ctx, err)
		}
		source.MetadataHash, err = sourceMetadataHash(boundedOriginal(chatName, 256), boundedOriginal(senderName, 256))
		if err != nil {
			rows.Close()
			return nil, err
		}
		source.Ref.Version.PolicyGeneration = f.snapshot.PolicyGeneration
		source.OriginalSentAt, source.ExpiresAt = fromMillis(sent), fromMillis(expiry)
		source.Ref.Version.ExpiresAt = source.ExpiresAt
		candidates = append(candidates, Candidate{Kind: CandidateKeyword, Score: score, Ref: DocumentRef{MessageID: source.Ref.Version.MessageID, Kind: DocumentWindow, Sources: []SourceRef{source.Ref}, ReadSources: []SourceReadVersion{source}}})
		f.note(source.OriginalSentAt, source.ExpiresAt)
		f.pin(source.Ref.Version.MessageID, source.Ref.Version.Revision)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	roots, err := f.extensionRootKeywordCandidates(expression, limit)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, roots...)
	if generation.ID == "" {
		return extensionLimitKeywordCandidates(candidates, limit), nil
	}
	// Reuse the same document-local term density rather than global BM25
	// statistics, whose score can depend on denied documents.
	derivedRank, derivedRankArgs := f.documentRank(expression, generation)
	args = append(append([]any(nil), derivedRankArgs...), generation.ID)
	args = append(args, f.args...)
	args = append(args, expression, limit)
	rows, err = f.tx.QueryContext(f.ctx, `SELECT d.id,d.kind,`+derivedRank+` AS lexical_score FROM extension_documents d
 JOIN extension_document_fts ON extension_document_fts.rowid=d.rowid
 WHERE d.generation=? AND d.kind='text_window' AND `+f.extensionDocumentPredicate()+` AND extension_document_fts MATCH ?
 ORDER BY lexical_score DESC,d.id LIMIT ?`, args...)
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	type hit struct {
		id    DocumentID
		kind  DocumentKind
		score float64
	}
	var hits []hit
	for rows.Next() {
		var h hit
		if err = rows.Scan(&h.id, &h.kind, &h.score); err != nil {
			rows.Close()
			return nil, readError(f.ctx, err)
		}
		hits = append(hits, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	for _, h := range hits {
		ref, err := f.extensionDocumentRef(h.id, h.kind)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, Candidate{Ref: ref, Kind: CandidateKeyword, Score: h.score})
	}
	return extensionLimitKeywordCandidates(candidates, limit), nil
}

func extensionLimitKeywordCandidates(candidates []Candidate, limit int) []Candidate {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return string(candidates[i].Ref.ID)+string(candidates[i].Ref.MessageID) < string(candidates[j].Ref.ID)+string(candidates[j].Ref.MessageID)
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}

func (s *Store) CandidatePair(ctx context.Context, scope ReadScope, request CandidatePairRequest) (CandidatePair, error) {
	expression, err := literalQuery(request.Search.Query)
	if err != nil {
		return CandidatePair{}, err
	}
	limit, err := readLimit(request.Search.Limit)
	if err != nil || request.Search.Cursor != "" || (request.KeywordOnly && len(request.Query) != 0) {
		return CandidatePair{}, ErrInvalidQuery
	}
	f, err := s.beginRead(ctx, scope, request.Search.Chats, request.Search.From, request.Search.Until)
	if err != nil {
		return CandidatePair{}, err
	}
	defer f.close()
	generation, err := extensionActiveGeneration(f.ctx, f.tx)
	if err != nil {
		return CandidatePair{}, err
	}
	var query []byte
	if !request.KeywordOnly {
		if generation.ID == "" || request.Generation != generation.ID || request.Version != generation.Version {
			return CandidatePair{}, ErrCursorStale
		}
		query, err = extensionQueryVector(request.Query, generation.Version.Dimensions)
		if err != nil {
			return CandidatePair{}, err
		}
	} else if (request.Generation != "" && request.Generation != generation.ID) || (request.Version.ID != "" && request.Version != generation.Version) {
		return CandidatePair{}, ErrCursorStale
	}
	pair := CandidatePair{Snapshot: f.snapshot}
	pair.Keyword, err = f.extensionKeywordCandidates(expression, limit, generation)
	if err != nil {
		return CandidatePair{}, err
	}
	if request.KeywordOnly {
		pair.VectorCode = "keyword_only"
	} else {
		args := []any{generation.ID, generation.ID, generation.Version.Dimensions}
		args = append(args, f.args...)
		args = append(args, query, limit)
		rows, err := f.tx.QueryContext(f.ctx, `WITH eligible AS MATERIALIZED (
 SELECT d.id,d.kind,v.values_blob FROM extension_documents d JOIN extension_vectors v ON v.document_id=d.id
 WHERE d.generation=? AND v.generation=? AND v.dimensions=? AND `+f.extensionDocumentPredicate()+`)
 SELECT id,kind,vec_distance_L2(values_blob,?) AS distance FROM eligible ORDER BY distance,id LIMIT ?`, args...)
		if err != nil {
			return CandidatePair{}, readError(f.ctx, err)
		}
		type hit struct {
			id       DocumentID
			kind     DocumentKind
			distance float64
		}
		var hits []hit
		for rows.Next() {
			var h hit
			if err = rows.Scan(&h.id, &h.kind, &h.distance); err != nil {
				rows.Close()
				return CandidatePair{}, readError(f.ctx, err)
			}
			hits = append(hits, h)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return CandidatePair{}, readError(f.ctx, err)
		}
		for _, h := range hits {
			ref, err := f.extensionDocumentRef(h.id, h.kind)
			if err != nil {
				return CandidatePair{}, err
			}
			pair.Semantic = append(pair.Semantic, Candidate{Ref: ref, Kind: CandidateVector, Score: -h.distance})
		}
	}
	if err = f.finish(); err != nil {
		return CandidatePair{}, err
	}
	return pair, nil
}

func extensionCandidateSameSource(a, b SourceReadVersion) bool {
	return a.Ref == b.Ref && a.ChatID == b.ChatID && a.SenderID == b.SenderID && a.OriginalSentAt.Equal(b.OriginalSentAt) && a.ExpiresAt.Equal(b.ExpiresAt) && a.MetadataHash != [32]byte{} && a.MetadataHash == b.MetadataHash
}

func extensionReadableProvenance(kind DocumentKind, p DerivedProvenance) bool {
	if p.Kind != kind {
		return false
	}
	if kind == DocumentWindow {
		return p.MediaID == "" && p.ProfileID == "" && p.ExtractorVersion == "" && p.PromptVersion == "" && p.StartMS == 0 && p.EndMS == 0 && p.OffsetKind == "none"
	}
	if kind != DocumentOCR && kind != DocumentCaption && kind != DocumentTranscript {
		return false
	}
	if !validReadID(string(p.MediaID)) || !validReadID(p.ProfileID) || !validReadID(p.ExtractorVersion) || (p.PromptVersion != "" && !validReadID(p.PromptVersion)) || p.StartMS < 0 || p.EndMS < p.StartMS || p.EndMS > 24*60*60*1000 {
		return false
	}
	if kind == DocumentTranscript {
		return (p.OffsetKind == "none" && p.StartMS == 0 && p.EndMS == 0) || p.OffsetKind == "slice" || p.OffsetKind == "word"
	}
	return p.OffsetKind == "none" && p.StartMS == 0 && p.EndMS == 0
}

func extensionExpectedSources(ref DocumentRef) (map[MessageID]SourceReadVersion, error) {
	if len(ref.Sources) == 0 || len(ref.Sources) > maxResponseSources || len(ref.Sources) != len(ref.ReadSources) {
		return nil, ErrSourceUnavailable
	}
	expected := make(map[MessageID]SourceReadVersion, len(ref.Sources))
	for i, source := range ref.ReadSources {
		if source.Ref != ref.Sources[i] || source.MetadataHash == [32]byte{} || !validReadID(string(source.Ref.Version.MessageID)) || source.Ref.Version.Revision == 0 || source.Ref.Version.Deleted || source.Ref.Version.Excluded || source.ChatID == "" || source.SenderID == "" || source.OriginalSentAt.IsZero() || !validReadTime(source.OriginalSentAt) || !validReadTime(source.ExpiresAt) || !source.ExpiresAt.Equal(source.Ref.Version.ExpiresAt) {
			return nil, ErrSourceUnavailable
		}
		if _, exists := expected[source.Ref.Version.MessageID]; exists {
			return nil, ErrSourceUnavailable
		}
		expected[source.Ref.Version.MessageID] = source
	}
	return expected, nil
}

func (f *readFrame) extensionReadSources(expected map[MessageID]SourceReadVersion) ([]SourceReadVersion, error) {
	if len(expected) == 0 {
		return nil, nil
	}
	marks := make([]string, 0, len(expected))
	args := append([]any(nil), f.args...)
	for id := range expected {
		marks = append(marks, "?")
		args = append(args, id)
	}
	rows, err := f.tx.QueryContext(f.ctx, `SELECT `+extensionSourceColumns+` FROM `+authorizedTables+` WHERE `+f.where+` AND m.id IN (`+strings.Join(marks, ",")+`) ORDER BY m.original_sent_at,m.id`, args...)
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	defer rows.Close()
	var sources []SourceReadVersion
	for rows.Next() {
		source, err := scanExtensionSource(rows, f.snapshot.PolicyGeneration)
		if err != nil {
			return nil, readError(f.ctx, err)
		}
		if !extensionCandidateSameSource(source, expected[source.Ref.Version.MessageID]) {
			return nil, ErrSourceUnavailable
		}
		sources = append(sources, source)
		f.note(source.OriginalSentAt, source.ExpiresAt)
		f.pin(source.Ref.Version.MessageID, source.Ref.Version.Revision)
	}
	if err = rows.Err(); err != nil {
		return nil, readError(f.ctx, err)
	}
	if len(sources) != len(expected) {
		return nil, ErrSourceUnavailable
	}
	return sources, nil
}

// Window text is original quoted source material. Reconstruct its byte spans
// from the already-authorized source snapshot rather than trusting stored
// window text as an original field. Each SQL slice is bounded before loading.
func (f *readFrame) extensionOriginalWindow(id DocumentID, sources []SourceReadVersion) (string, error) {
	var size int
	if err := f.tx.QueryRowContext(f.ctx, `SELECT length(CAST(spans_json AS BLOB)) FROM extension_documents WHERE id=? AND embedding_input=0`, id).Scan(&size); errors.Is(err, sql.ErrNoRows) {
		return "", ErrSourceUnavailable
	} else if err != nil {
		return "", readError(f.ctx, err)
	}
	if size > maxResponseSources*2048 {
		return "", ErrLimitExceeded
	}
	var encoded string
	if err := f.tx.QueryRowContext(f.ctx, `SELECT spans_json FROM extension_documents WHERE id=? AND embedding_input=0`, id).Scan(&encoded); errors.Is(err, sql.ErrNoRows) {
		return "", ErrSourceUnavailable
	} else if err != nil {
		return "", readError(f.ctx, err)
	}
	var spans []SourceSpan
	if json.Unmarshal([]byte(encoded), &spans) != nil || len(spans) == 0 {
		return "", ErrSourceUnavailable
	}
	if len(spans) > maxResponseSources {
		return "", ErrLimitExceeded
	}
	type member struct {
		ref     SourceRef
		ordinal int
	}
	members := make(map[MessageID]member, len(sources))
	for i, source := range sources {
		members[source.Ref.Version.MessageID] = member{ref: source.Ref, ordinal: i}
	}
	used := make(map[MessageID]bool, len(sources))
	parts := make([]string, 0, len(spans))
	lastSource, lastField, total := -1, -1, 0
	for _, span := range spans {
		m, ok := members[span.Source.Version.MessageID]
		if !ok || !extensionSameSource(m.ref, span.Source) || span.StartByte < 0 || span.EndByte <= span.StartByte {
			return "", ErrSourceUnavailable
		}
		field, fieldOrder := "body", 0
		switch span.Field {
		case "body":
		case "caption":
			field, fieldOrder = "caption", 1
		default:
			return "", ErrSourceUnavailable
		}
		if m.ordinal < lastSource || (m.ordinal == lastSource && fieldOrder <= lastField) {
			return "", ErrSourceUnavailable
		}
		lastSource, lastField = m.ordinal, fieldOrder
		length := span.EndByte - span.StartByte
		if len(parts) > 0 {
			total++
		}
		if length > extensionMaxPassageBytes-total {
			return "", ErrLimitExceeded
		}
		// field is selected only from the fixed body/caption allowlist. IDs,
		// revisions, hashes, and offsets remain bound parameters.
		var fieldBytes int
		var fragment []byte
		err := f.tx.QueryRowContext(f.ctx, `SELECT length(CAST(m.`+field+` AS BLOB)),substr(CAST(m.`+field+` AS BLOB),?,?) FROM messages m
 WHERE m.account=? AND m.id=? AND m.local_revision=? AND m.content_hash=? AND m.state='active' AND m.original_date_known=1`, span.StartByte+1, length, m.ref.Account, m.ref.Version.MessageID, m.ref.Version.Revision, m.ref.ContentHash).Scan(&fieldBytes, &fragment)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrSourceUnavailable
		}
		if err != nil {
			return "", readError(f.ctx, err)
		}
		if span.EndByte > fieldBytes || len(fragment) != length || !utf8.Valid(fragment) {
			return "", ErrSourceUnavailable
		}
		parts = append(parts, string(fragment))
		total += length
		used[m.ref.Version.MessageID] = true
	}
	if len(used) != len(members) {
		return "", ErrSourceUnavailable
	}
	return strings.Join(parts, "\n"), nil
}

func (s *Store) HydrateDocuments(ctx context.Context, scope ReadScope, request HydrateRequest) (HydratedPage, error) {
	identity, err := extensionScopeIdentity(scope)
	if err != nil {
		return HydratedPage{}, err
	}
	if len(request.Documents) > 100 || request.MaxBytes < 0 || request.MaxBytes > maxContextBytes {
		return HydratedPage{}, ErrLimitExceeded
	}
	memberCopies := 0
	for _, ref := range request.Documents {
		if len(ref.Sources) > maxResponseSources-memberCopies {
			return HydratedPage{}, ErrLimitExceeded
		}
		if len(ref.ReadSources) != len(ref.Sources) {
			return HydratedPage{}, ErrSourceUnavailable
		}
		memberCopies += len(ref.Sources)
	}
	maximum := request.MaxBytes
	if maximum == 0 {
		maximum = maxContextBytes
	}
	f, err := s.beginRead(ctx, scope, nil, time.Time{}, time.Time{})
	if err != nil {
		return HydratedPage{}, err
	}
	defer f.close()
	if request.Snapshot.PolicyGeneration != f.snapshot.PolicyGeneration {
		return HydratedPage{}, ErrPolicyChanged
	}
	if request.Snapshot.IndexGeneration != f.snapshot.IndexGeneration || request.Snapshot.RecoveryEpoch != f.snapshot.RecoveryEpoch {
		return HydratedPage{}, ErrCursorStale
	}
	generation, err := extensionActiveGeneration(f.ctx, f.tx)
	if err != nil {
		return HydratedPage{}, err
	}
	page := HydratedPage{Snapshot: f.snapshot, Guard: ResponseGuard{PolicyGeneration: f.snapshot.PolicyGeneration, Generation: generation.ID, IndexVersionID: generation.Version.ID, ScopeIdentity: identity, safetyEpoch: f.epoch, indexGeneration: f.snapshot.IndexGeneration, recoveryEpoch: f.snapshot.RecoveryEpoch, coverageEpoch: f.snapshot.CoverageEpoch, rolling: f.rolling}}
	guard := make(map[MessageID]SourceReadVersion)
	rootPins := make(map[DocumentID]extensionRootResponsePin)
	for _, ref := range request.Documents {
		expected, err := extensionExpectedSources(ref)
		if err != nil {
			return HydratedPage{}, err
		}
		if ref.Kind != DocumentWindow {
			for i, source := range ref.Sources {
				if ref.ReadSources[i].Ref != source {
					return HydratedPage{}, ErrSourceUnavailable
				}
			}
		}
		sources, err := f.extensionReadSources(expected)
		if err != nil {
			return HydratedPage{}, err
		}
		anchor := sources[0]
		var text string
		provenance := DerivedProvenance{Kind: DocumentWindow, OffsetKind: "none"}
		if ref.rootReference {
			if ref.root == nil || ref.root.ID != ref.ID || ref.MessageID != "" || ref.Kind == DocumentWindow {
				return HydratedPage{}, ErrSourceUnavailable
			}
			current, err := f.extensionRootRef(ref.ID, ref.Kind)
			if err != nil {
				return HydratedPage{}, err
			}
			if current.root == nil || *current.root != *ref.root || !extensionSameOrderedRootMembers(current.Sources, ref.Sources) {
				return HydratedPage{}, ErrSourceUnavailable
			}
			for _, source := range current.ReadSources {
				if !extensionCandidateSameSource(source, expected[source.Ref.Version.MessageID]) {
					return HydratedPage{}, ErrSourceUnavailable
				}
			}
			root, err := s.extensionLoadRoot(f.ctx, f.tx, ref.ID)
			if err != nil {
				return HydratedPage{}, err
			}
			if root.Kind != ref.Kind || !extensionSameOrderedRootMembers(root.Sources, current.Sources) {
				return HydratedPage{}, ErrSourceUnavailable
			}
			ref, text, provenance = current, root.Text, root.Provenance
			anchor = current.ReadSources[0]
		} else if ref.ID == "" {
			if ref.root != nil || len(sources) != 1 || ref.MessageID != sources[0].Ref.Version.MessageID || ref.Kind != DocumentWindow {
				return HydratedPage{}, ErrSourceUnavailable
			}
			var body, caption string
			if err = f.tx.QueryRowContext(f.ctx, `SELECT body,caption FROM messages WHERE id=?`, ref.MessageID).Scan(&body, &caption); err != nil {
				return HydratedPage{}, readError(f.ctx, err)
			}
			text = originalSnippet(body, caption)
		} else {
			if !validString(string(ref.ID), 256) || ref.MessageID != "" || generation.ID == "" {
				return HydratedPage{}, ErrSourceUnavailable
			}
			var kind DocumentKind
			var provenanceJSON string
			var count int
			var textBytes, provenanceBytes int
			// Reject corrupt or obsolete oversized stored payloads before
			// allocating their text/provenance in the Go process.
			err = f.tx.QueryRowContext(f.ctx, `SELECT length(CAST(d.text AS BLOB)),length(CAST(d.provenance_json AS BLOB)) FROM extension_documents d WHERE d.id=? AND d.generation=? AND `+f.extensionDocumentPredicate(), append([]any{ref.ID, generation.ID}, f.args...)...).Scan(&textBytes, &provenanceBytes)
			if errors.Is(err, sql.ErrNoRows) {
				return HydratedPage{}, ErrSourceUnavailable
			}
			if err != nil {
				return HydratedPage{}, readError(f.ctx, err)
			}
			if textBytes > maxDerivedBytes || provenanceBytes > 4096 {
				return HydratedPage{}, ErrLimitExceeded
			}
			err = f.tx.QueryRowContext(f.ctx, `SELECT d.kind,CASE WHEN d.kind='text_window' THEN '' ELSE d.text END,d.provenance_json,d.source_count FROM extension_documents d WHERE d.id=? AND d.generation=? AND `+f.extensionDocumentPredicate(), append([]any{ref.ID, generation.ID}, f.args...)...).Scan(&kind, &text, &provenanceJSON, &count)
			if errors.Is(err, sql.ErrNoRows) {
				return HydratedPage{}, ErrSourceUnavailable
			}
			if err != nil {
				return HydratedPage{}, readError(f.ctx, err)
			}
			if kind != ref.Kind || count != len(expected) || !utf8.ValidString(text) || json.Unmarshal([]byte(provenanceJSON), &provenance) != nil || !extensionReadableProvenance(kind, provenance) {
				return HydratedPage{}, ErrSourceUnavailable
			}
			current, err := f.extensionDocumentRef(ref.ID, kind)
			if err != nil {
				return HydratedPage{}, err
			}
			if len(current.ReadSources) != len(expected) {
				return HydratedPage{}, ErrSourceUnavailable
			}
			for _, source := range current.ReadSources {
				if !extensionCandidateSameSource(source, expected[source.Ref.Version.MessageID]) {
					return HydratedPage{}, ErrSourceUnavailable
				}
			}
			if kind != DocumentWindow && (current.root == nil || ref.root == nil || *current.root != *ref.root || !extensionSameOrderedRootMembers(current.Sources, ref.Sources)) {
				return HydratedPage{}, ErrSourceUnavailable
			}
			// Preserve the stored first-source identity for generated media
			// documents. Window text remains chronological source material.
			ref = current
			if kind != DocumentWindow {
				anchor = current.ReadSources[0]
				if ref.root == nil {
					return HydratedPage{}, ErrSourceUnavailable
				}
				text, err = f.extensionRootFragmentText(ref)
				if err != nil {
					return HydratedPage{}, err
				}
			}
			if kind == DocumentWindow {
				text, err = f.extensionOriginalWindow(ref.ID, sources)
				if err != nil {
					return HydratedPage{}, err
				}
			}
		}
		document := RetrievedDocument{Ref: ref, ChatID: anchor.ChatID, SentAt: anchor.OriginalSentAt}
		if err = f.tx.QueryRowContext(f.ctx, `SELECT m.body_kind,substr(c.name,1,256),substr(i.name,1,256) FROM messages m JOIN chats c ON c.id=m.chat_id AND c.account=m.account JOIN identities i ON i.id=m.sender_id AND i.account=m.account WHERE m.id=?`, anchor.Ref.Version.MessageID).Scan(&document.Kind, &document.ChatName, &document.SenderName); err != nil {
			return HydratedPage{}, readError(f.ctx, err)
		}
		document.ChatName, document.SenderName = boundedOriginal(document.ChatName, 256), boundedOriginal(document.SenderName, 256)
		document.Excerpts = []DerivedExcerpt{{DocumentID: ref.ID, Source: anchor.Ref.Version, Text: text, Provenance: provenance}}
		page.Documents = append(page.Documents, document)
		if err = extensionAddRootPin(rootPins, ref.root); err != nil {
			return HydratedPage{}, err
		}
		for _, source := range sources {
			if previous, exists := guard[source.Ref.Version.MessageID]; exists && !extensionCandidateSameSource(previous, source) {
				return HydratedPage{}, ErrSourceUnavailable
			}
			guard[source.Ref.Version.MessageID] = source
			if len(guard) > maxResponseSources {
				return HydratedPage{}, ErrLimitExceeded
			}
		}
	}
	for _, source := range guard {
		page.Guard.Sources = append(page.Guard.Sources, source)
	}
	page.Guard.minSent, page.Guard.minExpiry = f.minSent, f.minExpiry
	page.Guard.roots = extensionSortedRootPins(rootPins)
	sort.Slice(page.Guard.Sources, func(i, j int) bool {
		return page.Guard.Sources[i].Ref.Version.MessageID < page.Guard.Sources[j].Ref.Version.MessageID
	})
	encoded, err := json.Marshal(page)
	if err != nil || len(encoded) > maximum {
		return HydratedPage{}, ErrLimitExceeded
	}
	if err = f.finish(); err != nil {
		return HydratedPage{}, err
	}
	return page, nil
}

// The caller has acquired a connection before entering the shared commit
// fence. This fresh frame has no prior SQLite snapshot and calls no clock or
// caller hook while the fence is held.
func extensionResponseFrame(s *Store, ctx context.Context, tx *sql.Tx, scope ReadScope, now time.Time) (*readFrame, error) {
	f := &readFrame{s: s, ctx: ctx, tx: tx, scope: scope, now: now}
	var mode DateMode
	var importStart, rolling int64
	err := tx.QueryRowContext(ctx, `SELECT p.generation,p.date_mode,p.import_start,p.rolling_retention,a.archive_revision,a.index_generation,a.recovery_epoch,a.recovery_code FROM archive_policy p JOIN archive_meta a ON a.id=1 WHERE p.id=1`).Scan(&f.snapshot.PolicyGeneration, &mode, &importStart, &rolling, &f.snapshot.ArchiveRevision, &f.snapshot.IndexGeneration, &f.snapshot.RecoveryEpoch, &f.snapshot.RecoveryCode)
	if err != nil {
		return nil, readError(ctx, err)
	}
	if err = unknownHistoryGapSnapshot(ctx, tx, scope.Account, &f.snapshot); err != nil {
		return nil, err
	}
	if f.snapshot.PolicyGeneration != scope.PolicyGeneration {
		return nil, ErrPolicyChanged
	}
	f.from, f.until, f.rolling = scope.From.UTC(), scope.Until.UTC(), scope.Rolling
	if mode == DateSince {
		f.from = later(f.from, fromMillis(importStart))
	}
	if rolling > 0 && (f.rolling == 0 || time.Duration(rolling) < f.rolling) {
		f.rolling = time.Duration(rolling)
	}
	if f.rolling > 0 {
		f.from = later(f.from, now.Add(-f.rolling))
	}
	f.where = `m.account=? AND m.state='active' AND m.original_date_known=1 AND ap.generation=?
 AND (m.expires_at=0 OR m.expires_at>?) AND (ap.chat_mode='all' OR EXISTS(SELECT 1 FROM policy_chats pc WHERE pc.chat_id=m.chat_id))
 AND (ap.date_mode='all_available' OR m.original_sent_at>=ap.import_start)`
	f.args = []any{scope.Account, scope.PolicyGeneration, unixms(now)}
	if !scope.AllChats {
		marks := make([]string, len(scope.Chats))
		for i, id := range scope.Chats {
			marks[i] = "?"
			f.args = append(f.args, id)
		}
		f.where += ` AND m.chat_id IN (` + strings.Join(marks, ",") + `)`
	}
	if !f.from.IsZero() {
		f.where += ` AND m.original_sent_at>=?`
		f.args = append(f.args, ceilMillis(f.from))
	}
	if !f.until.IsZero() {
		f.where += ` AND m.original_sent_at<?`
		f.args = append(f.args, ceilMillis(f.until))
	}
	return f, nil
}

func (s *Store) CheckResponse(ctx context.Context, scope ReadScope, check ResponseCheck) (ReadSnapshot, error) {
	identity, err := extensionScopeIdentity(scope)
	if err != nil || identity != check.Guard.ScopeIdentity {
		return ReadSnapshot{}, ErrUnauthorized
	}
	if check.Guard.PolicyGeneration != scope.PolicyGeneration {
		return ReadSnapshot{}, ErrPolicyChanged
	}
	if len(check.Guard.Sources) > maxResponseSources {
		return ReadSnapshot{}, ErrLimitExceeded
	}
	if len(check.Guard.roots) > maxRootResponsePins {
		return ReadSnapshot{}, ErrLimitExceeded
	}
	expected := make(map[MessageID]SourceReadVersion, len(check.Guard.Sources))
	for _, source := range check.Guard.Sources {
		ref := DocumentRef{Sources: []SourceRef{source.Ref}, ReadSources: []SourceReadVersion{source}}
		if _, err = extensionExpectedSources(ref); err != nil || source.Ref.Account != scope.Account || source.Ref.Version.PolicyGeneration != scope.PolicyGeneration {
			return ReadSnapshot{}, ErrSourceUnavailable
		}
		if previous, exists := expected[source.Ref.Version.MessageID]; exists && !extensionCandidateSameSource(previous, source) {
			return ReadSnapshot{}, ErrSourceUnavailable
		}
		expected[source.Ref.Version.MessageID] = source
	}
	if check.Credential != nil {
		credentialIdentity, err := extensionScopeIdentity(check.Credential.Scope)
		if err != nil || credentialIdentity != identity {
			return ReadSnapshot{}, ErrUnauthorized
		}
	}
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	// Pool admission must complete before taking the writer commit fence.
	connection, err := s.readDB.Conn(bounded)
	if err != nil {
		return ReadSnapshot{}, readError(bounded, err)
	}
	defer connection.Close()
	serviceSample := s.now()
	clockSample := time.Now()
	fenced, stop := context.WithTimeout(bounded, responseDeadline)
	defer stop()
	s.safetyMu.RLock()
	defer s.safetyMu.RUnlock()
	if s.unclean || s.unsafe[scope.Account] {
		return ReadSnapshot{}, ErrReconciliationRequired
	}
	// Metadata pages have no contributor membership to revalidate. Their
	// private archive-built epoch protects aggregate counts and chat bounds.
	if len(expected) == 0 && check.Guard.safetyEpoch != s.safetyEpoch {
		return ReadSnapshot{}, ErrPolicyChanged
	}
	if fenced.Err() != nil {
		return ReadSnapshot{}, fenced.Err()
	}
	now := serviceSample.Add(time.Since(clockSample))
	tx, err := connection.BeginTx(fenced, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ReadSnapshot{}, readError(fenced, err)
	}
	defer tx.Rollback()
	f, err := extensionResponseFrame(s, fenced, tx, scope, now)
	if err != nil {
		return ReadSnapshot{}, err
	}
	if check.Guard.indexGeneration != 0 && check.Guard.indexGeneration != f.snapshot.IndexGeneration {
		return ReadSnapshot{}, ErrCursorStale
	}
	if check.Guard.coverageEpoch != f.snapshot.CoverageEpoch {
		return ReadSnapshot{}, ErrCursorStale
	}
	if check.Guard.recoveryEpoch != f.snapshot.RecoveryEpoch {
		return ReadSnapshot{}, ErrCursorStale
	}
	generation, err := extensionActiveGeneration(fenced, tx)
	if err != nil {
		return ReadSnapshot{}, err
	}
	if generation.ID != check.Guard.Generation || generation.Version.ID != check.Guard.IndexVersionID {
		return ReadSnapshot{}, ErrCursorStale
	}
	var unsafe int
	if err = tx.QueryRowContext(fenced, `SELECT unsafe FROM account_safety WHERE account=?`, scope.Account).Scan(&unsafe); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ReadSnapshot{}, ErrUnauthorized
		}
		return ReadSnapshot{}, readError(fenced, err)
	}
	if unsafe != 0 {
		return ReadSnapshot{}, ErrReconciliationRequired
	}
	var credentialExpiry time.Time
	if check.Credential != nil {
		if err = s.checkCredentialGrantQueryAt(fenced, tx, *check.Credential, now); err != nil {
			return ReadSnapshot{}, err
		}
		credential, err := loadCredentialQuery(fenced, tx, check.Credential.CredentialID)
		if err != nil {
			return ReadSnapshot{}, err
		}
		credentialExpiry = credential.ExpiresAt
	}
	refs := make([]SourceRef, 0, len(expected))
	for _, source := range expected {
		refs = append(refs, source.Ref)
	}
	if len(refs) > 0 {
		if _, err = s.extensionCheckSourcesAt(fenced, tx, refs, false, now); err != nil {
			return ReadSnapshot{}, err
		}
	}
	currentSources, err := f.extensionReadSources(expected)
	if err != nil {
		return ReadSnapshot{}, err
	}
	if err = f.extensionCheckRootPinsWithSources(check.Guard.roots, expected, currentSources); err != nil {
		return ReadSnapshot{}, err
	}
	if err = f.checkAttachmentPins(check.Guard.attachments, expected); err != nil {
		return ReadSnapshot{}, err
	}
	f.note(check.Guard.minSent, check.Guard.minExpiry)
	if err = tx.Commit(); err != nil {
		return ReadSnapshot{}, readError(fenced, err)
	}
	// Include commit in the horizon while still holding the shared fence.
	// This cannot lengthen expiry/rolling authority and invokes no clock hook.
	finalNow := serviceSample.Add(time.Since(clockSample))
	rolling := f.rolling
	if check.Guard.rolling > 0 && (rolling == 0 || check.Guard.rolling < rolling) {
		rolling = check.Guard.rolling
	}
	if (!f.minExpiry.IsZero() && !finalNow.Before(f.minExpiry)) || (rolling > 0 && !f.minSent.IsZero() && f.minSent.Before(finalNow.Add(-rolling))) {
		return ReadSnapshot{}, ErrSourceUnavailable
	}
	if !credentialExpiry.IsZero() && !finalNow.Before(credentialExpiry) {
		return ReadSnapshot{}, ErrUnauthorized
	}
	return f.snapshot, nil
}
