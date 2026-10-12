//go:build sqlite_fts5

package archive

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func continuationRegister(t *testing.T, s *Store, key SourceKey, id string) {
	t.Helper()
	ctx := context.Background()
	p, err := s.RebuildProgress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Advance only the synthetic dispatch clock; production has a ten-second gate.
	if _, err = s.writeDB.Exec("UPDATE rebuild_operation SET last_attempt_at=0"); err != nil {
		t.Fatal(err)
	}
	if err = s.PendingRebuildRequest(ctx, RebuildRequest{ID: id, Generation: p.Generation, OperationID: p.OperationID, OperationRevision: p.Revision, Key: key, PolicyGeneration: p.PolicyGeneration, CreatedAt: s.now()}); err != nil {
		t.Fatal(err)
	}
}
func TestRebuildContinuationBeyond32RetainsSpendAcrossRestart(t *testing.T) {
	s, _, _ := rebuildFixtureSetup(t, func(src *Store, e SourceEvent) {
		p, _ := src.Policy(context.Background())
		for i := 0; i < 96; i++ {
			copy := e
			copy.Key.Message = fmt.Sprintf("continuation-%03d", i)
			apply(t, src, copy, p.Generation)
		}
	})
	for i := 0; i < 96; i++ {
		anchors, err := s.NextRebuildCandidates(context.Background(), 32)
		if err != nil || len(anchors) == 0 {
			t.Fatalf("stopped at %d: %v", i, err)
		}
		continuationRegister(t, s, anchors[0].Key, fmt.Sprintf("continuation-request-%d", i))
	}
	p, err := s.RebuildProgress(context.Background())
	if err != nil || p.CopySpent != 96 || p.CopyLimit != 10000 {
		t.Fatalf("unexpected progress %+v %v", p, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), s.privateDir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	p, err = reopened.RebuildProgress(context.Background())
	if err != nil || p.CopySpent != 96 {
		t.Fatal("restart reset spent")
	}
}
func TestRebuildControlCASIdempotentExtendAndCancel(t *testing.T) {
	s, _, e := rebuildFixture(t)
	ctx := context.Background()
	continuationRegister(t, s, e.Key, "pending-before-cancel")
	p, _ := s.RebuildProgress(ctx)
	control := RebuildControl{ExpectedRevision: p.Revision, CommandID: "extend-once", Action: "extend"}
	next, err := s.ControlRebuild(ctx, control)
	if err != nil || next.CopyLimit != p.CopyLimit+10000 {
		t.Fatal("extend failed")
	}
	again, err := s.ControlRebuild(ctx, control)
	if err != nil || again.CopyLimit != next.CopyLimit {
		t.Fatal("duplicate extend inflated budget")
	}
	if _, err = s.ControlRebuild(ctx, RebuildControl{ExpectedRevision: p.Revision, CommandID: "stale", Action: "pause"}); err == nil {
		t.Fatal("stale CAS accepted")
	}
	cancelled, err := s.ControlRebuild(ctx, RebuildControl{ExpectedRevision: next.Revision, CommandID: "cancel", Action: "cancel"})
	if err != nil || cancelled.State != "cancelled" {
		t.Fatal("cancel failed")
	}
	e.RebuildRequestID = "pending-before-cancel"
	policy, _ := s.Policy(ctx)
	if r := apply(t, s, e, policy.Generation); r.Outcome == OutcomeApplied {
		t.Fatal("cancelled request admitted")
	}
}
func TestRebuildPolicyRevisionRetainsSpentAndDuplicateKey(t *testing.T) {
	s, _, e := rebuildFixture(t)
	ctx := context.Background()
	continuationRegister(t, s, e.Key, "before-policy")
	before, _ := s.RebuildProgress(ctx)
	policy, _ := s.Policy(ctx)
	if _, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateSince, ImportStart: e.OriginalSentAt.Add(-time.Hour), ChatMode: ChatsAll}, policy.Generation); err != nil {
		t.Fatal(err)
	}
	after, err := s.RebuildProgress(ctx)
	if err != nil || after.CopySpent != before.CopySpent || after.CopyLimit != before.CopyLimit || after.Revision <= before.Revision {
		t.Fatal("policy reset operation")
	}
	if ok, err := s.MatchRebuildRequest(ctx, "before-policy", e.Key, before.Generation, after.PolicyGeneration); err != nil || ok {
		t.Fatal("stale policy matched")
	}
	if _, err = s.writeDB.Exec("UPDATE rebuild_operation SET last_attempt_at=0"); err != nil {
		t.Fatal(err)
	}
	if err = s.PendingRebuildRequest(ctx, RebuildRequest{ID: "new-id-same-key", Generation: after.Generation, OperationID: after.OperationID, OperationRevision: after.Revision, PolicyGeneration: after.PolicyGeneration, Key: e.Key, CreatedAt: s.now()}); err == nil {
		t.Fatal("policy change retried unknown key")
	}
}
func TestRebuildDiscoveryFairAcrossChatsAndNoCursorReplay(t *testing.T) {
	s, _, _ := rebuildFixtureSetup(t, func(src *Store, e SourceEvent) {
		p, _ := src.Policy(context.Background())
		for i := 0; i < 40; i++ {
			copy := e
			copy.Key.Chat = fmt.Sprintf("chat-%02d", i)
			copy.Key.Message = "anchor"
			apply(t, src, copy, p.Generation)
		}
	})
	ctx := context.Background()
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		d, err := s.NextRebuildDiscovery(ctx)
		if err != nil || d == nil {
			t.Fatalf("discovery stopped %d %v", i, err)
		}
		if seen[d.Anchor.Key.Chat] {
			t.Fatal("first chats monopolized discovery")
		}
		seen[d.Anchor.Key.Chat] = true
		if _, err = s.writeDB.Exec("UPDATE rebuild_operation SET last_attempt_at=0"); err != nil {
			t.Fatal(err)
		}
		if err = s.RegisterRebuildDiscovery(ctx, *d); err != nil {
			t.Fatal(err)
		}
		if err = s.RegisterRebuildDiscovery(ctx, *d); err == nil {
			t.Fatal("cursor resent")
		}
	}
}

