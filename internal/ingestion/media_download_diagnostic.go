package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
)

// Media stores may identify a fixed failure class without exposing their error
// text or requiring the connector to import the media package.
type mediaFailureClassifier interface{ MediaFailureClass() string }

func mediaDownloadErrorClass(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404):
		return "cdn_404"
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410):
		return "cdn_410"
	case errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403):
		return "cdn_403"
	case errors.Is(err, whatsmeow.ErrInvalidMediaHMAC):
		return "media_hmac"
	case errors.Is(err, whatsmeow.ErrInvalidMediaEncSHA256):
		return "ciphertext_hash"
	case errors.Is(err, whatsmeow.ErrInvalidMediaSHA256), errors.Is(err, whatsmeow.ErrInvalidUnencryptedMediaSHA256):
		return "plaintext_hash"
	case errors.Is(err, whatsmeow.ErrTooShortFile):
		return "file_too_short"
	case errors.Is(err, whatsmeow.ErrNoURLPresent):
		return "missing_path"
	case errors.Is(err, whatsmeow.ErrUnknownMediaType):
		return "unknown_media_type"
	}
	var marked mediaFailureClassifier
	if errors.As(err, &marked) && marked.MediaFailureClass() == "media_invalid_format" {
		return "media_invalid_format"
	}
	return "unknown"
}

func mediaTransferFailureDiagnostic(stage string, kind archive.MessageKind, err error) []byte {
	switch stage {
	case "admission", "download", "completion":
	default:
		stage = "unknown"
	}
	mediaKind := "unknown"
	if kind == archive.MessageVoice || kind == archive.MessageImage {
		mediaKind = string(kind)
	}
	encoded, _ := json.Marshal(struct {
		Event string `json:"event"`
		Stage string `json:"stage"`
		Kind  string `json:"kind"`
		Class string `json:"class"`
	}{"media_transfer_failure", stage, mediaKind, mediaDownloadErrorClass(err)})
	return append(encoded, '\n')
}

func recordMediaTransferFailure(stage string, kind archive.MessageKind, err error) {
	_, _ = os.Stderr.Write(mediaTransferFailureDiagnostic(stage, kind, err))
}
