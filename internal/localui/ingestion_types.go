package localui

import (
	"context"

	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
)

// IngestionApplication is optional and accessible only to the native owner.
// It reads existing connector counters without creating a connection or work.
type IngestionApplication interface {
	IngestionStatus(context.Context) (ingestion.LatencySnapshot, error)
}

// Keep the browser contract explicit: future internal snapshot fields must not
// accidentally become owner response fields, much less scoped MCP fields.
type ingestionStatusDTO struct {
	Running         bool                `json:"running"`
	Unsafe          bool                `json:"unsafe"`
	QueuedCount     int                 `json:"queued_count"`
	QueuedBytes     int64               `json:"queued_bytes"`
	WaitingCount    int                 `json:"waiting_count"`
	WaitingBytes    int64               `json:"waiting_bytes"`
	LivePending     int                 `json:"live_pending"`
	OldestLiveAgeMS int64               `json:"oldest_live_age_ms"`
	HistoryPending  int                 `json:"history_pending"`
	HistoryPrepared bool                `json:"history_prepared"`
	Phases          []ingestionPhaseDTO `json:"phases"`
}

type ingestionPhaseDTO struct {
	Lane      string `json:"lane"`
	Phase     string `json:"phase"`
	Samples   uint64 `json:"samples"`
	P50MS     int64  `json:"p50_ms"`
	P95MS     int64  `json:"p95_ms"`
	P99MS     int64  `json:"p99_ms"`
	MaxMS     int64  `json:"max_ms"`
	Saturated bool   `json:"saturated"`
}

func ingestionDTO(value ingestion.LatencySnapshot) ingestionStatusDTO {
	result := ingestionStatusDTO{
		Running: value.Running, Unsafe: value.Unsafe,
		QueuedCount: value.QueuedCount, QueuedBytes: value.QueuedBytes,
		WaitingCount: value.WaitingCount, WaitingBytes: value.WaitingBytes,
		LivePending: value.LivePending, OldestLiveAgeMS: value.OldestLiveAgeMS,
		HistoryPending: value.HistoryPending, HistoryPrepared: value.HistoryPrepared,
		Phases: make([]ingestionPhaseDTO, len(value.Phases)),
	}
	for i, phase := range value.Phases {
		result.Phases[i] = ingestionPhaseDTO{
			Lane: phase.Lane, Phase: phase.Phase, Samples: phase.Samples,
			P50MS: phase.P50MS, P95MS: phase.P95MS, P99MS: phase.P99MS,
			MaxMS: phase.MaxMS, Saturated: phase.Saturated,
		}
	}
	return result
}
