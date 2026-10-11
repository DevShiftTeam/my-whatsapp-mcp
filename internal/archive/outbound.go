package archive

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"
)

const ErrSendForbidden Error = "send_forbidden"
const ErrSendConflict Error = "send_request_conflict"
const ErrSendNotFound Error = "send_request_not_found"

type SendRecord struct {
	RequestID         string
	ChatID            ChatID
	ProtocolID, State string
	AcceptedAt        time.Time
}

// SendPermission is separate from read authority. All includes only targets
// still allowed by the credential and current archive policy.
type SendPermission struct {
	Mode     string   `json:"mode"`
	Chats    []ChatID `json:"chats"`
	Revision uint64   `json:"revision"`
}

type sendCommand struct {
	op                           string
	grant                        CredentialGrant
	chats                        []ChatID
	chat                         ChatID
	requestID, protocolID, state string
	digest                       [32]byte
	accepted                     time.Time
	permissionMode               string
	expectedPermissionRevision   *uint64
}

func (c sendCommand) priority() bool                    { return false }
func (c sendCommand) account() string                   { return c.grant.Scope.Account }
func (c sendCommand) sensitivity() extensionSensitivity { return extensionBackground }

// Permission writes use the existing durable authentication uncertainty fence.
// A timed-out accepted withdrawal must never leave the old authority usable.
type sendPermissionCommand struct{ sendCommand }

func (c sendPermissionCommand) priority() bool                    { return true }
func (c sendPermissionCommand) sensitivity() extensionSensitivity { return extensionCredentialMutation }
func (c sendPermissionCommand) credentialID() CredentialID        { return c.grant.CredentialID }

func (s *Store) SetSendPermission(ctx context.Context, g CredentialGrant, chats []ChatID) error {
	if len(chats) > 100 {
		return ErrInvalidQuery
	}
	ids, err := canonicalChats(chats)
	if err != nil {
		return err
	}
	mode := "selected"
	if len(ids) == 0 {
		mode = "none"
	}
	_, err = s.submitExtension(ctx, sendPermissionCommand{sendCommand{op: "permission", grant: g, chats: ids, permissionMode: mode}}, int64(1024+128*len(ids)))
	return err
}

// SetSendPermissionMode compares the permission's own revision atomically with
// the write. It does not change the credential or its read-policy revision.
func (s *Store) SetSendPermissionMode(ctx context.Context, g CredentialGrant, permission SendPermission, expected uint64) error {
	if expected >= math.MaxInt64 || permission.Revision != 0 || len(permission.Chats) > 100 {
		return ErrInvalidQuery
	}
	if permission.Mode != "none" && permission.Mode != "selected" && permission.Mode != "all" {
		return ErrInvalidQuery
	}
	if permission.Mode == "selected" && len(permission.Chats) == 0 || permission.Mode != "selected" && len(permission.Chats) != 0 {
		return ErrInvalidQuery
	}
	ids, err := canonicalChats(permission.Chats)
	if err != nil || len(ids) != len(permission.Chats) {
		return ErrInvalidQuery
	}
	_, err = s.submitExtension(ctx, sendPermissionCommand{sendCommand{op: "permission", grant: g, chats: ids, permissionMode: permission.Mode, expectedPermissionRevision: &expected}}, int64(1024+128*len(ids)))
	return err
}

func (s *Store) GetSendPermission(ctx context.Context, g CredentialGrant) (SendPermission, error) {
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	connection, err := s.readDB.Conn(bounded)
	if err != nil {
		return SendPermission{}, readError(bounded, err)
	}
	defer connection.Close()
	sample, sampledAt := s.now(), time.Now()
	fenced, stop := context.WithTimeout(bounded, responseDeadline)
	defer stop()
	s.safetyMu.RLock()
	defer s.safetyMu.RUnlock()
	if err = s.checkCredentialGrantQueryAt(fenced, connection, g, sample.Add(time.Since(sampledAt))); err != nil {
		return SendPermission{}, err
	}
	permission, err := getSendPermissionQuery(fenced, connection, g)
	if err == nil {
		err = s.checkCredentialGrantQueryAt(fenced, connection, g, sample.Add(time.Since(sampledAt)))
	}
	return permission, err
}

