//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

type unresolvedGroupState struct {
	revision ArchiveRevision
	epoch    uint64
	guards   int
	gaps     int
	tombs    int
}

func unresolvedGroupSnapshot(t *testing.T, s *Store) unresolvedGroupState {
	t.Helper()
	var state unresolvedGroupState
	if err := s.readDB.QueryRow("SELECT archive_revision,(SELECT count(*) FROM unresolved_deletions),(SELECT count(*) FROM sync_gaps),(SELECT count(*) FROM source_tombstones) FROM archive_meta WHERE id=1").Scan(&state.revision, &state.guards, &state.gaps, &state.tombs); err != nil {
		t.Fatal(err)
	}
	s.safetyMu.RLock()
	state.epoch = s.safetyEpoch
	s.safetyMu.RUnlock()
	return state
}

func TestUnresolvedGroupRevokeUnknownPersistsScopedNegativeOnly(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	const chat, message = "300@g.us", "unknown-group-revoke"
	otherID := readerApply(t, s, p, readerEvent(message, "400@g.us", "unknownrevokecrosschatreceipt", now.Add(-time.Minute)))
	before := unresolvedGroupSnapshot(t, s)
	if err := s.RecordUnresolvedDeletionIfUnknown(ctx, "account", chat, message); err != nil {
		t.Fatal(err)
	}
	after := unresolvedGroupSnapshot(t, s)
	if after.revision != before.revision+1 || after.epoch != before.epoch+1 || after.guards != before.guards+1 || after.gaps != before.gaps+1 || after.tombs != before.tombs {
		t.Fatalf("unknown guard did not atomically persist its gap and revision: before=%#v after=%#v", before, after)
	}
	for _, table := range []string{"messages", "source_anchors", "source_tombstones"} {
		var count int
		chatColumn, messageColumn := "chat", "message"
		if table == "messages" {
			chatColumn, messageColumn = "source_chat", "source_message"
		}
		if err := s.readDB.QueryRow("SELECT count(*) FROM "+table+" WHERE account=? AND "+chatColumn+"=? AND "+messageColumn+"=?", "account", chat, message).Scan(&count); err != nil || count != 0 {
			t.Fatalf("unknown guard invented source metadata in %s: count=%d err=%v", table, count, err)
		}
	}
	var observedOldest, observedLatest int64
	var guardedChat ChatID
	if err := s.readDB.QueryRow("SELECT id,observed_oldest,observed_latest FROM chats WHERE account=? AND source_chat=?", "account", chat).Scan(&guardedChat, &observedOldest, &observedLatest); err != nil || observedOldest != 0 || observedLatest != 0 {
		t.Fatal("unknown guard invented an original timestamp")
	}
	if err := s.RecordUnresolvedDeletionIfUnknown(ctx, "account", chat, message); err != nil || unresolvedGroupSnapshot(t, s) != after {
		t.Fatalf("exact marker replay changed the archive: %v", err)
	}
	for _, scope := range []ReadScope{unresolvedScope(p), readerScope(p, guardedChat)} {
		status, err := s.Status(ctx, scope)
		if err != nil || status.Paused || status.ReconciliationRequired || !slices.Contains(status.Codes, "unresolved_deletion") {
			t.Fatalf("guard did not leave a readable scoped gap: %#v %v", status, err)
		}
	}
	otherScope := readerScope(p, readerChat(t, s, otherID))
	status, err := s.Status(ctx, otherScope)
	if err != nil || slices.Contains(status.Codes, "unresolved_deletion") {
		t.Fatalf("group gap widened to unrelated scoped reads: %#v %v", status, err)
	}
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, dir, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, sender := range []string{"200@s.whatsapp.net", "777@s.whatsapp.net"} {
		for _, fromMe := range []bool{false, true} {
			for _, kind := range []SourceEventKind{EventOriginal, EventEditContent, EventDelete} {
				e := readerEvent(message, chat, "unknownrevokeforbiddenreceipt", now.Add(-time.Minute))
				e.Key.Sender, e.Key.FromMe, e.Kind, e.SourceRevision = sender, fromMe, kind, 9000
				if kind == EventDelete {
					e.Body = ""
				}
				result, err := s.ApplySourceEvent(ctx, e, p.Generation)
				if err != nil || result.Outcome != OutcomeStale || result.MessageID != "" || !result.Version.Deleted {
					t.Fatalf("unknown guard did not suppress a later source: %#v %v", result, err)
				}
			}
		}
	}
	targets, err := s.MutationTargets(ctx, "account", chat, message)
	if err != nil || len(targets) != 0 {
		t.Fatalf("suppressed originals became invented mutation targets: count=%d err=%v", len(targets), err)
	}
	before = unresolvedGroupSnapshot(t, s)
	if err := s.RecordUnresolvedDeletionIfUnknown(ctx, "account", chat, message); err != nil || unresolvedGroupSnapshot(t, s) != before {
		t.Fatalf("marker replay after suppressed originals changed the archive: %v", err)
	}
	unresolvedSearchCount(t, s, p, "unknownrevokeforbiddenreceipt", 0)
	unresolvedSearchCount(t, s, p, "unknownrevokecrosschatreceipt", 1)
}

