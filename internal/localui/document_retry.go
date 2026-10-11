package localui

import (
	"context"
	"errors"
	"net/http"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func (s *Server) documentRetry(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	app, ok := s.app.(DocumentRetryApplication)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "document_retry_unavailable")
		return
	}
	var input struct {
		MessageIDs []archive.MessageID `json:"message_ids"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !ValidDocumentRetryIDs(input.MessageIDs) {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	value, err := app.RetryDocumentDownloads(ctx, input.MessageIDs)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fail(w, http.StatusServiceUnavailable, "request_interrupted")
		} else {
			fail(w, http.StatusServiceUnavailable, "document_retry_unavailable")
		}
		return
	}
	limit := len(input.MessageIDs)
	if value.Queued < 0 || value.Skipped < 0 || value.Requested < 0 || value.Queued > limit || value.Skipped > limit || value.Queued+value.Skipped > limit || value.Requested > limit {
		fail(w, http.StatusServiceUnavailable, "document_retry_unavailable")
		return
	}
	respond(w, value)
}

// ValidDocumentRetryIDs validates opaque public identifiers only. The archive
// and connector still derive and recheck the authenticated source authority.
func ValidDocumentRetryIDs(ids []archive.MessageID) bool {
	if len(ids) < 1 || len(ids) > 8 {
		return false
	}
	seen := make(map[archive.MessageID]bool, len(ids))
	for _, id := range ids {
		if id == "" || len(id) > 128 || seen[id] {
			return false
		}
		for _, r := range id {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
				return false
			}
		}
		seen[id] = true
	}
	return true
}
