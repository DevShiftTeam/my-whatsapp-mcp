// Package media owns source-bound original and decoded temporary files. It
// accepts authenticated connector receipts, never download URLs or file paths.
package media

import (
	"errors"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/diskspace"
)

var (
	ErrUnsafeFile           = errors.New("media file unsafe")
	ErrUnavailable          = errors.New("media store unavailable")
	ErrInvalidRequest       = errors.New("media request invalid")
	ErrSourceChanged        = errors.New("media source changed")
	ErrAuthentication       = errors.New("media authentication failed")
	ErrFormat               = errors.New("media format unsupported")
	ErrLimit                = errors.New("media size exceeds admission")
	ErrWaitingForDiskBudget = errors.New("waiting_for_disk_budget")
	ErrActiveUse            = errors.New("media owner still active")
	ErrClosed               = errors.New("media store closed")
	ErrPublicationUncertain = errors.New("media publication uncertain")
	ErrFilesystemMismatch   = errors.New("media filesystem mismatch")
)

type Options struct {
	// ExpectedDevice is mandatory in app composition and optional standalone.
	// Every retained directory must match the shared broker's device before use.
	ExpectedDevice                       uint64
	Admission                            diskspace.Admission
	ImageMaximumBytes, VoiceMaximumBytes int64
	LeaseTTL                             time.Duration
	// OnTransition observes private body-free lifecycle checkpoints. It executes
	// synchronously and must not reenter this store. No paths or protocol keys
	// are supplied. The same checkpoints support process-termination recovery.
	OnTransition func(Transition)
}

type Transition struct {
	ArtifactID archive.ArtifactID
	Phase      string
}

// DownloadRequest is the trusted connector boundary. The expected version is
// never replaced with a newer CurrentSource snapshot to authorize old bytes.
type DownloadRequest struct {
	Key                              archive.SourceKey
	Version                          archive.SourceVersion
	MediaID                          archive.MediaID
	Kind                             archive.MessageKind
	DeclaredBytes, CiphertextMaximum int64
	ProtocolSHA256                   [32]byte
}

type OriginalMetadata struct {
	MediaID        archive.MediaID
	ArtifactID     archive.ArtifactID
	Source         archive.SourceRef
	SHA256, Format string
	Bytes          int64
}

type ReconcileReport struct {
	Cleaned, Active int
	Codes           []string
}

// Exact artifact inspection is recovery authority, not permission to read an
// original. AuthorizeOriginal remains required for every worker's source.
type artifactInspector = archive.MediaReservationInspector