func TestUnresolvedGroupRevokeRejectsInvalidScopeAndBounds(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("bind-account", "unknownrevokescopefixture", 1, EventOriginal)
	e.Key.Chat = "400@g.us"
	apply(t, s, e, p.Generation)
	before := unresolvedGroupSnapshot(t, s)
	for _, input := range []struct {
		account, chat, message string
		want                   error
	}{
		{"other-account", "300@g.us", "target", ErrUnauthorized},
		{"account", "300@s.whatsapp.net", "target", ErrInvalidEvent},
		{"account", "300@lid", "target", ErrInvalidEvent},
		{"account", "@g.us", "target", ErrInvalidEvent},
		{"account", "300@g.us/", "target", ErrInvalidEvent},
		{"account", "300@@g.us", "target", ErrInvalidEvent},
		{"account", "300 @g.us", "target", ErrInvalidEvent},
		{"", "300@g.us", "target", ErrInvalidEvent},
		{strings.Repeat("a", 513), "300@g.us", "target", ErrInvalidEvent},
		{"account", strings.Repeat("a", 508) + "@g.us", "target", ErrInvalidEvent},
		{"account", "300@g.us", "", ErrInvalidEvent},
		{"account", "300@g.us", strings.Repeat("m", 257), ErrInvalidEvent},
		{"account", "300@g.us", "nul\x00target", ErrInvalidEvent},
		{"account", "300@g.us", string([]byte{0xff}), ErrInvalidEvent},
	} {
		if err := s.RecordUnresolvedDeletionIfUnknown(context.Background(), input.account, input.chat, input.message); !errors.Is(err, input.want) {
			t.Fatalf("invalid scope/bounds returned %v, want %v", err, input.want)
		}
		if unresolvedGroupSnapshot(t, s) != before {
			t.Fatal("invalid scope/bounds changed source state")
		}
	}
	if _, err := s.writeDB.Exec("INSERT INTO identity_aliases(account,alias,canonical,provenance) VALUES('account','300@g.us','300@s.whatsapp.net','connector_mapping')"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUnresolvedDeletionIfUnknown(context.Background(), "account", "300@g.us", "target"); !errors.Is(err, ErrInvalidEvent) || unresolvedGroupSnapshot(t, s) != before {
		t.Fatalf("canonical non-group target was accepted: %v", err)
	}
}

func TestUnresolvedGroupRevokeRejectsEveryKnownSourceStateWithoutMutation(t *testing.T) {
	for _, fixture := range []string{"active", "other_author_and_direction", "multiple_targets", "invalidated", "excluded", "expired", "deleted", "anchor_only", "tombstone_only"} {
		t.Run(fixture, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			e := readerEvent("known-group-revoke", "300@g.us", "knownrevokepreservedreceipt", now.Add(-time.Minute))
			switch fixture {
			case "other_author_and_direction":
				e.Key.Sender, e.Key.FromMe = "777@s.whatsapp.net", true
			case "invalidated":
				e.Kind, e.Body, e.SourceRevision = EventEditInvalidation, "", 2
			case "expired":
				e.ExpiresAt = now.Add(-time.Second)
			case "deleted":
				e.Kind, e.Body = EventDelete, ""
			case "anchor_only":
				var err error
				p, err = s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective}, p.Generation)
				if err != nil {
					t.Fatal(err)
				}
			}
			result := apply(t, s, e, p.Generation)
			if fixture == "multiple_targets" {
				other := e
				other.Key.Sender, other.Key.FromMe = "777@s.whatsapp.net", true
				apply(t, s, other, p.Generation)
			}
			if fixture == "excluded" {
				var err error
				p, err = s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective}, p.Generation)
				if err != nil {
					t.Fatal(err)
				}
			}
			if fixture == "tombstone_only" {
				// An independent durable tombstone must count even without a
				// retained message or discovery anchor.
				if _, err := s.writeDB.Exec("INSERT INTO source_tombstones(account,chat,sender,message,from_me,protocol_revision) VALUES(?,?,?,?,?,?)", e.Key.Account, e.Key.Chat, e.Key.Sender, e.Key.Message, e.Key.FromMe, 7); err != nil {
					t.Fatal(err)
				}
				for _, statement := range []string{"DELETE FROM message_fts", "DELETE FROM messages", "DELETE FROM source_anchors"} {
					if _, err := s.writeDB.Exec(statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			metadata, err := s.SourceMetadata(ctx, e.Key)
			if err != nil {
				t.Fatal(err)
			}
			before := unresolvedGroupSnapshot(t, s)
			if err := s.RecordUnresolvedDeletionIfUnknown(ctx, e.Key.Account, e.Key.Chat, e.Key.Message); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("known %s target did not fail closed: %v", fixture, err)
			}
			if unresolvedGroupSnapshot(t, s) != before {
				t.Fatal("known-target rejection changed revision, safety epoch, gap or tombstone counts")
			}
			afterMetadata, err := s.SourceMetadata(ctx, e.Key)
			if err != nil || afterMetadata != metadata {
				t.Fatalf("known-target rejection changed source metadata: %v", err)
			}
			if fixture == "active" || fixture == "other_author_and_direction" || fixture == "multiple_targets" {
				page, err := s.Context(ctx, unresolvedScope(p), ContextRequest{MessageID: result.MessageID})
				if err != nil || len(page.Messages) != 1 || page.Messages[0].Body != e.Body {
					t.Fatalf("known-target rejection scrubbed retained plaintext: %v", err)
				}
			}
		})
	}
}

