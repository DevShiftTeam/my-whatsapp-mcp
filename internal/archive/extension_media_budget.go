package archive

import (
	"context"
	"database/sql"
	"math"
)

type extensionOriginalBudgetCommand struct {
	update *OriginalBudgetUpdate
}

func (c extensionOriginalBudgetCommand) priority() bool  { return false }
func (c extensionOriginalBudgetCommand) account() string { return "" }
func (c extensionOriginalBudgetCommand) sensitivity() extensionSensitivity {
	return extensionBackground
}
func (c extensionOriginalBudgetCommand) run(s *Store, ctx context.Context) (any, error) {
	var budget OriginalBudget
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		budget, err = extensionLoadOriginalBudget(ctx, tx)
		if err != nil {
			return err
		}
		if c.update == nil {
			return nil
		}
		if c.update.ExpectedRevision != budget.Revision {
			return ErrOriginalBudgetChanged
		}
		if c.update.MaximumBytes == budget.MaximumBytes {
			return nil
		}
		if budget.Revision == uint64(math.MaxInt64) {
			return ErrLimitExceeded
		}
		result, err := tx.ExecContext(ctx, `UPDATE extension_original_budget SET maximum_bytes=?,revision=revision+1 WHERE id=1 AND revision=?`, c.update.MaximumBytes, int64(budget.Revision))
		if err != nil {
			return fixedSQLError(err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fixedSQLError(err)
		}
		if affected != 1 {
			return ErrArchiveUnavailable
		}
		budget.MaximumBytes = c.update.MaximumBytes
		budget.Revision++
		budget.Pressure = budget.ChargedBytes >= budget.MaximumBytes
		return nil
	})
	if err != nil {
		return OriginalBudget{}, err
	}
	return budget, nil
}

func extensionLoadOriginalBudget(ctx context.Context, q sqlQuerier) (OriginalBudget, error) {
	var budget OriginalBudget
	var revision int64
	var maximumType, revisionType string
	var rows int
	err := q.QueryRowContext(ctx, `SELECT maximum_bytes,revision,typeof(maximum_bytes),typeof(revision),(SELECT count(*) FROM extension_original_budget) FROM extension_original_budget WHERE id=1`).Scan(&budget.MaximumBytes, &revision, &maximumType, &revisionType, &rows)
	if err != nil {
		return OriginalBudget{}, fixedSQLError(err)
	}
	if rows != 1 || maximumType != "integer" || revisionType != "integer" || budget.MaximumBytes < MinimumOriginalBudgetBytes || budget.MaximumBytes > MaximumOriginalBudgetBytes || revision <= 0 {
		return OriginalBudget{}, ErrArchiveUnavailable
	}
	budget.Revision = uint64(revision)
	var corrupt int
	// SQLite SUM checks signed integer overflow; explicit per-row type and
	// range checks also reject fractional or negative persisted charges.
	err = q.QueryRowContext(ctx, `SELECT coalesce(sum(CASE WHEN cleanup_ack=0 THEN charged_bytes ELSE 0 END),0),coalesce(max(CASE WHEN typeof(budget)!='text' OR typeof(cleanup_ack)!='integer' OR cleanup_ack NOT IN (0,1) OR (cleanup_ack=0 AND (typeof(charged_bytes)!='integer' OR charged_bytes<0 OR typeof(maximum_bytes)!='integer' OR maximum_bytes<=0 OR charged_bytes>maximum_bytes)) THEN 1 ELSE 0 END),0) FROM extension_media_artifacts WHERE budget='originals' OR budget=CAST('originals' AS BLOB)`).Scan(&budget.ChargedBytes, &corrupt)
	if err != nil {
		return OriginalBudget{}, fixedSQLError(err)
	}
	if corrupt != 0 || budget.ChargedBytes < 0 {
		return OriginalBudget{}, ErrArchiveUnavailable
	}
	budget.Pressure = budget.ChargedBytes >= budget.MaximumBytes
	return budget, nil
}

func (s *Store) GetOriginalBudget(ctx context.Context) (OriginalBudget, error) {
	if s.historicalSnapshot {
		bounded, cancel := s.readContext(ctx)
		defer cancel()
		tx, err := s.readDB.BeginTx(bounded, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return OriginalBudget{}, fixedSQLError(err)
		}
		defer tx.Rollback()
		return extensionLoadOriginalBudget(bounded, tx)
	}
	value, err := s.submitExtension(ctx, extensionOriginalBudgetCommand{}, 0)
	if err != nil {
		return OriginalBudget{}, err
	}
	return value.(OriginalBudget), nil
}
func (s *Store) SetOriginalBudget(ctx context.Context, update OriginalBudgetUpdate) (OriginalBudget, error) {
	if update.MaximumBytes < MinimumOriginalBudgetBytes || update.MaximumBytes > MaximumOriginalBudgetBytes || update.ExpectedRevision == 0 || update.ExpectedRevision > uint64(math.MaxInt64) {
		return OriginalBudget{}, ErrInvalidExtension
	}
	value, err := s.submitExtension(ctx, extensionOriginalBudgetCommand{update: &update}, 64)
	if err != nil {
		return OriginalBudget{}, err
	}
	return value.(OriginalBudget), nil
}

var _ OriginalBudgetStore = (*Store)(nil)
