package archive

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	extensionMaxSources      = 2048
	extensionMaxDocuments    = 256
	extensionMaxInputBytes   = 512 << 10
	extensionMaxPassageBytes = 16 << 10
	extensionMaxDerivedBytes = 64 << 10
	extensionMaxAttempts     = 8
)

type extensionIndexOperation uint8

const (
	extensionEnsureIndex extensionIndexOperation = iota
	extensionClaimDirty
	extensionLoadWindows
	extensionPublishWindows
	extensionClaimJob
	extensionLoadJobInput
	extensionRenewJob
	extensionCommitJob
	extensionRetryJob
	extensionActivateIndex
)

// The command classification is private and fixed independently of its lane.
type extensionIndexCommand struct {
	op          extensionIndexOperation
	version     IndexVersion
	generation  IndexGenerationID
	dirtyClaim  DirtyClaim
	work        DirtyWork
	publication WindowPublication
	jobClaim    JobClaim
	lease       JobLease
	ttl         time.Duration
	result      JobResult
	retry       JobRetry
}

func (c extensionIndexCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c extensionIndexCommand) priority() bool {
	return c.op == extensionRenewJob || c.op == extensionRetryJob
}
func (c extensionIndexCommand) account() string {
	switch c.op {
	case extensionClaimDirty:
		return c.dirtyClaim.Account
	case extensionLoadWindows:
		return c.work.Account
	case extensionPublishWindows:
		return c.publication.Work.Account
	case extensionClaimJob:
		return c.jobClaim.Account
	}
	return ""
}
func (c extensionIndexCommand) run(s *Store, ctx context.Context) (any, error) {
	switch c.op {
	case extensionEnsureIndex:
		return s.extensionEnsure(ctx, c.version)
	case extensionClaimDirty:
		return s.extensionDirtyClaim(ctx, c.dirtyClaim)
	case extensionLoadWindows:
		return s.extensionWindowInput(ctx, c.work)
	case extensionPublishWindows:
		return s.extensionWindowPublish(ctx, c.publication)
	case extensionClaimJob:
		return s.extensionJobClaim(ctx, c.jobClaim)
	case extensionLoadJobInput:
		return s.extensionJobInput(ctx, c.lease)
	case extensionRenewJob:
		return s.extensionJobRenew(ctx, c.lease, c.ttl)
	case extensionCommitJob:
		return s.extensionJobCommit(ctx, c.result)
	case extensionRetryJob:
		return nil, s.extensionJobRetry(ctx, c.retry)
	case extensionActivateIndex:
		return nil, s.extensionActivate(ctx, c.generation)
	}
	return nil, ErrInvalidExtension
}

func (s *Store) EnsureIndex(ctx context.Context, v IndexVersion) (IndexGeneration, error) {
	if !extensionValidVersion(v) {
		return IndexGeneration{}, ErrInvalidExtension
	}
	x, err := s.submitExtension(ctx, extensionIndexCommand{op: extensionEnsureIndex, version: v}, 1024)
	if err != nil {
		return IndexGeneration{}, err
	}
	return x.(IndexGeneration), nil
}
func (s *Store) ClaimDirty(ctx context.Context, c DirtyClaim) (DirtyWork, error) {
	if c.Limit == 0 {
		c.Limit = extensionMaxDocuments
	}
	if !validString(c.Account, 512) || !validString(string(c.Generation), 256) || !extensionValidTTL(c.TTL) || c.Limit < 1 || c.Limit > extensionMaxDocuments {
		return DirtyWork{}, ErrInvalidExtension
	}
	x, err := s.submitExtension(ctx, extensionIndexCommand{op: extensionClaimDirty, dirtyClaim: c}, 1024)
	if err != nil {
		return DirtyWork{}, err
	}
	return x.(DirtyWork), nil
}
func (s *Store) LoadWindowMessages(ctx context.Context, w DirtyWork) ([]WindowMessage, error) {
	if !extensionValidWork(w) {
		return nil, ErrInvalidExtension
	}
	x, err := s.submitExtension(ctx, extensionIndexCommand{op: extensionLoadWindows, work: w}, 1024)
	if err != nil {
		return nil, err
	}
	return x.([]WindowMessage), nil
}
func (s *Store) PublishWindows(ctx context.Context, p WindowPublication) (bool, error) {
	if !extensionValidWork(p.Work) || !extensionValidVersion(p.Version) {
		return false, ErrInvalidExtension
	}
	n, err := extensionDocumentBounds(p.Documents, extensionMaxInputBytes)
	if err != nil {
		return false, err
	}
	p.Documents = extensionCloneDocuments(p.Documents)
	x, err := s.submitExtension(ctx, extensionIndexCommand{op: extensionPublishWindows, publication: p}, int64(n+2048))
	if err != nil {
		return false, err
	}
	return x.(bool), nil
}
func (s *Store) ClaimJob(ctx context.Context, c JobClaim) (Job, error) {
	if !validString(c.Account, 512) || !extensionValidJobKind(c.Kind) || !extensionValidTTL(c.TTL) {
		return Job{}, ErrInvalidExtension
	}
	x, err := s.submitExtension(ctx, extensionIndexCommand{op: extensionClaimJob, jobClaim: c}, 1024)
	if err != nil {
		return Job{}, err
	}
	return x.(Job), nil
}
func (s *Store) LoadJobInput(ctx context.Context, l JobLease) (JobInput, error) {
	if !extensionValidLease(l) {
		return JobInput{}, ErrInvalidExtension
	}
	x, err := s.submitExtension(ctx, extensionIndexCommand{op: extensionLoadJobInput, lease: l}, 1024)
	if err != nil {
		return JobInput{}, err
	}
	return x.(JobInput), nil
}
func (s *Store) RenewJob(ctx context.Context, l JobLease, ttl time.Duration) (time.Time, error) {
	if !extensionValidLease(l) || !extensionValidTTL(ttl) {
		return time.Time{}, ErrInvalidExtension
	}
	x, err := s.submitExtension(ctx, extensionIndexCommand{op: extensionRenewJob, lease: l, ttl: ttl}, 1024)
	if err != nil {
		return time.Time{}, err
	}
	return x.(time.Time), nil
}
func (s *Store) CommitJob(ctx context.Context, r JobResult) (bool, error) {
	if !extensionValidLease(r.Lease) || !extensionValidVersion(r.Version) || !validString(string(r.Generation), 256) || len(r.ContentHash) != 64 || len(r.Sources) > extensionMaxSources || len(r.Embeddings) > extensionMaxDocuments {
		return false, ErrInvalidExtension
	}
	for _, ref := range r.Sources {
		if !extensionValidSourceMetadata(ref) {
			return false, ErrInvalidExtension
		}
	}
	n, err := extensionDocumentBounds(r.Documents, extensionMaxDerivedBytes)
	if err != nil {
		return false, err
	}
	for _, v := range r.Embeddings {
		if !validString(string(v.DocumentID), 256) {
			return false, ErrInvalidExtension
		}
		if len(v.Values) > 4096 {
			return false, ErrLimitExceeded
		}
		n += 4*len(v.Values) + len(v.DocumentID)
	}
	r.Sources = append([]SourceRef(nil), r.Sources...)
	r.Documents = extensionCloneDocuments(r.Documents)
	r.Embeddings = append([]Embedding(nil), r.Embeddings...)
	for i := range r.Embeddings {
		r.Embeddings[i].Values = append([]float32(nil), r.Embeddings[i].Values...)
	}
	x, err := s.submitExtension(ctx, extensionIndexCommand{op: extensionCommitJob, result: r}, int64(n+2048+len(r.Sources)*1024))
	if err != nil {
		return false, err
	}
	return x.(bool), nil
}
func (s *Store) RetryJob(ctx context.Context, r JobRetry) error {
	if !extensionValidLease(r.Lease) || !extensionValidRetryCode(r.Code) || r.After < 0 || r.After > 24*time.Hour {
		return ErrInvalidExtension
	}
	_, err := s.submitExtension(ctx, extensionIndexCommand{op: extensionRetryJob, retry: r}, 1024)
	return err
}
func (s *Store) ActivateIndex(ctx context.Context, g IndexGenerationID) error {
	if !validString(string(g), 256) {
		return ErrInvalidExtension
	}
	_, err := s.submitExtension(ctx, extensionIndexCommand{op: extensionActivateIndex, generation: g}, 1024)
	return err
}

