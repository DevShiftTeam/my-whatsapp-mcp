package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
)

const restoredSnapshotCode = "owner_restored_snapshot"

// SnapshotRecoveryReceipt describes explicit acceptance of a committed local
// snapshot. It neither identifies an account nor verifies current phone state.
type SnapshotRecoveryReceipt struct {
	RecoveryEpoch   uint64
	ArchiveRevision ArchiveRevision
	Mode            string
	Code            string
}

type snapshotRecoveryAudit struct {
	epoch, unsafeEpoch, beforeRevision, revision uint64
	account, reason, phase                       string
	observationDigest, unsafeDigest              string
	observationIdentity, unsafeIdentity          string
}

type snapshotMarker struct {
	name, digest, identity string
	info                   os.FileInfo
}

// RestoreCommittedSnapshot is an offline owner operation. The caller must hold
// the application instance lease, with no connector or worker alive. The Store
// adds its archive lifetime lock, excludes admissions, and serializes both
// recovery transactions with the writer. Ordinary startup never calls it.
func (s *Store) RestoreCommittedSnapshot(ctx context.Context, account string) (receipt SnapshotRecoveryReceipt, returnErr error) {
	if s.historicalSnapshot {
		return receipt, ErrHistoricalSnapshotReadOnly
	}
	if account != "" && !validString(account, 512) {
		return SnapshotRecoveryReceipt{}, ErrInvalidEvent
	}
	if err := ctx.Err(); err != nil {
		return SnapshotRecoveryReceipt{}, err
	}
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.closed {
		return SnapshotRecoveryReceipt{}, ErrArchiveUnavailable
	}
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	s.safetyMu.RLock()
	observing := s.observation != ""
	startFence := s.safetyEpoch
	s.safetyMu.RUnlock()
	if observing || len(s.bodies) != 0 || len(s.controls) != 0 || s.queuedCommands.Load() != 0 || s.queuedCount.Load() != 0 || s.queuedBytes.Load() != 0 {
		return SnapshotRecoveryReceipt{}, ErrReconciliationRequired
	}
	root, err := os.OpenRoot(s.privateDir)
	if err != nil {
		return SnapshotRecoveryReceipt{}, ErrUnsafeDataPath
	}
	defer root.Close()
	markers, err := captureSnapshotMarkers(root)
	if err != nil {
		return SnapshotRecoveryReceipt{}, err
	}
	audit, done, err := s.snapshotRecoveryEligibility(ctx, account, markers)
	if err != nil {
		return SnapshotRecoveryReceipt{}, err
	}
	if err = s.validateCommittedSnapshot(ctx); err != nil {
		return SnapshotRecoveryReceipt{}, err
	}
	if done {
		return audit.receipt(), nil
	}
	s.safetyMu.Lock()
	if s.safetyEpoch != startFence || s.observation != "" {
		s.safetyMu.Unlock()
		return SnapshotRecoveryReceipt{}, ErrPolicyChanged
	}
	// Keep every read denied through the durable prepare/unlink/complete protocol.
	s.unclean = true
	s.unsafe[audit.account] = true
	s.safetyEpoch++
	recoveryFence := s.safetyEpoch
	s.safetyMu.Unlock()
	if audit.phase != "prepared" {
		audit, err = s.prepareSnapshotRecovery(ctx, audit, markers)
		if err != nil {
			return SnapshotRecoveryReceipt{}, err
		}
		// The prepare transaction advanced archive_revision under the standard
		// commit fence. Any additional epoch change remains a competing denial.
		recoveryFence++
	}
	completed := false
	defer func() {
		if completed {
			return
		}
		s.safetyMu.Lock()
		defer s.safetyMu.Unlock()
		if s.safetyEpoch == recoveryFence {
			return
		}
		// Offline callers have no competing trusted worker. If that contract is
		// violated nevertheless, preserve a raced immediate denial on every
		// returned fault path, including failed unlink or directory fsync.
		persistCtx, cancel := context.WithTimeout(context.Background(), s.options.ControlDeadline)
		defer cancel()
		_, err := s.writeDB.ExecContext(persistCtx, `UPDATE account_safety SET unsafe=1,reason='reconciliation_required',epoch=epoch+1 WHERE account=? AND unsafe=1 AND epoch=?`, audit.account, audit.unsafeEpoch)
		receipt = SnapshotRecoveryReceipt{}
		returnErr = ErrPolicyChanged
		if err != nil {
			returnErr = ErrArchiveUnavailable
		}
	}()
	if err = s.snapshotRecoveryStage("after_prepare"); err != nil {
		return SnapshotRecoveryReceipt{}, err
	}
	for _, marker := range markers {
		if err = s.snapshotRecoveryStage("before_" + strings.TrimSuffix(marker.name, ".marker") + "_remove"); err != nil {
			return SnapshotRecoveryReceipt{}, err
		}
		if err = removeSnapshotMarker(root, marker); err != nil {
			return SnapshotRecoveryReceipt{}, err
		}
	}
	if err = s.snapshotRecoveryStage("before_directory_sync"); err != nil {
		return SnapshotRecoveryReceipt{}, err
	}
	if err = syncDir(s.privateDir); err != nil {
		return SnapshotRecoveryReceipt{}, err
	}
	if err = s.snapshotRecoveryStage("before_complete"); err != nil {
		return SnapshotRecoveryReceipt{}, err
	}
	// A new witness, including an unpersisted immediate denial, cannot be erased
	// merely because it raced while the writer/admission gates were held.
	s.safetyMu.Lock()
	defer s.safetyMu.Unlock()
	if s.safetyEpoch != recoveryFence || s.observation != "" {
		return SnapshotRecoveryReceipt{}, ErrPolicyChanged
	}
	for _, name := range []string{"observation.marker", "unsafe.marker"} {
		if _, err = root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			return SnapshotRecoveryReceipt{}, ErrReconciliationRequired
		}
	}
	if err = s.completeSnapshotRecovery(ctx, audit); err != nil {
		return SnapshotRecoveryReceipt{}, err
	}
	completed = true
	s.snapshotRecoveryPending.Store(false)
	s.unclean = false
	delete(s.unsafe, audit.account)
	s.safetyEpoch++
	return audit.receipt(), nil
}

