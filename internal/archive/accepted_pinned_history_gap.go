package archive

import (
	"context"
	"database/sql"
	"encoding/json"
)

const ownerAcceptedHistoryGapCode = "owner_accepted_history_gap"

func acceptedPartialAuditWitness(encoded string) (ClassifiedHistoryWitness, error) {
	var w ClassifiedHistoryWitness
	if len(encoded) > 8192 || json.Unmarshal([]byte(encoded), &w) != nil {
		return w, ErrReconciliationRequired
	}
	canonical, _, err := canonicalAcceptedPartialHistoryWitness(w)
	if err != nil || canonical != encoded {
		return w, ErrReconciliationRequired
	}
	return w, nil
}

// These owner-accepted shape gaps are not observations of undeclared raw codes.
// The immutable audit supplies a separate monotone response-guard component.
func acceptedPinnedHistoryGaps(ctx context.Context, q sqlQuerier, account string) (count, epoch uint64, err error) {
	rows, err := q.QueryContext(ctx, "SELECT recovery_epoch,witness_json,witness_sha256 FROM snapshot_recovery_audit WHERE account=? AND classification=? AND phase IN ('prepared','complete') ORDER BY recovery_epoch", account, AcceptedPartialHistoryGap)
	if err != nil {
		return 0, 0, fixedSQLError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var current uint64
		var encoded, digest string
		if err = rows.Scan(&current, &encoded, &digest); err != nil {
			return 0, 0, fixedSQLError(err)
		}
		w, e := acceptedPartialAuditWitness(encoded)
		if e != nil {
			return 0, 0, e
		}
		_, expectedDigest, e := canonicalAcceptedPartialHistoryWitness(w)
		if e != nil || digest != expectedDigest || current != w.RecoveryEpoch+1 {
			return 0, 0, ErrReconciliationRequired
		}
		if acceptedPartialHistoryCategory(w.Diagnostic, w.RuntimeBuildSHA) == acceptedHistoryUnresolvedShape {
			count++
			epoch = current
		}
	}
	return count, epoch, fixedSQLError(rows.Err())
}

func recordAcceptedPartialHistoryGap(ctx context.Context, tx *sql.Tx, a snapshotRecoveryAudit, s *Store) error {
	w, err := acceptedPartialAuditWitness(a.witnessJSON)
	if err != nil {
		return err
	}
	if acceptedPartialHistoryCategory(w.Diagnostic, w.RuntimeBuildSHA) == acceptedHistoryUndeclared {
		return recordUnknownHistoryGapTx(ctx, tx, a.account, 229, s.now())
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO sync_gaps(account,chat,code) VALUES(?,'',?)", a.account, ownerAcceptedHistoryGapCode); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO sync_state(account,chat,connection,coverage_unknown) VALUES(?,'','disconnected',1) ON CONFLICT(account,chat) DO UPDATE SET coverage_unknown=1", a.account)
	return err
}

func requireAcceptedPartialWitnessGap(ctx context.Context, q sqlQuerier, account string, w ClassifiedHistoryWitness) error {
	if _, _, err := canonicalAcceptedPartialHistoryWitness(w); err != nil {
		return err
	}
	if acceptedPartialHistoryCategory(w.Diagnostic, w.RuntimeBuildSHA) == acceptedHistoryUndeclared {
		return requireAcceptedPartialHistoryGap(ctx, q, account)
	}
	var present bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sync_gaps WHERE account=? AND chat='' AND code=?) AND EXISTS(SELECT 1 FROM sync_state WHERE account=? AND chat='' AND coverage_unknown=1)", account, ownerAcceptedHistoryGapCode, account).Scan(&present)
	if err != nil {
		return fixedSQLError(err)
	}
	if !present {
		return ErrReconciliationRequired
	}
	return nil
}
