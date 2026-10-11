package localui

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"math"
	"net/http"
	"sort"
)

type SendPermissionApplication interface {
	SetSendPermission(context.Context, string, uint64, []archive.ChatID) error
}

type SendPermissionModeApplication interface {
	SetSendPermissionMode(context.Context, string, uint64, archive.SendPermission, uint64) error
}

func (s *Server) sendPermission(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	app, ok := s.app.(SendPermissionApplication)
	if !ok {
		fail(w, 503, "send_unavailable")
		return
	}
	var request struct {
		ID                   string            `json:"id"`
		Revision             uint64            `json:"revision"`
		Chats                *[]archive.ChatID `json:"chats"`
		Mode                 json.RawMessage   `json:"mode"`
		ExpectedSendRevision json.RawMessage   `json:"expected_send_revision"`
	}
	if !decode(w, r, &request) {
		return
	}
	if !validOpaque(request.ID) || request.Revision == 0 || request.Chats == nil || len(*request.Chats) > 100 {
		fail(w, 400, "invalid_request")
		return
	}
	var err error
	permission := archive.SendPermission{Chats: []archive.ChatID{}}
	modePresent := len(request.Mode) != 0
	if modePresent {
		modeApp, supported := s.app.(SendPermissionModeApplication)
		if !supported {
			fail(w, 503, "send_unavailable")
			return
		}
		var mode string
		var expected uint64
		if len(request.ExpectedSendRevision) == 0 || string(request.ExpectedSendRevision) == "null" || json.Unmarshal(request.Mode, &mode) != nil || json.Unmarshal(request.ExpectedSendRevision, &expected) != nil || expected >= math.MaxInt64 || (mode != "none" && mode != "selected" && mode != "all") || (mode == "selected" && len(*request.Chats) == 0) || (mode != "selected" && len(*request.Chats) != 0) {
			fail(w, 400, "invalid_request")
			return
		}
		seen := make(map[archive.ChatID]bool, len(*request.Chats))
		for _, chat := range *request.Chats {
			if !validOpaque(string(chat)) || seen[chat] {
				fail(w, 400, "invalid_request")
				return
			}
			seen[chat] = true
		}
		permission.Mode = mode
		permission.Chats = append(permission.Chats, (*request.Chats)...)
		sort.Slice(permission.Chats, func(i, j int) bool { return permission.Chats[i] < permission.Chats[j] })
		err = modeApp.SetSendPermissionMode(ctx, request.ID, request.Revision, permission, expected)
		permission.Revision = expected + 1
	} else {
		if len(request.ExpectedSendRevision) != 0 {
			fail(w, 400, "invalid_request")
			return
		}
		err = app.SetSendPermission(ctx, request.ID, request.Revision, *request.Chats)
	}
	if err != nil {
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
	if modePresent {
		respond(w, map[string]any{"ok": true, "send_chat_count": len(*request.Chats), "send_permission": permission})
		return
	}
	respond(w, map[string]any{"ok": true, "send_chat_count": len(*request.Chats)})
}
