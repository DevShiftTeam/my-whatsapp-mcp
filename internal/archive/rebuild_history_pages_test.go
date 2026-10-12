//go:build sqlite_fts5

package archive

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeliveredHistoryPagesNewestFairDescendingAndRestart(t *testing.T) {
	s, a, e := deliveredWriterFixture(t)
	ctx := context.Background()
	newest := SourceAnchor{Key: e.Key, OriginalSentAt: e.OriginalSentAt.Add(time.Hour)}
	newest.Key.Message = "newest-z"
	other := newest
	other.Key.Chat = "z-page-chat"
	other.Key.Message = "other-newest"
	for _, anchor := range []SourceAnchor{newest, other} {
		if err := s.RecordRebuildDiscovery(ctx, anchor); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.NextDeliveredHistoryPage(ctx)
	if err != nil || first == nil || first.Anchor != newest {
		t.Fatal("did not seed newest first chat", first, err)
	}
	if err = s.RegisterDeliveredHistoryPage(ctx, *first); err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterDeliveredHistoryPage(ctx, *first); err == nil {
		t.Fatal("uncertain page retried")
	}
	second, err := s.NextDeliveredHistoryPage(ctx)
	if err != nil || second == nil || second.Anchor != other {
		t.Fatal("one waiting chat starved other", second, err)
	}
	if _, err = s.writeDB.Exec("UPDATE rebuild_operation SET last_attempt_at=0"); err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterDeliveredHistoryPage(ctx, *second); err != nil {
		t.Fatal(err)
	}
	if next, err := s.NextDeliveredHistoryPage(ctx); err != nil || next != nil {
		t.Fatal("no-progress page retried", next, err)
	}
	// Equal timestamp distinct full key advances only in a deterministic order.
	equal := newest
	equal.Key.Message = "newest-a"
	if err = s.ObserveDeliveredHistoryAnchor(ctx, a, equal); err != nil {
		t.Fatal(err)
	}
	third, err := s.NextDeliveredHistoryPage(ctx)
	if err != nil || third == nil || third.Anchor != equal {
		t.Fatal("equal-time distinct key failed", third, err)
	}
	if _, err = s.writeDB.Exec("UPDATE rebuild_operation SET last_attempt_at=0"); err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterDeliveredHistoryPage(ctx, *third); err != nil {
		t.Fatal(err)
	}
	if err = s.ObserveDeliveredHistoryAnchor(ctx, a, newest); err != nil {
		t.Fatal(err)
	}
	if next, err := s.NextDeliveredHistoryPage(ctx); err != nil || next != nil {
		t.Fatal("delayed newer chunk rewound cursor", next, err)
	}
	p, err := s.RebuildProgress(ctx)
	if err != nil || p.DiscoverySpent != 3 || p.PageAttempts != 3 || p.CopySpent != 0 {
		t.Fatal("wrong page credits", p, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	nextStore, err := Open(ctx, s.privateDir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer nextStore.Close()
	selection, err := nextStore.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{Policy: RebuildHistoryDelivered, AcceptDeliveredSnapshot: true})
	if err != nil || selection.StrategyEpoch != a.StrategyEpoch {
		t.Fatal("restart changed epoch", selection, err)
	}
	if next, err := nextStore.NextDeliveredHistoryPage(ctx); err != nil || next != nil {
		t.Fatal("restart retried unknown page", next, err)
	}
	older := equal
	older.Key.Message = "older-frontier"
	older.OriginalSentAt = older.OriginalSentAt.Add(-time.Minute)
	if err = nextStore.ObserveDeliveredHistoryAnchor(ctx, a, older); err != nil {
		t.Fatal(err)
	}
	resumed, err := nextStore.NextDeliveredHistoryPage(ctx)
	if err != nil || resumed == nil || resumed.Anchor != older {
		t.Fatal("older frontier failed after restart", resumed, err)
	}
}

func TestDeliveredHistoryPageStaleAndExhaustedRegistrationDoesNotSpend(t *testing.T) {
	for _, mode := range []string{"epoch", "policy", "paused", "budget", "metadata_full"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := deliveredWriterFixture(t)
			ctx := context.Background()
			page, err := s.NextDeliveredHistoryPage(ctx)
			if err != nil || page == nil {
				t.Fatal("missing seed", page, err)
			}
			switch mode {
			case "epoch":
				page.StrategyEpoch++
			case "policy":
				page.PolicyGeneration++
			case "paused":
				_, err = s.writeDB.Exec("UPDATE rebuild_operation SET state='paused'")
			case "budget":
				_, err = s.writeDB.Exec("UPDATE rebuild_operation SET discovery_spent=discovery_limit")
			case "metadata_full":
				_, err = s.writeDB.Exec("UPDATE rebuild_operation SET queue_full=1")
			}
			if err != nil {
				t.Fatal(err)
			}
			before := rebuildRows(t, s, "rebuild_operation", "discovery_spent,last_attempt_at")
			if err = s.RegisterDeliveredHistoryPage(ctx, *page); err == nil {
				t.Fatal("invalid page registered")
			}
			if before != rebuildRows(t, s, "rebuild_operation", "discovery_spent,last_attempt_at") {
				t.Fatal("rejected page spent credit")
			}
			var n int
			if err = s.readDB.QueryRow("SELECT count(*) FROM rebuild_history_pages").Scan(&n); err != nil || n != 0 {
				t.Fatal("rejected page persisted attempt", n, err)
			}
		})
	}
}

func TestDeliveredHistoryDurableOneSecondCadencePreservesSelectedTenSeconds(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		name := "selected-original"
		if delivered {
			name = "delivered-history"
		}
		t.Run(name, func(t *testing.T) {
			original, _, event := rebuildFixture(t)
			ctx := context.Background()
			other := SourceAnchor{Key: event.Key, OriginalSentAt: event.OriginalSentAt}
			other.Key.Chat = "z-cadence-chat"
			other.Key.Message = "second-cadence-anchor"
			if err := original.RecordRebuildDiscovery(ctx, other); err != nil {
				t.Fatal(err)
			}
			dir := original.privateDir
			if err := original.Close(); err != nil {
				t.Fatal(err)
			}
			base := time.Now().UTC().Truncate(time.Millisecond)
			var nowMillis atomic.Int64
			nowMillis.Store(base.UnixMilli())
			s, err := Open(ctx, dir, Options{Now: func() time.Time { return time.UnixMilli(nowMillis.Load()).UTC() }})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if delivered {
				if _, err = s.SelectRebuildHistoryPolicy(ctx, RebuildHistorySelectionOptions{Policy: RebuildHistoryDelivered, AcceptDeliveredSnapshot: true}); err != nil {
					t.Fatal(err)
				}
			}
			var first, second func() error
			if delivered {
				page, err := s.NextDeliveredHistoryPage(ctx)
				if err != nil || page == nil {
					t.Fatal("missing first delivered page", page, err)
				}
				first = func() error { return s.RegisterDeliveredHistoryPage(ctx, *page) }
				if err = first(); err != nil {
					t.Fatal(err)
				}
				next, err := s.NextDeliveredHistoryPage(ctx)
				if err != nil || next == nil {
					t.Fatal("missing distinct second delivered page", next, err)
				}
				second = func() error { return s.RegisterDeliveredHistoryPage(ctx, *next) }
			} else {
				page, err := s.NextRebuildDiscovery(ctx)
				if err != nil || page == nil {
					t.Fatal("missing selected discovery", page, err)
				}
				first = func() error { return s.RegisterRebuildDiscovery(ctx, *page) }
				if err = first(); err != nil {
					t.Fatal(err)
				}
				next, err := s.NextRebuildDiscovery(ctx)
				if err != nil || next == nil {
					t.Fatal("missing second selected discovery", next, err)
				}
				second = func() error { return s.RegisterRebuildDiscovery(ctx, *next) }
			}
			before := rebuildRows(t, s, "rebuild_operation", "discovery_spent,last_attempt_at,copy_spent,discovery_limit")
			nowMillis.Store(base.Add(999 * time.Millisecond).UnixMilli())
			if err = second(); err != ErrQueueLimitExceeded {
				t.Fatal("request before one second was not rejected", err)
			}
			if before != rebuildRows(t, s, "rebuild_operation", "discovery_spent,last_attempt_at,copy_spent,discovery_limit") {
				t.Fatal("early request changed accounting")
			}
			nowMillis.Store(base.Add(time.Second).UnixMilli())
			err = second()
			if delivered {
				if err != nil {
					t.Fatal("second delivered page denied at one second", err)
				}
			} else {
				if err != ErrQueueLimitExceeded {
					t.Fatal("selected-original ten-second cadence weakened", err)
				}
				nowMillis.Store(base.Add(10 * time.Second).UnixMilli())
				if err = second(); err != nil {
					t.Fatal("selected discovery denied at ten seconds", err)
				}
			}
			nowMillis.Store(base.Add(20 * time.Second).UnixMilli())
			if err = first(); err == nil {
				t.Fatal("new cadence retried an uncertain attempted frontier")
			}
			var spent, copySpent, limit int64
			if err = s.readDB.QueryRow("SELECT discovery_spent,copy_spent,discovery_limit FROM rebuild_operation WHERE id=1").Scan(&spent, &copySpent, &limit); err != nil || spent != 2 || copySpent != 0 || limit != DefaultRebuildDiscoveryLimit {
				t.Fatal("cadence reset or refilled credits", spent, copySpent, limit, err)
			}
		})
	}
}
