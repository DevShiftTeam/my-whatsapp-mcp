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
	"unicode/utf8"
)

type extensionPublicationOperation uint8

const (
	extensionLoadWindowBatch extensionPublicationOperation = iota
	extensionRenewDirty
	extensionPublishWindowBatch
	extensionLoadDerivedBatch
	extensionCommitDerivedBatch
)

type extensionPublicationCommand struct {
	op      extensionPublicationOperation
	work    DirtyWork
	lease   JobLease
	ttl     time.Duration
	window  WindowBatchPublication
	derived DerivedBatchPublication
	digest  string
}

func (c extensionPublicationCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c extensionPublicationCommand) priority() bool                    { return c.op == extensionRenewDirty }
func (c extensionPublicationCommand) account() string {
	if c.op == extensionPublishWindowBatch {
		return c.window.Work.Account
	}
	return c.work.Account
}
func (c extensionPublicationCommand) run(s *Store, ctx context.Context) (any, error) {
	switch c.op {
	case extensionLoadWindowBatch:
		return s.extensionWindowBatchInput(ctx, c.work)
	case extensionRenewDirty:
		return s.extensionDirtyRenew(ctx, c.work, c.ttl)
	case extensionPublishWindowBatch:
		return s.extensionWindowBatchPublish(ctx, c.window, c.digest)
	case extensionLoadDerivedBatch:
		return s.extensionDerivedBatchInput(ctx, c.lease)
	case extensionCommitDerivedBatch:
		return s.extensionDerivedBatchCommit(ctx, c.derived, c.digest)
	}
	return nil, ErrInvalidExtension
}

func (s *Store) LoadWindowBatch(ctx context.Context, w DirtyWork) (WindowBatchInput, error) {
	if !extensionValidWork(w) {
		return WindowBatchInput{}, ErrInvalidExtension
	}
	x, e := s.submitExtension(ctx, extensionPublicationCommand{op: extensionLoadWindowBatch, work: w}, 1024)
	if e != nil {
		return WindowBatchInput{}, e
	}
	return x.(WindowBatchInput), nil
}
func (s *Store) RenewDirty(ctx context.Context, w DirtyWork, ttl time.Duration) (time.Time, error) {
	if !extensionValidWork(w) || !extensionValidTTL(ttl) {
		return time.Time{}, ErrInvalidExtension
	}
	x, e := s.submitExtension(ctx, extensionPublicationCommand{op: extensionRenewDirty, work: w, ttl: ttl}, 1024)
	if e != nil {
		return time.Time{}, e
	}
	return x.(time.Time), nil
}
func (s *Store) PublishWindowBatch(ctx context.Context, p WindowBatchPublication) (PublicationProgress, error) {
	if !extensionValidWork(p.Work) || !extensionValidVersion(p.Version) || !validString(string(p.Cursor), 256) {
		return PublicationProgress{}, ErrInvalidExtension
	}
	n, e := extensionDocumentBounds(p.Documents, extensionMaxInputBytes)
	if e != nil {
		return PublicationProgress{}, e
	}
	p.Documents = extensionCloneDocuments(p.Documents)
	digest, e := extensionPublicationDigest(p)
	if e != nil {
		return PublicationProgress{}, e
	}
	x, e := s.submitExtension(ctx, extensionPublicationCommand{op: extensionPublishWindowBatch, window: p, digest: digest}, int64(n+2048))
	if e != nil {
		return PublicationProgress{}, e
	}
	return x.(PublicationProgress), nil
}
func (s *Store) LoadDerivedBatch(ctx context.Context, l JobLease) (DerivedBatchInput, error) {
	if !extensionValidLease(l) {
		return DerivedBatchInput{}, ErrInvalidExtension
	}
	x, e := s.submitExtension(ctx, extensionPublicationCommand{op: extensionLoadDerivedBatch, lease: l}, 1024)
	if e != nil {
		return DerivedBatchInput{}, e
	}
	return x.(DerivedBatchInput), nil
}
func (s *Store) CommitDerivedBatch(ctx context.Context, p DerivedBatchPublication) (PublicationProgress, error) {
	r := p.Result
	if !extensionValidLease(r.Lease) || !extensionValidVersion(r.Version) || !validString(string(r.Generation), 256) || len(r.ContentHash) != 64 || !validString(string(p.Cursor), 256) || len(r.Sources) > extensionMaxSources || len(r.Embeddings) > extensionMaxDocuments {
		return PublicationProgress{}, ErrInvalidExtension
	}
	for _, ref := range r.Sources {
		if !extensionValidSourceMetadata(ref) {
			return PublicationProgress{}, ErrInvalidExtension
		}
	}
	n, e := extensionDocumentBounds(r.Documents, extensionMaxDerivedBytes)
	if e != nil {
		return PublicationProgress{}, e
	}
	r.Sources = append([]SourceRef(nil), r.Sources...)
	r.Documents = extensionCloneDocuments(r.Documents)
	r.Embeddings = append([]Embedding(nil), r.Embeddings...)
	for i, v := range r.Embeddings {
		if !validString(string(v.DocumentID), 256) || len(v.Values) > 4096 {
			return PublicationProgress{}, ErrInvalidExtension
		}
		n += 4*len(v.Values) + len(v.DocumentID)
		r.Embeddings[i].Values = append([]float32(nil), v.Values...)
	}
	p.Result = r
	digest, e := extensionPublicationDigest(p)
	if e != nil {
		return PublicationProgress{}, e
	}
	x, e := s.submitExtension(ctx, extensionPublicationCommand{op: extensionCommitDerivedBatch, derived: p, digest: digest}, int64(n+2048+1024*len(r.Sources)))
	if e != nil {
		return PublicationProgress{}, e
	}
	return x.(PublicationProgress), nil
}

