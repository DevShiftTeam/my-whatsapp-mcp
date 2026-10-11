// Package retrieval composes scoped archive candidates with supervised queries.
// It owns neither native workers nor archive writes, and caches no result text.
package retrieval

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/inference"
)

var ErrConfiguration = errors.New("retrieval_configuration_invalid")

// QueryEncoder is borrowed. Its owner controls admission, cancellation and reap.
type QueryEncoder interface {
	EncodeQuery(context.Context, string) ([]float32, error)
	TokenizerIdentity() inference.TokenizerIdentity
}

type Config struct {
	Reader     archive.Reader
	Candidates archive.CandidateReader
	Encoder    QueryEncoder
	// Index is trusted app state, resolved per query; it must describe the exact
	// active generation, not install status or a client-supplied profile.
	Index          func(context.Context) (archive.IndexGeneration, error)
	QueryTimeout   time.Duration
	MaxOutputBytes int
}

type SearchResult struct {
	Page archive.SearchPage
	Mode string
	Code string
	// These are private in-process integration values, never an HTTP/MCP DTO.
	Guard                 archive.ResponseGuard       `json:"-"`
	Documents             []archive.RetrievedDocument `json:"-"`
	CandidateLimit        int
	CandidateLimitReached bool
}

type Service struct {
	config    Config
	guarded   archive.GuardedReader
	cursorKey [32]byte
}

var _ archive.Reader = (*Service)(nil)
var _ archive.GuardedReader = (*Service)(nil)

func NewService(config Config) (*Service, error) {
	guarded, ok := config.Reader.(archive.GuardedReader)
	if config.Reader == nil || config.Candidates == nil || !ok {
		return nil, ErrConfiguration
	}
	if config.QueryTimeout == 0 {
		config.QueryTimeout = 5 * time.Second
	}
	if config.MaxOutputBytes == 0 {
		config.MaxOutputBytes = 128 << 10
	}
	if config.QueryTimeout < 0 || config.QueryTimeout > 30*time.Second || config.MaxOutputBytes < 1 || config.MaxOutputBytes > 128<<10 {
		return nil, ErrConfiguration
	}
	service := &Service{config: config, guarded: guarded}
	if _, err := io.ReadFull(rand.Reader, service.cursorKey[:]); err != nil {
		return nil, ErrConfiguration
	}
	return service, nil
}

func validateSearch(scope archive.ReadScope, request archive.SearchRequest) (int, error) {
	if scope.Account == "" || scope.Principal == "" || scope.PolicyGeneration == 0 || (!scope.AllChats && len(scope.Chats) == 0) {
		return 0, archive.ErrUnauthorized
	}
	if len(scope.Account) > 512 || len(scope.Principal) > 256 || !utf8.ValidString(scope.Account) || !utf8.ValidString(scope.Principal) || scope.Rolling < 0 || !validTime(scope.From) || !validTime(scope.Until) || !validChats(scope.Chats) {
		return 0, archive.ErrUnauthorized
	}
	if !validTime(request.From) || !validTime(request.Until) || (!request.From.IsZero() && !request.Until.IsZero() && !request.From.Before(request.Until)) || !validChats(request.Chats) {
		return 0, archive.ErrInvalidQuery
	}
	if len(request.Query) > 1024 || !utf8.ValidString(request.Query) {
		return 0, archive.ErrInvalidQuery
	}
	terms, runes, inTerm := 0, 0, false
	for _, r := range archive.NormalizeLexical(request.Query) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || (unicode.IsMark(r) && inTerm) {
			if !inTerm {
				terms++
				inTerm = true
			}
			runes++
			if terms > 32 || runes > 128 {
				return 0, archive.ErrInvalidQuery
			}
		} else {
			runes = 0
			inTerm = false
		}
	}
	if terms == 0 || request.Limit < 0 || request.Limit > 100 || len(request.Cursor) > 1024 {
		return 0, archive.ErrInvalidQuery
	}
	limit := request.Limit
	if limit == 0 {
		limit = 20
	}
	return limit, nil
}

