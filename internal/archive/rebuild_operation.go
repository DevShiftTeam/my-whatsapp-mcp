package archive

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const rebuildCadence = 10 * time.Second
const maxRebuildCopyLimit int64 = 1000000
const maxRebuildDiscoveryLimit int64 = 65536

type rebuildOperation struct {
	RebuildProgress
	copyChat, discoveryChat string
}

// Only a rebuilt destination reaches this additive migration. The ordinary
// migration ledger and supported original backup schema remain unchanged.
func initializeRebuildOperation(ctx context.Context, tx *sql.Tx, provenance RebuildProvenance, now time.Time) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info(rebuild_requests)")
	if err != nil {
		return err
	}
	hasID, hasRevision := false, false
	for rows.Next() {
		var seq, notnull, pk int
		var name, kind string
		var def any
		if err = rows.Scan(&seq, &name, &kind, &notnull, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		hasID = hasID || name == "operation_id"
		hasRevision = hasRevision || name == "operation_revision"
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if !hasID {
		if _, err = tx.ExecContext(ctx, "ALTER TABLE rebuild_requests ADD COLUMN operation_id TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if !hasRevision {
		if _, err = tx.ExecContext(ctx, "ALTER TABLE rebuild_requests ADD COLUMN operation_revision INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, rebuildOperationSchema); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM rebuild_operation").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		var policy Generation
		var target, spent, last int64
		if err = tx.QueryRowContext(ctx, "SELECT generation,CASE WHEN date_mode='since' THEN import_start ELSE 0 END FROM archive_policy WHERE id=1").Scan(&policy, &target); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, "SELECT count(*),COALESCE(max(created_at),0) FROM rebuild_requests").Scan(&spent, &last); err != nil {
			return err
		}
		if spent > maxRebuildCopyLimit {
			return ErrReconciliationRequired
		}
		limit := DefaultRebuildCopyLimit
		if spent > limit {
			limit = spent
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO rebuild_operation(id,operation_id,generation,policy_generation,target_start,revision,state,copy_limit,copy_spent,discovery_limit,discovery_spent,last_attempt_at,created_at,updated_at) VALUES(1,?,?,?,?,1,'running',?,?,?,0,?,?,?)", provenance.Generation, provenance.Generation, policy, target, limit, spent, DefaultRebuildDiscoveryLimit, last, unixms(now), unixms(now)); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE rebuild_requests SET operation_id=(SELECT operation_id FROM rebuild_operation WHERE id=1),operation_revision=1 WHERE operation_id='' AND generation=?", provenance.Generation)
	return err
}
func (s *Store) upgradeRebuildOperation(ctx context.Context) error {
	if s.rebuild == nil {
		return nil
	}
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return ErrArchiveUnavailable
	}
	defer tx.Rollback()
	if err = initializeRebuildOperation(ctx, tx, *s.rebuild, s.now()); err != nil {
		return fixedSQLError(err)
	}
	return fixedSQLError(tx.Commit())
}

func loadRebuildOperation(ctx context.Context, q sqlQuerier) (rebuildOperation, error) {
	var o rebuildOperation
	var target, last int64
	err := q.QueryRowContext(ctx, "SELECT operation_id,generation,policy_generation,target_start,revision,state,copy_limit,copy_spent,discovery_limit,discovery_spent,last_attempt_at,last_copy_chat,last_discovery_chat,queue_full FROM rebuild_operation WHERE id=1").Scan(&o.OperationID, &o.Generation, &o.PolicyGeneration, &target, &o.Revision, &o.State, &o.CopyLimit, &o.CopySpent, &o.DiscoveryLimit, &o.DiscoverySpent, &last, &o.copyChat, &o.discoveryChat, &o.QueueFull)
	o.TargetStart = fromMillis(target)
	o.LastAttemptAt = fromMillis(last)
	return o, fixedSQLError(err)
}

// a is a canonical source_anchor, c its stable chat, p the current policy.
// Scalar alias lookup preserves pre-upgrade guard keys without rewriting them.
const rebuildEligibleAnchorSQL = `a.account=? AND a.date_known=1 AND a.original_sent_at>0
 AND (p.chat_mode='all' OR EXISTS(SELECT 1 FROM policy_chats pc WHERE pc.chat_id=c.id))
 AND NOT EXISTS(SELECT 1 FROM source_tombstones t WHERE t.account=a.account AND t.chat=a.chat AND t.sender=a.sender AND t.message=a.message AND t.from_me=a.from_me)
 AND NOT EXISTS(SELECT 1 FROM unresolved_deletions u WHERE u.account=a.account AND u.chat=a.chat AND u.message=a.message)
 AND NOT EXISTS(SELECT 1 FROM chat_tombstones t WHERE t.account=a.account AND t.chat=a.chat AND a.original_sent_at<=t.cutoff)
 AND NOT EXISTS(SELECT 1 FROM rebuild_guards g WHERE g.account=a.account AND g.message=a.message AND g.from_me=a.from_me AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=g.account AND alias=g.chat),g.chat)=a.chat AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=g.account AND alias=g.sender),g.sender)=a.sender AND (g.deleted=1 OR g.view_once=1 OR (g.expires_at>0 AND g.expires_at<=?)))
 AND NOT EXISTS(SELECT 1 FROM messages m WHERE m.account=a.account AND m.source_chat=a.chat AND m.source_sender=a.sender AND m.source_message=a.message AND m.from_me=a.from_me AND (m.state IN('deleted','expired') OR m.view_once=1 OR (m.expires_at>0 AND m.expires_at<=?)))`
const rebuildAnchorTables = `source_anchors a JOIN chats c ON c.account=a.account AND c.source_chat=a.chat JOIN archive_policy p ON p.id=1`
const rebuildUnattemptedCopySQL = ` AND NOT EXISTS(SELECT 1 FROM messages m WHERE m.account=a.account AND m.source_chat=a.chat AND m.source_sender=a.sender AND m.source_message=a.message AND m.from_me=a.from_me AND m.state='active')
 AND NOT EXISTS(SELECT 1 FROM rebuild_requests r WHERE r.account=a.account AND r.message=a.message AND r.from_me=a.from_me AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=r.account AND alias=r.chat),r.chat)=a.chat AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=r.account AND alias=r.sender),r.sender)=a.sender)`

func (s *Store) rebuildAnchorFilter(o rebuildOperation, discovery bool) (string, []any) {
	now := unixms(s.now())
	where := rebuildEligibleAnchorSQL
	args := []any{s.rebuild.Account, now, now}
	where += ` AND (p.date_mode='all_available' OR a.original_sent_at>=p.import_start) AND (p.rolling_retention=0 OR a.original_sent_at>=?-p.rolling_retention/1000000)`
	args = append(args, now)
	where += ` AND NOT EXISTS(SELECT 1 FROM rebuild_candidate_deferrals d WHERE d.account=a.account AND d.message=a.message AND d.from_me=a.from_me AND d.until_time>? AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=d.account AND alias=d.chat),d.chat)=a.chat AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=d.account AND alias=d.sender),d.sender)=a.sender)`
	args = append(args, now)
	if discovery {
		where += ` AND NOT EXISTS(SELECT 1 FROM rebuild_discovery_attempts d WHERE d.account=a.account AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=d.account AND alias=d.chat),d.chat)=a.chat AND d.original_sent_at<=a.original_sent_at)
 AND NOT EXISTS(SELECT 1 FROM source_anchors z WHERE z.account=a.account AND z.chat=a.chat AND z.date_known=1 AND z.original_sent_at>0 AND ((p.date_mode='since' AND z.original_sent_at<=p.import_start) OR (p.rolling_retention>0 AND z.original_sent_at<=?-p.rolling_retention/1000000)))`
		args = append(args, now)
	} else {
		where += rebuildUnattemptedCopySQL
	}
	return where, args
}

// Rotate eligible chats using the indexed account/chat prefix. One candidate
// per chat per window prevents a large first chat monopolizing dispatch.
func (s *Store) nextRebuildAnchors(ctx context.Context, q sqlQuerier, o rebuildOperation, limit int, discovery bool) ([]SourceAnchor, error) {
	if o.State != "running" || (discovery && o.QueueFull) || (discovery && o.DiscoverySpent >= o.DiscoveryLimit) || (!discovery && o.CopySpent >= o.CopyLimit) {
		return nil, nil
	}
	where, args := s.rebuildAnchorFilter(o, discovery)
	cursor := o.copyChat
	if discovery {
		cursor = o.discoveryChat
	}
	var chats []string
	for _, after := range []bool{true, false} {
		if len(chats) >= limit {
			break
		}
		cmp := ">"
		if !after {
			cmp = "<="
		}
		a := append(append([]any{}, args...), cursor, limit-len(chats))
		rows, err := q.QueryContext(ctx, "SELECT DISTINCT a.chat FROM "+rebuildAnchorTables+" WHERE "+where+" AND a.chat"+cmp+"? ORDER BY a.chat LIMIT ?", a...)
		if err != nil {
			return nil, fixedSQLError(err)
		}
		for rows.Next() {
			var chat string
			if err = rows.Scan(&chat); err != nil {
				rows.Close()
				return nil, fixedSQLError(err)
			}
			chats = append(chats, chat)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fixedSQLError(err)
		}
	}
	var out []SourceAnchor
	for _, chat := range chats {
		a := append(append([]any{}, args...), chat)
		var v SourceAnchor
		var sent int64
		err := q.QueryRowContext(ctx, "SELECT a.account,a.chat,a.sender,a.message,a.from_me,a.original_sent_at FROM "+rebuildAnchorTables+" WHERE "+where+" AND a.chat=? ORDER BY a.original_sent_at,a.sender,a.message,a.from_me LIMIT 1", a...).Scan(&v.Key.Account, &v.Key.Chat, &v.Key.Sender, &v.Key.Message, &v.Key.FromMe, &sent)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fixedSQLError(err)
		}
		v.OriginalSentAt = fromMillis(sent)
		out = append(out, v)
	}
	return out, nil
}
func (s *Store) NextRebuildCandidates(ctx context.Context, limit int) ([]SourceAnchor, error) {
	if s.rebuild == nil {
		return nil, nil
	}
	if limit < 1 || limit > 32 {
		return nil, ErrInvalidEvent
	}
	if _, err := s.readSafety(s.rebuild.Account); err != nil {
		return nil, err
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fixedSQLError(err)
	}
	defer tx.Rollback()
	o, err := loadRebuildOperation(ctx, tx)
	if err != nil {
		return nil, err
	}
	return s.nextRebuildAnchors(ctx, tx, o, limit, false)
}
func (s *Store) NextRebuildDiscovery(ctx context.Context) (*RebuildDiscovery, error) {
	if s.rebuild == nil {
		return nil, nil
	}
	if _, err := s.readSafety(s.rebuild.Account); err != nil {
		return nil, err
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fixedSQLError(err)
	}
	defer tx.Rollback()
	o, err := loadRebuildOperation(ctx, tx)
	if err != nil {
		return nil, err
	}
	anchors, err := s.nextRebuildAnchors(ctx, tx, o, 1, true)
	if err != nil || len(anchors) == 0 {
		return nil, err
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	return &RebuildDiscovery{ID: id, OperationID: o.OperationID, Generation: o.Generation, OperationRevision: o.Revision, PolicyGeneration: o.PolicyGeneration, Anchor: anchors[0], CreatedAt: s.now()}, nil
}

func (s *Store) rebuildProgressQuery(ctx context.Context, q sqlQuerier) (*RebuildProgress, error) {
	o, err := loadRebuildOperation(ctx, q)
	if err != nil {
		return nil, err
	}
	where, args := s.rebuildAnchorFilter(o, false)
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM "+rebuildAnchorTables+" WHERE "+where, args...).Scan(&o.Queued); err != nil {
		return nil, fixedSQLError(err)
	}
	var oldest int64
	if err = q.QueryRowContext(ctx, "SELECT count(*),COALESCE(min(original_sent_at),0) FROM source_anchors WHERE account=? AND date_known=1", s.rebuild.Account).Scan(&o.Discovered, &oldest); err != nil {
		return nil, fixedSQLError(err)
	}
	o.OldestObserved = fromMillis(oldest)
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM rebuild_candidate_deferrals WHERE until_time>?", unixms(s.now())).Scan(&o.Deferred); err != nil {
		return nil, fixedSQLError(err)
	}
	if err = q.QueryRowContext(ctx, "SELECT COALESCE(sum(state='pending'),0),COALESCE(sum(state='consumed'),0),COALESCE(sum(state='pending' AND policy_generation<>?),0) FROM rebuild_requests WHERE generation=?", o.PolicyGeneration, o.Generation).Scan(&o.Pending, &o.Accepted, &o.Stale); err != nil {
		return nil, fixedSQLError(err)
	}
	if o.State == "running" {
		discovery, err := s.nextRebuildAnchors(ctx, q, o, 1, true)
		if err != nil {
			return nil, err
		}
		if !(o.Queued > 0 && o.CopySpent < o.CopyLimit) && len(discovery) == 0 {
			switch {
			case (o.Queued > 0 && o.CopySpent >= o.CopyLimit) || o.DiscoverySpent >= o.DiscoveryLimit:
				o.State = "exhausted"
			case o.QueueFull:
				o.State = "queue_full"
			case o.Discovered == 0:
				o.State = "no_anchor"
			case o.Deferred > 0:
				o.State = "waiting_transport"
			default:
				o.State = "waiting_phone"
			}
		}
	}
	p := o.RebuildProgress
	return &p, nil
}
func (s *Store) RebuildProgress(ctx context.Context) (*RebuildProgress, error) {
	if s.rebuild == nil {
		return nil, nil
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fixedSQLError(err)
	}
	defer tx.Rollback()
	return s.rebuildProgressQuery(ctx, tx)
}

