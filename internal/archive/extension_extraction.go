package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

type extensionExtractionOperation uint8

const (
	extensionExtractionClaim extensionExtractionOperation = iota
	extensionExtractionLoad
	extensionExtractionRenew
	extensionExtractionCommit
	extensionExtractionRetry
)

type extensionExtractionCommand struct {
	op     extensionExtractionOperation
	claim  ExtractionClaim
	lease  JobLease
	ttl    time.Duration
	result ExtractionResult
	retry  JobRetry
}

func (c extensionExtractionCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c extensionExtractionCommand) priority() bool {
	return c.op == extensionExtractionRenew || c.op == extensionExtractionRetry
}
func (c extensionExtractionCommand) account() string {
	if c.op == extensionExtractionClaim {
		return c.claim.Account
	}
	return ""
}
func (c extensionExtractionCommand) run(s *Store, ctx context.Context) (any, error) {
	if c.op == extensionExtractionRetry {
		return nil, s.transaction(ctx, func(tx *sql.Tx) error { return s.extensionRetryExtraction(ctx, tx, c.retry) })
	}
	var value any
	err := s.extensionTransaction(ctx, c.account(), func(tx *sql.Tx) error {
		var err error
		switch c.op {
		case extensionExtractionClaim:
			value, err = s.extensionClaimExtraction(ctx, tx, c.claim)
		case extensionExtractionLoad:
			value, err = s.extensionLoadExtractionInput(ctx, tx, c.lease)
		case extensionExtractionRenew:
			value, err = s.extensionRenewExtraction(ctx, tx, c.lease, c.ttl)
		case extensionExtractionCommit:
			value, err = s.extensionCommitExtraction(ctx, tx, c.result)
		default:
			err = ErrInvalidExtension
		}
		return err
	})
	if c.op == extensionExtractionCommit && (errors.Is(err, ErrNoWork) || errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged)) {
		return false, nil
	}
	if err == nil && c.op == extensionExtractionClaim && value.(ExtractionWork).ID == "" {
		return ExtractionWork{}, ErrNoWork
	}
	return value, err
}
func extensionValidExtractionVersion(v ExtractionVersion) bool {
	return validString(v.ID, 256) && (v.Kind == JobOCR || v.Kind == JobCaption || v.Kind == JobTranscription) && validString(v.ProfileID, 128) && validString(v.ProfileIdentity, 512) && extensionMediaDigest(v.RuntimeSHA256) && validString(v.ExtractorVersion, 128) && validString(v.PromptVersion, 128) && validString(v.Language, 64)
}
func (s *Store) ClaimExtraction(ctx context.Context, c ExtractionClaim) (ExtractionWork, error) {
	if !validString(c.Account, 512) || !extensionValidExtractionVersion(c.Version) || !extensionValidTTL(c.TTL) {
		return ExtractionWork{}, ErrInvalidExtension
	}
	x, err := s.submitExtension(ctx, extensionExtractionCommand{op: extensionExtractionClaim, claim: c}, 2048)
	if err != nil {
		return ExtractionWork{}, err
	}
	return x.(ExtractionWork), nil
}
func (s *Store) LoadExtractionInput(ctx context.Context, l JobLease) (ExtractionWork, error) {
	if !extensionValidLease(l) {
		return ExtractionWork{}, ErrInvalidExtension
	}
	x, err := s.submitExtension(ctx, extensionExtractionCommand{op: extensionExtractionLoad, lease: l}, 512)
	if err != nil {
		return ExtractionWork{}, err
	}
	return x.(ExtractionWork), nil
}
func (s *Store) RenewExtraction(ctx context.Context, l JobLease, ttl time.Duration) (time.Time, error) {
	if !extensionValidLease(l) || !extensionValidTTL(ttl) {
		return time.Time{}, ErrInvalidExtension
	}
	x, err := s.submitExtension(ctx, extensionExtractionCommand{op: extensionExtractionRenew, lease: l, ttl: ttl}, 512)
	if err != nil {
		return time.Time{}, err
	}
	return x.(time.Time), nil
}
func extensionExtractionBounds(r ExtractionResult) (int, error) {
	if !extensionValidLease(r.Lease) || !extensionMediaDigest(r.ContentHash) {
		return 0, ErrInvalidExtension
	}
	if r.Outcome != ExtractionText && r.Outcome != ExtractionEmpty && r.Outcome != ExtractionSilent {
		return 0, ErrInvalidExtension
	}
	if len(r.Documents) > extensionMaxDocuments {
		return 0, ErrLimitExceeded
	}
	if (r.Outcome == ExtractionText && len(r.Documents) == 0) || (r.Outcome != ExtractionText && len(r.Documents) != 0) {
		return 0, ErrInvalidExtension
	}
	n := 0
	seen := make(map[DocumentID]bool, len(r.Documents))
	for _, d := range r.Documents {
		if len(d.Text) > extensionMaxDerivedBytes {
			return 0, ErrLimitExceeded
		}
		if !validString(string(d.ID), 256) || seen[d.ID] || !utf8.ValidString(d.Text) || strings.TrimSpace(d.Text) == "" || strings.ContainsRune(d.Text, 0) || len(d.Sources) != 1 || len(d.Spans) != 0 || d.PassageTokens != 0 || !validExtensionMediaSource(d.Sources[0]) || !extensionMediaDigest(d.Sources[0].ContentHash) || !extensionExtractionProvenance(d.Provenance, d.Kind, d.Provenance.MediaID) {
			return 0, ErrInvalidExtension
		}
		seen[d.ID] = true
		n += len(d.Text)
		if n > extensionMaxDerivedBytes {
			return 0, ErrLimitExceeded
		}
	}
	return n + len(r.Documents)*2048, nil
}
func (s *Store) CommitExtraction(ctx context.Context, r ExtractionResult) (bool, error) {
	n, err := extensionExtractionBounds(r)
	if err != nil {
		return false, err
	}
	r.Documents = extensionCloneDocuments(r.Documents)
	x, err := s.submitExtension(ctx, extensionExtractionCommand{op: extensionExtractionCommit, result: r}, int64(n+1024))
	if err != nil {
		return false, err
	}
	return x.(bool), nil
}
func (s *Store) RetryExtraction(ctx context.Context, r JobRetry) error {
	if !extensionValidLease(r.Lease) || !extensionValidRetryCode(r.Code) || r.After < 0 || r.After > 24*time.Hour {
		return ErrInvalidExtension
	}
	_, err := s.submitExtension(ctx, extensionExtractionCommand{op: extensionExtractionRetry, retry: r}, 512)
	return err
}
func extensionExtractionSHA(value []byte) string {
	h := sha256.Sum256(value)
	return hex.EncodeToString(h[:])
}
func extensionExtractionVersionJSON(v ExtractionVersion) (string, string, error) {
	if !extensionValidExtractionVersion(v) {
		return "", "", ErrInvalidExtension
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) > 4096 {
		return "", "", ErrInvalidExtension
	}
	return string(b), extensionExtractionSHA(b), nil
}
func extensionExtractionWorkHash(w ExtractionWork) (string, error) {
	_, version, err := extensionExtractionVersionJSON(w.Version)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(struct {
		Version, Account string
		Message          MessageID
		Revision         uint64
		Policy           Generation
		SourceHash       string
		Expires          int64
		Media            MediaID
		Artifact         ArtifactID
		Original         string
	}{version, w.Source.Account, w.Source.Version.MessageID, w.Source.Version.Revision, w.Source.Version.PolicyGeneration, w.Source.ContentHash, unixms(w.Source.Version.ExpiresAt), w.MediaID, w.ArtifactID, w.OriginalSHA256})
	if err != nil {
		return "", ErrInvalidExtension
	}
	return extensionExtractionSHA(b), nil
}
func extensionExtractionSameSource(a, b SourceRef) bool {
	return a.Account == b.Account && a.ContentHash == b.ContentHash && a.Version.MessageID == b.Version.MessageID && a.Version.Revision == b.Version.Revision && a.Version.PolicyGeneration == b.Version.PolicyGeneration && a.Version.ExpiresAt.Equal(b.Version.ExpiresAt) && a.Version.Deleted == b.Version.Deleted && a.Version.Excluded == b.Version.Excluded
}
func extensionExtractionDocumentKind(k JobKind) DocumentKind {
	switch k {
	case JobOCR:
		return DocumentOCR
	case JobCaption:
		return DocumentCaption
	case JobTranscription:
		return DocumentTranscript
	}
	return ""
}
func extensionExtractionProvenance(p DerivedProvenance, k DocumentKind, media MediaID) bool {
	if (k != DocumentOCR && k != DocumentCaption && k != DocumentTranscript) || p.Kind != k || p.MediaID != media || !validReadID(string(media)) || !validString(p.ProfileID, 128) || !validString(p.ExtractorVersion, 128) || !extensionOptionalString(p.PromptVersion, 128) || p.StartMS < 0 || p.EndMS < p.StartMS || p.EndMS > int64((24*time.Hour)/time.Millisecond) {
		return false
	}
	if p.OffsetKind != "none" && p.OffsetKind != "slice" && p.OffsetKind != "word" {
		return false
	}
	if p.OffsetKind == "none" && (p.StartMS != 0 || p.EndMS != 0) {
		return false
	}
	if k != DocumentTranscript && (p.StartMS != 0 || p.EndMS != 0 || p.OffsetKind != "none") {
		return false
	}
	return true
}

