package archive

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// OriginalCacheWitness is private stopped-backup evidence, not an authority to
// read media. The offline caller must hold instance, archive and media ownership
// and verify the existing original's inode, size, SHA and format before repair.
// It contains no path and cannot restore a message or derived document.
type OriginalCacheWitness struct {
	Source            SourceRef
	Key               SourceKey
	ChatID            ChatID
	MediaID           MediaID
	ArtifactID        ArtifactID
	Ordinal           int
	Kind              MessageKind
	MIMEType          string
	DeclaredBytes     int64
	ProtocolReference []byte
	MediaIdentity     string
	SHA256            string
	Bytes             int64
	Format            string
	DescriptorSHA256  string
	ProtocolSHA256    string
	LegacyIdentity    string
}

type OriginalCacheRecoveryStore interface {
	RecoverOriginalCache(context.Context, OriginalCacheWitness) (bool, error)
}

func validOriginalCacheWitness(w OriginalCacheWitness) bool {
	if !validExtensionMediaSource(w.Source) || !extensionDownloadDigest(w.Source.ContentHash) || !validKey(w.Key) || w.Key.Account != w.Source.Account || !validReadID(string(w.ChatID)) || !validReadID(string(w.MediaID)) || !validReadID(string(w.ArtifactID)) || w.Ordinal != 0 || !extensionDownloadDigest(w.MediaIdentity) || !extensionDownloadDigest(w.SHA256) || w.Bytes <= 0 || w.DeclaredBytes != w.Bytes || len(w.MIMEType) > 256 || !utf8.ValidString(w.MIMEType) || strings.ContainsRune(w.MIMEType, 0) {
		return false
	}
	if (w.Kind == MessageImage && w.Format != "png" && w.Format != "jpeg" && w.Format != "webp") || (w.Kind == MessageVoice && (!extensionMediaFormat(w.Format) || w.Format == "png" || w.Format == "jpeg" || w.Format == "webp")) || (w.Kind != MessageImage && w.Kind != MessageVoice) {
		return false
	}
	identity, err := sourceMediaIdentity([]MediaReference{{Kind: string(w.Kind), MimeType: w.MIMEType, ProtocolReference: w.ProtocolReference}})
	if err != nil || identity != w.MediaIdentity {
		return false
	}
	descriptor, raw, valid := extensionDownloadDescriptor(w.Kind, w.ProtocolReference, w.DeclaredBytes)
	if !valid || (w.ProtocolSHA256 != "" && w.ProtocolSHA256 != raw) || (w.DescriptorSHA256 != "" && w.DescriptorSHA256 != descriptor) || (w.LegacyIdentity != "" && w.LegacyIdentity != identity) {
		return false
	}
	if w.DescriptorSHA256 == "" || w.ProtocolSHA256 == "" {
		if w.DescriptorSHA256 != "" || w.ProtocolSHA256 != "" || w.LegacyIdentity != identity {
			return false
		}
	}
	// The existing plaintext completion must also agree with the authenticated
	// descriptor. A matching cache hash alone cannot bind it to this recording.
	options := proto.UnmarshalOptions{DiscardUnknown: true, RecursionLimit: 8}
	var plain []byte
	if w.Kind == MessageVoice {
		var m waE2E.AudioMessage
		if options.Unmarshal(w.ProtocolReference, &m) != nil {
			return false
		}
		plain = m.GetFileSHA256()
	} else {
		var m waE2E.ImageMessage
		if options.Unmarshal(w.ProtocolReference, &m) != nil {
			return false
		}
		plain = m.GetFileSHA256()
	}
	defer clear(plain)
	return hex.EncodeToString(plain) == w.SHA256
}

type originalCacheRecoveryCommand struct{ witness OriginalCacheWitness }

func (c originalCacheRecoveryCommand) priority() bool                    { return false }
func (c originalCacheRecoveryCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c originalCacheRecoveryCommand) account() string                   { return c.witness.Source.Account }
func (c originalCacheRecoveryCommand) run(s *Store, ctx context.Context) (any, error) {
	defer clear(c.witness.ProtocolReference)
	var repaired bool
	err := s.extensionTransaction(ctx, c.account(), func(tx *sql.Tx) error {
		var err error
		repaired, err = s.extensionRecoverOriginalCache(ctx, tx, c.witness)
		return err
	})
	if errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged) || errors.Is(err, ErrNoWork) {
		return false, nil
	}
	return repaired, err
}