// RebuildDispatchState is the scheduler's constant-size ledger read. Owner
// aggregate counts belong to RebuildProgress and never run in the polling loop.
func (s *Store) RebuildDispatchState(ctx context.Context) (*RebuildProgress, error) {
	if s.rebuild == nil {
		return nil, nil
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	o, err := loadRebuildOperation(ctx, s.readDB)
	if err != nil {
		return nil, err
	}
	p := o.RebuildProgress
	return &p, nil
}

type rebuildControlCommand struct{ control RebuildControl }

func (c rebuildControlCommand) priority() bool                    { return true }
func (c rebuildControlCommand) account() string                   { return "" }
func (c rebuildControlCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c rebuildControlCommand) run(s *Store, ctx context.Context) (any, error) {
	var result *RebuildProgress
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		o, err := loadRebuildOperation(ctx, tx)
		if err != nil {
			return err
		}
		var action string
		var expected uint64
		err = tx.QueryRowContext(ctx, "SELECT action,expected_revision FROM rebuild_commands WHERE command_id=?", c.control.CommandID).Scan(&action, &expected)
		if err == nil {
			if action != c.control.Action || expected != c.control.ExpectedRevision {
				return ErrPolicyChanged
			}
			result, err = s.rebuildProgressQuery(ctx, tx)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if o.Revision != c.control.ExpectedRevision {
			return ErrPolicyChanged
		}
		if o.State == "cancelled" {
			return ErrSourceUnavailable
		}
		state := o.State
		copyLimit, discoveryLimit := o.CopyLimit, o.DiscoveryLimit
		switch c.control.Action {
		case "pause":
			state = "paused"
		case "resume":
			state = "running"
		case "cancel":
			state = "cancelled"
		case "extend":
			if copyLimit > maxRebuildCopyLimit-DefaultRebuildCopyLimit || discoveryLimit > maxRebuildDiscoveryLimit-DefaultRebuildDiscoveryLimit {
				return ErrLimitExceeded
			}
			copyLimit += DefaultRebuildCopyLimit
			discoveryLimit += DefaultRebuildDiscoveryLimit
			state = "running"
		default:
			return ErrInvalidEvent
		}
		if _, err = tx.ExecContext(ctx, "UPDATE rebuild_operation SET revision=revision+1,state=?,copy_limit=?,discovery_limit=?,updated_at=? WHERE id=1", state, copyLimit, discoveryLimit, unixms(s.now())); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO rebuild_commands VALUES(?,?,?,?)", c.control.CommandID, c.control.ExpectedRevision, c.control.Action, o.Revision+1); err != nil {
			return err
		}
		result, err = s.rebuildProgressQuery(ctx, tx)
		return err
	})
	return result, err
}
func (s *Store) ControlRebuild(ctx context.Context, c RebuildControl) (*RebuildProgress, error) {
	if s.rebuild == nil {
		return nil, ErrSourceUnavailable
	}
	if !validString(c.CommandID, 128) || c.ExpectedRevision == 0 {
		return nil, ErrInvalidEvent
	}
	switch c.Action {
	case "pause", "resume", "extend", "cancel":
	default:
		return nil, ErrInvalidEvent
	}
	v, err := s.submitExtension(ctx, rebuildControlCommand{c}, 1024)
	if err != nil {
		return nil, err
	}
	return v.(*RebuildProgress), nil
}

