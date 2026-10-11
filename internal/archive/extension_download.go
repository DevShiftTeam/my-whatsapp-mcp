package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

const extensionDownloadReferenceLimit = 8192
const extensionDownloadBatch = 32
const extensionDownloadCleanupIncomplete Error = "media_cleanup_incomplete"

type extensionDownloadOperation uint8

const (
	extensionDownloadClaim extensionDownloadOperation = iota
	extensionDownloadLoad
	extensionDownloadFinish
	extensionDownloadRetry
)

type extensionDownloadCommand struct {
	op    extensionDownloadOperation
	claim OriginalDownloadClaim
	lease OriginalDownloadLease
	retry OriginalDownloadRetry
}

func (c extensionDownloadCommand) priority() bool                    { return false }
func (c extensionDownloadCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c extensionDownloadCommand) account() string {
	if c.op == extensionDownloadClaim {
		return c.claim.Account
	}
	return ""
}
func (c extensionDownloadCommand) run(s *Store, ctx context.Context) (any, error) {
	var value any
	var loadNow, loadSample time.Time
	if c.op == extensionDownloadLoad {
		loadNow, loadSample = s.now(), time.Now()
	}
	err := s.extensionTransaction(ctx, c.account(), func(tx *sql.Tx) error {
		var err error
		switch c.op {
		case extensionDownloadClaim:
			value, err = s.extensionClaimOriginalDownload(ctx, tx, c.claim)
		case extensionDownloadLoad:
			value, err = s.extensionLoadOriginalDownload(ctx, tx, c.lease)
		case extensionDownloadFinish:
			value, err = s.extensionFinishOriginalDownload(ctx, tx, c.lease)
		case extensionDownloadRetry:
			err = s.extensionRetryOriginalDownload(ctx, tx, c.retry)
		default:
			err = ErrInvalidExtension
		}
		return err
	})
	if c.op == extensionDownloadFinish && (errors.Is(err, ErrNoWork) || errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged)) {
		return false, nil
	}
	if err != nil {
		if in, ok := value.(OriginalDownloadInput); ok {
			clear(in.ProtocolReference)
		}
		return nil, err
	}
	if c.op == extensionDownloadClaim && value.(OriginalDownloadWork).Lease.MediaID == "" {
		return OriginalDownloadWork{}, ErrNoWork
	}
	if c.op == extensionDownloadLoad {
		in := value.(OriginalDownloadInput)
		if err = s.extensionOriginalDownloadResponse(ctx, in, loadNow, loadSample); err != nil {
			clear(in.ProtocolReference)
			return nil, err
		}
	}
	return value, nil
}

// A private protocol input has its own final response fence. Connection
// admission and the service clock run before the lock; no callback follows it.
// The earlier monotonic sample also covers a delayed initial write COMMIT when
// an injected service clock remains fixed.
func (s *Store) extensionOriginalDownloadResponse(ctx context.Context, in OriginalDownloadInput, earlierNow, earlierSample time.Time) error {
	expectedDescriptor, _, valid := extensionDownloadDescriptor(in.Work.Kind, in.ProtocolReference, in.Work.DeclaredBytes)
	if !valid {
		return ErrSourceUnavailable
	}
	now := s.now()
	if elapsedNow := earlierNow.Add(time.Since(earlierSample)); elapsedNow.After(now) {
		now = elapsedNow
	}
	sample := time.Now()
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	conn, err := s.readDB.Conn(bounded)
	if err != nil {
		return fixedSQLError(err)
	}
	defer conn.Close()
	s.safetyMu.RLock()
	defer s.safetyMu.RUnlock()
	if err = s.extensionSafetyLocked(in.Work.Source.Account); err != nil {
		return err
	}
	tx, err := conn.BeginTx(bounded, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fixedSQLError(err)
	}
	defer tx.Rollback()
	r, err := extensionLoadDownloadRow(bounded, tx, in.Work.Lease.MediaID)
	if err != nil {
		return err
	}
	if r.work.Lease != in.Work.Lease || r.work.Source != in.Work.Source || r.state != "leased" || r.work.LeaseUntil != in.Work.LeaseUntil || r.protocol != extensionDownloadSHA(in.ProtocolReference) || r.descriptor != expectedDescriptor {
		return ErrNoWork
	}
	current, err := s.extensionCheckSourcesAt(bounded, tx, []SourceRef{in.Work.Source}, true, now.Add(time.Since(sample)))
	if err != nil {
		return err
	}
	var reference []byte
	var kind MessageKind
	var declared, rolling int64
	var key SourceKey
	err = tx.QueryRowContext(bounded, `SELECT CASE WHEN typeof(a.kind)='text' AND length(CAST(a.kind AS BLOB))<=5 THEN a.kind ELSE '' END,CASE WHEN typeof(a.declared_bytes)='integer' AND a.declared_bytes>=0 THEN a.declared_bytes ELSE -1 END,CASE WHEN typeof(a.protocol_reference)='blob' AND length(a.protocol_reference) BETWEEN 1 AND 8192 THEN a.protocol_reference ELSE NULL END,
 CASE WHEN typeof(m.account)='text' AND length(CAST(m.account AS BLOB))<=512 THEN m.account ELSE '' END,
 CASE WHEN typeof(m.source_chat)='text' AND length(CAST(m.source_chat AS BLOB))<=512 THEN m.source_chat ELSE '' END,
 CASE WHEN typeof(m.source_sender)='text' AND length(CAST(m.source_sender AS BLOB))<=512 THEN m.source_sender ELSE '' END,
 CASE WHEN typeof(m.source_message)='text' AND length(CAST(m.source_message AS BLOB))<=512 THEN m.source_message ELSE '' END,m.from_me
 FROM extension_media_associations a JOIN messages m ON m.id=a.message_id AND m.account=a.account WHERE a.id=? AND a.account=? AND a.message_id=? AND a.local_revision=? AND a.policy_generation=? AND a.content_hash=? AND a.source_expires_at=? AND a.active=1 AND a.view_once=0`, in.Work.Lease.MediaID, in.Work.Source.Account, in.Work.Source.Version.MessageID, in.Work.Source.Version.Revision, in.Work.Source.Version.PolicyGeneration, in.Work.Source.ContentHash, unixms(in.Work.Source.Version.ExpiresAt)).Scan(&kind, &declared, &reference, &key.Account, &key.Chat, &key.Sender, &key.Message, &key.FromMe)
	defer clear(reference)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSourceUnavailable
	}
	if err != nil {
		return fixedSQLError(err)
	}
	if kind != in.Work.Kind || declared != in.Work.DeclaredBytes || key != in.Key || len(reference) == 0 || extensionDownloadSHA(reference) != r.protocol {
		return ErrSourceUnavailable
	}
	if err = tx.QueryRowContext(bounded, "SELECT rolling_retention FROM archive_policy WHERE id=1").Scan(&rolling); err != nil {
		return fixedSQLError(err)
	}
	if err = tx.Commit(); err != nil {
		return fixedSQLError(err)
	}
	finalNow := now.Add(time.Since(sample))
	if !finalNow.Before(r.work.LeaseUntil) {
		return ErrNoWork
	}
	for _, source := range current {
		if (!source.ExpiresAt.IsZero() && !finalNow.Before(source.ExpiresAt)) || (rolling > 0 && source.OriginalSentAt.Before(finalNow.Add(-time.Duration(rolling)))) {
			return ErrSourceUnavailable
		}
	}
	return bounded.Err()
}

