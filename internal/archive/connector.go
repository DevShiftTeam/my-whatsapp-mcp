package archive

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *Store) BoundAccount(ctx context.Context) (string, error) {
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	var a string
	e := s.readDB.QueryRowContext(ctx, "SELECT account FROM accounts LIMIT 1").Scan(&a)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	return a, fixedSQLError(e)
}
func (s *Store) SourcePolicy(ctx context.Context, account string) (IngestionPolicy, error) {
	if !validString(account, 512) {
		return IngestionPolicy{}, ErrInvalidEvent
	}
	bound, e := s.BoundAccount(ctx)
	if e != nil {
		return IngestionPolicy{}, e
	}
	if bound != "" && bound != account {
		return IngestionPolicy{}, ErrUnauthorized
	}
	if _, e = s.readSafety(account); e != nil {
		return IngestionPolicy{}, e
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	tx, e := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return IngestionPolicy{}, fixedSQLError(e)
	}
	defer tx.Rollback()
	p, e := s.loadPolicy(ctx, tx)
	if e != nil {
		return IngestionPolicy{}, e
	}
	out := IngestionPolicy{Account: account, Generation: p.Generation, DateMode: p.DateMode, ImportStart: p.ImportStart, ChatMode: p.ChatMode, RollingRetention: p.RollingRetention}
	rows, e := tx.QueryContext(ctx, "SELECT c.source_chat FROM chats c JOIN policy_chats p ON p.chat_id=c.id WHERE c.account=? ORDER BY c.source_chat", account)
	if e != nil {
		return out, fixedSQLError(e)
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		if e = rows.Scan(&c); e != nil {
			return out, fixedSQLError(e)
		}
		out.IncludedSourceChats = append(out.IncludedSourceChats, c)
	}
	return out, fixedSQLError(rows.Err())
}
func (s *Store) Canonical(ctx context.Context, account, identity string) (string, error) {
	if !validString(account, 512) || !validString(identity, 512) {
		return "", ErrInvalidEvent
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	v, e := canonicalQuery(ctx, s.readDB, account, identity)
	return v, fixedSQLError(e)
}
func (s *Store) SourceMetadata(ctx context.Context, key SourceKey) (SourceMetadata, error) {
	if !validKey(key) {
		return SourceMetadata{}, ErrInvalidEvent
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	tx, e := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return SourceMetadata{}, fixedSQLError(e)
	}
	defer tx.Rollback()
	key, e = canonicalKey(ctx, tx, key)
	if e != nil {
		return SourceMetadata{}, fixedSQLError(e)
	}
	out := SourceMetadata{Key: key}
	p, e := s.loadPolicy(ctx, tx)
	if e != nil {
		return out, e
	}
	guarded, e := hasUnresolvedDeletion(ctx, tx, key.Account, key.Chat, key.Message)
	if e != nil {
		return out, fixedSQLError(e)
	}
	if guarded {
		out.Version.Deleted = true
		out.Version.PolicyGeneration = p.Generation
	}
	v, exists, e := loadSourceRow(ctx, tx, key)
	if e != nil {
		return out, fixedSQLError(e)
	}
	if exists {
		out.OriginalSentAt = fromMillis(v.sent)
		out.OriginalDateKnown = v.known
		out.ProtocolRevision = v.protocol
		out.Version = versionOf(v, p.Generation)
		out.Version.Deleted = out.Version.Deleted || guarded
		return out, nil
	}
	if s.rebuild != nil {
		g, err := rebuildGuardQuery(ctx, tx, key)
		if err != nil {
			return out, err
		}
		out.OriginalSentAt = g.OriginalSentAt
		out.OriginalDateKnown = g.OriginalDateKnown
		out.ProtocolRevision = g.ProtocolRevision
		out.Version.Deleted = out.Version.Deleted || g.Deleted
		out.Version.ExpiresAt = g.ExpiresAt
		out.Version.PolicyGeneration = p.Generation
	}
	var revision int64
	e = tx.QueryRowContext(ctx, "SELECT protocol_revision FROM source_tombstones WHERE account=? AND chat=? AND sender=? AND message=? AND from_me=?", key.Account, key.Chat, key.Sender, key.Message, key.FromMe).Scan(&revision)
	if e == nil {
		out.ProtocolRevision = revision
		out.Version.Deleted = true
		out.Version.PolicyGeneration = p.Generation
		return out, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return out, fixedSQLError(e)
	}
	var sent int64
	var known bool
	e = tx.QueryRowContext(ctx, "SELECT original_sent_at,date_known FROM source_anchors WHERE account=? AND chat=? AND sender=? AND message=? AND from_me=?", key.Account, key.Chat, key.Sender, key.Message, key.FromMe).Scan(&sent, &known)
	if e == nil {
		out.OriginalSentAt = fromMillis(sent)
		out.OriginalDateKnown = known
	} else if !errors.Is(e, sql.ErrNoRows) {
		return out, fixedSQLError(e)
	}
	return out, nil
}
func (s *Store) MutationTargets(ctx context.Context, account, chat, message string) ([]SourceMetadata, error) {
	if !validString(account, 512) || !validString(chat, 512) || !validString(message, 512) {
		return nil, ErrInvalidEvent
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	chat, e := canonicalQuery(ctx, s.readDB, account, chat)
	if e != nil {
		return nil, fixedSQLError(e)
	}
	query := "SELECT source_sender,from_me FROM messages WHERE account=? AND source_chat=? AND source_message=? UNION SELECT sender,from_me FROM source_tombstones WHERE account=? AND chat=? AND message=? UNION SELECT sender,from_me FROM source_anchors WHERE account=? AND chat=? AND message=?"
	args := []any{account, chat, message, account, chat, message, account, chat, message}
	if s.rebuild != nil {
		query += " UNION SELECT COALESCE((SELECT canonical FROM identity_aliases WHERE account=g.account AND alias=g.sender),g.sender),from_me FROM rebuild_guards g WHERE account=? AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=g.account AND alias=g.chat),g.chat)=? AND message=?"
		args = append(args, account, chat, message)
	}
	rows, e := s.readDB.QueryContext(ctx, query+" LIMIT 3", args...)
	if e != nil {
		return nil, fixedSQLError(e)
	}
	var keys []SourceKey
	for rows.Next() {
		k := SourceKey{Account: account, Chat: chat, Message: message}
		if e = rows.Scan(&k.Sender, &k.FromMe); e != nil {
			rows.Close()
			return nil, fixedSQLError(e)
		}
		keys = append(keys, k)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, fixedSQLError(e)
	}
	if len(keys) > 1 {
		return nil, ErrSourceUnavailable
	}
	var out []SourceMetadata
	for _, k := range keys {
		m, e := s.SourceMetadata(ctx, k)
		if e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, nil
}
func (s *Store) BackfillAnchors(ctx context.Context, account string, limit int) ([]SourceAnchor, error) {
	if !validString(account, 512) || limit < 1 || limit > 1000 {
		return nil, ErrInvalidEvent
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	rows, e := s.readDB.QueryContext(ctx, "SELECT chat,sender,message,from_me,original_sent_at FROM source_anchors WHERE account=? AND date_known=1 ORDER BY original_sent_at DESC,chat,sender,message LIMIT ?", account, limit)
	if e != nil {
		return nil, fixedSQLError(e)
	}
	defer rows.Close()
	var out []SourceAnchor
	for rows.Next() {
		v := SourceAnchor{Key: SourceKey{Account: account}}
		var sent int64
		if e = rows.Scan(&v.Key.Chat, &v.Key.Sender, &v.Key.Message, &v.Key.FromMe, &sent); e != nil {
			return nil, fixedSQLError(e)
		}
		v.OriginalSentAt = fromMillis(sent)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fixedSQLError(err)
	}
	rows.Close()
	if s.rebuild != nil {
		filtered := out[:0]
		for _, v := range out {
			g, err := s.RebuildGuard(ctx, v.Key)
			if err != nil {
				return nil, err
			}
			if g.Deleted || g.ViewOnce || (!g.ExpiresAt.IsZero() && !g.ExpiresAt.After(s.now())) || (!g.ChatClearCutoff.IsZero() && !v.OriginalSentAt.After(g.ChatClearCutoff)) {
				continue
			}
			filtered = append(filtered, v)
		}
		out = filtered
	}
	return out, nil
}
func (s *Store) EligibleMedia(ctx context.Context, key SourceKey, version SourceVersion) (bool, error) {
	if !validKey(key) {
		return false, ErrInvalidEvent
	}
	epoch, e := s.readSafety(key.Account)
	if e != nil {
		return false, e
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	tx, e := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return false, fixedSQLError(e)
	}
	defer tx.Rollback()
	p, e := s.loadPolicy(ctx, tx)
	if e != nil {
		return false, e
	}
	key, e = canonicalKey(ctx, tx, key)
	if e != nil {
		return false, fixedSQLError(e)
	}
	v, exists, e := loadSourceRow(ctx, tx, key)
	if e != nil {
		return false, fixedSQLError(e)
	}
	eligible := exists && v.id == version.MessageID && v.local == version.Revision && p.Generation == version.PolicyGeneration && v.state == "active" && !v.viewOnce && (v.expires == 0 || v.expires > unixms(s.now())) && permittedPolicy(p, v.chatID, v.sent, v.known, s.now())
	if e = tx.Commit(); e != nil {
		return false, fixedSQLError(e)
	}
	if e = s.verifyReadSafety(key.Account, epoch); e != nil {
		return false, e
	}
	return eligible, nil
}

func (s *Store) updateMedia(ctx context.Context, k SourceKey, version SourceVersion, status string) error {
	// Retained-original storage belongs to the following durable media migration.
	if status == "retained" {
		return ErrInvalidEvent
	}
	if _, e := s.readSafety(k.Account); e != nil {
		return e
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		p, e := s.loadPolicy(ctx, tx)
		if e != nil {
			return e
		}
		if p.Generation != version.PolicyGeneration {
			return ErrPolicyChanged
		}
		k, e = canonicalKey(ctx, tx, k)
		if e != nil {
			return e
		}
		v, exists, e := loadSourceRow(ctx, tx, k)
		if e != nil {
			return e
		}
		if !exists || v.id != version.MessageID || v.local != version.Revision || v.state != "active" || v.viewOnce || (v.expires != 0 && v.expires <= unixms(s.now())) || !permittedPolicy(p, v.chatID, v.sent, v.known, s.now()) {
			return ErrSourceUnavailable
		}
		var old string
		if e = tx.QueryRowContext(ctx, "SELECT media_availability FROM messages WHERE id=?", v.id).Scan(&old); e != nil {
			return e
		}
		if old == status {
			return nil
		}
		if _, e = tx.ExecContext(ctx, "UPDATE messages SET media_availability=? WHERE id=?", status, v.id); e != nil {
			return e
		}
		_, e = bumpRevision(ctx, tx)
		return e
	})
}

// Already committed identity mappings need no new control mutation. Verify the
// exact pair in one read snapshot so media work cannot stall these no-op
// observations behind the serialized writer's control deadline.
func (s *Store) committedAlias(ctx context.Context, a SourceAlias) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.lifeMu.RLock()
	defer s.lifeMu.RUnlock()
	if s.closed {
		return false, ErrArchiveUnavailable
	}
	epoch, err := s.readSafety(a.Account)
	if err != nil {
		return false, err
	}
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	tx, err := s.readDB.BeginTx(bounded, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, fixedSQLError(err)
	}
	defer tx.Rollback()
	var accounts int
	var account string
	if err = tx.QueryRowContext(bounded, "SELECT count(*),COALESCE(MIN(account),'') FROM accounts").Scan(&accounts, &account); err != nil {
		return false, fixedSQLError(err)
	}
	if accounts == 0 {
		return false, nil
	}
	if accounts != 1 || account != a.Account {
		return false, ErrUnauthorized
	}
	var mappings, matching int
	if err = tx.QueryRowContext(bounded, "SELECT count(*),COALESCE(SUM(canonical=?),0) FROM identity_aliases WHERE account=? AND alias IN (?,?)", a.PhoneJID, a.Account, a.PhoneJID, a.LID).Scan(&mappings, &matching); err != nil {
		return false, fixedSQLError(err)
	}
	if mappings != 2 || matching != 2 {
		return false, nil
	}
	if err = tx.Commit(); err != nil {
		return false, fixedSQLError(err)
	}
	if err = bounded.Err(); err != nil {
		return false, err
	}
	if err = s.verifyReadSafety(a.Account, epoch); errors.Is(err, ErrPolicyChanged) {
		// The observation still needs authoritative handling after a clean
		// epoch transition. Falling back avoids dropping its source message.
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) applyAlias(ctx context.Context, a SourceAlias) error {
	if !strings.HasSuffix(a.PhoneJID, "@s.whatsapp.net") || !strings.HasSuffix(a.LID, "@lid") {
		return ErrInvalidEvent
	}
	if _, e := s.readSafety(a.Account); e != nil {
		return e
	}
	conflict := false
	e := s.transaction(ctx, func(tx *sql.Tx) error {
		if e := bindAccount(ctx, tx, a.Account); e != nil {
			return e
		}
		existingMappings := 0
		for _, alias := range []string{a.PhoneJID, a.LID} {
			var existing string
			e := tx.QueryRowContext(ctx, "SELECT canonical FROM identity_aliases WHERE account=? AND alias=?", a.Account, alias).Scan(&existing)
			if e == nil {
				existingMappings++
			}
			if e == nil && existing != a.PhoneJID {
				conflict = true
				return ErrIdentityAliasConflict
			}
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return e
			}
		}
		if existingMappings == 2 {
			return nil
		}
		if e := invalidateAliasSources(ctx, tx, a); e != nil {
			return e
		}
		// Prevent unique-key collision before moving any source key. Incompatible
		// already-returned messages are denied, never silently retargeted.
		var n int
		if e := tx.QueryRowContext(ctx, "SELECT count(*) FROM messages l JOIN messages p ON p.account=l.account AND p.source_message=l.source_message AND p.from_me=l.from_me AND p.source_chat=CASE WHEN l.source_chat=? THEN ? ELSE l.source_chat END AND p.source_sender=CASE WHEN l.source_sender=? THEN ? ELSE l.source_sender END WHERE l.account=? AND (l.source_chat=? OR l.source_sender=?) AND p.id<>l.id", a.LID, a.PhoneJID, a.LID, a.PhoneJID, a.Account, a.LID, a.LID).Scan(&n); e != nil {
			return e
		}
		if n > 0 {
			conflict = true
			return ErrIdentityAliasConflict
		}
		for _, table := range []string{"identity_aliases", "chat_aliases"} {
			for _, alias := range []string{a.PhoneJID, a.LID} {
				if _, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO "+table+"(account,alias,canonical,provenance) VALUES(?,?,?,?)", a.Account, alias, a.PhoneJID, a.Provenance); e != nil {
					return e
				}
			}
		}
		// Keep public IDs when only one side exists; when both exist the old
		// metadata ID stays an unavailable identity, never another person's ID.
		for _, x := range []struct{ table, col, foreign string }{{"chats", "source_chat", "chat_id"}, {"identities", "source_identity", "sender_id"}} {
			var pn, lid string
			ep := tx.QueryRowContext(ctx, "SELECT id FROM "+x.table+" WHERE account=? AND "+x.col+"=?", a.Account, a.PhoneJID).Scan(&pn)
			el := tx.QueryRowContext(ctx, "SELECT id FROM "+x.table+" WHERE account=? AND "+x.col+"=?", a.Account, a.LID).Scan(&lid)
			if ep != nil && !errors.Is(ep, sql.ErrNoRows) {
				return ep
			}
			if el != nil && !errors.Is(el, sql.ErrNoRows) {
				return el
			}
			if el == nil {
				if ep == nil {
					if _, e := tx.ExecContext(ctx, "UPDATE messages SET "+x.foreign+"=? WHERE account=? AND "+x.foreign+"=?", pn, a.Account, lid); e != nil {
						return e
					}
					if x.table == "chats" {
						if e := extensionReconcileCredentialChat(ctx, tx, ChatID(lid), ChatID(pn)); e != nil {
							return e
						}
						if _, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO policy_chats(chat_id) SELECT ? FROM policy_chats WHERE chat_id=?", pn, lid); e != nil {
							return e
						}
						if _, e := tx.ExecContext(ctx, "DELETE FROM policy_chats WHERE chat_id=?", lid); e != nil {
							return e
						}
					}
				} else {
					if _, e := tx.ExecContext(ctx, "UPDATE "+x.table+" SET "+x.col+"=? WHERE id=?", a.PhoneJID, lid); e != nil {
						return e
					}
				}
			}
		}
		if _, e := tx.ExecContext(ctx, "UPDATE messages SET source_chat=CASE WHEN source_chat=? THEN ? ELSE source_chat END,source_sender=CASE WHEN source_sender=? THEN ? ELSE source_sender END,reply_chat=CASE WHEN reply_chat=? THEN ? ELSE reply_chat END,reply_sender=CASE WHEN reply_sender=? THEN ? ELSE reply_sender END WHERE account=?", a.LID, a.PhoneJID, a.LID, a.PhoneJID, a.LID, a.PhoneJID, a.LID, a.PhoneJID, a.Account); e != nil {
			return e
		}
		for _, table := range []string{"source_tombstones", "source_anchors"} {
			if _, e := tx.ExecContext(ctx, "UPDATE OR IGNORE "+table+" SET chat=CASE WHEN chat=? THEN ? ELSE chat END,sender=CASE WHEN sender=? THEN ? ELSE sender END WHERE account=?", a.LID, a.PhoneJID, a.LID, a.PhoneJID, a.Account); e != nil {
				return e
			}
		}
		if _, e := tx.ExecContext(ctx, "INSERT INTO chat_tombstones(account,chat,cutoff) SELECT account,?,cutoff FROM chat_tombstones WHERE account=? AND chat=? ON CONFLICT(account,chat) DO UPDATE SET cutoff=MAX(cutoff,excluded.cutoff)", a.PhoneJID, a.Account, a.LID); e != nil {
			return e
		}
		rev, e := bumpRevision(ctx, tx)
		if e != nil {
			return e
		}
		p, e := s.loadPolicy(ctx, tx)
		if e != nil {
			return e
		}
		if e = terminalizeAliasTombstones(ctx, tx, a, p.Generation, rev); e != nil {
			return e
		}
		return reconstructDirty(ctx, tx, p.Generation)
	})
	if conflict {
		s.denyImmediate(a.Account, "identity_alias_conflict")
		persistCtx, cancel := context.WithTimeout(context.Background(), s.options.ControlDeadline)
		defer cancel()
		_ = s.persistUnsafe(persistCtx, a.Account, "identity_alias_conflict")
	}
	return e
}

