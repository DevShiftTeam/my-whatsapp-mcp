package archive

import (
	"context"
	"time"
)

type Generation uint64
type ArchiveRevision uint64
type ChatID string    // random opaque public ID, never a WhatsApp JID
type MessageID string // random opaque public ID, never a protocol message ID

type SourceKey struct {
	Account, Chat, Sender, Message string
	FromMe                         bool
}

type ArchivePolicy struct {
	Generation       Generation
	DateMode         DateMode      // Since or AllAvailable, exactly one
	ImportStart      time.Time     // inclusive, mandatory for Since
	ChatMode         ChatMode      // AllChats or Selective, exactly one
	IncludedChats    []ChatID      // Selective with no selections stores anchors only
	RollingRetention time.Duration // zero unless explicitly chosen
}

type ReadScope struct {
	AllChats bool // explicit server-owned owner grant; never client input

	Account          string // server-owned canonical source account, never client input
	Principal        string
	PolicyGeneration Generation
	Chats            []ChatID  // empty denies; no implicit wildcard
	From, Until      time.Time // inclusive From, exclusive Until
	Rolling          time.Duration
}

type SourceEvent struct {
	Key               SourceKey
	Kind              SourceEventKind // Original, EditInvalidation, EditContent, Delete
	OriginalSentAt    time.Time
	SourceRevision    int64 // trusted protocol mutation order, not ingest time
	OriginalDateKnown bool
	Body, Caption     string // original logical order; absent for invalidation/delete
	BodyKind          MessageKind
	ReplyTo           *SourceKey // opaque source target, never trusted quote content
	ExpiresAt         time.Time
	Media             []MediaReference // metadata only, bounded; no bytes or filesystem paths
	Checkpoint        *SyncCheckpoint  // observed input checkpoint, not lifetime completeness
}

type ApplyResult struct {
	MediaIDs         []MediaID
	Outcome          ApplyOutcome // Applied, Duplicate, Stale, Filtered, AwaitingMetadata
	MessageID        MessageID    // empty if no permitted source record exists
	ArchiveRevision  ArchiveRevision
	PolicyGeneration Generation
	Version          SourceVersion
}

type DateMode string

const (
	DateSince        DateMode = "since"
	DateAllAvailable DateMode = "all_available"
)

type ChatMode string

const (
	ChatsAll       ChatMode = "all"
	ChatsSelective ChatMode = "selective"
)

type SourceEventKind string

const (
	EventOriginal         SourceEventKind = "original"
	EventEditInvalidation SourceEventKind = "edit_invalidation"
	EventEditContent      SourceEventKind = "edit_content"
	EventDelete           SourceEventKind = "delete"
)

type MessageKind string

const (
	MessageText        MessageKind = "text"
	MessageImage       MessageKind = "image"
	MessageVoice       MessageKind = "voice"
	MessageDocument    MessageKind = "document"
	MessageUnsupported MessageKind = "unsupported"
)

type ApplyOutcome string

const (
	OutcomeApplied          ApplyOutcome = "applied"
	OutcomeDuplicate        ApplyOutcome = "duplicate"
	OutcomeStale            ApplyOutcome = "stale"
	OutcomeFiltered         ApplyOutcome = "filtered"
	OutcomeAwaitingMetadata ApplyOutcome = "awaiting_metadata"
)

type MediaReference struct {
	Kind              string
	ProtocolReference []byte // bounded, connector-authenticated reference only
	MimeType          string
	Filename          string // untrusted display metadata, UTF-8 <=255 bytes; never a path
	DeclaredBytes     int64
	ViewOnce          bool
	Availability      string
}
type SourceVersion struct {
	MessageID         MessageID
	Revision          uint64     // local monotonic source content/security revision
	PolicyGeneration  Generation // current policy generation at snapshot time
	Deleted, Excluded bool
	ExpiresAt         time.Time
}
type SourceMetadata struct {
	Key               SourceKey
	OriginalSentAt    time.Time
	OriginalDateKnown bool
	ProtocolRevision  int64
	Version           SourceVersion
}
type SourceAnchor struct {
	Key            SourceKey
	OriginalSentAt time.Time
}
type SourceAlias struct {
	Account, PhoneJID, LID string
	Provenance             AliasProvenance
}
type AliasProvenance string

const (
	AliasConnectorMapping AliasProvenance = "connector_mapping"
	AliasSessionMetadata  AliasProvenance = "session_metadata"
)

