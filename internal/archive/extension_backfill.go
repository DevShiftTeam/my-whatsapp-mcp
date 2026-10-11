package archive

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const extensionBackfillLimit = 32

var _ SelectedBackfillStore = (*Store)(nil)

// SelectedBackfillAnchors returns private protocol metadata. An anchor neither
// authorizes a source body nor establishes complete history coverage.
func (s *Store) SelectedBackfillAnchors(ctx context.Context, selection BackfillSelection) ([]SourceAnchor, error) {
	if !validString(selection.Account, 512) || selection.PolicyGeneration == 0 {
		return nil, ErrUnauthorized
	}
	if selection.Limit < 0 || selection.Limit > extensionBackfillLimit || (selection.AllSelected && len(selection.Chats) != 0) || (!selection.AllSelected && (len(selection.Chats) == 0 || len(selection.Chats) > extensionBackfillLimit)) {
		return nil, ErrInvalidQuery
	}
	chats, err := canonicalChats(selection.Chats)
	if err != nil {
		return nil, err
	}
	selection.Chats = chats // Own the finite selection across waits and callbacks.
	if selection.Limit == 0 {
		selection.Limit = extensionBackfillLimit
	}
	epoch, err := s.readSafety(selection.Account)
	if err != nil {
		return nil, err
	}
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	if err = bounded.Err(); err != nil {
		return nil, err
	}
	conn, err := s.readDB.Conn(bounded)
	if err != nil {
		return nil, readError(bounded, err)
	}
	defer conn.Close()
	now := s.now()
	tx, err := conn.BeginTx(bounded, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, readError(bounded, err)
	}
	defer tx.Rollback()
	policy, err := extensionBackfillPolicy(bounded, tx, selection)
	if err != nil {
		return nil, err
	}
	query, args := extensionBackfillEligibleSQL(selection, extensionBackfillCutoff(policy, now), unixms(now))
	query += `, ranked AS (
 SELECT chat,sender,message,from_me,original_sent_at,
 ROW_NUMBER() OVER(PARTITION BY chat ORDER BY admitted DESC,
 CASE WHEN admitted=1 THEN original_sent_at END,
 CASE WHEN admitted=0 THEN original_sent_at END DESC,sender,message,from_me) AS ordinal
 FROM eligible_anchors)
 SELECT chat,sender,message,from_me,original_sent_at FROM ranked WHERE ordinal=1
 ORDER BY original_sent_at,chat,sender,message,from_me LIMIT ?`
	args = append(args, selection.Limit)
	rows, err := tx.QueryContext(bounded, query, args...)
	if err != nil {
		return nil, readError(bounded, err)
	}
	var anchors []SourceAnchor
	for rows.Next() {
		anchor := SourceAnchor{Key: SourceKey{Account: selection.Account}}
		var sent int64
		if err = rows.Scan(&anchor.Key.Chat, &anchor.Key.Sender, &anchor.Key.Message, &anchor.Key.FromMe, &sent); err != nil {
			rows.Close()
			return nil, readError(bounded, err)
		}
		if !validKey(anchor.Key) || len(anchors) == extensionBackfillLimit {
			rows.Close()
			return nil, ErrArchiveUnavailable
		}
		anchor.OriginalSentAt = fromMillis(sent)
		anchors = append(anchors, anchor)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, readError(bounded, err)
	}
	if err = tx.Commit(); err != nil {
		return nil, readError(bounded, err)
	}
	if err = s.extensionCheckBackfillAnchors(bounded, conn, selection, epoch, anchors); err != nil {
		return nil, err
	}
	return anchors, nil
}

