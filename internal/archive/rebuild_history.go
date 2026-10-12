package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

type RebuildHistoryPolicy string

const (
	RebuildHistorySelectedOriginal RebuildHistoryPolicy = "selected-original"
	RebuildHistoryDelivered        RebuildHistoryPolicy = "delivered-history"
)

type RebuildHistorySelectionOptions struct {
	Policy                  RebuildHistoryPolicy
	CommandID               string
	AcceptDeliveredSnapshot bool
}
type RebuildHistorySelection struct {
	Policy                RebuildHistoryPolicy `json:"policy"`
	StrategyEpoch         uint64               `json:"strategy_epoch"`
	ObservationGeneration string               `json:"observation_generation"`
	Account               string               `json:"-"`
}
type DeliveredHistoryAdmission struct {
	StrategyEpoch         uint64
	ObservationGeneration string
	PolicyGeneration      Generation
}
type DeliveredHistoryPage struct {
	RebuildDiscovery
	StrategyEpoch uint64
}

const DefaultDeliveredHistoryLimit int64 = 10000

// SelectRebuildHistoryPolicy is called under the application's startup leases,
// before observation or serving reads. A successful selection is immutable for
// this Store lifetime, including when the caller repeats the startup call.
func (s *Store) SelectRebuildHistoryPolicy(ctx context.Context, options RebuildHistorySelectionOptions) (*RebuildHistorySelection, error) {
	mode := options.Policy
	if mode == "" {
		mode = RebuildHistorySelectedOriginal
	}
	if mode != RebuildHistorySelectedOriginal && mode != RebuildHistoryDelivered || options.AcceptDeliveredSnapshot != (mode == RebuildHistoryDelivered) {
		return nil, ErrInvalidPolicy
	}
	if s.historicalSnapshot {
		return nil, ErrHistoricalSnapshotReadOnly
	}
	if s.rebuild == nil {
		if mode == RebuildHistoryDelivered {
			return nil, ErrInvalidPolicy
		}
		return nil, nil
	}
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.closed {
		return nil, ErrArchiveUnavailable
	}
	if selected := s.historySelection.Load(); selected != nil {
		if selected.Policy != mode {
			return nil, ErrInvalidPolicy
		}
		copy := *selected
		return &copy, nil
	}
	if s.queuedCommands.Load() != 0 {
		return nil, ErrReconciliationRequired
	}
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	if s.observation != "" {
		return nil, ErrReconciliationRequired
	}
	// Unchanged startup verification remains available for owner diagnostics
	// on a fenced archive. Only a mode change requires positive source safety.
	current, err := s.loadHistorySelection(ctx, s.writeDB)
	if err != nil {
		return nil, err
	}
	if current.Policy == mode {
		s.historySelection.Store(current)
		copy := *current
		return &copy, nil
	}
	if _, err := s.readSafety(s.rebuild.Account); err != nil {
		return nil, err
	}
	var selected *RebuildHistorySelection
	invalidates := true
	err = s.transactionWithSafety(ctx, &invalidates, func(tx *sql.Tx) error {
		current, err := s.loadHistorySelection(ctx, tx)
		if err != nil {
			return err
		}
		if current.Policy == mode {
			selected = current
			return nil
		}
		if options.Policy == "" {
			return ErrInvalidPolicy
		}
		if current.StrategyEpoch >= math.MaxInt64 {
			return ErrLimitExceeded
		}
		next := current.StrategyEpoch + 1
		command := options.CommandID
		if command == "" {
			command = s.rebuild.Generation + ":" + string(mode) + ":" + fmt.Sprint(next)
		}
		if !validString(command, 128) {
			return ErrInvalidEvent
		}
		var used int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM rebuild_history_selection_audit WHERE command_id=?", command).Scan(&used); err != nil {
			return err
		}
		if used != 0 {
			return ErrPolicyChanged
		}
		p, err := s.loadPolicy(ctx, tx)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO rebuild_history_selection(id,account,generation,mode,epoch,accepted,body_limit,body_spent) VALUES(1,?,?,?,?,1,?,0) ON CONFLICT(id) DO UPDATE SET mode=excluded.mode,epoch=excluded.epoch`, s.rebuild.Account, s.rebuild.Generation, mode, next, DefaultDeliveredHistoryLimit)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO rebuild_history_selection_audit VALUES(?,?,?,?,?,?,1,?,?)", next, command, s.rebuild.Account, s.rebuild.Generation, current.Policy, mode, p.Generation, unixms(s.now()))
		if err != nil {
			return err
		}
		if _, err = bumpRevision(ctx, tx); err != nil {
			return err
		}
		selected = &RebuildHistorySelection{Policy: mode, StrategyEpoch: next, ObservationGeneration: s.rebuild.Generation, Account: s.rebuild.Account}
		return nil
	}, s.rebuild.Account)
	if err != nil {
		return nil, fixedSQLError(err)
	}
	s.historySelection.Store(selected)
	copy := *selected
	return &copy, nil
}
func (s *Store) loadHistorySelection(ctx context.Context, q sqlQuerier) (*RebuildHistorySelection, error) {
	if s.rebuild == nil {
		return nil, nil
	}
	out := &RebuildHistorySelection{Policy: RebuildHistorySelectedOriginal, ObservationGeneration: s.rebuild.Generation, Account: s.rebuild.Account}
	err := q.QueryRowContext(ctx, "SELECT account,generation,mode,epoch FROM rebuild_history_selection WHERE id=1").Scan(&out.Account, &out.ObservationGeneration, &out.Policy, &out.StrategyEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, fixedSQLError(err)
	}
	if out.Account != s.rebuild.Account || out.ObservationGeneration != s.rebuild.Generation {
		return nil, ErrReconciliationRequired
	}
	return out, nil
}
func (s *Store) RebuildHistorySelection(ctx context.Context) (*RebuildHistorySelection, error) {
	if selected := s.historySelection.Load(); selected != nil {
		copy := *selected
		return &copy, nil
	}
	if s.rebuild == nil {
		return nil, nil
	}
	ctx, cancel := s.readContext(ctx)
	defer cancel()
	return s.loadHistorySelection(ctx, s.readDB)
}
func (s *Store) CaptureDeliveredHistoryAdmission(ctx context.Context) (*DeliveredHistoryAdmission, error) {
	selected := s.historySelection.Load()
	if selected == nil || selected.Policy != RebuildHistoryDelivered {
		return nil, nil
	}
	p, err := s.Policy(ctx)
	if err != nil {
		return nil, err
	}
	return &DeliveredHistoryAdmission{StrategyEpoch: selected.StrategyEpoch, ObservationGeneration: selected.ObservationGeneration, PolicyGeneration: p.Generation}, nil
}
func (s *Store) deliveredHistoryAdmit(ctx context.Context, tx *sql.Tx, e SourceEvent, policy Generation) (bool, error) {
	a := e.DeliveredHistory
	if a == nil || e.RebuildRequestID != "" || !e.OriginalDateKnown || e.Kind != EventOriginal && e.Kind != EventEditContent {
		return false, nil
	}
	selected := s.historySelection.Load()
	if selected == nil || selected.Policy != RebuildHistoryDelivered || selected.StrategyEpoch != a.StrategyEpoch || selected.ObservationGeneration != a.ObservationGeneration || selected.Account != e.Key.Account || a.PolicyGeneration != policy {
		return false, nil
	}
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rebuild_history_selection h JOIN rebuild_operation o ON o.id=h.id WHERE h.account=? AND h.generation=? AND h.mode='delivered-history' AND h.epoch=? AND h.body_spent<h.body_limit AND o.state='running' AND o.policy_generation=?)`, e.Key.Account, a.ObservationGeneration, a.StrategyEpoch, policy).Scan(&eligible)
	return eligible, fixedSQLError(err)
}

