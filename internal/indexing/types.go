// Package indexing builds source-bound windows and runs durable embedding jobs.
// The archive owns persistence and authority; inference owns compute admission.
package indexing

import (
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/inference"
)

// Aliases keep one authoritative archive DTO contract for every consumer.
type Document = archive.Document
type Job = archive.Job
type SourceRef = archive.SourceRef
type SourceSpan = archive.SourceSpan
type WindowMessage = archive.WindowMessage
type IndexVersion = archive.IndexVersion
type IndexStore = archive.IndexStore
type PublicationStore = archive.PublicationStore
type WindowBatchInput = archive.WindowBatchInput

const (
	ChunkingVersion     = "window-v2-batch256-gap5m-msg20-token256-newline-utf8"
	MaxWindowMessages   = 20
	MaxWindowTokens     = 256
	MaxWindowGap        = 5 * time.Minute
	MaxPublicationBytes = 512 << 10
	MaxDocuments        = 256
	MaxSources          = 2048
	MaxInputMessages    = 256
	MaxSourceFieldBytes = 256 << 10
)

// Error contains a fixed body-free boundary code, never backend error text.
type Error string

func (e Error) Error() string { return string(e) }

const (
	ErrInvalidInput         Error = "indexing_invalid_input"
	ErrInvalidTokenizer     Error = "indexing_invalid_tokenizer"
	ErrInvalidConfiguration Error = "indexing_invalid_configuration"
	ErrLimit                Error = "indexing_limit_exceeded"
	ErrStore                Error = "indexing_store_failed"
	ErrBusy                 Error = "indexing_busy"
	ErrNotConfigured        Error = "indexing_not_configured"
)

// ServiceConfig binds work to one immutable index/tokenizer identity. The same
// inference Supervisor should supply Tokenizer and Encoder: it admits compute
// inside its actor rather than this service acquiring a second lease.
// Missing native configuration leaves semantic work pending; keyword reads are
// independent of this service. The caller retains ownership of native shutdown.
type ServiceConfig struct {
	Store                                       IndexStore
	Tokenizer                                   inference.PassageTokenizer
	Encoder                                     inference.Encoder
	Account                                     string
	Version                                     IndexVersion
	LeaseTTL, PollInterval, RetryBase, RetryMax time.Duration
	MaxAttempts                                 int
	Paused                                      bool
}

// StepResult and Snapshot contain only fixed status codes and aggregate counts.
type StepResult struct {
	State, Code         string
	Published, Embedded int
	Activated           bool
}
type Snapshot struct {
	State, Code         string
	Paused, Running     bool
	Published, Embedded uint64
}
