package archive

import (
	"context"
	"database/sql"
	"time"
)

type rebuildRestrictionCommand struct {
	key      SourceKey
	expires  time.Time
	viewOnce bool
}

func (c rebuildRestrictionCommand) priority() bool                    { return true }
func (c rebuildRestrictionCommand) account() string                   { return c.key.Account }
func (c rebuildRestrictionCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c rebuildRestrictionCommand) run(s *Store, ctx context.Context) (any, error) {
	invalidates := true
	return nil, s.transactionWithSafety(ctx, &invalidates, func(tx *sql.Tx) error {
		if _, err := s.readSafety(c.key.Account); err != nil {
			return err
		}
		key, err := canonicalKey(ctx, tx, c.key)
		if err != nil {
			return err
		}
		p, err := s.loadPolicy(ctx, tx)
		if err != nil {
			return err
		}
		return s.rebuildRestrict(ctx, tx, key, c.expires, c.viewOnce, p.Generation)
	}, c.key.Account)
}

// RecordRebuildRestriction records only authenticated negative scalar facts.
// It cannot introduce a body, revision witness, date or active message row.
// Admission/commit uncertainty has the same source fence as other controls.
func (s *Store) RecordRebuildRestriction(ctx context.Context, key SourceKey, expires time.Time, viewOnce bool) error {
	if s.rebuild == nil || !validKey(key) || key.Account != s.rebuild.Account || !validReadTime(expires) || (!expires.IsZero() && unixms(expires) <= 0) {
		return ErrInvalidEvent
	}
	if expires.IsZero() && !viewOnce {
		return nil
	}
	_, err := s.submitExtension(ctx, rebuildRestrictionCommand{key: key, expires: expires, viewOnce: viewOnce}, 1024)
	return err
}

func (s *Store) rebuildRestrict(ctx context.Context, tx *sql.Tx, key SourceKey, expires time.Time, viewOnce bool, policy Generation) error {
	if !validReadTime(expires) || (!expires.IsZero() && unixms(expires) <= 0) {
		return ErrInvalidEvent
	}
	if expires.IsZero() && !viewOnce {
		return nil
	}
	g, err := rebuildGuardQuery(ctx, tx, key)
	if err != nil {
		return err
	}
	tighterExpiry := !expires.IsZero() && (g.ExpiresAt.IsZero() || expires.Before(g.ExpiresAt))
	if !tighterExpiry && (!viewOnce || g.ViewOnce) {
		return nil
	}
	// Defaults are unknown metadata; the conflict path only tightens restrictions.
	// Alias collisions stay separate and lookup combines their strictest facts.
	_, err = tx.ExecContext(ctx, `INSERT INTO rebuild_guards(account,chat,sender,message,from_me,original_sent_at,date_known,expires_at,view_once,protocol_revision,revision_barrier,source_kind,deleted) VALUES(?,?,?,?,?,0,0,?,?,0,'','original',0) ON CONFLICT(account,chat,sender,message,from_me) DO UPDATE SET expires_at=CASE WHEN excluded.expires_at=0 THEN rebuild_guards.expires_at WHEN rebuild_guards.expires_at=0 THEN excluded.expires_at ELSE MIN(rebuild_guards.expires_at,excluded.expires_at) END,view_once=MAX(rebuild_guards.view_once,excluded.view_once)`, key.Account, key.Chat, key.Sender, key.Message, key.FromMe, unixms(expires), viewOnce)
	if err != nil {
		return err
	}
	v, exists, err := loadSourceRow(ctx, tx, key)
	if err != nil || !exists {
		return err
	}
	expiry := v.expires
	for _, candidate := range []int64{unixms(g.ExpiresAt), unixms(expires)} {
		if candidate > 0 && (expiry == 0 || candidate < expiry) {
			expiry = candidate
		}
	}
	view := v.viewOnce || g.ViewOnce || viewOnce
	if expiry == v.expires && view == v.viewOnce {
		return nil
	}
	if err = extensionInvalidate(ctx, tx, v.id, 0); err != nil {
		return err
	}
	expired := expiry > 0 && expiry <= unixms(s.now())
	if expired || view {
		if _, err = tx.ExecContext(ctx, "DELETE FROM message_fts WHERE rowid=?", v.rowid); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE messages SET state=CASE WHEN state='deleted' THEN state WHEN ? THEN 'expired' ELSE 'excluded' END,body='',caption='',normalized_body='',normalized_caption='',content_hash='',extension_media_identity='',reply_account='',reply_chat='',reply_sender='',reply_message='',reply_from_me=0,media_availability=CASE WHEN ? THEN 'expired' ELSE 'view_once' END WHERE id=?`, expired, expired, v.id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE messages SET expires_at=?,view_once=?,local_revision=local_revision+1 WHERE id=?", expiry, view, v.id); err != nil {
		return err
	}
	revision, err := bumpRevision(ctx, tx)
	if err != nil {
		return err
	}
	return dirtyWindow(ctx, tx, v.chatID, policy, v.sent, revision)
}
