package archive

import (
	"context"
	"time"
)

type MediaID string
type ArtifactID string // opaque app-owned artifact identity, never a path
type ReservationID string

type BudgetKind string

const (
	BudgetOriginals BudgetKind = "originals"
	BudgetDecoded   BudgetKind = "decoded_work"
)

type MediaAdmissionRequest struct {
	Source       SourceRef
	MediaID      MediaID
	Budget       BudgetKind
	MaximumBytes int64 // ciphertext+MAC/padding or decoded-work bound
	TTL          time.Duration
}
type MediaReservation struct {
	ID           ReservationID
	Source       SourceRef
	MediaID      MediaID
	Budget       BudgetKind
	MaximumBytes int64
	LeaseID      LeaseID
	LeaseUntil   time.Time
	ArtifactID   ArtifactID // assigned before any corresponding file is created
}
type OriginalPublication struct {
	Reservation  MediaReservation
	SHA256       string
	Bytes        int64
	ActualFormat string
}
type MediaArtifact struct {
	ID            ArtifactID
	MediaID       MediaID
	Source        SourceRef
	SHA256        string
	Bytes         int64
	ReservationID ReservationID
	CleanupLease  LeaseID
	Budget        BudgetKind
	ReservedBytes int64
	State         string // reserved, staged, retained, evicting; fixed allowlist
}
type MediaCleanup struct {
	ReservationID ReservationID
	LeaseID       LeaseID
	ArtifactID    ArtifactID
	Code          string
}
type MediaStore interface {
	CurrentSource(context.Context, SourceKey) (SourceRef, error)
	AdmitMedia(context.Context, MediaAdmissionRequest) (MediaReservation, error)
	RenewMedia(context.Context, MediaReservation, time.Duration) (time.Time, error)
	PublishOriginal(context.Context, OriginalPublication) (bool, error)
	EvictionCandidates(context.Context, int) ([]MediaID, error)
	BeginEviction(context.Context, MediaID) (MediaArtifact, error)
	FinishMediaCleanup(context.Context, MediaCleanup) error
	ReconcileMedia(context.Context, int) ([]MediaArtifact, error)
	AuthorizeOriginal(context.Context, ReadScope, MediaID) (MediaArtifact, error)
}

type OriginalDownloadLease struct {
	MediaID MediaID
	LeaseID LeaseID
}
type OriginalDownloadClaim struct {
	Account string
	TTL     time.Duration
}
type OriginalDownloadWork struct {
	Lease         OriginalDownloadLease
	Source        SourceRef
	Kind          MessageKind
	DeclaredBytes int64
	Attempts      int
	LeaseUntil    time.Time
}
type OriginalDownloadInput struct {
	Work              OriginalDownloadWork
	Key               SourceKey
	ProtocolReference []byte // <=8192 bytes; private reduced kind-specific protobuf
}
type OriginalDownloadRetry struct {
	Lease    OriginalDownloadLease
	Code     string
	After    time.Duration
	Terminal bool
}
type OriginalDownloadStore interface {
	ClaimOriginalDownload(context.Context, OriginalDownloadClaim) (OriginalDownloadWork, error)
	LoadOriginalDownload(context.Context, OriginalDownloadLease) (OriginalDownloadInput, error)
	FinishOriginalDownload(context.Context, OriginalDownloadLease) (bool, error)
	RetryOriginalDownload(context.Context, OriginalDownloadRetry) error
}

// MediaReservationInspector is a capability-bound private recovery port. It
// proves reservation/artifact state, and never grants access to media bytes.
type MediaReservationInspector interface {
	InspectMediaReservation(context.Context, MediaReservation) (MediaArtifact, error)
}

type ExtractionVersion struct {
	ID                                        string
	Kind                                      JobKind // OCR, caption, or transcription only
	ProfileID, ProfileIdentity, RuntimeSHA256 string
	ExtractorVersion, PromptVersion, Language string
}
type ExtractionClaim struct {
	Account string
	Version ExtractionVersion
	TTL     time.Duration
}
type ExtractionWork struct {
	ID             JobID
	Lease          JobLease
	LeaseUntil     time.Time
	Attempts       int
	Version        ExtractionVersion
	Source         SourceRef
	MediaID        MediaID
	ArtifactID     ArtifactID
	OriginalSHA256 string
	ContentHash    string
}
type ExtractionOutcome string

const (
	ExtractionText   ExtractionOutcome = "text"
	ExtractionEmpty  ExtractionOutcome = "no_text"
	ExtractionSilent ExtractionOutcome = "silent"
)

type ExtractionResult struct {
	Lease       JobLease
	ContentHash string
	Outcome     ExtractionOutcome
	Documents   []Document
}
type ExtractionStore interface {
	ClaimExtraction(context.Context, ExtractionClaim) (ExtractionWork, error)
	LoadExtractionInput(context.Context, JobLease) (ExtractionWork, error)
	RenewExtraction(context.Context, JobLease, time.Duration) (time.Time, error)
	CommitExtraction(context.Context, ExtractionResult) (bool, error)
	RetryExtraction(context.Context, JobRetry) error
}

type ExtractionRecoveryRequest struct {
	Account string
	After   JobID
	Limit   int
}
type PinnedExtraction struct {
	Account    string
	Lease      JobLease
	MediaID    MediaID
	ArtifactID ArtifactID
}
type ExtractionRecoveryPage struct {
	Items     []PinnedExtraction
	NextAfter JobID
}
type ExtractionRecoveryStore interface {
	ListPinnedExtractions(context.Context, ExtractionRecoveryRequest) (ExtractionRecoveryPage, error)
}

const (
	DefaultOriginalBudgetBytes int64 = 2 << 30
	MinimumOriginalBudgetBytes int64 = 64 << 20
	MaximumOriginalBudgetBytes int64 = 1 << 40
	ErrOriginalBudgetChanged   Error = "original_budget_changed"
)

type OriginalBudget struct {
	MaximumBytes, ChargedBytes int64
	Revision                   uint64
	Pressure                   bool
}
type OriginalBudgetUpdate struct {
	MaximumBytes     int64
	ExpectedRevision uint64
}
type OriginalBudgetStore interface {
	GetOriginalBudget(context.Context) (OriginalBudget, error)
	SetOriginalBudget(context.Context, OriginalBudgetUpdate) (OriginalBudget, error)
}
