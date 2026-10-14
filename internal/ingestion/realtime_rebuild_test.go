//go:build sqlite_fts5

package ingestion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// Selected-original history only discovers metadata. Hold that real store
// boundary, never fabricate a source apply to pretend it admitted an old body.
type realtimeDiscoveryBarrier struct{ *realtimeApplyBarrier }

func (s *realtimeDiscoveryBarrier) RecordRebuildDiscovery(ctx context.Context, anchor archive.SourceAnchor) error {
	if anchor.Key.Message == "realtime-held" {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.RecordRebuildDiscovery(ctx, anchor)
}

func TestRealtimePreparedHistoryPreservesBothRebuildStrategies(t *testing.T) {
	for _, strategy := range []archive.RebuildHistoryPolicy{archive.RebuildHistorySelectedOriginal, archive.RebuildHistoryDelivered} {
		t.Run(string(strategy), func(t *testing.T) {
			ctx := t.Context()
			source, sourceDir := durableFixture(t)
			policy, err := source.Policy(ctx)
			if err != nil {
				t.Fatal(err)
			}
			negativeKey := archive.SourceKey{Account: durableAccount, Chat: "200@s.whatsapp.net", Sender: "200@s.whatsapp.net", Message: "realtime-known-negative"}
			if _, err := source.ApplySourceEvent(ctx, archive.SourceEvent{Key: negativeKey, Kind: archive.EventDelete, SourceRevision: 2000000, BodyKind: archive.MessageUnsupported}, policy.Generation); err != nil {
				t.Fatal(err)
			}
			if err := source.MarkUnsafe(ctx, durableAccount, archive.UnsafeReason("mutation_metadata_missing")); err != nil {
				t.Fatal(err)
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(sourceDir, "archive.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(before)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(root, "archive")
			receipt, err := archive.BootstrapRebuild(ctx, sourceDir, destination, archive.RebuildOptions{ExpectedAccount: durableAccount, ExpectedSourceSHA256: hex.EncodeToString(digest[:]), DeviceID: durableAccount, Boundary: time.Unix(1500, 0)})
			if err != nil {
				t.Fatal(err)
			}
			store := durableStore(t, destination)
			if strategy == archive.RebuildHistoryDelivered {
				if _, err := store.SelectRebuildHistoryPolicy(ctx, archive.RebuildHistorySelectionOptions{Policy: strategy, CommandID: "synthetic-realtime-strategy", AcceptDeliveredSnapshot: true}); err != nil {
					t.Fatal(err)
				}
			}
			barrier := &realtimeDiscoveryBarrier{realtimeApplyBarrier: &realtimeApplyBarrier{Store: store, entered: make(chan struct{}), release: make(chan struct{})}}
			r := durableRuntime(t, barrier)
			r.config.Rebuild = &RebuildConfig{ObservationID: receipt.Generation, Account: receipt.Account, DeviceID: receipt.DeviceID, Boundary: receipt.Boundary, Requests: 0}
			r.config.RebuildHistoryPolicy = strategy
			if err := configureRebuild(ctx, &r.config, barrier); err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			release := func() { once.Do(func() { close(barrier.release) }) }
			t.Cleanup(release)
			if err := r.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			r.deliver(realtimeHistory(r, historyRecord("realtime-held", "rebuildheldreceipt"), historyRecord(negativeKey.Message, "rebuildnegativecanary")))
			awaitHistoryDrain(t, barrier.entered, "historical strategy store boundary")
			live := liveMessage("rebuild-fresh", "rebuildfreshreceipt")
			live.Info.Timestamp = time.Unix(2000, 0)
			r.deliver(live)
			deadline := time.Now().Add(time.Second)
			for {
				page, err := durableSearch(t, store, "rebuildfreshreceipt")
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Hits) == 1 {
					break
				}
				if !time.Now().Before(deadline) {
					t.Fatal("fresh plaintext waited for withheld historical work")
				}
				time.Sleep(time.Millisecond)
			}
			negative := liveMessage(negativeKey.Message, "rebuildnegativecanary")
			negative.Info.Timestamp = time.Unix(2000, 0)
			r.deliver(negative)
			release()
			if err := r.Disconnect(ctx); err != nil {
				t.Fatal(err)
			}
			page, err := durableSearch(t, store, "rebuildnegativecanary")
			if err != nil || len(page.Hits) != 0 {
				t.Fatal("realtime lane bypassed an inherited terminal restriction")
			}
			page, err = durableSearch(t, store, "rebuildheldreceipt")
			want := 0
			if strategy == archive.RebuildHistoryDelivered {
				want = 1
			}
			if err != nil || len(page.Hits) != want {
				t.Fatal("scheduling changed historical positive authority")
			}
			progress, err := store.RebuildDispatchState(ctx)
			if err != nil || progress.CopySpent != 0 || progress.DiscoverySpent != 0 || progress.Accepted != 0 {
				t.Fatal("realtime scheduling fabricated recovery proof or spent network allowance")
			}
			after, err := os.ReadFile(filepath.Join(sourceDir, "archive.sqlite"))
			if err != nil || sha256.Sum256(after) != digest {
				t.Fatal("preserved synthetic predecessor changed")
			}
		})
	}
}

func TestRealtimePreparedHistoryUsesCurrentLivePolicy(t *testing.T) {
	store, r, barrier, release := realtimeBarrier(t)
	r.deliver(realtimeHistory(r, historyRecord("realtime-held", "heldreceipt")))
	awaitHistoryDrain(t, barrier.entered, "prepared history")
	policy, err := store.Policy(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetPolicy(t.Context(), archive.ArchivePolicy{DateMode: archive.DateSince, ImportStart: time.Unix(1500, 0), ChatMode: archive.ChatsAll}, policy.Generation); err != nil {
		t.Fatal(err)
	}
	r.deliver(liveMessage("current-policy-filtered", "currentpolicycanary"))
	live := liveMessage("current-policy-permitted", "currentpolicyreceipt")
	live.Info.Timestamp = time.Unix(2000, 0)
	r.deliver(live)
	deadline := time.Now().Add(time.Second)
	for {
		page, err := durableSearch(t, store, "currentpolicyreceipt")
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Hits) == 1 {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("current authorized live body waited behind prepared history")
		}
		time.Sleep(time.Millisecond)
	}
	page, err := durableSearch(t, store, "currentpolicycanary")
	if err != nil || len(page.Hits) != 0 {
		t.Fatal("prepared history refreshed positive authority for a filtered live event")
	}
	release()
	if err := r.Disconnect(t.Context()); err != nil {
		t.Fatal(err)
	}
}