type SyncCheckpoint struct {
	Kind string
	ID   string // bounded opaque delivery ID; no payload/plaintext
}
type ConnectionState string
type CoverageState string
type ProgressState string
type SyncUpdate struct {
	Account, Chat                  string // internal source IDs, never returned directly
	Connection                     ConnectionState
	Checkpoint                     *SyncCheckpoint
	ObservedOldest, ObservedLatest time.Time
	Received, Applied, Dropped     int64
	GapCode                        string
	CoverageUnknown                bool
}
type UnsafeReason string  // allowlisted fixed reason code
type ObservationID string // random, process-owned lifecycle capability
type ReadSnapshot struct {
	PolicyGeneration Generation
	ArchiveRevision  ArchiveRevision
	IndexGeneration  uint64
	RecoveryEpoch    uint64
	RecoveryCode     string
}
type SearchRequest struct {
	Query       string
	Chats       []ChatID // optional narrowing only
	From, Until time.Time
	Limit       int
	Cursor      string
}
type SearchHit struct {
	DocumentID           DocumentID
	Sources              []SourceVersion
	Derived              []DerivedExcerpt
	MessageID            MessageID
	ChatID               ChatID
	ChatName, SenderName string
	SentAt               time.Time
	Kind                 MessageKind
	Snippet              string
	Score                float64
}
type SearchPage struct {
	Hits       []SearchHit
	NextCursor string
	Snapshot   ReadSnapshot
}
type ContextRequest struct {
	MessageID     MessageID
	Before, After int
}
type MessageView struct {
	Derived           []DerivedExcerpt
	ID                MessageID
	ChatID            ChatID
	SenderID          string // random opaque identity, never source JID
	SenderName        string
	SentAt            time.Time
	Kind              MessageKind
	Body, Caption     string
	ReplyTo           MessageID
	ReplyQuote        string
	Edited            bool
	MediaAvailability string
}
type ContextPage struct {
	Messages []MessageView
	Snapshot ReadSnapshot
}
type ChatListRequest struct {
	Query    string
	ChatType string // empty/all, direct, or group; narrows the permitted archive
	Limit    int
	Cursor   string
}
type ChatView struct {
	ID             ChatID
	Name           string
	Oldest, Latest time.Time
	MessageCount   int64
	Coverage       CoverageState
	GapCodes       []string
}
type ChatPage struct {
	Chats      []ChatView
	NextCursor string
	Snapshot   ReadSnapshot
}
type Progress struct {
	State                     ProgressState
	Pending, Complete, Failed int64
	Code                      string
}

// ConnectionDiagnostic exposes only finite operational values. Composition
// validates each value before publishing it; source identifiers and errors
// cannot be represented here.
type ConnectionDiagnostic struct {
	Phase, Event, ErrorClass                               string
	Reason                                                 int
	SocketStateKnown, SocketConnected, SocketAuthenticated bool
	OnConnect                                              bool // meaningful only for logged_out
}

type HistoricalSnapshot struct {
	CapturedAt time.Time `json:"captured_at"`
	ReadOnly   bool      `json:"read_only"`
	PhoneState string    `json:"current_phone_state"`
}

type StatusSnapshot struct {
	HistoricalSnapshot             *HistoricalSnapshot `json:"historical_snapshot,omitempty"`
	Connection                     ConnectionState
	Paused, ReconciliationRequired bool
	Coverage                       CoverageState
	Chats                          []ChatView // filtered; omit while unavailable or unauthorized
	Messages                       int64      // filtered
	Keyword, Semantic, Media       Progress
	Codes                          []string
	Snapshot                       ReadSnapshot
	ConnectionDiagnostic           ConnectionDiagnostic
}
type IngestionPolicy struct {
	Account             string
	Generation          Generation
	DateMode            DateMode
	ImportStart         time.Time
	ChatMode            ChatMode
	IncludedSourceChats []string // canonical source chat IDs; internal only
	RollingRetention    time.Duration
}
type DiscoveryChatView struct {
	ID                             ChatID
	Name                           string
	Selected                       bool
	ObservedOldest, ObservedLatest time.Time
	Coverage                       CoverageState
	GapCodes                       []string
}
type DiscoveryPage struct {
	Chats      []DiscoveryChatView
	NextCursor string
	Snapshot   ReadSnapshot
}

type Writer interface {
	Policy(context.Context) (ArchivePolicy, error)
	SetPolicy(context.Context, ArchivePolicy, Generation) (ArchivePolicy, error)
	ApplySourceEvent(context.Context, SourceEvent, Generation) (ApplyResult, error)
	ApplyAlias(context.Context, SourceAlias) error
	SetSyncStatus(context.Context, SyncUpdate) error
	MarkUnsafe(context.Context, string, UnsafeReason) error
}

type Reader interface {
	Search(context.Context, ReadScope, SearchRequest) (SearchPage, error)
	Context(context.Context, ReadScope, ContextRequest) (ContextPage, error)
	ListChats(context.Context, ReadScope, ChatListRequest) (ChatPage, error)
	Status(context.Context, ReadScope) (StatusSnapshot, error)
}

type ConnectorStore interface {
	Writer
	SetSourceNames(context.Context, SourceNames) error
	SourcePolicy(context.Context, string) (IngestionPolicy, error)
	Canonical(context.Context, string, string) (string, error)
	SourceMetadata(context.Context, SourceKey) (SourceMetadata, error)
	MutationTargets(context.Context, string, string, string) ([]SourceMetadata, error)
	// RecordUnresolvedDeletion durably withholds content by account, canonical chat
	// and message ID for an authenticated deletion with an unresolved author.
	RecordUnresolvedDeletion(context.Context, string, string, string) error
	BackfillAnchors(context.Context, string, int) ([]SourceAnchor, error)
	ClearChat(context.Context, string, string, time.Time) error
	BeginObservation(context.Context, string) (ObservationID, error)
	EndObservation(context.Context, ObservationID) error
	EligibleMedia(context.Context, SourceKey, SourceVersion) (bool, error)
	UpdateMedia(context.Context, SourceKey, SourceVersion, string) error
}
type OwnerStore interface {
	Writer
	Reader
	DiscoveryChats(context.Context, ChatListRequest) (DiscoveryPage, error)
	DiagnosticStatus(context.Context) (StatusSnapshot, error)
}

// SourceNames is trusted connector display metadata. Source identities alone
// choose records; names never establish or merge identity. Empty names preserve.
type SourceNames struct{ Account, Chat, Sender, ChatName, SenderName string }