func (a snapshotRecoveryAudit) receipt() SnapshotRecoveryReceipt {
	return SnapshotRecoveryReceipt{RecoveryEpoch: a.epoch, ArchiveRevision: ArchiveRevision(a.revision), Mode: "committed_snapshot", Code: restoredSnapshotCode}
}

func (s *Store) snapshotRecoveryStage(stage string) error {
	if s.snapshotRecoveryFault != nil {
		return s.snapshotRecoveryFault(stage)
	}
	return nil
}

func captureSnapshotMarkers(root *os.Root) ([]snapshotMarker, error) {
	var markers []snapshotMarker
	for _, name := range []string{"observation.marker", "unsafe.marker"} {
		info, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, ErrUnsafeDataPath
		}
		f, err := openSnapshotMarker(root, name)
		if err != nil {
			return nil, err
		}
		opened, statErr := f.Stat()
		body, readErr := io.ReadAll(io.LimitReader(f, 34))
		closeErr := f.Close()
		named, nameErr := root.Lstat(name)
		if statErr != nil || readErr != nil || closeErr != nil || nameErr != nil || !os.SameFile(info, opened) || !os.SameFile(opened, named) {
			return nil, ErrUnsafeDataPath
		}
		valid := string(body) == "unsafe\n"
		if name == "observation.marker" {
			valid = len(body) == 33 && body[32] == '\n' && len(strings.Trim(string(body[:32]), "0123456789abcdef")) == 0
		}
		if !valid {
			return nil, ErrReconciliationRequired
		}
		digest := sha256.Sum256(body)
		identity, err := snapshotMarkerFileIdentity(named)
		if err != nil {
			return nil, err
		}
		markers = append(markers, snapshotMarker{name: name, info: named, digest: hex.EncodeToString(digest[:]), identity: identity})
	}
	return markers, nil
}

func removeSnapshotMarker(root *os.Root, marker snapshotMarker) error {
	current, err := captureSnapshotMarkers(root)
	if err != nil {
		return err
	}
	for _, candidate := range current {
		if candidate.name == marker.name {
			if candidate.digest != marker.digest || candidate.identity != marker.identity || !os.SameFile(candidate.info, marker.info) {
				return ErrReconciliationRequired
			}
			if err = root.Remove(marker.name); err != nil {
				return ErrArchiveUnavailable
			}
			return nil
		}
	}
	return ErrReconciliationRequired
}

func markerDigest(markers []snapshotMarker, name string) string {
	for _, marker := range markers {
		if marker.name == name {
			return marker.digest
		}
	}
	return ""
}

