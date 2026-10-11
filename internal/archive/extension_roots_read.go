package archive

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxRootResponsePins  = 100
	maxCandidateRootPins = 200
)

// Bound materialized membership copies, including repeated sources in many
// documents. A deduplicated guard map alone does not bound these allocations.
func (f *readFrame) extensionReserveRefSources(count int) error {
	if count < 1 || count > maxResponseSources {
		return ErrSourceUnavailable
	}
	if count > maxResponseSources-f.refSourceCopies {
		return ErrLimitExceeded
	}
	f.refSourceCopies += count
	return nil
}

// Root pins are constructed by the archive from an immutable extraction root.
// They bind generated text and media authority independently of an encoder.
type extensionRootResponsePin struct {
	ID             DocumentID
	IdentitySHA256 [32]byte
}

// A retained original is a cache entry, not continued authority over generated
// text. The immutable root's media association must still name one exact member.
const extensionRootMediaPredicate = `EXISTS(SELECT 1 FROM extension_media_associations rm
 JOIN extension_root_sources rms ON rms.root_id=r.id AND rms.account=rm.account AND rms.message_id=rm.message_id
 AND rms.local_revision=rm.local_revision AND rms.policy_generation=rm.policy_generation
 AND rms.content_hash=rm.content_hash AND rms.expires_at=rm.source_expires_at
 WHERE rm.id=r.media_id AND rm.active=1 AND rm.view_once=0
 AND length(rm.last_original_sha256)=64 AND rm.last_original_sha256 NOT GLOB '*[^0-9a-f]*'
 AND ((r.kind IN ('ocr','caption') AND rm.kind='image') OR (r.kind='transcript' AND rm.kind='voice'))
 AND (r.extraction_job_id IS NULL OR EXISTS(SELECT 1 FROM extension_extraction_jobs ej
 JOIN extension_extraction_versions ev ON ev.id=ej.version_id
 JOIN extension_extraction_outputs eo ON eo.job_id=ej.id AND eo.root_id=r.id
 WHERE ej.id=r.extraction_job_id AND ej.state='complete' AND ej.outcome='text' AND r.source_count=1
 AND ej.media_id=rm.id AND ej.account=rm.account AND ej.message_id=rm.message_id
 AND ej.local_revision=rm.local_revision AND ej.policy_generation=rm.policy_generation
 AND ej.content_hash=rm.content_hash AND ej.source_expires_at=rm.source_expires_at
 AND ej.original_sha256=rm.last_original_sha256 AND length(ev.version_hash)=64
 AND ((r.kind='ocr' AND ej.kind='ocr') OR (r.kind='caption' AND ej.kind='caption') OR (r.kind='transcript' AND ej.kind='transcription')))))`

const extensionRootMetadataPredicate = `r.kind IN ('ocr','caption','transcript')
 AND r.text_bytes BETWEEN 1 AND 65536 AND length(CAST(r.text AS BLOB))=r.text_bytes
 AND length(r.text_sha256)=64 AND r.text_sha256 NOT GLOB '*[^0-9a-f]*' AND length(CAST(r.provenance_json AS BLOB))<=4096
 AND r.source_count BETWEEN 1 AND 2048
 AND r.source_count=(SELECT count(*) FROM extension_root_sources rs WHERE rs.root_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM extension_root_sources rs JOIN messages root_message ON root_message.id=rs.message_id AND root_message.account=rs.account WHERE rs.root_id=r.id AND root_message.view_once<>0)`

// All members and the media association are gated before rank, LIMIT, or body
// loading. The one scoped subquery uses exactly one copy of f.args.
func (f *readFrame) extensionRootPredicate() string {
	return extensionRootMetadataPredicate + ` AND ` + extensionRootMediaPredicate + `
 AND NOT EXISTS(SELECT 1 FROM extension_root_sources rs WHERE rs.root_id=r.id AND NOT EXISTS(
 SELECT 1 FROM ` + authorizedTables + ` WHERE ` + f.where + `
 AND m.account=rs.account AND m.id=rs.message_id AND m.local_revision=rs.local_revision
 AND m.content_hash=rs.content_hash AND ap.generation=rs.policy_generation AND m.expires_at=rs.expires_at))`
}