func (s *Store) reviseRebuildPolicy(ctx context.Context, tx *sql.Tx, p ArchivePolicy) error {
	if s.rebuild == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, "UPDATE rebuild_operation SET policy_generation=?,target_start=?,revision=revision+1,state=CASE WHEN state='cancelled' THEN state ELSE 'running' END,updated_at=? WHERE id=1", p.Generation, unixms(p.ImportStart), unixms(s.now()))
	return err
}
func checkRebuildDispatch(o rebuildOperation, id string, revision uint64, policy Generation, now time.Time, discovery bool) error {
	if o.OperationID != id || o.Revision != revision || o.PolicyGeneration != policy {
		return ErrPolicyChanged
	}
	if o.State != "running" {
		return ErrSourceUnavailable
	}
	if !o.LastAttemptAt.IsZero() && now.Before(o.LastAttemptAt.Add(rebuildCadence)) {
		return ErrQueueLimitExceeded
	}
	if (discovery && o.QueueFull) || (discovery && o.DiscoverySpent >= o.DiscoveryLimit) || (!discovery && o.CopySpent >= o.CopyLimit) {
		return ErrQueueLimitExceeded
	}
	return nil
}

type rebuildDiscoveryCommand struct{ discovery RebuildDiscovery }

func (c rebuildDiscoveryCommand) priority() bool                    { return false }
func (c rebuildDiscoveryCommand) account() string                   { return c.discovery.Anchor.Key.Account }
func (c rebuildDiscoveryCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c rebuildDiscoveryCommand) run(s *Store, ctx context.Context) (any, error) {
	return nil, s.transaction(ctx, func(tx *sql.Tx) error {
		d := c.discovery
		o, err := loadRebuildOperation(ctx, tx)
		if err != nil {
			return err
		}
		if _, err = s.readSafety(d.Anchor.Key.Account); err != nil {
			return err
		}
		if err = checkRebuildDispatch(o, d.OperationID, d.OperationRevision, d.PolicyGeneration, s.now(), true); err != nil {
			return err
		}
		key, err := canonicalKey(ctx, tx, d.Anchor.Key)
		if err != nil {
			return err
		}
		where, args := s.rebuildAnchorFilter(o, true)
		baseArgs := append([]any{}, args...)
		args = append(args, key.Account, key.Chat, key.Sender, key.Message, key.FromMe, unixms(d.Anchor.OriginalSentAt))
		var eligible bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+rebuildAnchorTables+" WHERE "+where+" AND a.account=? AND a.chat=? AND a.sender=? AND a.message=? AND a.from_me=? AND a.original_sent_at=?)", args...).Scan(&eligible); err != nil {
			return err
		}
		if !eligible {
			return ErrSourceUnavailable
		}
		// Refuse any newer frontier even if a caller skipped the selector.
		var older bool
		olderArgs := append(baseArgs, key.Chat, unixms(d.Anchor.OriginalSentAt))
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+rebuildAnchorTables+" WHERE "+where+" AND a.chat=? AND a.original_sent_at<?)", olderArgs...).Scan(&older); err != nil {
			return err
		}
		if older {
			return ErrSourceUnavailable
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO rebuild_discovery_attempts VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", d.ID, d.OperationID, d.Generation, key.Account, key.Chat, key.Sender, key.Message, key.FromMe, unixms(d.Anchor.OriginalSentAt), d.PolicyGeneration, d.OperationRevision, unixms(s.now())); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE rebuild_operation SET discovery_spent=discovery_spent+1,last_attempt_at=?,last_discovery_chat=?,updated_at=? WHERE id=1", unixms(s.now()), key.Chat, unixms(s.now()))
		return err
	})
}
func (s *Store) RegisterRebuildDiscovery(ctx context.Context, d RebuildDiscovery) error {
	if s.rebuild == nil || d.Generation != s.rebuild.Generation || d.Anchor.Key.Account != s.rebuild.Account || !validKey(d.Anchor.Key) || !validString(d.ID, 128) || d.Anchor.OriginalSentAt.IsZero() {
		return ErrInvalidEvent
	}
	_, err := s.submitExtension(ctx, rebuildDiscoveryCommand{d}, 1024)
	return err
}

