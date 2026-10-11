package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const extensionDerivedSeedDocuments = 16

// Original roots are immutable extraction outputs, independent of encoder
// generations. Their complete source membership survives original-file eviction.
func extensionDerivedRootsSQL() string {
	return `FROM extension_documents d JOIN extension_document_sources ds ON ds.document_id=d.id
	 JOIN messages m ON m.id=ds.message_id JOIN archive_policy p ON p.id=1 JOIN account_safety ac ON ac.account=m.account
	 WHERE d.kind IN('ocr','caption','transcript') AND d.origin_document_id=d.id
	 AND d.origin_start_byte=0 AND d.origin_end_byte=length(CAST(d.text AS BLOB)) AND d.origin_end_byte>0
	 AND d.source_count=1 AND (SELECT count(*) FROM extension_document_sources all_sources WHERE all_sources.document_id=d.id)=1
	 AND ds.account=m.account AND ds.local_revision=m.local_revision AND ds.policy_generation=p.generation
	 AND ds.expires_at=m.expires_at AND ds.content_hash=m.content_hash AND ac.unsafe=0 AND m.view_once=0 AND ` + extensionSourcePredicate()
}

// Ordered adjacency proves a partition; a sum alone permits overlap and gaps
// to cancel. These queries inspect metadata, never load an archive of bodies.
const extensionDerivedPartitionSQL = `WITH ordered_fragments AS (
	 SELECT f.generation,f.origin_document_id,f.origin_start_byte AS start_byte,f.origin_end_byte AS end_byte,
	 length(CAST(f.text AS BLOB)) AS text_bytes,f.passage_tokens,
		 v.document_id AS vector_id,v.generation AS vector_generation,v.dimensions,length(v.values_blob) AS vector_bytes,
	 json_extract(g.version_json,'$.Dimensions') AS expected_dimensions,json_extract(g.version_json,'$.MaxPassageTokens') AS max_tokens,
	 LAG(f.origin_end_byte) OVER(PARTITION BY f.generation,f.origin_document_id ORDER BY f.origin_start_byte,f.origin_end_byte,f.id) AS previous_end
	 FROM extension_documents f JOIN extension_generations g ON g.id=f.generation LEFT JOIN extension_vectors v ON v.document_id=f.id
	 WHERE f.embedding_input=0 AND f.kind IN('ocr','caption','transcript') AND f.origin_document_id<>''
	 AND (?='' OR f.generation=?) AND (?='' OR f.origin_document_id=?) AND g.state IN('active','building')
), complete_origins AS (
	 SELECT generation,origin_document_id,MAX(end_byte) AS end_byte FROM ordered_fragments
	 GROUP BY generation,origin_document_id
	 HAVING MIN(start_byte)=0 AND MAX(CASE WHEN start_byte<0 OR end_byte<=start_byte OR end_byte-start_byte<>text_bytes
	 OR(previous_end IS NOT NULL AND previous_end<>start_byte) OR passage_tokens<1 OR passage_tokens>max_tokens
	 OR expected_dimensions IS NULL OR max_tokens IS NULL OR vector_id IS NULL OR vector_generation<>generation
	 OR dimensions<>expected_dimensions OR vector_bytes<>expected_dimensions*4 THEN 1 ELSE 0 END)=0
)
`

// Every root member is current before metadata selection and limits. Bodies
// are materialized only by bounded legacy verification or range loading.
func extensionCurrentRootSQL() string {
	return `FROM extension_roots r WHERE r.source_count=(SELECT count(*) FROM extension_root_sources rs WHERE rs.root_id=r.id)
 AND NOT EXISTS(SELECT 1 FROM extension_root_sources rs WHERE rs.root_id=r.id AND NOT EXISTS(
 SELECT 1 FROM messages m JOIN archive_policy p ON p.id=1 JOIN account_safety ac ON ac.account=m.account
 WHERE m.id=rs.message_id AND m.account=rs.account AND m.local_revision=rs.local_revision AND m.content_hash=rs.content_hash
 AND m.expires_at=rs.expires_at AND p.generation=rs.policy_generation AND ac.unsafe=0 AND m.view_once=0 AND ` + extensionSourcePredicate() + `))
 AND EXISTS(SELECT 1 FROM extension_root_sources rs JOIN extension_media_associations ma ON ma.id=r.media_id AND ma.account=rs.account AND ma.message_id=rs.message_id
 AND ma.local_revision=rs.local_revision AND ma.policy_generation=rs.policy_generation AND ma.content_hash=rs.content_hash AND ma.source_expires_at=rs.expires_at
 WHERE rs.root_id=r.id AND ma.active=1 AND ma.view_once=0 AND ((r.kind='transcript' AND ma.kind='voice') OR(r.kind<>'transcript' AND ma.kind='image')))`
}
func extensionRootInventorySQL(generation string) string {
	return `NOT EXISTS(SELECT 1 FROM extension_job_origins jo JOIN extension_jobs j ON j.id=jo.job_id WHERE jo.root_id=r.id AND j.generation=` + generation + ` AND j.state IN('pending','leased','complete','failed'))`
}

