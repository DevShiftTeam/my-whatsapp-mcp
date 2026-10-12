package archive

import (
	"context"
	"strings"
	"time"
)

const HistoricalHandleMaxBytes = 256
const HistoricalCursorMaxBytes = 2304

// IsHistoricalHandle recognizes the reserved historical envelope, including
// malformed envelopes. Live readers and mutations must reject this namespace.
func IsHistoricalHandle(id string) bool { return strings.HasPrefix(id, "h1.") }

type HistoricalViewMetadata struct {
	Source            string    `json:"source"`
	SnapshotID        string    `json:"snapshot_id"`
	CapturedAt        time.Time `json:"captured_at"`
	ReadOnly          bool      `json:"read_only"`
	Coverage          string    `json:"coverage"`
	CurrentPhoneState string    `json:"current_phone_state"`
}

type HistoricalReadFactory interface {
	ForCredential(context.Context, CredentialGrant) (HistoricalRead, error)
	ForOwner(context.Context, ReadScope) (HistoricalRead, error)
	Metadata() HistoricalViewMetadata
}

// HistoricalRead is a request-local read capability under current live
// authority. Its guards belong to the historical store and must only be
// checked through this capability, with the original live scope.
type HistoricalRead interface {
	GuardedReader
	StatsGuarded(context.Context, ReadScope, StatsRequest) (StatsSnapshot, ResponseGuard, error)
	CheckResponse(context.Context, ReadScope, ResponseCheck) (ReadSnapshot, error)
	Metadata() HistoricalViewMetadata
}
