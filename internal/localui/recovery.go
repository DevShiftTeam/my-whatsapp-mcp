package localui

import (
	"context"
	"errors"
	"net/http"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// RecoveryApplication is an owner capability only. Scoped MCP status keeps
// the existing mode/partial/unknown summary and never receives these counts.
type RecoveryApplication interface {
	RebuildProgress(context.Context) (*archive.RebuildProgress, error)
	ControlRebuild(context.Context, archive.RebuildControl) (*archive.RebuildProgress, error)
}

func (s *Server) recovery(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	app, ok := s.app.(RecoveryApplication)
	if !ok {
		if r.Method == http.MethodGet {
			respond(w, nil)
		} else {
			fail(w, 503, "recovery_unavailable")
		}
		return
	}
	var value *archive.RebuildProgress
	var err error
	if r.Method == http.MethodGet {
		value, err = app.RebuildProgress(ctx)
	} else {
		var input struct {
			ExpectedRevision uint64 `json:"expected_revision"`
			CommandID        string `json:"command_id"`
			Action           string `json:"action"`
		}
		if !decode(w, r, &input) {
			return
		}
		if input.ExpectedRevision == 0 || !validRecoveryCommandID(input.CommandID) || (input.Action != "pause" && input.Action != "resume" && input.Action != "extend" && input.Action != "cancel") {
			fail(w, 400, "invalid_request")
			return
		}
		value, err = app.ControlRebuild(ctx, archive.RebuildControl{ExpectedRevision: input.ExpectedRevision, CommandID: input.CommandID, Action: input.Action})
	}
	if err != nil {
		switch {
		case errors.Is(err, archive.ErrPolicyChanged):
			fail(w, 409, "recovery_changed")
		case errors.Is(err, archive.ErrInvalidEvent):
			fail(w, 400, "invalid_request")
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			fail(w, 503, "request_interrupted")
		default:
			fail(w, 503, "recovery_unavailable")
		}
		return
	}
	if r.Method != http.MethodGet && value == nil {
		fail(w, 503, "recovery_unavailable")
		return
	}
	respond(w, value)
}

func validRecoveryCommandID(id string) bool {
	if len(id) < 16 || len(id) > 128 {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}
