package localui

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// ArchiveReadRequest is an owner-only read; it carries no filesystem or
// credential authority and cannot select a mutation.
type ArchiveReadRequest struct {
	Kind    string
	Search  archive.SearchRequest
	Context archive.ContextRequest
	Chats   archive.ChatListRequest
	Stats   archive.StatsRequest
}

// ArchiveReadResult retains the selected reader until the entire buffered
// response has passed its final authority check and been delivered/discarded.
type ArchiveReadResult struct {
	Value    any
	Metadata archive.HistoricalViewMetadata
	Check    func(context.Context) error
	Release  func()
}
type ArchiveReadApplication interface {
	ReadArchive(context.Context, ArchiveReadRequest) (ArchiveReadResult, error)
}

// LiveArchiveReadApplication keeps the live source guard through owner delivery.
type LiveArchiveReadApplication interface {
	ReadLiveArchive(context.Context, ArchiveReadRequest) (ArchiveReadResult, error)
}
type ownerReadCheckKey struct{}
type ownerReadAdmissionKey struct{}

func (s *Server) ownerReadCheck(r *http.Request) bool {
	if check, ok := r.Context().Value(ownerReadCheckKey{}).(func() bool); ok {
		return check()
	}
	var csrf string
	var ok bool
	if s.remote != nil {
		csrf, ok = s.remoteAuthenticated(r)
	} else {
		csrf, ok = s.authenticated(r)
	}
	return ok && subtle.ConstantTimeCompare([]byte(csrf), []byte(r.Header.Get("X-CSRF-Token"))) == 1
}