// Only scalar policy metadata enters Go; finite membership stays in SQL.
func extensionBackfillPolicy(ctx context.Context, q sqlQuerier, selection BackfillSelection) (ArchivePolicy, error) {
	var policy ArchivePolicy
	var start, rolling int64
	var unsafe bool
	err := q.QueryRowContext(ctx, `SELECT ap.generation,ap.date_mode,ap.import_start,ap.chat_mode,ap.rolling_retention,saf.unsafe
 FROM accounts a JOIN account_safety saf ON saf.account=a.account CROSS JOIN archive_policy ap
 WHERE a.account=? AND ap.id=1 AND NOT EXISTS(SELECT 1 FROM accounts other WHERE other.account<>a.account)`, selection.Account).
		Scan(&policy.Generation, &policy.DateMode, &start, &policy.ChatMode, &rolling, &unsafe)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, ErrUnauthorized
	}
	if err != nil {
		return policy, readError(ctx, err)
	}
	if unsafe {
		return policy, ErrReconciliationRequired
	}
	if policy.Generation == 0 {
		return policy, ErrUnauthorized
	}
	if policy.Generation != selection.PolicyGeneration {
		return policy, ErrPolicyChanged
	}
	if (policy.DateMode != DateAllAvailable && policy.DateMode != DateSince) || (policy.ChatMode != ChatsAll && policy.ChatMode != ChatsSelective) || rolling < 0 {
		return policy, ErrArchiveUnavailable
	}
	policy.ImportStart, policy.RollingRetention = fromMillis(start), time.Duration(rolling)
	return policy, nil
}

func extensionBackfillCutoff(policy ArchivePolicy, now time.Time) int64 {
	cutoff := int64(-1 << 63)
	if policy.DateMode == DateSince {
		cutoff = unixms(policy.ImportStart)
	}
	if policy.RollingRetention > 0 {
		rolling := ceilMillis(now.Add(-policy.RollingRetention))
		if rolling > cutoff {
			cutoff = rolling
		}
	}
	return cutoff
}

// Canonical requested chats and current policy membership are resolved before
// either ranking or result limiting. Only scalar protocol metadata participates.
// Start before the newest discovered message, then continue before the oldest
// admitted active message independently of the bounded discovery-key retention.
func extensionBackfillEligibleSQL(selection BackfillSelection, cutoff, now int64) (string, []any) {
	query := `WITH selected_chats AS (
 SELECT DISTINCT COALESCE(ca.canonical,c.source_chat) AS chat FROM chats c
 LEFT JOIN chat_aliases ca ON ca.account=c.account AND ca.alias=c.source_chat
 JOIN archive_policy ap ON ap.id=1 AND ap.generation=? WHERE c.account=?`
	args := []any{selection.PolicyGeneration, selection.Account}
	if !selection.AllSelected {
		marks := make([]string, len(selection.Chats))
		for i, chat := range selection.Chats {
			marks[i] = "?"
			args = append(args, chat)
		}
		query += " AND c.id IN (" + strings.Join(marks, ",") + ")"
	}
	query += ` AND (ap.chat_mode='all' OR EXISTS(
 SELECT 1 FROM policy_chats pc JOIN chats policy_chat ON policy_chat.id=pc.chat_id AND policy_chat.account=c.account
 LEFT JOIN chat_aliases policy_alias ON policy_alias.account=policy_chat.account AND policy_alias.alias=policy_chat.source_chat
 WHERE COALESCE(policy_alias.canonical,policy_chat.source_chat)=COALESCE(ca.canonical,c.source_chat)))),
 discovery_anchors AS (
 SELECT DISTINCT COALESCE(ca.canonical,a.chat) AS chat,COALESCE(ia.canonical,a.sender) AS sender,
 a.message,a.from_me,a.original_sent_at FROM source_anchors a
 LEFT JOIN chat_aliases ca ON ca.account=a.account AND ca.alias=a.chat
 LEFT JOIN identity_aliases ia ON ia.account=a.account AND ia.alias=a.sender
 JOIN selected_chats selected ON selected.chat=COALESCE(ca.canonical,a.chat)
 WHERE a.account=? AND a.date_known=1 AND a.original_sent_at>=?
 AND length(CAST(COALESCE(ca.canonical,a.chat) AS BLOB)) BETWEEN 1 AND 512
 AND length(CAST(COALESCE(ia.canonical,a.sender) AS BLOB)) BETWEEN 1 AND 512
 AND length(CAST(a.message AS BLOB)) BETWEEN 1 AND 512),
 active_anchors AS (
 SELECT DISTINCT COALESCE(ca.canonical,m.source_chat) AS chat,COALESCE(ia.canonical,m.source_sender) AS sender,
 m.source_message AS message,m.from_me,m.original_sent_at FROM messages m
 LEFT JOIN chat_aliases ca ON ca.account=m.account AND ca.alias=m.source_chat
 LEFT JOIN identity_aliases ia ON ia.account=m.account AND ia.alias=m.source_sender
 JOIN selected_chats selected ON selected.chat=COALESCE(ca.canonical,m.source_chat)
 WHERE m.account=? AND m.state='active' AND m.original_date_known=1 AND m.original_sent_at>=?
 AND (m.expires_at=0 OR m.expires_at>?)
 AND length(CAST(COALESCE(ca.canonical,m.source_chat) AS BLOB)) BETWEEN 1 AND 512
 AND length(CAST(COALESCE(ia.canonical,m.source_sender) AS BLOB)) BETWEEN 1 AND 512
 AND length(CAST(m.source_message AS BLOB)) BETWEEN 1 AND 512),
 eligible_anchors AS (
 SELECT chat,sender,message,from_me,original_sent_at,1 AS admitted FROM active_anchors
 UNION ALL
 SELECT d.chat,d.sender,d.message,d.from_me,d.original_sent_at,0 AS admitted FROM discovery_anchors d
 WHERE NOT EXISTS(SELECT 1 FROM active_anchors a WHERE a.chat=d.chat))`
	args = append(args, selection.Account, cutoff, selection.Account, cutoff, now)
	return query, args
}

