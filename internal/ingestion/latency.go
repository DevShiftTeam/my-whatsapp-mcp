package ingestion

import (
	"math"
	"sync"
	"time"
)

// Measurements retain only finite phase labels and elapsed monotonic durations.
// No message, source identity, event pointer, filename or credential is retained.
type latencyLane uint8
type latencyPhase uint8

const (
	latencyLive latencyLane = iota
	latencyHistory
	latencySource
)

const (
	latencyAdmission latencyPhase = iota
	latencyQueue
	latencyProjection
	latencyCommit
	latencySearchReady
	latencyDownload
	latencyDecode
	latencyPreparation
)

var latencyLaneLabels = [...]string{"live", "history", "source"}
var latencyPhaseLabels = [...]string{"admission_wait", "queue_wait", "projection", "source_commit", "callback_to_commit", "history_download", "history_decode", "history_preparation"}
var latencyBoundsMS = [...]int64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2000, 5000, 10000, 30000, 60000, 120000, 300000, 600000, 1800000, 3600000}

type latencyCell struct {
	count   uint64
	buckets [len(latencyBoundsMS) + 1]uint64
	maxMS   int64
}

type latencyMetrics struct {
	mu    sync.Mutex
	cells [len(latencyLaneLabels)][len(latencyPhaseLabels)]latencyCell
}

// LatencyPhaseSnapshot reports histogram upper bounds, clamped to the observed
// maximum. Quantiles are approximate, not per-message traces or exact timings.
type LatencyPhaseSnapshot struct {
	Lane      string `json:"lane"`
	Phase     string `json:"phase"`
	Samples   uint64 `json:"samples"`
	P50MS     int64  `json:"p50_ms"`
	P95MS     int64  `json:"p95_ms"`
	P99MS     int64  `json:"p99_ms"`
	MaxMS     int64  `json:"max_ms"`
	Saturated bool   `json:"saturated"`
}

// LatencySnapshot is an owner diagnostic, never a scoped MCP status. Counts
// include queued and in-flight source work; phases accumulate in fixed storage.
type LatencySnapshot struct {
	Running         bool                   `json:"running"`
	Unsafe          bool                   `json:"unsafe"`
	QueuedCount     int                    `json:"queued_count"`
	QueuedBytes     int64                  `json:"queued_bytes"`
	WaitingCount    int                    `json:"waiting_count"`
	WaitingBytes    int64                  `json:"waiting_bytes"`
	LivePending     int                    `json:"live_pending"`
	OldestLiveAgeMS int64                  `json:"oldest_live_age_ms"`
	HistoryPending  int                    `json:"history_pending"`
	HistoryPrepared bool                   `json:"history_prepared"`
	Phases          []LatencyPhaseSnapshot `json:"phases"`
}

func elapsedMS(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	ms := int64(d / time.Millisecond)
	if d%time.Millisecond != 0 {
		ms++
	}
	return ms
}

func (m *latencyMetrics) record(lane latencyLane, phase latencyPhase, elapsed time.Duration) bool {
	if int(lane) >= len(latencyLaneLabels) || int(phase) >= len(latencyPhaseLabels) {
		return false
	}
	ms := elapsedMS(elapsed)
	m.mu.Lock()
	defer m.mu.Unlock()
	cell := &m.cells[lane][phase]
	if ms > cell.maxMS {
		cell.maxMS = ms
	}
	if cell.count == math.MaxUint64 {
		return true
	}
	cell.count++
	index := len(latencyBoundsMS)
	for i, upper := range latencyBoundsMS {
		if ms <= upper {
			index = i
			break
		}
	}
	cell.buckets[index]++
	return true
}

func (c *latencyCell) percentile(percent uint64) int64 {
	if c.count == 0 {
		return 0
	}
	// ceil(count*percent/100), without overflowing saturated lifetime counters.
	rank := c.count/100*percent + (c.count%100*percent+99)/100
	var covered uint64
	for index, count := range c.buckets {
		covered += count
		if covered >= rank {
			if index == len(latencyBoundsMS) || latencyBoundsMS[index] > c.maxMS {
				return c.maxMS
			}
			return latencyBoundsMS[index]
		}
	}
	return c.maxMS
}

func (m *latencyMetrics) snapshot() []LatencyPhaseSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := make([]LatencyPhaseSnapshot, 0, len(latencyLaneLabels)*len(latencyPhaseLabels))
	for lane := range m.cells {
		for phase := range m.cells[lane] {
			cell := &m.cells[lane][phase]
			if cell.count == 0 {
				continue
			}
			rows = append(rows, LatencyPhaseSnapshot{Lane: latencyLaneLabels[lane], Phase: latencyPhaseLabels[phase], Samples: cell.count, P50MS: cell.percentile(50), P95MS: cell.percentile(95), P99MS: cell.percentile(99), MaxMS: cell.maxMS, Saturated: cell.count == math.MaxUint64})
		}
	}
	return rows
}

// Valid keeps optional application implementations inside the same fixed,
// body-free owner response contract. It never authorizes a source or a read.
func (s LatencySnapshot) Valid() bool {
	if s.QueuedCount < 0 || s.QueuedCount > 256 || s.WaitingCount < 0 || s.WaitingCount > 256 || s.QueuedBytes < 0 || s.QueuedBytes > 64<<20 || s.WaitingBytes < 0 || s.WaitingBytes > 64<<20 || s.LivePending < 0 || s.LivePending > s.QueuedCount || s.HistoryPending < 0 || s.HistoryPending > s.QueuedCount-s.LivePending || s.OldestLiveAgeMS < 0 || s.LivePending == 0 && s.OldestLiveAgeMS != 0 || s.HistoryPrepared && s.HistoryPending == 0 || len(s.Phases) > len(latencyLaneLabels)*len(latencyPhaseLabels) {
		return false
	}
	var seen [len(latencyLaneLabels)][len(latencyPhaseLabels)]bool
	for _, row := range s.Phases {
		lane, phase := -1, -1
		for i, label := range latencyLaneLabels {
			if row.Lane == label {
				lane = i
			}
		}
		for i, label := range latencyPhaseLabels {
			if row.Phase == label {
				phase = i
			}
		}
		if lane < 0 || phase < 0 || seen[lane][phase] || row.Samples == 0 || row.P50MS < 0 || row.P95MS < row.P50MS || row.P99MS < row.P95MS || row.MaxMS < row.P99MS || row.Saturated != (row.Samples == math.MaxUint64) {
			return false
		}
		seen[lane][phase] = true
	}
	return true
}