// RecoverOriginalCache repairs only a still-charged original whose exact source
// authority and durable completed identity survive. No download completion,
// extraction receipt, policy, alias, tombstone or credential is changed.
func (s *Store) RecoverOriginalCache(ctx context.Context, w OriginalCacheWitness) (bool, error) {
	w.ProtocolReference = bytes.Clone(w.ProtocolReference)
	if !validOriginalCacheWitness(w) {
		clear(w.ProtocolReference)
		return false, nil
	}
	value, err := s.submitExtension(ctx, originalCacheRecoveryCommand{witness: w}, int64(2048+len(w.ProtocolReference)))
	if err != nil {
		return false, err
	}
	return value.(bool), nil
}

func originalCacheSourceMatches(ctx context.Context, q sqlQuerier, w OriginalCacheWitness) (bool, error) {
	var key SourceKey
	var chat ChatID
	var identity, barrier, chatAccount, chatSource, senderAccount, senderSource string
	var sent int64
	err := q.QueryRowContext(ctx, `SELECT m.account,m.source_chat,m.source_sender,m.source_message,m.from_me,m.chat_id,m.extension_media_identity,m.revision_barrier,m.original_sent_at,c.account,c.source_chat,i.account,i.source_identity
 FROM messages m JOIN chats c ON c.id=m.chat_id JOIN identities i ON i.id=m.sender_id
 WHERE m.id=? AND m.account=? AND m.local_revision=? AND m.content_hash=? AND m.expires_at=?
 AND typeof(m.source_chat)='text' AND length(CAST(m.source_chat AS BLOB))<=512 AND typeof(m.source_sender)='text' AND length(CAST(m.source_sender AS BLOB))<=512
 AND typeof(m.source_message)='text' AND length(CAST(m.source_message AS BLOB))<=512 AND typeof(m.from_me)='integer' AND m.from_me IN (0,1)
 AND typeof(m.chat_id)='text' AND length(CAST(m.chat_id AS BLOB))<=128 AND typeof(m.extension_media_identity)='text' AND length(CAST(m.extension_media_identity AS BLOB))<=64
 AND typeof(m.revision_barrier)='text' AND length(CAST(m.revision_barrier AS BLOB))<=32 AND typeof(m.original_sent_at)='integer'
 AND length(CAST(c.account AS BLOB))<=512 AND length(CAST(c.source_chat AS BLOB))<=512 AND length(CAST(i.account AS BLOB))<=512 AND length(CAST(i.source_identity AS BLOB))<=512`, w.Source.Version.MessageID, w.Source.Account, w.Source.Version.Revision, w.Source.ContentHash, unixms(w.Source.Version.ExpiresAt)).Scan(&key.Account, &key.Chat, &key.Sender, &key.Message, &key.FromMe, &chat, &identity, &barrier, &sent, &chatAccount, &chatSource, &senderAccount, &senderSource)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fixedSQLError(err)
	}
	if key != w.Key || chat != w.ChatID || identity != w.MediaIdentity || barrier != "" || chatAccount != w.Source.Account || senderAccount != w.Source.Account || chatSource != key.Chat || senderSource != key.Sender {
		return false, nil
	}
	canonical, err := canonicalKey(ctx, q, key)
	if err != nil {
		return false, fixedSQLError(err)
	}
	if canonical != key {
		return false, nil
	}
	var denied bool
	err = q.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM source_tombstones WHERE account=? AND chat=? AND sender=? AND message=? AND from_me=?)
 OR EXISTS(SELECT 1 FROM chat_tombstones WHERE account=? AND chat=? AND cutoff>=?)
 OR EXISTS(SELECT 1 FROM sync_gaps WHERE account=? AND code='identity_alias_conflict')
 OR EXISTS(SELECT 1 FROM chat_aliases WHERE account=? AND alias=? AND canonical<>?)`, key.Account, key.Chat, key.Sender, key.Message, key.FromMe, key.Account, key.Chat, sent, key.Account, key.Account, key.Chat, key.Chat).Scan(&denied)
	if err != nil {
		return false, fixedSQLError(err)
	}
	if denied {
		return false, nil
	}
	denied, err = hasUnresolvedDeletion(ctx, q, key.Account, key.Chat, key.Message)
	return !denied, fixedSQLError(err)
}

func (s *Store) extensionRecoverOriginalCache(ctx context.Context, tx *sql.Tx, w OriginalCacheWitness) (bool, error) {
	if _, err := s.extensionCheckSources(ctx, tx, []SourceRef{w.Source}, true); err != nil {
		return false, err
	}
	valid, err := originalCacheSourceMatches(ctx, tx, w)
	if err != nil || !valid {
		return false, err
	}
	var active int
	var reference []byte
	var artifact ArtifactID
	var availability, marker string
	err = tx.QueryRowContext(ctx, `SELECT active,CASE WHEN typeof(protocol_reference)='blob' AND length(protocol_reference)<=8192 THEN protocol_reference ELSE NULL END,artifact_id,availability,last_original_sha256
 FROM extension_media_associations WHERE id=? AND account=? AND message_id=? AND local_revision=? AND policy_generation=? AND content_hash=? AND source_expires_at=?
 AND ordinal=? AND kind=? AND mime_type=? AND declared_bytes=? AND view_once=0
 AND typeof(protocol_reference)='blob' AND length(protocol_reference)<=8192
 AND typeof(active)='integer' AND active IN(0,1) AND typeof(artifact_id)='text' AND length(CAST(artifact_id AS BLOB))<=128
 AND typeof(availability)='text' AND length(CAST(availability AS BLOB))<=32 AND typeof(last_original_sha256)='text' AND length(CAST(last_original_sha256 AS BLOB))=64`, w.MediaID, w.Source.Account, w.Source.Version.MessageID, w.Source.Version.Revision, w.Source.Version.PolicyGeneration, w.Source.ContentHash, unixms(w.Source.Version.ExpiresAt), w.Ordinal, w.Kind, w.MIMEType, w.DeclaredBytes).Scan(&active, &reference, &artifact, &availability, &marker)
	defer clear(reference)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fixedSQLError(err)
	}
	if marker != w.SHA256 {
		return false, nil
	}
	var associations int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM extension_media_associations WHERE message_id=? AND local_revision=?`, w.Source.Version.MessageID, w.Source.Version.Revision).Scan(&associations); err != nil {
		return false, fixedSQLError(err)
	}
	if associations != 1 {
		return false, nil
	}
	idempotent := active == 1 && bytes.Equal(reference, w.ProtocolReference) && artifact == w.ArtifactID && availability == "retained"
	if !idempotent && (artifact != "" || (active == 0 && (len(reference) != 0 || (availability != "not_retained" && availability != "retained"))) || (active == 1 && (availability != "not_retained" || !bytes.Equal(reference, w.ProtocolReference)))) {
		return false, nil
	}
	download, err := extensionLoadDownloadRow(ctx, tx, w.MediaID)
	if errors.Is(err, ErrNoWork) || errors.Is(err, ErrSourceUnavailable) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if download.work.Source != w.Source || download.completed != w.SHA256 || (download.state != "complete" && download.state != "cancelled") || download.work.Lease.LeaseID != "" || !download.work.LeaseUntil.IsZero() || download.protocol != w.ProtocolSHA256 || download.descriptor != w.DescriptorSHA256 || download.legacy != w.LegacyIdentity {
		return false, nil
	}
	var state string
	var matching int
	err = tx.QueryRowContext(ctx, `SELECT state FROM extension_media_artifacts WHERE id=? AND media_id=? AND account=? AND message_id=? AND local_revision=? AND policy_generation=? AND content_hash=? AND source_expires_at=?
 AND budget='originals' AND cleanup_ack=0 AND sha256=? AND bytes=? AND actual_format=? AND charged_bytes=? AND maximum_bytes>=bytes
 AND typeof(bytes)='integer' AND typeof(charged_bytes)='integer' AND typeof(maximum_bytes)='integer' AND state IN ('evicting','retained')`, w.ArtifactID, w.MediaID, w.Source.Account, w.Source.Version.MessageID, w.Source.Version.Revision, w.Source.Version.PolicyGeneration, w.Source.ContentHash, unixms(w.Source.Version.ExpiresAt), w.SHA256, w.Bytes, w.Format, w.Bytes).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fixedSQLError(err)
	}
	if (idempotent && state != "retained") || (!idempotent && state != "evicting") {
		return false, nil
	}
	err = tx.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM extension_extraction_jobs WHERE (artifact_id=? OR artifact_id=CAST(? AS BLOB) OR media_id=?) AND input_pinned=1)
 +(SELECT count(*) FROM extension_media_artifacts WHERE cleanup_ack=0 AND budget='decoded_work' AND (input_artifact_id=? OR input_artifact_id=CAST(? AS BLOB) OR media_id=?))
 +(SELECT count(*) FROM extension_media_artifacts WHERE cleanup_ack=0 AND budget='originals' AND id<>? AND media_id=?)
 +(SELECT count(*) FROM extension_media_associations WHERE artifact_id=? AND id<>?)`, w.ArtifactID, w.ArtifactID, w.MediaID, w.ArtifactID, w.ArtifactID, w.MediaID, w.ArtifactID, w.MediaID, w.ArtifactID, w.MediaID).Scan(&matching)
	if err != nil {
		return false, fixedSQLError(err)
	}
	if matching != 0 {
		return false, nil
	}
	if idempotent {
		return true, nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE extension_media_associations SET active=1,protocol_reference=?,artifact_id=?,availability='retained' WHERE id=?`, w.ProtocolReference, w.ArtifactID, w.MediaID); err != nil {
		return false, fixedSQLError(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE extension_media_artifacts SET state='retained',updated_at=? WHERE id=?`, unixms(s.now()), w.ArtifactID); err != nil {
		return false, fixedSQLError(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET media_availability='retained' WHERE id=?`, w.Source.Version.MessageID); err != nil {
		return false, fixedSQLError(err)
	}
	if _, err = bumpRevision(ctx, tx); err != nil {
		return false, fixedSQLError(err)
	}
	return true, nil
}

