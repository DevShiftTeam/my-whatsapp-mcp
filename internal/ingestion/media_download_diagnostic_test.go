package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
)

type privateMediaDiagnosticError struct{ cause error }

func (privateMediaDiagnosticError) Error() string   { panic("media diagnostic formatted private error") }
func (e privateMediaDiagnosticError) Unwrap() error { return e.cause }

type markedMediaDiagnosticError struct{ class string }

func (markedMediaDiagnosticError) Error() string               { panic("media diagnostic formatted marked error") }
func (e markedMediaDiagnosticError) MediaFailureClass() string { return e.class }

func TestMediaDownloadDiagnosticClassifiesWrappedTypedCauses(t *testing.T) {
	for _, item := range []struct {
		cause error
		class string
	}{
		{whatsmeow.ErrMediaDownloadFailedWith404, "cdn_404"},
		{whatsmeow.ErrMediaDownloadFailedWith410, "cdn_410"},
		{whatsmeow.ErrMediaDownloadFailedWith403, "cdn_403"},
		{whatsmeow.ErrInvalidMediaHMAC, "media_hmac"},
		{whatsmeow.ErrInvalidMediaEncSHA256, "ciphertext_hash"},
		{whatsmeow.ErrInvalidMediaSHA256, "plaintext_hash"},
		{whatsmeow.ErrInvalidUnencryptedMediaSHA256, "plaintext_hash"},
		{whatsmeow.ErrTooShortFile, "file_too_short"},
		{whatsmeow.ErrNoURLPresent, "missing_path"},
		{whatsmeow.ErrUnknownMediaType, "unknown_media_type"},
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline"},
		{markedMediaDiagnosticError{class: "media_invalid_format"}, "media_invalid_format"},
		{markedMediaDiagnosticError{class: "SYNTHETIC_PRIVATE_CLASS"}, "unknown"},
		{privateMediaDiagnosticError{}, "unknown"},
		{whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 502}}, "unknown"},
	} {
		t.Run(item.class, func(t *testing.T) {
			for _, cause := range []error{item.cause, privateMediaDiagnosticError{cause: item.cause}} {
				encoded := mediaTransferFailureDiagnostic("download", archive.MessageVoice, cause)
				var diagnostic map[string]string
				if json.Unmarshal(encoded, &diagnostic) != nil || len(diagnostic) != 4 || diagnostic["event"] != "media_transfer_failure" || diagnostic["stage"] != "download" || diagnostic["kind"] != "voice" || diagnostic["class"] != item.class || strings.Count(string(encoded), "\n") != 1 {
					t.Fatal("media diagnostic lost a typed cause or emitted unrestricted fields")
				}
			}
		})
	}
}

func TestMediaDownloadDiagnosticRejectsUnrecognizedFieldsAndErrorDetails(t *testing.T) {
	const private = "SYNTHETIC_PRIVATE_DIAGNOSTIC_CANARY"
	encoded := mediaTransferFailureDiagnostic(private, archive.MessageKind(private), privateMediaDiagnosticError{cause: markedMediaDiagnosticError{class: private}})
	if string(encoded) != "{\"event\":\"media_transfer_failure\",\"stage\":\"unknown\",\"kind\":\"unknown\",\"class\":\"unknown\"}\n" || strings.Contains(string(encoded), private) {
		t.Fatal("media diagnostic admitted private stage, kind, or error data")
	}
	for _, stage := range []string{"admission", "download", "completion"} {
		var diagnostic map[string]string
		if json.Unmarshal(mediaTransferFailureDiagnostic(stage, archive.MessageImage, errors.New(private)), &diagnostic) != nil || diagnostic["stage"] != stage || diagnostic["kind"] != "image" || diagnostic["class"] != "unknown" {
			t.Fatal("media diagnostic formatted an unknown error or lost a closed stage")
		}
	}
}

func TestMediaTransferFailureDiagnosticPreservesOptionalOutcome(t *testing.T) {
	for _, stage := range []string{"admission", "download", "completion"} {
		t.Run(stage, func(t *testing.T) {
			r, store := admissionFixture(t)
			t.Cleanup(func() { _ = r.Close() })
			cause := error(privateMediaDiagnosticError{cause: whatsmeow.ErrMediaDownloadFailedWith410})
			wantClass := "cdn_410"
			if stage == "completion" {
				cause = privateMediaDiagnosticError{cause: markedMediaDiagnosticError{class: "media_invalid_format"}}
				wantClass = "media_invalid_format"
			}
			r.callbacks.MediaAdmitter = func(ctx context.Context, request MediaDownloadRequest) (DownloadAdmission, error) {
				if stage == "admission" {
					return nil, cause
				}
				file, closeFile := admissionFile(t, ctx, request.CiphertextMaximum)
				return &fakeDownloadAdmission{file: file, close: closeFile, complete: func(context.Context, VerifiedMedia) error {
					if stage == "completion" {
						return cause
					}
					return nil
				}}, nil
			}
			r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, file whatsmeow.File) error {
				if stage == "download" {
					return cause
				}
				_, err := file.Write([]byte("source"))
				return err
			}
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal("diagnostic pipe setup failed")
			}
			previous := os.Stderr
			os.Stderr = write
			defer func() { os.Stderr = previous; _ = read.Close(); _ = write.Close() }()
			err = commitAndTransferForTest(context.Background(), r, store, validImageEvent())
			os.Stderr = previous
			_ = write.Close()
			encoded, readErr := io.ReadAll(io.LimitReader(read, 1024))
			if readErr != nil || !errors.Is(err, ErrMediaUnavailable) || r.Status().ReconciliationRequired || len(store.unsafe) != 0 {
				t.Fatal("diagnostic changed optional media failure into an archive failure")
			}
			var diagnostic map[string]string
			if json.Unmarshal(encoded, &diagnostic) != nil || len(diagnostic) != 4 || diagnostic["stage"] != stage || diagnostic["kind"] != "image" || diagnostic["class"] != wantClass || strings.Count(string(encoded), "\n") != 1 {
				t.Fatal("diagnostic was not emitted at the actual failing transfer boundary")
			}
		})
	}
}