func getSendPermissionQuery(ctx context.Context, q sqlQuerier, g CredentialGrant) (SendPermission, error) {
	permission := SendPermission{Mode: "none", Chats: []ChatID{}}
	var credentialRevision, revision int64
	err := q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(credential_revision)='integer' THEN credential_revision ELSE 0 END,mode,CASE WHEN typeof(revision)='integer' THEN revision ELSE 0 END FROM send_permission_modes WHERE credential_id=?`, g.CredentialID).Scan(&credentialRevision, &permission.Mode, &revision)
	legacy := errors.Is(err, sql.ErrNoRows)
	if err != nil && !legacy {
		return SendPermission{}, readError(ctx, err)
	}
	if !legacy {
		if revision < 1 || credentialRevision < 1 || permission.Mode != "none" && permission.Mode != "selected" && permission.Mode != "all" {
			return SendPermission{}, ErrArchiveUnavailable
		}
		permission.Revision = uint64(revision)
		if uint64(credentialRevision) != g.CredentialRevision {
			permission.Mode = "none"
			return permission, nil
		}
		if permission.Mode != "selected" {
			return permission, nil
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT chat_id FROM send_permissions WHERE credential_id=? AND credential_revision=? ORDER BY chat_id LIMIT 101`, g.CredentialID, g.CredentialRevision)
	if err != nil {
		return SendPermission{}, readError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var chat ChatID
		if err = rows.Scan(&chat); err != nil || !validReadID(string(chat)) || len(permission.Chats) >= 100 {
			return SendPermission{}, ErrArchiveUnavailable
		}
		permission.Chats = append(permission.Chats, chat)
	}
	if err = rows.Err(); err != nil {
		return SendPermission{}, readError(ctx, err)
	}
	if len(permission.Chats) > 0 {
		permission.Mode = "selected"
	} else if legacy {
		permission.Mode = "none"
	} else {
		return SendPermission{}, ErrArchiveUnavailable
	}
	return permission, nil
}
func (s *Store) SendTarget(ctx context.Context, g CredentialGrant, chat ChatID) (string, error) {
	if !validReadID(string(chat)) {
		return "", ErrInvalidQuery
	}
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	// Wait for the read pool before taking the source/auth commit fence.
	connection, err := s.readDB.Conn(bounded)
	if err != nil {
		return "", readError(bounded, err)
	}
	defer connection.Close()
	sample, sampledAt := s.now(), time.Now()
	fenced, stop := context.WithTimeout(bounded, responseDeadline)
	defer stop()
	s.safetyMu.RLock()
	defer s.safetyMu.RUnlock()
	if err := fenced.Err(); err != nil {
		return "", err
	}
	// Both authority checks share the admitted connection and short fence;
	// the injected clock never runs while source/auth commits are blocked.
	if err := s.checkCredentialGrantQueryAt(fenced, connection, g, sample.Add(time.Since(sampledAt))); err != nil {
		return "", err
	}
	return s.sendTargetQuery(fenced, connection, g, chat, true, sample, sampledAt)
}
func (s *Store) sendTargetQuery(ctx context.Context, q sqlQuerier, g CredentialGrant, chat ChatID, permission bool, sample time.Time, sampledAt time.Time) (string, error) {
	query := `SELECT c.source_chat,(SELECT cr.expires_at FROM credentials cr WHERE cr.id=?) FROM chats c CROSS JOIN archive_policy p JOIN account_safety saf ON saf.account=c.account
 WHERE c.id=? AND c.account=? AND saf.unsafe=0 AND p.id=1 AND p.generation=?
 AND (p.chat_mode='all' OR EXISTS(SELECT 1 FROM policy_chats pc WHERE pc.chat_id=c.id))
 AND EXISTS(SELECT 1 FROM credentials cr WHERE cr.id=? AND cr.account=c.account AND cr.principal=? AND cr.revision=? AND cr.revoked=0 AND (cr.expires_at=0 OR cr.expires_at>?) AND (cr.all_chats=1 OR EXISTS(SELECT 1 FROM credential_chats cc WHERE cc.credential_id=cr.id AND cc.chat_id=c.id)))`
	args := []any{g.CredentialID, chat, g.Scope.Account, g.Scope.PolicyGeneration, g.CredentialID, g.Scope.Principal, g.CredentialRevision, unixms(sample.Add(time.Since(sampledAt)))}
	if permission {
		query += ` AND (EXISTS(SELECT 1 FROM send_permission_modes sm WHERE sm.credential_id=? AND typeof(sm.credential_revision)='integer' AND sm.credential_revision=? AND sm.mode='all' AND typeof(sm.revision)='integer' AND sm.revision>0) OR
 ((NOT EXISTS(SELECT 1 FROM send_permission_modes sm WHERE sm.credential_id=?) OR EXISTS(SELECT 1 FROM send_permission_modes sm WHERE sm.credential_id=? AND typeof(sm.credential_revision)='integer' AND sm.credential_revision=? AND sm.mode='selected' AND typeof(sm.revision)='integer' AND sm.revision>0))
 AND EXISTS(SELECT 1 FROM send_permissions sp WHERE sp.credential_id=? AND sp.credential_revision=? AND sp.chat_id=c.id)))`
		args = append(args, g.CredentialID, g.CredentialRevision, g.CredentialID, g.CredentialID, g.CredentialRevision, g.CredentialID, g.CredentialRevision)
	}
	var jid string
	var expires int64
	err := q.QueryRowContext(ctx, query, args...).Scan(&jid, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrSendForbidden
	}
	if err != nil {
		return "", readError(ctx, err)
	}
	if expires != 0 && !sample.Add(time.Since(sampledAt)).Before(fromMillis(expires)) {
		return "", ErrUnauthorized
	}
	return jid, nil
}
func (s *Store) ClaimSend(ctx context.Context, g CredentialGrant, chat ChatID, id string, digest [32]byte, protocol string) (SendRecord, bool, error) {
	if id == "" || len(id) > 128 || protocol == "" || len(protocol) > 128 || !validReadID(string(chat)) {
		return SendRecord{}, false, ErrInvalidQuery
	}
	v, err := s.submitExtension(ctx, sendCommand{op: "claim", grant: g, chat: chat, requestID: id, digest: digest, protocolID: protocol}, 1024)
	if err != nil {
		return SendRecord{}, false, err
	}
	r := v.(sendClaim)
	return r.record, r.fresh, nil
}