// Match the archive's public request bounds before spending query compute.
func validTime(value time.Time) bool {
	return value.IsZero() || (value.Year() >= 1 && value.Year() <= 9999)
}
func validChats(chats []archive.ChatID) bool {
	if len(chats) > 4096 {
		return false
	}
	for _, id := range chats {
		if id == "" || len(id) > 128 || !utf8.ValidString(string(id)) || strings.ContainsRune(string(id), 0) {
			return false
		}
	}
	return true
}

func (s *Service) query(ctx context.Context, text string) (archive.CandidatePairRequest, string, error) {
	request := archive.CandidatePairRequest{KeywordOnly: true}
	if err := ctx.Err(); err != nil {
		return request, "", err
	}
	if s.config.Encoder == nil {
		return request, "model_missing", nil
	}
	if s.config.Index == nil {
		return request, "index_unavailable", nil
	}
	generation, err := s.config.Index(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return request, "", ctx.Err()
		}
		return request, "index_unavailable", nil
	}
	if generation.ID == "" || generation.State != "active" {
		return request, "index_unavailable", nil
	}
	identity := s.config.Encoder.TokenizerIdentity()
	version := generation.Version
	if identity.ProfileID != version.EncoderProfile || identity.TokenizerSHA256 != version.TokenizerSHA256 || identity.Implementation != version.TokenizerImplementation || identity.PrefixVersion != version.PrefixVersion {
		return request, "profile_mismatch", nil
	}
	bounded, cancel := context.WithTimeout(ctx, s.config.QueryTimeout)
	defer cancel()
	values, err := s.config.Encoder.EncodeQuery(bounded, text)
	if ctx.Err() != nil {
		return request, "", ctx.Err()
	}
	if err != nil || bounded.Err() != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(bounded.Err(), context.DeadlineExceeded) {
			return request, "encoder_timeout", nil
		}
		return request, "encoder_unavailable", nil
	}
	if s.config.Encoder.TokenizerIdentity() != identity || !validVector(values, version.Dimensions) {
		return request, "encoder_invalid", nil
	}
	request.KeywordOnly = false
	request.Generation = generation.ID
	request.Version = version
	request.Query = values
	return request, "", nil
}
func validVector(values []float32, dimensions int) bool {
	if dimensions < 1 || dimensions > 4096 || len(values) != dimensions {
		return false
	}
	norm := 0.
	for _, v := range values {
		value := float64(v)
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
		norm += value * value
	}
	return norm > 0 && math.Abs(norm-1) <= .01
}