func extensionPublicationDigest(value any) (string, error) {
	encoded, e := json.Marshal(value)
	if e != nil {
		return "", ErrInvalidExtension
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}
func extensionWindowIdentity(w DirtyWork) (string, error) {
	w.LeaseID = ""
	w.LeaseUntil = time.Time{}
	return extensionPublicationDigest(w)
}
func extensionWorkIdentityMatches(a, b DirtyWork) bool {
	a.LeaseUntil = b.LeaseUntil
	return extensionSameWork(a, b)
}

type extensionPublicationState struct {
	kind, workID, account, identity, lease, cursor, lastCursor, lastDigest, lastNext, cleanupAfter string
	generation                                                                                     IndexGenerationID
	leaseUntil                                                                                     int64
	prepared, complete, lastComplete                                                               bool
	ordinal, field, start, loadedEnd                                                               int
}

func extensionLoadPublication(ctx context.Context, q sqlQuerier, kind, id string) (extensionPublicationState, error) {
	p := extensionPublicationState{kind: kind, workID: id}
	e := q.QueryRowContext(ctx, `SELECT account,generation,identity_hash,lease_id,lease_until,cursor,last_cursor,last_digest,last_next,last_complete,prepared,cleanup_after,source_ordinal,field_ordinal,start_byte,loaded_end,complete FROM extension_publications WHERE kind=? AND work_id=?`, kind, id).Scan(&p.account, &p.generation, &p.identity, &p.lease, &p.leaseUntil, &p.cursor, &p.lastCursor, &p.lastDigest, &p.lastNext, &p.lastComplete, &p.prepared, &p.cleanupAfter, &p.ordinal, &p.field, &p.start, &p.loadedEnd, &p.complete)
	if errors.Is(e, sql.ErrNoRows) {
		return p, ErrNoWork
	}
	return p, fixedSQLError(e)
}
func extensionPublicationSources(ctx context.Context, q sqlQuerier, kind, id string) ([]SourceRef, error) {
	rows, e := q.QueryContext(ctx, `SELECT account,message_id,local_revision,policy_generation,expires_at,content_hash FROM extension_publication_sources WHERE kind=? AND work_id=? ORDER BY ordinal LIMIT 2049`, kind, id)
	if e != nil {
		return nil, fixedSQLError(e)
	}
	defer rows.Close()
	var refs []SourceRef
	for rows.Next() {
		var ref SourceRef
		var expiry int64
		if e = rows.Scan(&ref.Account, &ref.Version.MessageID, &ref.Version.Revision, &ref.Version.PolicyGeneration, &expiry, &ref.ContentHash); e != nil {
			return nil, fixedSQLError(e)
		}
		ref.Version.ExpiresAt = fromMillis(expiry)
		refs = append(refs, ref)
		if len(refs) > extensionMaxSources {
			return nil, ErrLimitExceeded
		}
	}
	return refs, fixedSQLError(rows.Err())
}
func extensionCreatePublication(ctx context.Context, tx *sql.Tx, p extensionPublicationState, refs []SourceRef) error {
	cursor, e := randomID()
	if e != nil {
		return e
	}
	p.cursor = cursor
	_, e = tx.ExecContext(ctx, `INSERT INTO extension_publications(kind,work_id,account,generation,identity_hash,lease_id,lease_until,cursor) VALUES(?,?,?,?,?,?,?,?)`, p.kind, p.workID, p.account, p.generation, p.identity, p.lease, p.leaseUntil, p.cursor)
	if e != nil {
		return fixedSQLError(e)
	}
	for i, ref := range refs {
		if _, e = tx.ExecContext(ctx, `INSERT INTO extension_publication_sources(kind,work_id,account,message_id,local_revision,policy_generation,expires_at,content_hash,ordinal) VALUES(?,?,?,?,?,?,?,?,?)`, p.kind, p.workID, ref.Account, ref.Version.MessageID, ref.Version.Revision, ref.Version.PolicyGeneration, unixms(ref.Version.ExpiresAt), ref.ContentHash, i); e != nil {
			return fixedSQLError(e)
		}
	}
	return nil
}
func (s *Store) extensionPublicationAuthority(ctx context.Context, q sqlQuerier, p extensionPublicationState) error {
	if p.leaseUntil <= unixms(s.now()) {
		return ErrNoWork
	}
	g, e := extensionLoadGeneration(ctx, q, p.generation)
	if e != nil {
		return e
	}
	if g.State == "retired" {
		return ErrNoWork
	}
	if e = s.extensionRegisterGeneration(q, g); e != nil {
		return e
	}
	refs, e := extensionPublicationSources(ctx, q, p.kind, p.workID)
	if e != nil {
		return e
	}
	if len(refs) == 0 {
		return ErrSourceUnavailable
	}
	if _, e = s.extensionCheckSources(ctx, q, refs, p.kind == "derived"); e != nil {
		return e
	}
	s.extensionRegisterDeadline(q, fromMillis(p.leaseUntil))
	return nil
}
func extensionPublicationReplay(p extensionPublicationState, cursor PublicationCursor, digest string) (PublicationProgress, bool, error) {
	if string(cursor) != p.lastCursor {
		return PublicationProgress{}, false, nil
	}
	if digest != p.lastDigest {
		return PublicationProgress{}, true, ErrInvalidExtension
	}
	return PublicationProgress{Accepted: true, Complete: p.lastComplete, Next: PublicationCursor(p.lastNext)}, true, nil
}
func extensionAdvancePublication(ctx context.Context, tx *sql.Tx, p extensionPublicationState, digest string, complete bool) (PublicationProgress, error) {
	next := ""
	var e error
	if !complete {
		next, e = randomID()
		if e != nil {
			return PublicationProgress{}, e
		}
	}
	_, e = tx.ExecContext(ctx, `UPDATE extension_publications SET source_ordinal=?,field_ordinal=?,start_byte=?,loaded_end=0,last_cursor=cursor,last_digest=?,last_next=?,last_complete=?,cursor=?,complete=? WHERE kind=? AND work_id=? AND lease_id=?`, p.ordinal, p.field, p.start, digest, next, complete, next, complete, p.kind, p.workID, p.lease)
	return PublicationProgress{Accepted: true, Complete: complete, Next: PublicationCursor(next)}, fixedSQLError(e)
}

func (s *Store) extensionDirtyRenew(ctx context.Context, w DirtyWork, ttl time.Duration) (time.Time, error) {
	var result time.Time
	e := s.extensionTransaction(ctx, w.Account, func(tx *sql.Tx) error {
		d, e := extensionLoadDirty(ctx, tx, w.ID)
		if e != nil {
			return e
		}
		if !extensionWorkIdentityMatches(d.work, w) || !d.work.LeaseUntil.After(s.now()) {
			return ErrNoWork
		}
		refs, e := extensionLoadSources(ctx, tx, "dirty", w.ID)
		if e != nil {
			return e
		}
		if len(refs) == 0 {
			return ErrNoWork
		}
		if _, e = s.extensionCheckSources(ctx, tx, refs, false); e != nil {
			return e
		}
		g, e := extensionLoadGeneration(ctx, tx, w.Generation)
		if e != nil {
			return e
		}
		if g.State == "retired" {
			return ErrNoWork
		}
		if e = s.extensionRegisterGeneration(tx, g); e != nil {
			return e
		}
		s.extensionRegisterDeadline(tx, d.work.LeaseUntil)
		result = fromMillis(unixms(s.now().Add(ttl)))
		if _, e = tx.ExecContext(ctx, "UPDATE extension_dirty SET lease_until=? WHERE id=? AND lease_id=?", unixms(result), w.ID, w.LeaseID); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE extension_publications SET lease_until=? WHERE kind='window' AND work_id=? AND lease_id=?", unixms(result), w.ID, w.LeaseID); e != nil {
			return e
		}
		s.extensionRegisterDeadline(tx, result)
		return nil
	})
	return result, e
}

