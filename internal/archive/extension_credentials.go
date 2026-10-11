package archive

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	maxCredentials          = 1024
	maxCredentialLabel      = 128
	maxCredentialPrincipal  = 256
	maxCredentialFenceBytes = 64 << 10
	credentialFenceMarker   = "credentials-denied.marker"
	defaultCredentialPage   = 32
	maxCredentialPage       = 64
)

// Credential uncertainty is an authentication fence, independent of source
// reconciliation. The lock order is safetyMu, then credentialFence.mu.
type credentialFenceState struct {
	mu        sync.RWMutex
	epoch     uint64
	denied    map[CredentialID]bool
	allDenied bool
}

type createCredentialCommand struct{ request CredentialCreate }

func (c createCredentialCommand) run(s *Store, ctx context.Context) (any, error) {
	return s.createCredential(ctx, c.request)
}
func (c createCredentialCommand) priority() bool                    { return false }
func (c createCredentialCommand) account() string                   { return c.request.Policy.Account }
func (c createCredentialCommand) sensitivity() extensionSensitivity { return extensionBackground }

// Recovery shares Create's FIFO body lane; a priority read could overtake an
// accepted creation whose caller no longer knows whether persistence won.
type listCredentialsCommand struct{ request CredentialListRequest }

func (c listCredentialsCommand) run(s *Store, ctx context.Context) (any, error) {
	return s.listCredentials(ctx, c.request)
}
func (listCredentialsCommand) priority() bool                    { return false }
func (c listCredentialsCommand) account() string                 { return c.request.Account }
func (listCredentialsCommand) sensitivity() extensionSensitivity { return extensionBackground }

type setCredentialPolicyCommand struct {
	id       CredentialID
	policy   CredentialPolicy
	expected uint64
}

func (c setCredentialPolicyCommand) run(s *Store, ctx context.Context) (any, error) {
	return s.setCredentialPolicy(ctx, c.id, c.policy, c.expected)
}
func (c setCredentialPolicyCommand) priority() bool  { return true }
func (c setCredentialPolicyCommand) account() string { return c.policy.Account }
func (c setCredentialPolicyCommand) sensitivity() extensionSensitivity {
	return extensionCredentialMutation
}
func (c setCredentialPolicyCommand) credentialID() CredentialID { return c.id }

type revokeCredentialCommand struct {
	id       CredentialID
	expected uint64
}

func (c revokeCredentialCommand) run(s *Store, ctx context.Context) (any, error) {
	return nil, s.revokeCredential(ctx, c.id, c.expected)
}
func (c revokeCredentialCommand) priority() bool  { return true }
func (c revokeCredentialCommand) account() string { return "" }
func (c revokeCredentialCommand) sensitivity() extensionSensitivity {
	return extensionCredentialMutation
}
func (c revokeCredentialCommand) credentialID() CredentialID { return c.id }

func validCredentialID(id CredentialID) bool {
	if len(id) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(string(id))
	return err == nil && len(decoded) == 16 && strings.ToLower(string(id)) == string(id)
}