const originalCacheWitnessLimit = 256

// OpenOriginalCacheRecovery requires the caller's ordinary application instance
// lease. It qualifies an existing current schema without migration, keeps the
// normal archive lock/writer/commit fences, and defers only physical expiry.
// Ordinary launches continue expiry cleanup; current-source checks never defer.
func OpenOriginalCacheRecovery(ctx context.Context, privateArchiveDir string) (*Store, error) {
	path, err := originalCacheExistingArchive(privateArchiveDir, true)
	if err != nil {
		return nil, err
	}
	// Refuse filesystem-only safety evidence before opening a writable SQLite
	// connection: closing that connection could checkpoint a nonempty WAL.
	for _, name := range []string{"observation.marker", "unsafe.marker"} {
		if _, err := os.Lstat(filepath.Join(privateArchiveDir, name)); err == nil {
			return nil, ErrReconciliationRequired
		} else if !os.IsNotExist(err) {
			return nil, ErrUnsafeDataPath
		}
	}
	// Normal startup preallocates this marker before issuing credentials. Its
	// existing inode keeps credential initialization read-only in this factory.
	fence := filepath.Join(privateArchiveDir, credentialFenceMarker)
	if err := checkPrivateFile(fence); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(fence); err != nil {
		return nil, ErrUnsafeDataPath
	}
	if err := checkPrivateFile(fence + ".pending"); err != nil {
		return nil, err
	}
	if err = validateOriginalCacheRecoverySchema(ctx, path); err != nil {
		return nil, err
	}
	s, err := Open(ctx, privateArchiveDir, Options{originalCacheRecovery: true})
	if err != nil {
		return nil, err
	}
	account, err := s.BoundAccount(ctx)
	if err == nil && (!validString(account, 512) || account == "demo") {
		err = ErrUnauthorized
	}
	if err == nil {
		_, err = s.readSafety(account)
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func validateOriginalCacheRecoverySchema(ctx context.Context, path string) error {
	return validateOriginalCacheRecoverySchemaMode(ctx, path, false)
}

func validateOriginalCacheRecoverySchemaMode(ctx context.Context, path string, historical bool) error {
	base := url.URL{Scheme: "file", Path: path}
	base.RawQuery = url.Values{"mode": {"ro"}, "_busy_timeout": {"500"}, "_query_only": {"on"}}.Encode()
	db, err := sql.Open(queryOnlyDriverName, base.String())
	if err != nil {
		return fixedSQLError(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fixedSQLError(err)
	}
	defer tx.Rollback()
	var mode string
	if err = tx.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		return ErrArchiveUnavailable
	}
	if historical {
		if _, err = validateCompatibleSnapshotMigrations(ctx, tx); err != nil {
			return err
		}
	} else if err = validateOriginalCacheMigrationHistory(ctx, tx, true); err != nil {
		return err
	}
	var integrity string
	if err = tx.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&integrity); err != nil || integrity != "ok" {
		return ErrArchiveUnavailable
	}
	foreign, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fixedSQLError(err)
	}
	violations := foreign.Next()
	err = foreign.Err()
	foreign.Close()
	if err != nil || violations {
		return ErrArchiveUnavailable
	}
	var normalization string
	var prepared, unsafe, accounts int
	var account string
	if err = tx.QueryRowContext(ctx, `SELECT CASE WHEN typeof(normalization_version)='text' AND length(CAST(normalization_version AS BLOB))<=256 THEN normalization_version ELSE '' END,(SELECT count(*) FROM snapshot_recovery_audit WHERE phase='prepared'),(SELECT count(*) FROM account_safety WHERE unsafe<>0),(SELECT count(*) FROM accounts),COALESCE((SELECT CASE WHEN typeof(account)='text' AND length(CAST(account AS BLOB))<=512 THEN account ELSE '' END FROM accounts LIMIT 1),'') FROM archive_meta WHERE id=1 AND length(cursor_key)=32`).Scan(&normalization, &prepared, &unsafe, &accounts, &account); err != nil {
		return fixedSQLError(err)
	}
	if normalization != LexicalNormalizationVersion {
		return ErrSchemaChanged
	}
	if prepared != 0 || unsafe != 0 {
		return ErrReconciliationRequired
	}
	if accounts != 1 || !validString(account, 512) || account == "demo" {
		return ErrUnauthorized
	}
	return fixedSQLError(tx.Commit())
}

