package localui

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

var ErrVoiceRetryUnavailable = errors.New("voice_retry_unavailable")

type VoiceRetryApplication interface {
	RetryVoiceDownloads(context.Context) (VoiceRetrySnapshot, error)
}

type VoiceRetryChatApplication interface {
	RetryVoiceDownloadsForChat(context.Context, archive.ChatID) (VoiceRetrySnapshot, error)
}
type VoiceRecoveryApplication interface {
	VoiceRecoveryStatus(context.Context, archive.ChatID) (archive.OwnerVoiceRecoveryStatus, error)
}

type VoiceRetrySnapshot struct {
	Recovery  archive.OwnerVoiceRecoveryStatus `json:"recovery"`
	Queued    int                              `json:"queued"`
	Skipped   int                              `json:"skipped"`
	Requested int                              `json:"requested"`
}