func validCredentialText(value string, limit int) bool {
	if !validString(value, limit) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func normalizeCredentialPolicy(policy CredentialPolicy) (CredentialPolicy, error) {
	if !validString(policy.Account, 512) || policy.Rolling < 0 || !validReadTime(policy.From) || !validReadTime(policy.Until) || (policy.AllChats && len(policy.Chats) != 0) {
		return CredentialPolicy{}, ErrInvalidExtension
	}
	if !policy.From.IsZero() && !policy.Until.IsZero() && !policy.From.Before(policy.Until) {
		return CredentialPolicy{}, ErrInvalidExtension
	}
	chats, err := canonicalChats(policy.Chats)
	if err != nil {
		return CredentialPolicy{}, ErrInvalidExtension
	}
	policy.Chats = chats
	// Source timestamps are milliseconds. Ceiling both interval bounds preserves
	// the exact inclusive/exclusive permission on those source timestamps.
	if !policy.From.IsZero() {
		if ceilMillis(policy.From) == 0 {
			return CredentialPolicy{}, ErrInvalidExtension
		}
		policy.From = fromMillis(ceilMillis(policy.From))
	}
	if !policy.Until.IsZero() {
		if ceilMillis(policy.Until) == 0 {
			return CredentialPolicy{}, ErrInvalidExtension
		}
		policy.Until = fromMillis(ceilMillis(policy.Until))
	}
	if !policy.From.IsZero() && !policy.Until.IsZero() && !policy.From.Before(policy.Until) {
		return CredentialPolicy{}, ErrInvalidExtension
	}
	if !validReadTime(policy.From) || !validReadTime(policy.Until) {
		return CredentialPolicy{}, ErrInvalidExtension
	}
	return policy, nil
}

func validateCredentialPolicyQuery(ctx context.Context, q sqlQuerier, policy CredentialPolicy) error {
	var count int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM accounts WHERE account=?", policy.Account).Scan(&count); err != nil {
		return readError(ctx, err)
	}
	if count != 1 {
		return ErrInvalidExtension
	}
	for _, id := range policy.Chats {
		if err := q.QueryRowContext(ctx, "SELECT count(*) FROM chats WHERE id=? AND account=?", id, policy.Account).Scan(&count); err != nil {
			return readError(ctx, err)
		}
		if count != 1 {
			return ErrInvalidExtension
		}
	}
	return nil
}

func credentialPolicyBytes(policy CredentialPolicy) int64 {
	bytes := int64(64 + len(policy.Account))
	for _, id := range policy.Chats {
		bytes += int64(16 + len(id))
	}
	return bytes
}

func (s *Store) CreateCredential(ctx context.Context, request CredentialCreate) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	policy, err := normalizeCredentialPolicy(request.Policy)
	if err != nil {
		return Credential{}, err
	}
	if !validCredentialText(request.Label, maxCredentialLabel) || !validCredentialText(request.Principal, maxCredentialPrincipal) || request.Digest == ([32]byte{}) || !validReadTime(request.ExpiresAt) {
		return Credential{}, ErrInvalidExtension
	}
	request.Policy = policy
	if !request.ExpiresAt.IsZero() {
		if unixms(request.ExpiresAt) == 0 {
			return Credential{}, ErrInvalidExtension
		}
		request.ExpiresAt = fromMillis(unixms(request.ExpiresAt))
	}
	bytes := credentialPolicyBytes(policy) + int64(128+len(request.Label)+len(request.Principal))
	value, err := s.submitExtension(ctx, createCredentialCommand{request}, bytes)
	if err != nil {
		return Credential{}, err
	}
	return value.(Credential), nil
}

func (s *Store) ListCredentials(ctx context.Context, request CredentialListRequest) (CredentialMetadataPage, error) {
	if err := ctx.Err(); err != nil {
		return CredentialMetadataPage{}, err
	}
	if !validString(request.Account, 512) || (request.After != "" && !validCredentialID(request.After)) {
		return CredentialMetadataPage{}, ErrInvalidExtension
	}
	switch request.Namespace {
	case 0:
		if !validCredentialText(request.Principal, maxCredentialPrincipal) {
			return CredentialMetadataPage{}, ErrInvalidExtension
		}
	case CredentialNamespaceNativeClients, CredentialNamespaceRemoteClients:
		if request.Principal != "" {
			return CredentialMetadataPage{}, ErrInvalidExtension
		}
	default:
		return CredentialMetadataPage{}, ErrInvalidExtension
	}
	if request.Limit == 0 {
		request.Limit = defaultCredentialPage
	}
	if request.Limit < 1 {
		return CredentialMetadataPage{}, ErrInvalidExtension
	}
	if request.Limit > maxCredentialPage {
		return CredentialMetadataPage{}, ErrLimitExceeded
	}
	bytes := int64(128 + len(request.Account) + len(request.Principal) + len(request.After))
	value, err := s.submitExtension(ctx, listCredentialsCommand{request}, bytes)
	if err != nil {
		return CredentialMetadataPage{}, err
	}
	return value.(CredentialMetadataPage), nil
}