// Current recovery must not cause migration. Backup reads accept only a known,
// checksummed prefix from version 10 onward, whose original-cache evidence is
// compatible with the reader. Both paths reject gaps and future versions.
func validateOriginalCacheMigrationHistory(ctx context.Context, tx *sql.Tx, requireCurrent bool) error {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil || len(entries) < 11 {
		return ErrSchemaChanged
	}
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN typeof(version)='integer' THEN version ELSE 0 END,CASE WHEN typeof(checksum)='text' AND length(CAST(checksum AS BLOB))=64 THEN checksum ELSE '' END FROM schema_migrations ORDER BY version LIMIT ?`, len(entries)+1)
	if err != nil {
		return fixedSQLError(err)
	}
	defer rows.Close()
	applied := 0
	for rows.Next() {
		var version int
		var checksum string
		if err = rows.Scan(&version, &checksum); err != nil {
			return fixedSQLError(err)
		}
		if version > len(entries) {
			return ErrSchemaTooNew
		}
		if version != applied+1 || applied >= len(entries) {
			return ErrSchemaChanged
		}
		entry := entries[applied]
		number, numberErr := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		contents, readErr := migrationFiles.ReadFile("migrations/" + entry.Name())
		if entry.IsDir() || numberErr != nil || number != version || readErr != nil || extensionDownloadSHA(contents) != checksum {
			return ErrSchemaChanged
		}
		applied++
	}
	if err = rows.Err(); err != nil {
		return fixedSQLError(err)
	}
	if applied < 10 || (requireCurrent && applied != len(entries)) {
		return ErrSchemaChanged
	}
	return nil
}

func originalCacheExistingArchive(privateArchiveDir string, allowWAL bool) (string, error) {
	if !filepath.IsAbs(privateArchiveDir) || filepath.Clean(privateArchiveDir) != privateArchiveDir {
		return "", ErrUnsafeDataPath
	}
	for part := privateArchiveDir; ; part = filepath.Dir(part) {
		info, err := os.Lstat(part)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || (part == privateArchiveDir && info.Mode().Perm()&0077 != 0) {
			return "", ErrUnsafeDataPath
		}
		if filepath.Dir(part) == part {
			break
		}
	}
	path := filepath.Join(privateArchiveDir, "archive.sqlite")
	if err := checkPrivateFile(path); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Size() < 100 {
		return "", ErrUnsafeDataPath
	}
	f, err := os.Open(path)
	if err != nil {
		return "", ErrUnsafeDataPath
	}
	var header [16]byte
	_, headerErr := io.ReadFull(f, header[:])
	held, statErr := f.Stat()
	closeErr := f.Close()
	if headerErr != nil || statErr != nil || closeErr != nil || !os.SameFile(info, held) || string(header[:]) != "SQLite format 3\x00" {
		return "", ErrUnsafeDataPath
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err = checkPrivateFile(path + suffix); err != nil {
			return "", err
		}
		if suffix == "-wal" && !allowWAL {
			wal, e := os.Lstat(path + suffix)
			if e != nil && !os.IsNotExist(e) {
				return "", ErrUnsafeDataPath
			}
			if e == nil && wal.Size() != 0 {
				return "", ErrUnsafeDataPath
			}
		}
	}
	return path, nil
}

// ReadOriginalCacheWitnesses reads an already stopped, checkpointed backup.
// The caller owns its instance lease. It never opens a Store, migrates, changes
// permissions, or allows SQLite to create sidecars. Nonempty WAL is rejected
// rather than ignored by immutable mode. Known schemas from version 10 through
// the current embedded version are checksum-qualified without migration.
func ReadOriginalCacheWitnesses(ctx context.Context, privateArchiveDir string) ([]OriginalCacheWitness, error) {
	path, err := originalCacheExistingArchive(privateArchiveDir, false)
	if err != nil {
		return nil, err
	}
	base := url.URL{Scheme: "file", Path: path}
	base.RawQuery = url.Values{"mode": {"ro"}, "immutable": {"1"}, "_busy_timeout": {"500"}, "_query_only": {"on"}}.Encode()
	db, err := sql.Open(queryOnlyDriverName, base.String())
	if err != nil {
		return nil, fixedSQLError(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fixedSQLError(err)
	}
	defer tx.Rollback()
	if err = validateOriginalCacheMigrationHistory(ctx, tx, false); err != nil {
		return nil, err
	}
	var account string
	var accounts int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(MIN(CASE WHEN typeof(account)='text' AND length(CAST(account AS BLOB))<=512 THEN account ELSE '' END),'') FROM accounts`).Scan(&accounts, &account); err != nil {
		return nil, fixedSQLError(err)
	}
	if accounts != 1 || !validString(account, 512) || account == "demo" {
		return nil, ErrUnauthorized
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.account,m.id,m.local_revision,a.policy_generation,m.content_hash,m.expires_at,m.source_chat,m.source_sender,m.source_message,m.from_me,m.chat_id,m.extension_media_identity,
 a.id,o.id,a.ordinal,a.kind,CASE WHEN typeof(a.mime_type)='text' AND length(CAST(a.mime_type AS BLOB))<=256 THEN a.mime_type ELSE '' END,a.declared_bytes,
 CASE WHEN typeof(a.protocol_reference)='blob' AND length(a.protocol_reference) BETWEEN 1 AND 8192 THEN a.protocol_reference ELSE NULL END,
 o.sha256,o.bytes,o.actual_format,d.descriptor_sha256,d.protocol_sha256,d.legacy_source_media_identity
 FROM extension_media_associations a JOIN messages m ON m.id=a.message_id AND m.account=a.account JOIN extension_media_artifacts o ON o.id=a.artifact_id JOIN extension_original_downloads d ON d.media_id=a.id
 WHERE m.account=? AND m.state='active' AND m.original_date_known=1 AND m.view_once=0 AND a.active=1 AND a.view_once=0 AND a.availability='retained'
 AND a.local_revision=m.local_revision AND a.content_hash=m.content_hash AND a.source_expires_at=m.expires_at
 AND o.account=a.account AND o.message_id=a.message_id AND o.local_revision=a.local_revision AND o.policy_generation=a.policy_generation AND o.content_hash=a.content_hash AND o.source_expires_at=a.source_expires_at AND o.media_id=a.id
 AND o.budget='originals' AND o.state='retained' AND o.cleanup_ack=0 AND o.bytes>0 AND o.charged_bytes=o.bytes AND o.maximum_bytes>=o.bytes
 AND d.account=a.account AND d.message_id=a.message_id AND d.local_revision=a.local_revision AND d.policy_generation=a.policy_generation AND d.content_hash=a.content_hash AND d.source_expires_at=a.source_expires_at
 AND d.state IN ('complete','cancelled') AND d.completed_sha256=o.sha256 AND a.last_original_sha256=o.sha256
 AND typeof(m.id)='text' AND length(CAST(m.id AS BLOB))<=128 AND typeof(m.local_revision)='integer' AND m.local_revision>0 AND typeof(a.policy_generation)='integer' AND a.policy_generation>0
 AND typeof(m.content_hash)='text' AND length(CAST(m.content_hash AS BLOB))=64 AND typeof(m.expires_at)='integer'
 AND typeof(m.source_chat)='text' AND length(CAST(m.source_chat AS BLOB))<=512 AND typeof(m.source_sender)='text' AND length(CAST(m.source_sender AS BLOB))<=512 AND typeof(m.source_message)='text' AND length(CAST(m.source_message AS BLOB))<=512
 AND typeof(m.from_me)='integer' AND m.from_me IN(0,1) AND typeof(m.chat_id)='text' AND length(CAST(m.chat_id AS BLOB))<=128 AND typeof(m.extension_media_identity)='text' AND length(CAST(m.extension_media_identity AS BLOB))=64
 AND typeof(a.id)='text' AND length(CAST(a.id AS BLOB))<=128 AND typeof(o.id)='text' AND length(CAST(o.id AS BLOB))<=128 AND typeof(a.ordinal)='integer' AND a.ordinal=0 AND a.kind IN ('voice','image') AND typeof(a.declared_bytes)='integer'
 AND typeof(o.sha256)='text' AND length(CAST(o.sha256 AS BLOB))=64 AND typeof(o.bytes)='integer' AND typeof(o.actual_format)='text' AND length(CAST(o.actual_format AS BLOB))<=8
 AND typeof(d.descriptor_sha256)='text' AND length(CAST(d.descriptor_sha256 AS BLOB))<=64 AND typeof(d.protocol_sha256)='text' AND length(CAST(d.protocol_sha256 AS BLOB))<=64 AND typeof(d.legacy_source_media_identity)='text' AND length(CAST(d.legacy_source_media_identity AS BLOB))<=64
 AND (SELECT count(*) FROM extension_media_associations member WHERE member.message_id=a.message_id AND member.local_revision=a.local_revision)=1
 ORDER BY o.id LIMIT ?`, account, originalCacheWitnessLimit+1)
	if err != nil {
		return nil, fixedSQLError(err)
	}
	defer rows.Close()
	var witnesses []OriginalCacheWitness
	for rows.Next() {
		var w OriginalCacheWitness
		var expiry int64
		if err = rows.Scan(&w.Source.Account, &w.Source.Version.MessageID, &w.Source.Version.Revision, &w.Source.Version.PolicyGeneration, &w.Source.ContentHash, &expiry, &w.Key.Chat, &w.Key.Sender, &w.Key.Message, &w.Key.FromMe, &w.ChatID, &w.MediaIdentity, &w.MediaID, &w.ArtifactID, &w.Ordinal, &w.Kind, &w.MIMEType, &w.DeclaredBytes, &w.ProtocolReference, &w.SHA256, &w.Bytes, &w.Format, &w.DescriptorSHA256, &w.ProtocolSHA256, &w.LegacyIdentity); err != nil {
			return nil, fixedSQLError(err)
		}
		w.Key.Account, w.Source.Version.ExpiresAt = w.Source.Account, fromMillis(expiry)
		if !validOriginalCacheWitness(w) {
			clear(w.ProtocolReference)
			return nil, ErrSourceUnavailable
		}
		witnesses = append(witnesses, w)
		if len(witnesses) > originalCacheWitnessLimit {
			return nil, ErrLimitExceeded
		}
	}
	if err = rows.Err(); err != nil {
		return nil, fixedSQLError(err)
	}
	if err = rows.Close(); err != nil {
		return nil, fixedSQLError(err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fixedSQLError(err)
	}
	return witnesses, nil
}