func extensionValidDownloadLease(l OriginalDownloadLease) bool {
	return validString(string(l.MediaID), 128) && validString(string(l.LeaseID), 128)
}
func extensionDownloadRetryCode(c string) bool {
	switch c {
	case "media_unavailable", "media_source_changed", "media_canceled", "media_deadline_exceeded", "waiting_for_disk_budget", "media_cleanup_incomplete", "media_invalid_admission", "media_authentication_failed", "media_limit_exceeded":
		return true
	}
	return false
}
func (s *Store) ClaimOriginalDownload(ctx context.Context, c OriginalDownloadClaim) (OriginalDownloadWork, error) {
	if !validString(c.Account, 512) || !extensionValidTTL(c.TTL) {
		return OriginalDownloadWork{}, ErrInvalidExtension
	}
	x, err := s.submitExtension(ctx, extensionDownloadCommand{op: extensionDownloadClaim, claim: c}, 1024)
	if err != nil {
		return OriginalDownloadWork{}, err
	}
	return x.(OriginalDownloadWork), nil
}
func (s *Store) LoadOriginalDownload(ctx context.Context, l OriginalDownloadLease) (OriginalDownloadInput, error) {
	if !extensionValidDownloadLease(l) {
		return OriginalDownloadInput{}, ErrInvalidExtension
	}
	x, err := s.submitExtension(ctx, extensionDownloadCommand{op: extensionDownloadLoad, lease: l}, 512)
	if err != nil {
		return OriginalDownloadInput{}, err
	}
	return x.(OriginalDownloadInput), nil
}
func (s *Store) FinishOriginalDownload(ctx context.Context, l OriginalDownloadLease) (bool, error) {
	if !extensionValidDownloadLease(l) {
		return false, ErrInvalidExtension
	}
	x, err := s.submitExtension(ctx, extensionDownloadCommand{op: extensionDownloadFinish, lease: l}, 512)
	if err != nil {
		return false, err
	}
	return x.(bool), nil
}
func (s *Store) RetryOriginalDownload(ctx context.Context, r OriginalDownloadRetry) error {
	if !extensionValidDownloadLease(r.Lease) || !extensionDownloadRetryCode(r.Code) || r.After < 0 || r.After > 24*time.Hour {
		return ErrInvalidExtension
	}
	_, err := s.submitExtension(ctx, extensionDownloadCommand{op: extensionDownloadRetry, retry: r}, 512)
	return err
}

func extensionDownloadDigest(v string) bool {
	return extensionMediaDigest(v) && strings.ToLower(v) == v
}
func extensionDownloadSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// The descriptor identity contains only authenticated byte-intent fields.
// It intentionally excludes caption/context, MIME, source/policy generations,
// and extraction settings. Exact stored bytes get an independent lease pin.
func extensionDownloadDescriptor(kind MessageKind, reference []byte, declared int64) (string, string, bool) {
	if len(reference) == 0 || len(reference) > extensionDownloadReferenceLimit || declared < 0 {
		return "", "", false
	}
	options := proto.UnmarshalOptions{DiscardUnknown: true, RecursionLimit: 8}
	var path string
	var key, plain, encrypted []byte
	var length uint64
	switch kind {
	case MessageImage:
		var m waE2E.ImageMessage
		if options.Unmarshal(reference, &m) != nil {
			return "", "", false
		}
		path, key, plain, encrypted, length = m.GetDirectPath(), m.GetMediaKey(), m.GetFileSHA256(), m.GetFileEncSHA256(), m.GetFileLength()
	case MessageVoice:
		var m waE2E.AudioMessage
		if options.Unmarshal(reference, &m) != nil {
			return "", "", false
		}
		path, key, plain, encrypted, length = m.GetDirectPath(), m.GetMediaKey(), m.GetFileSHA256(), m.GetFileEncSHA256(), m.GetFileLength()
	default:
		return "", "", false
	}
	defer clear(key)
	defer clear(plain)
	defer clear(encrypted)
	if !strings.HasPrefix(path, "/") || len(path) > 4096 || !utf8.ValidString(path) || strings.IndexByte(path, 0) >= 0 || len(key) != 32 || len(plain) != 32 || len(encrypted) != 32 || length > math.MaxInt64 || int64(length) != declared {
		return "", "", false
	}
	h := sha256.New()
	_, _ = h.Write([]byte("wa-indexer-original-descriptor/v1\x00"))
	var size [8]byte
	for _, field := range [][]byte{[]byte(kind), []byte(path), key, plain, encrypted} {
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		_, _ = h.Write(size[:])
		_, _ = h.Write(field)
	}
	binary.BigEndian.PutUint64(size[:], length)
	_, _ = h.Write(size[:])
	return hex.EncodeToString(h.Sum(nil)), extensionDownloadSHA(reference), true
}