// A truthful legacy direct-vector completion is accepted only under its exact
// existing encoder version; no old vector is copied into a replacement.
func extensionLegacyRootCompleteSQL(generation string) string {
	return `EXISTS(SELECT 1 FROM extension_documents ld JOIN extension_vectors lv ON lv.document_id=ld.id JOIN extension_generations lg ON lg.id=ld.generation
 WHERE r.legacy_document_id<>'' AND ld.id=r.legacy_document_id AND ld.generation=` + generation + ` AND ld.origin_document_id=r.id
 AND ld.origin_start_byte=0 AND ld.origin_end_byte=r.text_bytes AND length(CAST(ld.text AS BLOB))=r.text_bytes AND ld.passage_tokens BETWEEN 1 AND json_extract(lg.version_json,'$.MaxPassageTokens')
 AND lv.generation=ld.generation AND lv.dimensions=json_extract(lg.version_json,'$.Dimensions') AND length(lv.values_blob)=lv.dimensions*4
 AND EXISTS(SELECT 1 FROM extension_job_documents jd JOIN extension_jobs lj ON lj.id=jd.job_id WHERE jd.document_id=ld.id AND lj.kind='derived_embedding' AND lj.state='complete' AND lj.generation=ld.generation))`
}
func (s *Store) extensionDerivedCoverageAt(ctx context.Context, q sqlQuerier, g IndexGeneration, now time.Time) error {
	p, e := s.loadPolicy(ctx, q)
	if e != nil {
		return e
	}
	var missing int
	e = q.QueryRowContext(ctx, extensionDerivedPartitionSQL+`SELECT EXISTS(SELECT 1 `+extensionCurrentRootSQL()+`
 AND (NOT EXISTS(SELECT 1 FROM complete_origins c WHERE c.generation=? AND c.origin_document_id=r.id AND c.end_byte=r.text_bytes)
 OR (NOT `+extensionLegacyRootCompleteSQL("?")+` AND NOT EXISTS(SELECT 1 FROM extension_job_origins jo JOIN extension_jobs j ON j.id=jo.job_id WHERE jo.root_id=r.id AND j.generation=? AND j.state='complete'))))`, g.ID, g.ID, "", "", unixms(now), ceilMillis(now.Add(-p.RollingRetention)), g.ID, g.ID, g.ID).Scan(&missing)
	if e != nil {
		return fixedSQLError(e)
	}
	if missing != 0 {
		return ErrNoWork
	}
	return nil
}
func (s *Store) extensionSeedNextDerivedJobs(ctx context.Context, tx *sql.Tx, account string) error {
	if e := s.extensionRecoverLegacyRoots(ctx, tx, account); e != nil {
		return e
	}
	p, e := s.loadPolicy(ctx, tx)
	if e != nil {
		return e
	}
	var id IndexGenerationID
	now := s.now()
	e = tx.QueryRowContext(ctx, `SELECT g.id FROM extension_generations g WHERE g.state IN('active','building') AND EXISTS(SELECT 1 `+extensionCurrentRootSQL()+`
 AND EXISTS(SELECT 1 FROM extension_root_sources rs WHERE rs.root_id=r.id AND rs.account=?) AND `+extensionRootInventorySQL("g.id")+` AND NOT `+extensionLegacyRootCompleteSQL("g.id")+`) ORDER BY g.created_at,g.id LIMIT 1`, unixms(now), ceilMillis(now.Add(-p.RollingRetention)), account).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return fixedSQLError(e)
	}
	return s.extensionSeedRootJobs(ctx, tx, id, account)
}
func (s *Store) extensionSeedDerivedJobs(ctx context.Context, tx *sql.Tx, id IndexGenerationID, account string) error {
	if e := s.extensionRecoverLegacyRoots(ctx, tx, account); e != nil {
		return e
	}
	return s.extensionSeedRootJobs(ctx, tx, id, account)
}
func (s *Store) extensionSeedRootJobs(ctx context.Context, tx *sql.Tx, id IndexGenerationID, account string) error {
	g, e := extensionLoadGeneration(ctx, tx, id)
	if e != nil {
		return e
	}
	if g.State == "retired" {
		return nil
	}
	if e = s.extensionRegisterGeneration(tx, g); e != nil {
		return e
	}
	p, e := s.loadPolicy(ctx, tx)
	if e != nil {
		return e
	}
	now := s.now()
	rows, e := tx.QueryContext(ctx, `SELECT r.id,r.text_bytes,r.source_count `+extensionCurrentRootSQL()+`
 AND (?='' OR EXISTS(SELECT 1 FROM extension_root_sources rs WHERE rs.root_id=r.id AND rs.account=?)) AND `+extensionRootInventorySQL("?")+` AND NOT `+extensionLegacyRootCompleteSQL("?")+` ORDER BY r.id LIMIT 16`, unixms(now), ceilMillis(now.Add(-p.RollingRetention)), account, account, id, id)
	if e != nil {
		return fixedSQLError(e)
	}
	var ids []DocumentID
	bytes, refs := 0, 0
	for rows.Next() {
		var root DocumentID
		var size, count int
		if e = rows.Scan(&root, &size, &count); e != nil {
			rows.Close()
			return fixedSQLError(e)
		}
		if size < 1 || size > extensionMaxDerivedBytes || count < 1 || count > extensionMaxSources {
			rows.Close()
			return ErrLimitExceeded
		}
		if bytes+size > extensionMaxDerivedBytes || refs+count > extensionMaxSources {
			break
		}
		bytes += size
		refs += count
		ids = append(ids, root)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return fixedSQLError(e)
	}
	for _, root := range ids {
		origin, e := s.extensionAuthorizeRoot(ctx, tx, root)
		if extensionSuperseded(e) {
			continue
		}
		if e != nil {
			return e
		}
		if e = extensionEnqueueOriginJob(ctx, tx, g, origin); e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) extensionEnqueueRootJobs(ctx context.Context, tx *sql.Tx, id DocumentID) error {
	origin, e := s.extensionAuthorizeRoot(ctx, tx, id)
	if e != nil {
		return e
	}
	var generation IndexGenerationID
	e = tx.QueryRowContext(ctx, `SELECT g.id FROM extension_generations g WHERE g.state IN('active','building') AND NOT EXISTS(SELECT 1 FROM extension_job_origins jo JOIN extension_jobs j ON j.id=jo.job_id WHERE jo.root_id=? AND j.generation=g.id AND j.state IN('pending','leased','complete','failed')) ORDER BY g.created_at,g.id LIMIT 1`, id).Scan(&generation)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return fixedSQLError(e)
	}
	g, e := extensionLoadGeneration(ctx, tx, generation)
	if e != nil {
		return e
	}
	if e = s.extensionRegisterGeneration(tx, g); e != nil {
		return e
	}
	return extensionEnqueueOriginJob(ctx, tx, g, origin)
}
func extensionEnqueueOriginJob(ctx context.Context, tx *sql.Tx, g IndexGeneration, origin DerivedOrigin) error {
	hash, e := CanonicalOriginWorkDigest(g.ID, g.Version, origin)
	if e != nil {
		return e
	}
	account := origin.Sources[0].Account
	for _, ref := range origin.Sources {
		if ref.Account != account {
			return ErrInvalidExtension
		}
	}
	inserted, e := tx.ExecContext(ctx, `INSERT INTO extension_jobs(id,account,kind,generation,media_id,content_hash,state) VALUES(?,?,'derived_embedding',?,?,?,'pending') ON CONFLICT(id) DO NOTHING`, hash, account, g.ID, origin.Provenance.MediaID, hash)
	if e != nil {
		return fixedSQLError(e)
	}
	n, e := inserted.RowsAffected()
	if e != nil {
		return fixedSQLError(e)
	}
	if n == 0 {
		x, e := extensionLoadJob(ctx, tx, JobID(hash))
		if e != nil {
			return e
		}
		if x.job.Kind != JobDerivedEmbedding || x.account != account || x.job.Generation != g.ID || x.job.Version != g.Version || x.job.MediaID != origin.Provenance.MediaID || x.job.ContentHash != hash || !extensionSameSources(x.job.Sources, origin.Sources) || len(x.job.Documents) > 1 || len(x.job.Documents) == 1 && x.job.Documents[0] != origin.ID {
			return ErrSchemaChanged
		}
		var storedRoot DocumentID
		var storedHash string
		e = tx.QueryRowContext(ctx, "SELECT root_id,origin_hash FROM extension_job_origins WHERE job_id=?", hash).Scan(&storedRoot, &storedHash)
		if errors.Is(e, sql.ErrNoRows) {
			if _, e = tx.ExecContext(ctx, "INSERT INTO extension_job_origins(job_id,root_id,origin_hash) VALUES(?,?,?)", hash, origin.ID, origin.TextSHA256); e != nil {
				return fixedSQLError(e)
			}
		} else if e != nil {
			return fixedSQLError(e)
		} else if storedRoot != origin.ID || storedHash != origin.TextSHA256 {
			return ErrSchemaChanged
		}
		if x.state == "complete" {
			complete, e := extensionOriginPartitionComplete(ctx, tx, g.ID, origin.ID, origin.Bytes)
			if e != nil {
				return e
			}
			if complete {
				return nil
			}
		} else if x.state != "cancelled" {
			return nil
		}
		_, e = tx.ExecContext(ctx, `UPDATE extension_jobs SET state='pending',attempts=0,retry_at=0,lease_id='',lease_until=0,last_code='' WHERE id=?`, hash)
		return fixedSQLError(e)
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO extension_job_origins(job_id,root_id,origin_hash) VALUES(?,?,?)`, hash, origin.ID, origin.TextSHA256); e != nil {
		return fixedSQLError(e)
	}
	for i, ref := range origin.Sources {
		if _, e = tx.ExecContext(ctx, `INSERT INTO extension_job_sources(job_id,account,message_id,local_revision,policy_generation,expires_at,content_hash,ordinal) VALUES(?,?,?,?,?,?,?,?)`, hash, ref.Account, ref.Version.MessageID, ref.Version.Revision, ref.Version.PolicyGeneration, unixms(ref.Version.ExpiresAt), ref.ContentHash, i); e != nil {
			return fixedSQLError(e)
		}
	}
	return nil
}
func (s *Store) extensionRecoverLegacyRoots(ctx context.Context, tx *sql.Tx, account string) error {
	p, e := s.loadPolicy(ctx, tx)
	if e != nil {
		return e
	}
	now := s.now()
	rows, e := tx.QueryContext(ctx, `SELECT d.id,length(CAST(d.text AS BLOB)),d.source_count,length(CAST(d.provenance_json AS BLOB)),length(CAST(d.spans_json AS BLOB)) `+extensionDerivedRootsSQL()+`
	 AND (?='' OR m.account=?) AND NOT EXISTS(SELECT 1 FROM extension_roots r WHERE r.legacy_document_id=d.id)
	 AND EXISTS(SELECT 1 FROM extension_media_associations ma WHERE ma.id=CASE WHEN json_valid(d.provenance_json) THEN json_extract(d.provenance_json,'$.MediaID') ELSE NULL END AND ma.account=ds.account AND ma.message_id=ds.message_id AND ma.local_revision=ds.local_revision AND ma.policy_generation=ds.policy_generation AND ma.content_hash=ds.content_hash AND ma.source_expires_at=ds.expires_at AND ma.active=1 AND ma.view_once=0 AND length(ma.last_original_sha256)=64 AND ma.last_original_sha256 NOT GLOB '*[^0-9a-fA-F]*' AND NOT EXISTS(SELECT 1 FROM extension_media_artifacts a WHERE a.id=ma.artifact_id AND a.state='retained' AND a.cleanup_ack=0 AND a.sha256<>ma.last_original_sha256)) ORDER BY d.id LIMIT 16`, unixms(now), ceilMillis(now.Add(-p.RollingRetention)), account, account)
	if e != nil {
		return fixedSQLError(e)
	}
	var ids []DocumentID
	bytes, refs := 0, 0
	for rows.Next() {
		var id DocumentID
		var size, count, provenance, spans int
		if e = rows.Scan(&id, &size, &count, &provenance, &spans); e != nil {
			rows.Close()
			return fixedSQLError(e)
		}
		if size < 1 || size > extensionMaxPassageBytes || provenance > 4096 || spans > 32 || count < 1 || count > extensionMaxSources {
			rows.Close()
			return ErrLimitExceeded
		}
		if bytes+size > extensionMaxDerivedBytes || refs+count > extensionMaxSources {
			break
		}
		bytes += size
		refs += count
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return fixedSQLError(e)
	}
	changed := false
	for _, id := range ids {
		d, e := extensionLoadDocument(ctx, tx, id)
		if e != nil {
			return e
		}
		if !extensionReusableDerivedDocument(d) || strings.ContainsRune(d.Text, 0) {
			return ErrInvalidExtension
		}
		if _, e = s.extensionCheckSources(ctx, tx, d.Sources, true); e != nil {
			return e
		}
		var media string
		var marker, retained sql.NullString
		e = tx.QueryRowContext(ctx, `SELECT id,last_original_sha256,(SELECT sha256 FROM extension_media_artifacts a WHERE a.id=ma.artifact_id AND a.state='retained' AND a.cleanup_ack=0) FROM extension_media_associations ma WHERE ma.id=? AND ma.active=1 AND ma.view_once=0 AND EXISTS(SELECT 1 FROM extension_document_sources ds WHERE ds.document_id=? AND ds.account=ma.account AND ds.message_id=ma.message_id AND ds.local_revision=ma.local_revision AND ds.policy_generation=ma.policy_generation AND ds.content_hash=ma.content_hash AND ds.expires_at=ma.source_expires_at)`, d.Provenance.MediaID, d.ID).Scan(&media, &marker, &retained)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return fixedSQLError(e)
		}
		if !marker.Valid || !extensionMediaDigest(marker.String) || retained.Valid && marker.String != retained.String {
			continue
		}
		provenance, e := json.Marshal(d.Provenance)
		if e != nil {
			return ErrInvalidExtension
		}
		hash := sha256.Sum256([]byte(d.Text))
		if _, e = tx.ExecContext(ctx, `INSERT INTO extension_roots(id,legacy_document_id,kind,text,text_sha256,text_bytes,media_id,provenance_json,source_count,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, d.ID, d.ID, d.Kind, d.Text, hex.EncodeToString(hash[:]), len(d.Text), d.Provenance.MediaID, string(provenance), len(d.Sources), unixms(now)); e != nil {
			return fixedSQLError(e)
		}
		for i, ref := range d.Sources {
			if _, e = tx.ExecContext(ctx, `INSERT INTO extension_root_sources(root_id,account,message_id,local_revision,policy_generation,content_hash,expires_at,ordinal) VALUES(?,?,?,?,?,?,?,?)`, d.ID, ref.Account, ref.Version.MessageID, ref.Version.Revision, ref.Version.PolicyGeneration, ref.ContentHash, unixms(ref.Version.ExpiresAt), i); e != nil {
				return fixedSQLError(e)
			}
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO extension_root_fts(rowid,root_id,text) SELECT rowid,id,? FROM extension_roots WHERE id=?`, NormalizeLexical(d.Text), d.ID); e != nil {
			return fixedSQLError(e)
		}
		// Same-generation legacy vectors can already satisfy semantic work, so
		// recovery must pin root/media authority even without later job seeding.
		if _, e = s.extensionAuthorizeRoot(ctx, tx, d.ID); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE extension_jobs SET state='cancelled',lease_id='',lease_until=0,last_code='legacy_converted' WHERE kind='derived_embedding' AND state IN('pending','leased') AND id IN(SELECT jd.job_id FROM extension_job_documents jd JOIN extension_documents input ON input.id=jd.document_id WHERE input.origin_document_id=?)`, d.ID); e != nil {
			return fixedSQLError(e)
		}
		changed = true
	}
	if changed {
		_, e = bumpRevision(ctx, tx)
		return e
	}
	return nil
}

