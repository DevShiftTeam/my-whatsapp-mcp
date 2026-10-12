package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// HistoricalView borrows two distinct stores. The application owns their
// lifetime and drains admitted requests before closing either store.
type HistoricalView struct {
	live, snapshot *Store
	account        string
	meta           HistoricalViewMetadata
}

type historicalRead struct {
	view        *HistoricalView
	original    ReadScope
	identity    [32]byte
	grant       *CredentialGrant
	translated  ReadScope
	liveScope   ReadScope
	liveMapping [32]byte
}

var _ HistoricalReadFactory = (*HistoricalView)(nil)
var _ HistoricalRead = (*historicalRead)(nil)

func NewHistoricalView(ctx context.Context, live *Store, snapshot *Store, meta HistoricalViewMetadata) (*HistoricalView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if live == nil || snapshot == nil || live == snapshot || live.readDB == nil || snapshot.readDB == nil || live.readDB == snapshot.readDB || live.historicalSnapshot || !snapshot.historicalSnapshot || snapshot.writeDB != nil || live.privateDir == snapshot.privateDir {
		return nil, ErrUnauthorized
	}
	if meta.Source != "historical" || !validHistoricalTag(meta.SnapshotID) || meta.CapturedAt.IsZero() || !validReadTime(meta.CapturedAt) || meta.CapturedAt.After(live.now()) || !meta.ReadOnly || meta.Coverage != "partial" || meta.CurrentPhoneState != "unknown" {
		return nil, ErrInvalidQuery
	}
	bounded, cancel := snapshot.readContext(ctx)
	defer cancel()
	var queryOnly int
	if err := snapshot.readDB.QueryRowContext(bounded, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		return nil, readError(bounded, err)
	}
	if queryOnly != 1 {
		return nil, ErrUnauthorized
	}
	liveState, err := historicalAuthorityState(ctx, live, "", nil)
	if err != nil {
		return nil, err
	}
	historyState, err := historicalAuthorityState(ctx, snapshot, "", nil)
	if err != nil {
		return nil, err
	}
	if liveState.account != historyState.account {
		return nil, ErrUnauthorized
	}
	meta.CapturedAt = meta.CapturedAt.UTC()
	return &HistoricalView{live: live, snapshot: snapshot, account: liveState.account, meta: meta}, nil
}

func (v *HistoricalView) ForCredential(ctx context.Context, grant CredentialGrant) (HistoricalRead, error) {
	copy := grant
	copy.Scope = cloneHistoricalScope(grant.Scope)
	return v.bind(ctx, copy.Scope, &copy)
}

func (v *HistoricalView) ForOwner(ctx context.Context, scope ReadScope) (HistoricalRead, error) {
	return v.bind(ctx, cloneHistoricalScope(scope), nil)
}

func (v *HistoricalView) Metadata() HistoricalViewMetadata { return v.meta }
func (r *historicalRead) Metadata() HistoricalViewMetadata { return r.view.meta }

func (v *HistoricalView) bind(ctx context.Context, scope ReadScope, grant *CredentialGrant) (HistoricalRead, error) {
	identity, err := extensionScopeIdentity(scope)
	if err != nil || scope.Account != v.account {
		return nil, ErrUnauthorized
	}
	liveState, err := historicalAuthorityState(ctx, v.live, v.account, grant)
	if err != nil {
		return nil, err
	}
	historyState, err := historicalAuthorityState(ctx, v.snapshot, v.account, nil)
	if err != nil {
		return nil, err
	}
	liveScope, translated, err := historicalProjection(scope, liveState, historyState)
	if err != nil {
		return nil, err
	}
	// Keep relative rolling retention in the identity. Its current cutoff is
	// checked on every read/final delivery, rather than baked into the cursor.
	binding := struct {
		View       string
		Identity   [32]byte
		Credential CredentialID
		Revision   uint64
		Scope      ReadScope
		Mapping    [32]byte
	}{View: v.meta.SnapshotID, Identity: identity, Scope: translated, Mapping: historicalMappingIdentity(liveScope, liveState)}
	if grant != nil {
		binding.Credential, binding.Revision = grant.CredentialID, grant.CredentialRevision
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return nil, ErrUnauthorized
	}
	digest := sha256.Sum256(encoded)
	translated.Principal = "historical:" + hex.EncodeToString(digest[:])
	r := &historicalRead{view: v, original: scope, identity: identity, grant: grant, translated: translated, liveScope: liveScope, liveMapping: binding.Mapping}
	if err = r.checkLive(ctx, scope); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *historicalRead) checkLive(ctx context.Context, scope ReadScope) error {
	_, err := r.checkLiveAt(ctx, scope)
	return err
}