// A new identity mapping revokes only sources whose identity or reply changes.
// Invalidating each source also removes every dependent multi-source window;
// unrelated original downloads, transcripts, and extraction leases stay valid.
func invalidateAliasSources(ctx context.Context, tx *sql.Tx, a SourceAlias) error {
	rows, err := tx.QueryContext(ctx, `SELECT m.id FROM messages m
 LEFT JOIN chats c ON c.id=m.chat_id LEFT JOIN identities i ON i.id=m.sender_id
 WHERE m.account=? AND (m.source_chat=? OR m.source_sender=? OR m.reply_chat=? OR m.reply_sender=?
 OR (c.account=m.account AND c.source_chat=?) OR (i.account=m.account AND i.source_identity=?))
 ORDER BY m.id`, a.Account, a.LID, a.LID, a.LID, a.LID, a.LID, a.LID)
	if err != nil {
		return err
	}
	var ids []MessageID
	for rows.Next() {
		var id MessageID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = extensionInvalidate(ctx, tx, id, 0); err != nil {
			return err
		}
	}
	return nil
}

// A mapped LID deletion can newly cover an existing PN source. Apply the same
// irreversible clear-chat transition after merging canonical tombstone keys.
func terminalizeAliasTombstones(ctx context.Context, tx *sql.Tx, a SourceAlias, generation Generation, rev ArchiveRevision) error {
	rows, err := tx.QueryContext(ctx, `SELECT m.rowid,m.id,m.chat_id,m.source_chat,m.source_sender,m.source_message,m.from_me,m.protocol_revision,m.original_sent_at
 FROM messages m WHERE m.account=? AND m.state<>'deleted' AND (
 (m.source_chat=? AND m.original_date_known=1 AND EXISTS(SELECT 1 FROM chat_tombstones ct WHERE ct.account=m.account AND ct.chat=m.source_chat AND m.original_sent_at<=ct.cutoff))
 OR ((m.source_chat=? OR m.source_sender=?) AND EXISTS(SELECT 1 FROM source_tombstones st WHERE st.account=m.account AND st.chat=m.source_chat AND st.sender=m.source_sender AND st.message=m.source_message AND st.from_me=m.from_me)))
 ORDER BY m.id`, a.Account, a.PhoneJID, a.PhoneJID, a.PhoneJID)
	if err != nil {
		return err
	}
	var list []sourceDeletion
	for rows.Next() {
		var v sourceDeletion
		if err = rows.Scan(&v.row, &v.id, &v.chat, &v.sourceChat, &v.sender, &v.message, &v.from, &v.revision, &v.sent); err != nil {
			rows.Close()
			return err
		}
		list = append(list, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, v := range list {
		if err = terminalizeSource(ctx, tx, a.Account, generation, v, rev); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SetSourceNames(ctx context.Context, n SourceNames) error {
	if !validString(n.Account, 512) || (n.Chat == "" && n.Sender == "") || (n.Chat != "" && !validString(n.Chat, 512)) || (n.Sender != "" && !validString(n.Sender, 512)) || len(n.ChatName) > 256 || len(n.SenderName) > 256 || !utf8.ValidString(n.ChatName) || !utf8.ValidString(n.SenderName) || strings.ContainsRune(n.ChatName, 0) || strings.ContainsRune(n.SenderName, 0) {
		return ErrInvalidEvent
	}
	_, e := s.submit(ctx, namesCommand{n}, n.Account, false, int64(512+len(n.Account)+len(n.Chat)+len(n.Sender)+len(n.ChatName)+len(n.SenderName)))
	return e
}
func (s *Store) setSourceNames(ctx context.Context, n SourceNames) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var before, after int64
		if e := tx.QueryRowContext(ctx, "SELECT total_changes()").Scan(&before); e != nil {
			return e
		}
		if e := bindAccount(ctx, tx, n.Account); e != nil {
			return e
		}
		if n.Chat != "" {
			chat, e := canonicalQuery(ctx, tx, n.Account, n.Chat)
			if e != nil {
				return e
			}
			id, e := ensureChat(ctx, tx, SourceKey{Account: n.Account, Chat: chat}, 0)
			if e != nil {
				return e
			}
			if n.ChatName != "" {
				if _, e = tx.ExecContext(ctx, "UPDATE chats SET name=? WHERE id=? AND name<>?", n.ChatName, id, n.ChatName); e != nil {
					return e
				}
			}
		}
		if n.Sender != "" {
			sender, e := canonicalQuery(ctx, tx, n.Account, n.Sender)
			if e != nil {
				return e
			}
			id, e := ensureIdentity(ctx, tx, SourceKey{Account: n.Account, Sender: sender})
			if e != nil {
				return e
			}
			if n.SenderName != "" {
				if _, e = tx.ExecContext(ctx, "UPDATE identities SET name=? WHERE id=? AND name<>?", n.SenderName, id, n.SenderName); e != nil {
					return e
				}
			}
		}
		if e := tx.QueryRowContext(ctx, "SELECT total_changes()").Scan(&after); e != nil {
			return e
		}
		if before == after {
			return nil
		}
		_, e := bumpRevision(ctx, tx)
		return e
	})
}
func (s *Store) DiscoveryChats(ctx context.Context, request ChatListRequest) (DiscoveryPage, error) {
	limit := request.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return DiscoveryPage{}, ErrInvalidQuery
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	tx, e := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return DiscoveryPage{}, fixedSQLError(e)
	}
	defer tx.Rollback()
	var page DiscoveryPage
	p, e := s.loadPolicy(ctx, tx)
	if e != nil {
		return page, e
	}
	page.Snapshot.PolicyGeneration = p.Generation
	if e = tx.QueryRowContext(ctx, "SELECT archive_revision,index_generation,recovery_epoch,recovery_code FROM archive_meta WHERE id=1").Scan(&page.Snapshot.ArchiveRevision, &page.Snapshot.IndexGeneration, &page.Snapshot.RecoveryEpoch, &page.Snapshot.RecoveryCode); e != nil {
		return page, fixedSQLError(e)
	}
	var account string
	e = tx.QueryRowContext(ctx, "SELECT account FROM accounts LIMIT 1").Scan(&account)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return page, fixedSQLError(e)
	}
	if e = unknownHistoryGapSnapshot(ctx, tx, account, &page.Snapshot); e != nil {
		return page, e
	}
	identity := cursorIdentity("owner_discovery", ReadScope{Account: account, Principal: "local_owner", PolicyGeneration: p.Generation, AllChats: true}, ChatListRequest{Limit: limit}, page.Snapshot, time.Time{}, time.Time{}, time.Time{})
	offset := 0
	if request.Cursor != "" {
		offset, e = decodeCursor(s.cursorKey, request.Cursor, identity)
		if e != nil {
			return page, e
		}
	}
	rows, e := tx.QueryContext(ctx, "SELECT c.id,c.name,c.observed_oldest,c.observed_latest FROM chats c WHERE c.account=? AND NOT EXISTS(SELECT 1 FROM chat_aliases a WHERE a.account=c.account AND a.alias=c.source_chat AND a.canonical<>c.source_chat) ORDER BY c.id LIMIT ? OFFSET ?", account, limit+1, offset)
	if e != nil {
		return page, fixedSQLError(e)
	}
	for rows.Next() {
		var v DiscoveryChatView
		var oldest, latest int64
		if e = rows.Scan(&v.ID, &v.Name, &oldest, &latest); e != nil {
			rows.Close()
			return page, fixedSQLError(e)
		}
		v.Name = boundedOriginal(v.Name, 256)
		v.ObservedOldest = fromMillis(oldest)
		v.ObservedLatest = fromMillis(latest)
		v.Coverage = "unknown"
		if page.Snapshot.UnknownHistoryGap {
			v.Coverage = "partial"
			v.GapCodes = append(v.GapCodes, UnknownHistoryGapCode)
		}
		v.Selected = p.Generation > 0 && p.ChatMode == ChatsAll
		for _, id := range p.IncludedChats {
			if id == v.ID {
				v.Selected = true
				break
			}
		}
		page.Chats = append(page.Chats, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return page, fixedSQLError(e)
	}
	if len(page.Chats) > limit {
		page.Chats = page.Chats[:limit]
		page.NextCursor, e = encodeCursor(s.cursorKey, identity, offset+limit)
		if e != nil {
			return page, e
		}
	}
	for i := range page.Chats {
		rows, e = tx.QueryContext(ctx, "SELECT g.code FROM sync_gaps g JOIN chats c ON c.account=g.account AND c.source_chat=g.chat WHERE c.id=? ORDER BY g.code LIMIT 16", page.Chats[i].ID)
		if e != nil {
			return page, fixedSQLError(e)
		}
		for rows.Next() {
			var code string
			if e = rows.Scan(&code); e != nil {
				rows.Close()
				return page, fixedSQLError(e)
			}
			if validGap(code) || validUnsafeReason(UnsafeReason(code)) {
				page.Chats[i].GapCodes = append(page.Chats[i].GapCodes, code)
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return page, fixedSQLError(e)
		}
	}
	return page, nil
}

// IsDiagnosticFailureCode permits only fixed operational reasons and app-state
// witnesses in public status. Witnesses do not authorize unsafe or gap commands.
// Connector errors, account identifiers and source text are excluded.
func IsDiagnosticFailureCode(code string) bool {
	if validUnsafeReason(UnsafeReason(code)) || code == "archive_unavailable" || code == "owner_restored_snapshot" {
		return true
	}
	switch code {
	case "app_state_collection_critical_block", "app_state_collection_critical_unblock_low",
		"app_state_collection_regular_low", "app_state_collection_regular_high",
		"app_state_collection_regular", "app_state_collection_unknown",
		"app_state_error_lthash", "app_state_error_patch_mac", "app_state_error_content_mac", "app_state_error_index_mac",
		"app_state_error_canceled", "app_state_error_deadline", "app_state_error_json_syntax", "app_state_error_json_type", "app_state_error_unknown",
		"app_state_full_sync", "app_state_incremental_sync":
		return true
	}
	return false
}

func (s *Store) DiagnosticStatus(ctx context.Context) (StatusSnapshot, error) {
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	account, e := s.BoundAccount(ctx)
	if e != nil {
		return StatusSnapshot{}, e
	}
	p, e := s.Policy(ctx)
	if e != nil {
		return StatusSnapshot{}, e
	}
	status := StatusSnapshot{Rebuild: s.rebuildStatus(), Connection: "setup_required", Coverage: "unknown", Keyword: Progress{State: "not_configured"}, Semantic: Progress{State: "not_configured"}, Media: Progress{State: "not_configured"}, Snapshot: ReadSnapshot{PolicyGeneration: p.Generation}}
	if status.Rebuild != nil {
		status.Coverage = "partial"
	}
	if e = s.readDB.QueryRowContext(ctx, "SELECT archive_revision,index_generation,recovery_epoch,recovery_code FROM archive_meta WHERE id=1").Scan(&status.Snapshot.ArchiveRevision, &status.Snapshot.IndexGeneration, &status.Snapshot.RecoveryEpoch, &status.Snapshot.RecoveryCode); e != nil {
		return status, fixedSQLError(e)
	}
	if e = unknownHistoryGapSnapshot(ctx, s.readDB, account, &status.Snapshot); e != nil {
		return status, e
	}
	if status.Snapshot.UnknownHistoryGap {
		status.Coverage = "partial"
		status.Codes = append(status.Codes, UnknownHistoryGapCode)
	}
	s.safetyMu.RLock()
	unsafe := s.unclean
	for _, v := range s.unsafe {
		unsafe = unsafe || v
	}
	s.safetyMu.RUnlock()
	if unsafe {
		status.Connection = "reconciliation_required"
		status.ReconciliationRequired = true
		status.Codes = append(status.Codes, "reconciliation_required")
		// The archive has one bound account. Read only its durable failure reason;
		// unsafe diagnostics must never authorize message or chat reads.
		var reason string
		e = s.readDB.QueryRowContext(ctx, "SELECT reason FROM account_safety WHERE account=? AND unsafe=1 LIMIT 1", account).Scan(&reason)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return status, fixedSQLError(e)
		}
		if validUnsafeReason(UnsafeReason(reason)) && reason != "reconciliation_required" {
			status.Codes = append(status.Codes, reason)
		}
		return status, nil
	}
	if account == "" || p.Generation == 0 {
		return status, nil
	}
	return s.Status(ctx, ReadScope{Account: account, Principal: "local_owner", PolicyGeneration: p.Generation, AllChats: true})
}
