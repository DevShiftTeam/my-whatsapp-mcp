package archive

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// VoiceRedeliverySource is private source metadata for a bounded owner-requested
// copy from the primary phone. It carries no download keys or plaintext.
type VoiceRedeliverySource struct {
	Key            SourceKey
	Source         SourceRef
	OriginalSentAt time.Time
}

func extensionOwnerVoiceScope(scope ReadScope, limit, maximum int) (ReadScope, error) {
	if limit < 1 || limit > maximum {
		return ReadScope{}, ErrInvalidExtension
	}
	if _, err := extensionScopeIdentity(scope); err != nil || scope.Principal != "owner" || scope.PolicyGeneration == 0 || !validString(scope.Account, 512) || (!scope.From.IsZero() && !scope.Until.IsZero() && !scope.From.Before(scope.Until)) {
		return ReadScope{}, ErrUnauthorized
	}
	chats, err := canonicalChats(scope.Chats)
	if err != nil {
		return ReadScope{}, ErrUnauthorized
	}
	scope.Chats = chats
	return scope, nil
}

const extensionVoiceCancelledUnpublishedSQL = `a.kind='voice' AND a.view_once=0 AND m.view_once=0 AND m.body_kind='voice'
 AND d.state='cancelled' AND d.completed_sha256='' AND a.last_original_sha256='' AND a.availability<>'retained' AND a.artifact_id=''
 AND d.lease_id='' AND d.lease_until=0 AND a.account=d.account AND a.message_id=d.message_id
 AND a.local_revision=d.local_revision AND a.policy_generation=d.policy_generation AND a.content_hash=d.content_hash AND a.source_expires_at=d.source_expires_at
 AND m.local_revision=d.local_revision AND m.content_hash=d.content_hash AND m.expires_at=d.source_expires_at AND ap.generation=d.policy_generation`