func TestRebuildLegacy15cfUpgradeRetainsEvolvingArchiveAndSpend(t *testing.T) {
	s, receipt, e := rebuildFixtureSetup(t, func(src *Store, e SourceEvent) {
		p, _ := src.Policy(context.Background())
		other := e
		other.Key.Message = "already-returned"
		apply(t, src, other, p.Generation)
	})
	ctx := context.Background()
	continuationRegister(t, s, e.Key, "legacy-pending")
	other := e
	other.Key.Message = "already-returned"
	continuationRegister(t, s, other.Key, "legacy-consumed")
	other.RebuildRequestID = "legacy-consumed"
	p, _ := s.Policy(ctx)
	admitted := apply(t, s, other, p.Generation)
	guards := rebuildRows(t, s, "rebuild_guards", "account,chat,sender,message,from_me,expires_at,view_once,protocol_revision,revision_barrier")
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Exact original15cf request shape: no operation columns or tables.
	for _, query := range []string{"ALTER TABLE rebuild_requests RENAME TO continuation_requests", "CREATE TABLE rebuild_requests(id TEXT PRIMARY KEY,generation TEXT NOT NULL,account TEXT NOT NULL,chat TEXT NOT NULL,sender TEXT NOT NULL,message TEXT NOT NULL,from_me INTEGER NOT NULL,policy_generation INTEGER NOT NULL,created_at INTEGER NOT NULL,state TEXT NOT NULL CHECK(state IN('pending','consumed')),consumed_at INTEGER NOT NULL DEFAULT 0)", "INSERT INTO rebuild_requests SELECT id,generation,account,chat,sender,message,from_me,policy_generation,created_at,state,consumed_at FROM continuation_requests", "DROP TABLE continuation_requests", "DROP TABLE rebuild_operation", "DROP TABLE rebuild_commands", "DROP TABLE rebuild_discovery_attempts", "DROP TABLE rebuild_candidate_deferrals"} {
		if _, err = tx.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, s.privateDir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	progress, err := upgraded.RebuildProgress(ctx)
	if err != nil || progress.CopySpent != 2 || progress.Pending != 1 || progress.Accepted != 1 || progress.Generation != receipt.Generation {
		t.Fatalf("legacy upgrade lost evidence: %+v %v", progress, err)
	}
	if got := rebuildRows(t, upgraded, "rebuild_guards", "account,chat,sender,message,from_me,expires_at,view_once,protocol_revision,revision_barrier"); got != guards {
		t.Fatal("legacy guards changed")
	}
	var body string
	if err = upgraded.readDB.QueryRow("SELECT body FROM messages WHERE id=?", admitted.MessageID).Scan(&body); err != nil || body != other.Body {
		t.Fatal("evolving destination original lost")
	}
	if ok, err := upgraded.MatchRebuildRequest(ctx, "legacy-pending", e.Key, receipt.Generation, p.Generation); err != nil || !ok {
		t.Fatal("pending legacy evidence lost")
	}
	if err = upgraded.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(ctx, upgraded.privateDir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	progress, err = again.RebuildProgress(ctx)
	if err != nil || progress.CopySpent != 2 {
		t.Fatal("second restart reminted/reset budget")
	}
}

func TestRebuildDispatchCadenceAndPauseDoNotMintCredits(t *testing.T) {
	s, _, e := rebuildFixture(t)
	ctx := context.Background()
	before, _ := s.RebuildProgress(ctx)
	request := RebuildRequest{ID: "cadence-first", Generation: before.Generation, OperationID: before.OperationID, OperationRevision: before.Revision, PolicyGeneration: before.PolicyGeneration, Key: e.Key, CreatedAt: s.now()}
	if err := s.PendingRebuildRequest(ctx, request); err != nil {
		t.Fatal(err)
	}
	next, _ := s.RebuildProgress(ctx)
	request.ID = "cadence-second"
	if err := s.PendingRebuildRequest(ctx, request); err == nil {
		t.Fatal("immediate dispatch allowed")
	}
	after, _ := s.RebuildProgress(ctx)
	if after.CopySpent != next.CopySpent {
		t.Fatal("denied dispatch spent credit")
	}
	pause := RebuildControl{ExpectedRevision: after.Revision, CommandID: "pause-once", Action: "pause"}
	paused, err := s.ControlRebuild(ctx, pause)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := s.ControlRebuild(ctx, RebuildControl{ExpectedRevision: paused.Revision, CommandID: "resume-once", Action: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.ControlRebuild(ctx, pause)
	if err != nil || replay.Revision != resumed.Revision || replay.State == "paused" || replay.CopySpent != 1 {
		t.Fatal("old pause replay changed current operation")
	}
}

func TestRebuildMoreThanPerChatAnchorCapSurvivesLiveIngress(t *testing.T) {
	s, _, e := rebuildFixture(t)
	ctx := context.Background()
	for i := 0; i < maxAnchorsPerChat+20; i++ {
		a := SourceAnchor{Key: e.Key, OriginalSentAt: e.OriginalSentAt}
		a.Key.Message = fmt.Sprintf("saved-%04d", i)
		if err := s.RecordRebuildDiscovery(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := s.Policy(ctx)
	e.Key.Message = "new-live"
	e.OriginalSentAt = time.Now().Add(time.Second)
	apply(t, s, e, p.Generation)
	var n int
	if err := s.readDB.QueryRow("SELECT count(*) FROM source_anchors WHERE message LIKE 'saved-%'").Scan(&n); err != nil || n != maxAnchorsPerChat+20 {
		t.Fatalf("durable metadata pruned: %d %v", n, err)
	}
}

func TestRebuildDeferredInvalidWindowCannotStarveOtherKeys(t *testing.T) {
	s, _, e := rebuildFixtureSetup(t, func(src *Store, e SourceEvent) {
		p, _ := src.Policy(context.Background())
		for i := 0; i < 40; i++ {
			copy := e
			copy.Key.Chat = fmt.Sprintf("bad-%02d", i)
			copy.Key.Message = "oldest"
			apply(t, src, copy, p.Generation)
			copy.Key.Message = "newer"
			copy.OriginalSentAt = copy.OriginalSentAt.Add(time.Second)
			apply(t, src, copy, p.Generation)
		}
	})
	ctx := context.Background()
	anchors, err := s.NextRebuildCandidates(ctx, 32)
	if err != nil || len(anchors) != 32 {
		t.Fatal("missing firstwindow")
	}
	for _, a := range anchors {
		if err = s.DeferRebuildCandidate(ctx, a.Key); err != nil {
			t.Fatal(err)
		}
	}
	next, err := s.NextRebuildCandidates(ctx, 32)
	if err != nil || len(next) == 0 {
		t.Fatal("deferral stalled allwork")
	}
	foundNewer := false
	for _, a := range next {
		for _, deferred := range anchors {
			if a.Key == deferred.Key {
				t.Fatal("deferred key returned before backoff elapsed")
			}
		}
		if a.Key.Chat != e.Key.Chat && a.Key.Message == "newer" {
			foundNewer = true
		}
	}
	if !foundNewer {
		t.Fatal("bad oldest blocked every newer candidate")
	}
	progress, _ := s.RebuildProgress(ctx)
	if progress.CopySpent != 0 || progress.DiscoverySpent != 0 {
		t.Fatal("metadata selection minted/spent authority")
	}
}

func TestRebuildQueueFullStopsDiscoveryButPreservesCopyAndControls(t *testing.T) {
	s, _, e := rebuildFixture(t)
	ctx := context.Background()
	discovery, err := s.NextRebuildDiscovery(ctx)
	if err != nil || discovery == nil {
		t.Fatalf("missing initial discovery: %v", err)
	}
	if _, err = s.writeDB.Exec("UPDATE rebuild_operation SET queue_full=1"); err != nil {
		t.Fatal(err)
	}
	if next, err := s.NextRebuildDiscovery(ctx); err != nil || next != nil {
		t.Errorf("queue_full still selected discovery: %v", err)
	}
	if err = s.RegisterRebuildDiscovery(ctx, *discovery); err == nil {
		t.Error("queue_full still registered discovery")
	}
	candidates, err := s.NextRebuildCandidates(ctx, 32)
	if err != nil || len(candidates) == 0 {
		t.Fatalf("queue_full blocked known copies: %v", err)
	}
	continuationRegister(t, s, candidates[0].Key, "copy-after-queue-full")
	policy, err := s.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e.Key.Message = "live-after-queue-full"
	e.OriginalSentAt = s.now().Add(time.Second)
	if result := apply(t, s, e, policy.Generation); result.Outcome != OutcomeApplied {
		t.Fatalf("queue_full blocked live body: %s", result.Outcome)
	}
	if err = s.RecordRebuildRestriction(ctx, e.Key, time.Time{}, true); err != nil {
		t.Fatal(err)
	}
	guard, err := s.RebuildGuard(ctx, e.Key)
	if err != nil || !guard.ViewOnce {
		t.Fatalf("queue_full blocked negative control: %v", err)
	}
}