func (s *Store) listCredentials(ctx context.Context, request CredentialListRequest) (CredentialMetadataPage, error) {
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	query := "SELECT id,account,principal,revision,created_at,expires_at,revoked FROM credentials WHERE account=? AND id>?"
	args := []any{request.Account, request.After}
	if request.Namespace == CredentialNamespaceNativeClients {
		// This is the whole closed reserved family, including malformed/future
		// suffixes and the one exact legacy principal. No caller chooses a prefix.
		query += " AND (substr(principal,1,?)=? OR principal=?)"
		args = append(args, len(NativeClientPrincipalPrefix), NativeClientPrincipalPrefix, LegacyNativeClientPrincipal)
	} else if request.Namespace == CredentialNamespaceRemoteClients {
		// Remote recovery is a separate whole reserved family. Native and its
		// legacy principal remain outside this metadata-only census.
		query += " AND substr(principal,1,?)=?"
		args = append(args, len(RemoteClientPrincipalPrefix), RemoteClientPrincipalPrefix)
	} else {
		query += " AND principal=?"
		args = append(args, request.Principal)
	}
	query += " ORDER BY id LIMIT ?"
	args = append(args, request.Limit+1)
	// The existing writer owns this SELECT solely to preserve Create ordering.
	// It reads metadata, never authority or bodies, even when source/auth is denied.
	rows, err := s.writeDB.QueryContext(bounded, query, args...)
	if err != nil {
		return CredentialMetadataPage{}, readError(bounded, err)
	}
	defer rows.Close()
	page := CredentialMetadataPage{Items: []CredentialMetadata{}}
	for rows.Next() {
		var metadata CredentialMetadata
		var created, expires int64
		if err = rows.Scan(&metadata.ID, &metadata.Account, &metadata.Principal, &metadata.Revision, &created, &expires, &metadata.Revoked); err != nil {
			return CredentialMetadataPage{}, readError(bounded, err)
		}
		metadata.CreatedAt, metadata.ExpiresAt = fromMillis(created), fromMillis(expires)
		page.Items = append(page.Items, metadata)
	}
	if err = rows.Err(); err != nil {
		return CredentialMetadataPage{}, readError(bounded, err)
	}
	if len(page.Items) > request.Limit {
		page.Items = page.Items[:request.Limit]
		page.NextAfter = page.Items[len(page.Items)-1].ID
	}
	return page, nil
}

