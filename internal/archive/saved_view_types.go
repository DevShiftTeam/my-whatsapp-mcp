package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var ErrSavedCursorStale = errors.New("saved_cursor_stale")
var ErrSavedReadLimit = errors.New("saved_read_limit")

type SavedLiveSearch interface {
	SearchGuarded(context.Context, ReadScope, SearchRequest) (SearchPage, ResponseGuard, error)
}
type SavedBrowseRequest struct {
	Chats       []ChatID
	From, Until time.Time
	Limit       int
	Cursor      string
}
type SavedViewMetadata struct {
	Source            string                  `json:"source"`
	Historical        *HistoricalViewMetadata `json:"historical,omitempty"`
	Coverage          string                  `json:"coverage"`
	CurrentPhoneState string                  `json:"current_phone_state"`
}
type SavedResponseGuard struct {
	owner            *savedOwnerRead
	live, historical []ResponseGuard
	historicalPins   []savedHistoricalPin
	aggregate        *savedAggregateWitness
}
type SavedOwnerRead interface {
	Search(context.Context, SearchRequest) (SearchPage, SavedResponseGuard, error)
	Browse(context.Context, SavedBrowseRequest) (SearchPage, SavedResponseGuard, error)
	Context(context.Context, ContextRequest) (ContextPage, SavedResponseGuard, error)
	Chats(context.Context, ChatListRequest) (ChatPage, SavedResponseGuard, error)
	Stats(context.Context, StatsRequest) (StatsSnapshot, SavedResponseGuard, error)
	Check(context.Context, SavedResponseGuard) error
}

const savedCandidateLimit = 2000
const savedMetadataLimit = 100000
const savedMemoryLimit = 32 << 20

type savedOwnerRead struct {
	live     *Store
	history  *HistoricalView
	bridge   *historicalRead
	scope    ReadScope
	search   SavedLiveSearch
	deadline time.Time
}
type savedHistoricalPin struct {
	metadata   savedMetadata
	canonical  SourceKey
	equivalent bool
	current    MessageID
	chat       ChatID
	expiry     time.Time
}
type savedAggregateWitness struct {
	request StatsRequest
	digest  [32]byte
	members map[string]bool
}

func NewSavedOwnerRead(ctx context.Context, live *Store, history *HistoricalView, scope ReadScope, search SavedLiveSearch) (SavedOwnerRead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if live == nil || search == nil {
		return nil, ErrUnauthorized
	}
	r := &savedOwnerRead{live: live, history: history, scope: cloneHistoricalScope(scope), search: search, deadline: time.Now().Add(5 * time.Second)}
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(r.deadline) {
		r.deadline = deadline
	}
	ctx, cancel := context.WithDeadline(ctx, r.deadline)
	defer cancel()
	if history != nil {
		if history.live != live {
			return nil, ErrUnauthorized
		}
		authority, err := historicalAuthorityState(ctx, live, r.scope.Account, nil)
		if err != nil {
			return nil, err
		}
		captured, err := historicalLiveScope(r.scope, authority)
		if err != nil {
			return nil, err
		}
		capturedHistory, err := historicalAuthorityState(ctx, history.snapshot, r.scope.Account, nil)
		if err != nil {
			return nil, err
		}
		canonicalHistory := map[string]ChatID{}
		for id, source := range capturedHistory.chats {
			key, e := savedCanonical(ctx, live.readDB, SourceKey{Account: r.scope.Account, Chat: source})
			if e != nil {
				return nil, e
			}
			if prior, ok := canonicalHistory[key.Chat]; ok && prior != id {
				return nil, ErrSourceUnavailable
			}
			canonicalHistory[key.Chat] = id
			capturedHistory.chats[id] = key.Chat
		}
		selected := map[ChatID]bool{}
		for _, id := range capturedHistory.policy.IncludedChats {
			selected[id] = true
		}
		overlap := false
		for _, id := range captured.Chats {
			hid, ok := canonicalHistory[authority.chats[id]]
			if ok && (capturedHistory.policy.ChatMode == ChatsAll || selected[hid]) {
				overlap = true
			}
		}
		if !overlap {
			return r, nil
		}
		historyFrom := captured.From
		if capturedHistory.policy.DateMode == DateSince {
			historyFrom = later(historyFrom, capturedHistory.policy.ImportStart)
		}
		rolling := historicalRolling(captured.Rolling, capturedHistory.policy.RollingRetention)
		if !captured.Until.IsZero() && (!historyFrom.IsZero() && !historyFrom.Before(captured.Until) || rolling > 0 && !authority.now.Add(-rolling).Before(captured.Until)) {
			return r, nil
		}
		current, translated, err := historicalProjection(captured, authority, capturedHistory)
		if err != nil {
			return nil, err
		}
		identity, err := extensionScopeIdentity(captured)
		if err != nil {
			return nil, err
		}
		mapping := historicalMappingIdentity(current, authority)
		encoded, _ := json.Marshal(struct {
			Scope    ReadScope
			Snapshot string
			Mapping  [32]byte
		}{translated, history.meta.SnapshotID, mapping})
		digest := sha256.Sum256(encoded)
		translated.Principal = "saved-historical:" + hex.EncodeToString(digest[:])
		r.bridge = &historicalRead{view: history, original: captured, identity: identity, translated: translated, liveScope: current, liveMapping: mapping, liveCoverageEpoch: authority.coverageEpoch, liveUnknownHistoryGap: authority.unknownHistoryGap, liveDeliveredHistorySnapshot: authority.deliveredHistorySnapshot}
		if err = r.bridge.checkLive(ctx, captured); err != nil {
			return nil, err
		}
	}
	return r, nil
}
