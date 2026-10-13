package localui

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

func (s *Server) voiceRetry(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	app, ok := s.app.(VoiceRetryApplication)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "voice_retry_unavailable")
		return
	}
	var input struct {
		ChatID archive.ChatID `json:"chat_id"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !validVoiceChat(input.ChatID) {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var value VoiceRetrySnapshot
	var err error
	if input.ChatID != "" {
		narrowed, ok := s.app.(VoiceRetryChatApplication)
		if !ok {
			fail(w, http.StatusServiceUnavailable, "voice_retry_unavailable")
			return
		}
		value, err = narrowed.RetryVoiceDownloadsForChat(ctx, input.ChatID)
	} else {
		value, err = app.RetryVoiceDownloads(ctx)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fail(w, http.StatusServiceUnavailable, "request_interrupted")
		} else {
			fail(w, http.StatusServiceUnavailable, "voice_retry_unavailable")
		}
		return
	}
	if value.Queued < 0 || value.Skipped < 0 || value.Requested < 0 || value.Requested > 8 || value.Queued > 32 || value.Skipped > 32 || value.Queued+value.Skipped > 32 || !validVoiceRecovery(value.Recovery) {
		fail(w, http.StatusServiceUnavailable, "voice_retry_unavailable")
		return
	}
	respond(w, value)
}

func validVoiceChat(chat archive.ChatID) bool {
	return len(chat) <= 128 && utf8.ValidString(string(chat)) && !strings.ContainsRune(string(chat), 0)
}
func validVoiceRecovery(value archive.OwnerVoiceRecoveryStatus) bool {
	for _, n := range []int{value.AwaitingPhone, value.WaitingCleanup, value.Queued, value.Downloading, value.Completed, value.Failed} {
		if n < 0 {
			return false
		}
	}
	switch value.Code {
	case "", "voice_retry_unavailable", "media_cleanup_incomplete", "awaiting_phone", "media_unavailable", "media_source_changed", "media_deadline_exceeded", "media_authentication_failed", "media_invalid_admission", "media_limit_exceeded", "media_canceled", "waiting_for_disk_budget":
		return true
	}
	return false
}
func (s *Server) voiceRecovery(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if len(r.URL.RawQuery) > 512 {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	values, parseErr := url.ParseQuery(r.URL.RawQuery)
	if parseErr != nil || len(values) > 1 || len(values["chat_id"]) > 1 {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	for key := range values {
		if key != "chat_id" {
			fail(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	chat := archive.ChatID(values.Get("chat_id"))
	if !validVoiceChat(chat) {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	app, ok := s.app.(VoiceRecoveryApplication)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "voice_retry_unavailable")
		return
	}
	value, err := app.VoiceRecoveryStatus(ctx, chat)
	if err != nil || !validVoiceRecovery(value) {
		fail(w, http.StatusServiceUnavailable, "voice_retry_unavailable")
		return
	}
	respond(w, value)
}
