package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
)

type recoveryControlConnector struct {
	connector
	wakes, drains int
	drainErr      error
}

func (c *recoveryControlConnector) WakeRebuild()                       { c.wakes++ }
func (c *recoveryControlConnector) DrainRebuild(context.Context) error { c.drains++; return c.drainErr }
func (c *recoveryControlConnector) Status() ingestion.Status {
	return ingestion.Status{Connection: "disconnected"}
}

func TestRebuildOwnerControlsPreserveCASBudgetAndWakePolicy(t *testing.T) {
	ctx := context.Background()
	cfg, _ := syntheticLiveRebuild(t)
	if _, err := BootstrapLiveRebuild(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	a, err := New(ctx, Config{DataDir: cfg.DestinationProfileDir, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	oldRuntime := a.runtime
	runtime := &recoveryControlConnector{}
	a.runtime = runtime
	a.paused = false
	defer func() { a.runtime = oldRuntime }()
	progress, err := a.RebuildProgress(ctx)
	if err != nil || progress == nil {
		t.Fatal("missing initial operation")
	}
	initialCopyLimit, initialDiscoveryLimit := progress.CopyLimit, progress.DiscoveryLimit
	pause := archive.RebuildControl{ExpectedRevision: progress.Revision, CommandID: "12345678-1234-1234-1234-123456789aaa", Action: "pause"}
	progress, err = a.ControlRebuild(ctx, pause)
	if err != nil || progress.State != "paused" || runtime.drains != 1 || runtime.wakes != 0 {
		t.Fatalf("pause failed to join optional worker: %v", err)
	}
	resume := archive.RebuildControl{ExpectedRevision: progress.Revision, CommandID: "12345678-1234-1234-1234-123456789bbb", Action: "resume"}
	progress, err = a.ControlRebuild(ctx, resume)
	if err != nil || runtime.wakes != 1 {
		t.Fatalf("resume failed to wake: %v", err)
	}
	if _, err := a.ControlRebuild(ctx, pause); err != nil || runtime.drains != 1 || runtime.wakes != 1 {
		t.Fatal("replayed old pause affected newer resume")
	}
	stale := archive.RebuildControl{ExpectedRevision: pause.ExpectedRevision, CommandID: "12345678-1234-1234-1234-123456789ccc", Action: "extend"}
	if _, err := a.ControlRebuild(ctx, stale); !errors.Is(err, archive.ErrPolicyChanged) {
		t.Fatal("stale operation CAS accepted")
	}
	extend := archive.RebuildControl{ExpectedRevision: progress.Revision, CommandID: "12345678-1234-1234-1234-123456789ddd", Action: "extend"}
	first, err := a.ControlRebuild(ctx, extend)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.ControlRebuild(ctx, extend)
	if err != nil {
		t.Fatal(err)
	}
	if first.CopyLimit != second.CopyLimit || second.CopyLimit != initialCopyLimit+10000 || second.DiscoveryLimit != initialDiscoveryLimit+512 {
		t.Fatal("duplicate owner grant inflated budget")
	}
	policy, err := a.store.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	policy.DateMode = archive.DateSince
	policy.ImportStart = time.Now().Add(-24 * time.Hour)
	wakes := runtime.wakes
	updated, err := a.SetPolicy(ctx, policy, policy.Generation)
	if err != nil || runtime.wakes != wakes+1 {
		t.Fatalf("date save did not wake durable operation: %v", err)
	}
	progress, err = a.RebuildProgress(ctx)
	if err != nil || progress.PolicyGeneration != updated.Generation || progress.CopySpent != second.CopySpent || progress.CopyLimit != second.CopyLimit {
		t.Fatal("policy minted credits or lost operation")
	}
	setup, err := a.Setup(ctx)
	if err != nil || setup.Recovery == nil || setup.Recovery.Revision != progress.Revision {
		t.Fatal("owner setup omitted progress")
	}
	if _, err := a.SetPolicy(ctx, policy, policy.Generation); !errors.Is(err, archive.ErrPolicyChanged) || runtime.wakes != wakes+1 {
		t.Fatal("failed policy CAS woke worker")
	}
	paused := archive.RebuildControl{ExpectedRevision: progress.Revision, CommandID: "12345678-1234-1234-1234-123456789eee", Action: "pause"}
	runtime.drainErr = context.DeadlineExceeded
	if _, err := a.ControlRebuild(ctx, paused); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("unjoined attempt reported paused success")
	}
}

func TestOrdinaryProfileHasNoRebuildOperationOrControl(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx, Config{DataDir: shortNativeRoot(t), Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	progress, err := a.RebuildProgress(ctx)
	if err != nil || progress != nil {
		t.Fatal("ordinary profile received rebuild operation")
	}
	setup, err := a.Setup(ctx)
	if err != nil || setup.Recovery != nil {
		t.Fatal("ordinary setup received recovery counts")
	}
	if _, err := a.ControlRebuild(ctx, archive.RebuildControl{ExpectedRevision: 1, CommandID: "12345678-1234-1234-1234-123456789abc", Action: "extend"}); err == nil {
		t.Fatal("ordinary profile minted recovery budget")
	}
}