func extensionWindowPosition(messages []WindowMessage, ordinal, field, start int) (int, int, int, error) {
	if ordinal < 0 || ordinal > len(messages) || field < 0 || field > 1 || start < 0 {
		return 0, 0, 0, ErrSchemaChanged
	}
	for ordinal < len(messages) {
		text := messages[ordinal].Body
		if field == 1 {
			text = messages[ordinal].Caption
		}
		if start > len(text) || !utf8.ValidString(text[:start]) {
			return 0, 0, 0, ErrSchemaChanged
		}
		if start < len(text) {
			return ordinal, field, start, nil
		}
		start = 0
		field++
		if field == 2 {
			ordinal++
			field = 0
		}
	}
	if start != 0 {
		return 0, 0, 0, ErrSchemaChanged
	}
	return ordinal, 0, 0, nil
}
func (s *Store) extensionPreparePublication(ctx context.Context, tx *sql.Tx, p *extensionPublicationState) (bool, error) {
	var query string
	if p.kind == "window" {
		query = `SELECT DISTINCT d.id FROM extension_documents d JOIN extension_document_sources ds ON ds.document_id=d.id JOIN extension_publication_sources ps ON ps.message_id=ds.message_id AND ps.account=ds.account WHERE ps.kind='window' AND ps.work_id=? AND d.generation=? AND d.kind='text_window' AND d.id>? ORDER BY d.id LIMIT 256`
	} else {
		query = `SELECT d.id FROM extension_documents d JOIN extension_job_origins jo ON jo.root_id=d.origin_document_id WHERE jo.job_id=? AND d.generation=? AND d.embedding_input=0 AND d.id>? ORDER BY d.id LIMIT 256`
	}
	rows, e := tx.QueryContext(ctx, query, p.workID, p.generation, p.cleanupAfter)
	if e != nil {
		return false, fixedSQLError(e)
	}
	var ids []DocumentID
	for rows.Next() {
		var id DocumentID
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return false, fixedSQLError(e)
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return false, fixedSQLError(e)
	}
	for _, id := range ids {
		if p.kind == "window" {
			// Retain the full old extent when deleting a window removes an
			// unclaimed member. SQL inspects membership metadata only.
			_, e = tx.ExecContext(ctx, `INSERT INTO extension_dirty(id,account,chat_id,generation,policy_generation,from_time,until_time,dirty_revision)
			 SELECT lower(hex(randomblob(16))),m.account,m.chat_id,d.generation,ap.generation,MIN(m.original_sent_at),MAX(m.original_sent_at)+1,am.archive_revision+1
			 FROM extension_documents d JOIN extension_document_sources ds ON ds.document_id=d.id JOIN messages m ON m.id=ds.message_id JOIN archive_policy ap ON ap.id=1 JOIN archive_meta am ON am.id=1
			 WHERE d.id=? AND EXISTS(SELECT 1 FROM extension_document_sources old WHERE old.document_id=d.id AND NOT EXISTS(SELECT 1 FROM extension_publication_sources ps WHERE ps.kind='window' AND ps.work_id=? AND ps.account=old.account AND ps.message_id=old.message_id)) GROUP BY m.account,m.chat_id,d.generation
			 ON CONFLICT(chat_id,generation) DO UPDATE SET from_time=MIN(from_time,excluded.from_time),until_time=MAX(until_time,excluded.until_time),dirty_revision=MAX(dirty_revision,excluded.dirty_revision),cursor_time=0,cursor_id=''`, id, p.workID)
			if e != nil {
				return false, fixedSQLError(e)
			}
		}
		if e = extensionDeleteDocument(ctx, tx, id); e != nil {
			return false, e
		}
		p.cleanupAfter = string(id)
	}
	if len(ids) > 0 {
		if _, e = bumpRevision(ctx, tx); e != nil {
			return false, e
		}
	}
	p.prepared = len(ids) < extensionMaxDocuments
	_, e = tx.ExecContext(ctx, `UPDATE extension_publications SET cleanup_after=?,prepared=? WHERE kind=? AND work_id=?`, p.cleanupAfter, p.prepared, p.kind, p.workID)
	return p.prepared, fixedSQLError(e)
}
func (s *Store) extensionWindowBatchInput(ctx context.Context, w DirtyWork) (WindowBatchInput, error) {
	var result WindowBatchInput
	e := s.extensionTransaction(ctx, w.Account, func(tx *sql.Tx) error {
		d, e := extensionLoadDirty(ctx, tx, w.ID)
		if e != nil {
			return e
		}
		if !extensionWorkIdentityMatches(d.work, w) || !d.work.LeaseUntil.After(s.now()) {
			return ErrNoWork
		}
		g, e := extensionLoadGeneration(ctx, tx, w.Generation)
		if e != nil {
			return e
		}
		if g.State == "retired" {
			return ErrNoWork
		}
		if e = s.extensionRegisterGeneration(tx, g); e != nil {
			return e
		}
		refs, e := extensionLoadSources(ctx, tx, "dirty", w.ID)
		if e != nil {
			return e
		}
		if len(refs) == 0 || len(refs) > extensionMaxDocuments {
			return ErrNoWork
		}
		if _, e = s.extensionCheckSources(ctx, tx, refs, false); e != nil {
			return e
		}
		s.extensionRegisterDeadline(tx, d.work.LeaseUntil)
		identity, e := extensionWindowIdentity(d.work)
		if e != nil {
			return e
		}
		p, e := extensionLoadPublication(ctx, tx, "window", w.ID)
		if errors.Is(e, ErrNoWork) {
			e = extensionCreatePublication(ctx, tx, extensionPublicationState{kind: "window", workID: w.ID, account: w.Account, generation: w.Generation, identity: identity, lease: string(w.LeaseID), leaseUntil: unixms(d.work.LeaseUntil)}, refs)
			if e == nil {
				p, e = extensionLoadPublication(ctx, tx, "window", w.ID)
			}
		}
		if e != nil {
			return e
		}
		if p.identity != identity || p.lease != string(w.LeaseID) || p.complete {
			return ErrNoWork
		}
		result.Work = d.work
		if !p.prepared {
			if _, e = s.extensionPreparePublication(ctx, tx, &p); e != nil {
				return e
			}
			return nil
		}
		input, e := s.extensionWindowMessages(ctx, tx, d.work)
		if e != nil {
			return e
		}
		p.ordinal, p.field, p.start, e = extensionWindowPosition(input, p.ordinal, p.field, p.start)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE extension_publications SET source_ordinal=?,field_ordinal=?,start_byte=? WHERE kind='window' AND work_id=?`, p.ordinal, p.field, p.start, w.ID); e != nil {
			return e
		}
		field := "body"
		if p.field == 1 {
			field = "caption"
		}
		result.Ready = true
		result.Cursor = PublicationCursor(p.cursor)
		result.Start = WindowPosition{SourceOrdinal: p.ordinal, Field: field, StartByte: p.start}
		result.Messages = input
		return nil
	})
	return result, e
}

func extensionValidateWindowPrefix(docs []Document, input []WindowMessage, v IndexVersion, p extensionPublicationState) (extensionPublicationState, error) {
	ordinal, field, start, e := extensionWindowPosition(input, p.ordinal, p.field, p.start)
	if e != nil {
		return p, e
	}
	initialOrdinal, initialField, initialStart := ordinal, field, start
	byID := make(map[MessageID]int, len(input))
	for i, m := range input {
		byID[m.Source.Version.MessageID] = i
	}
	for _, d := range docs {
		if d.Kind != DocumentWindow || d.PassageTokens < 1 || d.PassageTokens > v.MaxPassageTokens || len(d.Spans) == 0 || d.Provenance.Kind != "" && d.Provenance.Kind != DocumentWindow || d.Provenance.OffsetKind != "" && d.Provenance.OffsetKind != "none" || d.Provenance.MediaID != "" || d.Provenance.ProfileID != "" || d.Provenance.ExtractorVersion != "" || d.Provenance.PromptVersion != "" || d.Provenance.StartMS != 0 || d.Provenance.EndMS != 0 {
			return p, ErrInvalidExtension
		}
		members := make(map[MessageID]SourceRef, len(d.Sources))
		for _, ref := range d.Sources {
			index, ok := byID[ref.Version.MessageID]
			if !ok || !extensionSameSources([]SourceRef{ref}, []SourceRef{input[index].Source}) || members[ref.Version.MessageID].Account != "" {
				return p, ErrInvalidExtension
			}
			members[ref.Version.MessageID] = ref
		}
		var slices []string
		used := make(map[MessageID]bool)
		lastOrdinal, lastField := -1, -1
		for _, span := range d.Spans {
			if ordinal >= len(input) {
				return p, ErrInvalidExtension
			}
			name := "body"
			original := input[ordinal].Body
			if field == 1 {
				name = "caption"
				original = input[ordinal].Caption
			}
			if span.Field != name || !extensionSameSources([]SourceRef{span.Source}, []SourceRef{input[ordinal].Source}) || members[span.Source.Version.MessageID].Account == "" || span.StartByte != start || span.EndByte <= start || span.EndByte > len(original) || !utf8.ValidString(original[start:span.EndByte]) || ordinal < lastOrdinal || ordinal == lastOrdinal && field <= lastField {
				return p, ErrInvalidExtension
			}
			lastOrdinal, lastField = ordinal, field
			slices = append(slices, original[start:span.EndByte])
			used[span.Source.Version.MessageID] = true
			ordinal, field, start, e = extensionWindowPosition(input, ordinal, field, span.EndByte)
			if e != nil {
				return p, e
			}
		}
		if len(used) != len(members) || strings.Join(slices, "\n") != d.Text {
			return p, ErrInvalidExtension
		}
	}
	if ordinal == initialOrdinal && field == initialField && start == initialStart && ordinal != len(input) {
		return p, ErrInvalidExtension
	}
	p.ordinal, p.field, p.start = ordinal, field, start
	return p, nil
}

func extensionSaveWindowCoverage(ctx context.Context, tx *sql.Tx, g IndexGenerationID, input []WindowMessage) error {
	for _, m := range input {
		for _, field := range []string{"body", "caption"} {
			text := m.Body
			if field == "caption" {
				text = m.Caption
			}
			r := m.Source
			if _, e := tx.ExecContext(ctx, `INSERT INTO extension_window_coverage(generation,account,message_id,local_revision,policy_generation,expires_at,content_hash,field,bytes) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(generation,account,message_id,field) DO UPDATE SET local_revision=excluded.local_revision,policy_generation=excluded.policy_generation,expires_at=excluded.expires_at,content_hash=excluded.content_hash,bytes=excluded.bytes`, g, r.Account, r.Version.MessageID, r.Version.Revision, r.Version.PolicyGeneration, unixms(r.Version.ExpiresAt), r.ContentHash, field, len(text)); e != nil {
				return fixedSQLError(e)
			}
		}
	}
	return nil
}
func (s *Store) extensionAcknowledgeWindow(ctx context.Context, tx *sql.Tx, w DirtyWork, now time.Time) error {
	d, e := extensionLoadDirty(ctx, tx, w.ID)
	if e != nil {
		return e
	}
	if d.liveRevision == w.DirtyRevision {
		policy, e := s.loadPolicy(ctx, tx)
		if e != nil {
			return e
		}
		var remaining int
		e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages m JOIN archive_policy p ON p.id=1 WHERE m.account=? AND m.chat_id=? AND m.original_sent_at>=? AND m.original_sent_at<? AND(m.original_sent_at>? OR(m.original_sent_at=? AND m.id>?)) AND `+extensionSourcePredicate()+` AND(length(m.body)>0 OR length(m.caption)>0))`, w.Account, w.ChatID, d.liveFrom, d.liveUntil, d.claimCursorTime, d.claimCursorTime, d.claimCursorID, unixms(now), ceilMillis(now.Add(-policy.RollingRetention))).Scan(&remaining)
		if e != nil {
			return e
		}
		if remaining == 0 {
			_, e = tx.ExecContext(ctx, "DELETE FROM extension_dirty WHERE id=? AND lease_id=? AND dirty_revision=?", w.ID, w.LeaseID, w.DirtyRevision)
		} else {
			_, e = tx.ExecContext(ctx, "UPDATE extension_dirty SET cursor_time=claim_cursor_time,cursor_id=claim_cursor_id,lease_id='',lease_until=0 WHERE id=? AND lease_id=?", w.ID, w.LeaseID)
		}
	} else {
		_, e = tx.ExecContext(ctx, "UPDATE extension_dirty SET lease_id='',lease_until=0,cursor_time=0,cursor_id='' WHERE id=? AND lease_id=?", w.ID, w.LeaseID)
	}
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "DELETE FROM extension_dirty_sources WHERE dirty_id=?", w.ID)
	return e
}
func (s *Store) extensionWindowBatchPublish(ctx context.Context, b WindowBatchPublication, digest string) (PublicationProgress, error) {
	var result PublicationProgress
	e := s.extensionTransaction(ctx, b.Work.Account, func(tx *sql.Tx) error {
		p, e := extensionLoadPublication(ctx, tx, "window", b.Work.ID)
		if extensionSuperseded(e) {
			return nil
		}
		if e != nil {
			return e
		}
		identity, e := extensionWindowIdentity(b.Work)
		if e != nil {
			return e
		}
		if p.identity != identity || p.lease != string(b.Work.LeaseID) {
			return nil
		}
		if e = s.extensionPublicationAuthority(ctx, tx, p); extensionSuperseded(e) {
			return nil
		} else if e != nil {
			return e
		}
		g, e := extensionLoadGeneration(ctx, tx, p.generation)
		if e != nil {
			return e
		}
		if g.Version != b.Version {
			return nil
		}
		if e = s.extensionRegisterGeneration(tx, g); e != nil {
			return e
		}
		if replay, ok, e := extensionPublicationReplay(p, b.Cursor, digest); ok {
			result = replay
			return e
		}
		if p.complete || !p.prepared || p.cursor != string(b.Cursor) {
			return nil
		}
		d, e := extensionLoadDirty(ctx, tx, b.Work.ID)
		if e != nil {
			return e
		}
		if !extensionWorkIdentityMatches(d.work, b.Work) {
			return nil
		}
		input, e := s.extensionWindowMessages(ctx, tx, d.work)
		if extensionSuperseded(e) {
			return nil
		}
		if e != nil {
			return e
		}
		next, e := extensionValidateWindowPrefix(b.Documents, input, g.Version, p)
		if e != nil {
			return e
		}
		for _, document := range b.Documents {
			if !strings.HasPrefix(string(document.ID), string(g.ID)+"-") {
				return ErrInvalidExtension
			}
			if e = extensionSaveDocument(ctx, tx, g, document); e != nil {
				return e
			}
			if e = extensionEnqueueJob(ctx, tx, Job{Kind: JobTextEmbedding, Generation: g.ID, Version: g.Version, Sources: document.Sources, Documents: []DocumentID{document.ID}}); e != nil {
				return e
			}
		}
		complete := next.ordinal == len(input)
		if complete {
			if e = extensionSaveWindowCoverage(ctx, tx, g.ID, input); e != nil {
				return e
			}
			if e = s.extensionAcknowledgeWindow(ctx, tx, d.work, s.now()); e != nil {
				return e
			}
		}
		if _, e = bumpRevision(ctx, tx); e != nil {
			return e
		}
		result, e = extensionAdvancePublication(ctx, tx, next, digest, complete)
		return e
	})
	if extensionSuperseded(e) {
		return PublicationProgress{}, nil
	}
	if e != nil {
		return PublicationProgress{}, e
	}
	return result, nil
}