// The document predicate has already authorized its complete source set. Exact
// equality lets the root gate reuse that authority without additional scoped
// placeholders or filtering denied root members out through an inner join.
const extensionDocumentRootPredicate = `(d.kind='text_window' OR EXISTS(SELECT 1 FROM extension_roots r
 WHERE r.id=d.origin_document_id AND r.kind=d.kind AND r.provenance_json=d.provenance_json
 AND d.origin_start_byte>=0 AND d.origin_end_byte>d.origin_start_byte
 AND d.origin_end_byte<=r.text_bytes AND d.origin_end_byte-d.origin_start_byte<=16384
 AND (d.id<>r.id OR (r.legacy_document_id=d.id AND d.origin_start_byte=0
 AND d.origin_end_byte=r.text_bytes AND d.text=r.text))
 AND r.source_count=d.source_count AND ` + extensionRootMetadataPredicate + ` AND ` + extensionRootMediaPredicate + `
 AND NOT EXISTS(SELECT 1 FROM extension_root_sources rs WHERE rs.root_id=r.id AND NOT EXISTS(
 SELECT 1 FROM extension_document_sources ds WHERE ds.document_id=d.id AND ds.account=rs.account
 AND ds.message_id=rs.message_id AND ds.local_revision=rs.local_revision AND ds.policy_generation=rs.policy_generation
 AND ds.content_hash=rs.content_hash AND ds.expires_at=rs.expires_at AND ds.ordinal=rs.ordinal))))`

func extensionRootIdentity(record extensionRootRecord) (extensionRootResponsePin, error) {
	// Preserve source order: it is immutable origin metadata and names the
	// generated document's anchor. Names/current dates are separate source pins.
	metadata := struct {
		ID               DocumentID
		Kind             DocumentKind
		TextSHA256       string
		Bytes            int
		MediaID          MediaID
		ProvenanceJSON   string
		ExtractionJobID  JobID
		LegacyDocumentID DocumentID
		OriginalSHA256   string
		VersionHash      string
		Sources          []SourceRef
	}{record.Origin.ID, record.Kind, record.Origin.TextSHA256, record.Origin.Bytes, record.MediaID, record.ProvenanceJSON, record.ExtractionJobID, record.LegacyDocumentID, record.OriginalSHA256, record.VersionHash, record.Origin.Sources}
	if !validString(string(metadata.ID), 256) || len(metadata.Sources) == 0 || len(metadata.Sources) > maxResponseSources || len(metadata.ProvenanceJSON) > 4096 || !extensionReadableProvenance(record.Kind, record.Origin.Provenance) || record.Origin.Provenance.MediaID != record.MediaID {
		return extensionRootResponsePin{}, ErrSourceUnavailable
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return extensionRootResponsePin{}, ErrSourceUnavailable
	}
	domain := []byte("wa-indexer-extraction-root-response/v1\x00")
	return extensionRootResponsePin{ID: metadata.ID, IdentitySHA256: sha256.Sum256(append(domain, encoded...))}, nil
}