func TestUnresolvedGroupRevokeRejectsCanonicalRebuildGuard(t *testing.T) {
	var target SourceEvent
	s, _, _ := rebuildFixtureSetup(t, func(src *Store, e SourceEvent) {
		target = e
		target.Key.Chat, target.Key.Message = "300@g.us", "rebuild-known-group-revoke"
		p, err := src.Policy(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		apply(t, src, target, p.Generation)
	})
	if _, err := s.writeDB.Exec("INSERT INTO identity_aliases(account,alias,canonical,provenance) VALUES('account','300@g.us','400@g.us','connector_mapping')"); err != nil {
		t.Fatal(err)
	}
	targets, err := s.MutationTargets(context.Background(), "account", "400@g.us", target.Key.Message)
	if err != nil || len(targets) != 1 {
		t.Fatalf("fixture did not expose the equivalent canonical rebuild target: %v", err)
	}
	before := unresolvedGroupSnapshot(t, s)
	if err := s.RecordUnresolvedDeletionIfUnknown(context.Background(), "account", "400@g.us", target.Key.Message); !errors.Is(err, ErrSourceUnavailable) || unresolvedGroupSnapshot(t, s) != before {
		t.Fatalf("canonical rebuild target did not reject without mutation: %v", err)
	}
}

func TestUnresolvedGroupRevokeCanonicalCurrentTargetAndExactMessageKey(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("canonical-known-group-target", "canonicalgroupguardreceipt", 1, EventOriginal)
	e.Key.Chat = "400@g.us"
	apply(t, s, e, p.Generation)
	if _, err := s.writeDB.Exec("INSERT INTO identity_aliases(account,alias,canonical,provenance) VALUES('account','300@g.us','400@g.us','connector_mapping')"); err != nil {
		t.Fatal(err)
	}
	before := unresolvedGroupSnapshot(t, s)
	if err := s.RecordUnresolvedDeletionIfUnknown(ctx, "account", "300@g.us", e.Key.Message); !errors.Is(err, ErrSourceUnavailable) || unresolvedGroupSnapshot(t, s) != before {
		t.Fatalf("canonical current target did not reject without mutation: %v", err)
	}
	if err := s.RecordUnresolvedDeletionIfUnknown(ctx, "account", "300@g.us", "different-unknown-target"); err != nil {
		t.Fatal(err)
	}
	var canonicalGuards, aliasGuards int
	if err := s.readDB.QueryRow("SELECT (SELECT count(*) FROM unresolved_deletions WHERE chat='400@g.us'),(SELECT count(*) FROM unresolved_deletions WHERE chat='300@g.us')").Scan(&canonicalGuards, &aliasGuards); err != nil || canonicalGuards != 1 || aliasGuards != 0 {
		t.Fatal("unknown marker did not use only the canonical chat")
	}
	before = unresolvedGroupSnapshot(t, s)
	if err := s.RecordUnresolvedDeletionIfUnknown(ctx, "account", "400@g.us", "different-unknown-target"); err != nil || unresolvedGroupSnapshot(t, s) != before {
		t.Fatalf("canonical marker replay was not idempotent: %v", err)
	}
	unresolvedSearchCount(t, s, p, "canonicalgroupguardreceipt", 1)
}

func TestUnresolvedGroupRevokeRequiresPolicyAndSafeAccount(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	before := unresolvedGroupSnapshot(t, s)
	if err := s.RecordUnresolvedDeletionIfUnknown(ctx, "account", "300@g.us", "unknown-target"); !errors.Is(err, ErrInvalidPolicy) || unresolvedGroupSnapshot(t, s) != before {
		t.Fatalf("missing policy did not reject without mutation: %v", err)
	}
	testPolicy(t, s)
	if err := s.MarkUnsafe(ctx, "account", "history_corrupt"); err != nil {
		t.Fatal(err)
	}
	before = unresolvedGroupSnapshot(t, s)
	if err := s.RecordUnresolvedDeletionIfUnknown(ctx, "account", "300@g.us", "unknown-target"); !errors.Is(err, ErrReconciliationRequired) || unresolvedGroupSnapshot(t, s) != before {
		t.Fatalf("unsafe account accepted a new guard: %v", err)
	}
}

func TestUnresolvedGroupRevokeGapFailureRollsBackNegativeGuard(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	before := unresolvedGroupSnapshot(t, s)
	if _, err := s.writeDB.Exec("CREATE TRIGGER synthetic_group_revoke_gap_failure BEFORE INSERT ON sync_gaps BEGIN SELECT RAISE(ABORT,'synthetic_write_failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordUnresolvedDeletionIfUnknown(ctx, "account", "300@g.us", "unknown-target"); !errors.Is(err, ErrArchiveUnavailable) {
		t.Fatalf("gap persistence failure did not return the fixed boundary error: %v", err)
	}
	after := unresolvedGroupSnapshot(t, s)
	if after.guards != before.guards || after.gaps != before.gaps || after.tombs != before.tombs || after.revision != before.revision {
		t.Fatal("failed gap persistence left a partially committed negative guard")
	}
	if _, err := s.Search(ctx, unresolvedScope(p), SearchRequest{Query: "unknownreceipt"}); !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("failed negative persistence did not fail closed: %v", err)
	}
}

