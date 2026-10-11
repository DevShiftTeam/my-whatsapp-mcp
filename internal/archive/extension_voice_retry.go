package archive

import (
	"context"
	"database/sql"
	"errors"
)

// VoiceDownloadRetryResult reports one bounded owner-requested batch. It never
// exposes message identities or private protocol download references.
type VoiceDownloadRetryResult struct {
	Queued  int
	Skipped int
}

type extensionVoiceRetryCommand struct {
	scope ReadScope
	limit int
}

const extensionVoiceRetryEligibleSQL = extensionDownloadCurrentSQL + ` AND ` + extensionDownloadNoPhysicalSQL + `
 AND a.kind='voice' AND d.state='failed' AND d.completed_sha256='' AND a.last_original_sha256='' AND a.availability<>'retained'
 AND d.lease_id='' AND d.lease_until=0`

func (c extensionVoiceRetryCommand) priority() bool                    { return false }
func (c extensionVoiceRetryCommand) account() string                   { return c.scope.Account }
func (c extensionVoiceRetryCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c extensionVoiceRetryCommand) run(s *Store, ctx context.Context) (any, error) {
	var result VoiceDownloadRetryResult
	err := s.extensionTransaction(ctx, c.scope.Account, func(tx *sql.Tx) error {
		var err error
		result, err = s.extensionRetryFailedVoiceDownloads(ctx, tx, c.scope, c.limit)
		return err
	})
	return result, err
}

// RetryFailedVoiceDownloads grants one additional bounded download cycle only
// after an explicit owner action. Publication markers and generated text remain
// intact; complete, evicted, cancelled and already queued downloads are not reset.
func (s *Store) RetryFailedVoiceDownloads(ctx context.Context, scope ReadScope, limit int) (VoiceDownloadRetryResult, error) {
	if limit < 1 || limit > extensionDownloadBatch {
		return VoiceDownloadRetryResult{}, ErrInvalidExtension
	}
	if _, err := extensionScopeIdentity(scope); err != nil || scope.Principal != "owner" || scope.PolicyGeneration == 0 || (!scope.From.IsZero() && !scope.Until.IsZero() && !scope.From.Before(scope.Until)) {
		return VoiceDownloadRetryResult{}, ErrUnauthorized
	}
	chats, err := canonicalChats(scope.Chats)
	if err != nil {
		return VoiceDownloadRetryResult{}, ErrUnauthorized
	}
	scope.Chats = chats
	value, err := s.submitExtension(ctx, extensionVoiceRetryCommand{scope: scope, limit: limit}, 1024+int64(len(chats))*256)
	if err != nil {
		return VoiceDownloadRetryResult{}, err
	}
	return value.(VoiceDownloadRetryResult), nil
}

func (s *Store) extensionRetryFailedVoiceDownloads(ctx context.Context, tx *sql.Tx, scope ReadScope, limit int) (VoiceDownloadRetryResult, error) {
	var result VoiceDownloadRetryResult
	if _, err := s.readSafety(scope.Account); err != nil {
		return result, err
	}
	var account string
	err := tx.QueryRowContext(ctx, "SELECT account FROM accounts LIMIT 1").Scan(&account)
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
	// Scope and archive policy precede the batch limit. Only failed voice rows
	// enter this action; pending work and retained originals remain no-ops.
	args := append(append([]any(nil), frame.args...), scope.Account, unixms(now), unixms(now), extensionMediaClockRemainder(now), limit)
	rows, err := tx.QueryContext(ctx, `SELECT CASE WHEN typeof(d.media_id)='text' AND length(CAST(d.media_id AS BLOB))<=128 THEN d.media_id ELSE '' END,m.original_sent_at
 FROM `+authorizedTables+` JOIN extension_media_associations a ON a.message_id=m.id AND a.account=m.account
 JOIN extension_original_downloads d ON d.media_id=a.id
	 WHERE `+frame.where+` AND d.account=? AND `+extensionVoiceRetryEligibleSQL+`
 ORDER BY d.media_id LIMIT ?`, args...)
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
			if err := extensionCancelFailedVoiceDownload(ctx, tx, item.id); err != nil {
				return result, err
			}
			result.Skipped++
			continue
		}
		input, err := s.extensionReadDownloadReference(ctx, tx, record)
		clear(input.ProtocolReference)
		if errors.Is(err, ErrSourceUnavailable) || errors.Is(err, ErrPolicyChanged) {
			if err := extensionCancelFailedVoiceDownload(ctx, tx, item.id); err != nil {
				return result, err
			}
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
 WHERE media_id=? AND state='failed' AND completed_sha256='' AND lease_id='' AND lease_until=0`, item.id)
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

func extensionCancelFailedVoiceDownload(ctx context.Context, tx *sql.Tx, id MediaID) error {
	// The ordinary claim path uses the same terminal classification for an
	// unusable private descriptor. This prevents repeated owner clicks from
	// getting stuck on the same malformed prefix without scanning unbounded data.
	_, err := tx.ExecContext(ctx, `UPDATE extension_original_downloads SET state='cancelled',last_code='media_invalid_admission'
 WHERE media_id=? AND state='failed' AND completed_sha256='' AND lease_id='' AND lease_until=0`, id)
	return fixedSQLError(err)
}
