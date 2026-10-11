package localui

import (
	"context"
	"errors"
	"net/http"
)

func (s *Server) voiceRetry(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	app, ok := s.app.(VoiceRetryApplication)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "voice_retry_unavailable")
		return
	}
	var input struct{}
	if !decode(w, r, &input) {
		return
	}
	value, err := app.RetryVoiceDownloads(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fail(w, http.StatusServiceUnavailable, "request_interrupted")
		} else {
			fail(w, http.StatusServiceUnavailable, "voice_retry_unavailable")
		}
		return
	}
	if value.Queued < 0 || value.Skipped < 0 || value.Requested < 0 || value.Requested > 8 || value.Queued > 32 || value.Skipped > 32 || value.Queued+value.Skipped > 32 {
		fail(w, http.StatusServiceUnavailable, "voice_retry_unavailable")
		return
	}
	respond(w, value)
}
