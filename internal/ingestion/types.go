// Package ingestion bridges the pinned connector to the production archive.
// It exposes no sending, presence, or read-receipt operation.
package ingestion

import (
	"context"
	"github.com/Bodokh/wa-indexer/internal/archive"
	"go.mau.fi/whatsmeow"
	"io"
	"time"
)

type Config struct {
	SessionDir, TemporaryDir string
	BackfillRequests         int // zero explicitly disables history requests
	BodyQueueCount           int
	BodyQueueBytes, MediaMax int64
}

// BackfillRequest contains only current-policy opaque chat selection metadata.
// Account, authenticated anchors and protocol messages are server owned.
type BackfillRequest struct {
	PolicyGeneration archive.Generation
	AllSelected      bool
	Chats            []archive.ChatID
	Limit            int
}

type SelectedBackfillStore = archive.SelectedBackfillStore
type OriginalDownloadStore = archive.OriginalDownloadStore

type QRNotification struct {
	Event, Code string
	Timeout     time.Duration
}
type Status struct {
	Connection                                          archive.ConnectionState
	Paused, ReconciliationRequired, HasSession          bool
	Received, Applied, Dropped, HistoryChunks, Controls int64
	Codes                                               []string
	MediaCode                                           string // fixed outcome of the latest optional media attempt
	ConnectionDiagnostic                                ConnectionDiagnostic
}

// ConnectionDiagnostic contains only finite phases, events, typed error
// classes and bounded public protocol reason codes. It never retains errors,
// source identifiers, protocol nodes or account information.
type ConnectionDiagnostic struct {
	Phase, Event, ErrorClass                               string
	Reason                                                 int
	OnConnect                                              bool
	SocketStateKnown, SocketConnected, SocketAuthenticated bool
}

// MediaError is a finite body-free optional-retention outcome. It cannot carry
// a path, protocol key, backend diagnostic, or source body. These outcomes never
// establish a lost source/control observation or authorize archive quarantine.
type MediaError uint8

const (
	ErrMediaUnavailable MediaError = iota + 1
	ErrMediaSourceChanged
	ErrMediaCanceled
	ErrMediaDeadline
	ErrMediaWaitingForDiskBudget
	ErrMediaCleanupIncomplete
	ErrMediaInvalidAdmission
	ErrMediaAuthentication
	ErrMediaLimit
)

func (e MediaError) valid() bool { return e >= ErrMediaUnavailable && e <= ErrMediaLimit }
func (e MediaError) Error() string {
	switch e {
	case ErrMediaSourceChanged:
		return "media_source_changed"
	case ErrMediaCanceled:
		return "media_canceled"
	case ErrMediaDeadline:
		return "media_deadline_exceeded"
	case ErrMediaWaitingForDiskBudget:
		return "waiting_for_disk_budget"
	case ErrMediaCleanupIncomplete:
		return "media_cleanup_incomplete"
	case ErrMediaInvalidAdmission:
		return "media_invalid_admission"
	case ErrMediaAuthentication:
		return "media_authentication_failed"
	case ErrMediaLimit:
		return "media_limit_exceeded"
	default:
		return "media_unavailable"
	}
}

// VerifiedMedia is borrowed only for the duration of Complete or the isolated
// legacy test receiver. Complete must durably accept the exact association.
// Reader and protocol references never belong in public status or HTTP DTOs.
type VerifiedMedia struct {
	Key     archive.SourceKey
	Version archive.SourceVersion
	Kind    archive.MessageKind
	SHA256  [32]byte
	Bytes   int64
	Reader  io.Reader
}

// MediaDownloadRequest binds pre-byte quota admission to the association ID
// returned by the permitted source transaction. Protocol keys remain private.
type MediaDownloadRequest struct {
	Key                              archive.SourceKey
	Version                          archive.SourceVersion
	MediaID                          archive.MediaID
	Kind                             archive.MessageKind
	DeclaredBytes, CiphertextMaximum int64
	ProtocolSHA256                   [32]byte
}

// DownloadAdmission owns the bounded staged file and its durable reservation.
// Complete follows authenticated download and current-source validation. Close
// always follows synchronous writer shutdown and acknowledges cleanup.
type DownloadAdmission interface {
	File() whatsmeow.File
	Complete(context.Context, VerifiedMedia) error
	Close() error
}

type Callbacks struct {
	QRChanged     func(QRNotification)
	StatusChanged func(Status)
	// MediaReceiver is retained for isolated legacy tests. Public New requires
	// MediaAdmitter for retained bytes and never falls back to this receiver.
	MediaReceiver func(context.Context, VerifiedMedia) error
	MediaAdmitter func(context.Context, MediaDownloadRequest) (DownloadAdmission, error)
}

const (
	compressedMax int64 = 4 << 20
	decodedMax    int64 = 16 << 20
	recordMax           = 1 << 20
)
