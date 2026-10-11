package archive

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func documentTargetIDs(ids []MessageID) ([]MessageID, error) {
	if len(ids) < 1 || len(ids) > 8 {
		return nil, ErrInvalidExtension
	}
	result := append([]MessageID(nil), ids...)
	seen := make(map[MessageID]bool, len(ids))
	for _, id := range result {
		if !validReadID(string(id)) || seen[id] {
			return nil, ErrInvalidExtension
		}
		seen[id] = true
	}
	return result, nil
}
func documentTargetSQL(ids []MessageID) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// DocumentRedeliverySources is owner-only. A primary-phone copy remains a
// request for authenticated source metadata, never a download authorization.
func (s *Store) DocumentRedeliverySources(ctx context.Context, scope ReadScope, ids []MessageID) ([]DocumentRedeliverySource, error) {
	ids, err := documentTargetIDs(ids)
	if err != nil {
		return nil, err
	}
	scope, err = extensionOwnerVoiceScope(scope, len(ids), 8)
	if err != nil {
		return nil, err
	}
	f, err := s.beginRead(ctx, scope, nil, time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}
	defer f.close()
	f.captureSources = true
	places, targets := documentTargetSQL(ids)
	args := append(append([]any(nil), f.args...), targets...)
	rows, err := f.tx.QueryContext(f.ctx, `SELECT `+extensionSourceColumns+`,m.source_chat,m.source_sender,m.source_message,m.from_me
 FROM `+authorizedTables+` WHERE `+f.where+` AND m.id IN (`+places+`) AND m.view_once=0 AND m.edited=0 AND m.revision_barrier='' AND m.source_kind='original'
 AND ((m.body_kind='unsupported' AND m.body='' AND m.caption='' AND NOT EXISTS(SELECT 1 FROM extension_media_associations a WHERE a.message_id=m.id)
  AND EXISTS(SELECT 1 FROM extension_document_promotions p WHERE p.message_id=m.id AND p.account=m.account AND p.local_revision=m.local_revision AND p.protocol_revision=m.protocol_revision AND p.content_hash=m.content_hash AND p.media_identity=m.extension_media_identity))
 OR (m.body_kind='document' AND EXISTS(SELECT 1 FROM extension_media_associations a LEFT JOIN extension_original_downloads d ON d.media_id=a.id
  WHERE a.message_id=m.id AND a.account=m.account AND a.kind='document' AND a.local_revision=m.local_revision AND a.policy_generation=ap.generation AND a.content_hash=m.content_hash AND a.source_expires_at=m.expires_at
  AND a.view_once=0 AND a.artifact_id='' AND a.last_original_sha256='' AND a.availability<>'retained'
  AND (d.media_id IS NULL OR (d.completed_sha256='' AND d.state IN ('failed','cancelled') AND d.lease_id='' AND d.lease_until=0))
  AND `+extensionDownloadNoPhysicalSQL+`)))
 AND typeof(m.source_chat)='text' AND length(CAST(m.source_chat AS BLOB)) BETWEEN 1 AND 512
 AND typeof(m.source_sender)='text' AND length(CAST(m.source_sender AS BLOB)) BETWEEN 1 AND 512
 AND typeof(m.source_message)='text' AND length(CAST(m.source_message AS BLOB)) BETWEEN 1 AND 512
 AND typeof(m.id)='text' AND length(CAST(m.id AS BLOB)) BETWEEN 1 AND 128
 AND typeof(m.content_hash)='text' AND length(CAST(m.content_hash AS BLOB))=64
 ORDER BY m.id LIMIT 8`, args...)
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	items := make([]DocumentRedeliverySource, 0, len(ids))
	for rows.Next() {
		var source SourceReadVersion
		var item DocumentRedeliverySource
		var sent, expiry int64
		var chatName, senderName string
		if err = rows.Scan(&source.Ref.Account, &source.Ref.Version.MessageID, &source.Ref.Version.Revision, &source.Ref.ContentHash, &source.ChatID, &source.SenderID, &sent, &expiry, &chatName, &senderName, &item.Key.Chat, &item.Key.Sender, &item.Key.Message, &item.Key.FromMe); err != nil {
			rows.Close()
			return nil, readError(f.ctx, err)
		}
		item.Key.Account = scope.Account
		source.Ref.Version.PolicyGeneration = scope.PolicyGeneration
		source.OriginalSentAt, source.ExpiresAt = fromMillis(sent), fromMillis(expiry)
		source.Ref.Version.ExpiresAt = source.ExpiresAt
		if !validKey(item.Key) || !validExtensionMediaSource(source.Ref) || !validReadTime(source.OriginalSentAt) || source.OriginalSentAt.IsZero() {
			rows.Close()
			return nil, ErrSourceUnavailable
		}
		f.pin(source.Ref.Version.MessageID, source.Ref.Version.Revision)
		f.note(source.OriginalSentAt, source.ExpiresAt)
		if err = f.captureSource(source, boundedOriginal(chatName, 256), boundedOriginal(senderName, 256)); err != nil {
			rows.Close()
			return nil, err
		}
		item.Source, item.OriginalSentAt = source.Ref, source.OriginalSentAt
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	if err = f.finish(); err != nil {
		return nil, err
	}
	return items, nil
}

type documentDownloadRetryCommand struct {
	scope ReadScope
	ids   []MessageID
	grant *CredentialGrant
}

func (c documentDownloadRetryCommand) priority() bool                    { return false }
func (c documentDownloadRetryCommand) account() string                   { return c.scope.Account }
func (c documentDownloadRetryCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c documentDownloadRetryCommand) run(s *Store, ctx context.Context) (any, error) {
	var result DocumentDownloadRetry
	err := s.extensionTransaction(ctx, c.scope.Account, func(tx *sql.Tx) error {
		if c.grant != nil {
			if err := s.extensionRegisterCredential(ctx, tx, *c.grant); err != nil {
				return err
			}
		}
		var err error
		result, err = s.documentRetryDownloads(ctx, tx, c.scope, c.ids)
		return err
	})
	return result, err
}

// RetryDocumentDownloads queues only current, authorized descriptors that
// never published an original. Existing completion evidence is never reset.
// This scope-only method is for the owner. Client requests must supply their
// captured credential through RetryCredentialDocumentDownloads.
func (s *Store) RetryDocumentDownloads(ctx context.Context, scope ReadScope, ids []MessageID) (DocumentDownloadRetry, error) {
	ids, err := documentTargetIDs(ids)
	if err != nil {
		return DocumentDownloadRetry{}, err
	}
	scope, err = extensionOwnerVoiceScope(scope, len(ids), 8)
	if err != nil {
		return DocumentDownloadRetry{}, ErrUnauthorized
	}
	value, err := s.submitExtension(ctx, documentDownloadRetryCommand{scope: scope, ids: ids}, 1024+int64(len(scope.Chats))*256)
	if err != nil {
		return DocumentDownloadRetry{}, err
	}
	return value.(DocumentDownloadRetry), nil
}

// RetryCredentialDocumentDownloads queues only descriptors within the exact
// captured credential grant. It never requests new history from the phone.
func (s *Store) RetryCredentialDocumentDownloads(ctx context.Context, grant CredentialGrant, ids []MessageID) (DocumentDownloadRetry, error) {
	ids, err := documentTargetIDs(ids)
	if err != nil {
		return DocumentDownloadRetry{}, err
	}
	if !validCredentialID(grant.CredentialID) || grant.CredentialRevision == 0 {
		return DocumentDownloadRetry{}, ErrUnauthorized
	}
	if _, err = extensionScopeIdentity(grant.Scope); err != nil {
		return DocumentDownloadRetry{}, ErrUnauthorized
	}
	grant.Scope.Chats, err = canonicalChats(grant.Scope.Chats)
	if err != nil {
		return DocumentDownloadRetry{}, ErrUnauthorized
	}
	value, err := s.submitExtension(ctx, documentDownloadRetryCommand{scope: grant.Scope, ids: ids, grant: &grant}, 1024+int64(len(grant.Scope.Chats))*256)
	if err != nil {
		return DocumentDownloadRetry{}, err
	}
	return value.(DocumentDownloadRetry), nil
}
func (s *Store) documentRetryDownloads(ctx context.Context, tx *sql.Tx, scope ReadScope, ids []MessageID) (DocumentDownloadRetry, error) {
	result := DocumentDownloadRetry{Skipped: len(ids)}
	if _, err := s.readSafety(scope.Account); err != nil {
		return result, err
	}
	now := s.now()
	frame, err := extensionResponseFrame(s, ctx, tx, scope, now)
	if err != nil {
		return result, err
	}
	places, targets := documentTargetSQL(ids)
	args := append(append([]any(nil), frame.args...), targets...)
	args = append(args, unixms(now), unixms(now), extensionMediaClockRemainder(now))
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN typeof(d.media_id)='text' AND length(CAST(d.media_id AS BLOB))<=128 THEN d.media_id ELSE '' END,m.original_sent_at FROM `+authorizedTables+`
 JOIN extension_media_associations a ON a.message_id=m.id AND a.account=m.account JOIN extension_original_downloads d ON d.media_id=a.id
 WHERE `+frame.where+` AND m.id IN (`+places+`) AND m.body_kind='document' AND a.kind='document' AND a.artifact_id='' AND a.availability<>'retained'
 AND a.last_original_sha256='' AND d.completed_sha256='' AND d.state IN ('failed','cancelled') AND d.lease_id='' AND d.lease_until=0
 AND `+extensionDownloadCurrentSQL+` AND `+extensionDownloadNoPhysicalSQL+` ORDER BY m.id,a.ordinal LIMIT 8`, args...)
	if err != nil {
		return result, fixedSQLError(err)
	}
	type candidate struct {
		id   MediaID
		sent int64
	}
	items := make([]candidate, 0, 8)
	for rows.Next() {
		var item candidate
		if err = rows.Scan(&item.id, &item.sent); err != nil {
			rows.Close()
			return result, fixedSQLError(err)
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, fixedSQLError(err)
	}
	seen := make(map[MessageID]bool, len(ids))
	for _, item := range items {
		if !validReadID(string(item.id)) {
			continue
		}
		record, err := extensionLoadDownloadRow(ctx, tx, item.id)
		if errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrNoWork) {
			continue
		}
		if err != nil {
			return result, err
		}
		if !extensionDownloadDigest(record.protocol) || !extensionDownloadDigest(record.descriptor) {
			continue
		}
		input, err := s.extensionReadDownloadReference(ctx, tx, record)
		clear(input.ProtocolReference)
		if errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged) {
			continue
		}
		if err != nil {
			return result, err
		}
		if input.Work.Kind != MessageDocument || seen[record.work.Source.Version.MessageID] {
			continue
		}
		if frame.rolling > 0 {
			s.extensionRegisterDeadline(tx, fromMillis(item.sent).Add(frame.rolling))
		}
		updated, err := tx.ExecContext(ctx, `UPDATE extension_original_downloads SET state='pending',attempts=0,next_due=0,lease_id='',lease_until=0,last_code=''
 WHERE media_id=? AND state IN ('failed','cancelled') AND completed_sha256='' AND lease_id='' AND lease_until=0`, item.id)
		if err != nil {
			return result, fixedSQLError(err)
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return result, fixedSQLError(err)
		}
		if count == 1 {
			seen[record.work.Source.Version.MessageID] = true
			result.Queued++
			result.Skipped--
		}
	}
	return result, nil
}
