package archive

import (
	"context"
	"database/sql"
	"errors"
	"math"
)

type HistoryUncertaintyReason string

const (
	HistoryUncertaintyUnsupportedMetadata HistoryUncertaintyReason = "unsupported_metadata"
	HistoryUncertaintyUnsupportedNotice   HistoryUncertaintyReason = "unsupported_notice"
	HistoryUncertaintyUnsupportedControl  HistoryUncertaintyReason = "unsupported_control"
	historyUncertaintyGapCode                                      = "unsupported_history_semantics"
)

type HistoryUncertaintyCount struct {
	Reason HistoryUncertaintyReason `json:"reason"`
	Count  uint64                   `json:"count"`
}

type HistoryUncertaintyDiagnostics struct {
	Epoch             uint64                    `json:"epoch"`
	TotalObservations uint64                    `json:"total_observations"`
	Reasons           []HistoryUncertaintyCount `json:"reasons"`
}

type historyUncertaintyCommand struct {
	account     string
	reason      HistoryUncertaintyReason
	observation ObservationID
}

const errHistoryObservationChanged Error = "history_observation_changed"

func validHistoryUncertaintyReason(reason HistoryUncertaintyReason) bool {
	return reason == HistoryUncertaintyUnsupportedMetadata || reason == HistoryUncertaintyUnsupportedNotice || reason == HistoryUncertaintyUnsupportedControl
}

// The observation is captured once at the authenticated history boundary.
// A gap can deny certainty, but cannot supply any positive source authority.
func (s *Store) RecordHistoryUncertainty(ctx context.Context, account string, reason HistoryUncertaintyReason, observation ObservationID) error {
	if !validString(account, 512) || !validString(string(observation), 128) || !validHistoryUncertaintyReason(reason) {
		return ErrInvalidEvent
	}
	c := historyUncertaintyCommand{account, reason, observation}
	return s.admitHistoryUncertainty(c, func() error { _, err := s.submit(ctx, c, account, true, 0); return err })
}

// The narrow submit callback makes the lifecycle boundary testable without
// mutable global hooks; production always passes the ordinary writer submit.
func (s *Store) admitHistoryUncertainty(c historyUncertaintyCommand, submit func() error) error {
	account, observation := c.account, c.observation
	// Reject a stale callback before even canceled-context/queue admission can
	// trigger a safety fence for the current observation. The writer repeats
	// this check after queueing, under serialized lifecycle ownership.
	s.safetyMu.RLock()
	matches := s.observation != "" && s.observation == observation && s.observationAccount == account
	s.safetyMu.RUnlock()
	if !matches {
		return errHistoryObservationChanged
	}
	err := submit()
	if err != nil && !errors.Is(err, ErrHistoricalSnapshotReadOnly) && !errors.Is(err, errHistoryObservationChanged) {
		s.denyHistoryUncertainty(c)
	}
	return err
}

// Generic queue failure handlers cannot bind a captured observation. This
// command owns its denial: check and fence the matching lifecycle atomically.
func (s *Store) denyHistoryUncertainty(c historyUncertaintyCommand) {
	s.safetyMu.Lock()
	if s.observation != c.observation || s.observationAccount != c.account {
		s.safetyMu.Unlock()
		return
	}
	s.unsafe[c.account] = true
	s.safetyEpoch++
	s.safetyMu.Unlock()
	// The unsafe flag prevents End/Begin from replacing this observation while
	// its independent durable marker is written.
	_ = s.writeMarker("unsafe.marker", "unsafe\n", false)
}