func TestUnresolvedGroupRevokeExactExistingGuardReplayDoesNotWiden(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("existing-group-guard", "existinggroupguardreceipt", 1, EventOriginal)
	e.Key.Chat = "300@g.us"
	apply(t, s, e, p.Generation)
	if err := s.RecordUnresolvedDeletion(context.Background(), e.Key.Account, e.Key.Chat, e.Key.Message); err != nil {
		t.Fatal(err)
	}
	before := unresolvedGroupSnapshot(t, s)
	if err := s.RecordUnresolvedDeletionIfUnknown(context.Background(), e.Key.Account, e.Key.Chat, e.Key.Message); err != nil || unresolvedGroupSnapshot(t, s) != before {
		t.Fatalf("exact previously established marker was not idempotent: %v", err)
	}
	other := e
	other.Key.Chat = "400@g.us"
	apply(t, s, other, p.Generation)
	before = unresolvedGroupSnapshot(t, s)
	if err := s.RecordUnresolvedDeletionIfUnknown(context.Background(), other.Key.Account, other.Key.Chat, other.Key.Message); !errors.Is(err, ErrSourceUnavailable) || unresolvedGroupSnapshot(t, s) != before {
		t.Fatalf("another chat's marker bypassed the unknown-target check: %v", err)
	}
	unresolvedSearchCount(t, s, p, "existinggroupguardreceipt", 1)
}