func markerIdentity(markers []snapshotMarker, name string) string {
	for _, marker := range markers {
		if marker.name == name {
			return marker.identity
		}
	}
	return ""
}

func (s *Store) snapshotRecoveryEligibility(ctx context.Context, requested string, markers []snapshotMarker) (snapshotRecoveryAudit, bool, error) {
	var a snapshotRecoveryAudit
	var count, unsafe, pins, conflicts int
	if err := s.writeDB.QueryRowContext(ctx, `SELECT count(*),COALESCE(MIN(account),'') FROM accounts`).Scan(&count, &a.account); err != nil {
		return a, false, fixedSQLError(err)
	}
	if count != 1 || !validString(a.account, 512) || (requested != "" && requested != a.account) {
		return a, false, ErrUnauthorized
	}
	if err := s.writeDB.QueryRowContext(ctx, `SELECT unsafe,reason,epoch FROM account_safety WHERE account=?`, a.account).Scan(&unsafe, &a.reason, &a.unsafeEpoch); err != nil {
		return a, false, fixedSQLError(err)
	}
	if err := s.writeDB.QueryRowContext(ctx, `SELECT archive_revision,recovery_epoch FROM archive_meta WHERE id=1`).Scan(&a.beforeRevision, &a.epoch); err != nil {
		return a, false, fixedSQLError(err)
	}
	policy, err := s.loadPolicy(ctx, s.writeDB)
	if err != nil || policy.Generation == 0 {
		return a, false, ErrReconciliationRequired
	}
	if err = s.writeDB.QueryRowContext(ctx, `SELECT count(*) FROM sync_gaps WHERE code='identity_alias_conflict'`).Scan(&conflicts); err != nil {
		return a, false, fixedSQLError(err)
	}
	if conflicts != 0 {
		return a, false, ErrIdentityAliasConflict
	}
	if err = s.writeDB.QueryRowContext(ctx, `SELECT count(*) FROM extension_extraction_jobs WHERE input_pinned=1`).Scan(&pins); err != nil {
		return a, false, fixedSQLError(err)
	}
	if pins != 0 {
		return a, false, ErrReconciliationRequired
	}
	var prior snapshotRecoveryAudit
	err = s.writeDB.QueryRowContext(ctx, `SELECT recovery_epoch,account,prior_unsafe_epoch,prior_unsafe_reason,prior_archive_revision,archive_revision,observation_sha256,unsafe_sha256,observation_file_identity,unsafe_file_identity,phase FROM snapshot_recovery_audit WHERE recovery_epoch=?`, a.epoch).Scan(&prior.epoch, &prior.account, &prior.unsafeEpoch, &prior.reason, &prior.beforeRevision, &prior.revision, &prior.observationDigest, &prior.unsafeDigest, &prior.observationIdentity, &prior.unsafeIdentity, &prior.phase)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return a, false, fixedSQLError(err)
	}
	if err == nil && prior.phase == "prepared" {
		if prior.account != a.account || unsafe != 1 || prior.unsafeEpoch != a.unsafeEpoch || prior.reason != a.reason || prior.revision != a.beforeRevision {
			return a, false, ErrPolicyChanged
		}
		for _, marker := range markers {
			expected := prior.unsafeDigest
			expectedIdentity := prior.unsafeIdentity
			if marker.name == "observation.marker" {
				expected = prior.observationDigest
				expectedIdentity = prior.observationIdentity
			}
			if marker.digest != expected || expected == "" || marker.identity != expectedIdentity || expectedIdentity == "" {
				return a, false, ErrReconciliationRequired
			}
		}
		return prior, false, nil
	}
	if unsafe == 0 && len(markers) == 0 && err == nil && prior.phase == "complete" && prior.account == a.account && a.unsafeEpoch == prior.unsafeEpoch+1 {
		return prior, true, nil
	}
	if unsafe == 0 && markerDigest(markers, "observation.marker") != "" && markerDigest(markers, "unsafe.marker") == "" {
		a.reason = "unclean_shutdown"
	} else if unsafe != 1 || (a.reason != "drain_failed" && a.reason != "unclean_shutdown") {
		return a, false, ErrReconciliationRequired
	}
	return a, false, nil
}