func (s *Store) recordHistoryUncertainty(ctx context.Context, c historyUncertaintyCommand) error {
	invalidates := true
	return s.transactionWithSafety(ctx, &invalidates, func(tx *sql.Tx) error {
		s.safetyMu.RLock()
		matches := s.observation != "" && s.observation == c.observation && s.observationAccount == c.account
		denied := s.unclean || s.unsafe[c.account]
		s.safetyMu.RUnlock()
		if !matches {
			return errHistoryObservationChanged
		}
		if denied {
			return ErrReconciliationRequired
		}
		var unsafe bool
		if err := tx.QueryRowContext(ctx, "SELECT unsafe FROM account_safety WHERE account=?", c.account).Scan(&unsafe); err != nil {
			return err
		}
		if unsafe {
			return ErrReconciliationRequired
		}
		var epoch, total, count int64
		err := tx.QueryRowContext(ctx, "SELECT epoch,total_observations FROM history_uncertainty_coverage WHERE account=?", c.account).Scan(&epoch, &total)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		err = tx.QueryRowContext(ctx, "SELECT count FROM history_uncertainty_reasons WHERE account=? AND reason=?", c.account, c.reason).Scan(&count)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if epoch == math.MaxInt64 || total == math.MaxInt64 || count == math.MaxInt64 {
			return ErrQueueLimitExceeded
		}
		now := s.now()
		if now.IsZero() || !validReadTime(now) {
			return ErrInvalidEvent
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO history_uncertainty_coverage(account,epoch,total_observations) VALUES(?,1,1) ON CONFLICT(account) DO UPDATE SET epoch=epoch+1,total_observations=total_observations+1", c.account); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO history_uncertainty_reasons(account,reason,count,first_seen,last_seen) VALUES(?,?,1,?,?) ON CONFLICT(account,reason) DO UPDATE SET count=count+1,last_seen=MAX(last_seen,excluded.last_seen)", c.account, c.reason, unixms(now), unixms(now)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO sync_gaps(account,chat,code) VALUES(?,'',?)", c.account, historyUncertaintyGapCode); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO sync_state(account,chat,connection,coverage_unknown) VALUES(?,'','disconnected',1) ON CONFLICT(account,chat) DO UPDATE SET coverage_unknown=1", c.account); err != nil {
			return err
		}
		// Checked combined arithmetic is part of admission, never a later read fault.
		var snapshot ReadSnapshot
		if err = unknownHistoryGapSnapshot(ctx, tx, c.account, &snapshot); err != nil {
			return err
		}
		_, err = bumpRevision(ctx, tx)
		return err
	}, c.account)
}

func (s *Store) HistoryUncertaintyDiagnostics(ctx context.Context, account string) (*HistoryUncertaintyDiagnostics, error) {
	if !validString(account, 512) {
		return nil, ErrInvalidEvent
	}
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	tx, err := s.readDB.BeginTx(bounded, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, readError(bounded, err)
	}
	defer tx.Rollback()
	out := &HistoryUncertaintyDiagnostics{Reasons: []HistoryUncertaintyCount{}}
	var version int
	if err = tx.QueryRowContext(bounded, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		return nil, readError(bounded, err)
	}
	if version >= 14 && version <= 18 {
		return out, nil
	}
	if version != 19 && version != 20 {
		return nil, ErrSchemaChanged
	}
	err = tx.QueryRowContext(bounded, "SELECT epoch,total_observations FROM history_uncertainty_coverage WHERE account=?", account).Scan(&out.Epoch, &out.TotalObservations)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, readError(bounded, err)
	}
	rows, err := tx.QueryContext(bounded, "SELECT reason,count FROM history_uncertainty_reasons WHERE account=? ORDER BY reason LIMIT 4", account)
	if err != nil {
		return nil, readError(bounded, err)
	}
	defer rows.Close()
	for rows.Next() {
		var item HistoryUncertaintyCount
		if err = rows.Scan(&item.Reason, &item.Count); err != nil {
			return nil, readError(bounded, err)
		}
		if !validHistoryUncertaintyReason(item.Reason) || len(out.Reasons) == 3 {
			return nil, ErrReconciliationRequired
		}
		out.Reasons = append(out.Reasons, item)
	}
	if err = rows.Err(); err != nil {
		return nil, readError(bounded, err)
	}
	return out, nil
}
