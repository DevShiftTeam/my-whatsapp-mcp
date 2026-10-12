package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

func (s *Store) loadRebuildProvenance(ctx context.Context) error {
	// Startup already owns the exclusive archive writer. Use its initialized
	// connection for schema discovery before opening any pooled reader.
	q := s.writeDB
	var n int
	if e := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='rebuild_provenance'").Scan(&n); e != nil {
		return ErrArchiveUnavailable
	}
	if n == 0 {
		return nil
	}
	var p RebuildProvenance
	var boundary int64
	var receipt string
	if e := q.QueryRowContext(ctx, "SELECT generation,account,device_id,source_sha256,boundary,fence_reason,fence_epoch,receipt FROM rebuild_provenance WHERE id=1").Scan(&p.Generation, &p.Account, &p.DeviceID, &p.SourceSHA256, &boundary, &p.OriginalFenceReason, &p.OriginalFenceEpoch, &receipt); e != nil {
		return ErrReconciliationRequired
	}
	p.Boundary = fromMillis(boundary)
	var sealed RebuildReceipt
	if json.Unmarshal([]byte(receipt), &sealed) != nil || validateRebuildLineage(sealed) != nil || sealed.RebuildProvenance != p || len(p.Generation) != 32 || !validString(p.Account, 512) || !validString(p.DeviceID, 512) || boundary <= 0 || len(p.SourceSHA256) != 64 {
		return ErrReconciliationRequired
	}
	s.rebuild = &p
	return s.upgradeRebuildOperation(ctx)
}
func (s *Store) RebuildProvenance(ctx context.Context) (*RebuildProvenance, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if s.rebuild == nil {
		return nil, nil
	}
	p := *s.rebuild
	return &p, nil
}
func (s *Store) rebuildStatus() *RebuildStatus {
	if s.rebuild == nil {
		return nil
	}
	return &RebuildStatus{Mode: "live_rebuild", Coverage: "partial", CurrentPhoneState: "unknown"}
}

