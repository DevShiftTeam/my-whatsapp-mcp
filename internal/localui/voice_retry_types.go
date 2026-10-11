package localui

import (
	"context"
	"errors"
)

var ErrVoiceRetryUnavailable = errors.New("voice_retry_unavailable")

type VoiceRetryApplication interface {
	RetryVoiceDownloads(context.Context) (VoiceRetrySnapshot, error)
}

type VoiceRetrySnapshot struct {
	Queued  int `json:"queued"`
	Skipped int `json:"skipped"`
}
