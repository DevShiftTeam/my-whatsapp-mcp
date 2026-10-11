package archive

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const existingNameBatchLimit = 64

// Each primary-key window is bounded before alias/name lookup and merging.
// Account authorization is checked first and repeated on the resulting rows.
const existingNameTargetsSQL = `WITH chat_window AS (
 SELECT id,account,source_chat FROM chats WHERE ? AND id>? ORDER BY id LIMIT ?),
 identity_window AS (
 SELECT id,account,source_identity FROM identities WHERE id>? ORDER BY id LIMIT ?)
 SELECT key,chat,sender,name FROM (
 SELECT 'c:'||c.id AS key,canonical.source_chat AS chat,'' AS sender,canonical.name AS name
 FROM chat_window c LEFT JOIN chat_aliases a ON a.account=c.account AND a.alias=c.source_chat
 JOIN chats canonical ON canonical.account=c.account AND canonical.source_chat=COALESCE(a.canonical,c.source_chat)
 WHERE c.account=?
 UNION ALL
 SELECT 'i:'||i.id AS key,'' AS chat,canonical.source_identity AS sender,canonical.name AS name
 FROM identity_window i LEFT JOIN identity_aliases a ON a.account=i.account AND a.alias=i.source_identity
 JOIN identities canonical ON canonical.account=i.account AND canonical.source_identity=COALESCE(a.canonical,i.source_identity)
 WHERE i.account=?) ORDER BY key LIMIT ?`

// ExistingNameTarget is private connector metadata, never a client read grant.
// Exactly one of Chat and Sender names an already existing canonical source.
type ExistingNameTarget struct {
	Key    string
	Chat   string
	Sender string
	Name   string
}