func (s *Store) createCredential(ctx context.Context, request CredentialCreate) (Credential, error) {
	s.credentialFence.mu.RLock()
	denied := s.credentialFence.allDenied
	s.credentialFence.mu.RUnlock()
	if denied {
		return Credential{}, ErrUnauthorized
	}
	now := s.now()
	if !request.ExpiresAt.IsZero() && !now.Before(request.ExpiresAt) {
		return Credential{}, ErrInvalidExtension
	}
	id, err := randomID()
	if err != nil {
		return Credential{}, err
	}
	c := Credential{ID: CredentialID(id), Label: request.Label, Principal: request.Principal, Revision: 1, Policy: request.Policy, CreatedAt: fromMillis(unixms(now)), ExpiresAt: request.ExpiresAt}
	err = s.createCredentialTransaction(ctx, func(tx *sql.Tx) error {
		if err := validateCredentialPolicyQuery(ctx, tx, request.Policy); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM credentials").Scan(&count); err != nil {
			return err
		}
		if count >= maxCredentials {
			return ErrLimitExceeded
		}
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM credentials WHERE digest=?", request.Digest[:]).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return ErrInvalidExtension
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO credentials(id,digest,account,label,principal,revision,all_chats,from_time,until_time,rolling,created_at,expires_at,revoked) VALUES(?,?,?,?,?,1,?,?,?,?,?,?,0)`, c.ID, request.Digest[:], request.Policy.Account, request.Label, request.Principal, request.Policy.AllChats, unixms(request.Policy.From), unixms(request.Policy.Until), int64(request.Policy.Rolling), unixms(c.CreatedAt), unixms(c.ExpiresAt))
		if err != nil {
			return err
		}
		return replaceCredentialChats(ctx, tx, c.ID, request.Policy.Chats)
	})
	if err != nil {
		return Credential{}, err
	}
	c.Policy.Chats = append([]ChatID(nil), c.Policy.Chats...)
	return c, nil
}

func (s *Store) createCredentialTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fixedSQLError(err)
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return fixedSQLError(err)
	}
	// SQL and the injected clock run before this final fence. A concurrent
	// auth-storage failure must prevent issuing a newly unusable credential.
	s.safetyMu.Lock()
	defer s.safetyMu.Unlock()
	s.credentialFence.mu.RLock()
	defer s.credentialFence.mu.RUnlock()
	if s.credentialFence.allDenied {
		return ErrUnauthorized
	}
	return fixedSQLError(tx.Commit())
}

func replaceCredentialChats(ctx context.Context, tx *sql.Tx, id CredentialID, chats []ChatID) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM credential_chats WHERE credential_id=?", id); err != nil {
		return err
	}
	for _, chat := range chats {
		if _, err := tx.ExecContext(ctx, "INSERT INTO credential_chats(credential_id,chat_id) VALUES(?,?)", id, chat); err != nil {
			return err
		}
	}
	return nil
}

// extensionReconcileCredentialChat runs inside the trusted source alias
// transaction. Membership and revision move together before the old chat's
// foreign key can be removed. It cannot join authority across accounts.
func extensionReconcileCredentialChat(ctx context.Context, tx *sql.Tx, oldChat, newChat ChatID) error {
	if !validReadID(string(oldChat)) || !validReadID(string(newChat)) {
		return ErrInvalidExtension
	}
	if oldChat == newChat {
		return nil
	}
	var oldAccount, newAccount string
	err := tx.QueryRowContext(ctx, "SELECT old.account,new.account FROM chats old JOIN chats new ON new.id=? WHERE old.id=?", newChat, oldChat).Scan(&oldAccount, &newAccount)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidExtension
	}
	if err != nil {
		return fixedSQLError(err)
	}
	if oldAccount != newAccount {
		return ErrInvalidExtension
	}
	var invalid int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM credentials c JOIN credential_chats cc ON cc.credential_id=c.id WHERE cc.chat_id=? AND (c.account<>? OR c.revision>=?)`, oldChat, oldAccount, int64(math.MaxInt64)).Scan(&invalid); err != nil {
		return fixedSQLError(err)
	}
	if invalid != 0 {
		return ErrInvalidExtension
	}
	// A credential with both memberships advances only once, including when
	// INSERT OR IGNORE will collapse the duplicate canonical membership.
	if _, err = tx.ExecContext(ctx, "UPDATE credentials SET revision=revision+1 WHERE id IN (SELECT credential_id FROM credential_chats WHERE chat_id=?)", oldChat); err != nil {
		return fixedSQLError(err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO credential_chats(credential_id,chat_id) SELECT credential_id,? FROM credential_chats WHERE chat_id=?", newChat, oldChat); err != nil {
		return fixedSQLError(err)
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM credential_chats WHERE chat_id=?", oldChat)
	return fixedSQLError(err)
}

func loadCredentialQuery(ctx context.Context, q sqlQuerier, id CredentialID) (Credential, error) {
	var c Credential
	var from, until, rolling, created, expires int64
	err := q.QueryRowContext(ctx, `SELECT id,label,principal,revision,account,all_chats,from_time,until_time,rolling,created_at,expires_at,revoked FROM credentials WHERE id=?`, id).Scan(&c.ID, &c.Label, &c.Principal, &c.Revision, &c.Policy.Account, &c.Policy.AllChats, &from, &until, &rolling, &created, &expires, &c.Revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, ErrUnauthorized
	}
	if err != nil {
		return Credential{}, readError(ctx, err)
	}
	c.Policy.From, c.Policy.Until, c.Policy.Rolling = fromMillis(from), fromMillis(until), time.Duration(rolling)
	c.CreatedAt, c.ExpiresAt = fromMillis(created), fromMillis(expires)
	rows, err := q.QueryContext(ctx, "SELECT chat_id FROM credential_chats WHERE credential_id=? ORDER BY chat_id LIMIT ?", id, maxReadChats+1)
	if err != nil {
		return Credential{}, readError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var chat ChatID
		if err = rows.Scan(&chat); err != nil {
			return Credential{}, readError(ctx, err)
		}
		c.Policy.Chats = append(c.Policy.Chats, chat)
	}
	if err = rows.Err(); err != nil {
		return Credential{}, readError(ctx, err)
	}
	if !validCredentialID(c.ID) || !validCredentialText(c.Label, maxCredentialLabel) || !validCredentialText(c.Principal, maxCredentialPrincipal) || c.Revision == 0 || c.Revision > math.MaxInt64 {
		return Credential{}, ErrUnauthorized
	}
	if _, err = normalizeCredentialPolicy(c.Policy); err != nil {
		return Credential{}, ErrUnauthorized
	}
	return c, nil
}

// Callers hold safetyMu.RLock before consulting the authentication fence.
func (s *Store) credentialGrantQueryAt(ctx context.Context, q sqlQuerier, c Credential, now time.Time) (CredentialGrant, error) {
	s.credentialFence.mu.RLock()
	denied := s.credentialFence.allDenied || s.credentialFence.denied[c.ID]
	s.credentialFence.mu.RUnlock()
	if denied || c.Revoked || (!c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt)) {
		return CredentialGrant{}, ErrUnauthorized
	}
	if s.unclean || s.unsafe[c.Policy.Account] {
		return CredentialGrant{}, ErrReconciliationRequired
	}
	var unsafe bool
	err := q.QueryRowContext(ctx, "SELECT unsafe FROM account_safety WHERE account=?", c.Policy.Account).Scan(&unsafe)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialGrant{}, ErrUnauthorized
	}
	if err != nil {
		return CredentialGrant{}, readError(ctx, err)
	}
	if unsafe {
		return CredentialGrant{}, ErrReconciliationRequired
	}
	policy, err := s.loadPolicy(ctx, q)
	if err != nil {
		return CredentialGrant{}, readError(ctx, err)
	}
	if policy.Generation == 0 {
		return CredentialGrant{}, ErrUnauthorized
	}
	scope := ReadScope{Account: c.Policy.Account, Principal: c.Principal, PolicyGeneration: policy.Generation, AllChats: c.Policy.AllChats, From: c.Policy.From, Until: c.Policy.Until, Rolling: c.Policy.Rolling}
	if policy.DateMode == DateSince {
		scope.From = later(scope.From, policy.ImportStart)
	}
	if policy.RollingRetention > 0 && (scope.Rolling == 0 || policy.RollingRetention < scope.Rolling) {
		scope.Rolling = policy.RollingRetention
	}
	if !scope.AllChats {
		included := make(map[ChatID]bool, len(policy.IncludedChats))
		for _, id := range policy.IncludedChats {
			included[id] = true
		}
		for _, id := range c.Policy.Chats {
			if policy.ChatMode == ChatsAll || included[id] {
				scope.Chats = append(scope.Chats, id)
			}
		}
		if len(scope.Chats) == 0 {
			return CredentialGrant{}, ErrUnauthorized
		}
	}
	if !scope.From.IsZero() && !scope.Until.IsZero() && !scope.From.Before(scope.Until) {
		return CredentialGrant{}, ErrUnauthorized
	}
	return CredentialGrant{CredentialID: c.ID, CredentialRevision: c.Revision, Scope: scope}, nil
}