func (s *Service) SearchDetails(ctx context.Context, scope archive.ReadScope, request archive.SearchRequest) (SearchResult, error) {
	limit, err := validateSearch(scope, request)
	if err != nil {
		return SearchResult{}, err
	}
	var cursor cursorPayload
	if request.Cursor != "" {
		cursor, err = s.decodeCursor(request.Cursor)
		if err != nil {
			return SearchResult{}, err
		}
	}
	candidateRequest, code, err := s.query(ctx, request.Query)
	if err != nil {
		return SearchResult{}, err
	}
	identity, err := staticCursorIdentity(scope, request, candidateRequest)
	if err != nil {
		return SearchResult{}, err
	}
	if request.Cursor != "" && cursor.Identity != identity {
		return SearchResult{}, archive.ErrCursorStale
	}
	candidateRequest.Search = request
	candidateRequest.Search.Cursor = ""
	candidateRequest.Search.Limit = 100
	// One source retry and one keyword switch have separate budgets. A late
	// index change cannot consume the selection needed to execute fallback.
	freshRetried := false
	retrySelection := func(err error, remaining bool) (bool, error) {
		if !retryable(err) {
			return false, err
		}
		if request.Cursor != "" {
			return false, archive.ErrCursorStale
		}
		if !remaining {
			return false, err
		}
		if errors.Is(err, archive.ErrCursorStale) && !candidateRequest.KeywordOnly {
			candidateRequest = archive.CandidatePairRequest{Search: candidateRequest.Search, KeywordOnly: true}
			code = "index_changed"
			var identityErr error
			identity, identityErr = staticCursorIdentity(scope, request, candidateRequest)
			return identityErr == nil, identityErr
		}
		if !freshRetried {
			freshRetried = true
			return true, nil
		}
		return false, err
	}
	for selection := 0; selection < 3; selection++ {
		pair, err := s.config.Candidates.CandidatePair(ctx, scope, candidateRequest)
		if err != nil {
			if again, retryErr := retrySelection(err, selection < 2); again {
				continue
			} else {
				return SearchResult{}, retryErr
			}
		}
		ranked, err := fuse(pair.Keyword, pair.Semantic)
		if err != nil {
			return SearchResult{}, err
		}
		ranking, err := rankingIdentity(pair)
		if err != nil {
			return SearchResult{}, err
		}
		offset := 0
		if request.Cursor != "" {
			if cursor.Snapshot != pair.Snapshot || cursor.Ranking != ranking {
				return SearchResult{}, archive.ErrCursorStale
			}
			offset = cursor.Offset
			if offset >= len(ranked) {
				return SearchResult{}, archive.ErrCursorStale
			}
		}
		end := offset + limit
		if end > len(ranked) {
			end = len(ranked)
		}
		more := end < len(ranked)
		ranked = ranked[offset:end]
		refs := make([]archive.DocumentRef, len(ranked))
		for i, candidate := range ranked {
			refs[i] = candidate.ref
		}
		hydrated, err := s.config.Candidates.HydrateDocuments(ctx, scope, archive.HydrateRequest{Documents: refs, Snapshot: pair.Snapshot, MaxBytes: s.config.MaxOutputBytes})
		if err != nil {
			if again, retryErr := retrySelection(err, selection < 2); again {
				continue
			} else {
				return SearchResult{}, retryErr
			}
		}
		if len(hydrated.Documents) != len(ranked) {
			return SearchResult{}, archive.ErrSourceUnavailable
		}
		result := SearchResult{Page: archive.SearchPage{Snapshot: pair.Snapshot}, Mode: "hybrid", Code: code, Guard: hydrated.Guard, Documents: hydrated.Documents, CandidateLimit: 100, CandidateLimitReached: len(pair.Keyword) >= 100 || len(pair.Semantic) >= 100}
		if more {
			now := time.Now()
			result.Page.NextCursor, err = s.encodeCursor(cursorPayload{Schema: 1, Offset: end, Identity: identity, Ranking: ranking, Snapshot: pair.Snapshot, IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(cursorLifetime).UnixMilli()})
			if err != nil {
				return SearchResult{}, err
			}
		}
		if candidateRequest.KeywordOnly {
			result.Mode = "keyword"
			if result.Code == "" {
				result.Code = pair.VectorCode
			}
			if result.Code == "" {
				result.Code = "keyword_only"
			}
		}
		for i, document := range hydrated.Documents {
			hit, err := searchHit(document, ranked[i])
			if err != nil {
				return SearchResult{}, err
			}
			result.Page.Hits = append(result.Page.Hits, hit)
		}
		if err = boundedOutput(result, s.config.MaxOutputBytes); err != nil {
			return SearchResult{}, err
		}
		if _, err = s.CheckResponse(ctx, scope, archive.ResponseCheck{Guard: result.Guard}); err != nil {
			if again, retryErr := retrySelection(err, selection < 2); again {
				continue
			} else {
				return SearchResult{}, retryErr
			}
		}
		return result, nil
	}
	return SearchResult{}, archive.ErrSourceUnavailable
}
func retryable(err error) bool {
	return errors.Is(err, archive.ErrSourceUnavailable) || errors.Is(err, archive.ErrCursorStale)
}
func boundedOutput(value any, maximum int) error {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maximum {
		return archive.ErrLimitExceeded
	}
	return nil
}
func searchHit(document archive.RetrievedDocument, candidate rankedCandidate) (archive.SearchHit, error) {
	if !sameRef(document.Ref, candidate.ref) || len(document.Ref.Sources) == 0 {
		return archive.SearchHit{}, archive.ErrSourceUnavailable
	}
	anchor := document.Ref.MessageID
	if anchor == "" {
		anchor = document.Ref.Sources[0].Version.MessageID
	}
	hit := archive.SearchHit{DocumentID: document.Ref.ID, MessageID: anchor, ChatID: document.ChatID, ChatName: document.ChatName, SenderName: document.SenderName, SentAt: document.SentAt, Kind: document.Kind, Score: candidate.score}
	for _, source := range document.Ref.Sources {
		hit.Sources = append(hit.Sources, source.Version)
	}
	for _, excerpt := range document.Excerpts {
		if excerpt.Provenance.Kind == archive.DocumentWindow {
			if hit.Snippet != "" {
				hit.Snippet += "\n"
			}
			hit.Snippet += excerpt.Text
		} else {
			hit.Derived = append(hit.Derived, excerpt)
		}
	}
	runes := []rune(hit.Snippet)
	if len(runes) > 320 {
		hit.Snippet = string(runes[:320])
	}
	return hit, nil
}