type extensionStoredExtraction struct {
	work        ExtractionWork
	state       string
	versionHash string
	pinned      bool
	outcome     ExtractionOutcome
	completion  string
	lastCode    string
}

func extensionLoadExtraction(ctx context.Context, q sqlQuerier, id JobID) (extensionStoredExtraction, error) {
	var x extensionStoredExtraction
	var encoded string
	var versionID string
	var expiry, deadline int64
	var kind JobKind
	err := q.QueryRowContext(ctx, `SELECT
	 CASE WHEN typeof(j.id)='text' AND length(CAST(j.id AS BLOB))<=128 THEN j.id ELSE '' END,
	 CASE WHEN typeof(j.lease_id)='text' AND length(CAST(j.lease_id AS BLOB))<=256 THEN j.lease_id ELSE '' END,
	 j.lease_until,j.attempts,
	 CASE WHEN typeof(j.account)='text' AND length(CAST(j.account AS BLOB))<=512 THEN j.account ELSE '' END,
	 CASE WHEN typeof(j.message_id)='text' AND length(CAST(j.message_id AS BLOB))<=128 THEN j.message_id ELSE '' END,
	 j.local_revision,j.policy_generation,
	 CASE WHEN typeof(j.content_hash)='text' AND length(CAST(j.content_hash AS BLOB))<=64 THEN j.content_hash ELSE '' END,
	 j.source_expires_at,
	 CASE WHEN typeof(j.media_id)='text' AND length(CAST(j.media_id AS BLOB))<=128 THEN j.media_id ELSE '' END,
	 CASE WHEN typeof(j.artifact_id)='text' AND length(CAST(j.artifact_id AS BLOB))<=128 THEN j.artifact_id ELSE '' END,
	 CASE WHEN typeof(j.original_sha256)='text' AND length(CAST(j.original_sha256 AS BLOB))<=64 THEN j.original_sha256 ELSE '' END,
	 CASE WHEN typeof(j.work_hash)='text' AND length(CAST(j.work_hash AS BLOB))<=64 THEN j.work_hash ELSE '' END,
	 CASE WHEN typeof(j.state)='text' AND length(CAST(j.state AS BLOB))<=9 THEN j.state ELSE '' END,CASE WHEN typeof(j.input_pinned)='integer' AND j.input_pinned IN (0,1) THEN j.input_pinned ELSE NULL END,
	 CASE WHEN typeof(j.outcome)='text' AND length(CAST(j.outcome AS BLOB))<=7 THEN j.outcome ELSE '' END,
	 CASE WHEN typeof(j.completion_digest)='text' AND length(CAST(j.completion_digest AS BLOB))<=64 THEN j.completion_digest ELSE '' END,
	 CASE WHEN typeof(j.last_code)='text' AND length(CAST(j.last_code AS BLOB))<=64 THEN j.last_code ELSE '' END,
	 CASE WHEN typeof(j.kind)='text' AND length(CAST(j.kind AS BLOB))<=13 THEN j.kind ELSE '' END,
	 CASE WHEN typeof(v.version_json)='text' AND length(CAST(v.version_json AS BLOB))<=4096 THEN v.version_json ELSE '' END,
	 CASE WHEN typeof(v.version_hash)='text' AND length(CAST(v.version_hash AS BLOB))<=64 THEN v.version_hash ELSE '' END,
	 CASE WHEN typeof(v.id)='text' AND length(CAST(v.id AS BLOB))<=256 THEN v.id ELSE '' END
	 FROM extension_extraction_jobs j JOIN extension_extraction_versions v ON v.id=j.version_id WHERE j.id=?`, id).Scan(&x.work.ID, &x.work.Lease.LeaseID, &deadline, &x.work.Attempts, &x.work.Source.Account, &x.work.Source.Version.MessageID, &x.work.Source.Version.Revision, &x.work.Source.Version.PolicyGeneration, &x.work.Source.ContentHash, &expiry, &x.work.MediaID, &x.work.ArtifactID, &x.work.OriginalSHA256, &x.work.ContentHash, &x.state, &x.pinned, &x.outcome, &x.completion, &x.lastCode, &kind, &encoded, &x.versionHash, &versionID)
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNoWork
	}
	if err != nil {
		return x, readError(ctx, err)
	}
	if len(encoded) > 4096 || json.Unmarshal([]byte(encoded), &x.work.Version) != nil || !extensionValidExtractionVersion(x.work.Version) || x.work.Version.ID != versionID || x.work.Version.Kind != kind || extensionExtractionSHA([]byte(encoded)) != x.versionHash || !extensionMediaDigest(x.work.OriginalSHA256) {
		return x, ErrInvalidExtension
	}
	x.work.Lease.JobID = x.work.ID
	x.work.LeaseUntil = fromMillis(deadline)
	x.work.Source.Version.ExpiresAt = fromMillis(expiry)
	if !validReadID(string(x.work.ID)) || !extensionValidLease(x.work.Lease) || !validExtensionMediaSource(x.work.Source) || !extensionMediaDigest(x.work.Source.ContentHash) || !validReadID(string(x.work.MediaID)) || !validReadID(string(x.work.ArtifactID)) || !extensionMediaDigest(x.work.ContentHash) || x.work.Attempts < 0 || x.work.Attempts > extensionMaxAttempts || (x.state != "pending" && x.state != "leased" && x.state != "complete" && x.state != "failed" && x.state != "cancelled") {
		return x, ErrInvalidExtension
	}
	hash, err := extensionExtractionWorkHash(x.work)
	if err != nil || hash != x.work.ContentHash {
		return x, ErrInvalidExtension
	}
	return x, nil
}
func (s *Store) extensionClaimExtraction(ctx context.Context, tx *sql.Tx, c ExtractionClaim) (ExtractionWork, error) {
	encoded, digest, err := extensionExtractionVersionJSON(c.Version)
	if err != nil {
		return ExtractionWork{}, err
	}
	var stored string
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN typeof(version_json)='text' AND length(CAST(version_json AS BLOB))<=4096 THEN version_json ELSE '' END FROM extension_extraction_versions WHERE id=?`, c.Version.ID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO extension_extraction_versions(id,version_json,version_hash) VALUES(?,?,?)`, c.Version.ID, encoded, digest); err != nil {
			return ExtractionWork{}, fixedSQLError(err)
		}
	} else if err != nil {
		return ExtractionWork{}, readError(ctx, err)
	} else if stored != encoded {
		return ExtractionWork{}, ErrInvalidExtension
	}
	kind := "image"
	if c.Version.Kind == JobTranscription {
		kind = "voice"
	}
	now := s.now()
	if _, err = tx.ExecContext(ctx, `UPDATE extension_extraction_jobs SET state='failed',updated_at=? WHERE id IN (SELECT id FROM extension_extraction_jobs WHERE account=? AND version_id=? AND state='pending' AND input_pinned=0 AND attempts>=? LIMIT 256)`, unixms(now), c.Account, c.Version.ID, extensionMaxAttempts); err != nil {
		return ExtractionWork{}, fixedSQLError(err)
	}
	join := ` LEFT JOIN extension_extraction_jobs e ON e.version_id=? AND e.media_id=l.id AND e.account=l.account AND e.message_id=l.message_id AND e.local_revision=l.local_revision AND e.policy_generation=l.policy_generation AND e.content_hash=l.content_hash AND e.source_expires_at=l.source_expires_at AND e.original_sha256=a.sha256 WHERE `
	eligible := strings.Replace(extensionMediaCurrentRetainedSQL, " WHERE ", join, 1)
	var w ExtractionWork
	var existing JobID
	var expiry int64
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT
	 CASE WHEN typeof(l.id)='text' AND length(CAST(l.id AS BLOB))<=128 THEN l.id ELSE '' END,
	 CASE WHEN typeof(a.id)='text' AND length(CAST(a.id AS BLOB))<=128 THEN a.id ELSE '' END,
	 CASE WHEN typeof(a.sha256)='text' AND length(CAST(a.sha256 AS BLOB))<=64 THEN a.sha256 ELSE '' END,
	 CASE WHEN typeof(l.account)='text' AND length(CAST(l.account AS BLOB))<=512 THEN l.account ELSE '' END,
	 CASE WHEN typeof(l.message_id)='text' AND length(CAST(l.message_id AS BLOB))<=128 THEN l.message_id ELSE '' END,
	 l.local_revision,l.policy_generation,
	 CASE WHEN typeof(l.content_hash)='text' AND length(CAST(l.content_hash AS BLOB))<=64 THEN l.content_hash ELSE '' END,
	 l.source_expires_at,
	 CASE WHEN e.id IS NULL THEN '' WHEN typeof(e.id)='text' AND length(CAST(e.id AS BLOB))<=128 THEN e.id ELSE NULL END,
	 COALESCE(e.attempts,0)
	 FROM extension_media_associations l `+eligible+` AND l.account=? AND l.kind=? AND (e.id IS NULL OR (e.state='pending' AND e.input_pinned=0 AND e.attempts<? AND e.retry_at<=?) OR (e.state='leased' AND e.input_pinned=0 AND e.attempts<? AND e.lease_until<=?)) ORDER BY COALESCE(e.retry_at,0),l.id LIMIT 1`, c.Version.ID, unixms(now), unixms(now), extensionMediaClockRemainder(now), c.Account, kind, extensionMaxAttempts, unixms(now), extensionMaxAttempts, unixms(now)).Scan(&w.MediaID, &w.ArtifactID, &w.OriginalSHA256, &w.Source.Account, &w.Source.Version.MessageID, &w.Source.Version.Revision, &w.Source.Version.PolicyGeneration, &w.Source.ContentHash, &expiry, &existing, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return ExtractionWork{}, nil
	}
	if err != nil {
		return ExtractionWork{}, readError(ctx, err)
	}
	w.Source.Version.ExpiresAt = fromMillis(expiry)
	w.Version = c.Version
	if !extensionMediaDigest(w.OriginalSHA256) || !validExtensionMediaSource(w.Source) || !extensionMediaDigest(w.Source.ContentHash) || !validReadID(string(w.MediaID)) || !validReadID(string(w.ArtifactID)) || (existing != "" && !validReadID(string(existing))) {
		return ExtractionWork{}, ErrInvalidExtension
	}
	_, artifact, err := s.extensionMediaAssociation(ctx, tx, w.Source, w.MediaID)
	if err != nil {
		return ExtractionWork{}, err
	}
	if artifact != w.ArtifactID {
		return ExtractionWork{}, ErrSourceUnavailable
	}
	var previousSHA string
	if err = tx.QueryRowContext(ctx, `SELECT CASE WHEN typeof(last_original_sha256)='text' AND length(CAST(last_original_sha256 AS BLOB))<=64 THEN last_original_sha256 ELSE NULL END FROM extension_media_associations WHERE id=?`, w.MediaID).Scan(&previousSHA); err != nil {
		return ExtractionWork{}, readError(ctx, err)
	}
	if previousSHA != "" && previousSHA != w.OriginalSHA256 {
		return ExtractionWork{}, ErrSourceUnavailable
	}
	if previousSHA == "" {
		if _, err = tx.ExecContext(ctx, `UPDATE extension_media_associations SET last_original_sha256=? WHERE id=?`, w.OriginalSHA256, w.MediaID); err != nil {
			return ExtractionWork{}, fixedSQLError(err)
		}
	}
	w.ID = existing
	if w.ID == "" {
		id, err := randomID()
		if err != nil {
			return ExtractionWork{}, err
		}
		w.ID = JobID(id)
	}
	lease, err := randomID()
	if err != nil {
		return ExtractionWork{}, err
	}
	w.Lease = JobLease{JobID: w.ID, LeaseID: LeaseID(lease)}
	w.LeaseUntil = fromMillis(unixms(now.Add(c.TTL)))
	w.Attempts = attempts + 1
	if w.Attempts > extensionMaxAttempts {
		w.Attempts = extensionMaxAttempts
	}
	w.ContentHash, err = extensionExtractionWorkHash(w)
	if err != nil {
		return ExtractionWork{}, err
	}
	var result sql.Result
	if existing == "" {
		result, err = tx.ExecContext(ctx, `INSERT INTO extension_extraction_jobs(id,account,version_id,kind,media_id,artifact_id,original_sha256,message_id,local_revision,policy_generation,content_hash,source_expires_at,work_hash,state,attempts,lease_id,lease_until,input_pinned,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'leased',?,?,?,1,?,?)`, w.ID, c.Account, c.Version.ID, c.Version.Kind, w.MediaID, w.ArtifactID, w.OriginalSHA256, w.Source.Version.MessageID, w.Source.Version.Revision, w.Source.Version.PolicyGeneration, w.Source.ContentHash, expiry, w.ContentHash, w.Attempts, w.Lease.LeaseID, unixms(w.LeaseUntil), unixms(now), unixms(now))
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE extension_extraction_jobs SET artifact_id=?,work_hash=?,state='leased',attempts=?,lease_id=?,lease_until=?,input_pinned=1,updated_at=? WHERE id=? AND input_pinned=0 AND state IN ('pending','leased')`, w.ArtifactID, w.ContentHash, w.Attempts, w.Lease.LeaseID, unixms(w.LeaseUntil), unixms(now), w.ID)
	}
	if err = extensionExtractionCAS(result, err); err != nil {
		return ExtractionWork{}, fixedSQLError(err)
	}
	s.extensionRegisterDeadline(tx, w.LeaseUntil)
	return w, nil
}
func extensionExtractionLeaseMatches(x extensionStoredExtraction, l JobLease, now time.Time) bool {
	return x.state == "leased" && x.pinned && x.work.Lease == l && now.Before(x.work.LeaseUntil)
}
func extensionExtractionCAS(result sql.Result, err error) error {
	if err != nil {
		return fixedSQLError(err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fixedSQLError(err)
	}
	if affected == 0 {
		return ErrNoWork
	}
	if affected != 1 {
		return ErrArchiveUnavailable
	}
	return nil
}
func (s *Store) extensionCheckExtraction(ctx context.Context, q sqlQuerier, w ExtractionWork, original bool) error {
	kind, artifact, err := s.extensionMediaAssociation(ctx, q, w.Source, w.MediaID)
	if err != nil {
		return err
	}
	if (w.Version.Kind == JobTranscription && kind != "voice") || ((w.Version.Kind == JobOCR || w.Version.Kind == JobCaption) && kind != "image") {
		return ErrSourceUnavailable
	}
	var currentSHA string
	if err = q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(last_original_sha256)='text' AND length(CAST(last_original_sha256 AS BLOB))<=64 THEN last_original_sha256 ELSE '' END FROM extension_media_associations WHERE id=?`, w.MediaID).Scan(&currentSHA); err != nil {
		return readError(ctx, err)
	}
	if currentSHA != w.OriginalSHA256 {
		return ErrSourceUnavailable
	}
	if !original {
		return nil
	}
	if artifact != w.ArtifactID {
		return ErrSourceUnavailable
	}
	var sha string
	err = q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(sha256)='text' AND length(CAST(sha256 AS BLOB))<=64 THEN sha256 ELSE '' END FROM extension_media_artifacts WHERE id=? AND budget='originals' AND state='retained' AND cleanup_ack=0`, w.ArtifactID).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) || err == nil && sha != w.OriginalSHA256 {
		return ErrSourceUnavailable
	}
	if err != nil {
		return readError(ctx, err)
	}
	return nil
}
func (s *Store) extensionLoadExtractionInput(ctx context.Context, tx *sql.Tx, l JobLease) (ExtractionWork, error) {
	x, err := extensionLoadExtraction(ctx, tx, l.JobID)
	if err != nil {
		return ExtractionWork{}, err
	}
	if !extensionExtractionLeaseMatches(x, l, s.now()) {
		return ExtractionWork{}, ErrNoWork
	}
	if err = s.extensionCheckExtraction(ctx, tx, x.work, true); err != nil {
		return ExtractionWork{}, err
	}
	s.extensionRegisterDeadline(tx, x.work.LeaseUntil)
	return x.work, nil
}
func (s *Store) extensionRenewExtraction(ctx context.Context, tx *sql.Tx, l JobLease, ttl time.Duration) (time.Time, error) {
	x, err := extensionLoadExtraction(ctx, tx, l.JobID)
	if err != nil {
		return time.Time{}, err
	}
	now := s.now()
	if !extensionExtractionLeaseMatches(x, l, now) {
		return time.Time{}, ErrNoWork
	}
	s.extensionRegisterDeadline(tx, x.work.LeaseUntil)
	if err = s.extensionCheckExtraction(ctx, tx, x.work, true); err != nil {
		return time.Time{}, err
	}
	deadline := fromMillis(unixms(now.Add(ttl)))
	if deadline.Before(x.work.LeaseUntil) {
		deadline = x.work.LeaseUntil
	}
	err = extensionExtractionCAS(tx.ExecContext(ctx, `UPDATE extension_extraction_jobs SET lease_until=?,updated_at=? WHERE id=? AND lease_id=? AND state='leased' AND input_pinned=1`, unixms(deadline), unixms(now), l.JobID, l.LeaseID))
	if err != nil {
		return time.Time{}, fixedSQLError(err)
	}
	return deadline, nil
}
func extensionExtractionResultDigest(r ExtractionResult) (string, error) {
	docs := extensionCloneDocuments(r.Documents)
	for i := range docs {
		for j := range docs[i].Sources {
			if !docs[i].Sources[j].Version.ExpiresAt.IsZero() {
				docs[i].Sources[j].Version.ExpiresAt = docs[i].Sources[j].Version.ExpiresAt.UTC()
			}
		}
	}
	b, err := json.Marshal(struct {
		Hash      string
		Outcome   ExtractionOutcome
		Documents []Document
	}{r.ContentHash, r.Outcome, docs})
	if err != nil {
		return "", ErrInvalidExtension
	}
	return extensionExtractionSHA(b), nil
}
func (s *Store) extensionCommitExtraction(ctx context.Context, tx *sql.Tx, r ExtractionResult) (bool, error) {
	x, err := extensionLoadExtraction(ctx, tx, r.Lease.JobID)
	if errors.Is(err, ErrNoWork) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if x.work.Lease != r.Lease || x.work.ContentHash != r.ContentHash {
		return false, nil
	}
	digest, err := extensionExtractionResultDigest(r)
	if err != nil {
		return false, err
	}
	if x.state == "complete" {
		if err = s.extensionCheckExtraction(ctx, tx, x.work, false); err != nil {
			return false, err
		}
		return x.completion == digest, nil
	}
	if !extensionExtractionLeaseMatches(x, r.Lease, s.now()) {
		return false, nil
	}
	if err = s.extensionCheckExtraction(ctx, tx, x.work, true); err != nil {
		return false, err
	}
	s.extensionRegisterDeadline(tx, x.work.LeaseUntil)
	if (r.Outcome == ExtractionEmpty && x.work.Version.Kind == JobTranscription) || (r.Outcome == ExtractionSilent && x.work.Version.Kind != JobTranscription) {
		return false, ErrInvalidExtension
	}
	for _, d := range r.Documents {
		kind := extensionExtractionDocumentKind(x.work.Version.Kind)
		if d.Kind != kind || !extensionExtractionSameSource(d.Sources[0], x.work.Source) || !extensionExtractionProvenance(d.Provenance, kind, x.work.MediaID) || d.Provenance.ProfileID != x.work.Version.ProfileID || d.Provenance.ExtractorVersion != x.work.Version.ExtractorVersion || d.Provenance.PromptVersion != x.work.Version.PromptVersion {
			return false, ErrInvalidExtension
		}
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM extension_roots WHERE id=?) OR EXISTS(SELECT 1 FROM extension_documents WHERE id=?)`, d.ID, d.ID).Scan(&exists); err != nil {
			return false, readError(ctx, err)
		}
		if exists != 0 {
			return false, ErrInvalidExtension
		}
	}
	now := unixms(s.now())
	for ordinal, d := range r.Documents {
		provenance, err := json.Marshal(d.Provenance)
		if err != nil || len(provenance) > 4096 {
			return false, ErrInvalidExtension
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO extension_roots(id,extraction_job_id,kind,text,text_sha256,text_bytes,media_id,provenance_json,source_count,created_at) VALUES(?,?,?,?,?,?,?,?,1,?)`, d.ID, x.work.ID, d.Kind, d.Text, extensionExtractionSHA([]byte(d.Text)), len(d.Text), x.work.MediaID, string(provenance), now); err != nil {
			return false, fixedSQLError(err)
		}
		ref := x.work.Source
		if _, err = tx.ExecContext(ctx, `INSERT INTO extension_root_sources(root_id,account,message_id,local_revision,policy_generation,content_hash,expires_at,ordinal) VALUES(?,?,?,?,?,?,?,0)`, d.ID, ref.Account, ref.Version.MessageID, ref.Version.Revision, ref.Version.PolicyGeneration, ref.ContentHash, unixms(ref.Version.ExpiresAt)); err != nil {
			return false, fixedSQLError(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO extension_extraction_outputs(job_id,root_id,ordinal) VALUES(?,?,?)`, x.work.ID, d.ID, ordinal); err != nil {
			return false, fixedSQLError(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO extension_root_fts(rowid,root_id,text) SELECT rowid,id,? FROM extension_roots WHERE id=?`, NormalizeLexical(d.Text), d.ID); err != nil {
			return false, fixedSQLError(err)
		}
	}
	if err = extensionExtractionCAS(tx.ExecContext(ctx, `UPDATE extension_extraction_jobs SET state='complete',input_pinned=0,outcome=?,completion_digest=?,updated_at=? WHERE id=? AND lease_id=? AND state='leased' AND input_pinned=1`, r.Outcome, digest, now, x.work.ID, r.Lease.LeaseID)); err != nil {
		return false, fixedSQLError(err)
	}
	for _, d := range r.Documents {
		if err = s.extensionEnqueueRootJobs(ctx, tx, d.ID); err != nil {
			return false, err
		}
	}
	if _, err = bumpRevision(ctx, tx); err != nil {
		return false, fixedSQLError(err)
	}
	return true, nil
}
func (s *Store) extensionRetryExtraction(ctx context.Context, tx *sql.Tx, r JobRetry) error {
	x, err := extensionLoadExtraction(ctx, tx, r.Lease.JobID)
	if err != nil {
		return err
	}
	if x.work.Lease != r.Lease {
		return ErrNoWork
	}
	if x.state == "complete" {
		if !x.pinned {
			if x.work.LeaseUntil.IsZero() {
				return nil
			}
			return ErrNoWork
		}
		// Inventory includes all states. An exact stopped owner may settle an
		// anomalous completed pin without rerunning or changing its outcome,
		// receipt, roots, or charged physical artifact. Deadline zero records
		// this settlement for exact-lease idempotency.
		return extensionExtractionCAS(tx.ExecContext(ctx, `UPDATE extension_extraction_jobs SET input_pinned=0,lease_until=0 WHERE id=? AND lease_id=? AND state='complete' AND input_pinned=1`, r.Lease.JobID, r.Lease.LeaseID))
	}
	if !x.pinned {
		if x.lastCode == r.Code {
			return nil
		}
		return ErrNoWork
	}
	// Exact ownership acknowledgement remains possible after expiry, source
	// invalidation, or safety denial. It exposes no original or generated body.
	state := "pending"
	if x.state == "cancelled" {
		state = "cancelled"
	} else if r.Terminal || x.work.Attempts >= extensionMaxAttempts {
		state = "failed"
	}
	attempts := x.work.Attempts
	waiting := !r.Terminal && (r.Code == "model_unavailable" || r.Code == "waiting_for_disk_budget")
	if waiting {
		if state != "cancelled" {
			state = "pending"
		}
		if attempts > 0 {
			attempts--
		}
	}
	after := r.After
	if after < time.Second {
		after = time.Second
	}
	if waiting && after < time.Minute {
		after = time.Minute
	}
	return extensionExtractionCAS(tx.ExecContext(ctx, `UPDATE extension_extraction_jobs SET state=?,attempts=?,retry_at=?,lease_until=0,input_pinned=0,last_code=?,updated_at=? WHERE id=? AND lease_id=? AND input_pinned=1`, state, attempts, unixms(s.now().Add(after)), r.Code, unixms(s.now()), r.Lease.JobID, r.Lease.LeaseID))
}

