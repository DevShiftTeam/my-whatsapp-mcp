//go:build sqlite_fts5

package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func rebuildFixture(t *testing.T) (*Store, RebuildReceipt, SourceEvent) {
	return rebuildFixtureSetup(t, nil)
}
func rebuildFixtureSetup(t *testing.T, setup func(*Store, SourceEvent)) (*Store, RebuildReceipt, SourceEvent) {
	t.Helper()
	ctx := context.Background()
	src := testArchive(t)
	p := testPolicy(t, src)
	e := event("old", "private historical canary", 1, EventOriginal)
	apply(t, src, e, p.Generation)
	if setup != nil {
		setup(src, e)
	}
	if err := src.MarkUnsafe(ctx, "account", UnsafeReason("mutation_metadata_missing")); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(src.privateDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(before)
	dest := filepath.Join(filepath.Dir(src.privateDir), "rebuild")
	receipt, err := BootstrapRebuild(ctx, src.privateDir, dest, RebuildOptions{ExpectedAccount: "account", ExpectedSourceSHA256: hex.EncodeToString(hash[:]), DeviceID: "synthetic-device", Boundary: time.Now().UTC()})
	if err != nil {
		t.Fatal("bootstrap:", err)
	}
	after, _ := os.ReadFile(filepath.Join(src.privateDir, "archive.sqlite"))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("source changed")
	}
	dst, err := Open(ctx, dest, Options{})
	if err != nil {
		t.Fatal("destination open:", err)
	}
	t.Cleanup(func() { dst.Close() })
	return dst, receipt, e
}

func TestRebuildBootstrapWithholdsBodiesAndFreshRequestAdmitsOnce(t *testing.T) {
	s, receipt, e := rebuildFixture(t)
	ctx := context.Background()
	for _, table := range []string{"messages", "message_fts", "sync_state", "dirty_windows", "send_requests"} {
		var n int
		if err := s.readDB.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d %v", table, n, err)
		}
	}
	if receipt.OriginalFenceReason != "mutation_metadata_missing" || receipt.Generation == "" {
		t.Fatal("lost provenance")
	}
	p, err := s.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := apply(t, s, e, p.Generation)
	if r.Outcome != OutcomeFiltered {
		t.Fatalf("old unrequested body admitted: %#v", r)
	}
	req := RebuildRequest{ID: "fresh-request", Generation: receipt.Generation, Key: e.Key, PolicyGeneration: p.Generation, CreatedAt: time.Now().UTC()}
	if err = rebuildRegisterTest(s, ctx, req); err != nil {
		t.Fatal(err)
	}
	wrong := e.Key
	wrong.Sender = "other"
	if ok, err := s.MatchRebuildRequest(ctx, req.ID, wrong, req.Generation, p.Generation); err != nil || ok {
		t.Fatalf("wrong key matched %v %v", ok, err)
	}
	e.RebuildRequestID = req.ID
	r = apply(t, s, e, p.Generation)
	if r.Outcome != OutcomeApplied {
		t.Fatalf("fresh matched copy denied: %#v", r)
	}
	if ok, err := s.MatchRebuildRequest(ctx, req.ID, e.Key, req.Generation, p.Generation); err != nil || ok {
		t.Fatalf("request reusable %v %v", ok, err)
	}
}