// ExistingNameTargets pages existing archive metadata for an optional cached
// connector-name refresh. It does not enumerate the connector's phone book.
func (s *Store) ExistingNameTargets(ctx context.Context, account, after string, limit int) ([]ExistingNameTarget, string, error) {
	if !validString(account, 512) {
		return nil, "", ErrInvalidEvent
	}
	if limit < 0 || limit > existingNameBatchLimit || (after != "" && !validExistingNameKey(after)) {
		return nil, "", ErrInvalidQuery
	}
	if limit == 0 {
		limit = existingNameBatchLimit
	}
	epoch, err := s.readSafety(account)
	if err != nil {
		return nil, "", err
	}
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	conn, err := s.readDB.Conn(bounded)
	if err != nil {
		return nil, "", readError(bounded, err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(bounded, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", readError(bounded, err)
	}
	defer tx.Rollback()
	if err = existingNamesAccount(bounded, tx, account); err != nil {
		return nil, "", err
	}
	chatAfter, identityAfter := "", ""
	chatsEnabled := true
	if strings.HasPrefix(after, "c:") {
		chatAfter = after[2:]
	} else if strings.HasPrefix(after, "i:") {
		chatsEnabled, identityAfter = false, after[2:]
	}
	rows, err := tx.QueryContext(bounded, existingNameTargetsSQL,
		chatsEnabled, chatAfter, limit+1, identityAfter, limit+1, account, account, limit+1)
	if err != nil {
		return nil, "", readError(bounded, err)
	}
	var targets []ExistingNameTarget
	for rows.Next() {
		var target ExistingNameTarget
		if err = rows.Scan(&target.Key, &target.Chat, &target.Sender, &target.Name); err != nil {
			rows.Close()
			return nil, "", readError(bounded, err)
		}
		if !validExistingNameKey(target.Key) || (target.Chat == "") == (target.Sender == "") ||
			(target.Chat != "" && !validString(target.Chat, 512)) || (target.Sender != "" && !validString(target.Sender, 512)) || !validExistingNameLabel(target.Name) {
			rows.Close()
			return nil, "", ErrArchiveUnavailable
		}
		targets = append(targets, target)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", readError(bounded, err)
	}
	if err = tx.Commit(); err != nil {
		return nil, "", readError(bounded, err)
	}
	if err = s.checkExistingNameTargets(bounded, conn, account, epoch); err != nil {
		return nil, "", err
	}
	next := ""
	if len(targets) > limit {
		targets = targets[:limit]
		next = targets[len(targets)-1].Key
	}
	return targets, next, nil
}

func validExistingNameKey(key string) bool {
	return len(key) > 2 && (strings.HasPrefix(key, "c:") || strings.HasPrefix(key, "i:")) && validString(key[2:], 256)
}

func validExistingNameLabel(name string) bool {
	return len(name) <= 256 && utf8.ValidString(name) && !strings.ContainsRune(name, 0)
}

func existingNamesAccount(ctx context.Context, q sqlQuerier, account string) error {
	var unsafe bool
	err := q.QueryRowContext(ctx, `SELECT saf.unsafe FROM accounts a
 JOIN account_safety saf ON saf.account=a.account WHERE a.account=?
 AND NOT EXISTS(SELECT 1 FROM accounts other WHERE other.account<>a.account)`, account).Scan(&unsafe)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnauthorized
	}
	if err != nil {
		return fixedSQLError(err)
	}
	if unsafe {
		return ErrReconciliationRequired
	}
	return nil
}

func (s *Store) checkExistingNameTargets(ctx context.Context, conn *sql.Conn, account string, epoch uint64) error {
	// The read connection is already admitted before taking the final fence.
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	s.safetyMu.RLock()
	defer s.safetyMu.RUnlock()
	if s.unclean || s.unsafe[account] {
		return ErrReconciliationRequired
	}
	if s.safetyEpoch != epoch {
		return ErrPolicyChanged
	}
	tx, err := conn.BeginTx(bounded, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return readError(bounded, err)
	}
	defer tx.Rollback()
	if err = existingNamesAccount(bounded, tx, account); err != nil {
		return err
	}
	return readError(bounded, tx.Commit())
}

type refreshExistingNamesCommand struct {
	names []SourceNames
}

func (c refreshExistingNamesCommand) run(s *Store, ctx context.Context) (any, error) {
	return nil, s.refreshExistingSourceNames(ctx, c.names)
}

func (refreshExistingNamesCommand) priority() bool                    { return false }
func (c refreshExistingNamesCommand) account() string                 { return c.names[0].Account }
func (refreshExistingNamesCommand) sensitivity() extensionSensitivity { return extensionBackground }

// RefreshExistingSourceNames updates labels only for existing canonical rows.
// Optional cache refresh failures do not revoke source or credential authority.
func (s *Store) RefreshExistingSourceNames(ctx context.Context, names []SourceNames) error {
	if len(names) > existingNameBatchLimit {
		return ErrLimitExceeded
	}
	if len(names) == 0 {
		return ctx.Err()
	}
	owned := append([]SourceNames(nil), names...)
	account := owned[0].Account
	var bytes int64
	for _, name := range owned {
		if !validString(name.Account, 512) || (name.Chat == "" && name.Sender == "") ||
			(name.Chat != "" && !validString(name.Chat, 512)) || (name.Sender != "" && !validString(name.Sender, 512)) ||
			(name.Chat == "" && name.ChatName != "") || (name.Sender == "" && name.SenderName != "") ||
			!validExistingNameLabel(name.ChatName) || !validExistingNameLabel(name.SenderName) {
			return ErrInvalidEvent
		}
		if name.Account != account {
			return ErrUnauthorized
		}
		bytes += int64(512 + len(name.Account) + len(name.Chat) + len(name.Sender) + len(name.ChatName) + len(name.SenderName))
	}
	if _, err := s.readSafety(account); err != nil {
		return err
	}
	_, err := s.submitExtension(ctx, refreshExistingNamesCommand{names: owned}, bytes)
	return err
}

func (s *Store) refreshExistingSourceNames(ctx context.Context, names []SourceNames) error {
	account := names[0].Account
	return s.extensionTransaction(ctx, account, func(tx *sql.Tx) error {
		if err := existingNamesAccount(ctx, tx, account); err != nil {
			return err
		}
		changed := false
		for _, name := range names {
			for _, target := range []struct {
				source, label, query string
			}{
				{name.Chat, name.ChatName, "UPDATE chats SET name=? WHERE account=? AND source_chat=? AND name<>?"},
				{name.Sender, name.SenderName, "UPDATE identities SET name=? WHERE account=? AND source_identity=? AND name<>?"},
			} {
				if target.label == "" {
					continue
				}
				canonical, err := canonicalQuery(ctx, tx, account, target.source)
				if err != nil {
					return err
				}
				if canonical != target.source {
					return ErrSourceUnavailable // A cached target no longer identifies this canonical row.
				}
				result, err := tx.ExecContext(ctx, target.query, target.label, account, canonical, target.label)
				if err != nil {
					return err
				}
				count, err := result.RowsAffected()
				if err != nil {
					return err
				}
				changed = changed || count > 0
			}
		}
		if !changed {
			return nil
		}
		if _, err := bumpRevision(ctx, tx); err != nil {
			return err
		}
		s.extensionRegisterMetadataChange(tx)
		return nil
	})
}