func (r *historicalRead) checkLiveAt(ctx context.Context, scope ReadScope) (time.Time, error) {
	identity, err := extensionScopeIdentity(scope)
	if err != nil || identity != r.identity {
		return time.Time{}, ErrUnauthorized
	}
	state, err := historicalAuthorityState(ctx, r.view.live, r.view.account, r.grant)
	if err != nil {
		return time.Time{}, err
	}
	effective, err := historicalLiveScope(r.original, state)
	if err != nil {
		return time.Time{}, err
	}
	if !sameCredentialScope(effective, r.liveScope) || historicalMappingIdentity(effective, state) != r.liveMapping {
		return time.Time{}, ErrUnauthorized
	}
	if r.translated.Rolling > 0 && !r.translated.Until.IsZero() && !state.now.Add(-r.translated.Rolling).Before(r.translated.Until) {
		return time.Time{}, ErrUnauthorized
	}
	return state.now, nil
}

// CheckResponse applies current live authority on both sides of the snapshot
// check. The snapshot guard remains untouched and is never checked in live.
func (r *historicalRead) CheckResponse(ctx context.Context, scope ReadScope, check ResponseCheck) (ReadSnapshot, error) {
	if r.grant == nil {
		if check.Credential != nil {
			return ReadSnapshot{}, ErrUnauthorized
		}
	} else {
		if check.Credential == nil || check.Credential.CredentialID != r.grant.CredentialID || check.Credential.CredentialRevision != r.grant.CredentialRevision {
			return ReadSnapshot{}, ErrUnauthorized
		}
		identity, err := extensionScopeIdentity(check.Credential.Scope)
		if err != nil || identity != r.identity {
			return ReadSnapshot{}, ErrUnauthorized
		}
	}
	if err := r.checkLive(ctx, scope); err != nil {
		return ReadSnapshot{}, err
	}
	snapshot, err := r.view.snapshot.CheckResponse(ctx, r.translated, ResponseCheck{Guard: check.Guard})
	if err != nil {
		return ReadSnapshot{}, err
	}
	now, err := r.checkLiveAt(ctx, scope)
	if err != nil {
		return ReadSnapshot{}, err
	}
	// Independent live clock fencing prevents captured content's expiry or
	// rolling authorization from being extended by either store's read work.
	rolling := r.translated.Rolling
	if check.Guard.rolling > 0 && (rolling == 0 || check.Guard.rolling < rolling) {
		rolling = check.Guard.rolling
	}
	if (!check.Guard.minExpiry.IsZero() && !now.Before(check.Guard.minExpiry)) || (rolling > 0 && !check.Guard.minSent.IsZero() && check.Guard.minSent.Before(now.Add(-rolling))) {
		return ReadSnapshot{}, ErrSourceUnavailable
	}
	return snapshot, nil
}

func (r *historicalRead) finish(ctx context.Context, scope ReadScope, guard ResponseGuard) error {
	_, err := r.CheckResponse(ctx, scope, ResponseCheck{Guard: guard, Credential: r.grant})
	return err
}

func (r *historicalRead) SearchGuarded(ctx context.Context, scope ReadScope, request SearchRequest) (SearchPage, ResponseGuard, error) {
	if err := r.checkLive(ctx, scope); err != nil {
		return SearchPage{}, ResponseGuard{}, err
	}
	chats, err := r.requestChats(request.Chats)
	if err != nil {
		return SearchPage{}, ResponseGuard{}, err
	}
	request.Chats = chats
	request.Cursor, err = r.view.unwrapCursor(request.Cursor)
	if err != nil {
		return SearchPage{}, ResponseGuard{}, err
	}
	page, guard, err := r.view.snapshot.SearchGuarded(ctx, r.translated, request)
	if err != nil {
		return SearchPage{}, ResponseGuard{}, err
	}
	if err = r.view.wrapSearch(&page); err != nil {
		return SearchPage{}, ResponseGuard{}, err
	}
	if err = r.finish(ctx, scope, guard); err != nil {
		return SearchPage{}, ResponseGuard{}, err
	}
	return page, guard, nil
}

