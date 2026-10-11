package localui

import (
	"context"
	"errors"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

var ErrDocumentRetryUnavailable = errors.New("document_retry_unavailable")

type DocumentRetryApplication interface {
	RetryDocumentDownloads(context.Context, []archive.MessageID) (DocumentRetrySnapshot, error)
}

type DocumentRetrySnapshot struct {
	Queued    int `json:"queued"`
	Skipped   int `json:"skipped"`
	Requested int `json:"requested"`
}