func (f *readFrame) extensionRootRef(id DocumentID, kind DocumentKind) (DocumentRef, error) {
	var admitted DocumentID
	var count int
	args := append([]any{id, kind}, f.args...)
	err := f.tx.QueryRowContext(f.ctx, `SELECT r.id,r.source_count FROM extension_roots r WHERE r.id=? AND r.kind=? AND `+f.extensionRootPredicate(), args...).Scan(&admitted, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return DocumentRef{}, ErrSourceUnavailable
	}
	if err != nil {
		return DocumentRef{}, readError(f.ctx, err)
	}
	if err = f.extensionReserveRefSources(count); err != nil {
		return DocumentRef{}, err
	}
	record, err := f.s.extensionAuthorizeRootAt(f.ctx, f.tx, admitted, f.now)
	if err != nil {
		return DocumentRef{}, err
	}
	if record.Kind != kind {
		return DocumentRef{}, ErrSourceUnavailable
	}
	pin, err := extensionRootIdentity(record)
	if err != nil {
		return DocumentRef{}, err
	}
	ref := DocumentRef{ID: admitted, Kind: kind, root: &pin, rootReference: true}
	args = append(append([]any(nil), f.args...), admitted)
	rows, err := f.tx.QueryContext(f.ctx, `SELECT `+extensionSourceColumns+` FROM `+authorizedTables+`
 JOIN extension_root_sources rs ON rs.message_id=m.id AND rs.account=m.account
 WHERE `+f.where+` AND rs.root_id=? AND rs.local_revision=m.local_revision AND rs.content_hash=m.content_hash
 AND rs.policy_generation=ap.generation AND rs.expires_at=m.expires_at ORDER BY rs.ordinal,m.id`, args...)
	if err != nil {
		return DocumentRef{}, readError(f.ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		source, err := scanExtensionSource(rows, f.snapshot.PolicyGeneration)
		if err != nil {
			return DocumentRef{}, readError(f.ctx, err)
		}
		ordinal := len(ref.Sources)
		if ordinal >= len(record.Origin.Sources) || source.Ref != record.Origin.Sources[ordinal] {
			return DocumentRef{}, ErrSourceUnavailable
		}
		ref.Sources = append(ref.Sources, source.Ref)
		ref.ReadSources = append(ref.ReadSources, source)
		f.note(source.OriginalSentAt, source.ExpiresAt)
		f.pin(source.Ref.Version.MessageID, source.Ref.Version.Revision)
	}
	if err = rows.Err(); err != nil {
		return DocumentRef{}, readError(f.ctx, err)
	}
	if len(ref.Sources) != len(record.Origin.Sources) {
		return DocumentRef{}, ErrSourceUnavailable
	}
	if err = f.captureRootRef(ref); err != nil {
		return DocumentRef{}, err
	}
	return ref, nil
}

func (f *readFrame) extensionRootKeywordCandidates(expression string, limit int) ([]Candidate, error) {
	rank, args := f.rootRank(expression, false)
	args = append(args, f.args...)
	args = append(args, expression, limit)
	rows, err := f.tx.QueryContext(f.ctx, `SELECT r.id,r.kind,`+rank+` AS lexical_score FROM extension_roots r
 JOIN extension_root_fts ON extension_root_fts.rowid=r.rowid AND extension_root_fts.root_id=r.id
 WHERE `+f.extensionRootPredicate()+` AND extension_root_fts MATCH ? ORDER BY lexical_score DESC,r.id LIMIT ?`, args...)
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
	candidates := make([]Candidate, 0, len(hits))
	for _, h := range hits {
		ref, err := f.extensionRootRef(h.id, h.kind)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, Candidate{Ref: ref, Kind: CandidateKeyword, Score: h.score})
	}
	return candidates, nil
}

func extensionAddRootPin(pins map[DocumentID]extensionRootResponsePin, pin *extensionRootResponsePin) error {
	return extensionAddRootPinLimit(pins, pin, maxRootResponsePins)
}

func extensionAddRootPinLimit(pins map[DocumentID]extensionRootResponsePin, pin *extensionRootResponsePin, maximum int) error {
	if pin == nil {
		return nil
	}
	if !validString(string(pin.ID), 256) || pin.IdentitySHA256 == [32]byte{} {
		return ErrSourceUnavailable
	}
	if previous, exists := pins[pin.ID]; exists && previous != *pin {
		return ErrSourceUnavailable
	}
	if _, exists := pins[pin.ID]; !exists && len(pins) >= maximum {
		return ErrLimitExceeded
	}
	pins[pin.ID] = *pin
	return nil
}

