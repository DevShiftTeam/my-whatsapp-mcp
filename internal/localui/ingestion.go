package localui

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

func (s *Server) ingestionStatusAdmission(w http.ResponseWriter, r *http.Request, ctx context.Context) bool {
	if ctx.Err() != nil {
		fail(w, http.StatusServiceUnavailable, "request_interrupted")
		return false
	}
	if err := s.ownerReadAdmissionError(r); err != nil {
		if ctx.Err() != nil {
			fail(w, http.StatusServiceUnavailable, "request_interrupted")
		} else if err == errOwnerAdmissionBusy {
			fail(w, http.StatusServiceUnavailable, "ingestion_status_unavailable")
		} else {
			fail(w, http.StatusUnauthorized, "unauthorized")
		}
		return false
	}
	if ctx.Err() != nil {
		fail(w, http.StatusServiceUnavailable, "request_interrupted")
		return false
	}
	return true
}

func (s *Server) ingestionStatus(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if s.realm != realmNative || s.remote != nil {
		fail(w, http.StatusNotFound, "route_unavailable")
		return
	}
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method_denied")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery || !bodyFree(r) {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if !s.ingestionStatusAdmission(w, r, ctx) {
		return
	}
	app, ok := s.app.(IngestionApplication)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "ingestion_status_unavailable")
		return
	}
	value, err := app.IngestionStatus(ctx)
	if err != nil || !value.Valid() {
		if ctx.Err() != nil {
			fail(w, http.StatusServiceUnavailable, "request_interrupted")
		} else {
			fail(w, http.StatusServiceUnavailable, "ingestion_status_unavailable")
		}
		return
	}
	// Serialize only this fixed, validated DTO, never a provider response. The
	// full buffer (including its newline) must fit before final owner admission.
	data, err := json.Marshal(ingestionDTO(value))
	if err != nil || len(data)+1 > 16*1024 {
		fail(w, http.StatusServiceUnavailable, "ingestion_status_unavailable")
		return
	}
	defer clear(data)
	// Preserve the request's additional owner-read fence, when supplied, without
	// using the blocking cookie check. Actual capability admission below uses
	// TryLock, so lifecycle contention cannot hold a buffered diagnostic open.
	if check, ok := r.Context().Value(ownerReadCheckKey{}).(func() bool); ok && !check() {
		if ctx.Err() != nil {
			fail(w, http.StatusServiceUnavailable, "request_interrupted")
		} else {
			fail(w, http.StatusUnauthorized, "unauthorized")
		}
		return
	}
	if !s.ingestionStatusAdmission(w, r, ctx) {
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(append(data, '\n'))
}