func sameCredentialScope(a, b ReadScope) bool {
	if a.Account != b.Account || a.Principal != b.Principal || a.PolicyGeneration != b.PolicyGeneration || a.AllChats != b.AllChats || !a.From.Equal(b.From) || !a.Until.Equal(b.Until) || a.Rolling != b.Rolling {
		return false
	}
	chats, err := canonicalChats(a.Chats)
	if err != nil || len(chats) != len(b.Chats) {
		return false
	}
	for i := range chats {
		if chats[i] != b.Chats[i] {
			return false
		}
	}
	return true
}

// The At variant lets final response guards sample the service clock before
// taking safetyMu, then validate credential and source authority in one fence.
func (s *Store) checkCredentialGrantQueryAt(ctx context.Context, q sqlQuerier, grant CredentialGrant, now time.Time) error {
	if !validCredentialID(grant.CredentialID) || grant.CredentialRevision == 0 {
		return ErrUnauthorized
	}
	c, err := loadCredentialQuery(ctx, q, grant.CredentialID)
	if err != nil {
		return err
	}
	return s.checkCredentialGrantValueAt(ctx, q, grant, c, now)
}

func (s *Store) checkCredentialGrantValueAt(ctx context.Context, q sqlQuerier, grant CredentialGrant, c Credential, now time.Time) error {
	current, err := s.credentialGrantQueryAt(ctx, q, c, now)
	if err != nil {
		return err
	}
	if current.CredentialRevision != grant.CredentialRevision {
		return ErrUnauthorized
	}
	if current.Scope.PolicyGeneration != grant.Scope.PolicyGeneration {
		return ErrPolicyChanged
	}
	if !sameCredentialScope(grant.Scope, current.Scope) {
		return ErrUnauthorized
	}
	return nil
}

