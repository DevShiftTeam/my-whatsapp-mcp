//go:build sqlite_fts5

package archive

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func forwardBootstrapTest(t *testing.T, s *Store) (RebuildReceipt, string, error) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	hash, err := rebuildFileHash(filepath.Join(s.privateDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "forward")
	receipt, err := BootstrapRebuild(context.Background(), s.privateDir, dest, RebuildOptions{ExpectedAccount: "account", DeviceID: "synthetic-device", ExpectedSourceSHA256: hash, Boundary: time.Now().UTC()})
	after, hashErr := rebuildFileHash(filepath.Join(s.privateDir, "archive.sqlite"))
	if hashErr != nil || hash != after {
		t.Fatal("forward bootstrap changed source")
	}
	return receipt, dest, err
}

func TestRebuildForwardCarriesEvidenceWithoutBodiesOrBudgetRefill(t *testing.T) {
	s, prior, e := rebuildFixture(t)
	ctx := context.Background()
	for i := 0; i < 48; i++ {
		k := e.Key
		k.Message = fmt.Sprintf("forward-anchor-%d", i)
		if err := s.RecordRebuildDiscovery(ctx, SourceAnchor{Key: k, OriginalSentAt: e.OriginalSentAt}); err != nil {
			t.Fatal(err)
		}
	}
	continuationRegister(t, s, e.Key, "ancestor-consumed")
	p, _ := s.Policy(ctx)
	e.RebuildRequestID = "ancestor-consumed"
	apply(t, s, e, p.Generation)
	pending := e.Key
	pending.Message = "forward-anchor-0"
	continuationRegister(t, s, pending, "ancestor-pending")
	restricted := e.Key
	restricted.Message = "inherited-view-once"
	if err := s.RecordRebuildRestriction(ctx, restricted, time.Time{}, true); err != nil {
		t.Fatal(err)
	}
	discovery, err := s.NextRebuildDiscovery(ctx)
	if err != nil || discovery == nil {
		t.Fatal("missing ancestor discovery", err)
	}
	if _, err = s.writeDB.Exec("UPDATE rebuild_operation SET last_attempt_at=0"); err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterRebuildDiscovery(ctx, *discovery); err != nil {
		t.Fatal(err)
	}
	// Populate every authority table with current revisions, including a revoked
	// credential and an unknown outbound send whose identity must survive.
	var chatID string
	if err = s.readDB.QueryRow("SELECT id FROM chats WHERE account=? AND source_chat=?", e.Key.Account, e.Key.Chat).Scan(&chatID); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("%032x", 17)
	digest := make([]byte, 32)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO credentials(id,digest,account,label,principal,revision,all_chats,from_time,until_time,rolling,created_at,expires_at,revoked) VALUES(?,?,'account','forward','synthetic',7,0,1,2000000000000,0,1,0,1)", []any{id, digest}},
		{"INSERT INTO policy_chats VALUES(?)", []any{chatID}},
		{"INSERT INTO credential_chats VALUES(?,?)", []any{id, chatID}},
		{"INSERT INTO send_permissions VALUES(?,7,?)", []any{id, chatID}},
		{"INSERT INTO send_permission_modes VALUES(?,7,'selected',19)", []any{id}},
		{"INSERT INTO send_requests VALUES(?,'forward-unknown','account',?,?,'unchanged-protocol','outcome_unknown',0)", []any{id, chatID, digest}},
	} {
		if _, err = s.writeDB.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	core := map[string]bool{"accounts": true, "archive_policy": true, "policy_chats": true, "credentials": true, "credential_chats": true, "send_permissions": true, "send_permission_modes": true, "send_requests": true}
	expected := map[string]string{}
	for _, projection := range rebuildProjections {
		if core[projection.table] {
			expected[projection.table] = rebuildRows(t, s, projection.table, projection.columns)
		}
	}
	progress, _ := s.RebuildProgress(ctx)
	extend := RebuildControl{ExpectedRevision: progress.Revision, CommandID: "ancestor-extend", Action: "extend"}
	progress, err = s.ControlRebuild(ctx, extend)
	if err != nil {
		t.Fatal(err)
	}
	progress, err = s.ControlRebuild(ctx, RebuildControl{ExpectedRevision: progress.Revision, CommandID: "ancestor-pause", Action: "pause"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MarkUnsafe(ctx, "account", UnsafeReason("mutation_metadata_missing")); err != nil {
		t.Fatal(err)
	}
	receipt, dest, err := forwardBootstrapTest(t, s)
	if err != nil {
		t.Fatal("forward bootstrap:", err)
	}
	if receipt.Generation == prior.Generation {
		t.Fatal("observation was reused")
	}
	if len(receipt.Ancestors) != 1 || receipt.Ancestors[0] != prior.RebuildProvenance {
		t.Fatal("lineage missing or changed")
	}
	dst, err := Open(ctx, dest, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	for _, projection := range rebuildProjections {
		if core[projection.table] && rebuildRows(t, dst, projection.table, projection.columns) != expected[projection.table] {
			t.Fatalf("authority changed: %s", projection.table)
		}
	}
	for _, table := range []string{"messages", "message_fts", "dirty_windows", "sync_state"} {
		var n int
		if err = dst.readDB.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("copied old positive state %s: %d %v", table, n, err)
		}
	}
	next, err := dst.RebuildProgress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next.OperationID != progress.OperationID || next.CopyLimit != progress.CopyLimit || next.CopySpent != 2 || next.DiscoverySpent != 1 || next.Revision != progress.Revision+1 || next.State != "paused" || next.Accepted != 0 || next.Pending != 0 {
		t.Fatalf("operation reset: %+v", next)
	}
	again, err := dst.ControlRebuild(ctx, extend)
	if err != nil || again.CopyLimit != progress.CopyLimit || again.State != "paused" {
		t.Fatalf("old command replay granted/resumed: %+v %v", again, err)
	}
	g, err := dst.RebuildGuard(ctx, restricted)
	if err != nil || !g.ViewOnce {
		t.Fatal("lost inherited body-free negative")
	}
	if ok, err := dst.MatchRebuildRequest(ctx, "ancestor-pending", pending, receipt.Generation, next.PolicyGeneration); err != nil || ok {
		t.Fatal("ancestor request admitted")
	}
	next, err = dst.ControlRebuild(ctx, RebuildControl{ExpectedRevision: next.Revision, CommandID: "resume-forward", Action: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := dst.NextRebuildDiscovery(ctx); err != nil || d != nil {
		t.Fatal("ancestor discovery cursor retried", err)
	}
	anchors, err := dst.NextRebuildCandidates(ctx, 32)
	if err != nil || len(anchors) == 0 {
		t.Fatal("lost anchor union", err)
	}
	for _, a := range anchors {
		if a.Key == pending || a.Key == e.Key {
			t.Fatal("ancestor attempt was queued again")
		}
	}
	continuationRegister(t, dst, anchors[0].Key, "fresh-forward")
	fresh := e
	fresh.Key = anchors[0].Key
	fresh.RebuildRequestID = "fresh-forward"
	fresh.OriginalSentAt = anchors[0].OriginalSentAt
	if r := apply(t, dst, fresh, next.PolicyGeneration); r.Outcome != OutcomeApplied {
		t.Fatal("new exact response rejected", r)
	}
	if _, _, err = forwardBootstrapTest(t, dst); err == nil {
		t.Fatal("accepted third ancestor profile")
	}
}

func TestRebuildForwardLegacy15cfMigratesSpendOnce(t *testing.T) {
	s, _, e := rebuildFixture(t)
	ctx := context.Background()
	continuationRegister(t, s, e.Key, "legacy-forward-pending")
	for _, query := range []string{
		"ALTER TABLE rebuild_requests RENAME TO continuation_requests",
		"CREATE TABLE rebuild_requests(id TEXT PRIMARY KEY,generation TEXT NOT NULL,account TEXT NOT NULL,chat TEXT NOT NULL,sender TEXT NOT NULL,message TEXT NOT NULL,from_me INTEGER NOT NULL,policy_generation INTEGER NOT NULL,created_at INTEGER NOT NULL,state TEXT NOT NULL CHECK(state IN('pending','consumed')),consumed_at INTEGER NOT NULL DEFAULT 0)",
		"INSERT INTO rebuild_requests SELECT id,generation,account,chat,sender,message,from_me,policy_generation,created_at,state,consumed_at FROM continuation_requests",
		"DROP TABLE continuation_requests", "DROP TABLE rebuild_operation", "DROP TABLE rebuild_commands", "DROP TABLE rebuild_discovery_attempts", "DROP TABLE rebuild_candidate_deferrals",
	} {
		if _, err := s.writeDB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	_, dest, err := forwardBootstrapTest(t, s)
	if err != nil {
		t.Fatal("legacy forward rejected", err)
	}
	dst, err := Open(ctx, dest, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	progress, err := dst.RebuildProgress(ctx)
	if err != nil || progress.CopySpent != 1 || progress.CopyLimit != DefaultRebuildCopyLimit {
		t.Fatalf("legacy spend reset: %+v %v", progress, err)
	}
	if _, err = dst.writeDB.Exec("UPDATE rebuild_operation SET last_attempt_at=0"); err != nil {
		t.Fatal(err)
	}
	if err = dst.PendingRebuildRequest(ctx, RebuildRequest{ID: "retry-unknown", Generation: progress.Generation, OperationID: progress.OperationID, OperationRevision: progress.Revision, Key: e.Key, PolicyGeneration: progress.PolicyGeneration, CreatedAt: dst.now()}); err == nil {
		t.Fatal("legacy unknown key resent")
	}
}

func TestRebuildForwardRejectsUnknownSchemaAndGuardDateConflict(t *testing.T) {
	for _, kind := range []string{"extra_schema", "guard_date_conflict"} {
		t.Run(kind, func(t *testing.T) {
			s, _, e := rebuildFixture(t)
			ctx := context.Background()
			if kind == "extra_schema" {
				if _, err := s.writeDB.Exec("CREATE TABLE unrecognized_forward_object(id INTEGER)"); err != nil {
					t.Fatal(err)
				}
			} else {
				continuationRegister(t, s, e.Key, "latest-copy")
				p, _ := s.Policy(ctx)
				e.RebuildRequestID = "latest-copy"
				apply(t, s, e, p.Generation)
				if _, err := s.writeDB.Exec("UPDATE rebuild_guards SET original_sent_at=original_sent_at-1 WHERE message=?", e.Key.Message); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := forwardBootstrapTest(t, s)
			if err == nil {
				t.Fatal("accepted unsupported/conflicting forward source")
			}
		})
	}
}

func TestRebuildForwardEditRestrictionCannotBecomeNewerOriginal(t *testing.T) {
	for _, kind := range []SourceEventKind{EventEditInvalidation, EventEditContent} {
		t.Run(string(kind), func(t *testing.T) {
			t.Run("bootstrap", func(t *testing.T) {
				s, _, e := rebuildFixture(t)
				ctx := context.Background()
				continuationRegister(t, s, e.Key, "before-conflicting-metadata")
				policy, _ := s.Policy(ctx)
				e.RebuildRequestID = "before-conflicting-metadata"
				apply(t, s, e, policy.Generation)
				// A coherent SQL schema does not establish semantic consistency.
				// Admission rejects this combination; offline import must reject it too.
				if _, err := s.writeDB.Exec("UPDATE rebuild_guards SET source_kind=?,protocol_revision=1,revision_barrier=''", kind); err != nil {
					t.Fatal(err)
				}
				if _, err := s.writeDB.Exec("UPDATE messages SET source_kind=?,protocol_revision=2", EventOriginal); err != nil {
					t.Fatal(err)
				}
				if _, _, err := forwardBootstrapTest(t, s); err == nil {
					t.Fatal("forward import weakened inherited edit restriction")
				}
			})
			for _, originalFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("merge_original_first_%v", originalFirst), func(t *testing.T) {
					s, _, e := rebuildFixture(t)
					ctx := context.Background()
					tx, err := s.writeDB.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					if _, err = tx.Exec("DELETE FROM rebuild_guards"); err != nil {
						t.Fatal(err)
					}
					edit := RebuildGuard{ProtocolRevision: 1, SourceKind: kind}
					original := RebuildGuard{ProtocolRevision: 2, SourceKind: EventOriginal}
					first, second := edit, original
					if originalFirst {
						first, second = original, edit
					}
					if err = mergeForwardGuard(ctx, tx, e.Key, first); err != nil {
						t.Fatal(err)
					}
					if err = mergeForwardGuard(ctx, tx, e.Key, second); err == nil {
						t.Fatal("canonical merge weakened edit restriction")
					}
				})
			}
		})
	}
}

func TestRebuildForwardNewerEditPreservesNegativeUnionInBothOrders(t *testing.T) {
	for _, kind := range []SourceEventKind{EventEditInvalidation, EventEditContent} {
		for _, originalFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_original_first_%v", kind, originalFirst), func(t *testing.T) {
				s, _, e := rebuildFixture(t)
				ctx := context.Background()
				tx, err := s.writeDB.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err = tx.Exec("DELETE FROM rebuild_guards"); err != nil {
					t.Fatal(err)
				}
				expires := fromMillis(unixms(s.now().Add(time.Hour)))
				original := RebuildGuard{ProtocolRevision: 1, SourceKind: EventOriginal, ViewOnce: true, ExpiresAt: expires}
				edit := RebuildGuard{ProtocolRevision: 2, SourceKind: kind, Deleted: true, ExpiresAt: expires.Add(time.Hour)}
				first, second := edit, original
				if originalFirst {
					first, second = original, edit
				}
				if err = mergeForwardGuard(ctx, tx, e.Key, first); err != nil {
					t.Fatal(err)
				}
				if err = mergeForwardGuard(ctx, tx, e.Key, second); err != nil {
					t.Fatal("legitimate newer edit rejected", err)
				}
				guard, err := rebuildGuardQuery(ctx, tx, e.Key)
				if err != nil || guard.SourceKind != kind || guard.ProtocolRevision != 2 || !guard.ViewOnce || !guard.Deleted || !guard.ExpiresAt.Equal(expires) {
					t.Fatalf("newer edit weakened negative union: %+v %v", guard, err)
				}
			})
		}
	}
}