func (s *Store) extensionAuthorizedOriginJob(ctx context.Context, q sqlQuerier, l JobLease) (Job, DerivedOrigin, error) {
	x, e := extensionLoadJob(ctx, q, l.JobID)
	if e != nil {
		return Job{}, DerivedOrigin{}, e
	}
	if !extensionMatchingLease(x, l, s.now()) || x.job.Kind != JobDerivedEmbedding {
		return Job{}, DerivedOrigin{}, ErrNoWork
	}
	var root DocumentID
	e = q.QueryRowContext(ctx, "SELECT root_id FROM extension_job_origins WHERE job_id=?", l.JobID).Scan(&root)
	if errors.Is(e, sql.ErrNoRows) {
		return Job{}, DerivedOrigin{}, ErrInvalidExtension
	}
	if e != nil {
		return Job{}, DerivedOrigin{}, fixedSQLError(e)
	}
	origin, e := s.extensionAuthorizeRoot(ctx, q, root)
	if e != nil {
		return Job{}, DerivedOrigin{}, e
	}
	if !extensionSameSources(origin.Sources, x.job.Sources) || len(x.job.Documents) != 1 || x.job.Documents[0] != root {
		return Job{}, DerivedOrigin{}, ErrSourceUnavailable
	}
	g, e := extensionLoadGeneration(ctx, q, x.job.Generation)
	if e != nil {
		return Job{}, DerivedOrigin{}, e
	}
	if g.State == "retired" {
		return Job{}, DerivedOrigin{}, ErrNoWork
	}
	if e = s.extensionRegisterGeneration(q, g); e != nil {
		return Job{}, DerivedOrigin{}, e
	}
	hash, e := CanonicalOriginWorkDigest(g.ID, g.Version, origin)
	if e != nil {
		return Job{}, DerivedOrigin{}, e
	}
	if hash != x.job.ContentHash {
		return Job{}, DerivedOrigin{}, ErrSourceUnavailable
	}
	s.extensionRegisterDeadline(q, x.job.LeaseUntil)
	return x.job, origin, nil
}
func (s *Store) extensionDerivedBatchInput(ctx context.Context, l JobLease) (DerivedBatchInput, error) {
	var result DerivedBatchInput
	account, e := extensionPublicationJobAccount(ctx, s.writeDB, l.JobID)
	if e != nil {
		return result, e
	}
	e = s.extensionTransaction(ctx, account, func(tx *sql.Tx) error {
		j, origin, e := s.extensionAuthorizedOriginJob(ctx, tx, l)
		if e != nil {
			return e
		}
		p, e := extensionLoadPublication(ctx, tx, "derived", string(j.ID))
		if errors.Is(e, ErrNoWork) {
			e = extensionCreatePublication(ctx, tx, extensionPublicationState{kind: "derived", workID: string(j.ID), account: j.Sources[0].Account, generation: j.Generation, identity: j.ContentHash, lease: string(j.Lease.LeaseID), leaseUntil: unixms(j.LeaseUntil)}, j.Sources)
			if e == nil {
				p, e = extensionLoadPublication(ctx, tx, "derived", string(j.ID))
			}
		}
		if e != nil {
			return e
		}
		if p.identity != j.ContentHash {
			return ErrSourceUnavailable
		}
		if p.lease != string(j.Lease.LeaseID) {
			cursor, e := randomID()
			if e != nil {
				return e
			}
			// A failed/expired worker retains its accepted prefix. Explicit repair
			// of a previously completed job starts bounded obsolete cleanup again.
			if p.complete {
				p.prepared = false
				p.start = 0
				p.cleanupAfter = ""
				p.complete = false
			}
			if _, e = tx.ExecContext(ctx, `UPDATE extension_publications SET lease_id=?,lease_until=?,cursor=?,last_cursor='',last_digest='',last_next='',last_complete=0,prepared=?,start_byte=?,loaded_end=0,cleanup_after=?,complete=0 WHERE kind='derived' AND work_id=?`, j.Lease.LeaseID, unixms(j.LeaseUntil), cursor, p.prepared, p.start, p.cleanupAfter, j.ID); e != nil {
				return e
			}
			p.lease, p.leaseUntil, p.cursor = string(j.Lease.LeaseID), unixms(j.LeaseUntil), cursor
			p.loadedEnd = 0
		}
		result.Job = j
		result.Origin = origin
		if !p.prepared {
			if _, e = s.extensionPreparePublication(ctx, tx, &p); e != nil {
				return e
			}
			return nil
		}
		if p.start < 0 || p.start >= origin.Bytes {
			return ErrSchemaChanged
		}
		var text []byte
		if e = tx.QueryRowContext(ctx, `SELECT substr(CAST(text AS BLOB),?,?) FROM extension_roots WHERE id=?`, p.start+1, extensionMaxPassageBytes, origin.ID).Scan(&text); e != nil {
			return fixedSQLError(e)
		}
		for removed := 0; !utf8.Valid(text) && removed < 4; removed++ {
			text = text[:len(text)-1]
		}
		if len(text) == 0 || !utf8.Valid(text) || p.start+len(text) > origin.Bytes {
			return ErrSchemaChanged
		}
		p.loadedEnd = p.start + len(text)
		if _, e = tx.ExecContext(ctx, `UPDATE extension_publications SET loaded_end=? WHERE kind='derived' AND work_id=? AND lease_id=?`, p.loadedEnd, j.ID, j.Lease.LeaseID); e != nil {
			return e
		}
		result.Ready = true
		result.Cursor = PublicationCursor(p.cursor)
		result.StartByte = p.start
		result.EndByte = p.loadedEnd
		result.Text = string(text)
		return nil
	})
	return result, e
}
func (s *Store) extensionDerivedBatchCommit(ctx context.Context, b DerivedBatchPublication, digest string) (PublicationProgress, error) {
	var result PublicationProgress
	account, e := extensionPublicationJobAccount(ctx, s.writeDB, b.Result.Lease.JobID)
	if extensionSuperseded(e) {
		return result, nil
	}
	if e != nil {
		return result, e
	}
	e = s.extensionTransaction(ctx, account, func(tx *sql.Tx) error {
		p, e := extensionLoadPublication(ctx, tx, "derived", string(b.Result.Lease.JobID))
		if extensionSuperseded(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if p.lease != string(b.Result.Lease.LeaseID) || p.identity != b.Result.ContentHash {
			return nil
		}
		stored, e := extensionLoadJob(ctx, tx, b.Result.Lease.JobID)
		if extensionSuperseded(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if p.complete {
			if stored.state != "complete" || stored.job.ContentHash != p.identity {
				return nil
			}
		} else if !extensionMatchingLease(stored, b.Result.Lease, s.now()) {
			return nil
		}
		if e = s.extensionPublicationAuthority(ctx, tx, p); extensionSuperseded(e) {
			return nil
		} else if e != nil {
			return e
		}
		var root DocumentID
		if e = tx.QueryRowContext(ctx, "SELECT root_id FROM extension_job_origins WHERE job_id=?", p.workID).Scan(&root); errors.Is(e, sql.ErrNoRows) {
			return nil
		} else if e != nil {
			return e
		}
		origin, e := s.extensionAuthorizeRoot(ctx, tx, root)
		if extensionSuperseded(e) {
			return nil
		}
		if e != nil {
			return e
		}
		g, e := extensionLoadGeneration(ctx, tx, p.generation)
		if e != nil {
			return e
		}
		r := b.Result
		if r.Generation != g.ID || r.Version != g.Version || !extensionSameSources(r.Sources, origin.Sources) {
			return nil
		}
		if e = s.extensionRegisterGeneration(tx, g); e != nil {
			return e
		}
		if replay, ok, e := extensionPublicationReplay(p, b.Cursor, digest); ok {
			result = replay
			return e
		}
		if p.complete || !p.prepared || p.cursor != string(b.Cursor) || p.loadedEnd <= p.start {
			return nil
		}
		j, checked, e := s.extensionAuthorizedOriginJob(ctx, tx, r.Lease)
		if extensionSuperseded(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if checked.ID != origin.ID || j.ContentHash != r.ContentHash {
			return nil
		}
		if len(r.Documents) == 0 || len(r.Documents) != len(r.Embeddings) {
			return ErrInvalidExtension
		}
		var loaded []byte
		if e = tx.QueryRowContext(ctx, `SELECT substr(CAST(text AS BLOB),?,?) FROM extension_roots WHERE id=?`, p.start+1, p.loadedEnd-p.start, origin.ID).Scan(&loaded); e != nil {
			return e
		}
		vectors := make(map[DocumentID][]float32, len(r.Embeddings))
		for _, v := range r.Embeddings {
			if vectors[v.DocumentID] != nil || !extensionVectorValid(v.Values, g.Version.Dimensions) {
				return ErrInvalidExtension
			}
			vectors[v.DocumentID] = v.Values
		}
		start := p.start
		for _, d := range r.Documents {
			end := start + len(d.Text)
			if d.ID != DocumentID(fmt.Sprintf("%s-%d-%d", j.ID, start, end)) || len(d.Text) == 0 || end > p.loadedEnd || !utf8.ValidString(d.Text) || string(loaded[start-p.start:end-p.start]) != d.Text || d.Kind != origin.Provenance.Kind || d.Provenance != origin.Provenance || len(d.Spans) != 0 || !extensionSameSources(d.Sources, origin.Sources) || d.PassageTokens < 1 || d.PassageTokens > g.Version.MaxPassageTokens || vectors[d.ID] == nil {
				return ErrInvalidExtension
			}
			start = end
		}
		// The archive derives the accepted prefix; no worker EOF/progress flag
		// can assert coverage of bytes omitted from this exact publication.
		start = p.start
		for _, d := range r.Documents {
			end := start + len(d.Text)
			// Membership order is the immutable origin order, including the
			// citation anchor. Callers may supply an equal set in another order.
			d.Sources = append([]SourceRef(nil), origin.Sources...)
			if e = extensionSaveDocument(ctx, tx, g, d); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, `UPDATE extension_documents SET origin_document_id=?,origin_start_byte=?,origin_end_byte=? WHERE id=? AND generation=? AND embedding_input=0`, origin.ID, start, end, d.ID, g.ID); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, `INSERT INTO extension_vectors(document_id,generation,dimensions,values_blob) VALUES(?,?,?,?)`, d.ID, g.ID, g.Version.Dimensions, extensionVectorBlob(vectors[d.ID])); e != nil {
				return e
			}
			start = end
		}
		p.start = start
		complete := start == origin.Bytes
		if complete {
			valid, e := extensionOriginPartitionComplete(ctx, tx, g.ID, origin.ID, origin.Bytes)
			if e != nil {
				return e
			}
			if !valid {
				return ErrInvalidExtension
			}
			if _, e = tx.ExecContext(ctx, `UPDATE extension_jobs SET state='complete',lease_id='',lease_until=0,last_code='' WHERE id=? AND lease_id=? AND state='leased'`, j.ID, j.Lease.LeaseID); e != nil {
				return e
			}
		}
		if _, e = bumpRevision(ctx, tx); e != nil {
			return e
		}
		result, e = extensionAdvancePublication(ctx, tx, p, digest, complete)
		return e
	})
	if extensionSuperseded(e) {
		return PublicationProgress{}, nil
	}
	if e != nil {
		return PublicationProgress{}, e
	}
	return result, nil
}

// These methods run on the owned writer lane. Account selection comes from
// durable work metadata; callers cannot redirect its final safety fence.
func extensionPublicationJobAccount(ctx context.Context, q sqlQuerier, id JobID) (string, error) {
	var account string
	e := q.QueryRowContext(ctx, "SELECT account FROM extension_jobs WHERE id=?", id).Scan(&account)
	if errors.Is(e, sql.ErrNoRows) {
		return "", ErrNoWork
	}
	if e != nil {
		return "", fixedSQLError(e)
	}
	if !validString(account, 512) {
		return "", ErrSchemaChanged
	}
	return account, nil
}

var _ PublicationStore = (*Store)(nil)