func TestRebuildInheritedGuardsWinOverFreshRequests(t *testing.T) {
	for _, guard := range []string{"expiry", "view_once", "edit"} {
		t.Run(guard, func(t *testing.T) {
			s, receipt, e := rebuildFixture(t)
			ctx := context.Background()
			p, _ := s.Policy(ctx)
			switch guard {
			case "expiry":
				_, _ = s.writeDB.Exec("UPDATE rebuild_guards SET expires_at=?", time.Now().Add(-time.Hour).UnixMilli())
			case "view_once":
				_, _ = s.writeDB.Exec("UPDATE rebuild_guards SET view_once=1")
			case "edit":
				_, _ = s.writeDB.Exec("UPDATE rebuild_guards SET protocol_revision=8,source_kind='edit_content'")
			}
			if err := rebuildRegisterTest(s, ctx, RebuildRequest{ID: "guarded", Generation: receipt.Generation, Key: e.Key, PolicyGeneration: p.Generation, CreatedAt: time.Now()}); err != nil {
				return
			}
			e.RebuildRequestID = "guarded"
			r := apply(t, s, e, p.Generation)
			if r.Outcome == OutcomeApplied {
				t.Fatal("guard bypass")
			}
			var n int
			_ = s.readDB.QueryRow("SELECT count(*) FROM message_fts").Scan(&n)
			if n != 0 {
				t.Fatal("guarded searchable")
			}
		})
	}
}

func TestRebuildRefusesExistingDestination(t *testing.T) {
	s, _, _ := rebuildFixture(t)
	_, err := BootstrapRebuild(context.Background(), s.privateDir, s.privateDir, RebuildOptions{ExpectedAccount: "account", ExpectedSourceSHA256: string(make([]byte, 64)), DeviceID: "synthetic-device", Boundary: time.Now()})
	if err == nil {
		t.Fatal("existing destination accepted")
	}
}

func rebuildRows(t *testing.T, s *Store, table, columns string) string {
	t.Helper()
	rows, err := s.readDB.Query("SELECT " + columns + " FROM " + table + " ORDER BY " + columns)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all [][]any
	for rows.Next() {
		v := make([]any, len(strings.Split(columns, ",")))
		refs := make([]any, len(v))
		for i := range v {
			refs[i] = &v[i]
		}
		if err = rows.Scan(refs...); err != nil {
			t.Fatal(err)
		}
		all = append(all, v)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(all)
	return string(out)
}

func TestRebuildPreservesEightCoreTablesAndStableAliasesExactly(t *testing.T) {
	expected := map[string]string{}
	var oldCursor []byte
	core := map[string]bool{"accounts": true, "archive_policy": true, "policy_chats": true, "credentials": true, "credential_chats": true, "send_permissions": true, "send_permission_modes": true, "send_requests": true, "identities": true, "identity_aliases": true, "chat_aliases": true, "extension_original_budget": true}
	s, receipt, _ := rebuildFixtureSetup(t, func(src *Store, e SourceEvent) {
		must := func(query string, args ...any) {
			t.Helper()
			if _, err := src.writeDB.Exec(query, args...); err != nil {
				t.Fatal(err)
			}
		}
		must("INSERT INTO chats(id,account,source_chat,name) VALUES('stable-scope','account','extra-chat','private name')")
		must("INSERT INTO policy_chats VALUES('stable-scope')")
		must("INSERT INTO identity_aliases VALUES('account','synthetic@lid','sender','connector_mapping')")
		must("INSERT INTO chat_aliases VALUES('account','chat@lid','chat','connector_mapping')")
		for i, state := range []string{"accepted", "failed", "outcome_unknown"} {
			id := fmt.Sprintf("%032x", i+1)
			digest := make([]byte, 32)
			digest[0] = byte(i + 1)
			must("INSERT INTO credentials(id,digest,account,label,principal,revision,all_chats,from_time,until_time,rolling,created_at,expires_at,revoked) VALUES(?,?,'account',?,'synthetic-client',7,0,1,2000000000000,0,1,?,?)", id, digest, "client"+id, []int{0, 1, 0}[i], []int{0, 0, 1}[i])
			must("INSERT INTO credential_chats VALUES(?,'stable-scope')", id)
			must("INSERT INTO send_permissions VALUES(?,7,'stable-scope')", id)
			must("INSERT INTO send_permission_modes VALUES(?,7,?,19)", id, []string{"selected", "all", "none"}[i])
			must("INSERT INTO send_requests VALUES(?,?,'account','stable-scope',?,?,?,?)", id, "request"+id, digest, "protocol"+id, state, []int{77, 0, 0}[i])
		}
		must("UPDATE extension_original_budget SET maximum_bytes=1073741824,revision=9")
		for _, state := range []string{"accepted", "failed", "outcome_unknown"} {
			digest := make([]byte, 32)
			digest[0] = 1
			must("INSERT INTO send_requests VALUES(?,?,'account','stable-scope',?,?,?,0)", fmt.Sprintf("%032x", 1), "replay-"+state, digest, "saved-protocol-"+state, state)
		}
		for _, p := range rebuildProjections {
			if core[p.table] {
				expected[p.table] = rebuildRows(t, src, p.table, p.columns)
			}
		}
		expected["chat_ids"] = rebuildRows(t, src, "chats", "id,account,source_chat,name")
		if err := src.readDB.QueryRow("SELECT cursor_key FROM archive_meta").Scan(&oldCursor); err != nil {
			t.Fatal(err)
		}
	})
	for _, p := range rebuildProjections {
		if core[p.table] {
			if got := rebuildRows(t, s, p.table, p.columns); got != expected[p.table] {
				t.Fatalf("%s metadata changed", p.table)
			}
			if receipt.Tables[p.table].SHA256 == "" {
				t.Fatalf("missing manifest %s", p.table)
			}
		}
	}
	if got := rebuildRows(t, s, "chats", "id,account,source_chat,name"); got != expected["chat_ids"] {
		t.Fatal("opaque chat scope changed")
	}
	var n int
	if err := s.readDB.QueryRow("SELECT count(*) FROM chats WHERE observed_oldest<>0 OR observed_latest<>0").Scan(&n); err != nil || n != 0 {
		t.Fatal("old coverage copied")
	}
	var cursor []byte
	if err := s.readDB.QueryRow("SELECT cursor_key FROM archive_meta").Scan(&cursor); err != nil || reflect.DeepEqual(cursor, oldCursor) {
		t.Fatal("cursor identity reused")
	}
	grant, err := s.ResolveCredentialID(context.Background(), CredentialID(fmt.Sprintf("%032x", 1)))
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"accepted", "failed", "outcome_unknown"} {
		var digest [32]byte
		digest[0] = 1
		record, fresh, err := s.ClaimSend(context.Background(), grant, "stable-scope", "replay-"+state, digest, "must-not-be-used")
		if err != nil || fresh || record.State != state || record.ProtocolID != "saved-protocol-"+state {
			t.Fatalf("journal replay changed authority: fresh=%v state=%s err=%v", fresh, record.State, err)
		}
	}
}