// This record is body-free and includes the immutable primary-extraction or
// legacy linkage. Readers use it while holding their final safety fence.
type extensionRootRecord struct {
	Origin           DerivedOrigin
	Kind             DocumentKind
	MediaID          MediaID
	ProvenanceJSON   string
	ExtractionJobID  JobID
	LegacyDocumentID DocumentID
	OriginalSHA256   string
	VersionHash      string
}

func (s *Store) extensionAuthorizeRootAt(ctx context.Context, q sqlQuerier, id DocumentID, now time.Time) (extensionRootRecord, error) {
	var root extensionRootRecord
	var job sql.NullString
	var count int
	var provenance string
	err := q.QueryRowContext(ctx, `SELECT
	 CASE WHEN typeof(id)='text' AND length(CAST(id AS BLOB))<=256 THEN id ELSE '' END,
	 CASE WHEN extraction_job_id IS NULL THEN NULL WHEN typeof(extraction_job_id)='text' AND length(CAST(extraction_job_id AS BLOB))<=128 THEN extraction_job_id ELSE '' END,
	 CASE WHEN typeof(legacy_document_id)='text' AND length(CAST(legacy_document_id AS BLOB))<=256 THEN legacy_document_id ELSE '' END,
	 CASE WHEN typeof(kind)='text' AND length(CAST(kind AS BLOB))<=10 THEN kind ELSE '' END,
	 CASE WHEN typeof(text_sha256)='text' AND length(CAST(text_sha256 AS BLOB))<=64 THEN text_sha256 ELSE '' END,text_bytes,
	 CASE WHEN typeof(media_id)='text' AND length(CAST(media_id AS BLOB))<=128 THEN media_id ELSE '' END,
	 CASE WHEN typeof(provenance_json)='text' AND length(CAST(provenance_json AS BLOB))<=4096 THEN provenance_json ELSE '' END,source_count
	 FROM extension_roots WHERE id=?`, id).Scan(&root.Origin.ID, &job, &root.LegacyDocumentID, &root.Kind, &root.Origin.TextSHA256, &root.Origin.Bytes, &root.MediaID, &provenance, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return root, ErrSourceUnavailable
	}
	if err != nil {
		return root, readError(ctx, err)
	}
	if !validString(string(root.Origin.ID), 256) || !extensionMediaDigest(root.Origin.TextSHA256) || root.Origin.Bytes < 1 || root.Origin.Bytes > extensionMaxDerivedBytes || count < 1 || count > extensionMaxSources || len(provenance) > 4096 || json.Unmarshal([]byte(provenance), &root.Origin.Provenance) != nil || !extensionExtractionProvenance(root.Origin.Provenance, root.Kind, root.MediaID) {
		return root, ErrSourceUnavailable
	}
	canonical, err := json.Marshal(root.Origin.Provenance)
	if err != nil {
		return root, ErrSourceUnavailable
	}
	root.ProvenanceJSON = string(canonical)
	rows, err := q.QueryContext(ctx, `SELECT CASE WHEN typeof(account)='text' AND length(CAST(account AS BLOB))<=512 THEN account ELSE '' END,CASE WHEN typeof(message_id)='text' AND length(CAST(message_id AS BLOB))<=128 THEN message_id ELSE '' END,local_revision,policy_generation,CASE WHEN typeof(content_hash)='text' AND length(CAST(content_hash AS BLOB))<=64 THEN content_hash ELSE '' END,expires_at,ordinal FROM extension_root_sources WHERE root_id=? ORDER BY ordinal LIMIT 2049`, id)
	if err != nil {
		return root, readError(ctx, err)
	}
	for rows.Next() {
		var ref SourceRef
		var expiry int64
		var ordinal int
		if err = rows.Scan(&ref.Account, &ref.Version.MessageID, &ref.Version.Revision, &ref.Version.PolicyGeneration, &ref.ContentHash, &expiry, &ordinal); err != nil {
			rows.Close()
			return root, readError(ctx, err)
		}
		ref.Version.ExpiresAt = fromMillis(expiry)
		if ordinal != len(root.Origin.Sources) || !validExtensionMediaSource(ref) {
			rows.Close()
			return root, ErrSourceUnavailable
		}
		root.Origin.Sources = append(root.Origin.Sources, ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return root, readError(ctx, err)
	}
	if len(root.Origin.Sources) != count {
		return root, ErrSourceUnavailable
	}
	if _, err = s.extensionCheckSourcesAt(ctx, q, root.Origin.Sources, true, now); err != nil {
		return root, err
	}
	authorized := false
	var currentSHA string
	for _, ref := range root.Origin.Sources {
		var kind string
		err = q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(kind)='text' AND length(CAST(kind AS BLOB))<=5 THEN kind ELSE '' END,CASE WHEN typeof(last_original_sha256)='text' AND length(CAST(last_original_sha256 AS BLOB))<=64 THEN last_original_sha256 ELSE '' END FROM extension_media_associations WHERE id=? AND account=? AND message_id=? AND local_revision=? AND policy_generation=? AND content_hash=? AND source_expires_at=? AND active=1 AND view_once=0`, root.MediaID, ref.Account, ref.Version.MessageID, ref.Version.Revision, ref.Version.PolicyGeneration, ref.ContentHash, unixms(ref.Version.ExpiresAt)).Scan(&kind, &currentSHA)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return root, readError(ctx, err)
		}
		if (root.Kind == DocumentTranscript && kind == "voice") || ((root.Kind == DocumentOCR || root.Kind == DocumentCaption) && kind == "image") {
			authorized = true
			break
		}
	}
	if !authorized || !extensionMediaDigest(currentSHA) {
		return root, ErrSourceUnavailable
	}
	if job.Valid {
		root.ExtractionJobID = JobID(job.String)
		if !validReadID(string(root.ExtractionJobID)) {
			return root, ErrSourceUnavailable
		}
		x, err := extensionLoadExtraction(ctx, q, root.ExtractionJobID)
		if err != nil {
			return root, err
		}
		if root.LegacyDocumentID != "" || count != 1 || x.state != "complete" || x.outcome != ExtractionText || currentSHA != x.work.OriginalSHA256 || !extensionMediaDigest(x.completion) || x.work.MediaID != root.MediaID || !extensionExtractionSameSource(root.Origin.Sources[0], x.work.Source) || extensionExtractionDocumentKind(x.work.Version.Kind) != root.Kind || root.Origin.Provenance.ProfileID != x.work.Version.ProfileID || root.Origin.Provenance.ExtractorVersion != x.work.Version.ExtractorVersion || root.Origin.Provenance.PromptVersion != x.work.Version.PromptVersion {
			return root, ErrSourceUnavailable
		}
		var inventory int
		if err = q.QueryRowContext(ctx, `SELECT count(*) FROM extension_extraction_outputs WHERE job_id=? AND root_id=?`, root.ExtractionJobID, id).Scan(&inventory); err != nil {
			return root, readError(ctx, err)
		}
		if inventory != 1 {
			return root, ErrSourceUnavailable
		}
		root.OriginalSHA256 = x.work.OriginalSHA256
		root.VersionHash = x.versionHash
	} else if !validString(string(root.LegacyDocumentID), 256) {
		return root, ErrSourceUnavailable
	} else {
		root.OriginalSHA256 = currentSHA
	}
	if err = s.extensionRegisterRoot(q, root); err != nil {
		return root, err
	}
	return root, nil
}
func (s *Store) extensionAuthorizeRoot(ctx context.Context, q sqlQuerier, id DocumentID) (DerivedOrigin, error) {
	root, err := s.extensionAuthorizeRootAt(ctx, q, id, s.now())
	if err != nil {
		return DerivedOrigin{}, err
	}
	s.registerExtensionSources(q, root.Origin.Sources, true)
	return root.Origin, nil
}
func (s *Store) extensionLoadRoot(ctx context.Context, q sqlQuerier, id DocumentID) (Document, error) {
	origin, err := s.extensionAuthorizeRoot(ctx, q, id)
	if err != nil {
		return Document{}, err
	}
	var text string
	err = q.QueryRowContext(ctx, `SELECT text FROM extension_roots WHERE id=? AND length(CAST(text AS BLOB))<=65536`, id).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrSourceUnavailable
	}
	if err != nil {
		return Document{}, readError(ctx, err)
	}
	if len(text) != origin.Bytes || !utf8.ValidString(text) || strings.ContainsRune(text, 0) || extensionExtractionSHA([]byte(text)) != origin.TextSHA256 {
		return Document{}, ErrSourceUnavailable
	}
	return Document{ID: origin.ID, Kind: origin.Provenance.Kind, Text: text, Sources: origin.Sources, Provenance: origin.Provenance}, nil
}

var _ ExtractionStore = (*Store)(nil)
