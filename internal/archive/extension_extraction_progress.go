package archive

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ExtractionProgress reports one configured extractor's current, authorized
// work. An eligible retained original counts as pending before a worker claims
// it. Completed outcomes survive ordinary original-cache eviction.
// Authentication and the final credential response fence remain the caller's
// responsibility, as for Status; ReadScope is server-owned authority.
func (s *Store) ExtractionProgress(ctx context.Context, scope ReadScope, version ExtractionVersion) (Progress, error) {
	f, err := s.beginRead(ctx, scope, nil, time.Time{}, time.Time{})
	if err != nil {
		return Progress{}, err
	}
	defer f.close()
	progress, err := s.extractionProgressFrame(f, version)
	if err != nil {
		return Progress{}, err
	}
	if err = f.finish(); err != nil {
		return Progress{}, err
	}
	return progress, nil
}

// StatusWithExtractionGuarded binds configured extraction counts and status
// metadata to the same authorized snapshot and final response clock horizon.
func (s *Store) StatusWithExtractionGuarded(ctx context.Context, scope ReadScope, version ExtractionVersion) (StatusSnapshot, ResponseGuard, error) {
	var guard ResponseGuard
	status, err := s.statusPage(ctx, scope, &guard, &version)
	if err != nil {
		return StatusSnapshot{}, ResponseGuard{}, err
	}
	return status, guard, nil
}

func (s *Store) StatusWithExtraction(ctx context.Context, scope ReadScope, version ExtractionVersion) (StatusSnapshot, error) {
	return s.statusPage(ctx, scope, nil, &version)
}

func (s *Store) extractionProgressFrame(f *readFrame, version ExtractionVersion) (Progress, error) {
	encoded, digest, err := extensionExtractionVersionJSON(version)
	if err != nil {
		return Progress{}, err
	}
	var stored, storedDigest string
	err = f.tx.QueryRowContext(f.ctx, `SELECT
	 CASE WHEN typeof(version_json)='text' AND length(CAST(version_json AS BLOB))<=4096 THEN version_json ELSE '' END,
	 CASE WHEN typeof(version_hash)='text' AND length(CAST(version_hash AS BLOB))=64 THEN version_hash ELSE '' END
	 FROM extension_extraction_versions WHERE id=?`, version.ID).Scan(&stored, &storedDigest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Progress{}, readError(f.ctx, err)
	}
	if err == nil && (stored != encoded || storedDigest != digest) {
		return Progress{}, ErrInvalidExtension
	}
	kind := "image"
	if version.Kind == JobTranscription {
		kind = "voice"
	}
	// The existing successful-outcome predicate validates output membership,
	// source pins and extractor provenance. Cache bytes are required for pending
	// work, but are not continued authority over already committed transcripts.
	complete := extensionMediaExtractionDoneSQL("'" + string(version.Kind) + "'")
	replacements := [][2]string{
		{"WHERE x.media_id=l.id", "WHERE x.version_id=? AND x.media_id=l.id"},
		{" AND x.original_sha256=(SELECT original.sha256 FROM extension_media_artifacts original WHERE original.id=l.artifact_id)", ""},
		{"AND x.input_pinned=0 AND x.state='complete'", "AND x.input_pinned=0 AND x.state='complete' AND length(x.completion_digest)=64 AND x.completion_digest NOT GLOB '*[^0-9a-f]*'"},
		{"r.id IS NULL OR r.extraction_job_id", "r.id IS NULL OR NOT (" + extensionRootMetadataPredicate + " AND " + extensionRootMediaPredicate + ") OR r.extraction_job_id"},
	}
	for _, replacement := range replacements {
		// A later edit to the shared predicate must fail closed rather than
		// silently dropping configured-version or root-authority checks.
		if strings.Count(complete, replacement[0]) != 1 {
			return Progress{}, ErrInvalidExtension
		}
		complete = strings.Replace(complete, replacement[0], replacement[1], 1)
	}
	query := `SELECT
	 COALESCE(SUM(CASE WHEN a.state='retained' AND a.budget='originals' AND a.cleanup_ack=0
	 AND a.sha256=l.last_original_sha256 AND (e.id IS NULL OR e.state IN ('pending','leased')) THEN 1 ELSE 0 END),0),
	 COALESCE(SUM(CASE WHEN ` + complete + ` THEN 1 ELSE 0 END),0),
	 COALESCE(SUM(CASE WHEN e.state='failed' THEN 1 ELSE 0 END),0),
	 MIN(m.original_sent_at),MIN(NULLIF(m.expires_at,0))
	 FROM ` + authorizedTables + `
	 JOIN extension_media_associations l ON l.message_id=m.id AND l.account=m.account
	 AND l.local_revision=m.local_revision AND l.policy_generation=ap.generation
	 AND l.content_hash=m.content_hash AND l.source_expires_at=m.expires_at
	 LEFT JOIN extension_media_artifacts a ON a.id=l.artifact_id
	 LEFT JOIN extension_extraction_jobs e ON e.version_id=? AND e.kind=? AND e.media_id=l.id
	 AND e.account=l.account AND e.message_id=l.message_id AND e.local_revision=l.local_revision
	 AND e.policy_generation=l.policy_generation AND e.content_hash=l.content_hash
	 AND e.source_expires_at=l.source_expires_at AND e.original_sha256=l.last_original_sha256
	 WHERE ` + f.where + ` AND l.active=1 AND l.view_once=0 AND m.view_once=0 AND l.kind=?`
	args := append([]any{version.ID, version.ID, version.Kind}, f.args...)
	args = append(args, kind)
	progress := Progress{State: "ready"}
	var minSent, minExpiry sql.NullInt64
	if err = f.tx.QueryRowContext(f.ctx, query, args...).Scan(&progress.Pending, &progress.Complete, &progress.Failed, &minSent, &minExpiry); err != nil {
		return Progress{}, readError(f.ctx, err)
	}
	if minSent.Valid {
		f.note(fromMillis(minSent.Int64), fromMillis(minExpiry.Int64))
	}
	if progress.Pending > 0 {
		progress.State = "pending"
	} else if progress.Failed > 0 {
		progress.State = "failed"
	}
	return progress, nil
}
