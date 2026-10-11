package archive

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
)

func validUnsafeReason(r UnsafeReason) bool {
	switch r {
	case "reconciliation_required", "control_admission_failed", "queue_limit_exceeded", "mutation_metadata_missing", "identity_alias_conflict", "unsafe_mutation_projection", "history_limit_exceeded", "history_corrupt", "drain_failed", "source_revision_conflict", "unclean_shutdown":
		return true
	case "app_state_sync_error", "undecryptable_message", "ambiguous_message_structure", "ambiguous_edit_structure":
		return true
	}
	return false
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return ErrArchiveUnavailable
	}
	defer f.Close()
	if e = f.Sync(); e != nil {
		return ErrArchiveUnavailable
	}
	return nil
}
func (s *Store) writeMarker(name, content string, exclusive bool) error {
	path := filepath.Join(s.privateDir, name)
	if e := checkPrivateFile(path); e != nil {
		return e
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	f, e := os.OpenFile(path, flags, 0600)
	if errors.Is(e, os.ErrExist) && !exclusive {
		return syncDir(s.privateDir)
	}
	if e != nil {
		return ErrArchiveUnavailable
	}
	if _, e = f.WriteString(content); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil || ce != nil {
		return ErrArchiveUnavailable
	}
	return syncDir(s.privateDir)
}
func (s *Store) denyImmediate(account string, reason UnsafeReason) error {
	s.safetyMu.Lock()
	if account == "" {
		s.unclean = true
	} else {
		s.unsafe[account] = true
	}
	s.safetyEpoch++
	s.safetyMu.Unlock()
	// This independent fsynced marker survives SQLite errors and a full queue.
	return s.writeMarker("unsafe.marker", "unsafe\n", false)
}
func (s *Store) MarkUnsafe(ctx context.Context, account string, reason UnsafeReason) error {
	if (account != "" && !validString(account, 512)) || !validUnsafeReason(reason) {
		return ErrInvalidEvent
	}
	markerErr := s.denyImmediate(account, reason)
	_, e := s.submit(ctx, unsafeCommand{account, reason}, account, true, 0)
	if markerErr != nil {
		return markerErr
	}
	return e
}
func (s *Store) persistUnsafe(ctx context.Context, account string, reason UnsafeReason) error {
	if account == "" {
		return nil
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if e := bindAccount(ctx, tx, account); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "UPDATE account_safety SET unsafe=1,reason=?,epoch=epoch+1 WHERE account=?", reason, account)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO sync_gaps(account,chat,code) VALUES(?,'',?)", account, reason)
		return e
	})
}
func (s *Store) BeginObservation(ctx context.Context, account string) (ObservationID, error) {
	if account != "" && !validString(account, 512) {
		return "", ErrInvalidEvent
	}
	v, e := s.submit(ctx, observationCommand{account: account, begin: true}, account, true, 0)
	if e != nil {
		return "", e
	}
	return v.(ObservationID), nil
}
func (s *Store) EndObservation(ctx context.Context, id ObservationID) error {
	if !validString(string(id), 128) {
		return ErrInvalidEvent
	}
	_, e := s.submit(ctx, observationCommand{id: id}, "", true, 0)
	return e
}
func (s *Store) beginObservation(ctx context.Context, account string) (ObservationID, error) {
	s.safetyMu.RLock()
	denied := s.unclean || s.observation != ""
	for _, v := range s.unsafe {
		denied = denied || v
	}
	s.safetyMu.RUnlock()
	if denied {
		return "", ErrReconciliationRequired
	}
	id, e := randomID()
	if e != nil {
		return "", e
	}
	if e = s.writeMarker("observation.marker", id+"\n", true); e != nil {
		s.denyImmediate(account, "unclean_shutdown")
		return "", ErrReconciliationRequired
	}
	if account != "" {
		if e = s.transaction(ctx, func(tx *sql.Tx) error { return bindAccount(ctx, tx, account) }); e != nil {
			s.denyImmediate(account, "unclean_shutdown")
			return "", e
		}
	}
	s.safetyMu.Lock()
	s.observation = ObservationID(id)
	s.observationAccount = account
	s.safetyMu.Unlock()
	return ObservationID(id), nil
}
func (s *Store) endObservation(ctx context.Context, id ObservationID) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	s.safetyMu.RLock()
	owned := s.observation == id
	account := s.observationAccount
	denied := s.unclean
	for _, v := range s.unsafe {
		denied = denied || v
	}
	s.safetyMu.RUnlock()
	if !owned || denied {
		return ErrReconciliationRequired
	}
	if len(s.controls) > 0 || len(s.bodies) > 0 || s.queuedBytes.Load() != 0 {
		return ErrArchiveUnavailable
	}
	path := filepath.Join(s.privateDir, "observation.marker")
	if e := checkPrivateFile(path); e != nil {
		s.denyImmediate(account, "unclean_shutdown")
		return e
	}
	content, e := os.ReadFile(path)
	if e != nil || string(content) != string(id)+"\n" {
		s.denyImmediate(account, "unclean_shutdown")
		return ErrReconciliationRequired
	}
	if e = os.Remove(path); e != nil {
		s.denyImmediate(account, "drain_failed")
		return ErrArchiveUnavailable
	}
	if e = syncDir(s.privateDir); e != nil {
		s.denyImmediate(account, "drain_failed")
		return e
	}
	s.safetyMu.Lock()
	s.observation = ""
	s.observationAccount = ""
	s.safetyMu.Unlock()
	return nil
}