func (s *Store) checkCredentialGrantQuery(ctx context.Context, q sqlQuerier, grant CredentialGrant) error {
	// This convenience wrapper owns the fence. Call At when the final response
	// already holds safetyMu, so a clock callback never runs under that fence.
	now := s.now()
	s.safetyMu.RLock()
	defer s.safetyMu.RUnlock()
	return s.checkCredentialGrantQueryAt(ctx, q, grant, now)
}

func (s *Store) ResolveCredential(ctx context.Context, digest [32]byte) (CredentialGrant, error) {
	if digest == ([32]byte{}) {
		return CredentialGrant{}, ErrUnauthorized
	}
	return s.resolveCredentialRead(ctx, "", &digest)
}

// ResolveCredentialID is for a cryptographically verified Access identity's
// explicit server mapping. An opaque ID alone must never be exposed as auth.
func (s *Store) ResolveCredentialID(ctx context.Context, id CredentialID) (CredentialGrant, error) {
	if !validCredentialID(id) {
		return CredentialGrant{}, ErrUnauthorized
	}
	return s.resolveCredentialRead(ctx, id, nil)
}

func (s *Store) resolveCredentialRead(ctx context.Context, id CredentialID, digest *[32]byte) (CredentialGrant, error) {
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	if err := bounded.Err(); err != nil {
		return CredentialGrant{}, err
	}
	conn, err := s.readDB.Conn(bounded)
	if err != nil {
		return CredentialGrant{}, readError(bounded, err)
	}
	defer conn.Close()
	clockSample, sampledAt := s.now(), time.Now()
	s.safetyMu.RLock()
	defer s.safetyMu.RUnlock()
	tx, err := conn.BeginTx(bounded, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CredentialGrant{}, readError(bounded, err)
	}
	defer tx.Rollback()
	if digest != nil {
		err = tx.QueryRowContext(bounded, "SELECT id FROM credentials WHERE digest=?", digest[:]).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return CredentialGrant{}, ErrUnauthorized
		}
		if err != nil {
			return CredentialGrant{}, readError(bounded, err)
		}
	}
	c, err := loadCredentialQuery(bounded, tx, id)
	if err != nil {
		return CredentialGrant{}, err
	}
	grant, err := s.credentialGrantQueryAt(bounded, tx, c, clockSample.Add(time.Since(sampledAt)))
	if err != nil {
		return CredentialGrant{}, err
	}
	if err = tx.Commit(); err != nil {
		return CredentialGrant{}, readError(bounded, err)
	}
	if !c.ExpiresAt.IsZero() && !clockSample.Add(time.Since(sampledAt)).Before(c.ExpiresAt) {
		return CredentialGrant{}, ErrUnauthorized
	}
	return grant, nil
}