// Canonical aliases can be learned after bootstrap. Resolve both stored guard
// components at lookup and combine collisions using the strictest constraints.
func rebuildGuardQuery(ctx context.Context, q sqlQuerier, k SourceKey) (RebuildGuard, error) {
	var g RebuildGuard
	var expires, sent int64
	var known, view, deleted bool
	var revision int64
	var barrier, kind string
	rows, e := q.QueryContext(ctx, `SELECT original_sent_at,date_known,expires_at,view_once,protocol_revision,revision_barrier,source_kind,deleted FROM rebuild_guards g WHERE account=? AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=g.account AND alias=g.chat),g.chat)=? AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=g.account AND alias=g.sender),g.sender)=? AND message=? AND from_me=?`, k.Account, k.Chat, k.Sender, k.Message, k.FromMe)
	if e != nil {
		return g, ErrArchiveUnavailable
	}
	for rows.Next() {
		if e = rows.Scan(&sent, &known, &expires, &view, &revision, &barrier, &kind, &deleted); e != nil {
			rows.Close()
			return g, ErrArchiveUnavailable
		}
		g.Deleted = g.Deleted || deleted
		g.ViewOnce = g.ViewOnce || view
		if expires > 0 && (g.ExpiresAt.IsZero() || expires < unixms(g.ExpiresAt)) {
			g.ExpiresAt = fromMillis(expires)
		}
		if known && (!g.OriginalDateKnown || sent < unixms(g.OriginalSentAt)) {
			g.OriginalSentAt = fromMillis(sent)
			g.OriginalDateKnown = true
		}
		if revision >= g.ProtocolRevision {
			g.ProtocolRevision = revision
			g.SourceKind = SourceEventKind(kind)
		}
		if barrier != "" {
			g.RevisionBarrier = barrier
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return g, ErrArchiveUnavailable
	}
	var n int
	e = q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM source_tombstones WHERE account=? AND chat=? AND sender=? AND message=? AND from_me=?)+(SELECT count(*) FROM unresolved_deletions WHERE account=? AND chat=? AND message=?)`, k.Account, k.Chat, k.Sender, k.Message, k.FromMe, k.Account, k.Chat, k.Message).Scan(&n)
	if e != nil {
		return g, ErrArchiveUnavailable
	}
	g.Deleted = g.Deleted || n > 0
	{
		var cutoff int64
		e = q.QueryRowContext(ctx, "SELECT cutoff FROM chat_tombstones WHERE account=? AND chat=?", k.Account, k.Chat).Scan(&cutoff)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return g, ErrArchiveUnavailable
		}
		if e == nil {
			g.ChatClearCutoff = fromMillis(cutoff)
		}
		g.Deleted = g.Deleted || (e == nil && g.OriginalDateKnown && unixms(g.OriginalSentAt) <= cutoff)
	}
	// Controls and stricter security facts observed in this namespace also win.
	v, exists, e := loadSourceRow(ctx, q, k)
	if e != nil {
		return g, ErrArchiveUnavailable
	}
	if exists {
		g.Deleted = g.Deleted || v.state == "deleted" || v.state == "expired"
		g.ViewOnce = g.ViewOnce || v.viewOnce
		if v.expires > 0 && (g.ExpiresAt.IsZero() || v.expires < unixms(g.ExpiresAt)) {
			g.ExpiresAt = fromMillis(v.expires)
		}
		if v.protocol >= g.ProtocolRevision {
			g.ProtocolRevision = v.protocol
			g.SourceKind = SourceEventKind(v.kind)
		}
		if v.barrier != "" {
			g.RevisionBarrier = v.barrier
		}
		if v.known {
			g.OriginalDateKnown = true
			g.OriginalSentAt = fromMillis(v.sent)
		}
	}
	if !g.OriginalDateKnown {
		var anchorSent int64
		var anchorKnown bool
		e = q.QueryRowContext(ctx, "SELECT original_sent_at,date_known FROM source_anchors WHERE account=? AND chat=? AND sender=? AND message=? AND from_me=?", k.Account, k.Chat, k.Sender, k.Message, k.FromMe).Scan(&anchorSent, &anchorKnown)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return g, ErrArchiveUnavailable
		}
		if e == nil && anchorKnown {
			g.OriginalDateKnown = true
			g.OriginalSentAt = fromMillis(anchorSent)
		}
	}
	if g.OriginalDateKnown && !g.ChatClearCutoff.IsZero() && !g.OriginalSentAt.After(g.ChatClearCutoff) {
		g.Deleted = true
	}
	return g, nil
}
func (s *Store) RebuildGuard(ctx context.Context, k SourceKey) (RebuildGuard, error) {
	if !validKey(k) {
		return RebuildGuard{}, ErrInvalidEvent
	}
	if s.rebuild == nil {
		return RebuildGuard{}, nil
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	k, e := canonicalKey(ctx, s.readDB, k)
	if e != nil {
		return RebuildGuard{}, ErrArchiveUnavailable
	}
	return rebuildGuardQuery(ctx, s.readDB, k)
}

type rebuildRequestCommand struct {
	request RebuildRequest
	anchor  *SourceAnchor
}

func (c rebuildRequestCommand) priority() bool { return false }
func (c rebuildRequestCommand) account() string {
	if c.anchor != nil {
		return c.anchor.Key.Account
	}
	return c.request.Key.Account
}
func (c rebuildRequestCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c rebuildRequestCommand) run(s *Store, ctx context.Context) (any, error) {
	full := false
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if s.rebuild == nil {
			return ErrInvalidEvent
		}
		if _, e := s.readSafety(c.account()); e != nil {
			return e
		}
		if c.anchor != nil {
			a := *c.anchor
			k, e := canonicalKey(ctx, tx, a.Key)
			if e != nil {
				return e
			}
			g, e := rebuildGuardQuery(ctx, tx, k)
			if e != nil {
				return e
			}
			if g.Deleted || g.ViewOnce || (!g.ExpiresAt.IsZero() && !g.ExpiresAt.After(s.now())) {
				return nil
			}
			var cutoff int64
			e = tx.QueryRowContext(ctx, "SELECT cutoff FROM chat_tombstones WHERE account=? AND chat=?", k.Account, k.Chat).Scan(&cutoff)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			if e == nil && unixms(a.OriginalSentAt) <= cutoff {
				return nil
			}
			if _, e = ensureChat(ctx, tx, k, 0); e != nil {
				return e
			}
			stored, err := s.saveRebuildAnchor(ctx, tx, k, unixms(a.OriginalSentAt), true)
			if err != nil {
				return err
			}
			if !stored {
				full = true
			}
			return nil
		}
		r := c.request
		p, e := s.loadPolicy(ctx, tx)
		if e != nil {
			return e
		}
		if p.Generation != r.PolicyGeneration {
			return ErrPolicyChanged
		}
		k, e := canonicalKey(ctx, tx, r.Key)
		if e != nil {
			return e
		}
		g, e := rebuildGuardQuery(ctx, tx, k)
		if e != nil {
			return e
		}
		if g.Deleted || g.ViewOnce || (!g.ExpiresAt.IsZero() && !g.ExpiresAt.After(s.now())) {
			return ErrSourceUnavailable
		}
		var chatID string
		if e = tx.QueryRowContext(ctx, "SELECT id FROM chats WHERE account=? AND source_chat=?", k.Account, k.Chat).Scan(&chatID); e != nil {
			return ErrSourceUnavailable
		}
		if !permittedPolicy(p, chatID, unixms(g.OriginalSentAt), g.OriginalDateKnown, s.now()) {
			return ErrSourceUnavailable
		}
		o, e := loadRebuildOperation(ctx, tx)
		if e != nil {
			return e
		}
		if e = checkRebuildDispatch(o, r.OperationID, r.OperationRevision, r.PolicyGeneration, s.now(), false); e != nil {
			return e
		}
		where, args := s.rebuildAnchorFilter(o, false)
		args = append(args, k.Chat, k.Sender, k.Message, k.FromMe)
		var eligible bool
		if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+rebuildAnchorTables+" WHERE "+where+" AND a.chat=? AND a.sender=? AND a.message=? AND a.from_me=?)", args...).Scan(&eligible); e != nil {
			return e
		}
		if !eligible {
			return ErrSourceUnavailable
		}
		var attempted bool
		e = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rebuild_requests x WHERE account=? AND message=? AND from_me=? AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=x.account AND alias=x.chat),x.chat)=? AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=x.account AND alias=x.sender),x.sender)=?)`, k.Account, k.Message, k.FromMe, k.Chat, k.Sender).Scan(&attempted)
		if e != nil {
			return e
		}
		if attempted {
			return ErrSourceUnavailable
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO rebuild_requests(id,generation,account,chat,sender,message,from_me,policy_generation,created_at,state,operation_id,operation_revision) VALUES(?,?,?,?,?,?,?,?,?,'pending',?,?)", r.ID, r.Generation, k.Account, k.Chat, k.Sender, k.Message, k.FromMe, r.PolicyGeneration, unixms(s.now()), r.OperationID, r.OperationRevision)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "UPDATE rebuild_operation SET copy_spent=copy_spent+1,last_attempt_at=?,last_copy_chat=?,updated_at=? WHERE id=1", unixms(s.now()), k.Chat, unixms(s.now()))
		return e
	})
	return full, err
}
func (s *Store) PendingRebuildRequest(ctx context.Context, r RebuildRequest) error {
	if s.rebuild == nil || !validString(r.ID, 128) || !validKey(r.Key) || r.Key.Account != s.rebuild.Account || r.Generation != s.rebuild.Generation || r.CreatedAt.Before(s.rebuild.Boundary) || r.CreatedAt.After(s.now().Add(time.Minute)) || r.PolicyGeneration == 0 {
		return ErrInvalidEvent
	}
	_, e := s.submitExtension(ctx, rebuildRequestCommand{request: r}, 1024)
	return e
}
func (s *Store) RecordRebuildDiscovery(ctx context.Context, a SourceAnchor) error {
	if s.rebuild == nil || !validKey(a.Key) || a.Key.Account != s.rebuild.Account || a.OriginalSentAt.IsZero() || !validReadTime(a.OriginalSentAt) {
		return ErrInvalidEvent
	}
	v, e := s.submitExtension(ctx, rebuildRequestCommand{anchor: &a}, 1024)
	if e != nil {
		return e
	}
	if full, _ := v.(bool); full {
		return ErrQueueLimitExceeded
	}
	return nil
}
func rebuildMatchRequest(ctx context.Context, q sqlQuerier, id string, k SourceKey, generation string, policy Generation) (bool, error) {
	var n int
	e := q.QueryRowContext(ctx, `SELECT count(*) FROM rebuild_requests r WHERE id=? AND generation=? AND account=? AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=r.account AND alias=r.chat),r.chat)=? AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=r.account AND alias=r.sender),r.sender)=? AND message=? AND from_me=? AND policy_generation=? AND state='pending' AND policy_generation=(SELECT generation FROM archive_policy WHERE id=1) AND EXISTS(SELECT 1 FROM rebuild_operation o WHERE o.operation_id=r.operation_id AND o.generation=r.generation AND o.state<>'cancelled' AND o.policy_generation=r.policy_generation)`, id, generation, k.Account, k.Chat, k.Sender, k.Message, k.FromMe, policy).Scan(&n)
	return n == 1, fixedSQLError(e)
}
func (s *Store) MatchRebuildRequest(ctx context.Context, id string, k SourceKey, generation string, policy Generation) (bool, error) {
	if s.rebuild == nil || !validKey(k) || !validString(id, 128) || generation != s.rebuild.Generation {
		return false, nil
	}
	if _, e := s.readSafety(k.Account); e != nil {
		return false, e
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	k, e := canonicalKey(ctx, s.readDB, k)
	if e != nil {
		return false, ErrArchiveUnavailable
	}
	return rebuildMatchRequest(ctx, s.readDB, id, k, generation, policy)
}

// rebuildAdmit runs within the source transaction before any body arguments
// reach SQLite. Negative controls remain authoritative without positive proof.
func (s *Store) rebuildAdmit(ctx context.Context, tx *sql.Tx, e *SourceEvent, policy Generation) (bool, error) {
	if s.rebuild == nil {
		return true, nil
	}
	if e.Key.Account != s.rebuild.Account {
		return false, ErrUnauthorized
	}
	viewOnce := false
	for _, media := range e.Media {
		viewOnce = viewOnce || media.ViewOnce
	}
	if err := s.rebuildRestrict(ctx, tx, e.Key, e.ExpiresAt, viewOnce, policy); err != nil {
		return false, err
	}
	if e.Kind == EventDelete || e.Kind == EventEditInvalidation {
		return true, nil
	}
	g, err := rebuildGuardQuery(ctx, tx, e.Key)
	if err != nil {
		return false, err
	}
	if g.Deleted || g.ViewOnce || (!g.ExpiresAt.IsZero() && !g.ExpiresAt.After(s.now())) {
		return false, nil
	}
	if g.ProtocolRevision > e.SourceRevision || (g.SourceKind == EventEditContent || g.SourceKind == EventEditInvalidation) && e.Kind == EventOriginal || (g.RevisionBarrier != "" && e.SourceRevision <= g.ProtocolRevision) {
		return false, nil
	}
	if g.OriginalDateKnown {
		if e.OriginalDateKnown && unixms(e.OriginalSentAt) != unixms(g.OriginalSentAt) {
			return false, nil
		}
		e.OriginalDateKnown = true
		e.OriginalSentAt = g.OriginalSentAt
	}
	if !g.ExpiresAt.IsZero() && (e.ExpiresAt.IsZero() || e.ExpiresAt.After(g.ExpiresAt)) {
		e.ExpiresAt = g.ExpiresAt
	}
	if e.RebuildRequestID != "" || !e.OriginalDateKnown || e.OriginalSentAt.Before(s.rebuild.Boundary) {
		ok, err := rebuildMatchRequest(ctx, tx, e.RebuildRequestID, e.Key, s.rebuild.Generation, policy)
		if err != nil || !ok {
			return false, err
		}
		_, err = tx.ExecContext(ctx, "UPDATE rebuild_requests SET state='consumed',consumed_at=? WHERE id=? AND state='pending'", unixms(s.now()), e.RebuildRequestID)
		return err == nil, err
	}
	return true, nil
}