type extensionDownloadRow struct {
	work                                           OriginalDownloadWork
	state, protocol, descriptor, legacy, completed string
	due                                            int64
}

func extensionLoadDownloadRow(ctx context.Context, q sqlQuerier, id MediaID) (extensionDownloadRow, error) {
	var r extensionDownloadRow
	var expiry, until int64
	var integerMetadata int
	err := q.QueryRowContext(ctx, `SELECT
 CASE WHEN typeof(media_id)='text' AND length(CAST(media_id AS BLOB))<=128 THEN media_id ELSE '' END,
 CASE WHEN typeof(account)='text' AND length(CAST(account AS BLOB))<=512 THEN account ELSE '' END,
 CASE WHEN typeof(message_id)='text' AND length(CAST(message_id AS BLOB))<=128 THEN message_id ELSE '' END,
 CASE WHEN typeof(local_revision)='integer' AND local_revision>0 THEN local_revision ELSE 0 END,
 CASE WHEN typeof(policy_generation)='integer' AND policy_generation>0 THEN policy_generation ELSE 0 END,
 CASE WHEN typeof(content_hash)='text' AND length(CAST(content_hash AS BLOB))<=64 THEN content_hash ELSE '' END,
 CASE WHEN typeof(source_expires_at)='integer' THEN source_expires_at ELSE 0 END,
 CASE WHEN typeof(lease_id)='text' AND length(CAST(lease_id AS BLOB))<=128 THEN lease_id ELSE '' END,
 CASE WHEN typeof(lease_until)='integer' THEN lease_until ELSE 0 END,
 CASE WHEN typeof(attempts)='integer' AND attempts BETWEEN 0 AND 8 THEN attempts ELSE -1 END,
 CASE WHEN typeof(next_due)='integer' THEN next_due ELSE 0 END,
 CASE WHEN typeof(state)='text' AND length(CAST(state AS BLOB))<=16 THEN state ELSE '' END,
 CASE WHEN typeof(protocol_sha256)='text' AND length(CAST(protocol_sha256 AS BLOB))<=64 THEN protocol_sha256 ELSE '' END,
 CASE WHEN typeof(descriptor_sha256)='text' AND length(CAST(descriptor_sha256 AS BLOB))<=64 THEN descriptor_sha256 ELSE '' END,
 CASE WHEN typeof(legacy_source_media_identity)='text' AND length(CAST(legacy_source_media_identity AS BLOB))<=64 THEN legacy_source_media_identity ELSE '' END,
 CASE WHEN typeof(completed_sha256)='text' AND length(CAST(completed_sha256 AS BLOB))<=64 THEN completed_sha256 ELSE '' END,
 (typeof(local_revision)='integer' AND local_revision>0 AND typeof(policy_generation)='integer' AND policy_generation>0 AND typeof(source_expires_at)='integer' AND typeof(lease_until)='integer' AND typeof(attempts)='integer' AND attempts BETWEEN 0 AND 8 AND typeof(next_due)='integer')
 FROM extension_original_downloads WHERE media_id=?`, id).Scan(&r.work.Lease.MediaID, &r.work.Source.Account, &r.work.Source.Version.MessageID, &r.work.Source.Version.Revision, &r.work.Source.Version.PolicyGeneration, &r.work.Source.ContentHash, &expiry, &r.work.Lease.LeaseID, &until, &r.work.Attempts, &r.due, &r.state, &r.protocol, &r.descriptor, &r.legacy, &r.completed, &integerMetadata)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNoWork
	}
	if err != nil {
		return r, fixedSQLError(err)
	}
	r.work.Source.Version.ExpiresAt, r.work.LeaseUntil = fromMillis(expiry), fromMillis(until)
	if integerMetadata != 1 || !validString(string(r.work.Lease.MediaID), 128) || !validExtensionMediaSource(r.work.Source) || !extensionDownloadDigest(r.work.Source.ContentHash) || r.work.Attempts < 0 || r.work.Attempts > extensionMaxAttempts || !validReadTime(r.work.LeaseUntil) {
		return r, ErrSourceUnavailable
	}
	return r, nil
}