func TestUnresolvedGroupRevokeWriterTransactionOrdering(t *testing.T) {
	for _, originalInFlight := range []bool{true, false} {
		name := "priority_guard_overtakes_earlier_queued_original"
		if originalInFlight {
			name = "inflight_original_commits_before_guard_check"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			s := testArchive(t)
			p := testPolicy(t, s)
			before := unresolvedGroupSnapshot(t, s)
			e := event("atomic-group-target", "atomicgrouporiginalreceipt", 1, EventOriginal)
			e.Key.Chat = "300@g.us"
			blocked := e
			if !originalInFlight {
				blocked.Key.Message = "unrelated-writer-blocker"
			}
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(resume) }) }
			conn, err := s.writeDB.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = conn.Raw(func(raw any) error {
				return raw.(*sqlite3.SQLiteConn).RegisterFunc("synthetic_group_revoke_original_boundary", func() int {
					close(entered)
					<-resume
					return 0
				}, false)
			})
			if err == nil {
				_, err = conn.ExecContext(ctx, "CREATE TEMP TRIGGER synthetic_group_revoke_original_boundary BEFORE UPDATE OF body ON messages WHEN NEW.source_message='"+blocked.Key.Message+"' BEGIN SELECT synthetic_group_revoke_original_boundary(); END")
			}
			_ = conn.Close()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(release)
			type eventResult struct {
				value ApplyResult
				err   error
			}
			firstDone := make(chan eventResult, 1)
			go func() {
				value, err := s.ApplySourceEvent(ctx, blocked, p.Generation)
				firstDone <- eventResult{value, err}
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("original did not reach the real SQLite transaction boundary")
			}
			targets, err := s.MutationTargets(ctx, e.Key.Account, e.Key.Chat, e.Key.Message)
			if err != nil || len(targets) != 0 {
				t.Fatalf("fixture's separate lookup was not empty before commit: %v", err)
			}
			queuedDone := make(chan eventResult, 1)
			if !originalInFlight {
				go func() {
					value, err := s.ApplySourceEvent(ctx, e, p.Generation)
					queuedDone <- eventResult{value, err}
				}()
				waitArchiveCondition(t, func() bool { return len(s.bodies) == 1 })
			}
			guardDone := make(chan error, 1)
			go func() {
				guardDone <- s.RecordUnresolvedDeletionIfUnknown(ctx, e.Key.Account, e.Key.Chat, e.Key.Message)
			}()
			select {
			case control := <-s.controls:
				if _, ok := control.payload.(unresolvedDeletionCommand); !ok || !control.control || !control.sourceSensitive {
					t.Fatal("unknown deletion did not use the source-sensitive priority writer")
				}
				s.controls <- control
			case <-ctx.Done():
				t.Fatal("unknown deletion did not queue behind the in-flight transaction")
			}
			release()
			first := <-firstDone
			guardErr := <-guardDone
			if first.err != nil || first.value.Outcome != OutcomeApplied {
				t.Fatalf("in-flight original failed: %v", first.err)
			}
			if originalInFlight {
				if !errors.Is(guardErr, ErrSourceUnavailable) {
					t.Fatalf("transaction-local check missed the newly committed original: %v", guardErr)
				}
				after := unresolvedGroupSnapshot(t, s)
				if after.revision != first.value.ArchiveRevision || after.guards != before.guards || after.gaps != before.gaps || after.epoch != before.epoch {
					t.Fatal("race rejection changed source state or added an unresolved marker")
				}
				unresolvedSearchCount(t, s, p, "atomicgrouporiginalreceipt", 1)
			} else {
				queued := <-queuedDone
				if guardErr != nil || queued.err != nil || queued.value.Outcome != OutcomeStale || queued.value.MessageID != "" || !queued.value.Version.Deleted {
					t.Fatalf("priority guard failed to suppress the earlier queued original: guard=%v original=%#v err=%v", guardErr, queued.value, queued.err)
				}
				after := unresolvedGroupSnapshot(t, s)
				if queued.value.ArchiveRevision != first.value.ArchiveRevision+1 || after.revision != queued.value.ArchiveRevision || after.guards != before.guards+1 || after.gaps != before.gaps+1 || after.epoch != before.epoch+1 {
					t.Fatal("priority guard did not commit before the earlier queued original")
				}
				targets, err := s.MutationTargets(ctx, e.Key.Account, e.Key.Chat, e.Key.Message)
				if err != nil || len(targets) != 0 {
					t.Fatal("suppressed queued original invented source metadata")
				}
			}
		})
	}
}