func TestRebuildRejectsMalformedGuardMetadataBeforeReady(t *testing.T) {
	ctx := context.Background()
	src := testArchive(t)
	p := testPolicy(t, src)
	apply(t, src, event("malformed", "never copied", 1, EventOriginal), p.Generation)
	if _, err := src.writeDB.Exec("UPDATE messages SET source_kind='unrecognized'"); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	hash, err := rebuildFileHash(filepath.Join(src.privateDir, "archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(filepath.Dir(src.privateDir), "malformed-rebuild")
	if _, err = BootstrapRebuild(ctx, src.privateDir, destination, RebuildOptions{ExpectedAccount: "account", DeviceID: "synthetic-device", ExpectedSourceSHA256: hash, Boundary: time.Now()}); err == nil {
		t.Fatal("malformed scalar metadata published")
	}
	if _, err = Open(ctx, destination, Options{}); err == nil {
		t.Fatal("interrupted import opened")
	}
}

func TestRebuildImportsNegativeMetadataAndFiltersDiscovery(t *testing.T) {
	for _, kind := range []string{"tombstone", "clear", "unresolved", "expiry", "view_once", "edit"} {
		t.Run(kind, func(t *testing.T) {
			s, receipt, e := rebuildFixtureSetup(t, func(src *Store, e SourceEvent) {
				var err error
				switch kind {
				case "tombstone":
					_, err = src.writeDB.Exec("INSERT INTO source_tombstones VALUES('account','chat','sender','old',0,8)")
				case "clear":
					_, err = src.writeDB.Exec("INSERT INTO chat_tombstones VALUES('account','chat',?)", e.OriginalSentAt.UnixMilli())
				case "unresolved":
					_, err = src.writeDB.Exec("INSERT INTO unresolved_deletions VALUES('account','chat','old')")
				case "expiry":
					_, err = src.writeDB.Exec("UPDATE messages SET expires_at=1")
				case "view_once":
					_, err = src.writeDB.Exec("UPDATE messages SET view_once=1")
				case "edit":
					_, err = src.writeDB.Exec("UPDATE messages SET protocol_revision=8,source_kind='edit_content',revision_barrier='source_revision_conflict'")
				}
				if err != nil {
					t.Fatal(err)
				}
			})
			p, _ := s.Policy(context.Background())
			if kind != "edit" {
				anchors, err := s.BackfillAnchors(context.Background(), "account", 32)
				if err != nil || len(anchors) != 0 {
					t.Fatalf("unsafe anchor retained %v %v", anchors, err)
				}
			}
			g, err := s.RebuildGuard(context.Background(), e.Key)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "edit" && g.ProtocolRevision != 8 {
				t.Fatal("edit barrier lost")
			}
			err = rebuildRegisterTest(s, context.Background(), RebuildRequest{ID: "negative", Generation: receipt.Generation, Key: e.Key, PolicyGeneration: p.Generation, CreatedAt: time.Now()})
			if kind == "edit" && err != nil {
				t.Fatal(err)
			}
			e.RebuildRequestID = "negative"
			r := apply(t, s, e, p.Generation)
			if r.Outcome == OutcomeApplied {
				t.Fatal("negative metadata bypass")
			}
		})
	}
}

func TestRebuildRequestBudgetPersistsAndPolicyChangeInvalidates(t *testing.T) {
	s, receipt, e := rebuildFixture(t)
	ctx := context.Background()
	p, _ := s.Policy(ctx)
	for i := 0; i < 1; i++ {
		if err := rebuildRegisterTest(s, ctx, RebuildRequest{ID: fmt.Sprintf("request-%d", i), Generation: receipt.Generation, Key: e.Key, PolicyGeneration: p.Generation, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, s.privateDir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = rebuildRegisterTest(reopened, ctx, RebuildRequest{ID: "request-overflow", Generation: receipt.Generation, Key: e.Key, PolicyGeneration: p.Generation, CreatedAt: time.Now()}); err == nil {
		t.Fatal("same source key retried after restart")
	}
	p2, err := reopened.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsAll}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := reopened.MatchRebuildRequest(ctx, "request-0", e.Key, receipt.Generation, p2.Generation); err != nil || ok {
		t.Fatalf("stale policy matched %v %v", ok, err)
	}
}

func TestRebuildUnmatchedAndStaleCopiesKeepStricterRestrictions(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		for _, restriction := range []string{"expiry", "view_once"} {
			t.Run(fmt.Sprintf("admitted_%v_%s", admitted, restriction), func(t *testing.T) {
				s, receipt, e := rebuildFixture(t)
				ctx := context.Background()
				p, err := s.Policy(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if admitted {
					if err = rebuildRegisterTest(s, ctx, RebuildRequest{ID: "initial-copy", Generation: receipt.Generation, Key: e.Key, PolicyGeneration: p.Generation, CreatedAt: time.Now()}); err != nil {
						t.Fatal(err)
					}
					e.RebuildRequestID = "initial-copy"
					if result := apply(t, s, e, p.Generation); result.Outcome != OutcomeApplied {
						t.Fatal("initial copy not admitted")
					}
				}
				e.RebuildRequestID = ""
				e.SourceRevision = 0
				e.Body = "unmatched negative copy must never persist"
				if restriction == "expiry" {
					e.ExpiresAt = time.Now().Add(-time.Hour)
				} else {
					e.Media = []MediaReference{{Kind: "image", ViewOnce: true, Availability: "not_retained"}}
				}
				apply(t, s, e, p.Generation)
				g, err := s.RebuildGuard(ctx, e.Key)
				if err != nil {
					t.Fatal(err)
				}
				if restriction == "expiry" && (g.ExpiresAt.IsZero() || g.ExpiresAt.After(time.Now())) {
					t.Fatal("unmatched tighter expiry lost")
				}
				if restriction == "view_once" && !g.ViewOnce {
					t.Fatal("unmatched view-once restriction lost")
				}
				var count int
				if err = s.readDB.QueryRow("SELECT count(*) FROM message_fts").Scan(&count); err != nil || count != 0 {
					t.Fatalf("restricted old body still indexed: %d %v", count, err)
				}
				if err = s.readDB.QueryRow("SELECT count(*) FROM messages WHERE body<>'' OR caption<>''").Scan(&count); err != nil || count != 0 {
					t.Fatalf("restricted old body retained: %d %v", count, err)
				}
			})
		}
	}
}

func TestRebuildFutureExpiryRestrictionRevokesPriorVersion(t *testing.T) {
	s, receipt, e := rebuildFixture(t)
	ctx := context.Background()
	p, _ := s.Policy(ctx)
	if err := rebuildRegisterTest(s, ctx, RebuildRequest{ID: "future-expiry", Generation: receipt.Generation, Key: e.Key, PolicyGeneration: p.Generation, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	e.RebuildRequestID = "future-expiry"
	before := apply(t, s, e, p.Generation)
	expiry := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	if err := s.RecordRebuildRestriction(ctx, e.Key, expiry, false); err != nil {
		t.Fatal(err)
	}
	after, err := s.SourceMetadata(ctx, e.Key)
	if err != nil || !after.Version.ExpiresAt.Equal(expiry) || after.Version.Revision <= before.Version.Revision {
		t.Fatalf("tighter future expiry lost version boundary: %v", err)
	}
	if eligible, err := s.EligibleMedia(ctx, e.Key, before.Version); err != nil || eligible {
		t.Fatalf("old media version remained authorized: %v %v", eligible, err)
	}
	var body string
	if err = s.readDB.QueryRow("SELECT body FROM messages WHERE id=?", before.MessageID).Scan(&body); err != nil || body != e.Body {
		t.Fatal("unexpired original unexpectedly changed")
	}
	if err = s.RecordRebuildRestriction(ctx, e.Key, expiry.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	after, err = s.SourceMetadata(ctx, e.Key)
	if err != nil || !after.Version.ExpiresAt.Equal(expiry) {
		t.Fatal("expiry extended")
	}
}

func TestRebuildRestrictionPersistenceFailureFences(t *testing.T) {
	s, _, e := rebuildFixture(t)
	ctx := context.Background()
	if _, err := s.writeDB.Exec("CREATE TRIGGER synthetic_restriction_failure BEFORE INSERT ON rebuild_guards BEGIN SELECT RAISE(ABORT,'synthetic'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRebuildRestriction(ctx, e.Key, time.Now().Add(time.Hour), false); err == nil {
		t.Fatal("restriction failure ignored")
	}
	if _, err := s.readSafety(e.Key.Account); err == nil {
		t.Fatal("failed restriction did not fence")
	}
}

func rebuildRegisterTest(s *Store, ctx context.Context, r RebuildRequest) error {
	p, err := s.RebuildProgress(ctx)
	if err != nil {
		return err
	}
	r.OperationID = p.OperationID
	r.OperationRevision = p.Revision
	if _, err = s.writeDB.Exec("UPDATE rebuild_operation SET last_attempt_at=0"); err != nil {
		return err
	}
	return s.PendingRebuildRequest(ctx, r)
}
