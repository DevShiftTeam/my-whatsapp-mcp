package archive

import (
	"context"
	"time"
)

type CandidateKind string

const (
	CandidateKeyword CandidateKind = "keyword"
	CandidateVector  CandidateKind = "vector"
)

type CandidatePairRequest struct {
	Search      SearchRequest
	Generation  IndexGenerationID
	Version     IndexVersion
	Query       []float32
	KeywordOnly bool
}
type SourceReadVersion struct {
	Ref            SourceRef
	MetadataHash   [32]byte // authoritative display labels from the original read transaction
	ChatID         ChatID
	SenderID       string
	OriginalSentAt time.Time
	ExpiresAt      time.Time
}
type DocumentRef struct {
	ID            DocumentID
	MessageID     MessageID // anchor for original-message keyword fallback
	Sources       []SourceRef
	ReadSources   []SourceReadVersion
	Kind          DocumentKind
	root          *extensionRootResponsePin // archive-issued immutable root authority
	rootReference bool                      // keyword root, rather than a semantic fragment carrying its pin
}
type Candidate struct {
	Ref   DocumentRef
	Kind  CandidateKind
	Score float64
}
type CandidatePair struct {
	Keyword, Semantic []Candidate
	Snapshot          ReadSnapshot
	VectorCode        string // fixed fallback/status code, empty for normal hybrid
}
type HydrateRequest struct {
	Documents []DocumentRef
	Snapshot  ReadSnapshot
	MaxBytes  int
}
type RetrievedDocument struct {
	Kind                 MessageKind
	Ref                  DocumentRef
	ChatID               ChatID
	ChatName, SenderName string
	SentAt               time.Time
	Excerpts             []DerivedExcerpt
}
type ResponseGuard struct {
	safetyEpoch        uint64
	indexGeneration    uint64
	minSent, minExpiry time.Time
	rolling            time.Duration
	roots              []extensionRootResponsePin
	Sources            []SourceReadVersion // every excerpt/reply contributor, not only anchors
	PolicyGeneration   Generation
	Generation         IndexGenerationID
	IndexVersionID     string
	ScopeIdentity      [32]byte // static authority identity; not a stale rolling cutoff
}
type HydratedPage struct {
	Documents []RetrievedDocument
	Snapshot  ReadSnapshot
	Guard     ResponseGuard
}
type ResponseCheck struct {
	Guard      ResponseGuard
	Credential *CredentialGrant // nil for authenticated local owner, never remote input
}
type DerivedExcerpt struct {
	DocumentID DocumentID
	Source     SourceVersion
	Text       string
	Provenance DerivedProvenance
}
type CandidateReader interface {
	CandidatePair(context.Context, ReadScope, CandidatePairRequest) (CandidatePair, error)
	HydrateDocuments(context.Context, ReadScope, HydrateRequest) (HydratedPage, error)
	CheckResponse(context.Context, ReadScope, ResponseCheck) (ReadSnapshot, error)
}

// ActiveIndexReader is an internal, body-free per-query generation seam.
// CandidatePair and CheckResponse still revalidate the returned identity.
type ActiveIndexReader interface {
	ActiveIndex(context.Context) (IndexGeneration, error)
}

// GuardedReader preserves legacy pages and supplies an archive-built final
// response guard from the same read transaction, including reply contributors.
type GuardedReader interface {
	SearchGuarded(context.Context, ReadScope, SearchRequest) (SearchPage, ResponseGuard, error)
	ContextGuarded(context.Context, ReadScope, ContextRequest) (ContextPage, ResponseGuard, error)
	ListChatsGuarded(context.Context, ReadScope, ChatListRequest) (ChatPage, ResponseGuard, error)
	StatusGuarded(context.Context, ReadScope) (StatusSnapshot, ResponseGuard, error)
}