type sendClaim struct {
	record SendRecord
	fresh  bool
}

func (s *Store) FinishSend(ctx context.Context, g CredentialGrant, id, state string, at time.Time) error {
	if id == "" || len(id) > 128 || (state != "accepted" && state != "failed") || state == "accepted" && at.IsZero() {
		return ErrInvalidQuery
	}
	_, err := s.submitExtension(ctx, sendCommand{op: "finish", grant: g, requestID: id, state: state, accepted: at}, 1024)
	return err
}
func (s *Store) SendStatus(ctx context.Context, g CredentialGrant, id string) (SendRecord, error) {
	if id == "" || len(id) > 128 {
		return SendRecord{}, ErrInvalidQuery
	}
	if err := s.CheckCredentialGrant(ctx, g); err != nil {
		return SendRecord{}, err
	}
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	r, _, err := loadSend(bounded, s.readDB, g, id)
	if err == nil {
		_, err = s.SendTarget(bounded, g, r.ChatID)
	}
	return r, err
}
func loadSend(ctx context.Context, q sqlQuerier, g CredentialGrant, id string) (SendRecord, [32]byte, error) {
	r := SendRecord{RequestID: id}
	var digest []byte
	var at int64
	err := q.QueryRowContext(ctx, `SELECT chat_id,digest,protocol_id,state,accepted_at FROM send_requests WHERE credential_id=? AND account=? AND request_id=?`, g.CredentialID, g.Scope.Account, id).Scan(&r.ChatID, &digest, &r.ProtocolID, &r.State, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return r, [32]byte{}, ErrSendNotFound
	}
	if err != nil {
		return r, [32]byte{}, readError(ctx, err)
	}
	var hash [32]byte
	if len(digest) != 32 {
		return r, hash, ErrArchiveUnavailable
	}
	copy(hash[:], digest)
	r.AcceptedAt = fromMillis(at)
	return r, hash, nil
}
func (c sendCommand) run(s *Store, ctx context.Context) (any, error) {
	var result any
	err := s.extensionTransaction(ctx, c.grant.Scope.Account, func(tx *sql.Tx) error {
		if err := s.checkCredentialGrantQuery(ctx, tx, c.grant); err != nil {
			return err
		}
		var err error
		switch c.op {
		case "permission":
			permission, permissionErr := getSendPermissionQuery(ctx, tx, c.grant)
			if permissionErr != nil {
				return permissionErr
			}
			if c.expectedPermissionRevision != nil && permission.Revision != *c.expectedPermissionRevision {
				return ErrSendConflict
			}
			if permission.Revision >= math.MaxInt64 {
				return ErrLimitExceeded
			}
			for _, chat := range c.chats {
				if _, err = s.sendTargetQuery(ctx, tx, c.grant, chat, false, s.now(), time.Now()); err != nil {
					return err
				}
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO send_permission_modes(credential_id,credential_revision,mode,revision) VALUES(?,?,?,?)
 ON CONFLICT(credential_id) DO UPDATE SET credential_revision=excluded.credential_revision,mode=excluded.mode,revision=excluded.revision`, c.grant.CredentialID, c.grant.CredentialRevision, c.permissionMode, permission.Revision+1)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "DELETE FROM send_permissions WHERE credential_id=?", c.grant.CredentialID); err != nil {
				return err
			}
			for _, chat := range c.chats {
				if _, err = tx.ExecContext(ctx, "INSERT INTO send_permissions VALUES(?,?,?)", c.grant.CredentialID, c.grant.CredentialRevision, chat); err != nil {
					return err
				}
			}
		case "claim":
			if _, err = s.sendTargetQuery(ctx, tx, c.grant, c.chat, true, s.now(), time.Now()); err != nil {
				return err
			}
			record, hash, err := loadSend(ctx, tx, c.grant, c.requestID)
			if err == nil {
				if hash != c.digest || record.ChatID != c.chat {
					return ErrSendConflict
				}
				result = sendClaim{record: record}
				return nil
			}
			if !errors.Is(err, ErrSendNotFound) {
				return err
			}
			var count int
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM send_requests").Scan(&count); err != nil {
				return err
			}
			if count >= 100000 {
				return ErrLimitExceeded
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO send_requests(credential_id,request_id,account,chat_id,digest,protocol_id,state) VALUES(?,?,?,?,?,?,'outcome_unknown')`, c.grant.CredentialID, c.requestID, c.grant.Scope.Account, c.chat, c.digest[:], c.protocolID)
			if err != nil {
				return err
			}
			result = sendClaim{SendRecord{RequestID: c.requestID, ChatID: c.chat, ProtocolID: c.protocolID, State: "outcome_unknown"}, true}
		case "finish":
			// The receipt is journaled even if send permission was withdrawn after transmission.
			_, err = tx.ExecContext(ctx, `UPDATE send_requests SET state=?,accepted_at=? WHERE credential_id=? AND request_id=? AND account=? AND state='outcome_unknown'`, c.state, unixms(c.accepted), c.grant.CredentialID, c.requestID, c.grant.Scope.Account)
			return err
		default:
			return ErrInvalidQuery
		}
		return nil
	})
	return result, err
}
