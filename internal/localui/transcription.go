package localui

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

func validTranscriptionStatus(value TranscriptionStatus) bool {
	switch value.State {
	case "not_configured":
		return !value.Configured && !value.HebrewReady
	case "connecting", "unavailable", "mismatch":
		return value.Configured && !value.HebrewReady
	case "ready":
		return value.Configured && value.HebrewReady
	case "busy":
		return value.Configured
	case "stopped":
		return !value.HebrewReady
	default:
		return false
	}
}

func (s *Server) transcriptionStatus(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if s.realm != realmNative {
		fail(w, http.StatusNotFound, "route_unavailable")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery || !bodyFree(r) {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	app, ok := s.app.(TranscriptionApplication)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "transcription_status_unavailable")
		return
	}
	// One active check and at most one admission per second across owner tabs.
	// The slot stays owned until the app returns, including after cancellation.
	s.mu.Lock()
	now := s.clock.nowLocked()
	if s.transcriptionChecking || now.Before(s.transcriptionNextCheck) {
		s.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		fail(w, http.StatusTooManyRequests, "transcription_status_busy")
		return
	}
	s.transcriptionChecking = true
	s.transcriptionNextCheck = now.Add(time.Second)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.transcriptionChecking = false; s.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	value, err := app.TranscriptionStatus(ctx)
	if err != nil || !validTranscriptionStatus(value) {
		if ctx.Err() != nil {
			fail(w, http.StatusServiceUnavailable, "request_interrupted")
		} else {
			fail(w, http.StatusServiceUnavailable, "transcription_status_unavailable")
		}
		return
	}
	// Serialize the fixed DTO before the final owner capability check. No
	// service response or private configuration can be streamed to the browser.
	data, err := json.Marshal(value)
	if err != nil || len(data) > 128 {
		fail(w, http.StatusServiceUnavailable, "transcription_status_unavailable")
		return
	}
	defer clear(data)
	if ctx.Err() != nil {
		fail(w, http.StatusServiceUnavailable, "request_interrupted")
		return
	}
	if !s.ownerReadCheck(r) {
		if ctx.Err() != nil {
			fail(w, http.StatusServiceUnavailable, "request_interrupted")
			return
		}
		fail(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := s.ownerReadAdmissionError(r); err != nil {
		if ctx.Err() != nil {
			fail(w, http.StatusServiceUnavailable, "request_interrupted")
		} else if err == errOwnerAdmissionBusy {
			fail(w, http.StatusServiceUnavailable, "transcription_status_unavailable")
		} else {
			fail(w, http.StatusUnauthorized, "unauthorized")
		}
		return
	}
	if ctx.Err() != nil {
		fail(w, http.StatusServiceUnavailable, "request_interrupted")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(append(data, '\n'))
}