func extensionValidVersion(v IndexVersion) bool {
	if !validString(v.ID, 256) || !validString(v.EncoderProfile, 256) || !validString(v.TokenizerImplementation, 256) || !validString(v.PrefixVersion, 256) || !validString(v.ChunkingVersion, 256) || v.NormalizationVersion != LexicalNormalizationVersion || v.Dimensions < 1 || v.Dimensions > 4096 || v.MaxPassageTokens < 1 || v.MaxPassageTokens > 256 {
		return false
	}
	if len(v.TokenizerSHA256) != 64 {
		return false
	}
	_, err := hex.DecodeString(v.TokenizerSHA256)
	return err == nil
}
func extensionValidTTL(ttl time.Duration) bool { return ttl >= time.Second && ttl <= 15*time.Minute }
func extensionValidLease(l JobLease) bool {
	return validString(string(l.JobID), 256) && validString(string(l.LeaseID), 256)
}
func extensionValidWork(w DirtyWork) bool {
	return validString(w.ID, 256) && validString(string(w.LeaseID), 256) && validString(w.Account, 512) && validString(string(w.ChatID), 256) && validString(string(w.Generation), 256) && w.PolicyGeneration > 0 && w.DirtyRevision > 0 && !w.LeaseUntil.IsZero() && !w.From.IsZero() && w.Until.After(w.From)
}
func extensionValidJobKind(k JobKind) bool {
	switch k {
	case JobTextEmbedding, JobOCR, JobCaption, JobTranscription, JobDerivedEmbedding:
		return true
	}
	return false
}
func extensionValidRetryCode(c string) bool {
	switch c {
	case "model_unavailable", "tokenizer_unavailable", "worker_failed", "worker_exited", "timeout", "cancelled", "invalid_input", "invalid_result", "unsupported", "source_unavailable", "extraction_failed", "embedding_failed", "retryable", "waiting_for_disk_budget":
		return true
	}
	return false
}
func extensionDocumentBounds(docs []Document, maxBytes int) (int, error) {
	if len(docs) > extensionMaxDocuments {
		return 0, ErrLimitExceeded
	}
	n, sources, spans := 0, 0, 0
	for _, d := range docs {
		if !validString(string(d.ID), 256) || len(d.Text) > extensionMaxPassageBytes || !utf8.ValidString(d.Text) || len(d.Sources) == 0 || len(d.Sources) > extensionMaxSources || len(d.Spans) > extensionMaxSources {
			return 0, ErrInvalidExtension
		}
		switch d.Kind {
		case DocumentWindow, DocumentOCR, DocumentCaption, DocumentTranscript:
		default:
			return 0, ErrInvalidExtension
		}
		p := d.Provenance
		if !extensionOptionalString(string(p.MediaID), 128) || !extensionOptionalString(p.ProfileID, 128) || !extensionOptionalString(p.ExtractorVersion, 128) || !extensionOptionalString(p.PromptVersion, 128) {
			return 0, ErrInvalidExtension
		}
		switch p.Kind {
		case "", DocumentWindow, DocumentOCR, DocumentCaption, DocumentTranscript:
		default:
			return 0, ErrInvalidExtension
		}
		switch p.OffsetKind {
		case "", "none", "slice", "word":
		default:
			return 0, ErrInvalidExtension
		}
		for _, ref := range d.Sources {
			if !extensionValidSourceMetadata(ref) {
				return 0, ErrInvalidExtension
			}
		}
		for _, sp := range d.Spans {
			if !extensionValidSourceMetadata(sp.Source) || sp.Field != "body" && sp.Field != "caption" {
				return 0, ErrInvalidExtension
			}
		}
		n += len(d.Text)
		sources += len(d.Sources)
		spans += len(d.Spans)
		if n > maxBytes || sources+spans > extensionMaxSources {
			return 0, ErrLimitExceeded
		}
	}
	return n + (sources+spans+len(docs))*1024, nil
}

func extensionOptionalString(s string, limit int) bool { return s == "" || validString(s, limit) }
func extensionValidSourceMetadata(r SourceRef) bool {
	return validString(r.Account, 512) && validString(string(r.Version.MessageID), 128) && len(r.ContentHash) == 64 && validReadTime(r.Version.ExpiresAt)
}
func extensionCloneDocuments(docs []Document) []Document {
	out := append([]Document(nil), docs...)
	for i := range out {
		out[i].Sources = append([]SourceRef(nil), out[i].Sources...)
		out[i].Spans = append([]SourceSpan(nil), out[i].Spans...)
	}
	return out
}
func extensionSameSource(a, b SourceRef) bool {
	return a.Account == b.Account && a.ContentHash == b.ContentHash && a.Version.MessageID == b.Version.MessageID && a.Version.Revision == b.Version.Revision && a.Version.PolicyGeneration == b.Version.PolicyGeneration && a.Version.Deleted == b.Version.Deleted && a.Version.Excluded == b.Version.Excluded && unixms(a.Version.ExpiresAt) == unixms(b.Version.ExpiresAt)
}
func extensionSameSources(a, b []SourceRef) bool {
	if len(a) != len(b) || len(a) > extensionMaxSources {
		return false
	}
	copyA, copyB := append([]SourceRef(nil), a...), append([]SourceRef(nil), b...)
	less := func(x, y SourceRef) bool {
		if x.Account != y.Account {
			return x.Account < y.Account
		}
		return x.Version.MessageID < y.Version.MessageID
	}
	sort.Slice(copyA, func(i, j int) bool { return less(copyA[i], copyA[j]) })
	sort.Slice(copyB, func(i, j int) bool { return less(copyB[i], copyB[j]) })
	for i := range copyA {
		if !extensionSameSource(copyA[i], copyB[i]) || i > 0 && !less(copyA[i-1], copyA[i]) {
			return false
		}
	}
	return true
}
func extensionSuperseded(err error) bool {
	return errors.Is(err, ErrNoWork) || errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged)
}

func extensionLoadGeneration(ctx context.Context, q sqlQuerier, id IndexGenerationID) (IndexGeneration, error) {
	g := IndexGeneration{ID: id}
	var encoded string
	err := q.QueryRowContext(ctx, "SELECT version_json,state FROM extension_generations WHERE id=?", id).Scan(&encoded, &g.State)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNoWork
	}
	if err != nil {
		return g, fixedSQLError(err)
	}
	if json.Unmarshal([]byte(encoded), &g.Version) != nil || !extensionValidVersion(g.Version) {
		return g, ErrSchemaChanged
	}
	return g, nil
}
func extensionLoadSources(ctx context.Context, q sqlQuerier, table, key string) ([]SourceRef, error) {
	var query string
	switch table {
	case "dirty":
		query = "SELECT account,message_id,local_revision,policy_generation,expires_at,content_hash FROM extension_dirty_sources WHERE dirty_id=? ORDER BY ordinal LIMIT 2049"
	case "document":
		query = "SELECT account,message_id,local_revision,policy_generation,expires_at,content_hash FROM extension_document_sources WHERE document_id=? ORDER BY ordinal LIMIT 2049"
	case "job":
		query = "SELECT account,message_id,local_revision,policy_generation,expires_at,content_hash FROM extension_job_sources WHERE job_id=? ORDER BY ordinal LIMIT 2049"
	default:
		return nil, ErrInvalidExtension
	}
	rows, err := q.QueryContext(ctx, query, key)
	if err != nil {
		return nil, fixedSQLError(err)
	}
	defer rows.Close()
	var refs []SourceRef
	for rows.Next() {
		var r SourceRef
		var expires int64
		if err = rows.Scan(&r.Account, &r.Version.MessageID, &r.Version.Revision, &r.Version.PolicyGeneration, &expires, &r.ContentHash); err != nil {
			return nil, fixedSQLError(err)
		}
		r.Version.ExpiresAt = fromMillis(expires)
		refs = append(refs, r)
		if len(refs) > extensionMaxSources {
			return nil, ErrLimitExceeded
		}
	}
	return refs, fixedSQLError(rows.Err())
}
func extensionLoadDocument(ctx context.Context, q sqlQuerier, id DocumentID) (Document, error) {
	d := Document{ID: id}
	var provenance, spans string
	var size, count int
	err := q.QueryRowContext(ctx, "SELECT length(CAST(text AS BLOB)),source_count FROM extension_documents WHERE id=?", id).Scan(&size, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrSourceUnavailable
	}
	if err != nil {
		return d, fixedSQLError(err)
	}
	if size > extensionMaxPassageBytes || count < 1 || count > extensionMaxSources {
		return d, ErrLimitExceeded
	}
	err = q.QueryRowContext(ctx, "SELECT kind,text,provenance_json,spans_json,passage_tokens FROM extension_documents WHERE id=?", id).Scan(&d.Kind, &d.Text, &provenance, &spans, &d.PassageTokens)
	if err != nil {
		return d, fixedSQLError(err)
	}
	if len(provenance) > 4096 || len(spans) > extensionMaxSources*2048 || json.Unmarshal([]byte(provenance), &d.Provenance) != nil || json.Unmarshal([]byte(spans), &d.Spans) != nil {
		return d, ErrSchemaChanged
	}
	d.Sources, err = extensionLoadSources(ctx, q, "document", string(id))
	if err == nil && len(d.Sources) != count {
		err = ErrSourceUnavailable
	}
	return d, err
}