func extensionSameRootMembers(a, b []SourceRef) bool {
	if len(a) == 0 || len(a) != len(b) || len(a) > maxResponseSources {
		return false
	}
	set := make(map[MessageID]SourceRef, len(a))
	for _, source := range a {
		if _, exists := set[source.Version.MessageID]; exists {
			return false
		}
		set[source.Version.MessageID] = source
	}
	for _, source := range b {
		if remembered, exists := set[source.Version.MessageID]; !exists || source != remembered {
			return false
		}
		delete(set, source.Version.MessageID)
	}
	return len(set) == 0
}

func extensionSameOrderedRootMembers(a, b []SourceRef) bool {
	if !extensionSameRootMembers(a, b) {
		return false
	}
	for i, source := range a {
		if source != b[i] {
			return false
		}
	}
	return true
}

// A verified legacy whole document can retain its original vector and ID.
// Prove that it is exactly the root before returning the same explicit root
// reference as keyword retrieval. Distinct-ID fragments keep their namespace.
func (f *readFrame) extensionCanonicalLegacyRoot(ref, root DocumentRef) (bool, error) {
	var whole int
	err := f.tx.QueryRowContext(f.ctx, `SELECT 1 FROM extension_documents d JOIN extension_roots r ON r.id=d.origin_document_id
 JOIN extension_generations g ON g.id=d.generation AND g.state='active'
 WHERE d.id=? AND r.id=? AND r.legacy_document_id=d.id AND d.id=r.id
 AND d.origin_start_byte=0 AND d.origin_end_byte=r.text_bytes AND r.text_bytes<=16384
 AND d.text=r.text AND d.kind=r.kind AND d.provenance_json=r.provenance_json`, ref.ID, root.ID).Scan(&whole)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, readError(f.ctx, err)
	}
	if !extensionSameOrderedRootMembers(ref.Sources, root.Sources) {
		return false, ErrSourceUnavailable
	}
	// The SQL equality binds the document text to this root. Verify the actual
	// root SHA too before treating two retrieval branches as one identity.
	body, err := f.s.extensionLoadRoot(f.ctx, f.tx, root.ID)
	if err != nil {
		return false, err
	}
	if len(body.Text) > extensionMaxPassageBytes || body.Kind != root.Kind || !extensionSameOrderedRootMembers(body.Sources, root.Sources) {
		return false, ErrSourceUnavailable
	}
	return whole == 1, nil
}

func (f *readFrame) extensionRootFragmentText(ref DocumentRef) (string, error) {
	if ref.root == nil || ref.rootReference {
		return "", ErrSourceUnavailable
	}
	root, err := f.s.extensionLoadRoot(f.ctx, f.tx, ref.root.ID)
	if err != nil {
		return "", err
	}
	if root.Kind != ref.Kind || !extensionSameOrderedRootMembers(root.Sources, ref.Sources) {
		return "", ErrSourceUnavailable
	}
	var start, end int
	if err = f.tx.QueryRowContext(f.ctx, `SELECT origin_start_byte,origin_end_byte FROM extension_documents WHERE id=? AND origin_document_id=? AND embedding_input=0`, ref.ID, ref.root.ID).Scan(&start, &end); errors.Is(err, sql.ErrNoRows) {
		return "", ErrSourceUnavailable
	} else if err != nil {
		return "", readError(f.ctx, err)
	}
	if start < 0 || end <= start || end > len(root.Text) {
		return "", ErrSourceUnavailable
	}
	if end-start > extensionMaxPassageBytes {
		return "", ErrLimitExceeded
	}
	text := root.Text[start:end]
	if !utf8.ValidString(text) {
		return "", ErrSourceUnavailable
	}
	return text, nil
}

