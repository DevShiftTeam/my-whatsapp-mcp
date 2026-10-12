package archive

import (
	"errors"
	"time"
)

var ErrRebuildSessionTransition = errors.New("requires_session_transition")

// RebuildOptions describes an offline, checkpointed, privately owned backup.
// Device identity is verified by the profile owner before this archive call.
type RebuildOptions struct {
	ExpectedAccount      string
	ExpectedSourceSHA256 string
	DeviceID             string
	Boundary             time.Time
}

type RebuildProvenance struct {
	Generation          string    `json:"generation"`
	Account             string    `json:"account"`
	DeviceID            string    `json:"device_id"`
	SourceSHA256        string    `json:"source_sha256"`
	Boundary            time.Time `json:"boundary"`
	OriginalFenceReason string    `json:"original_fence_reason"`
	OriginalFenceEpoch  uint64    `json:"original_fence_epoch"`
}
type RebuildTableDigest struct {
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}
type RebuildReceipt struct {
	RebuildProvenance
	Tables    map[string]RebuildTableDigest `json:"tables"`
	Ancestors []RebuildProvenance           `json:"ancestors,omitempty"`
}

const MaxForwardAncestorProfiles = 2

type RebuildRequest struct {
	OperationID       string
	OperationRevision uint64
	ID, Generation    string
	Key               SourceKey
	PolicyGeneration  Generation
	CreatedAt         time.Time
}

const DefaultRebuildCopyLimit int64 = 10000
const DefaultRebuildDiscoveryLimit int64 = 512
const MaxRebuildMetadata int64 = 1000000

type RebuildProgress struct {
	Strategy          RebuildHistoryPolicy `json:"strategy"`
	StrategyEpoch     uint64               `json:"strategy_epoch"`
	DeliveredAccepted int64                `json:"delivered_accepted"`
	DeliveredLimit    int64                `json:"delivered_limit"`
	ReadableMessages  int64                `json:"readable_messages"`
	DeliveredOldest   time.Time            `json:"delivered_oldest"`
	PageAttempts      int64                `json:"page_attempts"`
	PageState         string               `json:"page_state"`
	OperationID       string               `json:"operation_id"`
	Generation        string               `json:"generation"`
	Revision          uint64               `json:"revision"`
	PolicyGeneration  Generation           `json:"policy_generation"`
	TargetStart       time.Time            `json:"target_start"`
	State             string               `json:"state"`
	CopyLimit         int64                `json:"copy_limit"`
	CopySpent         int64                `json:"copy_spent"`
	DiscoveryLimit    int64                `json:"discovery_limit"`
	DiscoverySpent    int64                `json:"discovery_spent"`
	Discovered        int64                `json:"discovered"`
	Queued            int64                `json:"queued"`
	Pending           int64                `json:"pending"`
	Accepted          int64                `json:"accepted"`
	Stale             int64                `json:"stale"`
	Deferred          int64                `json:"deferred"`
	LastAttemptAt     time.Time            `json:"last_attempt_at"`
	OldestObserved    time.Time            `json:"oldest_observed"`
	QueueFull         bool                 `json:"queue_full"`
}
type RebuildControl struct {
	ExpectedRevision uint64 `json:"expected_revision"`
	CommandID        string `json:"command_id"`
	Action           string `json:"action"`
}
type RebuildDiscovery struct {
	ID, OperationID, Generation string
	OperationRevision           uint64
	PolicyGeneration            Generation
	Anchor                      SourceAnchor
	CreatedAt                   time.Time
}