func extensionReusableDerivedDocument(d Document) bool {
	var kind JobKind
	switch d.Kind {
	case DocumentOCR:
		kind = JobOCR
	case DocumentCaption:
		kind = JobCaption
	case DocumentTranscript:
		kind = JobTranscription
	default:
		return false
	}
	return extensionDerivedDocumentValid(d, Job{Kind: kind, MediaID: d.Provenance.MediaID, Sources: d.Sources, Version: IndexVersion{MaxPassageTokens: 256}})
}

type extensionDerivedInputState struct {
	origin     DocumentID
	start, end int
	hidden     bool
}

func extensionLoadDerivedInputState(ctx context.Context, q sqlQuerier, id DocumentID) (extensionDerivedInputState, error) {
	var state extensionDerivedInputState
	err := q.QueryRowContext(ctx, `SELECT origin_document_id,origin_start_byte,origin_end_byte,embedding_input FROM extension_documents WHERE id=?`, id).Scan(&state.origin, &state.start, &state.end, &state.hidden)
	if errors.Is(err, sql.ErrNoRows) {
		return state, ErrNoWork
	}
	return state, fixedSQLError(err)
}

func extensionDerivedHiddenComplete(ctx context.Context, q sqlQuerier, j Job) (bool, error) {
	if j.Kind != JobDerivedEmbedding || len(j.Documents) != 1 {
		return false, nil
	}
	state, err := extensionLoadDerivedInputState(ctx, q, j.Documents[0])
	if err != nil {
		return false, err
	}
	if !state.hidden {
		return false, nil
	}
	var found int
	err = q.QueryRowContext(ctx, extensionDerivedPartitionSQL+`SELECT EXISTS(SELECT 1 FROM complete_origins WHERE generation=? AND origin_document_id=? AND end_byte=?)`, j.Generation, j.Generation, state.origin, state.origin, j.Generation, state.origin, state.end).Scan(&found)
	return found == 1, fixedSQLError(err)
}