// ownerReadAdmission performs no blocking lock acquisition or generation lookup
// after the final selected-source fence. Contention fails closed.
func (s *Server) ownerReadAdmission(r *http.Request) bool {
	if admit, ok := r.Context().Value(ownerReadAdmissionKey{}).(func() bool); ok {
		return admit()
	}
	if !s.mu.TryLock() {
		return false
	}
	defer s.mu.Unlock()
	var owner session
	if s.remote != nil {
		key, ok := cookieDigest(r, remoteCookie)
		if !ok {
			return false
		}
		v, ok := s.remote.sessions[key]
		generation, ready := s.remote.auth.TryGeneration()
		if !ok || !ready || s.remote.revoked || s.remote.ctx.Err() != nil || v.generation != generation {
			return false
		}
		owner = v.session
	} else {
		var token string
		count := 0
		for _, c := range r.Cookies() {
			if c.Name == s.cookieName {
				token = c.Value
				count++
			}
		}
		if count != 1 || token == "" || len(token) > 128 || s.owner == nil {
			return false
		}
		digest := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(digest[:], s.owner.digest[:]) != 1 {
			return false
		}
		owner = *s.owner
	}
	return r.Context().Err() == nil && s.clock.nowLocked().Before(owner.expires) && subtle.ConstantTimeCompare([]byte(owner.csrf), []byte(r.Header.Get("X-CSRF-Token"))) == 1
}
func (s *RemoteServer) historicalAdmission(r *http.Request, digest [32]byte, csrf string) bool {
	if !s.mu.TryLock() {
		return false
	}
	defer s.mu.Unlock()
	owner, ok := s.sessions[digest]
	return ok && !s.closed && r.Context().Err() == nil && s.clock.nowLocked().Before(owner.expires) && subtle.ConstantTimeCompare([]byte(owner.csrf), []byte(csrf)) == 1
}
func validArchiveSource(source string) bool {
	return source == "" || source == "live" || source == "historical"
}
func validArchiveID(id, source, kind string) bool {
	if source != "historical" {
		return !archive.IsHistoricalHandle(id) && validOpaque(id)
	}
	if len(id) > archive.HistoricalHandleMaxBytes {
		return false
	}
	parts := strings.SplitN(id, ".", 4)
	if len(parts) != 4 || parts[0] != "h1" || len(parts[1]) != 16 || parts[2] != kind || !validOpaque(parts[3]) {
		return false
	}
	for _, c := range parts[1] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func validArchiveIDs(ids []archive.ChatID, source string) bool {
	if len(ids) > 100 {
		return false
	}
	seen := map[archive.ChatID]bool{}
	for _, id := range ids {
		if seen[id] || !validArchiveID(string(id), source, "c") {
			return false
		}
		seen[id] = true
	}
	return true
}
func validArchiveCursor(cursor, source string) bool {
	if source != "historical" {
		return len(cursor) <= 8192 && !archive.IsHistoricalHandle(cursor)
	}
	if cursor == "" {
		return true
	}
	parts := strings.SplitN(cursor, ".", 4)
	return len(cursor) <= archive.HistoricalCursorMaxBytes && len(parts) == 4 && parts[0] == "h1" && len(parts[1]) == 16 && parts[2] == "p" && len(parts[3]) > 0 && len(parts[3]) <= 2048
}
func archiveReadQuery(r *http.Request, chats bool) (url.Values, error) {
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return nil, archive.ErrInvalidQuery
	}
	for key, v := range q {
		if len(v) != 1 || key != "source" && (!chats || key != "limit" && key != "cursor") {
			return nil, archive.ErrInvalidQuery
		}
	}
	if !validArchiveSource(q.Get("source")) {
		return nil, archive.ErrInvalidQuery
	}
	return q, nil
}
func (s *Server) historicalRead(w http.ResponseWriter, r *http.Request, ctx context.Context, q ArchiveReadRequest) {
	app, ok := s.app.(ArchiveReadApplication)
	if !ok {
		fail(w, 404, "historical_source_unavailable")
		return
	}
	value, err := app.ReadArchive(ctx, q)
	if err != nil {
		publicError(w, err)
		return
	}
	s.deliverArchiveRead(w, r, ctx, value, true)
}
func (s *Server) liveArchiveRead(w http.ResponseWriter, r *http.Request, ctx context.Context, q ArchiveReadRequest) bool {
	app, ok := s.app.(LiveArchiveReadApplication)
	if !ok {
		return false
	}
	value, err := app.ReadLiveArchive(ctx, q)
	if err != nil {
		publicError(w, err)
		return true
	}
	s.deliverArchiveRead(w, r, ctx, value, false)
	return true
}
func (s *Server) deliverArchiveRead(w http.ResponseWriter, r *http.Request, ctx context.Context, value ArchiveReadResult, historical bool) {
	if value.Release != nil {
		defer value.Release()
	}
	if value.Check == nil || value.Release == nil || (historical && value.Metadata.Source != "historical") || (!historical && value.Metadata.Source != "") {
		fail(w, 503, "archive_unavailable")
		return
	}
	if err := value.Check(ctx); err != nil {
		publicError(w, err)
		return
	}
	data, err := json.Marshal(value.Value)
	if err != nil {
		fail(w, 503, "archive_unavailable")
		return
	}
	defer clear(data)
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		fail(w, 503, "archive_unavailable")
		return
	}
	if historical {
		fields["archive_view"], err = json.Marshal(value.Metadata)
		if err != nil {
			fail(w, 503, "archive_unavailable")
			return
		}
	}
	body, err := json.Marshal(fields)
	if err != nil {
		fail(w, 503, "archive_unavailable")
		return
	}
	defer clear(body)
	if ctx.Err() != nil || !s.ownerReadCheck(r) {
		fail(w, 401, "unauthorized")
		return
	}
	if err := value.Check(ctx); err != nil {
		publicError(w, err)
		return
	}
	if !s.ownerReadAdmission(r) || ctx.Err() != nil {
		fail(w, 401, "unauthorized")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(append(body, '\n'))
}
func (s *Server) archiveStatus(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	q, err := archiveReadQuery(r, false)
	if err != nil {
		fail(w, 400, "invalid_query")
		return
	}
	if q.Get("source") == "historical" {
		s.historicalRead(w, r, ctx, ArchiveReadRequest{Kind: "status"})
		return
	}
	if s.liveArchiveRead(w, r, ctx, ArchiveReadRequest{Kind: "status"}) {
		return
	}
	value, err := s.app.Status(ctx)
	result(w, value, err)
}