func extensionSortedRootPins(pins map[DocumentID]extensionRootResponsePin) []extensionRootResponsePin {
	result := make([]extensionRootResponsePin, 0, len(pins))
	for _, pin := range pins {
		result = append(result, pin)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// The source union is checked once, rather than once per root. This wrapper is
// also used by initial reader admission, which has no prepared fresh map.
func (f *readFrame) extensionCheckRootPins(pins []extensionRootResponsePin, expected map[MessageID]SourceReadVersion) error {
	if len(pins) == 0 {
		return nil
	}
	if len(pins) > maxCandidateRootPins || len(expected) > maxResponseSources {
		return ErrLimitExceeded
	}
	if len(expected) == 0 {
		return ErrSourceUnavailable
	}
	refs := make([]SourceRef, 0, len(expected))
	for _, source := range expected {
		refs = append(refs, source.Ref)
	}
	if _, err := f.s.extensionCheckSourcesAt(f.ctx, f.tx, refs, true, f.now); err != nil {
		return err
	}
	current, err := f.extensionReadSources(expected)
	if err != nil {
		return err
	}
	return f.extensionCheckRootPinsWithSources(pins, expected, current)
}

type extensionRootGuardRecord struct {
	root       extensionRootRecord
	job        sql.NullString
	count      int
	mediaKind  string
	currentSHA string
	outputs    int
}

// All metadata and ordered members come from this same fresh transaction.
// The prepared source map was fully reauthorized (including display names)
// against this transaction before it reaches this helper. Root-specific
// view-once and current-media authority still gate every selected root in SQL.
func (f *readFrame) extensionCheckRootPinsWithSources(pins []extensionRootResponsePin, expected map[MessageID]SourceReadVersion, current []SourceReadVersion) error {
	if len(pins) == 0 {
		return nil
	}
	if len(pins) > maxCandidateRootPins || len(expected) > maxResponseSources || len(current) > maxResponseSources {
		return ErrLimitExceeded
	}
	if len(expected) == 0 || len(current) != len(expected) {
		return ErrSourceUnavailable
	}
	seen := make(map[DocumentID]extensionRootResponsePin, len(pins))
	for _, pin := range pins {
		if err := extensionAddRootPinLimit(seen, &pin, maxCandidateRootPins); err != nil {
			return err
		}
	}
	fresh := make(map[MessageID]SourceReadVersion, len(current))
	for _, source := range current {
		id := source.Ref.Version.MessageID
		if _, exists := fresh[id]; exists || !extensionCandidateSameSource(source, expected[id]) {
			return ErrSourceUnavailable
		}
		fresh[id] = source
	}
	if len(fresh) == 0 || len(fresh) != len(expected) {
		return ErrSourceUnavailable
	}
	ids := make([]DocumentID, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	marks := make([]string, len(ids))
	args := make([]any, 0, len(ids)+len(f.args))
	for i, id := range ids {
		marks[i] = "?"
		args = append(args, id)
	}
	memberArgs := append([]any(nil), args...)
	args = append(args, f.args...)
	// Complete source/media/scope gates precede even bounded provenance decode.
	// No original cache retention check is added: verified roots survive eviction.
	rows, err := f.tx.QueryContext(f.ctx, `SELECT
 CASE WHEN typeof(r.id)='text' AND length(CAST(r.id AS BLOB))<=256 THEN r.id ELSE '' END,
 CASE WHEN r.extraction_job_id IS NULL THEN NULL WHEN typeof(r.extraction_job_id)='text' AND length(CAST(r.extraction_job_id AS BLOB))<=128 THEN r.extraction_job_id ELSE '' END,
 CASE WHEN typeof(r.legacy_document_id)='text' AND length(CAST(r.legacy_document_id AS BLOB))<=256 THEN r.legacy_document_id ELSE '' END,
 CASE WHEN typeof(r.kind)='text' AND length(CAST(r.kind AS BLOB))<=10 THEN r.kind ELSE '' END,
 CASE WHEN typeof(r.text_sha256)='text' AND length(CAST(r.text_sha256 AS BLOB))<=64 THEN r.text_sha256 ELSE '' END,CASE WHEN typeof(r.text_bytes)='integer' THEN r.text_bytes ELSE -1 END,
 CASE WHEN typeof(r.media_id)='text' AND length(CAST(r.media_id AS BLOB))<=128 THEN r.media_id ELSE '' END,
 CASE WHEN typeof(r.provenance_json)='text' AND length(CAST(r.provenance_json AS BLOB))<=4096 THEN r.provenance_json ELSE '' END,CASE WHEN typeof(r.source_count)='integer' THEN r.source_count ELSE -1 END,
 CASE WHEN typeof(media.kind)='text' AND length(CAST(media.kind AS BLOB))<=5 THEN media.kind ELSE '' END,
 CASE WHEN typeof(media.last_original_sha256)='text' AND length(CAST(media.last_original_sha256 AS BLOB))<=64 THEN media.last_original_sha256 ELSE '' END,
 (SELECT count(*) FROM extension_extraction_outputs eo WHERE eo.job_id=r.extraction_job_id AND eo.root_id=r.id)
 FROM extension_roots r JOIN extension_media_associations media ON media.id=r.media_id
 WHERE r.id IN (`+strings.Join(marks, ",")+`) AND `+f.extensionRootPredicate()+` ORDER BY r.id`, args...)
	if err != nil {
		return readError(f.ctx, err)
	}
	records := make(map[DocumentID]*extensionRootGuardRecord, len(ids))
	totalMembers := 0
	for rows.Next() {
		var record extensionRootGuardRecord
		var provenance string
		root := &record.root
		if err = rows.Scan(&root.Origin.ID, &record.job, &root.LegacyDocumentID, &root.Kind, &root.Origin.TextSHA256, &root.Origin.Bytes, &root.MediaID, &provenance, &record.count, &record.mediaKind, &record.currentSHA, &record.outputs); err != nil {
			rows.Close()
			return readError(f.ctx, err)
		}
		if _, exists := seen[root.Origin.ID]; !exists || records[root.Origin.ID] != nil || !validString(string(root.Origin.ID), 256) || !extensionMediaDigest(root.Origin.TextSHA256) || root.Origin.Bytes < 1 || root.Origin.Bytes > extensionMaxDerivedBytes || record.count < 1 || record.count > maxResponseSources || len(provenance) > 4096 || json.Unmarshal([]byte(provenance), &root.Origin.Provenance) != nil || !extensionExtractionProvenance(root.Origin.Provenance, root.Kind, root.MediaID) || !extensionMediaDigest(record.currentSHA) || !((root.Kind == DocumentTranscript && record.mediaKind == "voice") || ((root.Kind == DocumentOCR || root.Kind == DocumentCaption) && record.mediaKind == "image")) {
			rows.Close()
			return ErrSourceUnavailable
		}
		if record.count > maxResponseSources-totalMembers {
			rows.Close()
			return ErrLimitExceeded
		}
		totalMembers += record.count
		canonical, err := json.Marshal(root.Origin.Provenance)
		if err != nil {
			rows.Close()
			return ErrSourceUnavailable
		}
		root.ProvenanceJSON = string(canonical)
		records[root.Origin.ID] = &record
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return readError(f.ctx, err)
	}
	if len(records) != len(seen) {
		return ErrSourceUnavailable
	}
	// The declared total was bounded before any membership payload allocation.
	for _, record := range records {
		record.root.Origin.Sources = make([]SourceRef, 0, record.count)
	}
	rows, err = f.tx.QueryContext(f.ctx, `SELECT root_id,
 CASE WHEN typeof(account)='text' AND length(CAST(account AS BLOB))<=512 THEN account ELSE '' END,
 CASE WHEN typeof(message_id)='text' AND length(CAST(message_id AS BLOB))<=128 THEN message_id ELSE '' END,
 CASE WHEN typeof(local_revision)='integer' AND local_revision>0 THEN local_revision ELSE 0 END,
 CASE WHEN typeof(policy_generation)='integer' AND policy_generation>0 THEN policy_generation ELSE 0 END,
 CASE WHEN typeof(content_hash)='text' AND length(CAST(content_hash AS BLOB))<=64 THEN content_hash ELSE '' END,
 CASE WHEN typeof(expires_at)='integer' THEN expires_at ELSE 0 END,
 CASE WHEN typeof(ordinal)='integer' THEN ordinal ELSE -1 END,
 (typeof(local_revision)='integer' AND typeof(policy_generation)='integer' AND typeof(expires_at)='integer' AND typeof(ordinal)='integer')
 FROM extension_root_sources WHERE root_id IN (`+strings.Join(marks, ",")+`) ORDER BY root_id,ordinal LIMIT 2049`, memberArgs...)
	if err != nil {
		return readError(f.ctx, err)
	}
	copied := 0
	for rows.Next() {
		var id DocumentID
		var ref SourceRef
		var expiry int64
		var ordinal int
		var numericMember bool
		if err = rows.Scan(&id, &ref.Account, &ref.Version.MessageID, &ref.Version.Revision, &ref.Version.PolicyGeneration, &ref.ContentHash, &expiry, &ordinal, &numericMember); err != nil {
			rows.Close()
			return readError(f.ctx, err)
		}
		ref.Version.ExpiresAt = fromMillis(expiry)
		record := records[id]
		source, exists := fresh[ref.Version.MessageID]
		if !numericMember || record == nil || copied >= totalMembers || copied >= maxResponseSources || ordinal != len(record.root.Origin.Sources) || ordinal >= record.count || !validExtensionMediaSource(ref) || !exists || source.Ref != ref {
			rows.Close()
			return ErrSourceUnavailable
		}
		record.root.Origin.Sources = append(record.root.Origin.Sources, ref)
		copied++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return readError(f.ctx, err)
	}
	if copied != totalMembers {
		return ErrSourceUnavailable
	}
	// Reuse each immutable extraction job's existing bounded/hash-checked loader.
	// Legacy roots need no job SELECT; roots from one job share one verification.
	jobs := make(map[JobID]extensionStoredExtraction)
	for _, id := range ids {
		record := records[id]
		root := &record.root
		if len(root.Origin.Sources) != record.count {
			return ErrSourceUnavailable
		}
		if record.job.Valid {
			root.ExtractionJobID = JobID(record.job.String)
			if !validReadID(string(root.ExtractionJobID)) {
				return ErrSourceUnavailable
			}
			x, exists := jobs[root.ExtractionJobID]
			if !exists {
				x, err = extensionLoadExtraction(f.ctx, f.tx, root.ExtractionJobID)
				if err != nil {
					return err
				}
				jobs[root.ExtractionJobID] = x
			}
			if root.LegacyDocumentID != "" || record.count != 1 || x.state != "complete" || x.outcome != ExtractionText || record.currentSHA != x.work.OriginalSHA256 || !extensionMediaDigest(x.completion) || x.work.MediaID != root.MediaID || !extensionExtractionSameSource(root.Origin.Sources[0], x.work.Source) || extensionExtractionDocumentKind(x.work.Version.Kind) != root.Kind || root.Origin.Provenance.ProfileID != x.work.Version.ProfileID || root.Origin.Provenance.ExtractorVersion != x.work.Version.ExtractorVersion || root.Origin.Provenance.PromptVersion != x.work.Version.PromptVersion || record.outputs != 1 {
				return ErrSourceUnavailable
			}
			root.OriginalSHA256 = x.work.OriginalSHA256
			root.VersionHash = x.versionHash
		} else if !validString(string(root.LegacyDocumentID), 256) {
			return ErrSourceUnavailable
		} else {
			root.OriginalSHA256 = record.currentSHA
		}
		pin, err := extensionRootIdentity(*root)
		if err != nil || pin != seen[id] {
			return ErrSourceUnavailable
		}
		if err = f.s.extensionRegisterRoot(f.tx, *root); err != nil {
			return err
		}
	}
	return nil
}