func (s *Store) saveRebuildAnchor(ctx context.Context, tx *sql.Tx, key SourceKey, sent int64, known bool) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM source_anchors WHERE account=? AND chat=? AND sender=? AND message=? AND from_me=?)", key.Account, key.Chat, key.Sender, key.Message, key.FromMe).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		var count int64
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM source_anchors").Scan(&count); err != nil {
			return false, err
		}
		if count >= MaxRebuildMetadata {
			_, err := tx.ExecContext(ctx, "UPDATE rebuild_operation SET queue_full=1 WHERE id=1")
			return false, err
		}
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO source_anchors(account,chat,sender,message,from_me,original_sent_at,date_known) VALUES(?,?,?,?,?,?,?) ON CONFLICT(account,chat,sender,message,from_me) DO UPDATE SET original_sent_at=CASE WHEN source_anchors.date_known=0 THEN excluded.original_sent_at ELSE source_anchors.original_sent_at END,date_known=MAX(source_anchors.date_known,excluded.date_known)", key.Account, key.Chat, key.Sender, key.Message, key.FromMe, sent, known)
	return err == nil, err
}

type rebuildDeferralCommand struct{ key SourceKey }

func (c rebuildDeferralCommand) priority() bool                    { return false }
func (c rebuildDeferralCommand) account() string                   { return c.key.Account }
func (c rebuildDeferralCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c rebuildDeferralCommand) run(s *Store, ctx context.Context) (any, error) {
	return nil, s.transaction(ctx, func(tx *sql.Tx) error {
		key, err := canonicalKey(ctx, tx, c.key)
		if err != nil {
			return err
		}
		if _, err = s.readSafety(key.Account); err != nil {
			return err
		}
		var exists bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM source_anchors WHERE account=? AND chat=? AND sender=? AND message=? AND from_me=?)", key.Account, key.Chat, key.Sender, key.Message, key.FromMe).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrSourceUnavailable
		}
		var count int64
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM rebuild_candidate_deferrals").Scan(&count); err != nil {
			return err
		}
		if count >= MaxRebuildMetadata {
			return ErrQueueLimitExceeded
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO rebuild_candidate_deferrals VALUES(?,?,?,?,?,?) ON CONFLICT(account,chat,sender,message,from_me) DO UPDATE SET until_time=excluded.until_time", key.Account, key.Chat, key.Sender, key.Message, key.FromMe, unixms(s.now().Add(10*time.Minute)))
		return err
	})
}

// DeferRebuildCandidate backs off metadata that cannot be prepared for dispatch.
// It neither registers a phone request nor refunds/retries an unknown send.
func (s *Store) DeferRebuildCandidate(ctx context.Context, key SourceKey) error {
	if s.rebuild == nil || !validKey(key) || key.Account != s.rebuild.Account {
		return ErrInvalidEvent
	}
	_, err := s.submitExtension(ctx, rebuildDeferralCommand{key}, 1024)
	return err
}