// All current authorization precedes the SQL blob read. CASE bounds the bytes
// themselves, including a corrupt legacy value, before database/sql allocates.
func (s *Store) extensionReadDownloadReference(ctx context.Context, q sqlQuerier, r extensionDownloadRow) (OriginalDownloadInput, error) {
	var in OriginalDownloadInput
	in.Work = r.work
	if _, err := s.extensionCheckSources(ctx, q, []SourceRef{r.work.Source}, true); err != nil {
		return in, err
	}
	err := q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(a.kind)='text' AND length(CAST(a.kind AS BLOB))<=5 THEN a.kind ELSE '' END,CASE WHEN typeof(a.declared_bytes)='integer' AND a.declared_bytes>=0 THEN a.declared_bytes ELSE -1 END,
 CASE WHEN typeof(a.protocol_reference)='blob' AND length(a.protocol_reference) BETWEEN 1 AND 8192 THEN a.protocol_reference ELSE NULL END,
 CASE WHEN typeof(m.account)='text' AND length(CAST(m.account AS BLOB))<=512 THEN m.account ELSE '' END,
 CASE WHEN typeof(m.source_chat)='text' AND length(CAST(m.source_chat AS BLOB))<=512 THEN m.source_chat ELSE '' END,
 CASE WHEN typeof(m.source_sender)='text' AND length(CAST(m.source_sender AS BLOB))<=512 THEN m.source_sender ELSE '' END,
 CASE WHEN typeof(m.source_message)='text' AND length(CAST(m.source_message AS BLOB))<=512 THEN m.source_message ELSE '' END,m.from_me
 FROM extension_media_associations a JOIN messages m ON m.id=a.message_id AND m.account=a.account
 WHERE a.id=? AND a.account=? AND a.message_id=? AND a.local_revision=? AND a.policy_generation=? AND a.content_hash=? AND a.source_expires_at=? AND a.active=1 AND a.view_once=0`, r.work.Lease.MediaID, r.work.Source.Account, r.work.Source.Version.MessageID, r.work.Source.Version.Revision, r.work.Source.Version.PolicyGeneration, r.work.Source.ContentHash, unixms(r.work.Source.Version.ExpiresAt)).Scan(&in.Work.Kind, &in.Work.DeclaredBytes, &in.ProtocolReference, &in.Key.Account, &in.Key.Chat, &in.Key.Sender, &in.Key.Message, &in.Key.FromMe)
	if errors.Is(err, sql.ErrNoRows) {
		return in, ErrSourceUnavailable
	}
	if err != nil {
		return in, fixedSQLError(err)
	}
	descriptor, raw, ok := extensionDownloadDescriptor(in.Work.Kind, in.ProtocolReference, in.Work.DeclaredBytes)
	if !ok || !validKey(in.Key) || in.Key.Account != r.work.Source.Account || (r.protocol != "" && r.protocol != raw) || (r.descriptor != "" && r.descriptor != descriptor) {
		clear(in.ProtocolReference)
		return OriginalDownloadInput{}, ErrSourceUnavailable
	}
	return in, nil
}

// Physical accounting belongs to MediaStore. Even a genuinely new reference
// in the same source slot waits for its older synchronous writer's cleanup.
const extensionDownloadNoPhysicalSQL = `NOT EXISTS(SELECT 1 FROM extension_media_artifacts physical LEFT JOIN extension_media_associations owner ON owner.id=physical.media_id WHERE physical.cleanup_ack=0 AND (physical.id=a.artifact_id OR physical.media_id=a.id OR (owner.account=a.account AND owner.message_id=a.message_id AND owner.ordinal=a.ordinal)))`
const extensionDownloadCurrentSQL = `a.active=1 AND a.view_once=0 AND a.kind IN ('image','voice') AND a.declared_bytes>=0 AND typeof(a.protocol_reference)='blob' AND length(a.protocol_reference) BETWEEN 1 AND 8192
 AND a.account=d.account AND a.message_id=d.message_id AND a.local_revision=d.local_revision AND a.policy_generation=d.policy_generation AND a.content_hash=d.content_hash AND a.source_expires_at=d.source_expires_at
 AND EXISTS(SELECT 1 FROM ` + authorizedTables + ` WHERE m.id=d.message_id AND m.account=d.account AND m.state='active' AND m.original_date_known=1 AND m.view_once=0 AND m.local_revision=d.local_revision AND m.content_hash=d.content_hash AND m.expires_at=d.source_expires_at AND ap.generation=d.policy_generation
 AND (m.expires_at=0 OR m.expires_at>?) AND (ap.chat_mode='all' OR EXISTS(SELECT 1 FROM policy_chats pc WHERE pc.chat_id=m.chat_id)) AND (ap.date_mode='all_available' OR m.original_sent_at>=ap.import_start) AND ` + extensionMediaRollingSQL + `)`

func (s *Store) extensionClaimOriginalDownload(ctx context.Context, tx *sql.Tx, c OriginalDownloadClaim) (OriginalDownloadWork, error) {
	if _, err := s.readSafety(c.Account); err != nil {
		return OriginalDownloadWork{}, err
	}
	now := s.now()
	// Marker evidence excludes work even after a publication reply was lost or
	// the verified original has left processed cache. No availability inference.
	_, err := tx.ExecContext(ctx, `UPDATE extension_original_downloads SET state='complete',completed_sha256=(SELECT a.last_original_sha256 FROM extension_media_associations a WHERE a.id=media_id)
 WHERE media_id IN(SELECT d.media_id FROM extension_original_downloads d JOIN extension_media_associations a ON a.id=d.media_id WHERE d.account=? AND d.state IN ('pending','leased') AND a.account=d.account AND a.message_id=d.message_id AND a.local_revision=d.local_revision AND a.policy_generation=d.policy_generation AND a.content_hash=d.content_hash AND a.source_expires_at=d.source_expires_at AND typeof(a.last_original_sha256)='text' AND length(a.last_original_sha256)=64 AND length(CAST(a.last_original_sha256 AS BLOB))=64 AND a.last_original_sha256 NOT GLOB '*[^0-9a-f]*' LIMIT 32)`, c.Account)
	if err != nil {
		return OriginalDownloadWork{}, fixedSQLError(err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE extension_original_downloads SET state='failed',lease_id='',lease_until=0,last_code='media_unavailable' WHERE media_id IN(SELECT media_id FROM extension_original_downloads WHERE account=? AND attempts>=8 AND (state='pending' OR (state='leased' AND lease_until<=?)) LIMIT 32)`, c.Account, unixms(now))
	if err != nil {
		return OriginalDownloadWork{}, fixedSQLError(err)
	}
	for pass := 0; pass < extensionDownloadBatch; pass++ {
		var id MediaID
		err = tx.QueryRowContext(ctx, `SELECT CASE WHEN typeof(d.media_id)='text' AND length(CAST(d.media_id AS BLOB))<=128 THEN d.media_id ELSE '' END FROM extension_original_downloads d JOIN extension_media_associations a ON a.id=d.media_id WHERE d.account=? AND d.attempts<8 AND d.next_due<=? AND (d.state='pending' OR (d.state='leased' AND d.lease_until<=?)) AND d.completed_sha256='' AND a.last_original_sha256='' AND `+extensionDownloadCurrentSQL+` AND `+extensionDownloadNoPhysicalSQL+` ORDER BY d.next_due,d.media_id LIMIT 1`, c.Account, unixms(now), unixms(now), unixms(now), unixms(now), extensionMediaClockRemainder(now)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return OriginalDownloadWork{}, nil
		}
		if err != nil {
			return OriginalDownloadWork{}, fixedSQLError(err)
		}
		if !validString(string(id), 128) {
			return OriginalDownloadWork{}, ErrInvalidExtension
		}
		r, e := extensionLoadDownloadRow(ctx, tx, id)
		if e != nil {
			return OriginalDownloadWork{}, e
		}
		in, e := s.extensionReadDownloadReference(ctx, tx, r)
		if e != nil {
			clear(in.ProtocolReference)
		}
		if errors.Is(e, ErrSourceUnavailable) || errors.Is(e, ErrPolicyChanged) {
			if _, err = tx.ExecContext(ctx, "UPDATE extension_original_downloads SET state='cancelled',lease_id='',lease_until=0,last_code='media_invalid_admission' WHERE media_id=?", id); err != nil {
				return OriginalDownloadWork{}, fixedSQLError(err)
			}
			continue
		}
		if e != nil {
			return OriginalDownloadWork{}, e
		}
		descriptor, raw, _ := extensionDownloadDescriptor(in.Work.Kind, in.ProtocolReference, in.Work.DeclaredBytes)
		clear(in.ProtocolReference)
		lease, e := randomID()
		if e != nil {
			return OriginalDownloadWork{}, fixedSQLError(e)
		}
		w := in.Work
		w.Lease.LeaseID = LeaseID(lease)
		w.LeaseUntil = fromMillis(unixms(now.Add(c.TTL)))
		w.Attempts++
		_, err = tx.ExecContext(ctx, "UPDATE extension_original_downloads SET state='leased',attempts=?,lease_id=?,lease_until=?,protocol_sha256=?,descriptor_sha256=? WHERE media_id=?", w.Attempts, w.Lease.LeaseID, unixms(w.LeaseUntil), raw, descriptor, id)
		if err != nil {
			return OriginalDownloadWork{}, fixedSQLError(err)
		}
		s.extensionRegisterDeadline(tx, w.LeaseUntil)
		return w, nil
	}
	return OriginalDownloadWork{}, nil
}