const rebuildOperationSchema = `
CREATE TABLE IF NOT EXISTS rebuild_operation(id INTEGER PRIMARY KEY CHECK(id=1),operation_id TEXT NOT NULL UNIQUE,generation TEXT NOT NULL UNIQUE,policy_generation INTEGER NOT NULL,target_start INTEGER NOT NULL,revision INTEGER NOT NULL,state TEXT NOT NULL CHECK(state IN('running','paused','cancelled')),copy_limit INTEGER NOT NULL,copy_spent INTEGER NOT NULL,discovery_limit INTEGER NOT NULL,discovery_spent INTEGER NOT NULL,last_attempt_at INTEGER NOT NULL DEFAULT 0,last_copy_chat TEXT NOT NULL DEFAULT '',last_discovery_chat TEXT NOT NULL DEFAULT '',queue_full INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS rebuild_commands(command_id TEXT PRIMARY KEY,expected_revision INTEGER NOT NULL,action TEXT NOT NULL,result_revision INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS rebuild_discovery_attempts(id TEXT PRIMARY KEY,operation_id TEXT NOT NULL,generation TEXT NOT NULL,account TEXT NOT NULL,chat TEXT NOT NULL,sender TEXT NOT NULL,message TEXT NOT NULL,from_me INTEGER NOT NULL,original_sent_at INTEGER NOT NULL,policy_generation INTEGER NOT NULL,operation_revision INTEGER NOT NULL,created_at INTEGER NOT NULL,UNIQUE(generation,account,chat,sender,message,from_me));
CREATE INDEX IF NOT EXISTS rebuild_requests_key ON rebuild_requests(generation,account,message,from_me,chat,sender);
CREATE INDEX IF NOT EXISTS rebuild_discovery_chat ON rebuild_discovery_attempts(generation,account,chat,original_sent_at);
CREATE TABLE IF NOT EXISTS rebuild_candidate_deferrals(account TEXT NOT NULL,chat TEXT NOT NULL,sender TEXT NOT NULL,message TEXT NOT NULL,from_me INTEGER NOT NULL,until_time INTEGER NOT NULL,PRIMARY KEY(account,chat,sender,message,from_me));
CREATE INDEX IF NOT EXISTS rebuild_deferral_message ON rebuild_candidate_deferrals(account,message,from_me);
`

type RebuildGuard struct {
	ChatClearCutoff   time.Time
	ExpiresAt         time.Time
	ViewOnce          bool
	ProtocolRevision  int64
	RevisionBarrier   string
	Deleted           bool
	OriginalSentAt    time.Time
	OriginalDateKnown bool
	SourceKind        SourceEventKind
}

// RebuildStatus is safe for scoped public status. Private provenance never is.
type RebuildStatus struct {
	Mode              string `json:"mode"`
	Coverage          string `json:"coverage"`
	CurrentPhoneState string `json:"current_phone_state"`
}

const rebuildSchema = `
CREATE TABLE rebuild_provenance(id INTEGER PRIMARY KEY CHECK(id=1),generation TEXT NOT NULL,account TEXT NOT NULL REFERENCES accounts(account),device_id TEXT NOT NULL,source_sha256 TEXT NOT NULL,boundary INTEGER NOT NULL,fence_reason TEXT NOT NULL,fence_epoch INTEGER NOT NULL,receipt TEXT NOT NULL);
CREATE TABLE rebuild_guards(account TEXT NOT NULL REFERENCES accounts(account),chat TEXT NOT NULL,sender TEXT NOT NULL,message TEXT NOT NULL,from_me INTEGER NOT NULL,original_sent_at INTEGER NOT NULL,date_known INTEGER NOT NULL,expires_at INTEGER NOT NULL,view_once INTEGER NOT NULL,protocol_revision INTEGER NOT NULL,revision_barrier TEXT NOT NULL,source_kind TEXT NOT NULL,deleted INTEGER NOT NULL,PRIMARY KEY(account,chat,sender,message,from_me));
CREATE INDEX rebuild_guard_message ON rebuild_guards(account,message,from_me);
CREATE TABLE rebuild_requests(id TEXT PRIMARY KEY,generation TEXT NOT NULL,account TEXT NOT NULL,chat TEXT NOT NULL,sender TEXT NOT NULL,message TEXT NOT NULL,from_me INTEGER NOT NULL,policy_generation INTEGER NOT NULL,created_at INTEGER NOT NULL,state TEXT NOT NULL CHECK(state IN('pending','consumed')),consumed_at INTEGER NOT NULL DEFAULT 0);
`
