package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

type historicalAuthority struct {
	account string
	policy  ArchivePolicy
	chats   map[ChatID]string // opaque public ID -> exact canonical source identity
	now     time.Time
}

// Resolve bounded inventory and authority in one read transaction under the
// store's commit fence. Pool admission and clock hooks precede that fence.
func historicalAuthorityState(ctx context.Context, s *Store, account string, grant *CredentialGrant) (historicalAuthority, error) {
	bounded, cancel := s.readContext(ctx)
	defer cancel()
	conn, err := s.readDB.Conn(bounded)
	if err != nil {
		return historicalAuthority{}, readError(bounded, err)
	}
	defer conn.Close()
	clockSample, sampledAt := s.now(), time.Now()
	fenced, stop := context.WithTimeout(bounded, responseDeadline)
	defer stop()
	s.safetyMu.RLock()
	defer s.safetyMu.RUnlock()
	if s.unclean || s.unsafe[account] {
		return historicalAuthority{}, ErrReconciliationRequired
	}
	tx, err := conn.BeginTx(fenced, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return historicalAuthority{}, readError(fenced, err)
	}
	defer tx.Rollback()
	// A first-row account lookup could silently accept an ambiguous store.
	var bound string
	var accounts int
	err = tx.QueryRowContext(fenced, "SELECT COUNT(*),COALESCE(MIN(account),'') FROM accounts").Scan(&accounts, &bound)
	if err != nil {
		return historicalAuthority{}, readError(fenced, err)
	}
	if accounts != 1 || !validString(bound, 512) || account != "" && bound != account {
		return historicalAuthority{}, ErrUnauthorized
	}
	if s.unsafe[bound] {
		return historicalAuthority{}, ErrReconciliationRequired
	}
	var unsafe bool
	err = tx.QueryRowContext(fenced, "SELECT unsafe FROM account_safety WHERE account=?", bound).Scan(&unsafe)
	if errors.Is(err, sql.ErrNoRows) {
		return historicalAuthority{}, ErrUnauthorized
	}
	if err != nil {
		return historicalAuthority{}, readError(fenced, err)
	}
	if unsafe {
		return historicalAuthority{}, ErrReconciliationRequired
	}
	policy, err := s.loadPolicy(fenced, tx)
	if err != nil {
		return historicalAuthority{}, err
	}
	if policy.Generation == 0 || policy.DateMode != DateSince && policy.DateMode != DateAllAvailable || policy.ChatMode != ChatsAll && policy.ChatMode != ChatsSelective || policy.RollingRetention < 0 || policy.DateMode == DateSince && policy.ImportStart.IsZero() {
		return historicalAuthority{}, ErrUnauthorized
	}
	var credentialExpiry time.Time
	if grant != nil {
		if grant.Scope.Account != bound {
			return historicalAuthority{}, ErrUnauthorized
		}
		if err = s.checkCredentialGrantQueryAt(fenced, tx, *grant, clockSample.Add(time.Since(sampledAt))); err != nil {
			return historicalAuthority{}, err
		}
		credential, err := loadCredentialQuery(fenced, tx, grant.CredentialID)
		if err != nil {
			return historicalAuthority{}, err
		}
		credentialExpiry = credential.ExpiresAt
	}
	chats, err := historicalInventory(fenced, tx, bound)
	if err != nil {
		return historicalAuthority{}, err
	}
	if err = tx.Commit(); err != nil {
		return historicalAuthority{}, readError(fenced, err)
	}
	now := clockSample.Add(time.Since(sampledAt))
	if !credentialExpiry.IsZero() && !now.Before(credentialExpiry) {
		return historicalAuthority{}, ErrUnauthorized
	}
	return historicalAuthority{bound, policy, chats, now}, nil
}