func (s *Store) validateCommittedSnapshot(ctx context.Context) error {
	// Revalidate immutable migration receipts even though Open already checked
	// them. No repair or content reconstruction occurs in this operation.
	if err := migrate(ctx, s.writeDB); err != nil {
		return err
	}
	rows, err := s.writeDB.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return ErrArchiveUnavailable
	}
	count := 0
	for rows.Next() {
		var outcome string
		if err = rows.Scan(&outcome); err != nil || outcome != "ok" {
			rows.Close()
			return ErrArchiveUnavailable
		}
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil || count != 1 {
		return ErrArchiveUnavailable
	}
	rows, err = s.writeDB.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return ErrArchiveUnavailable
	}
	invalid := rows.Next()
	err = rows.Err()
	rows.Close()
	if invalid || err != nil {
		return ErrArchiveUnavailable
	}
	for _, name := range []string{"message_fts", "extension_document_fts", "extension_root_fts"} {
		if _, err = s.writeDB.ExecContext(ctx, `INSERT INTO `+name+`(`+name+`) VALUES('integrity-check')`); err != nil {
			return ErrArchiveUnavailable
		}
	}
	var invalidIndex int
	if err = s.writeDB.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM message_fts f LEFT JOIN messages m ON m.rowid=f.rowid WHERE m.id IS NULL OR m.state<>'active' OR f.normalized_body IS NOT m.normalized_body OR f.normalized_caption IS NOT m.normalized_caption)+
 (SELECT count(*) FROM messages m LEFT JOIN message_fts f ON f.rowid=m.rowid WHERE m.state='active' AND f.rowid IS NULL)+
 (SELECT count(*) FROM extension_document_fts f LEFT JOIN extension_documents d ON d.rowid=f.rowid WHERE d.id IS NULL OR d.embedding_input<>0 OR f.normalized_text IS NOT d.normalized_text)+
 (SELECT count(*) FROM extension_documents d LEFT JOIN extension_document_fts f ON f.rowid=d.rowid WHERE d.embedding_input=0 AND f.rowid IS NULL)+
 (SELECT count(*) FROM extension_root_fts f LEFT JOIN extension_roots r ON r.rowid=f.rowid WHERE r.id IS NULL OR f.root_id IS NOT r.id OR length(CAST(f.text AS BLOB))>1048576)+
 (SELECT count(*) FROM extension_roots r LEFT JOIN extension_root_fts f ON f.rowid=r.rowid WHERE f.rowid IS NULL OR length(CAST(r.text AS BLOB))>65536)`).Scan(&invalidIndex); err != nil || invalidIndex != 0 {
		return ErrArchiveUnavailable
	}
	// Root FTS stores normalized text while roots preserve the source-derived
	// spelling. Stream one bounded root at a time and retain no content in audit.
	rows, err = s.writeDB.QueryContext(ctx, `SELECT r.text,f.text FROM extension_roots r JOIN extension_root_fts f ON f.rowid=r.rowid`)
	if err != nil {
		return ErrArchiveUnavailable
	}
	for rows.Next() {
		var text, normalized string
		if err = rows.Scan(&text, &normalized); err != nil || NormalizeLexical(text) != normalized {
			rows.Close()
			return ErrArchiveUnavailable
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ErrArchiveUnavailable
	}
	return nil
}

func (s *Store) prepareSnapshotRecovery(ctx context.Context, a snapshotRecoveryAudit, markers []snapshotMarker) (snapshotRecoveryAudit, error) {
	result := a
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var unsafe int
		var reason string
		var epoch, revision, recovery uint64
		if err := tx.QueryRowContext(ctx, `SELECT unsafe,reason,epoch FROM account_safety WHERE account=?`, a.account).Scan(&unsafe, &reason, &epoch); err != nil {
			return err
		}
		if epoch != a.unsafeEpoch || (unsafe == 1 && reason != a.reason) || (unsafe == 0 && (a.reason != "unclean_shutdown" || markerDigest(markers, "observation.marker") == "")) {
			return ErrPolicyChanged
		}
		if err := tx.QueryRowContext(ctx, `SELECT archive_revision,recovery_epoch FROM archive_meta WHERE id=1`).Scan(&revision, &recovery); err != nil {
			return err
		}
		if revision != a.beforeRevision || recovery != a.epoch {
			return ErrPolicyChanged
		}
		if unsafe == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE account_safety SET unsafe=1,reason='unclean_shutdown',epoch=epoch+1 WHERE account=? AND unsafe=0 AND epoch=?`, a.account, epoch); err != nil {
				return err
			}
			result.unsafeEpoch++
		}
		result.epoch++
		result.revision = revision + 1
		result.observationDigest = markerDigest(markers, "observation.marker")
		result.unsafeDigest = markerDigest(markers, "unsafe.marker")
		result.observationIdentity = markerIdentity(markers, "observation.marker")
		result.unsafeIdentity = markerIdentity(markers, "unsafe.marker")
		result.phase = "prepared"
		if _, err := tx.ExecContext(ctx, `INSERT INTO snapshot_recovery_audit(recovery_epoch,account,prior_unsafe_epoch,prior_unsafe_reason,prior_archive_revision,archive_revision,observation_sha256,unsafe_sha256,observation_file_identity,unsafe_file_identity,phase,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,'prepared',?)`, result.epoch, result.account, result.unsafeEpoch, result.reason, result.beforeRevision, result.revision, result.observationDigest, result.unsafeDigest, result.observationIdentity, result.unsafeIdentity, unixms(s.now())); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE archive_meta SET archive_revision=?,recovery_epoch=?,recovery_code=? WHERE id=1`, result.revision, result.epoch, restoredSnapshotCode); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO sync_gaps(account,chat,code) VALUES(?,'',?)`, a.account, restoredSnapshotCode); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sync_state(account,chat,connection,coverage_unknown) VALUES(?,'','disconnected',1) ON CONFLICT(account,chat) DO UPDATE SET connection='disconnected',coverage_unknown=1`, a.account); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sync_state SET connection='disconnected',coverage_unknown=1 WHERE account=?`, a.account); err != nil {
			return err
		}
		return revokeSnapshotWorkLeases(ctx, tx, a.account)
	})
	if err == nil {
		s.snapshotRecoveryPending.Store(true)
	}
	return result, err
}

