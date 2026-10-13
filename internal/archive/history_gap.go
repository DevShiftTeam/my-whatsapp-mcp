package archive

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"
)

const UnknownHistoryGapCode = "unknown_history_notice"
const MaxUnknownHistoryGapCodes = 64

type UnknownHistoryGapCount struct {
	RawStub   int32     `json:"raw_stub"`
	Count     uint64    `json:"count"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// UnknownHistoryGapDiagnostics is owner-only metadata, never a scoped read DTO.
type UnknownHistoryGapDiagnostics struct {
	OwnerAcceptedShapeGaps uint64                   `json:"owner_accepted_shape_gaps,omitempty"`
	CoverageEpoch          uint64                   `json:"coverage_epoch"`
	TotalObservations      uint64                   `json:"total_observations"`
	Codes                  []UnknownHistoryGapCount `json:"codes"`
}

type unknownHistoryGapCommand struct {
	account string
	rawStub int32
}

func (s *Store) RecordUnknownHistoryGap(ctx context.Context, account string, rawStub int32) error {
	if !validString(account, 512) || rawStub <= 0 {
		return ErrInvalidEvent
	}
	_, err := s.submit(ctx, unknownHistoryGapCommand{account, rawStub}, account, true, 0)
	if err != nil && !errors.Is(err, ErrHistoricalSnapshotReadOnly) {
		s.denyImmediate(account, "control_admission_failed")
	}
	return err
}

func (s *Store) recordUnknownHistoryGap(ctx context.Context, account string, rawStub int32) error {
	invalidates := true
	return s.transactionWithSafety(ctx, &invalidates, func(tx *sql.Tx) error {
		if err := recordUnknownHistoryGapTx(ctx, tx, account, rawStub, s.now()); err != nil {
			return err
		}
		_, err := bumpRevision(ctx, tx)
		return err
	}, account)
}

// The caller owns its transaction and revision bump. Recovery prepare uses this
// same helper while retaining its single existing archive revision increment.
func recordUnknownHistoryGapTx(ctx context.Context, tx *sql.Tx, account string, rawStub int32, now time.Time) error {
	if !validString(account, 512) || rawStub <= 0 || now.IsZero() || !validReadTime(now) {
		return ErrInvalidEvent
	}
	if err := bindAccount(ctx, tx, account); err != nil {
		return err
	}
	var epoch, total int64
	err := tx.QueryRowContext(ctx, "SELECT epoch,total_observations FROM unknown_history_coverage WHERE account=?", account).Scan(&epoch, &total)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if epoch == math.MaxInt64 || total == math.MaxInt64 {
		return ErrQueueLimitExceeded
	}
	var count int64
	err = tx.QueryRowContext(ctx, "SELECT count FROM unknown_history_codes WHERE account=? AND raw_stub=?", account, rawStub).Scan(&count)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		var distinct int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM unknown_history_codes WHERE account=?", account).Scan(&distinct); err != nil {
			return err
		}
		if distinct >= MaxUnknownHistoryGapCodes {
			return ErrQueueLimitExceeded
		}
	}
	if count == math.MaxInt64 {
		return ErrQueueLimitExceeded
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO unknown_history_coverage(account,epoch,total_observations) VALUES(?,1,1) ON CONFLICT(account) DO UPDATE SET epoch=epoch+1,total_observations=total_observations+1", account); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO unknown_history_codes(account,raw_stub,count,first_seen,last_seen) VALUES(?,?,1,?,?) ON CONFLICT(account,raw_stub) DO UPDATE SET count=count+1,last_seen=MAX(last_seen,excluded.last_seen)", account, rawStub, unixms(now), unixms(now)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO sync_gaps(account,chat,code) VALUES(?,'',?)", account, UnknownHistoryGapCode); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO sync_state(account,chat,connection,coverage_unknown) VALUES(?,'','disconnected',1) ON CONFLICT(account,chat) DO UPDATE SET coverage_unknown=1", account)
	return err
}

// Older historical readers were already admitted with exact checked migrations.
// They have no gap tables; never query or create those tables on old snapshots.
func unknownHistoryGapSnapshot(ctx context.Context, q sqlQuerier, account string, snapshot *ReadSnapshot) error {
	var version int
	if err := q.QueryRowContext(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		return readError(ctx, err)
	}
	if version >= 14 && version <= 16 {
		return nil
	}
	if version != 17 && version != 18 && version != 19 {
		return ErrSchemaChanged
	}
	var epoch uint64
	err := q.QueryRowContext(ctx, "SELECT epoch FROM unknown_history_coverage WHERE account=?", account).Scan(&epoch)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return readError(ctx, err)
	}
	_, acceptedEpoch, err := acceptedPinnedHistoryGaps(ctx, q, account)
	if err != nil {
		return err
	}
	if acceptedEpoch > math.MaxUint64-epoch {
		return ErrReconciliationRequired
	}
	snapshot.CoverageEpoch = epoch + acceptedEpoch
	snapshot.UnknownHistoryGap = epoch != 0 || acceptedEpoch != 0
	if version == 19 {
		var runtimeEpoch uint64
		err = q.QueryRowContext(ctx, "SELECT epoch FROM history_uncertainty_coverage WHERE account=?", account).Scan(&runtimeEpoch)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return readError(ctx, err)
		}
		if runtimeEpoch > math.MaxUint64-snapshot.CoverageEpoch {
			return ErrReconciliationRequired
		}
		snapshot.CoverageEpoch += runtimeEpoch
		snapshot.UnknownHistoryGap = snapshot.UnknownHistoryGap || runtimeEpoch != 0
	}
	if version >= 18 {
		var strategy uint64
		err = q.QueryRowContext(ctx, "SELECT epoch FROM rebuild_history_selection WHERE account=?", account).Scan(&strategy)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return readError(ctx, err)
		}
		snapshot.DeliveredHistorySnapshot = strategy != 0
		if strategy > math.MaxUint64-snapshot.CoverageEpoch {
			return ErrReconciliationRequired
		}
		snapshot.CoverageEpoch += strategy
	}
	return nil
}

func (s *Store) UnknownHistoryGapDiagnostics(ctx context.Context, account string) (*UnknownHistoryGapDiagnostics, error) {
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
	snapshot := ReadSnapshot{}
	if err = unknownHistoryGapSnapshot(bounded, tx, account, &snapshot); err != nil {
		return nil, err
	}
	out := &UnknownHistoryGapDiagnostics{CoverageEpoch: snapshot.CoverageEpoch, Codes: []UnknownHistoryGapCount{}}
	if !snapshot.UnknownHistoryGap {
		return out, nil
	}
	out.OwnerAcceptedShapeGaps, _, err = acceptedPinnedHistoryGaps(bounded, tx, account)
	if err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(bounded, "SELECT total_observations FROM unknown_history_coverage WHERE account=?", account).Scan(&out.TotalObservations); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, readError(bounded, err)
	}
	rows, err := tx.QueryContext(bounded, "SELECT raw_stub,count,first_seen,last_seen FROM unknown_history_codes WHERE account=? ORDER BY raw_stub LIMIT ?", account, MaxUnknownHistoryGapCodes+1)
	if err != nil {
		return nil, readError(bounded, err)
	}
	defer rows.Close()
	for rows.Next() {
		var v UnknownHistoryGapCount
		var first, last int64
		if err = rows.Scan(&v.RawStub, &v.Count, &first, &last); err != nil {
			return nil, readError(bounded, err)
		}
		v.FirstSeen = fromMillis(first)
		v.LastSeen = fromMillis(last)
		out.Codes = append(out.Codes, v)
	}
	if err = rows.Err(); err != nil {
		return nil, readError(bounded, err)
	}
	if len(out.Codes) > MaxUnknownHistoryGapCodes {
		return nil, ErrLimitExceeded
	}
	return out, nil
}

// CheckOwnerMetadataSnapshot checks only owner discovery/diagnostic metadata.
// It is not a content authorization check and deliberately permits unsafe state.
func (s *Store) CheckOwnerMetadataSnapshot(ctx context.Context, expected ReadSnapshot) error {
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	// Pool admission precedes the commit fence, as for body response guards.
	conn, err := s.readDB.Conn(bounded)
	if err != nil {
		return readError(bounded, err)
	}
	defer conn.Close()
	fenced, stop := context.WithTimeout(bounded, responseDeadline)
	defer stop()
	s.safetyMu.RLock()
	defer s.safetyMu.RUnlock()
	tx, err := conn.BeginTx(fenced, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return readError(fenced, err)
	}
	defer tx.Rollback()
	var current ReadSnapshot
	err = tx.QueryRowContext(fenced, "SELECT COALESCE((SELECT generation FROM archive_policy WHERE id=1),0),archive_revision,index_generation,recovery_epoch,recovery_code FROM archive_meta WHERE id=1").Scan(&current.PolicyGeneration, &current.ArchiveRevision, &current.IndexGeneration, &current.RecoveryEpoch, &current.RecoveryCode)
	if err != nil {
		return readError(fenced, err)
	}
	var account string
	var accounts int
	if err = tx.QueryRowContext(fenced, "SELECT count(*),COALESCE(min(account),'') FROM accounts").Scan(&accounts, &account); err != nil {
		return readError(fenced, err)
	}
	if accounts > 1 {
		return ErrUnauthorized
	}
	if err = unknownHistoryGapSnapshot(fenced, tx, account, &current); err != nil {
		return err
	}
	if current != expected {
		return ErrCursorStale
	}
	return readError(fenced, tx.Commit())
}