func (s *Store) CheckCredentialGrant(ctx context.Context, grant CredentialGrant) error {
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	if err := bounded.Err(); err != nil {
		return err
	}
	conn, err := s.readDB.Conn(bounded)
	if err != nil {
		return readError(bounded, err)
	}
	defer conn.Close()
	clockSample, sampledAt := s.now(), time.Now()
	s.safetyMu.RLock()
	defer s.safetyMu.RUnlock()
	tx, err := conn.BeginTx(bounded, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return readError(bounded, err)
	}
	defer tx.Rollback()
	if !validCredentialID(grant.CredentialID) || grant.CredentialRevision == 0 {
		return ErrUnauthorized
	}
	c, err := loadCredentialQuery(bounded, tx, grant.CredentialID)
	if err != nil {
		return err
	}
	if err = s.checkCredentialGrantValueAt(bounded, tx, grant, c, clockSample.Add(time.Since(sampledAt))); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return readError(bounded, err)
	}
	if !c.ExpiresAt.IsZero() && !clockSample.Add(time.Since(sampledAt)).Before(c.ExpiresAt) {
		return ErrUnauthorized
	}
	return nil
}

func (s *Store) SetCredentialPolicy(ctx context.Context, id CredentialID, policy CredentialPolicy, expected uint64) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	if !validCredentialID(id) || expected == 0 || expected >= math.MaxInt64 {
		return Credential{}, ErrInvalidExtension
	}
	policy, err := normalizeCredentialPolicy(policy)
	if err != nil {
		return Credential{}, err
	}
	value, err := s.submitExtension(ctx, setCredentialPolicyCommand{id, policy, expected}, credentialPolicyBytes(policy)+64)
	if err != nil {
		return Credential{}, err
	}
	return value.(Credential), nil
}