func (s *Store) extensionCheckBackfillAnchors(ctx context.Context, conn *sql.Conn, selection BackfillSelection, epoch uint64, anchors []SourceAnchor) error {
	// Connection acquisition and callbacks happen before the bounded final fence.
	clockSample, sampledAt := s.now(), time.Now()
	fenced, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	s.safetyMu.RLock()
	defer s.safetyMu.RUnlock()
	if s.unclean || s.unsafe[selection.Account] {
		return ErrReconciliationRequired
	}
	if s.safetyEpoch != epoch {
		return ErrPolicyChanged
	}
	tx, err := conn.BeginTx(fenced, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return readError(fenced, err)
	}
	defer tx.Rollback()
	policy, err := extensionBackfillPolicy(fenced, tx, selection)
	if err != nil {
		return err
	}
	if len(anchors) > 0 {
		currentTime := clockSample.Add(time.Since(sampledAt))
		query, args := extensionBackfillEligibleSQL(selection, extensionBackfillCutoff(policy, currentTime), unixms(currentTime))
		marks := make([]string, len(anchors))
		for i, anchor := range anchors {
			marks[i] = "(?,?,?,?,?)"
			args = append(args, anchor.Key.Chat, anchor.Key.Sender, anchor.Key.Message, anchor.Key.FromMe, unixms(anchor.OriginalSentAt))
		}
		query += ", expected(chat,sender,message,from_me,original_sent_at) AS (VALUES " + strings.Join(marks, ",") + `)
 SELECT COUNT(*) FROM expected e WHERE EXISTS(SELECT 1 FROM eligible_anchors a
 WHERE a.chat=e.chat AND a.sender=e.sender AND a.message=e.message AND a.from_me=e.from_me AND a.original_sent_at=e.original_sent_at)`
		var current int
		if err = tx.QueryRowContext(fenced, query, args...).Scan(&current); err != nil {
			return readError(fenced, err)
		}
		if current != len(anchors) {
			return ErrPolicyChanged
		}
	}
	if err = tx.Commit(); err != nil {
		return readError(fenced, err)
	}
	// A shared final horizon catches rolling-retention expiry while the queries ran.
	cutoff := extensionBackfillCutoff(policy, clockSample.Add(time.Since(sampledAt)))
	for _, anchor := range anchors {
		if unixms(anchor.OriginalSentAt) < cutoff {
			return ErrPolicyChanged
		}
	}
	return fenced.Err()
}