func (s *Service) Search(ctx context.Context, scope archive.ReadScope, request archive.SearchRequest) (archive.SearchPage, error) {
	result, err := s.SearchDetails(ctx, scope, request)
	return result.Page, err
}
func (s *Service) SearchGuarded(ctx context.Context, scope archive.ReadScope, request archive.SearchRequest) (archive.SearchPage, archive.ResponseGuard, error) {
	result, err := s.SearchDetails(ctx, scope, request)
	return result.Page, result.Guard, err
}
func (s *Service) CheckResponse(ctx context.Context, scope archive.ReadScope, check archive.ResponseCheck) (archive.ReadSnapshot, error) {
	return s.config.Candidates.CheckResponse(ctx, scope, check)
}

func (s *Service) Context(ctx context.Context, scope archive.ReadScope, request archive.ContextRequest) (archive.ContextPage, error) {
	page, _, err := s.ContextGuarded(ctx, scope, request)
	return page, err
}
func (s *Service) ContextGuarded(ctx context.Context, scope archive.ReadScope, request archive.ContextRequest) (archive.ContextPage, archive.ResponseGuard, error) {
	page, guard, err := s.guarded.ContextGuarded(ctx, scope, request)
	if err == nil {
		err = boundedOutput(page, s.config.MaxOutputBytes)
	}
	if err == nil {
		_, err = s.CheckResponse(ctx, scope, archive.ResponseCheck{Guard: guard})
	}
	if err != nil {
		return archive.ContextPage{}, archive.ResponseGuard{}, err
	}
	return page, guard, nil
}
func (s *Service) ListChats(ctx context.Context, scope archive.ReadScope, request archive.ChatListRequest) (archive.ChatPage, error) {
	page, _, err := s.ListChatsGuarded(ctx, scope, request)
	return page, err
}
func (s *Service) ListChatsGuarded(ctx context.Context, scope archive.ReadScope, request archive.ChatListRequest) (archive.ChatPage, archive.ResponseGuard, error) {
	page, guard, err := s.guarded.ListChatsGuarded(ctx, scope, request)
	if err == nil {
		err = boundedOutput(page, s.config.MaxOutputBytes)
	}
	if err == nil {
		_, err = s.CheckResponse(ctx, scope, archive.ResponseCheck{Guard: guard})
	}
	if err != nil {
		return archive.ChatPage{}, archive.ResponseGuard{}, err
	}
	return page, guard, nil
}
func (s *Service) Status(ctx context.Context, scope archive.ReadScope) (archive.StatusSnapshot, error) {
	page, _, err := s.StatusGuarded(ctx, scope)
	return page, err
}
func (s *Service) StatusGuarded(ctx context.Context, scope archive.ReadScope) (archive.StatusSnapshot, archive.ResponseGuard, error) {
	page, guard, err := s.guarded.StatusGuarded(ctx, scope)
	if err == nil {
		err = boundedOutput(page, s.config.MaxOutputBytes)
	}
	if err == nil {
		_, err = s.CheckResponse(ctx, scope, archive.ResponseCheck{Guard: guard})
	}
	if err != nil {
		return archive.StatusSnapshot{}, archive.ResponseGuard{}, err
	}
	return page, guard, nil
}