func (r *historicalRead) ContextGuarded(ctx context.Context, scope ReadScope, request ContextRequest) (ContextPage, ResponseGuard, error) {
	if err := r.checkLive(ctx, scope); err != nil {
		return ContextPage{}, ResponseGuard{}, err
	}
	id, err := r.view.unwrapHandle(string(request.MessageID), "m")
	if err != nil {
		return ContextPage{}, ResponseGuard{}, err
	}
	request.MessageID = MessageID(id)
	page, guard, err := r.view.snapshot.ContextGuarded(ctx, r.translated, request)
	if err != nil {
		return ContextPage{}, ResponseGuard{}, err
	}
	if err = r.view.wrapContext(&page); err != nil {
		return ContextPage{}, ResponseGuard{}, err
	}
	if err = r.finish(ctx, scope, guard); err != nil {
		return ContextPage{}, ResponseGuard{}, err
	}
	return page, guard, nil
}

func (r *historicalRead) ListChatsGuarded(ctx context.Context, scope ReadScope, request ChatListRequest) (ChatPage, ResponseGuard, error) {
	if err := r.checkLive(ctx, scope); err != nil {
		return ChatPage{}, ResponseGuard{}, err
	}
	var err error
	request.Cursor, err = r.view.unwrapCursor(request.Cursor)
	if err != nil {
		return ChatPage{}, ResponseGuard{}, err
	}
	page, guard, err := r.view.snapshot.ListChatsGuarded(ctx, r.translated, request)
	if err != nil {
		return ChatPage{}, ResponseGuard{}, err
	}
	if err = r.view.wrapChats(page.Chats); err != nil {
		return ChatPage{}, ResponseGuard{}, err
	}
	page.NextCursor, err = r.view.wrapCursor(page.NextCursor)
	if err != nil {
		return ChatPage{}, ResponseGuard{}, err
	}
	if err = r.finish(ctx, scope, guard); err != nil {
		return ChatPage{}, ResponseGuard{}, err
	}
	return page, guard, nil
}

func (r *historicalRead) StatusGuarded(ctx context.Context, scope ReadScope) (StatusSnapshot, ResponseGuard, error) {
	if err := r.checkLive(ctx, scope); err != nil {
		return StatusSnapshot{}, ResponseGuard{}, err
	}
	page, guard, err := r.view.snapshot.StatusGuarded(ctx, r.translated)
	if err != nil {
		return StatusSnapshot{}, ResponseGuard{}, err
	}
	if err = r.view.wrapChats(page.Chats); err != nil {
		return StatusSnapshot{}, ResponseGuard{}, err
	}
	page.Connection, page.Coverage, page.Paused, page.Rebuild = "historical_snapshot", "partial", true, nil
	page.HistoricalSnapshot = &HistoricalSnapshot{CapturedAt: r.view.meta.CapturedAt, ReadOnly: true, PhoneState: "unknown"}
	page.ConnectionDiagnostic = ConnectionDiagnostic{}
	page.Semantic, page.Media = Progress{State: "not_configured", Code: "historical_read_only"}, Progress{State: "not_configured", Code: "historical_read_only"}
	page.Codes = []string{"historical_snapshot", "partial_history", "current_phone_unknown"}
	if err = r.finish(ctx, scope, guard); err != nil {
		return StatusSnapshot{}, ResponseGuard{}, err
	}
	return page, guard, nil
}

func (r *historicalRead) StatsGuarded(ctx context.Context, scope ReadScope, request StatsRequest) (StatsSnapshot, ResponseGuard, error) {
	if err := r.checkLive(ctx, scope); err != nil {
		return StatsSnapshot{}, ResponseGuard{}, err
	}
	if request.ChatID != "" {
		chats, err := r.requestChats([]ChatID{request.ChatID})
		if err != nil {
			return StatsSnapshot{}, ResponseGuard{}, err
		}
		request.ChatID = chats[0]
	}
	page, guard, err := r.view.snapshot.StatsGuarded(ctx, r.translated, request)
	if err != nil {
		return StatsSnapshot{}, ResponseGuard{}, err
	}
	if err = r.view.wrapStats(&page); err != nil {
		return StatsSnapshot{}, ResponseGuard{}, err
	}
	if err = r.finish(ctx, scope, guard); err != nil {
		return StatsSnapshot{}, ResponseGuard{}, err
	}
	return page, guard, nil
}

func cloneHistoricalScope(scope ReadScope) ReadScope {
	scope.Chats = append([]ChatID(nil), scope.Chats...)
	return scope
}

// Retention durations represent the stricter nonzero relative window.
func historicalRolling(a, b time.Duration) time.Duration {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}