// VoiceRedeliverySources selects at most eight current ordinary voice messages
// whose original was never published and whose reference was invalidated. It
// does not restore a reference or authorize a download; only authenticated SDK
// redelivery through the normal source writer may do that.
func (s *Store) VoiceRedeliverySources(ctx context.Context, scope ReadScope, limit int) ([]VoiceRedeliverySource, error) {
	scope, err := extensionOwnerVoiceScope(scope, limit, 8)
	if err != nil {
		return nil, err
	}
	f, err := s.beginRead(ctx, scope, nil, time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}
	defer f.close()
	f.captureSources = true
	args := append(append([]any(nil), f.args...), limit)
	rows, err := f.tx.QueryContext(f.ctx, `SELECT DISTINCT `+extensionSourceColumns+`,m.source_chat,m.source_sender,m.source_message,m.from_me
 FROM `+authorizedTables+` JOIN extension_media_associations a ON a.message_id=m.id AND a.account=m.account
 JOIN extension_original_downloads d ON d.media_id=a.id
 WHERE `+f.where+` AND `+extensionVoiceCancelledUnpublishedSQL+` AND d.last_code='media_source_changed'
 AND (a.active=0 OR length(a.protocol_reference)=0) AND `+extensionDownloadNoPhysicalSQL+`
 AND typeof(m.source_chat)='text' AND length(CAST(m.source_chat AS BLOB)) BETWEEN 1 AND 512
 AND typeof(m.source_sender)='text' AND length(CAST(m.source_sender AS BLOB)) BETWEEN 1 AND 512
 AND typeof(m.source_message)='text' AND length(CAST(m.source_message AS BLOB)) BETWEEN 1 AND 512
 AND typeof(m.id)='text' AND length(CAST(m.id AS BLOB)) BETWEEN 1 AND 128
 AND typeof(m.content_hash)='text' AND length(CAST(m.content_hash AS BLOB))=64
 ORDER BY m.original_sent_at DESC,m.id LIMIT ?`, args...)
	if err != nil {
		return nil, readError(f.ctx, err)
	}
	items := make([]VoiceRedeliverySource, 0, limit)
	for rows.Next() {
		var source SourceReadVersion
		var item VoiceRedeliverySource
		var sent, expiry int64
		var chatName, senderName string
		err = rows.Scan(&source.Ref.Account, &source.Ref.Version.MessageID, &source.Ref.Version.Revision, &source.Ref.ContentHash, &source.ChatID, &source.SenderID, &sent, &expiry, &chatName, &senderName, &item.Key.Chat, &item.Key.Sender, &item.Key.Message, &item.Key.FromMe)
		if err != nil {
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

type extensionVoiceRedeliveryRetryCommand struct {
	scope ReadScope
	limit int
}

func (c extensionVoiceRedeliveryRetryCommand) priority() bool  { return false }
func (c extensionVoiceRedeliveryRetryCommand) account() string { return c.scope.Account }
func (c extensionVoiceRedeliveryRetryCommand) sensitivity() extensionSensitivity {
	return extensionBackground
}
func (c extensionVoiceRedeliveryRetryCommand) run(s *Store, ctx context.Context) (any, error) {
	var result VoiceDownloadRetryResult
	err := s.extensionTransaction(ctx, c.scope.Account, func(tx *sql.Tx) error {
		var err error
		result, err = s.extensionRetryRedeliveredVoiceDownloads(ctx, tx, c.scope, c.limit)
		return err
	})
	return result, err
}

// RetryRedeliveredVoiceDownloads grants one bounded cycle to an unpublished
// cancelled download only after its current reference has been restored by an
// authenticated source event. It does not re-activate associations or change
// source revisions, hashes, completion evidence, or retained originals.
func (s *Store) RetryRedeliveredVoiceDownloads(ctx context.Context, scope ReadScope, limit int) (VoiceDownloadRetryResult, error) {
	scope, err := extensionOwnerVoiceScope(scope, limit, extensionDownloadBatch)
	if err != nil {
		return VoiceDownloadRetryResult{}, err
	}
	value, err := s.submitExtension(ctx, extensionVoiceRedeliveryRetryCommand{scope: scope, limit: limit}, 1024+int64(len(scope.Chats))*256)
	if err != nil {
		return VoiceDownloadRetryResult{}, err
	}
	return value.(VoiceDownloadRetryResult), nil
}

func (s *Store) extensionRetryRedeliveredVoiceDownloads(ctx context.Context, tx *sql.Tx, scope ReadScope, limit int) (VoiceDownloadRetryResult, error) {
	var result VoiceDownloadRetryResult
	if _, err := s.readSafety(scope.Account); err != nil {
		return result, err
	}
	var account string
	err := tx.QueryRowContext(ctx, `SELECT CASE WHEN typeof(account)='text' AND length(CAST(account AS BLOB))<=512 THEN account ELSE '' END FROM accounts LIMIT 1`).Scan(&account)
	if errors.Is(err, sql.ErrNoRows) || err == nil && account != scope.Account {
		return result, ErrUnauthorized
	}
	if err != nil {
		return result, fixedSQLError(err)
	}
	now := s.now()
	frame, err := extensionResponseFrame(s, ctx, tx, scope, now)
	if err != nil {
		return result, err
	}
	args := append(append([]any(nil), frame.args...), scope.Account, unixms(now), unixms(now), extensionMediaClockRemainder(now), limit)
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN typeof(d.media_id)='text' AND length(CAST(d.media_id AS BLOB))<=128 THEN d.media_id ELSE '' END,m.original_sent_at
 FROM `+authorizedTables+` JOIN extension_media_associations a ON a.message_id=m.id AND a.account=m.account
 JOIN extension_original_downloads d ON d.media_id=a.id
 WHERE `+frame.where+` AND d.account=? AND `+extensionDownloadCurrentSQL+` AND `+extensionDownloadNoPhysicalSQL+`
 AND `+extensionVoiceCancelledUnpublishedSQL+` AND d.last_code IN ('media_source_changed','')
 ORDER BY m.original_sent_at DESC,d.media_id LIMIT ?`, args...)
	if err != nil {
		return result, fixedSQLError(err)
	}
	type candidate struct {
		id   MediaID
		sent int64
	}
	items := make([]candidate, 0, limit)
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
	for _, item := range items {
		if !validString(string(item.id), 128) {
			result.Skipped++
			continue
		}
		record, err := extensionLoadDownloadRow(ctx, tx, item.id)
		if errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrNoWork) {
			result.Skipped++
			continue
		}
		if err != nil {
			return result, err
		}
		if !extensionDownloadDigest(record.protocol) || !extensionDownloadDigest(record.descriptor) {
			result.Skipped++
			continue
		}
		input, err := s.extensionReadDownloadReference(ctx, tx, record)
		clear(input.ProtocolReference)
		if errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged) {
			result.Skipped++
			continue
		}
		if err != nil {
			return result, err
		}
		if input.Work.Kind != MessageVoice {
			result.Skipped++
			continue
		}
		if frame.rolling > 0 {
			s.extensionRegisterDeadline(tx, fromMillis(item.sent).Add(frame.rolling))
		}
		updated, err := tx.ExecContext(ctx, `UPDATE extension_original_downloads SET state='pending',attempts=0,next_due=0,lease_id='',lease_until=0,last_code=''
 WHERE media_id=? AND state='cancelled' AND last_code IN ('media_source_changed','') AND completed_sha256='' AND lease_id='' AND lease_until=0`, item.id)
		if err != nil {
			return result, fixedSQLError(err)
		}
		count, err := updated.RowsAffected()
		if err != nil {
			return result, fixedSQLError(err)
		}
		if count == 1 {
			result.Queued++
		} else {
			result.Skipped++
		}
	}
	return result, nil
}
