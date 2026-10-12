//go:build sqlite_fts5

package archive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkDeliveredHistoryImport10000 measures local storage, not phone delivery.
// Each iteration uses a separate disk-backed production Store and sealed rebuild
// bootstrap. Setup, event allocation, metadata anchors and validation are outside
// the timer; normal writer admission, FULL/WAL commits and FTS remain inside it.
func BenchmarkDeliveredHistoryImport10000(b *testing.B) {
	const count = 10000
	const body = "historybench synthetic plain text with searchable words and a fixed message body for durable archive throughput measurement."
	ctx := context.Background()
	var importElapsed, pagePlanningElapsed time.Duration
	b.SetBytes(int64(count * len(body)))
	b.StopTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		events := make([]SourceEvent, count)
		for i := range events {
			events[i] = event(fmt.Sprintf("history-benchmark-%05d", i), body, 1, EventOriginal)
		}
		s, admission := deliveredHistoryBenchmarkFixture(b, events)
		for i := range events {
			events[i].DeliveredHistory = admission
		}
		pageStarted := time.Now()
		page, err := s.NextDeliveredHistoryPage(ctx)
		pagePlanningElapsed += time.Since(pageStarted)
		if err != nil || page == nil {
			b.Fatalf("missing planned history page: %v", err)
		}

		b.StartTimer()
		started := time.Now()
		for i := range events {
			result, err := s.ApplySourceEvent(ctx, events[i], admission.PolicyGeneration)
			if err != nil || result.Outcome != OutcomeApplied {
				b.Fatalf("message %d: outcome=%s error=%v", i, result.Outcome, err)
			}
		}
		elapsed := time.Since(started)
		b.StopTimer()
		importElapsed += elapsed
		b.Logf("storage-only iteration=%d import_elapsed=%s page_plan_elapsed_total=%s", iteration+1, elapsed, pagePlanningElapsed)

		var active, receipts, indexed int
		if err := s.readDB.QueryRowContext(ctx, "SELECT count(*) FROM messages WHERE state='active'").Scan(&active); err != nil {
			b.Fatal(err)
		}
		if err := s.readDB.QueryRowContext(ctx, "SELECT count(*) FROM rebuild_history_bodies").Scan(&receipts); err != nil {
			b.Fatal(err)
		}
		if err := s.readDB.QueryRowContext(ctx, "SELECT count(*) FROM message_fts WHERE message_fts MATCH 'historybench'").Scan(&indexed); err != nil {
			b.Fatal(err)
		}
		progress, err := s.RebuildProgress(ctx)
		if err != nil || active != count || receipts != count || indexed != count || progress.ReadableMessages != count || progress.DeliveredAccepted != count || progress.DeliveredLimit != count {
			b.Fatalf("import verification: active=%d receipts=%d indexed=%d progress=%+v error=%v", active, receipts, indexed, progress, err)
		}
		if err = s.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(count*b.N)/importElapsed.Seconds(), "messages/s")
	b.ReportMetric(importElapsed.Seconds()/float64(b.N), "s/10000")
	b.ReportMetric(float64(pagePlanningElapsed.Microseconds())/1000/float64(b.N), "ms/page-plan")
	b.ReportMetric(count, "readable/iteration")
	b.ReportMetric(count, "accepted/iteration")
	b.ReportMetric(count, "indexed/iteration")
}

func deliveredHistoryBenchmarkFixture(t testing.TB, events []SourceEvent) (*Store, *DeliveredHistoryAdmission) {
	t.Helper()
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(root, "source")
	source, err := Open(ctx, sourceDir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	p, err := source.SetPolicy(ctx, ArchivePolicy{DateMode: DateSince, ImportStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ChatMode: ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	seed := events[0]
	if result, err := source.ApplySourceEvent(ctx, seed, p.Generation); err != nil || result.Outcome != OutcomeApplied {
		t.Fatalf("source seed: outcome=%s error=%v", result.Outcome, err)
	}
	// These test-only body-free source anchors give BootstrapRebuild a realistic
	// known-history manifest without spending time writing 10,000 source bodies.
	tx, err := source.writeDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(ctx, "INSERT OR IGNORE INTO source_anchors VALUES(?,?,?,?,?,?,1)")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if _, err = statement.ExecContext(ctx, e.Key.Account, e.Key.Chat, e.Key.Sender, e.Key.Message, e.Key.FromMe, unixms(e.OriginalSentAt)); err != nil {
			statement.Close()
			t.Fatal(err)
		}
	}
	if err = statement.Close(); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var anchors int
	if err = source.readDB.QueryRowContext(ctx, "SELECT count(*) FROM source_anchors").Scan(&anchors); err != nil || anchors != len(events) {
		t.Fatalf("wrong source metadata anchor count: %d %v", anchors, err)
	}
	if err = source.MarkUnsafe(ctx, "account", UnsafeReason("mutation_metadata_missing")); err != nil {
		t.Fatal(err)
	}
	if err = source.Close(); err != nil {
		t.Fatal(err)
	}
	sourceHash, err := rebuildFileHash(filepath.Join(sourceDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	destinationDir := filepath.Join(root, "destination")
	if _, err = BootstrapRebuild(ctx, sourceDir, destinationDir, RebuildOptions{ExpectedAccount: "account", ExpectedSourceSHA256: sourceHash, DeviceID: "synthetic-benchmark-device", Boundary: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	if after, err := rebuildFileHash(filepath.Join(sourceDir, "archive.sqlite")); err != nil || after != sourceHash {
		t.Fatalf("bootstrap changed source: %v", err)
	}
	destination, err := Open(ctx, destinationDir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { destination.Close() })
	if _, err = destination.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{Policy: RebuildHistoryDelivered, AcceptDeliveredSnapshot: true}); err != nil {
		t.Fatal(err)
	}
	admission, err := destination.CaptureDeliveredHistoryAdmission(ctx)
	if err != nil || admission == nil {
		t.Fatalf("missing captured admission: %v", err)
	}
	var journal string
	var synchronous, messages int
	if err = destination.writeDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("production WAL unavailable: %s %v", journal, err)
	}
	if err = destination.writeDB.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("production FULL durability unavailable: %d %v", synchronous, err)
	}
	if err = destination.readDB.QueryRowContext(ctx, "SELECT count(*) FROM messages").Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("bootstrap retained source bodies: %d %v", messages, err)
	}
	return destination, admission
}
