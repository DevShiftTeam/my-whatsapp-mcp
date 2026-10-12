package archive

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// DeliveredHistoryRequestInterval is the delivered-mode phone request cadence.
// Selected-original requests retain their existing ten-second cadence.
const DeliveredHistoryRequestInterval = time.Second

// A frontier is an authenticated observation hint, never a response receipt.
// Its exact full key is attempted at most once per strategy/policy generation.
func (s *Store) nextDeliveredAnchor(ctx context.Context, q sqlQuerier, o rebuildOperation, epoch uint64) (*SourceAnchor, error) {
	now := unixms(s.now())
	query := `WITH eligible AS (
 SELECT a.*,row_number() OVER(PARTITION BY a.account,a.chat ORDER BY a.original_sent_at DESC,a.sender DESC,a.message DESC,a.from_me DESC) AS rank
 FROM ` + rebuildAnchorTables + ` WHERE ` + rebuildEligibleAnchorSQL + `
 AND (p.date_mode='all_available' OR a.original_sent_at>=p.import_start)
 AND (p.rolling_retention=0 OR a.original_sent_at>=?-p.rolling_retention/1000000)
 AND NOT EXISTS(SELECT 1 FROM rebuild_candidate_deferrals d WHERE d.account=a.account AND d.chat=a.chat AND d.sender=a.sender AND d.message=a.message AND d.from_me=a.from_me AND d.until_time>?)
 ), candidates AS (
 SELECT a.* FROM eligible a LEFT JOIN rebuild_history_frontiers f ON f.generation=? AND f.strategy_epoch=? AND f.policy_generation=? AND f.account=a.account AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=f.account AND alias=f.chat),f.chat)=a.chat
 WHERE (f.chat IS NULL AND a.rank=1) OR (f.original_sent_at=a.original_sent_at AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=f.account AND alias=f.sender),f.sender)=a.sender AND f.message=a.message AND f.from_me=a.from_me)
 ) SELECT a.account,a.chat,a.sender,a.message,a.from_me,a.original_sent_at FROM candidates a
 WHERE NOT EXISTS(SELECT 1 FROM rebuild_history_pages d WHERE d.generation=? AND d.strategy_epoch=? AND d.policy_generation=? AND d.account=a.account AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=d.account AND alias=d.chat),d.chat)=a.chat AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=d.account AND alias=d.sender),d.sender)=a.sender AND d.message=a.message AND d.from_me=a.from_me)
 ORDER BY a.chat <= (SELECT last_page_chat FROM rebuild_history_selection WHERE id=1),a.chat LIMIT 1`
	var a SourceAnchor
	var sent int64
	err := q.QueryRowContext(ctx, query, s.rebuild.Account, now, now, now, now, o.Generation, epoch, o.PolicyGeneration, o.Generation, epoch, o.PolicyGeneration).Scan(&a.Key.Account, &a.Key.Chat, &a.Key.Sender, &a.Key.Message, &a.Key.FromMe, &sent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fixedSQLError(err)
	}
	a.OriginalSentAt = fromMillis(sent)
	return &a, nil
}
func (s *Store) NextDeliveredHistoryPage(ctx context.Context) (*DeliveredHistoryPage, error) {
	selected := s.historySelection.Load()
	if selected == nil || selected.Policy != RebuildHistoryDelivered {
		return nil, nil
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
	if o.State != "running" || o.QueueFull || o.DiscoverySpent >= o.DiscoveryLimit {
		return nil, nil
	}
	var credits bool
	if err = tx.QueryRowContext(ctx, "SELECT body_spent<body_limit FROM rebuild_history_selection WHERE id=1").Scan(&credits); err != nil {
		return nil, fixedSQLError(err)
	}
	if !credits {
		return nil, nil
	}
	a, err := s.nextDeliveredAnchor(ctx, tx, o, selected.StrategyEpoch)
	if err != nil || a == nil {
		return nil, err
	}
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	return &DeliveredHistoryPage{RebuildDiscovery: RebuildDiscovery{ID: id, OperationID: o.OperationID, OperationRevision: o.Revision, Generation: o.Generation, PolicyGeneration: o.PolicyGeneration, Anchor: *a, CreatedAt: s.now()}, StrategyEpoch: selected.StrategyEpoch}, nil
}

type deliveredPageCommand struct{ page DeliveredHistoryPage }

func (c deliveredPageCommand) priority() bool                    { return false }
func (c deliveredPageCommand) account() string                   { return c.page.Anchor.Key.Account }
func (c deliveredPageCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c deliveredPageCommand) run(s *Store, ctx context.Context) (any, error) {
	return nil, s.transaction(ctx, func(tx *sql.Tx) error {
		d := c.page
		selected := s.historySelection.Load()
		if selected == nil || selected.Policy != RebuildHistoryDelivered || d.StrategyEpoch != selected.StrategyEpoch || d.Generation != selected.ObservationGeneration {
			return ErrPolicyChanged
		}
		if _, err := s.readSafety(d.Anchor.Key.Account); err != nil {
			return err
		}
		o, err := loadRebuildOperation(ctx, tx)
		if err != nil {
			return err
		}
		if err = checkRebuildDispatchInterval(o, d.OperationID, d.OperationRevision, d.PolicyGeneration, s.now(), true, DeliveredHistoryRequestInterval); err != nil {
			return err
		}
		var credits bool
		if err = tx.QueryRowContext(ctx, "SELECT body_spent<body_limit FROM rebuild_history_selection WHERE id=1").Scan(&credits); err != nil {
			return err
		}
		if !credits {
			return ErrLimitExceeded
		}
		d.Anchor.Key, err = canonicalKey(ctx, tx, d.Anchor.Key)
		if err != nil {
			return err
		}
		next, err := s.nextDeliveredAnchor(ctx, tx, o, d.StrategyEpoch)
		if err != nil {
			return err
		}
		if next == nil || next.Key != d.Anchor.Key || !next.OriginalSentAt.Equal(d.Anchor.OriginalSentAt) {
			return ErrPolicyChanged
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO rebuild_history_pages VALUES(?,?,?,?,?,?,?,?,?,?,?)", d.ID, d.Generation, d.StrategyEpoch, d.PolicyGeneration, d.Anchor.Key.Account, d.Anchor.Key.Chat, d.Anchor.Key.Sender, d.Anchor.Key.Message, d.Anchor.Key.FromMe, unixms(d.Anchor.OriginalSentAt), unixms(s.now()))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO rebuild_history_frontiers VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(generation,strategy_epoch,policy_generation,account,chat) DO NOTHING`, d.Generation, d.StrategyEpoch, d.PolicyGeneration, d.Anchor.Key.Account, d.Anchor.Key.Chat, d.Anchor.Key.Sender, d.Anchor.Key.Message, d.Anchor.Key.FromMe, unixms(d.Anchor.OriginalSentAt))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE rebuild_history_selection SET last_page_chat=? WHERE id=1", d.Anchor.Key.Chat); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE rebuild_operation SET discovery_spent=discovery_spent+1,last_attempt_at=?,updated_at=? WHERE id=1", unixms(s.now()), unixms(s.now()))
		return err
	})
}
func (s *Store) RegisterDeliveredHistoryPage(ctx context.Context, page DeliveredHistoryPage) error {
	if s.rebuild == nil || page.Generation != s.rebuild.Generation || page.Anchor.Key.Account != s.rebuild.Account || !validKey(page.Anchor.Key) || !validString(page.ID, 128) || page.Anchor.OriginalSentAt.IsZero() || page.StrategyEpoch == 0 {
		return ErrInvalidEvent
	}
	_, err := s.submitExtension(ctx, deliveredPageCommand{page}, 1024)
	return err
}

type deliveredAnchorCommand struct {
	admission DeliveredHistoryAdmission
	anchor    SourceAnchor
}

func (c deliveredAnchorCommand) priority() bool                    { return false }
func (c deliveredAnchorCommand) account() string                   { return c.anchor.Key.Account }
func (c deliveredAnchorCommand) sensitivity() extensionSensitivity { return extensionBackground }
func (c deliveredAnchorCommand) run(s *Store, ctx context.Context) (any, error) {
	return nil, s.transaction(ctx, func(tx *sql.Tx) error {
		selected := s.historySelection.Load()
		a := c.admission
		k := c.anchor.Key
		if selected == nil || selected.Policy != RebuildHistoryDelivered || a.StrategyEpoch != selected.StrategyEpoch || a.ObservationGeneration != selected.ObservationGeneration {
			return nil
		}
		p, err := s.loadPolicy(ctx, tx)
		if err != nil {
			return err
		}
		if p.Generation != a.PolicyGeneration {
			return nil
		}
		if _, err = s.readSafety(k.Account); err != nil {
			return err
		}
		k, err = canonicalKey(ctx, tx, k)
		if err != nil {
			return err
		}
		g, err := rebuildGuardQuery(ctx, tx, k)
		if err != nil {
			return err
		}
		if g.Deleted || g.ViewOnce || (!g.ExpiresAt.IsZero() && !g.ExpiresAt.After(s.now())) || (!g.ChatClearCutoff.IsZero() && !c.anchor.OriginalSentAt.After(g.ChatClearCutoff)) {
			return nil
		}
		if g.OriginalDateKnown && !g.OriginalSentAt.Equal(c.anchor.OriginalSentAt) {
			return nil
		}
		saved, err := s.saveRebuildAnchor(ctx, tx, k, unixms(c.anchor.OriginalSentAt), true)
		if err != nil || !saved {
			return err
		}
		// Only an existing traversal can advance. Before the first request, the
		// newest valid saved anchor remains the seed regardless of chunk order.
		var old SourceAnchor
		var sent int64
		err = tx.QueryRowContext(ctx, "SELECT sender,message,from_me,original_sent_at FROM rebuild_history_frontiers WHERE generation=? AND strategy_epoch=? AND policy_generation=? AND account=? AND chat=?", a.ObservationGeneration, a.StrategyEpoch, a.PolicyGeneration, k.Account, k.Chat).Scan(&old.Key.Sender, &old.Key.Message, &old.Key.FromMe, &sent)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		incoming := unixms(c.anchor.OriginalSentAt)
		if incoming > sent || incoming == sent && !deliveredKeyLess(k, old.Key) {
			return nil
		}
		_, err = tx.ExecContext(ctx, "UPDATE rebuild_history_frontiers SET sender=?,message=?,from_me=?,original_sent_at=? WHERE generation=? AND strategy_epoch=? AND policy_generation=? AND account=? AND chat=?", k.Sender, k.Message, k.FromMe, incoming, a.ObservationGeneration, a.StrategyEpoch, a.PolicyGeneration, k.Account, k.Chat)
		return err
	})
}
func deliveredKeyLess(a, b SourceKey) bool {
	if a.Sender != b.Sender {
		return a.Sender < b.Sender
	}
	if a.Message != b.Message {
		return a.Message < b.Message
	}
	return !a.FromMe && b.FromMe
}
func (s *Store) ObserveDeliveredHistoryAnchor(ctx context.Context, admission DeliveredHistoryAdmission, anchor SourceAnchor) error {
	if s.rebuild == nil || anchor.Key.Account != s.rebuild.Account || !validKey(anchor.Key) || anchor.OriginalSentAt.IsZero() {
		return ErrInvalidEvent
	}
	_, err := s.submitExtension(ctx, deliveredAnchorCommand{admission, anchor}, 1024)
	return err
}