func revokeSnapshotWorkLeases(ctx context.Context, tx *sql.Tx, account string) error {
	for _, statement := range []struct {
		query   string
		bounded bool
	}{
		{`UPDATE extension_original_downloads SET state=CASE WHEN attempts>=? THEN 'failed' ELSE 'pending' END,lease_id='',lease_until=0 WHERE account=? AND state='leased'`, true},
		{`UPDATE extension_extraction_jobs SET state=CASE WHEN attempts>=? THEN 'failed' ELSE 'pending' END,lease_id='',lease_until=0 WHERE account=? AND state='leased' AND input_pinned=0`, true},
		{`UPDATE extension_jobs SET state=CASE WHEN attempts>=? THEN 'failed' ELSE 'pending' END,lease_id='',lease_until=0 WHERE account=? AND state='leased'`, true},
		{`UPDATE extension_media_artifacts SET lease_until=0 WHERE account=? AND state IN ('reserved','staged')`, false},
		{`UPDATE extension_publications SET lease_until=0 WHERE account=? AND complete=0`, false},
		{`UPDATE extension_dirty SET lease_id='',lease_until=0 WHERE account=? AND lease_id<>''`, false},
	} {
		arguments := []any{account}
		if statement.bounded {
			arguments = []any{extensionMaxAttempts, account}
		}
		if _, err := tx.ExecContext(ctx, statement.query, arguments...); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) completeSnapshotRecovery(ctx context.Context, a snapshotRecoveryAudit) error {
	// Restore holds safetyMu through this commit; using transaction would try
	// to acquire that same fence again. Both CAS writes commit together here.
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fixedSQLError(err)
	}
	defer tx.Rollback()
	err = func() error {
		result, err := tx.ExecContext(ctx, `UPDATE account_safety SET unsafe=0,epoch=epoch+1 WHERE account=? AND unsafe=1 AND reason=? AND epoch=? AND EXISTS(SELECT 1 FROM snapshot_recovery_audit r JOIN archive_meta m ON m.id=1 WHERE r.recovery_epoch=? AND r.account=account_safety.account AND r.phase='prepared' AND r.prior_unsafe_epoch=account_safety.epoch AND m.recovery_epoch=r.recovery_epoch AND m.archive_revision=r.archive_revision)`, a.account, a.reason, a.unsafeEpoch, a.epoch)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrPolicyChanged
		}
		result, err = tx.ExecContext(ctx, `UPDATE snapshot_recovery_audit SET phase='complete',completed_at=? WHERE recovery_epoch=? AND account=? AND phase='prepared' AND prior_unsafe_epoch=?`, unixms(s.now()), a.epoch, a.account, a.unsafeEpoch)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrPolicyChanged
		}
		return nil
	}()
	if err != nil {
		return fixedSQLError(err)
	}
	return fixedSQLError(tx.Commit())
}