// This predicate uses only current authoritative source rows and policy.
func extensionSourcePredicate() string {
	return "m.state='active' AND m.original_date_known=1 AND (m.expires_at=0 OR m.expires_at>?) AND (p.date_mode='all_available' OR m.original_sent_at>=p.import_start) AND (p.rolling_retention=0 OR m.original_sent_at>=?) AND (p.chat_mode='all' OR EXISTS(SELECT 1 FROM policy_chats pc WHERE pc.chat_id=m.chat_id))"
}
func (s *Store) extensionEnsure(ctx context.Context, v IndexVersion) (IndexGeneration, error) {
	var result IndexGeneration
	err := s.extensionTransaction(ctx, "", func(tx *sql.Tx) error {
		rows, e := tx.QueryContext(ctx, "SELECT id,version_json,state FROM extension_generations ORDER BY created_at,id")
		if e != nil {
			return e
		}
		for rows.Next() {
			var g IndexGeneration
			var encoded string
			if e = rows.Scan(&g.ID, &encoded, &g.State); e != nil {
				rows.Close()
				return e
			}
			if json.Unmarshal([]byte(encoded), &g.Version) != nil {
				rows.Close()
				return ErrSchemaChanged
			}
			if g.Version.ID == v.ID {
				rows.Close()
				if g.Version != v {
					return ErrInvalidExtension
				}
				result = g
				return nil
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		id, e := randomID()
		if e != nil {
			return e
		}
		encoded, e := json.Marshal(v)
		if e != nil {
			return ErrInvalidExtension
		}
		result = IndexGeneration{ID: IndexGenerationID(id), Version: v, State: "building"}
		if _, e = tx.ExecContext(ctx, "INSERT INTO extension_generations(id,version_json,state,created_at) VALUES(?,?,'building',?)", result.ID, string(encoded), unixms(s.now())); e != nil {
			return e
		}
		p, e := s.loadPolicy(ctx, tx)
		if e != nil {
			return e
		}
		now := s.now()
		var rev ArchiveRevision
		if e = tx.QueryRowContext(ctx, "SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&rev); e != nil {
			return e
		}
		rows, e = tx.QueryContext(ctx, "SELECT m.account,m.chat_id,MIN(m.original_sent_at),MAX(m.original_sent_at)+1 FROM messages m JOIN archive_policy p ON p.id=1 WHERE "+extensionSourcePredicate()+" AND (length(m.body)>0 OR length(m.caption)>0) GROUP BY m.account,m.chat_id", unixms(now), ceilMillis(now.Add(-p.RollingRetention)))
		if e != nil {
			return e
		}
		type seed struct {
			account, chat string
			from, until   int64
		}
		var seeds []seed
		for rows.Next() {
			var x seed
			if e = rows.Scan(&x.account, &x.chat, &x.from, &x.until); e != nil {
				rows.Close()
				return e
			}
			seeds = append(seeds, x)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, x := range seeds {
			id, e = randomID()
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO extension_dirty(id,account,chat_id,generation,policy_generation,from_time,until_time,dirty_revision) VALUES(?,?,?,?,?,?,?,?)", id, x.account, x.chat, result.ID, p.Generation, x.from, x.until, rev); e != nil {
				return e
			}
		}
		return s.extensionSeedDerivedJobs(ctx, tx, result.ID, "")
	})
	return result, err
}

type extensionDirtyState struct {
	work                        DirtyWork
	liveRevision                ArchiveRevision
	liveFrom, liveUntil         int64
	cursorTime, claimCursorTime int64
	cursorID, claimCursorID     string
}

func extensionLoadDirty(ctx context.Context, q sqlQuerier, id string) (extensionDirtyState, error) {
	var d extensionDirtyState
	d.work.ID = id
	var lease, from, until int64
	err := q.QueryRowContext(ctx, "SELECT account,chat_id,generation,policy_generation,lease_id,lease_until,claim_revision,claim_from,claim_until,dirty_revision,from_time,until_time,cursor_time,cursor_id,claim_cursor_time,claim_cursor_id FROM extension_dirty WHERE id=?", id).Scan(&d.work.Account, &d.work.ChatID, &d.work.Generation, &d.work.PolicyGeneration, &d.work.LeaseID, &lease, &d.work.DirtyRevision, &from, &until, &d.liveRevision, &d.liveFrom, &d.liveUntil, &d.cursorTime, &d.cursorID, &d.claimCursorTime, &d.claimCursorID)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNoWork
	}
	if err != nil {
		return d, fixedSQLError(err)
	}
	d.work.LeaseUntil = fromMillis(lease)
	d.work.From = fromMillis(from)
	d.work.Until = fromMillis(until)
	return d, nil
}
func extensionSameWork(a, b DirtyWork) bool {
	return a.ID == b.ID && a.LeaseID == b.LeaseID && a.Account == b.Account && a.ChatID == b.ChatID && a.PolicyGeneration == b.PolicyGeneration && a.Generation == b.Generation && unixms(a.From) == unixms(b.From) && unixms(a.Until) == unixms(b.Until) && a.DirtyRevision == b.DirtyRevision
}

func (s *Store) extensionDirtyClaim(ctx context.Context, c DirtyClaim) (DirtyWork, error) {
	var result DirtyWork
	err := s.extensionTransaction(ctx, c.Account, func(tx *sql.Tx) error {
		g, e := extensionLoadGeneration(ctx, tx, c.Generation)
		if e != nil {
			return e
		}
		if g.State == "retired" {
			return ErrNoWork
		}
		if e = s.extensionRegisterGeneration(tx, g); e != nil {
			return e
		}
		p, e := s.loadPolicy(ctx, tx)
		if e != nil {
			return e
		}
		now := s.now()
		for tries := 0; tries < 64; tries++ {
			var id string
			var from, until, cursor int64
			var cursorID string
			var rev ArchiveRevision
			e = tx.QueryRowContext(ctx, "SELECT id,from_time,until_time,dirty_revision,cursor_time,cursor_id FROM extension_dirty WHERE account=? AND generation=? AND (lease_id='' OR lease_until<=?) ORDER BY chat_id,id LIMIT 1", c.Account, c.Generation, unixms(now)).Scan(&id, &from, &until, &rev, &cursor, &cursorID)
			if errors.Is(e, sql.ErrNoRows) {
				return nil
			}
			if e != nil {
				return e
			}
			d, e := extensionLoadDirty(ctx, tx, id)
			if e != nil {
				return e
			}
			publication, publicationErr := extensionLoadPublication(ctx, tx, "window", id)
			if publicationErr == nil && !publication.complete {
				identity, e := extensionWindowIdentity(d.work)
				if e != nil {
					return e
				}
				if publication.identity == identity && d.work.PolicyGeneration == p.Generation {
					refs, e := extensionPublicationSources(ctx, tx, "window", id)
					if e != nil {
						return e
					}
					if _, e = s.extensionCheckSources(ctx, tx, refs, false); e == nil {
						lease, e := randomID()
						if e != nil {
							return e
						}
						cursor, e := randomID()
						if e != nil {
							return e
						}
						deadline := fromMillis(unixms(s.now().Add(c.TTL)))
						if _, e = tx.ExecContext(ctx, `UPDATE extension_dirty SET lease_id=?,lease_until=? WHERE id=?`, lease, unixms(deadline), id); e != nil {
							return e
						}
						if _, e = tx.ExecContext(ctx, `UPDATE extension_publications SET lease_id=?,lease_until=?,cursor=?,last_cursor='',last_digest='',last_next='',last_complete=0 WHERE kind='window' AND work_id=?`, lease, unixms(deadline), cursor, id); e != nil {
							return e
						}
						result = d.work
						result.LeaseID = LeaseID(lease)
						result.LeaseUntil = deadline
						s.extensionRegisterDeadline(tx, deadline)
						return nil
					} else if !extensionSuperseded(e) {
						return e
					}
				}
			} else if publicationErr != nil && !errors.Is(publicationErr, ErrNoWork) {
				return publicationErr
			}
			if _, e = tx.ExecContext(ctx, "DELETE FROM extension_publications WHERE kind='window' AND work_id=?", id); e != nil {
				return e
			}
			rows, e := tx.QueryContext(ctx, "SELECT m.id,m.local_revision,m.expires_at,m.content_hash,m.original_sent_at,length(CAST(m.body AS BLOB))+length(CAST(m.caption AS BLOB)) FROM messages m JOIN archive_policy p ON p.id=1 WHERE m.account=? AND m.chat_id=? AND m.original_sent_at>=? AND m.original_sent_at<? AND (?='' OR m.original_sent_at>? OR (m.original_sent_at=? AND m.id>?)) AND "+extensionSourcePredicate()+" AND (length(m.body)>0 OR length(m.caption)>0) ORDER BY m.original_sent_at,m.id LIMIT ?", c.Account, d.work.ChatID, from, until, cursorID, cursor, cursor, cursorID, unixms(now), ceilMillis(now.Add(-p.RollingRetention)), c.Limit+1)
			if e != nil {
				return e
			}
			var refs []SourceRef
			var lastTime int64
			var lastID MessageID
			bytes := 0
			for rows.Next() {
				var r SourceRef
				var expiry, sent int64
				var size int
				r.Account = c.Account
				r.Version.PolicyGeneration = p.Generation
				if e = rows.Scan(&r.Version.MessageID, &r.Version.Revision, &expiry, &r.ContentHash, &sent, &size); e != nil {
					rows.Close()
					return e
				}
				if len(refs) == c.Limit || bytes+size > extensionMaxInputBytes {
					break
				}
				r.Version.ExpiresAt = fromMillis(expiry)
				refs = append(refs, r)
				bytes += size
				lastTime = sent
				lastID = r.Version.MessageID
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
			if len(refs) == 0 {
				if _, e = tx.ExecContext(ctx, "DELETE FROM extension_dirty WHERE id=?", id); e != nil {
					return e
				}
				continue
			}
			if _, e = s.extensionCheckSources(ctx, tx, refs, false); e != nil {
				return e
			}
			lease, e := randomID()
			if e != nil {
				return e
			}
			deadline := fromMillis(unixms(now.Add(c.TTL)))
			if _, e = tx.ExecContext(ctx, "DELETE FROM extension_dirty_sources WHERE dirty_id=?", id); e != nil {
				return e
			}
			for i, r := range refs {
				if _, e = tx.ExecContext(ctx, "INSERT INTO extension_dirty_sources(dirty_id,account,message_id,local_revision,policy_generation,expires_at,content_hash,ordinal) VALUES(?,?,?,?,?,?,?,?)", id, r.Account, r.Version.MessageID, r.Version.Revision, r.Version.PolicyGeneration, unixms(r.Version.ExpiresAt), r.ContentHash, i); e != nil {
					return e
				}
			}
			if _, e = tx.ExecContext(ctx, "UPDATE extension_dirty SET policy_generation=?,lease_id=?,lease_until=?,claim_revision=?,claim_from=?,claim_until=?,claim_cursor_time=?,claim_cursor_id=? WHERE id=?", p.Generation, lease, unixms(deadline), rev, from, lastTime+1, lastTime, lastID, id); e != nil {
				return e
			}
			result = DirtyWork{ID: id, LeaseID: LeaseID(lease), LeaseUntil: deadline, Account: c.Account, ChatID: d.work.ChatID, PolicyGeneration: p.Generation, Generation: c.Generation, From: fromMillis(from), Until: fromMillis(lastTime + 1), DirtyRevision: rev}
			s.extensionRegisterDeadline(tx, deadline)
			return nil
		}
		return nil
	})
	if err == nil && result.ID == "" {
		return DirtyWork{}, ErrNoWork
	}
	return result, err
}
func (s *Store) extensionWindowMessages(ctx context.Context, q sqlQuerier, w DirtyWork) ([]WindowMessage, error) {
	d, e := extensionLoadDirty(ctx, q, w.ID)
	if e != nil {
		return nil, e
	}
	if !extensionSameWork(d.work, w) || !d.work.LeaseUntil.After(s.now()) {
		return nil, ErrNoWork
	}
	s.extensionRegisterDeadline(q, d.work.LeaseUntil)
	g, e := extensionLoadGeneration(ctx, q, w.Generation)
	if e != nil {
		return nil, e
	}
	if g.State == "retired" {
		return nil, ErrNoWork
	}
	if e = s.extensionRegisterGeneration(q, g); e != nil {
		return nil, e
	}
	refs, e := extensionLoadSources(ctx, q, "dirty", w.ID)
	if e != nil {
		return nil, e
	}
	if len(refs) == 0 || len(refs) > extensionMaxDocuments {
		return nil, ErrSourceUnavailable
	}
	read, e := s.extensionCheckSources(ctx, q, refs, false)
	if e != nil {
		return nil, e
	}
	var messages []WindowMessage
	bytes := 0
	for i, r := range refs {
		if read[i].ChatID != w.ChatID {
			return nil, ErrSourceUnavailable
		}
		var m WindowMessage
		m.Source = r
		m.ChatID = read[i].ChatID
		m.SentAt = read[i].OriginalSentAt
		var size int
		if e = q.QueryRowContext(ctx, "SELECT length(CAST(body AS BLOB))+length(CAST(caption AS BLOB)) FROM messages WHERE id=?", r.Version.MessageID).Scan(&size); e != nil {
			return nil, fixedSQLError(e)
		}
		bytes += size
		if bytes > extensionMaxInputBytes {
			return nil, ErrLimitExceeded
		}
		if e = q.QueryRowContext(ctx, "SELECT body,caption FROM messages WHERE id=?", r.Version.MessageID).Scan(&m.Body, &m.Caption); e != nil {
			return nil, fixedSQLError(e)
		}
		messages = append(messages, m)
	}
	return messages, nil
}
func (s *Store) extensionWindowInput(ctx context.Context, w DirtyWork) ([]WindowMessage, error) {
	var result []WindowMessage
	err := s.extensionTransaction(ctx, w.Account, func(tx *sql.Tx) error { var e error; result, e = s.extensionWindowMessages(ctx, tx, w); return e })
	return result, err
}

// Acknowledging a page certifies the complete original fields, not just an
// anchor or a fabricated excerpt. Spans retain the original UTF-8 offsets.
func extensionValidateWindows(docs []Document, input []WindowMessage, v IndexVersion) error {
	if len(docs) == 0 && len(input) > 0 {
		return ErrInvalidExtension
	}
	type sourceFields struct {
		ref           SourceRef
		body, caption string
	}
	fields := make(map[MessageID]sourceFields, len(input))
	ordinals := make(map[MessageID]int, len(input))
	for i, m := range input {
		fields[m.Source.Version.MessageID] = sourceFields{m.Source, m.Body, m.Caption}
		ordinals[m.Source.Version.MessageID] = i
	}
	type interval struct{ start, end int }
	type fieldKey struct {
		id    MessageID
		field string
	}
	coverage := make(map[fieldKey][]interval)
	ids := make(map[DocumentID]bool)
	for _, d := range docs {
		if ids[d.ID] || d.Kind != DocumentWindow || d.PassageTokens < 1 || d.PassageTokens > v.MaxPassageTokens || len(d.Spans) == 0 || d.Provenance.Kind != DocumentWindow || d.Provenance.MediaID != "" || d.Provenance.ProfileID != "" || d.Provenance.ExtractorVersion != "" || d.Provenance.PromptVersion != "" || d.Provenance.StartMS != 0 || d.Provenance.EndMS != 0 || d.Provenance.OffsetKind != "none" {
			return ErrInvalidExtension
		}
		ids[d.ID] = true
		members := make(map[MessageID]SourceRef)
		for _, r := range d.Sources {
			f, ok := fields[r.Version.MessageID]
			if !ok || !extensionSameSource(f.ref, r) || members[r.Version.MessageID].Version.MessageID != "" {
				return ErrInvalidExtension
			}
			members[r.Version.MessageID] = r
		}
		used := make(map[MessageID]bool)
		slices := make([]string, 0, len(d.Spans))
		lastSource, lastField := -1, -1
		for _, sp := range d.Spans {
			r, ok := members[sp.Source.Version.MessageID]
			if !ok || !extensionSameSource(r, sp.Source) {
				return ErrInvalidExtension
			}
			f := fields[r.Version.MessageID]
			var original string
			fieldOrder := 0
			switch sp.Field {
			case "body":
				original = f.body
			case "caption":
				original = f.caption
				fieldOrder = 1
			default:
				return ErrInvalidExtension
			}
			ordinal := ordinals[r.Version.MessageID]
			// A field fragment is contiguous within one document. Separate
			// fragment documents may partition it; adding separators inside the
			// same original field would change its logical text.
			if ordinal < lastSource || ordinal == lastSource && fieldOrder <= lastField {
				return ErrInvalidExtension
			}
			lastSource, lastField = ordinal, fieldOrder
			if sp.StartByte < 0 || sp.EndByte <= sp.StartByte || sp.EndByte > len(original) || !utf8.ValidString(original[:sp.StartByte]) || !utf8.ValidString(original[sp.StartByte:sp.EndByte]) {
				return ErrInvalidExtension
			}
			slices = append(slices, original[sp.StartByte:sp.EndByte])
			used[r.Version.MessageID] = true
			k := fieldKey{r.Version.MessageID, sp.Field}
			coverage[k] = append(coverage[k], interval{sp.StartByte, sp.EndByte})
		}
		if len(used) != len(members) || strings.Join(slices, "\n") != d.Text {
			return ErrInvalidExtension
		}
	}
	for id, f := range fields {
		for _, field := range []string{"body", "caption"} {
			original := f.body
			if field == "caption" {
				original = f.caption
			}
			if original == "" {
				continue
			}
			spans := coverage[fieldKey{id, field}]
			sort.Slice(spans, func(i, j int) bool {
				if spans[i].start != spans[j].start {
					return spans[i].start < spans[j].start
				}
				return spans[i].end < spans[j].end
			})
			end := 0
			for _, sp := range spans {
				if sp.start != end {
					return ErrInvalidExtension
				}
				end = sp.end
			}
			if end != len(original) {
				return ErrInvalidExtension
			}
		}
	}
	return nil
}
func extensionDeleteDocument(ctx context.Context, tx *sql.Tx, id DocumentID) error {
	if _, e := tx.ExecContext(ctx, `DELETE FROM extension_window_coverage WHERE (generation,account,message_id) IN(SELECT d.generation,ds.account,ds.message_id FROM extension_documents d JOIN extension_document_sources ds ON ds.document_id=d.id WHERE d.id=? AND d.kind='text_window')`, id); e != nil {
		return e
	}
	if _, e := tx.ExecContext(ctx, "DELETE FROM extension_document_fts WHERE rowid IN(SELECT rowid FROM extension_documents WHERE id=?)", id); e != nil {
		return e
	}
	if _, e := tx.ExecContext(ctx, "UPDATE extension_jobs SET state='cancelled',lease_id='',lease_until=0,last_code='source_unavailable' WHERE state IN('pending','leased') AND id IN(SELECT job_id FROM extension_job_documents WHERE document_id=?)", id); e != nil {
		return e
	}
	_, e := tx.ExecContext(ctx, "DELETE FROM extension_documents WHERE id=?", id)
	return e
}
func extensionSaveDocument(ctx context.Context, tx *sql.Tx, g IndexGeneration, d Document) error {
	if _, e := extensionDocumentBounds([]Document{d}, extensionMaxInputBytes); e != nil {
		return e
	}
	if d.Provenance.Kind == "" {
		d.Provenance.Kind = d.Kind
	}
	if d.Provenance.OffsetKind == "" {
		d.Provenance.OffsetKind = "none"
	}
	provenance, e := json.Marshal(d.Provenance)
	if e != nil {
		return ErrInvalidExtension
	}
	spans, e := json.Marshal(d.Spans)
	if e != nil {
		return ErrInvalidExtension
	}
	digestKind := JobTextEmbedding
	origin := DocumentID("")
	originEnd, hidden := 0, false
	if d.Kind != DocumentWindow {
		origin, originEnd = d.ID, len(d.Text)
		hidden = d.PassageTokens == 0
		if hidden {
			digestKind = JobDerivedEmbedding
		}
	}
	hash, e := CanonicalWorkDigest(g.ID, g.Version, digestKind, d.Provenance.MediaID, d.Sources, []Document{d})
	if e != nil {
		return e
	}
	var oldGeneration IndexGenerationID
	var oldHash string
	e = tx.QueryRowContext(ctx, "SELECT generation,content_hash FROM extension_documents WHERE id=?", d.ID).Scan(&oldGeneration, &oldHash)
	if e == nil {
		if oldGeneration == g.ID && oldHash == hash {
			return nil
		}
		return ErrInvalidExtension
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO extension_documents(id,generation,kind,text,normalized_text,provenance_json,spans_json,passage_tokens,content_hash,source_count,origin_document_id,origin_start_byte,origin_end_byte,embedding_input) VALUES(?,?,?,?,?,?,?,?,?,?,?,0,?,?)", d.ID, g.ID, d.Kind, d.Text, NormalizeLexical(d.Text), string(provenance), string(spans), d.PassageTokens, hash, len(d.Sources), origin, originEnd, hidden); e != nil {
		return e
	}
	for i, r := range d.Sources {
		if _, e = tx.ExecContext(ctx, "INSERT INTO extension_document_sources(document_id,account,message_id,local_revision,policy_generation,expires_at,content_hash,ordinal) VALUES(?,?,?,?,?,?,?,?)", d.ID, r.Account, r.Version.MessageID, r.Version.Revision, r.Version.PolicyGeneration, unixms(r.Version.ExpiresAt), r.ContentHash, i); e != nil {
			return e
		}
	}
	if hidden {
		return nil
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO extension_document_fts(rowid,normalized_text) SELECT rowid,normalized_text FROM extension_documents WHERE id=?", d.ID)
	return e
}

func (s *Store) extensionWindowBoundaryExpansion(ctx context.Context, tx *sql.Tx, work DirtyWork, dirty extensionDirtyState, input []WindowMessage, obsolete []DocumentID, now time.Time) (int64, int64, bool, error) {
	from, until := dirty.liveFrom, dirty.liveUntil
	claimed := make(map[MessageID]bool, len(input))
	for _, message := range input {
		claimed[message.Source.Version.MessageID] = true
	}
	p, err := s.loadPolicy(ctx, tx)
	if err != nil {
		return from, until, false, err
	}
	seen := make(map[MessageID]bool)
	examined := 0
	expand := false
	for _, id := range obsolete {
		rows, err := tx.QueryContext(ctx, "SELECT m.id,m.chat_id,m.original_sent_at FROM extension_document_sources ds JOIN messages m ON m.id=ds.message_id JOIN archive_policy p ON p.id=1 WHERE ds.document_id=? AND m.account=? AND "+extensionSourcePredicate()+" LIMIT 2049", id, work.Account, unixms(now), ceilMillis(now.Add(-p.RollingRetention)))
		if err != nil {
			return from, until, false, err
		}
		for rows.Next() {
			examined++
			if examined > extensionMaxSources {
				rows.Close()
				return from, until, false, ErrLimitExceeded
			}
			var message MessageID
			var chat ChatID
			var sent int64
			if err = rows.Scan(&message, &chat, &sent); err != nil {
				rows.Close()
				return from, until, false, err
			}
			if seen[message] {
				continue
			}
			seen[message] = true
			if len(seen) > extensionMaxSources {
				rows.Close()
				return from, until, false, ErrLimitExceeded
			}
			if claimed[message] {
				continue
			}
			if chat != work.ChatID {
				rows.Close()
				return from, until, false, ErrSourceUnavailable
			}
			future := sent >= dirty.liveFrom && sent < dirty.liveUntil && (sent > dirty.claimCursorTime || sent == dirty.claimCursorTime && string(message) > dirty.claimCursorID)
			if future {
				continue
			}
			expand = true
			if sent < from {
				from = sent
			}
			if sent+1 > until {
				until = sent + 1
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return from, until, false, err
		}
	}
	return from, until, expand, nil
}
func (s *Store) extensionWindowPublish(ctx context.Context, p WindowPublication) (bool, error) {
	published := false
	err := s.extensionTransaction(ctx, p.Work.Account, func(tx *sql.Tx) error {
		input, e := s.extensionWindowMessages(ctx, tx, p.Work)
		if extensionSuperseded(e) {
			return nil
		}
		if e != nil {
			return e
		}
		g, e := extensionLoadGeneration(ctx, tx, p.Work.Generation)
		if extensionSuperseded(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if g.Version != p.Version || g.State == "retired" {
			return nil
		}
		for i := range p.Documents {
			if p.Documents[i].Provenance.Kind == "" {
				p.Documents[i].Provenance.Kind = DocumentWindow
			}
			if p.Documents[i].Provenance.OffsetKind == "" {
				p.Documents[i].Provenance.OffsetKind = "none"
			}
		}
		if e = extensionValidateWindows(p.Documents, input, g.Version); e != nil {
			return e
		}
		newHashes := make(map[DocumentID]string, len(p.Documents))
		for _, d := range p.Documents {
			hash, e := CanonicalWorkDigest(g.ID, g.Version, JobTextEmbedding, d.Provenance.MediaID, d.Sources, []Document{d})
			if e != nil {
				return e
			}
			newHashes[d.ID] = hash
		}
		rows, e := tx.QueryContext(ctx, "SELECT DISTINCT d.id,d.content_hash FROM extension_documents d JOIN extension_document_sources ds ON ds.document_id=d.id JOIN extension_dirty_sources cs ON cs.message_id=ds.message_id WHERE d.generation=? AND d.kind='text_window' AND cs.dirty_id=? LIMIT 2049", g.ID, p.Work.ID)
		if e != nil {
			return e
		}
		var obsolete []DocumentID
		affected := 0
		for rows.Next() {
			var id DocumentID
			var hash string
			if e = rows.Scan(&id, &hash); e != nil {
				rows.Close()
				return e
			}
			affected++
			if affected > extensionMaxSources {
				rows.Close()
				return ErrLimitExceeded
			}
			if newHashes[id] != hash {
				obsolete = append(obsolete, id)
				if len(obsolete) > extensionMaxSources {
					rows.Close()
					return ErrLimitExceeded
				}
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		dirty, e := extensionLoadDirty(ctx, tx, p.Work.ID)
		if e != nil {
			return e
		}
		publicationNow := s.now()
		expandedFrom, expandedUntil, expand, e := s.extensionWindowBoundaryExpansion(ctx, tx, p.Work, dirty, input, obsolete, publicationNow)
		if e != nil {
			return e
		}
		for _, id := range obsolete {
			if e = extensionDeleteDocument(ctx, tx, id); e != nil {
				return e
			}
		}
		for _, d := range p.Documents {
			if e = extensionSaveDocument(ctx, tx, g, d); e != nil {
				return e
			}
			if e = extensionEnqueueJob(ctx, tx, Job{Kind: JobTextEmbedding, Generation: g.ID, Version: g.Version, Sources: d.Sources, Documents: []DocumentID{d.ID}}); e != nil {
				return e
			}
		}
		if e = extensionSaveWindowCoverage(ctx, tx, g.ID, input); e != nil {
			return e
		}
		publicationRevision, e := bumpRevision(ctx, tx)
		if e != nil {
			return e
		}
		if expand {
			if _, e = tx.ExecContext(ctx, "UPDATE extension_dirty SET from_time=?,until_time=?,dirty_revision=?,cursor_time=0,cursor_id='' WHERE id=? AND lease_id=?", expandedFrom, expandedUntil, publicationRevision, p.Work.ID, p.Work.LeaseID); e != nil {
				return e
			}
		}
		d, e := extensionLoadDirty(ctx, tx, p.Work.ID)
		if e != nil {
			return e
		}
		if d.liveRevision == p.Work.DirtyRevision {
			var remaining int
			policy, e := s.loadPolicy(ctx, tx)
			if e != nil {
				return e
			}
			now := publicationNow
			e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM messages m JOIN archive_policy p ON p.id=1 WHERE m.account=? AND m.chat_id=? AND m.original_sent_at>=(SELECT from_time FROM extension_dirty WHERE id=?) AND m.original_sent_at<(SELECT until_time FROM extension_dirty WHERE id=?) AND (m.original_sent_at>? OR(m.original_sent_at=? AND m.id>?)) AND "+extensionSourcePredicate()+" AND(length(m.body)>0 OR length(m.caption)>0))", p.Work.Account, p.Work.ChatID, p.Work.ID, p.Work.ID, d.claimCursorTime, d.claimCursorTime, d.claimCursorID, unixms(now), ceilMillis(now.Add(-policy.RollingRetention))).Scan(&remaining)
			if e != nil {
				return e
			}
			if remaining == 0 {
				if _, e = tx.ExecContext(ctx, "DELETE FROM extension_dirty WHERE id=? AND dirty_revision=? AND lease_id=?", p.Work.ID, p.Work.DirtyRevision, p.Work.LeaseID); e != nil {
					return e
				}
			} else {
				if _, e = tx.ExecContext(ctx, "UPDATE extension_dirty SET cursor_time=claim_cursor_time,cursor_id=claim_cursor_id,lease_id='',lease_until=0 WHERE id=? AND dirty_revision=? AND lease_id=?", p.Work.ID, p.Work.DirtyRevision, p.Work.LeaseID); e != nil {
					return e
				}
			}
		} else {
			if _, e = tx.ExecContext(ctx, "UPDATE extension_dirty SET lease_id='',lease_until=0,cursor_time=0,cursor_id='' WHERE id=? AND lease_id=?", p.Work.ID, p.Work.LeaseID); e != nil {
				return e
			}
		}
		if _, e = tx.ExecContext(ctx, "DELETE FROM extension_dirty_sources WHERE dirty_id=?", p.Work.ID); e != nil {
			return e
		}
		published = true
		return nil
	})
	if extensionSuperseded(err) {
		return false, nil
	}
	return published && err == nil, err
}

type extensionStoredJob struct {
	job            Job
	account, state string
	retryAt        int64
}

func extensionLoadJob(ctx context.Context, q sqlQuerier, id JobID) (extensionStoredJob, error) {
	var x extensionStoredJob
	x.job.ID = id
	x.job.Lease.JobID = id
	var until int64
	e := q.QueryRowContext(ctx, "SELECT account,kind,generation,media_id,content_hash,state,attempts,retry_at,lease_id,lease_until FROM extension_jobs WHERE id=?", id).Scan(&x.account, &x.job.Kind, &x.job.Generation, &x.job.MediaID, &x.job.ContentHash, &x.state, &x.job.Attempts, &x.retryAt, &x.job.Lease.LeaseID, &until)
	if errors.Is(e, sql.ErrNoRows) {
		return x, ErrNoWork
	}
	if e != nil {
		return x, fixedSQLError(e)
	}
	x.job.LeaseUntil = fromMillis(until)
	g, e := extensionLoadGeneration(ctx, q, x.job.Generation)
	if e != nil {
		return x, e
	}
	x.job.Version = g.Version
	x.job.Sources, e = extensionLoadSources(ctx, q, "job", string(id))
	if e != nil {
		return x, e
	}
	rows, e := q.QueryContext(ctx, "SELECT document_id FROM extension_job_documents WHERE job_id=? ORDER BY ordinal LIMIT 257", id)
	if e != nil {
		return x, fixedSQLError(e)
	}
	defer rows.Close()
	for rows.Next() {
		var d DocumentID
		if e = rows.Scan(&d); e != nil {
			return x, fixedSQLError(e)
		}
		x.job.Documents = append(x.job.Documents, d)
		if len(x.job.Documents) > extensionMaxDocuments {
			return x, ErrLimitExceeded
		}
	}
	if e = rows.Err(); e != nil {
		return x, fixedSQLError(e)
	}
	rows.Close()
	var root DocumentID
	e = q.QueryRowContext(ctx, "SELECT root_id FROM extension_job_origins WHERE job_id=?", id).Scan(&root)
	if e == nil {
		if len(x.job.Documents) != 0 {
			return x, ErrSchemaChanged
		}
		x.job.Documents = []DocumentID{root}
	} else if !errors.Is(e, sql.ErrNoRows) {
		return x, fixedSQLError(e)
	}
	return x, nil
}

// Enqueue stores references only. Its content identity is derived by archive,
// and identical publication retries do not replace existing lease authority.
func extensionEnqueueJob(ctx context.Context, tx *sql.Tx, j Job) error {
	if !extensionValidJobKind(j.Kind) || len(j.Sources) == 0 || len(j.Sources) > extensionMaxSources || len(j.Documents) > extensionMaxDocuments || !extensionSameSources(j.Sources, j.Sources) {
		return ErrInvalidExtension
	}
	g, e := extensionLoadGeneration(ctx, tx, j.Generation)
	if e != nil {
		return e
	}
	if g.State == "retired" {
		return nil
	}
	if j.Version.ID != "" && j.Version != g.Version {
		return ErrInvalidExtension
	}
	j.Version = g.Version
	account := j.Sources[0].Account
	for _, r := range j.Sources {
		if r.Account != account {
			return ErrInvalidExtension
		}
	}
	var docs []Document
	for _, id := range j.Documents {
		d, e := extensionLoadDocument(ctx, tx, id)
		if e != nil {
			return e
		}
		docs = append(docs, d)
	}
	hash, e := CanonicalWorkDigest(j.Generation, j.Version, j.Kind, j.MediaID, j.Sources, docs)
	if e != nil {
		return e
	}
	j.ID = JobID(hash)
	r, e := tx.ExecContext(ctx, "INSERT INTO extension_jobs(id,account,kind,generation,media_id,content_hash,state) VALUES(?,?,?,?,?,?,'pending') ON CONFLICT(id) DO NOTHING", j.ID, account, j.Kind, j.Generation, j.MediaID, hash)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n == 0 {
		existing, e := extensionLoadJob(ctx, tx, j.ID)
		if e != nil {
			return e
		}
		if existing.job.Kind != j.Kind || existing.job.Generation != j.Generation || existing.job.MediaID != j.MediaID || existing.job.ContentHash != hash || !extensionSameSources(existing.job.Sources, j.Sources) {
			return ErrSchemaChanged
		}
		repair := existing.state == "cancelled"
		if len(j.Documents) > 0 {
			if len(existing.job.Documents) != len(j.Documents) {
				repair = true
			} else {
				for i, id := range j.Documents {
					if existing.job.Documents[i] != id {
						repair = true
						break
					}
				}
			}
			if existing.state == "complete" {
				hiddenComplete, e := extensionDerivedHiddenComplete(ctx, tx, existing.job)
				if e != nil {
					return e
				}
				if !hiddenComplete {
					var vectors int
					if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM extension_job_documents jd JOIN extension_vectors v ON v.document_id=jd.document_id WHERE jd.job_id=? AND v.generation=? AND v.dimensions=?", j.ID, j.Generation, j.Version.Dimensions).Scan(&vectors); e != nil {
						return e
					}
					if vectors != len(j.Documents) {
						repair = true
					}
				}
			}
		}
		if !repair {
			return nil
		}
		// A deterministic document may recur after neighboring boundaries
		// changed. Restore its reference-only work under fresh lease authority.
		if _, e = tx.ExecContext(ctx, "DELETE FROM extension_job_documents WHERE job_id=?", j.ID); e != nil {
			return e
		}
		for i, id := range j.Documents {
			if _, e = tx.ExecContext(ctx, "INSERT INTO extension_job_documents(job_id,document_id,ordinal) VALUES(?,?,?)", j.ID, id, i); e != nil {
				return e
			}
		}
		_, e = tx.ExecContext(ctx, "UPDATE extension_jobs SET state='pending',attempts=0,retry_at=0,lease_id='',lease_until=0,last_code='' WHERE id=?", j.ID)
		return e
	}
	for i, ref := range j.Sources {
		if _, e = tx.ExecContext(ctx, "INSERT INTO extension_job_sources(job_id,account,message_id,local_revision,policy_generation,expires_at,content_hash,ordinal) VALUES(?,?,?,?,?,?,?,?)", j.ID, ref.Account, ref.Version.MessageID, ref.Version.Revision, ref.Version.PolicyGeneration, unixms(ref.Version.ExpiresAt), ref.ContentHash, i); e != nil {
			return e
		}
	}
	for i, id := range j.Documents {
		if _, e = tx.ExecContext(ctx, "INSERT INTO extension_job_documents(job_id,document_id,ordinal) VALUES(?,?,?)", j.ID, id, i); e != nil {
			return e
		}
	}
	return nil
}
func extensionEnqueueMediaJobsForGeneration(ctx context.Context, tx *sql.Tx, ref SourceRef, media MediaID, kind string, g IndexGenerationID) error {
	var kinds []JobKind
	switch kind {
	case "image":
		kinds = []JobKind{JobOCR, JobCaption}
	case "voice":
		kinds = []JobKind{JobTranscription}
	default:
		return ErrInvalidExtension
	}
	for _, k := range kinds {
		if e := extensionEnqueueJob(ctx, tx, Job{Kind: k, Generation: g, Sources: []SourceRef{ref}, MediaID: media}); e != nil {
			return e
		}
	}
	return nil
}
func extensionEnqueueMediaJobs(ctx context.Context, tx *sql.Tx, ref SourceRef, media MediaID, kind string) error {
	var kinds string
	var required int
	switch kind {
	case "image":
		kinds, required = "('ocr','caption')", 2
	case "voice":
		kinds, required = "('transcription')", 1
	default:
		return ErrInvalidExtension
	}
	// Publication starts one generation's work. Later extraction claims seed
	// the remaining generations, skipping those with complete job inventory.
	// The fixed kind fragments are private SQL, never caller-supplied text.
	var id IndexGenerationID
	err := tx.QueryRowContext(ctx, `SELECT g.id FROM extension_generations g WHERE g.state IN('building','active') AND
	 (SELECT count(DISTINCT j.kind) FROM extension_jobs j JOIN extension_job_sources js ON js.job_id=j.id
	 WHERE j.generation=g.id AND j.media_id=? AND j.kind IN `+kinds+` AND j.state<>'cancelled'
	 AND js.account=? AND js.message_id=? AND js.local_revision=? AND js.policy_generation=? AND js.expires_at=? AND js.content_hash=?
	 AND (SELECT count(*) FROM extension_job_sources extra WHERE extra.job_id=j.id)=1)<?
	 ORDER BY g.created_at,g.id LIMIT 1`, media, ref.Account, ref.Version.MessageID, ref.Version.Revision, ref.Version.PolicyGeneration, unixms(ref.Version.ExpiresAt), ref.ContentHash, required).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return extensionEnqueueMediaJobsForGeneration(ctx, tx, ref, media, kind, id)
}

func (s *Store) extensionEnqueueMediaJobs(ctx context.Context, tx *sql.Tx, ref SourceRef, media MediaID, kind string) error {
	return extensionEnqueueMediaJobs(ctx, tx, ref, media, kind)
}

func (s *Store) extensionEnqueueMediaJobsForGeneration(ctx context.Context, tx *sql.Tx, ref SourceRef, media MediaID, kind string, generation IndexGenerationID) error {
	return extensionEnqueueMediaJobsForGeneration(ctx, tx, ref, media, kind, generation)
}
func (s *Store) extensionCheckJob(ctx context.Context, q sqlQuerier, j Job) error {
	g, e := extensionLoadGeneration(ctx, q, j.Generation)
	if e != nil {
		return e
	}
	if g.State == "retired" || g.Version != j.Version {
		return ErrNoWork
	}
	if e = s.extensionRegisterGeneration(q, g); e != nil {
		return e
	}
	if len(j.Sources) == 0 || !extensionSameSources(j.Sources, j.Sources) {
		return ErrSourceUnavailable
	}
	if _, e = s.extensionCheckSources(ctx, q, j.Sources, j.Kind != JobTextEmbedding); e != nil {
		return e
	}
	if j.Kind == JobDerivedEmbedding {
		var root DocumentID
		err := q.QueryRowContext(ctx, "SELECT root_id FROM extension_job_origins WHERE job_id=?", j.ID).Scan(&root)
		if err == nil {
			origin, err := s.extensionAuthorizeRoot(ctx, q, root)
			if err != nil {
				return err
			}
			hash, err := CanonicalOriginWorkDigest(j.Generation, j.Version, origin)
			if err != nil {
				return err
			}
			if hash != j.ContentHash || !extensionSameSources(origin.Sources, j.Sources) {
				return ErrSourceUnavailable
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fixedSQLError(err)
		}
	}
	if j.Kind == JobOCR || j.Kind == JobCaption || j.Kind == JobTranscription {
		ok, e := s.extensionMediaJobEligible(ctx, q, j)
		if e != nil {
			return e
		}
		if !ok {
			return ErrSourceUnavailable
		}
	}
	return nil
}
func extensionMatchingLease(x extensionStoredJob, l JobLease, now time.Time) bool {
	return x.state == "leased" && x.job.Lease == l && x.job.LeaseUntil.After(now)
}
func (s *Store) extensionJobClaim(ctx context.Context, c JobClaim) (Job, error) {
	var result Job
	err := s.extensionTransaction(ctx, c.Account, func(tx *sql.Tx) error {
		for pass := 0; ; pass++ {
			now := s.now()
			if _, e := tx.ExecContext(ctx, "UPDATE extension_jobs SET state='failed',lease_id='',lease_until=0,last_code='worker_failed' WHERE account=? AND kind=? AND attempts>=? AND(state='pending' OR(state='leased' AND lease_until<=?))", c.Account, c.Kind, extensionMaxAttempts, unixms(now)); e != nil {
				return e
			}
			rows, e := tx.QueryContext(ctx, "SELECT id FROM extension_jobs WHERE account=? AND kind=? AND attempts<? AND retry_at<=? AND(state='pending' OR(state='leased' AND lease_until<=?)) AND (?<>'derived_embedding' OR EXISTS(SELECT 1 FROM extension_job_origins jo WHERE jo.job_id=extension_jobs.id)) ORDER BY retry_at,id LIMIT 64", c.Account, c.Kind, extensionMaxAttempts, unixms(now), unixms(now), c.Kind)
			if e != nil {
				return e
			}
			var ids []JobID
			for rows.Next() {
				var id JobID
				if e = rows.Scan(&id); e != nil {
					rows.Close()
					return e
				}
				ids = append(ids, id)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
			for _, id := range ids {
				x, e := extensionLoadJob(ctx, tx, id)
				if e != nil {
					return e
				}
				e = s.extensionCheckJob(ctx, tx, x.job)
				if extensionSuperseded(e) {
					if _, e = tx.ExecContext(ctx, "UPDATE extension_jobs SET state='cancelled',lease_id='',lease_until=0,last_code='source_unavailable' WHERE id=?", id); e != nil {
						return e
					}
					continue
				}
				if e != nil {
					return e
				}
				lease, e := randomID()
				if e != nil {
					return e
				}
				until := fromMillis(unixms(now.Add(c.TTL)))
				if _, e = tx.ExecContext(ctx, "UPDATE extension_jobs SET state='leased',attempts=attempts+1,lease_id=?,lease_until=?,last_code='' WHERE id=?", lease, unixms(until), id); e != nil {
					return e
				}
				result = x.job
				result.Lease = JobLease{JobID: id, LeaseID: LeaseID(lease)}
				result.LeaseUntil = until
				result.Attempts++
				s.extensionRegisterDeadline(tx, until)
				return nil
			}
			// Existing derived inventory is claimed first. Missing-origin seeding
			// only runs after the bounded eligible queue has been exhausted.
			if c.Kind == JobDerivedEmbedding && pass == 0 {
				if e := s.extensionSeedNextDerivedJobs(ctx, tx, c.Account); e != nil {
					return e
				}
				continue
			}
			return nil
		}
	})
	if err == nil && result.ID == "" {
		return Job{}, ErrNoWork
	}
	return result, err
}
func (s *Store) extensionLoadAuthorizedJobInput(ctx context.Context, q sqlQuerier, l JobLease) (JobInput, error) {
	x, e := extensionLoadJob(ctx, q, l.JobID)
	if e != nil {
		return JobInput{}, e
	}
	if !extensionMatchingLease(x, l, s.now()) {
		return JobInput{}, ErrNoWork
	}
	var origin int
	if e = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM extension_job_origins WHERE job_id=?)", l.JobID).Scan(&origin); e != nil {
		return JobInput{}, fixedSQLError(e)
	}
	if origin != 0 {
		return JobInput{}, ErrInvalidExtension
	}
	s.extensionRegisterDeadline(q, x.job.LeaseUntil)
	if e = s.extensionCheckJob(ctx, q, x.job); e != nil {
		return JobInput{}, e
	}
	input := JobInput{Job: x.job}
	bytes := 0
	for _, id := range x.job.Documents {
		d, e := extensionLoadDocument(ctx, q, id)
		if e != nil {
			return JobInput{}, e
		}
		bytes += len(d.Text)
		if bytes > extensionMaxInputBytes {
			return JobInput{}, ErrLimitExceeded
		}
		if !extensionSameSources(d.Sources, x.job.Sources) {
			return JobInput{}, ErrSourceUnavailable
		}
		var generation IndexGenerationID
		if e = q.QueryRowContext(ctx, "SELECT generation FROM extension_documents WHERE id=?", id).Scan(&generation); e != nil {
			return JobInput{}, fixedSQLError(e)
		}
		if generation != x.job.Generation {
			return JobInput{}, ErrSourceUnavailable
		}
		input.Documents = append(input.Documents, d)
	}
	hash, e := CanonicalWorkDigest(x.job.Generation, x.job.Version, x.job.Kind, x.job.MediaID, x.job.Sources, input.Documents)
	if e != nil {
		return JobInput{}, e
	}
	if hash != x.job.ContentHash {
		return JobInput{}, ErrSourceUnavailable
	}
	return input, nil
}
func (s *Store) extensionJobInput(ctx context.Context, l JobLease) (JobInput, error) {
	var result JobInput
	err := s.extensionTransaction(ctx, "", func(tx *sql.Tx) error {
		var e error
		result, e = s.extensionLoadAuthorizedJobInput(ctx, tx, l)
		return e
	})
	return result, err
}
func (s *Store) extensionJobRenew(ctx context.Context, l JobLease, ttl time.Duration) (time.Time, error) {
	var result time.Time
	err := s.extensionTransaction(ctx, "", func(tx *sql.Tx) error {
		x, e := extensionLoadJob(ctx, tx, l.JobID)
		if e != nil {
			return e
		}
		now := s.now()
		if !extensionMatchingLease(x, l, now) {
			return ErrNoWork
		}
		s.extensionRegisterDeadline(tx, x.job.LeaseUntil)
		if e = s.extensionCheckJob(ctx, tx, x.job); e != nil {
			return e
		}
		result = fromMillis(unixms(now.Add(ttl)))
		s.extensionRegisterDeadline(tx, result)
		_, e = tx.ExecContext(ctx, "UPDATE extension_jobs SET lease_until=? WHERE id=? AND lease_id=? AND state='leased'", unixms(result), l.JobID, l.LeaseID)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE extension_publications SET lease_until=? WHERE kind='derived' AND work_id=? AND lease_id=?", unixms(result), l.JobID, l.LeaseID)
		return e
	})
	return result, err
}
func (s *Store) extensionJobRetry(ctx context.Context, r JobRetry) error {
	return s.extensionTransaction(ctx, "", func(tx *sql.Tx) error {
		x, e := extensionLoadJob(ctx, tx, r.Lease.JobID)
		if e != nil {
			return e
		}
		now := s.now()
		if !extensionMatchingLease(x, r.Lease, now) {
			return ErrNoWork
		}
		s.extensionRegisterDeadline(tx, x.job.LeaseUntil)
		if e = s.extensionCheckJob(ctx, tx, x.job); e != nil {
			return e
		}
		state := "pending"
		waiting := !r.Terminal && (r.Code == "model_unavailable" || r.Code == "tokenizer_unavailable" || r.Code == "waiting_for_disk_budget")
		if r.Terminal || !waiting && x.job.Attempts >= extensionMaxAttempts {
			state = "failed"
		}
		after := r.After
		if after < time.Second {
			after = time.Second
		}
		if waiting && after < time.Minute {
			after = time.Minute
		}
		// Stable bounded jitter prevents synchronized restart retries.
		jitter := time.Duration(0)
		if len(r.Lease.JobID) > 0 {
			jitter = time.Duration(r.Lease.JobID[len(r.Lease.JobID)-1]%11) * after / 100
		}
		attempts := x.job.Attempts
		if waiting && attempts > 0 {
			attempts--
		}
		_, e = tx.ExecContext(ctx, "UPDATE extension_jobs SET state=?,attempts=?,retry_at=?,lease_id='',lease_until=0,last_code=? WHERE id=? AND lease_id=?", state, attempts, unixms(now.Add(after+jitter)), r.Code, r.Lease.JobID, r.Lease.LeaseID)
		return e
	})
}

func extensionVectorValid(values []float32, dimensions int) bool {
	if dimensions < 1 || dimensions > 4096 || len(values) != dimensions {
		return false
	}
	norm := float64(0)
	for _, v := range values {
		x := float64(v)
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
		norm += x * x
	}
	return math.Abs(norm-1) <= 0.002
}
func extensionVectorBlob(values []float32) []byte {
	encoded := make([]byte, len(values)*4)
	for i, v := range values {
		binary.LittleEndian.PutUint32(encoded[i*4:], math.Float32bits(v))
	}
	return encoded
}
func extensionDerivedDocumentValid(d Document, j Job) bool {
	var kind DocumentKind
	switch j.Kind {
	case JobOCR:
		kind = DocumentOCR
	case JobCaption:
		kind = DocumentCaption
	case JobTranscription:
		kind = DocumentTranscript
	default:
		return false
	}
	p := d.Provenance
	if d.Kind != kind || p.Kind != kind || p.MediaID != j.MediaID || !validString(string(p.MediaID), 128) || !validString(p.ProfileID, 128) || !validString(p.ExtractorVersion, 128) || len(p.PromptVersion) > 128 || !utf8.ValidString(p.PromptVersion) || strings.ContainsRune(p.PromptVersion, 0) || p.StartMS < 0 || p.EndMS < p.StartMS || p.EndMS > 24*60*60*1000 || d.Text == "" || len(d.Spans) != 0 || d.PassageTokens < 0 || d.PassageTokens > j.Version.MaxPassageTokens || !extensionSameSources(d.Sources, j.Sources) {
		return false
	}
	if kind == DocumentTranscript {
		return p.OffsetKind == "none" && p.StartMS == 0 && p.EndMS == 0 || p.OffsetKind == "slice" || p.OffsetKind == "word"
	}
	return p.OffsetKind == "none" && p.StartMS == 0 && p.EndMS == 0
}
func (s *Store) extensionJobCommit(ctx context.Context, r JobResult) (bool, error) {
	committed := false
	err := s.extensionTransaction(ctx, "", func(tx *sql.Tx) error {
		input, e := s.extensionLoadAuthorizedJobInput(ctx, tx, r.Lease)
		if extensionSuperseded(e) {
			return nil
		}
		if e != nil {
			return e
		}
		j := input.Job
		if j.Generation != r.Generation || j.Version != r.Version || j.ContentHash != r.ContentHash || !extensionSameSources(j.Sources, r.Sources) {
			return nil
		}
		g, e := extensionLoadGeneration(ctx, tx, j.Generation)
		if e != nil {
			return e
		}
		hidden := false
		if j.Kind == JobDerivedEmbedding && len(input.Documents) == 1 {
			state, e := extensionLoadDerivedInputState(ctx, tx, input.Documents[0].ID)
			if e != nil {
				return e
			}
			hidden = state.hidden
		}
		if hidden {
			if e = extensionCommitDerivedFragments(ctx, tx, g, j, input.Documents[0], r); e != nil {
				return e
			}
		} else if j.Kind == JobTextEmbedding || j.Kind == JobDerivedEmbedding {
			if len(r.Embeddings) != len(j.Documents) || len(j.Documents) == 0 {
				return ErrInvalidExtension
			}
			if len(r.Documents) > 0 {
				if len(r.Documents) != len(input.Documents) {
					return ErrInvalidExtension
				}
				for i, d := range r.Documents {
					encodedA, e := json.Marshal(d)
					if e != nil {
						return ErrInvalidExtension
					}
					encodedB, e := json.Marshal(input.Documents[i])
					if e != nil {
						return ErrSchemaChanged
					}
					if string(encodedA) != string(encodedB) {
						return ErrInvalidExtension
					}
				}
			}
			members := make(map[DocumentID]bool, len(j.Documents))
			for _, id := range j.Documents {
				members[id] = true
			}
			seen := make(map[DocumentID]bool, len(r.Embeddings))
			for _, vector := range r.Embeddings {
				if !members[vector.DocumentID] || seen[vector.DocumentID] || !extensionVectorValid(vector.Values, j.Version.Dimensions) {
					return ErrInvalidExtension
				}
				seen[vector.DocumentID] = true
				if _, e = tx.ExecContext(ctx, "INSERT INTO extension_vectors(document_id,generation,dimensions,values_blob) VALUES(?,?,?,?) ON CONFLICT(document_id) DO UPDATE SET generation=excluded.generation,dimensions=excluded.dimensions,values_blob=excluded.values_blob", vector.DocumentID, j.Generation, j.Version.Dimensions, extensionVectorBlob(vector.Values)); e != nil {
					return e
				}
			}
		} else {
			if len(r.Embeddings) != 0 {
				return ErrInvalidExtension
			}
			seen := make(map[DocumentID]bool, len(r.Documents))
			for _, d := range r.Documents {
				if !extensionDerivedDocumentValid(d, j) || seen[d.ID] {
					return ErrInvalidExtension
				}
				seen[d.ID] = true
				if e = extensionSaveDocument(ctx, tx, g, d); e != nil {
					return e
				}
				// Derived vectors remain valid when an original cache file is evicted.
				if e = extensionEnqueueJob(ctx, tx, Job{Kind: JobDerivedEmbedding, Generation: g.ID, Version: g.Version, Sources: d.Sources, Documents: []DocumentID{d.ID}}); e != nil {
					return e
				}
			}
		}
		if _, e = tx.ExecContext(ctx, "UPDATE extension_jobs SET state='complete',lease_id='',lease_until=0,last_code='' WHERE id=? AND lease_id=? AND state='leased'", r.Lease.JobID, r.Lease.LeaseID); e != nil {
			return e
		}
		if _, e = bumpRevision(ctx, tx); e != nil {
			return e
		}
		committed = true
		return nil
	})
	if extensionSuperseded(err) {
		return false, nil
	}
	return committed && err == nil, err
}

func (s *Store) extensionActivate(ctx context.Context, id IndexGenerationID) error {
	return s.extensionTransaction(ctx, "", func(tx *sql.Tx) error {
		g, e := extensionLoadGeneration(ctx, tx, id)
		if e != nil {
			return e
		}
		if g.State == "active" {
			return nil
		}
		if g.State != "building" {
			return ErrNoWork
		}
		if e = s.extensionIndexCoverageAt(ctx, tx, id, s.now()); e != nil {
			return e
		}
		// The final fence rechecks coverage with bulk metadata SQL rather than
		// materializing an unbounded source list or issuing 100k point queries.
		s.extensionRegisterIndexActivation(tx, id)
		if _, e = tx.ExecContext(ctx, "UPDATE extension_generations SET state='retired' WHERE state='active'"); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE extension_generations SET state='active' WHERE id=? AND state='building'", id); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE archive_meta SET archive_revision=archive_revision+1,index_generation=index_generation+1 WHERE id=1"); e != nil {
			return e
		}
		return nil
	})
}

func (s *Store) extensionIndexCoverageAt(ctx context.Context, q sqlQuerier, id IndexGenerationID, now time.Time) error {
	g, e := extensionLoadGeneration(ctx, q, id)
	if e != nil {
		return e
	}
	p, e := s.loadPolicy(ctx, q)
	if e != nil {
		return e
	}
	var missing int
	if e = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM extension_dirty WHERE generation=?) OR EXISTS(SELECT 1 FROM extension_jobs WHERE generation=? AND(kind='text_embedding' OR(kind='derived_embedding' AND EXISTS(SELECT 1 FROM extension_job_origins jo WHERE jo.job_id=extension_jobs.id))) AND state IN('pending','leased','failed'))", id, id).Scan(&missing); e != nil {
		return fixedSQLError(e)
	}
	if missing != 0 {
		return ErrNoWork
	}
	// All fragments, including every derived fragment, need compatible vectors.
	if e = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM extension_documents d LEFT JOIN extension_vectors v ON v.document_id=d.id WHERE d.generation=? AND d.embedding_input=0 AND(d.kind='text_window' OR EXISTS(SELECT 1 FROM extension_roots r WHERE r.id=d.origin_document_id)) AND(v.document_id IS NULL OR v.generation<>d.generation OR v.dimensions<>? OR length(v.values_blob)<>? OR d.source_count<>(SELECT count(*) FROM extension_document_sources ds WHERE ds.document_id=d.id)))", id, g.Version.Dimensions, g.Version.Dimensions*4).Scan(&missing); e != nil {
		return fixedSQLError(e)
	}
	if missing != 0 {
		return ErrNoWork
	}
	if e = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM extension_documents d JOIN extension_document_sources ds ON ds.document_id=d.id LEFT JOIN messages m ON m.id=ds.message_id LEFT JOIN archive_policy p ON p.id=1 WHERE d.generation=? AND(m.id IS NULL OR m.account<>ds.account OR m.local_revision<>ds.local_revision OR m.content_hash<>ds.content_hash OR m.expires_at<>ds.expires_at OR p.generation<>ds.policy_generation OR NOT("+extensionSourcePredicate()+")))", id, unixms(now), ceilMillis(now.Add(-p.RollingRetention))).Scan(&missing); e != nil {
		return fixedSQLError(e)
	}
	if missing != 0 {
		return ErrSourceUnavailable
	}
	if e = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages m JOIN archive_policy p ON p.id=1 WHERE `+extensionSourcePredicate()+` AND EXISTS(SELECT 1 FROM(SELECT 'body' AS field,length(CAST(m.body AS BLOB)) AS bytes UNION ALL SELECT 'caption',length(CAST(m.caption AS BLOB))) f WHERE f.bytes>0 AND NOT EXISTS(SELECT 1 FROM extension_window_coverage wc WHERE wc.generation=? AND wc.account=m.account AND wc.message_id=m.id AND wc.local_revision=m.local_revision AND wc.policy_generation=p.generation AND wc.expires_at=m.expires_at AND wc.content_hash=m.content_hash AND wc.field=f.field AND wc.bytes=f.bytes)))`, unixms(now), ceilMillis(now.Add(-p.RollingRetention)), id).Scan(&missing); e != nil {
		return fixedSQLError(e)
	}
	if missing != 0 {
		return ErrNoWork
	}
	return s.extensionDerivedCoverageAt(ctx, q, g, now)
}

var _ IndexStore = (*Store)(nil)