func historicalInventory(ctx context.Context, tx *sql.Tx, account string) (map[ChatID]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.source_chat,COALESCE(a.canonical,c.source_chat),COALESCE(b.canonical,COALESCE(a.canonical,c.source_chat))
 FROM chats c LEFT JOIN identity_aliases a ON a.account=c.account AND a.alias=c.source_chat
 LEFT JOIN identity_aliases b ON b.account=c.account AND b.alias=COALESCE(a.canonical,c.source_chat)
 WHERE c.account=? ORDER BY c.id LIMIT ?`, account, maxReadChats+1)
	if err != nil {
		return nil, readError(ctx, err)
	}
	defer rows.Close()
	chats := make(map[ChatID]string)
	canonical := make(map[string]ChatID)
	for rows.Next() {
		var id ChatID
		var source, name, terminal string
		if err = rows.Scan(&id, &source, &name, &terminal); err != nil {
			return nil, readError(ctx, err)
		}
		if len(chats) == maxReadChats {
			return nil, ErrLimitExceeded
		}
		if !validReadID(string(id)) || IsHistoricalHandle(string(id)) || !validString(source, 512) || !validString(name, 512) || name != terminal {
			return nil, ErrUnauthorized
		}
		if previous, exists := canonical[name]; exists && previous != id {
			return nil, ErrUnauthorized
		}
		if _, exists := chats[id]; exists {
			return nil, ErrUnauthorized
		}
		chats[id], canonical[name] = name, id
	}
	if err = rows.Err(); err != nil {
		return nil, readError(ctx, err)
	}
	return chats, nil
}

func historicalLiveScope(scope ReadScope, live historicalAuthority) (ReadScope, error) {
	if scope.Account != live.account {
		return ReadScope{}, ErrUnauthorized
	}
	if scope.PolicyGeneration != live.policy.Generation {
		return ReadScope{}, ErrPolicyChanged
	}
	chats, err := canonicalChats(scope.Chats)
	if err != nil {
		return ReadScope{}, ErrUnauthorized
	}
	allowed := make(map[ChatID]bool, len(chats))
	for _, id := range chats {
		if _, exists := live.chats[id]; !exists {
			return ReadScope{}, ErrUnauthorized
		}
		allowed[id] = true
	}
	selected := make(map[ChatID]bool, len(live.policy.IncludedChats))
	for _, id := range live.policy.IncludedChats {
		if _, exists := live.chats[id]; !exists {
			return ReadScope{}, ErrUnauthorized
		}
		selected[id] = true
	}
	out := cloneHistoricalScope(scope)
	out.AllChats, out.Chats = false, nil
	for id := range live.chats {
		if (scope.AllChats || allowed[id]) && (live.policy.ChatMode == ChatsAll || selected[id]) {
			out.Chats = append(out.Chats, id)
		}
	}
	if len(out.Chats) == 0 {
		return ReadScope{}, ErrUnauthorized
	}
	sort.Slice(out.Chats, func(i, j int) bool { return out.Chats[i] < out.Chats[j] })
	if live.policy.DateMode == DateSince {
		out.From = later(out.From, live.policy.ImportStart)
	}
	out.Rolling = historicalRolling(out.Rolling, live.policy.RollingRetention)
	if !out.From.IsZero() && !out.Until.IsZero() && !out.From.Before(out.Until) || out.Rolling > 0 && !out.Until.IsZero() && !live.now.Add(-out.Rolling).Before(out.Until) {
		return ReadScope{}, ErrUnauthorized
	}
	return out, nil
}

func historicalProjection(scope ReadScope, live, history historicalAuthority) (ReadScope, ReadScope, error) {
	current, err := historicalLiveScope(scope, live)
	if err != nil {
		return ReadScope{}, ReadScope{}, err
	}
	if current.Account != history.account {
		return ReadScope{}, ReadScope{}, ErrUnauthorized
	}
	byCanonical := make(map[string]ChatID, len(history.chats))
	for id, source := range history.chats {
		byCanonical[source] = id
	}
	selected := make(map[ChatID]bool, len(history.policy.IncludedChats))
	for _, id := range history.policy.IncludedChats {
		if _, exists := history.chats[id]; !exists {
			return ReadScope{}, ReadScope{}, ErrUnauthorized
		}
		selected[id] = true
	}
	translated := cloneHistoricalScope(current)
	translated.PolicyGeneration, translated.Chats = history.policy.Generation, nil
	for _, id := range current.Chats {
		mapped, exists := byCanonical[live.chats[id]]
		if exists && (history.policy.ChatMode == ChatsAll || selected[mapped]) {
			translated.Chats = append(translated.Chats, mapped)
		}
	}
	if len(translated.Chats) == 0 {
		return ReadScope{}, ReadScope{}, ErrUnauthorized
	}
	sort.Slice(translated.Chats, func(i, j int) bool { return translated.Chats[i] < translated.Chats[j] })
	if history.policy.DateMode == DateSince {
		translated.From = later(translated.From, history.policy.ImportStart)
	}
	translated.Rolling = historicalRolling(translated.Rolling, history.policy.RollingRetention)
	if !translated.From.IsZero() && !translated.Until.IsZero() && !translated.From.Before(translated.Until) || translated.Rolling > 0 && !translated.Until.IsZero() && !live.now.Add(-translated.Rolling).Before(translated.Until) {
		return ReadScope{}, ReadScope{}, ErrUnauthorized
	}
	return current, translated, nil
}

func (r *historicalRead) requestChats(handles []ChatID) ([]ChatID, error) {
	if len(handles) > maxReadChats {
		return nil, ErrInvalidQuery
	}
	allowed := make(map[ChatID]bool, len(r.translated.Chats))
	for _, id := range r.translated.Chats {
		allowed[id] = true
	}
	chats := make([]ChatID, 0, len(handles))
	for _, handle := range handles {
		id, err := r.view.unwrapHandle(string(handle), "c")
		if err != nil {
			return nil, err
		}
		if !allowed[ChatID(id)] {
			return nil, ErrUnauthorized
		}
		chats = append(chats, ChatID(id))
	}
	return chats, nil
}

func historicalMappingIdentity(scope ReadScope, state historicalAuthority) [32]byte {
	mapping := make([][2]string, 0, len(scope.Chats))
	for _, id := range scope.Chats {
		mapping = append(mapping, [2]string{string(id), state.chats[id]})
	}
	encoded, _ := json.Marshal(mapping) // fixed string tuples cannot fail to encode
	return sha256.Sum256(encoded)
}
