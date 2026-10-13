package localui

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"net/http"
	"time"
)

const maxArchiveResponseBytes = 8 << 20

var errOwnerAdmissionBusy = errors.New("archive_busy")

// SavedArchiveReadApplication provides only owner content reads. Operational
// settings and scoped client tools keep their existing application interfaces.
type SavedArchiveReadApplication interface {
	ReadSavedArchive(context.Context, ArchiveReadRequest) (ArchiveReadResult, error)
}

func (s *Server) savedRead(w http.ResponseWriter, r *http.Request, ctx context.Context, q ArchiveReadRequest) {
	app, ok := s.app.(SavedArchiveReadApplication)
	if !ok {
		fail(w, 404, "route_unavailable")
		return
	}
	value, err := app.ReadSavedArchive(ctx, q)
	if err != nil {
		publicError(w, err)
		return
	}
	if value.SavedMetadata == nil {
		if value.Release != nil {
			value.Release()
		}
		fail(w, 503, "archive_unavailable")
		return
	}
	s.deliverArchiveRead(w, r, ctx, value, false)
}
func (s *Server) savedBrowse(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	var request struct {
		Source string           `json:"source"`
		Chats  []archive.ChatID `json:"chats"`
		From   time.Time        `json:"from"`
		Until  time.Time        `json:"until"`
		Limit  int              `json:"limit"`
		Cursor string           `json:"cursor"`
	}
	if !decode(w, r, &request) {
		return
	}
	if request.Source != "saved" || !validArchiveIDs(request.Chats, "saved") || !validArchiveCursor(request.Cursor, "saved") || request.Limit < 0 || request.Limit > 100 || (!request.From.IsZero() && !request.Until.IsZero() && !request.From.Before(request.Until)) {
		fail(w, 400, "invalid_query")
		return
	}
	if request.Limit == 0 {
		request.Limit = 50
	}
	s.savedRead(w, r, ctx, ArchiveReadRequest{Kind: "browse", Browse: archive.SavedBrowseRequest{Chats: request.Chats, From: request.From, Until: request.Until, Limit: request.Limit, Cursor: request.Cursor}})
}
