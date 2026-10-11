package archive

import (
	"context"
	"time"
)

type DocumentID string
type JobID string
type LeaseID string
type IndexGenerationID string

type SourceRef struct {
	Account     string
	Version     SourceVersion
	ContentHash string
}

type IndexVersion struct {
	ID                                                       string
	EncoderProfile, TokenizerSHA256, TokenizerImplementation string
	PrefixVersion, ChunkingVersion, NormalizationVersion     string
	Dimensions, MaxPassageTokens                             int
}
type IndexGeneration struct {
	ID      IndexGenerationID
	Version IndexVersion
	State   string // building, active, retired; fixed allowlist
}

type DocumentKind string

const (
	DocumentWindow     DocumentKind = "text_window"
	DocumentOCR        DocumentKind = "ocr"
	DocumentCaption    DocumentKind = "caption"
	DocumentTranscript DocumentKind = "transcript"
)

type SourceSpan struct {
	Source             SourceRef
	StartByte, EndByte int    // original logical-order UTF-8 field offsets
	Field              string // body or caption, fixed allowlist
}
type DerivedProvenance struct {
	Kind                                       DocumentKind
	MediaID                                    MediaID
	ProfileID, ExtractorVersion, PromptVersion string
	StartMS, EndMS                             int64
	OffsetKind                                 string // none, slice, word; no fabricated word alignment
}
type Document struct {
	ID            DocumentID
	Kind          DocumentKind
	Text          string
	Sources       []SourceRef
	Spans         []SourceSpan
	Provenance    DerivedProvenance
	PassageTokens int
}
type WindowMessage struct {
	Source        SourceRef
	ChatID        ChatID
	SentAt        time.Time
	Body, Caption string
}
type DirtyClaim struct {
	Account    string
	Generation IndexGenerationID
	TTL        time.Duration
	Limit      int
}
type DirtyWork struct {
	ID               string
	LeaseID          LeaseID
	LeaseUntil       time.Time
	Account          string
	ChatID           ChatID
	PolicyGeneration Generation
	Generation       IndexGenerationID
	From, Until      time.Time
	DirtyRevision    ArchiveRevision
}
type WindowPublication struct {
	Work      DirtyWork
	Version   IndexVersion
	Documents []Document
}

type JobKind string

const (
	JobTextEmbedding    JobKind = "text_embedding"
	JobOCR              JobKind = "ocr"
	JobCaption          JobKind = "caption"
	JobTranscription    JobKind = "transcription"
	JobDerivedEmbedding JobKind = "derived_embedding"
)

type JobClaim struct {
	Account string
	Kind    JobKind
	TTL     time.Duration
}
type JobLease struct {
	JobID   JobID
	LeaseID LeaseID
}
type Job struct {
	ID          JobID
	Kind        JobKind
	Lease       JobLease
	LeaseUntil  time.Time
	Attempts    int
	Generation  IndexGenerationID
	Version     IndexVersion
	Sources     []SourceRef
	Documents   []DocumentID
	MediaID     MediaID
	ContentHash string
}
type JobInput struct {
	Job       Job
	Documents []Document // bounded authorized input, never persisted in job payload
}
type Embedding struct {
	DocumentID DocumentID
	Values     []float32
}
type JobResult struct {
	Lease       JobLease
	Generation  IndexGenerationID
	Version     IndexVersion
	Sources     []SourceRef
	ContentHash string
	Documents   []Document
	Embeddings  []Embedding
}
type JobRetry struct {
	Lease    JobLease
	Code     string // fixed allowlisted status; no supplied text/path
	After    time.Duration
	Terminal bool
}

type IndexStore interface {
	CurrentSource(context.Context, SourceKey) (SourceRef, error)
	EnsureIndex(context.Context, IndexVersion) (IndexGeneration, error)
	ClaimDirty(context.Context, DirtyClaim) (DirtyWork, error)
	LoadWindowMessages(context.Context, DirtyWork) ([]WindowMessage, error)
	PublishWindows(context.Context, WindowPublication) (bool, error)
	ClaimJob(context.Context, JobClaim) (Job, error)
	LoadJobInput(context.Context, JobLease) (JobInput, error)
	RenewJob(context.Context, JobLease, time.Duration) (time.Time, error)
	CommitJob(context.Context, JobResult) (bool, error)
	RetryJob(context.Context, JobRetry) error
	ActivateIndex(context.Context, IndexGenerationID) error
}

type PublicationCursor string // opaque, store-issued continuation authority

type PublicationProgress struct {
	Accepted, Complete bool
	Next               PublicationCursor
}

// WindowPosition is output metadata, never caller-supplied progress authority.
type WindowPosition struct {
	SourceOrdinal int
	Field         string // body or caption
	StartByte     int
}
type WindowBatchInput struct {
	Work     DirtyWork
	Cursor   PublicationCursor
	Ready    bool
	Start    WindowPosition
	Messages []WindowMessage
}
type WindowBatchPublication struct {
	Work      DirtyWork
	Version   IndexVersion
	Cursor    PublicationCursor
	Documents []Document
}

type DerivedOrigin struct {
	ID         DocumentID
	TextSHA256 string
	Bytes      int
	Provenance DerivedProvenance
	Sources    []SourceRef
}
type DerivedBatchInput struct {
	Job                Job
	Cursor             PublicationCursor
	Ready              bool
	Origin             DerivedOrigin
	StartByte, EndByte int
	Text               string
}
type DerivedBatchPublication struct {
	Result JobResult
	Cursor PublicationCursor
}
type PublicationStore interface {
	LoadWindowBatch(context.Context, DirtyWork) (WindowBatchInput, error)
	RenewDirty(context.Context, DirtyWork, time.Duration) (time.Time, error)
	PublishWindowBatch(context.Context, WindowBatchPublication) (PublicationProgress, error)
	LoadDerivedBatch(context.Context, JobLease) (DerivedBatchInput, error)
	CommitDerivedBatch(context.Context, DerivedBatchPublication) (PublicationProgress, error)
}

// ProductionIndexStore includes bounded publication and independent extraction;
// the one-shot compatibility ports remain available.
type ProductionIndexStore interface {
	IndexStore
	PublicationStore
	ExtractionStore
}