func (s *Store) setCredentialPolicy(ctx context.Context, id CredentialID, policy CredentialPolicy, expected uint64) (Credential, error) {
	var updated Credential
	invalidates := false
	err := s.transactionWithSafety(ctx, &invalidates, func(tx *sql.Tx) error {
		c, err := loadCredentialQuery(ctx, tx, id)
		if err != nil {
			return err
		}
		if c.Revision != expected {
			return ErrPolicyChanged
		}
		if c.Revoked {
			return ErrUnauthorized
		}
		if c.Policy.Account != policy.Account {
			return ErrInvalidExtension
		}
		if err = validateCredentialPolicyQuery(ctx, tx, policy); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE credentials SET revision=revision+1,all_chats=?,from_time=?,until_time=?,rolling=? WHERE id=? AND revision=? AND revoked=0`, policy.AllChats, unixms(policy.From), unixms(policy.Until), int64(policy.Rolling), id, expected)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrPolicyChanged
		}
		if err = replaceCredentialChats(ctx, tx, id, policy.Chats); err != nil {
			return err
		}
		c.Revision++
		c.Policy = policy
		updated = c
		return nil
	})
	if err != nil {
		return Credential{}, err
	}
	return updated, nil
}

func (s *Store) RevokeCredential(ctx context.Context, id CredentialID, expected uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validCredentialID(id) || expected == 0 || expected >= math.MaxInt64 {
		return ErrInvalidExtension
	}
	_, err := s.submitExtension(ctx, revokeCredentialCommand{id, expected}, 64)
	return err
}

func (s *Store) revokeCredential(ctx context.Context, id CredentialID, expected uint64) error {
	invalidates := false
	return s.transactionWithSafety(ctx, &invalidates, func(tx *sql.Tx) error {
		c, err := loadCredentialQuery(ctx, tx, id)
		if err != nil {
			return err
		}
		if c.Revision != expected {
			return ErrPolicyChanged
		}
		if c.Revoked {
			return nil
		}
		result, err := tx.ExecContext(ctx, "UPDATE credentials SET revoked=1,revision=revision+1 WHERE id=? AND revision=? AND revoked=0", id, expected)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrPolicyChanged
		}
		return nil
	})
}

func initializeCredentialFence(s *Store) error {
	s.credentialFence.denied = make(map[CredentialID]bool)
	for _, name := range []string{credentialFenceMarker, credentialFenceMarker + ".pending"} {
		path := filepath.Join(s.privateDir, name)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := checkPrivateFile(path); err != nil {
			s.credentialFence.allDenied = true
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			s.credentialFence.allDenied = true
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, maxCredentialFenceBytes+1))
		closeErr := f.Close()
		if err != nil || closeErr != nil || len(data) > maxCredentialFenceBytes || !strings.HasPrefix(string(data), "v1\n") || !strings.HasSuffix(string(data), "\n") {
			s.credentialFence.allDenied = true
			continue
		}
		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		if len(lines) > maxCredentials+1 {
			s.credentialFence.allDenied = true
			continue
		}
		for _, line := range lines[1:] {
			id := CredentialID(line)
			if line == "*" {
				s.credentialFence.allDenied = true
				continue
			}
			if !validCredentialID(id) {
				s.credentialFence.allDenied = true
				continue
			}
			s.credentialFence.denied[id] = true
		}
	}
	// Allocate the final marker before any credential can be issued. A later
	// disk-full failure can still truncate this existing inode to a durable,
	// corrupt marker which denies authentication on restart without new blocks.
	path := filepath.Join(s.privateDir, credentialFenceMarker)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		content := "v1\n"
		if s.credentialFence.allDenied || len(s.credentialFence.denied) != 0 {
			content += "*\n"
			s.credentialFence.allDenied = true
		}
		if err = s.writeMarker(credentialFenceMarker, content, true); err != nil {
			s.credentialFence.allDenied = true
			s.truncateCredentialFence()
		}
	}
	return nil
}

func (s *Store) truncateCredentialFence() {
	path := filepath.Join(s.privateDir, credentialFenceMarker)
	if err := checkPrivateFile(path); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return
	}
	f.Sync()
	f.Close()
	syncDir(s.privateDir)
}

// denyCredentialImmediate is called only for an uncertain accepted auth
// mutation. Failed read-only operations never call this function.
func (s *Store) denyCredentialImmediate(id CredentialID) error {
	s.safetyMu.Lock()
	defer s.safetyMu.Unlock()
	s.credentialFence.mu.Lock()
	defer s.credentialFence.mu.Unlock()
	if s.credentialFence.denied == nil {
		s.credentialFence.denied = make(map[CredentialID]bool)
	}
	if !validCredentialID(id) {
		s.credentialFence.allDenied = true
	} else {
		s.credentialFence.denied[id] = true
	}
	s.credentialFence.epoch++
	ids := make([]string, 0, len(s.credentialFence.denied))
	for deniedID := range s.credentialFence.denied {
		ids = append(ids, string(deniedID))
	}
	sort.Strings(ids)
	if len(ids) > maxCredentials {
		s.credentialFence.allDenied = true
	}
	content := "v1\n"
	if s.credentialFence.allDenied {
		content += "*\n"
	} else if len(ids) > 0 {
		content += strings.Join(ids, "\n") + "\n"
	}
	path := filepath.Join(s.privateDir, credentialFenceMarker)
	pending := path + ".pending"
	fail := func(err error) error {
		s.credentialFence.allDenied = true
		s.truncateCredentialFence()
		return err
	}
	if err := checkPrivateFile(path); err != nil {
		return fail(err)
	}
	if err := checkPrivateFile(pending); err != nil {
		return fail(err)
	}
	f, err := os.OpenFile(pending, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fail(ErrArchiveUnavailable)
	}
	// An empty pending file is itself a deny-all recovery state. Persist its
	// directory entry before writing the more specific credential set.
	if err = f.Sync(); err != nil {
		f.Close()
		return fail(ErrArchiveUnavailable)
	}
	if err = syncDir(s.privateDir); err != nil {
		f.Close()
		return fail(err)
	}
	_, writeErr := f.WriteString(content)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return fail(ErrArchiveUnavailable)
	}
	if err = os.Rename(pending, path); err != nil {
		return fail(ErrArchiveUnavailable)
	}
	if err = syncDir(s.privateDir); err != nil {
		return fail(err)
	}
	return nil
}

var _ CredentialStore = (*Store)(nil)
var _ CredentialRecoveryStore = (*Store)(nil)
