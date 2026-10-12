package localui

import (
	"context"
	"net/http"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// StatsApplication is optional so existing application adapters remain valid.
type StatsApplication interface {
	Stats(context.Context, archive.StatsRequest) (archive.StatsSnapshot, error)
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	var request struct {
		Source    string         `json:"source"`
		From      *int64         `json:"from"`
		Until     *int64         `json:"until"`
		ChatID    archive.ChatID `json:"chat_id"`
		Kind      string         `json:"kind"`
		Breakdown string         `json:"breakdown"`
		Page      int            `json:"page"`
		PageSize  int            `json:"page_size"`
		Sort      string         `json:"sort"`
	}
	if !decode(w, r, &request) {
		return
	}
	// The archive uses time.Time's zero value for an omitted bound. Reject that
	// exact instant rather than silently interpreting an explicit Until as all.
	validTime := func(value *int64) bool { return value == nil || (*value > -62135596800 && *value <= 253402300799) }
	if !validArchiveSource(request.Source) || !validTime(request.From) || !validTime(request.Until) ||
		(request.From != nil && request.Until != nil && *request.From >= *request.Until) ||
		(request.ChatID != "" && !validArchiveID(string(request.ChatID), request.Source, "c")) {
		fail(w, http.StatusBadRequest, "invalid_query")
		return
	}
	query := archive.StatsRequest{ChatID: request.ChatID, Kind: request.Kind, Breakdown: request.Breakdown,
		Page: request.Page, PageSize: request.PageSize, Sort: request.Sort}
	if request.From != nil {
		query.From = time.Unix(*request.From, 0).UTC()
	}
	if request.Until != nil {
		query.Until = time.Unix(*request.Until, 0).UTC()
	}
	validationQuery := query
	if request.Source == "historical" {
		validationQuery.ChatID = ""
	}
	if !archive.ValidStatsRequest(validationQuery) {
		fail(w, http.StatusBadRequest, "invalid_query")
		return
	}
	if request.Source == "historical" {
		s.historicalRead(w, r, ctx, ArchiveReadRequest{Kind: "stats", Stats: query})
		return
	}
	app, ok := s.app.(StatsApplication)
	if !ok {
		fail(w, http.StatusNotFound, "route_unavailable")
		return
	}
	value, err := app.Stats(ctx, query)
	result(w, value, err)
}