const deliveredBodyExistsQuery = `SELECT EXISTS(SELECT 1 FROM rebuild_history_bodies b WHERE b.account=? AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=b.account AND alias=b.chat),b.chat)=? AND COALESCE((SELECT canonical FROM identity_aliases WHERE account=b.account AND alias=b.sender),b.sender)=? AND b.message=? AND b.from_me=? AND b.kind=? AND b.protocol_revision=?)`

func deliveredBodyExists(ctx context.Context, q sqlQuerier, e SourceEvent) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, deliveredBodyExistsQuery, e.Key.Account, e.Key.Chat, e.Key.Sender, e.Key.Message, e.Key.FromMe, e.Kind, e.SourceRevision).Scan(&exists)
	return exists, err
}
func (s *Store) recordDeliveredBody(ctx context.Context, tx *sql.Tx, e SourceEvent) error {
	if e.DeliveredHistory == nil {
		return nil
	}
	exists, err := deliveredBodyExists(ctx, tx, e)
	if err != nil || exists {
		return err
	}
	a := e.DeliveredHistory
	if _, err = tx.ExecContext(ctx, "INSERT INTO rebuild_history_bodies VALUES(?,?,?,?,?,?,?,?,?,?,?)", e.Key.Account, e.Key.Chat, e.Key.Sender, e.Key.Message, e.Key.FromMe, e.Kind, e.SourceRevision, a.ObservationGeneration, a.StrategyEpoch, a.PolicyGeneration, unixms(s.now())); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE rebuild_history_selection SET body_spent=body_spent+1 WHERE id=1 AND body_spent<body_limit")
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrLimitExceeded
	}
	return err
}

