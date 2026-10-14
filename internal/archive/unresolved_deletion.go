package archive

import (
	"context"
	"database/sql"
	"strings"
)

type unresolvedDeletionCommand struct {
	account, chat, message string
	requireUnknown         bool
}

// RecordUnresolvedDeletion retains an authoritative group deletion whose
// original author is unavailable. It withholds every matching message ID in
// that group without inventing a sender, direction, revision, or date.
func (s *Store) RecordUnresolvedDeletion(ctx context.Context, account, chat, message string) error {
	return s.submitUnresolvedDeletion(ctx, account, chat, message, false)
}

// RecordUnresolvedDeletionIfUnknown records a group deletion only when no
// durable mutation target is known. The check and negative-only guard share
// the serialized writer transaction. An exact existing guard is idempotent;
// otherwise a known target returns ErrSourceUnavailable without changing it.
func (s *Store) RecordUnresolvedDeletionIfUnknown(ctx context.Context, account, chat, message string) error {
	return s.submitUnresolvedDeletion(ctx, account, chat, message, true)
}

func (s *Store) submitUnresolvedDeletion(ctx context.Context, account, chat, message string, requireUnknown bool) error {
	if !validString(account, 512) || !validUnresolvedDeletionChat(chat) || !validString(message, 256) {
		return ErrInvalidEvent
	}
	_, err := s.submit(ctx, unresolvedDeletionCommand{account: account, chat: chat, message: message, requireUnknown: requireUnknown}, account, true, int64(len(account)+len(chat)+len(message)))
	return err
}

func validUnresolvedDeletionChat(chat string) bool {
	return validString(chat, 512) && strings.Count(chat, "@") == 1 && strings.HasSuffix(chat, "@g.us") && len(chat) > len("@g.us") && !strings.ContainsAny(chat, " \t\r\n/\\")
}

func hasUnresolvedDeletion(ctx context.Context, q sqlQuerier, account, chat, message string) (bool, error) {
	var guarded bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM unresolved_deletions WHERE account=? AND chat=? AND message=?)", account, chat, message).Scan(&guarded)
	return guarded, err
}

func (s *Store) recordUnresolvedDeletion(ctx context.Context, account, chat, message string, requireUnknown bool) error {
	if _, err := s.readSafety(account); err != nil {
		return err
	}
	invalidates := true
	return s.transactionWithSafety(ctx, &invalidates, func(tx *sql.Tx) error {
		p, err := s.loadPolicy(ctx, tx)
		if err != nil {
			return err
		}
		if p.Generation == 0 {
			return ErrInvalidPolicy
		}
		if _, err = s.readSafety(account); err != nil {
			return err
		}
		if err = bindAccount(ctx, tx, account); err != nil {
			return err
		}
		chat, err = canonicalQuery(ctx, tx, account, chat)
		if err != nil {
			return err
		}
		if !validUnresolvedDeletionChat(chat) {
			return ErrInvalidEvent
		}
		if requireUnknown {
			guarded, err := hasUnresolvedDeletion(ctx, tx, account, chat, message)
			if err != nil {
				return err
			}
			if guarded {
				// Replay only this exact canonical key. Do not widen an existing
				// guard or scrub a target learned after it was established.
				return nil
			}
			// Match MutationTargets' durable metadata sources, without filtering
			// state, sender or direction or using a separate read-pool lookup.
			query := "SELECT EXISTS(SELECT 1 FROM messages WHERE account=? AND source_chat=? AND source_message=? UNION ALL SELECT 1 FROM source_tombstones WHERE account=? AND chat=? AND message=? UNION ALL SELECT 1 FROM source_anchors WHERE account=? AND chat=? AND message=?"
			args := []any{account, chat, message, account, chat, message, account, chat, message}
			if s.rebuild != nil {
				query += " UNION ALL SELECT 1 FROM rebuild_guards g WHERE account=? AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=g.account AND alias=g.chat),g.chat)=? AND message=?"
				args = append(args, account, chat, message)
			}
			var known bool
			if err = tx.QueryRowContext(ctx, query+")", args...).Scan(&known); err != nil {
				return err
			}
			if known {
				return ErrSourceUnavailable
			}
		}
		guard, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO unresolved_deletions(account,chat,message) VALUES(?,?,?)", account, chat, message)
		if err != nil {
			return err
		}
		guardChanged, err := guard.RowsAffected()
		if err != nil {
			return err
		}
		gap, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO sync_gaps(account,chat,code) VALUES(?,?,'unresolved_deletion')", account, chat)
		if err != nil {
			return err
		}
		gapChanged, err := gap.RowsAffected()
		if err != nil {
			return err
		}
		// Chat metadata establishes the scope of the fixed gap. A zero observed
		// timestamp does not claim when the missing original was sent.
		if _, err = ensureChat(ctx, tx, SourceKey{Account: account, Chat: chat}, 0); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, "SELECT rowid,id,chat_id,original_sent_at FROM messages WHERE account=? AND source_chat=? AND source_message=? AND state<>'deleted'", account, chat, message)
		if err != nil {
			return err
		}
		type target struct {
			rowid int64
			id    MessageID
			chat  string
			sent  int64
		}
		var targets []target
		for rows.Next() {
			var target target
			if err = rows.Scan(&target.rowid, &target.id, &target.chat, &target.sent); err != nil {
				rows.Close()
				return err
			}
			targets = append(targets, target)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if guardChanged == 0 && gapChanged == 0 && len(targets) == 0 {
			return nil
		}
		rev, err := bumpRevision(ctx, tx)
		if err != nil {
			return err
		}
		for _, target := range targets {
			if err = extensionInvalidate(ctx, tx, target.id, rev); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "DELETE FROM message_fts WHERE rowid=?", target.rowid); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE messages SET state='deleted',body='',caption='',normalized_body='',normalized_caption='',content_hash='',extension_media_identity='',local_revision=local_revision+1,media_availability='not_retained',reply_account='',reply_chat='',reply_sender='',reply_message='',reply_from_me=0 WHERE id=?", target.id); err != nil {
				return err
			}
			if err = dirtyWindow(ctx, tx, target.chat, p.Generation, target.sent, rev); err != nil {
				return err
			}
		}
		return nil
	}, account)
}
