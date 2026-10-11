package app

import (
	"context"
	"errors"
	"fmt"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/diskspace"
	"sort"
	"testing"
	"time"
)

type recoveryInventoryFixture struct {
	pins     map[archive.JobID]archive.PinnedExtraction
	requests []archive.ExtractionRecoveryRequest
	acks     []archive.JobRetry
	ackErr   error
}

func (f *recoveryInventoryFixture) ListPinnedExtractions(_ context.Context, request archive.ExtractionRecoveryRequest) (archive.ExtractionRecoveryPage, error) {
	f.requests = append(f.requests, request)
	var ids []string
	for id := range f.pins {
		if id > request.After {
			ids = append(ids, string(id))
		}
	}
	sort.Strings(ids)
	page := archive.ExtractionRecoveryPage{}
	count := len(ids)
	if count > request.Limit {
		count = request.Limit
	}
	for _, id := range ids[:count] {
		page.Items = append(page.Items, f.pins[archive.JobID(id)])
	}
	if len(ids) > count {
		page.NextAfter = page.Items[count-1].Lease.JobID
	}
	return page, nil
}
func (f *recoveryInventoryFixture) RetryExtraction(_ context.Context, request archive.JobRetry) error {
	f.acks = append(f.acks, request)
	if f.ackErr != nil {
		return f.ackErr
	}
	delete(f.pins, request.Lease.JobID)
	return nil
}
func recoveryFixture(count int) *recoveryInventoryFixture {
	f := &recoveryInventoryFixture{pins: map[archive.JobID]archive.PinnedExtraction{}}
	for i := 0; i < count; i++ {
		id := archive.JobID(fmt.Sprintf("job-%04d", i))
		f.pins[id] = archive.PinnedExtraction{Account: "account", Lease: archive.JobLease{JobID: id, LeaseID: archive.LeaseID(fmt.Sprintf("old-lease-%04d", i))}, MediaID: "media", ArtifactID: "artifact"}
	}
	return f
}
func TestExtractionStartupCapRetainsPinsAndResumesServerCursor(t *testing.T) {
	inventory := recoveryFixture(1056)
	recovery, err := recoverExtractionPins(context.Background(), inventory, "account")
	if err != nil || recovery.complete || len(inventory.acks) != 1024 || len(inventory.pins) != 32 || len(inventory.requests) != 32 {
		t.Fatal("startup cap was treated as EOF")
	}
	r := &resources{recovery: recovery}
	r.recoveryPending.Store(true)
	if _, err := r.Reserve(context.Background(), diskspace.Request{Class: diskspace.Original, MaximumGrowthBytes: 1}); !errors.Is(err, errExtractionRecovery) {
		t.Fatal("physical growth admitted during recovery")
	}
	done, err := recovery.step(context.Background(), inventory)
	if err != nil || !done || len(inventory.pins) != 0 {
		t.Fatal("bounded recovery did not resume")
	}
	if inventory.requests[32].After != inventory.acks[1023].Lease.JobID || inventory.requests[33].After != "" || inventory.requests[33].Limit != 1 {
		t.Fatal("resume cursor or final recheck missing")
	}
	for _, ack := range inventory.acks {
		if ack.Code != "cancelled" || ack.After != time.Second || ack.Terminal || ack.Lease.LeaseID == "" {
			t.Fatal("recovery forged a new capability or terminal failure")
		}
	}
}
func TestExtractionRecoveryAcknowledgementFailureRetainsExactOldPin(t *testing.T) {
	inventory := recoveryFixture(1)
	inventory.ackErr = errors.New("uncertain_ack")
	recovery, err := recoverExtractionPins(context.Background(), inventory, "account")
	if err == nil || recovery.complete || len(inventory.pins) != 1 {
		t.Fatal("uncertain cleanup released durable ownership")
	}
	old := inventory.acks[0].Lease
	inventory.ackErr = nil
	if done, err := recovery.step(context.Background(), inventory); err != nil || !done {
		t.Fatal(err)
	}
	if inventory.acks[1].Lease != old {
		t.Fatal("retry lost the old exact lease")
	}
}
func TestExtractionRecoveryRejectsCrossAccountMetadataWithoutAck(t *testing.T) {
	inventory := recoveryFixture(1)
	item := inventory.pins["job-0000"]
	item.Account = "other"
	inventory.pins["job-0000"] = item
	recovery, err := recoverExtractionPins(context.Background(), inventory, "account")
	if err == nil || recovery.complete || len(inventory.acks) != 0 || len(inventory.pins) != 1 {
		t.Fatal("cross-account inventory supplied cleanup authority")
	}
}
func TestCloseWaitsForRecoveryOwnerBeforeReleasingInstance(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	cancelled := make(chan struct{})
	a.recoveryOwner.cancel = func() {
		select {
		case <-cancelled:
		default:
			close(cancelled)
		}
	}
	a.recoveryOwner.done = done
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err = a.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("recovery did not receive cancellation")
	}
	if lease, err := appdata.Acquire(a.paths); err == nil {
		lease.Close()
		t.Fatal("instance released before recovery joined")
	}
	close(done)
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