func (s *Store) deliveredProgress(ctx context.Context, q sqlQuerier, p *RebuildProgress, counts bool) error {
	selected, err := s.loadHistorySelection(ctx, q)
	if err != nil {
		return err
	}
	p.Strategy, p.StrategyEpoch = selected.Policy, selected.StrategyEpoch
	if selected.StrategyEpoch == 0 {
		return nil
	}
	if err = q.QueryRowContext(ctx, "SELECT body_spent,body_limit FROM rebuild_history_selection WHERE id=1").Scan(&p.DeliveredAccepted, &p.DeliveredLimit); err != nil {
		return err
	}
	if !counts {
		return nil
	}
	var oldest int64
	policy, err := s.loadPolicy(ctx, q)
	if err != nil {
		return err
	}
	now := s.now()
	cutoff := int64(0)
	if policy.RollingRetention > 0 {
		cutoff = ceilMillis(now.Add(-policy.RollingRetention))
	}
	if err = q.QueryRowContext(ctx, `SELECT count(*),COALESCE(min(m.original_sent_at),0) FROM messages m JOIN archive_policy p ON p.id=1 WHERE m.account=? AND m.state='active' AND m.original_date_known=1 AND m.view_once=0 AND (m.expires_at=0 OR m.expires_at>?) AND (p.chat_mode='all' OR EXISTS(SELECT 1 FROM policy_chats pc WHERE pc.chat_id=m.chat_id)) AND (p.date_mode='all_available' OR m.original_sent_at>=p.import_start) AND (p.rolling_retention=0 OR m.original_sent_at>=?)`, s.rebuild.Account, unixms(now), cutoff).Scan(&p.ReadableMessages, &oldest); err != nil {
		return err
	}
	p.DeliveredOldest = fromMillis(oldest)
	if err = q.QueryRowContext(ctx, "SELECT count(*) FROM rebuild_history_pages").Scan(&p.PageAttempts); err != nil {
		return err
	}
	if selected.Policy == RebuildHistoryDelivered {
		o, err := loadRebuildOperation(ctx, q)
		if err != nil {
			return err
		}
		p.State = o.State
		p.PageState = "waiting_phone"
		if o.State == "running" && (p.DeliveredAccepted >= p.DeliveredLimit || o.DiscoverySpent >= o.DiscoveryLimit) {
			p.State = "exhausted"
		}
	}
	return nil
}
