package localui

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"net/http"
)

type SendPermissionApplication interface {
	SetSendPermission(context.Context, string, uint64, []archive.ChatID) error
}

func (s *Server) sendPermission(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	app, ok := s.app.(SendPermissionApplication)
	if !ok {
		fail(w, 503, "send_unavailable")
		return
	}
	var request struct {
		ID       string            `json:"id"`
		Revision uint64            `json:"revision"`
		Chats    *[]archive.ChatID `json:"chats"`
	}
	if !decode(w, r, &request) {
		return
	}
	if !validOpaque(request.ID) || request.Revision == 0 || request.Chats == nil || len(*request.Chats) > 100 {
		fail(w, 400, "invalid_request")
		return
	}
	if err := app.SetSendPermission(ctx, request.ID, request.Revision, *request.Chats); err != nil {
		status := 503
		if errors.Is(err, archive.ErrSendForbidden) {
			status = 403
		}
		if errors.Is(err, archive.ErrSendConflict) {
			status = 409
		}
		if errors.Is(err, archive.ErrInvalidQuery) {
			status = 400
		}
		fail(w, status, "send_permission_denied")
		return
	}
	respond(w, map[string]any{"ok": true, "send_chat_count": len(*request.Chats)})
}