func extensionOriginPartitionComplete(ctx context.Context, q sqlQuerier, generation IndexGenerationID, root DocumentID, bytes int) (bool, error) {
	var complete int
	err := q.QueryRowContext(ctx, extensionDerivedPartitionSQL+`SELECT EXISTS(SELECT 1 FROM complete_origins WHERE generation=? AND origin_document_id=? AND end_byte=?)`, generation, generation, root, root, generation, root, bytes).Scan(&complete)
	return complete == 1, fixedSQLError(err)
}

func extensionCommitDerivedFragments(ctx context.Context, tx *sql.Tx, g IndexGeneration, j Job, input Document, r JobResult) error {
	state, err := extensionLoadDerivedInputState(ctx, tx, input.ID)
	if err != nil {
		return err
	}
	if !state.hidden || input.PassageTokens != 0 || state.origin == "" || state.start != 0 || state.end != len(input.Text) || !extensionReusableDerivedDocument(input) || len(r.Documents) == 0 || len(r.Documents) > extensionMaxDocuments || len(r.Embeddings) != len(r.Documents) {
		return ErrInvalidExtension
	}
	start := 0
	vectors := make(map[DocumentID][]float32, len(r.Embeddings))
	for _, v := range r.Embeddings {
		if vectors[v.DocumentID] != nil || !extensionVectorValid(v.Values, g.Version.Dimensions) {
			return ErrInvalidExtension
		}
		vectors[v.DocumentID] = v.Values
	}
	for _, d := range r.Documents {
		end := start + len(d.Text)
		expected := DocumentID(fmt.Sprintf("%s-%d-%d", j.ID, start, end))
		if d.ID != expected || len(d.Text) == 0 || end > len(input.Text) || input.Text[start:end] != d.Text || d.Kind != input.Kind || d.Provenance != input.Provenance || len(d.Spans) != 0 || !extensionSameSources(d.Sources, input.Sources) || d.PassageTokens < 1 || d.PassageTokens > g.Version.MaxPassageTokens || vectors[d.ID] == nil {
			return ErrInvalidExtension
		}
		start = end
	}
	if start != len(input.Text) {
		return ErrInvalidExtension
	}
	// Validate the complete replacement before deleting any previous own
	// fragments. Roots and hidden copies retain immutable identities/content.
	rows, err := tx.QueryContext(ctx, `SELECT id FROM extension_documents WHERE generation=? AND origin_document_id=? AND embedding_input=0 LIMIT 257`, g.ID, state.origin)
	if err != nil {
		return fixedSQLError(err)
	}
	var old []DocumentID
	for rows.Next() {
		var id DocumentID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return fixedSQLError(err)
		}
		old = append(old, id)
		if len(old) > extensionMaxDocuments {
			rows.Close()
			return ErrLimitExceeded
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fixedSQLError(err)
	}
	for _, id := range old {
		if err = extensionDeleteDocument(ctx, tx, id); err != nil {
			return err
		}
	}
	start = 0
	for _, d := range r.Documents {
		end := start + len(d.Text)
		if err = extensionSaveDocument(ctx, tx, g, d); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE extension_documents SET origin_document_id=?,origin_start_byte=?,origin_end_byte=? WHERE id=? AND generation=? AND embedding_input=0`, state.origin, start, end, d.ID, g.ID); err != nil {
			return fixedSQLError(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO extension_vectors(document_id,generation,dimensions,values_blob) VALUES(?,?,?,?)`, d.ID, g.ID, g.Version.Dimensions, extensionVectorBlob(vectors[d.ID])); err != nil {
			return fixedSQLError(err)
		}
		start = end
	}
	return nil
}