func (s *Store) extensionCurrentDownloadLease(ctx context.Context, q sqlQuerier, l OriginalDownloadLease) (extensionDownloadRow, error) {
	r, err := extensionLoadDownloadRow(ctx, q, l.MediaID)
	if err != nil {
		return r, err
	}
	if r.work.Lease != l || (r.state != "leased" && r.state != "complete") || !s.now().Before(r.work.LeaseUntil) {
		return r, ErrNoWork
	}
	s.extensionRegisterDeadline(q, r.work.LeaseUntil)
	return r, nil
}
func (s *Store) extensionLoadOriginalDownload(ctx context.Context, tx *sql.Tx, l OriginalDownloadLease) (OriginalDownloadInput, error) {
	r, err := s.extensionCurrentDownloadLease(ctx, tx, l)
	if err != nil {
		return OriginalDownloadInput{}, err
	}
	if r.state != "leased" || !extensionDownloadDigest(r.protocol) || !extensionDownloadDigest(r.descriptor) {
		return OriginalDownloadInput{}, ErrNoWork
	}
	return s.extensionReadDownloadReference(ctx, tx, r)
}
func (s *Store) extensionFinishOriginalDownload(ctx context.Context, tx *sql.Tx, l OriginalDownloadLease) (bool, error) {
	r, err := s.extensionCurrentDownloadLease(ctx, tx, l)
	if err != nil {
		return false, err
	}
	if !extensionDownloadDigest(r.protocol) || !extensionDownloadDigest(r.descriptor) {
		return false, ErrNoWork
	}
	in, err := s.extensionReadDownloadReference(ctx, tx, r)
	defer clear(in.ProtocolReference)
	if err != nil {
		return false, err
	}
	clear(in.ProtocolReference)
	var sha string
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN typeof(original.sha256)='text' AND length(CAST(original.sha256 AS BLOB))=64 THEN original.sha256 ELSE '' END FROM extension_media_associations a JOIN extension_media_artifacts original ON original.id=a.artifact_id WHERE a.id=? AND a.active=1 AND a.view_once=0 AND original.state='retained' AND original.budget='originals' AND original.cleanup_ack=0 AND original.sha256=a.last_original_sha256`, l.MediaID).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fixedSQLError(err)
	}
	if !extensionDownloadDigest(sha) || (r.completed != "" && r.completed != sha) {
		return false, nil
	}
	_, err = tx.ExecContext(ctx, "UPDATE extension_original_downloads SET state='complete',completed_sha256=? WHERE media_id=? AND lease_id=?", sha, l.MediaID, l.LeaseID)
	return err == nil, fixedSQLError(err)
}
func (s *Store) extensionRetryOriginalDownload(ctx context.Context, tx *sql.Tx, retry OriginalDownloadRetry) error {
	r, err := s.extensionCurrentDownloadLease(ctx, tx, retry.Lease)
	if err != nil {
		return err
	}
	in, err := s.extensionReadDownloadReference(ctx, tx, r)
	defer clear(in.ProtocolReference)
	if err != nil {
		return err
	}
	clear(in.ProtocolReference)
	if extensionDownloadDigest(r.completed) {
		return nil
	}
	var settled int
	err = tx.QueryRowContext(ctx, `SELECT `+extensionDownloadNoPhysicalSQL+` FROM extension_media_associations a WHERE a.id=?`, retry.Lease.MediaID).Scan(&settled)
	if err != nil {
		return fixedSQLError(err)
	}
	if settled != 1 {
		return extensionDownloadCleanupIncomplete
	}
	attempts := r.work.Attempts
	if retry.Code == "waiting_for_disk_budget" && attempts > 0 {
		attempts--
	}
	state := "pending"
	if retry.Terminal || attempts >= extensionMaxAttempts {
		state = "failed"
	}
	after := retry.After
	if after < time.Second {
		after = time.Second
	}
	if retry.Code == "waiting_for_disk_budget" && after < time.Minute {
		after = time.Minute
	}
	_, err = tx.ExecContext(ctx, "UPDATE extension_original_downloads SET state=?,attempts=?,next_due=?,lease_id='',lease_until=0,last_code=? WHERE media_id=? AND lease_id=?", state, attempts, unixms(s.now().Add(after)), retry.Code, retry.Lease.MediaID, retry.Lease.LeaseID)
	return fixedSQLError(err)
}

// Enqueue is called only from the existing source writer, after storing the
// association. Invalid optional protobuf never fails or quarantines a source.
func extensionEnqueueOriginalDownload(ctx context.Context, tx *sql.Tx, ref SourceRef, id MediaID, m MediaReference) error {
	descriptor, raw, ok := extensionDownloadDescriptor(MessageKind(m.Kind), m.ProtocolReference, m.DeclaredBytes)
	if !ok || m.ViewOnce || !validString(string(id), 128) || !validExtensionMediaSource(ref) || !extensionDownloadDigest(ref.ContentHash) {
		return nil
	}
	var ordinal int
	var mime, marker, legacy string
	err := tx.QueryRowContext(ctx, `SELECT a.ordinal,
 CASE WHEN typeof(a.mime_type)='text' AND length(CAST(a.mime_type AS BLOB))<=256 THEN a.mime_type ELSE '' END,
 CASE WHEN typeof(a.last_original_sha256)='text' AND length(CAST(a.last_original_sha256 AS BLOB))<=64 THEN a.last_original_sha256 ELSE '' END,
 CASE WHEN m.account=a.account AND m.id=a.message_id AND m.local_revision=a.local_revision AND m.content_hash=a.content_hash AND m.expires_at=a.source_expires_at AND m.state='active' AND m.view_once=0 AND typeof(m.extension_media_identity)='text' AND length(m.extension_media_identity)=64 AND length(CAST(m.extension_media_identity AS BLOB))=64 AND m.extension_media_identity NOT GLOB '*[^0-9a-f]*' THEN m.extension_media_identity ELSE '' END
 FROM extension_media_associations a JOIN messages m ON m.id=a.message_id AND m.account=a.account WHERE a.id=? AND a.account=? AND a.message_id=? AND a.local_revision=? AND a.policy_generation=? AND a.content_hash=? AND a.source_expires_at=? AND a.active=1 AND a.view_once=0 AND a.kind=? AND a.declared_bytes=? AND a.protocol_reference=?`, id, ref.Account, ref.Version.MessageID, ref.Version.Revision, ref.Version.PolicyGeneration, ref.ContentHash, unixms(ref.Version.ExpiresAt), m.Kind, m.DeclaredBytes, m.ProtocolReference).Scan(&ordinal, &mime, &marker, &legacy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fixedSQLError(err)
	}
	if ordinal < 0 || ordinal >= 8 {
		return nil
	}
	old, loadErr := extensionLoadDownloadRow(ctx, tx, id)
	if loadErr != nil && !errors.Is(loadErr, ErrNoWork) && !errors.Is(loadErr, ErrSourceUnavailable) {
		return loadErr
	}
	exact := loadErr == nil && old.work.Source == ref && old.protocol == raw && old.descriptor == descriptor
	completed := ""
	if exact && extensionDownloadDigest(old.completed) {
		completed = old.completed
	}
	if extensionDownloadDigest(marker) && (errors.Is(loadErr, ErrNoWork) || exact || (old.descriptor == "" && old.legacy == legacy && legacy != "")) {
		completed = marker
	}
	if completed == "" {
		err = tx.QueryRowContext(ctx, `SELECT d.completed_sha256 FROM extension_original_downloads d JOIN extension_media_associations a ON a.id=d.media_id WHERE d.account=? AND d.message_id=? AND a.ordinal=? AND a.kind=? AND typeof(d.completed_sha256)='text' AND length(d.completed_sha256)=64 AND length(CAST(d.completed_sha256 AS BLOB))=64 AND d.completed_sha256 NOT GLOB '*[^0-9a-f]*'
 AND (d.descriptor_sha256=? OR (d.descriptor_sha256='' AND ?<>'' AND d.legacy_source_media_identity=? AND a.mime_type=? AND a.declared_bytes=?)) ORDER BY d.media_id LIMIT 1`, ref.Account, ref.Version.MessageID, ordinal, m.Kind, descriptor, legacy, legacy, mime, m.DeclaredBytes).Scan(&completed)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fixedSQLError(err)
		}
	}
	// Current decodable bytes alone cannot prove which historical descriptor
	// produced an unprepared completion. Preserve that deny evidence before
	// source invalidation clears its reference; a later association must still
	// encounter the unknown completion rather than infer a new download intent.
	if completed == "" && extensionDownloadDigest(old.completed) && old.descriptor == "" {
		return nil
	}
	state, code := "pending", ""
	attempts, due := 0, int64(0)
	lease, until := LeaseID(""), int64(0)
	if exact {
		state, attempts, due, lease, until = old.state, old.work.Attempts, old.due, old.work.Lease.LeaseID, unixms(old.work.LeaseUntil)
	}
	if extensionDownloadDigest(completed) {
		state = "complete"
	} else {
		var unknown int
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM extension_original_downloads d JOIN extension_media_associations a ON a.id=d.media_id WHERE d.account=? AND d.message_id=? AND a.ordinal=? AND d.descriptor_sha256='' AND typeof(d.completed_sha256)='text' AND length(d.completed_sha256)=64 AND length(CAST(d.completed_sha256 AS BLOB))=64 AND d.completed_sha256 NOT GLOB '*[^0-9a-f]*')`, ref.Account, ref.Version.MessageID, ordinal).Scan(&unknown)
		if err != nil {
			return fixedSQLError(err)
		}
		if unknown != 0 {
			state, code, lease, until = "cancelled", "media_invalid_admission", "", 0
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO extension_original_downloads(media_id,account,message_id,local_revision,policy_generation,content_hash,source_expires_at,protocol_sha256,descriptor_sha256,legacy_source_media_identity,state,attempts,next_due,lease_id,lease_until,last_code,completed_sha256) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(media_id) DO UPDATE SET account=excluded.account,message_id=excluded.message_id,local_revision=excluded.local_revision,policy_generation=excluded.policy_generation,content_hash=excluded.content_hash,source_expires_at=excluded.source_expires_at,protocol_sha256=excluded.protocol_sha256,descriptor_sha256=excluded.descriptor_sha256,legacy_source_media_identity=excluded.legacy_source_media_identity,state=excluded.state,attempts=excluded.attempts,next_due=excluded.next_due,lease_id=excluded.lease_id,lease_until=excluded.lease_until,last_code=excluded.last_code,completed_sha256=excluded.completed_sha256`, id, ref.Account, ref.Version.MessageID, ref.Version.Revision, ref.Version.PolicyGeneration, ref.ContentHash, unixms(ref.Version.ExpiresAt), raw, descriptor, legacy, state, attempts, due, lease, until, code, completed)
	if err != nil {
		return fixedSQLError(err)
	}
	if completed != "" {
		_, err = tx.ExecContext(ctx, "UPDATE extension_media_associations SET last_original_sha256=? WHERE id=?", completed, id)
	}
	return fixedSQLError(err)
}

// Only one message's <=8 current references are prepared before invalidation.
// Global invalidation captures fixed source-set metadata entirely inside SQL.
func extensionPrepareOriginalDownloads(ctx context.Context, tx *sql.Tx, id MessageID) error {
	rows, err := tx.QueryContext(ctx, `SELECT
 CASE WHEN typeof(a.id)='text' AND length(CAST(a.id AS BLOB))<=128 THEN a.id ELSE '' END,
 CASE WHEN typeof(a.account)='text' AND length(CAST(a.account AS BLOB))<=512 THEN a.account ELSE '' END,
 CASE WHEN typeof(a.message_id)='text' AND length(CAST(a.message_id AS BLOB))<=128 THEN a.message_id ELSE '' END,
 CASE WHEN typeof(a.local_revision)='integer' AND a.local_revision>0 THEN a.local_revision ELSE 0 END,
 CASE WHEN typeof(a.policy_generation)='integer' AND a.policy_generation>0 THEN a.policy_generation ELSE 0 END,
 CASE WHEN typeof(a.content_hash)='text' AND length(CAST(a.content_hash AS BLOB))<=64 THEN a.content_hash ELSE '' END,
 CASE WHEN typeof(a.source_expires_at)='integer' THEN a.source_expires_at ELSE 0 END,
 CASE WHEN typeof(a.kind)='text' AND length(CAST(a.kind AS BLOB))<=5 THEN a.kind ELSE '' END,CASE WHEN typeof(a.declared_bytes)='integer' AND a.declared_bytes>=0 THEN a.declared_bytes ELSE -1 END,
 CASE WHEN typeof(a.mime_type)='text' AND length(CAST(a.mime_type AS BLOB))<=256 THEN a.mime_type ELSE '' END,
 CASE WHEN typeof(a.ordinal)='integer' AND a.ordinal BETWEEN 0 AND 7 AND typeof(a.protocol_reference)='blob' AND length(a.protocol_reference) BETWEEN 1 AND 8192 THEN a.protocol_reference ELSE NULL END
 FROM extension_media_associations a JOIN messages m ON m.id=a.message_id AND m.account=a.account WHERE a.message_id=? AND a.active=1 AND a.view_once=0 AND m.state='active' AND m.view_once=0 AND m.local_revision=a.local_revision AND m.content_hash=a.content_hash AND m.expires_at=a.source_expires_at ORDER BY a.ordinal LIMIT 9`, id)
	if err != nil {
		return fixedSQLError(err)
	}
	type item struct {
		id    MediaID
		ref   SourceRef
		media MediaReference
	}
	var items []item
	for rows.Next() {
		var v item
		var expiry int64
		if err = rows.Scan(&v.id, &v.ref.Account, &v.ref.Version.MessageID, &v.ref.Version.Revision, &v.ref.Version.PolicyGeneration, &v.ref.ContentHash, &expiry, &v.media.Kind, &v.media.DeclaredBytes, &v.media.MimeType, &v.media.ProtocolReference); err != nil {
			rows.Close()
			return fixedSQLError(err)
		}
		v.ref.Version.ExpiresAt = fromMillis(expiry)
		items = append(items, v)
	}
	err = rows.Err()
	rows.Close()
	defer func() {
		for _, v := range items {
			clear(v.media.ProtocolReference)
		}
	}()
	if err != nil {
		return fixedSQLError(err)
	}
	if len(items) > 8 {
		return nil
	}
	for _, v := range items {
		if err = extensionEnqueueOriginalDownload(ctx, tx, v.ref, v.id, v.media); err != nil {
			return err
		}
	}
	return nil
}
func extensionInvalidateOriginalDownloads(ctx context.Context, tx *sql.Tx, id MessageID) error {
	if err := extensionPrepareOriginalDownloads(ctx, tx, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE extension_original_downloads SET state='cancelled',lease_id='',lease_until=0,last_code='media_source_changed' WHERE message_id=?", id)
	return fixedSQLError(err)
}
func extensionInvalidateOriginalDownloadsAll(ctx context.Context, tx *sql.Tx, account string) error {
	_, err := tx.ExecContext(ctx, `UPDATE extension_original_downloads AS d SET legacy_source_media_identity=COALESCE((SELECT m.extension_media_identity FROM messages m JOIN extension_media_associations a ON a.id=d.media_id WHERE m.id=d.message_id AND m.account=d.account AND a.account=d.account AND a.message_id=d.message_id AND a.local_revision=d.local_revision AND a.content_hash=d.content_hash AND a.source_expires_at=d.source_expires_at AND m.local_revision=d.local_revision AND m.content_hash=d.content_hash AND m.expires_at=d.source_expires_at AND m.state='active' AND m.view_once=0 AND typeof(m.extension_media_identity)='text' AND length(m.extension_media_identity)=64 AND length(CAST(m.extension_media_identity AS BLOB))=64 AND m.extension_media_identity NOT GLOB '*[^0-9a-f]*'),legacy_source_media_identity),state='cancelled',lease_id='',lease_until=0,last_code='media_source_changed' WHERE (?='' OR account=?)`, account, account)
	return fixedSQLError(err)
}

// A verified publication and its completion evidence share one transaction.
// Missing/invalid optional rows never reject a valid retained original. Exact
// live lease identity is retained so Finish can acknowledge a lost reply.
func extensionCompleteOriginalDownloadPublication(ctx context.Context, tx *sql.Tx, id MediaID, sha string) error {
	if !extensionDownloadDigest(sha) {
		return nil
	}
	r, err := extensionLoadDownloadRow(ctx, tx, id)
	if errors.Is(err, ErrNoWork) || errors.Is(err, ErrSourceUnavailable) {
		return nil
	}
	if err != nil {
		return err
	}
	var kind MessageKind
	var declared int64
	var reference []byte
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN typeof(kind)='text' AND length(CAST(kind AS BLOB))<=5 THEN kind ELSE '' END,CASE WHEN typeof(declared_bytes)='integer' AND declared_bytes>=0 THEN declared_bytes ELSE -1 END,CASE WHEN typeof(protocol_reference)='blob' AND length(protocol_reference) BETWEEN 1 AND 8192 THEN protocol_reference ELSE NULL END FROM extension_media_associations WHERE id=? AND account=? AND message_id=? AND local_revision=? AND policy_generation=? AND content_hash=? AND source_expires_at=? AND active=1 AND view_once=0 AND last_original_sha256=?`, id, r.work.Source.Account, r.work.Source.Version.MessageID, r.work.Source.Version.Revision, r.work.Source.Version.PolicyGeneration, r.work.Source.ContentHash, unixms(r.work.Source.Version.ExpiresAt), sha).Scan(&kind, &declared, &reference)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fixedSQLError(err)
	}
	defer clear(reference)
	descriptor, raw, ok := extensionDownloadDescriptor(kind, reference, declared)
	if !ok || (r.protocol != "" && r.protocol != raw) || (r.descriptor != "" && r.descriptor != descriptor) {
		return nil
	}
	_, err = tx.ExecContext(ctx, "UPDATE extension_original_downloads SET state='complete',completed_sha256=?,protocol_sha256=?,descriptor_sha256=? WHERE media_id=?", sha, raw, descriptor, id)
	return fixedSQLError(err)
}
