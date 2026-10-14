package ingestion

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLatencyMetricsEmptyAndFiniteLabels(t *testing.T) {
	var metrics latencyMetrics
	if len(metrics.snapshot()) != 0 {
		t.Fatal("empty measurements fabricated latency")
	}
	if metrics.record(latencyLane(255), latencyQueue, time.Second) || metrics.record(latencyLive, latencyPhase(255), time.Second) {
		t.Fatal("unknown metric label was retained")
	}
	for lane := latencyLive; lane <= latencySource; lane++ {
		for phase := latencyAdmission; phase <= latencyPreparation; phase++ {
			if !metrics.record(lane, phase, time.Millisecond) {
				t.Fatal("known finite metric label refused")
			}
		}
	}
	rows := metrics.snapshot()
	if len(rows) != 24 {
		t.Fatal("metric storage exceeded or omitted its fixed cells")
	}
	if !(LatencySnapshot{Phases: rows}).Valid() {
		t.Fatal("generated measurements failed their public contract")
	}
	encoded, err := json.Marshal(LatencySnapshot{Phases: rows})
	if err != nil || len(encoded) > 16<<10 {
		t.Fatal("fixed metrics exceeded the owner response budget")
	}
	for _, forbidden := range []string{"message_id", "chat_id", "sender", "body", "token", "account", "path", "url"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatal("metrics contain a private-data field")
		}
	}
}

func TestLatencyMetricsPercentilesAndSmallSamples(t *testing.T) {
	var metrics latencyMetrics
	for i := 0; i < 95; i++ {
		metrics.record(latencyLive, latencySearchReady, 2*time.Millisecond)
	}
	for i := 0; i < 4; i++ {
		metrics.record(latencyLive, latencySearchReady, 200*time.Millisecond)
	}
	metrics.record(latencyLive, latencySearchReady, 5*time.Second)
	rows := metrics.snapshot()
	if len(rows) != 1 {
		t.Fatal("one phase created another metric cell")
	}
	row := rows[0]
	if row.Lane != "live" || row.Phase != "callback_to_commit" || row.Samples != 100 || row.P50MS != 5 || row.P95MS != 5 || row.P99MS != 250 || row.MaxMS != 5000 || row.Saturated {
		t.Fatal("fixed-bucket percentiles changed", row)
	}
	var small latencyMetrics
	small.record(latencyHistory, latencyDecode, time.Nanosecond)
	row = small.snapshot()[0]
	if row.P50MS != 1 || row.P95MS != 1 || row.P99MS != 1 || row.MaxMS != 1 {
		t.Fatal("submillisecond samples rounded to a misleading zero")
	}
	small.record(latencyHistory, latencyPreparation, -time.Second)
	row = small.snapshot()[1]
	if row.MaxMS != 0 || row.P95MS != 0 {
		t.Fatal("negative elapsed time escaped a bounded metric")
	}
}

func TestLatencyMetricsOverflowBucketAndSnapshotCopies(t *testing.T) {
	var metrics latencyMetrics
	metrics.record(latencySource, latencyProjection, 2*time.Hour)
	row := metrics.snapshot()[0]
	if row.P95MS != 7200000 || row.MaxMS != 7200000 {
		t.Fatal("overflow bucket understated a long wait")
	}
	rows := metrics.snapshot()
	rows[0].Lane = "PRIVATE_CANARY"
	rows[0].Samples = 0
	if next := metrics.snapshot()[0]; next.Lane != "source" || next.Samples != 1 {
		t.Fatal("snapshot mutation changed internal measurements")
	}
}

func TestLatencyMetricsConcurrentRecordAndSnapshot(t *testing.T) {
	var metrics latencyMetrics
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := 0; i < 1000; i++ {
				metrics.record(latencyLive, latencyCommit, 3*time.Millisecond)
				if !(LatencySnapshot{Phases: metrics.snapshot()}).Valid() {
					t.Error("concurrent snapshot invalid")
				}
			}
		}()
	}
	workers.Wait()
	row := metrics.snapshot()[0]
	if row.Samples != 8000 || row.MaxMS != 3 || row.P95MS != 3 {
		t.Fatal("concurrent measurement lost samples")
	}
}

func TestLatencyMetricCountersSaturateWithoutWrapping(t *testing.T) {
	var metrics latencyMetrics
	cell := &metrics.cells[latencyLive][latencyQueue]
	cell.count = math.MaxUint64 - 1
	cell.buckets[0] = math.MaxUint64 - 1
	metrics.record(latencyLive, latencyQueue, time.Millisecond)
	metrics.record(latencyLive, latencyQueue, 2*time.Millisecond)
	row := metrics.snapshot()[0]
	if row.Samples != math.MaxUint64 || !row.Saturated || row.MaxMS != 2 || row.P95MS != 1 {
		t.Fatal("saturated measurement wrapped or overflowed quantile arithmetic")
	}
}

func TestLatencySnapshotRejectsMalformedDiagnostics(t *testing.T) {
	row := LatencyPhaseSnapshot{Lane: "live", Phase: "queue_wait", Samples: 1, P50MS: 1, P95MS: 2, P99MS: 3, MaxMS: 4}
	valid := LatencySnapshot{Running: true, QueuedCount: 2, QueuedBytes: 100, LivePending: 1, HistoryPending: 1, HistoryPrepared: true, Phases: []LatencyPhaseSnapshot{row}}
	if !valid.Valid() {
		t.Fatal("ordinary bounded snapshot invalid")
	}
	for _, mutate := range []func(*LatencySnapshot){
		func(s *LatencySnapshot) { s.QueuedCount = -1 }, func(s *LatencySnapshot) { s.QueuedCount = 257 },
		func(s *LatencySnapshot) { s.QueuedBytes = -1 }, func(s *LatencySnapshot) { s.QueuedBytes = 64<<20 + 1 },
		func(s *LatencySnapshot) { s.WaitingCount = 257 }, func(s *LatencySnapshot) { s.WaitingBytes = -1 },
		func(s *LatencySnapshot) { s.LivePending = 3 }, func(s *LatencySnapshot) { s.HistoryPending = 3 },
		func(s *LatencySnapshot) { s.LivePending = math.MaxInt; s.HistoryPending = math.MaxInt },
		func(s *LatencySnapshot) { s.OldestLiveAgeMS = -1 }, func(s *LatencySnapshot) { s.HistoryPending = 0 },
		func(s *LatencySnapshot) { s.Phases[0].Lane = "PRIVATE_CANARY" }, func(s *LatencySnapshot) { s.Phases[0].Phase = "PRIVATE_CANARY" },
		func(s *LatencySnapshot) { s.Phases[0].Samples = 0 }, func(s *LatencySnapshot) { s.Phases[0].P95MS = -1 },
		func(s *LatencySnapshot) { s.Phases[0].P95MS = 0 }, func(s *LatencySnapshot) { s.Phases[0].P99MS = 5 },
		func(s *LatencySnapshot) { s.Phases[0].Saturated = true }, func(s *LatencySnapshot) { s.Phases = append(s.Phases, s.Phases[0]) },
		func(s *LatencySnapshot) { s.Phases = make([]LatencyPhaseSnapshot, 25) },
	} {
		candidate := valid
		candidate.Phases = append([]LatencyPhaseSnapshot(nil), valid.Phases...)
		mutate(&candidate)
		if candidate.Valid() {
			t.Fatal("malformed diagnostics accepted")
		}
	}
}
