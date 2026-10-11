//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiagnosticStatusExposesOnlyAllowlistedUnsafeReasons(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []UnsafeReason{"history_limit_exceeded", "unsafe_mutation_projection",
		"app_state_sync_error", "undecryptable_message", "ambiguous_message_structure", "ambiguous_edit_structure"} {
		t.Run(string(reason), func(t *testing.T) {
			s := testArchive(t)
			p := testPolicy(t, s)
			apply(t, s, event("diagnostic", "private fixture body", 1, EventOriginal), p.Generation)
			if !IsDiagnosticFailureCode(string(reason)) || IsDiagnosticFailureCode("PRIVATE_ACCOUNT_OR_ERROR_TEXT") {
				t.Fatal("public diagnostic allowlist accepted an unknown code or rejected a fixed reason")
			}
			if err := s.MarkUnsafe(ctx, "account", reason); err != nil {
				t.Fatal(err)
			}
			if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", GapCode: string(reason), CoverageUnknown: true}); err != nil {
				t.Fatal(err)
			}
			check := func(want []string) {
				t.Helper()
				status, err := s.DiagnosticStatus(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !status.ReconciliationRequired || status.Connection != "reconciliation_required" {
					t.Fatal("unsafe archive did not report a blocked import")
				}
				if status.Messages != 0 || len(status.Chats) != 0 {
					t.Fatal("unsafe diagnostic exposed archive contents")
				}
				if !reflect.DeepEqual(status.Codes, want) {
					t.Fatalf("diagnostic reason codes = %v, want %v", status.Codes, want)
				}
			}
			check([]string{"reconciliation_required", string(reason)})
			// A corrupted or future database value must never become a public error.
			if _, err := s.writeDB.ExecContext(ctx, "UPDATE account_safety SET reason=? WHERE account=?", "PRIVATE_ACCOUNT_OR_ERROR_TEXT", "account"); err != nil {
				t.Fatal(err)
			}
			check([]string{"reconciliation_required"})
		})
	}
	t.Run("app_state_witness_is_diagnostic_only", func(t *testing.T) {
		s := testArchive(t)
		p := testPolicy(t, s)
		apply(t, s, event("diagnostic-witness", "private fixture body", 1, EventOriginal), p.Generation)
		for _, code := range []string{
			"app_state_collection_critical_block", "app_state_collection_critical_unblock_low",
			"app_state_collection_regular_low", "app_state_collection_regular_high",
			"app_state_collection_regular", "app_state_collection_unknown",
			"app_state_error_lthash", "app_state_error_patch_mac", "app_state_error_content_mac", "app_state_error_index_mac",
			"app_state_error_canceled", "app_state_error_deadline", "app_state_error_json_syntax", "app_state_error_json_type", "app_state_error_unknown",
			"app_state_full_sync", "app_state_incremental_sync",
		} {
			if !IsDiagnosticFailureCode(code) {
				t.Fatalf("fixed witness code rejected: %s", code)
			}
			if err := s.MarkUnsafe(ctx, "account", UnsafeReason(code)); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("diagnostic witness accepted as unsafe command: %s, %v", code, err)
			}
			if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", GapCode: code}); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("diagnostic witness accepted as coverage gap: %s, %v", code, err)
			}
		}
		for _, code := range []string{"app_state_collection_PRIVATE", "app_state_error_PRIVATE", "app_state_error_json_syntax:private", "PRIVATE_ACCOUNT_OR_ERROR_TEXT"} {
			if IsDiagnosticFailureCode(code) {
				t.Fatal("diagnostic witness allowlist accepted an unknown value")
			}
		}
		if _, err := os.Lstat(filepath.Join(s.privateDir, "unsafe.marker")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rejected witness created an unsafe marker: %v", err)
		}
		if err := s.MarkUnsafe(ctx, "account", "app_state_sync_error"); err != nil {
			t.Fatal(err)
		}
		// Witness codes cannot become persisted safety reasons through corruption.
		if _, err := s.writeDB.ExecContext(ctx, "UPDATE account_safety SET reason=? WHERE account=?", "app_state_collection_regular_high", "account"); err != nil {
			t.Fatal(err)
		}
		status, err := s.DiagnosticStatus(ctx)
		if err != nil || !status.ReconciliationRequired || status.Messages != 0 || len(status.Chats) != 0 || !reflect.DeepEqual(status.Codes, []string{"reconciliation_required"}) {
			t.Fatalf("diagnostic witness changed unsafe read authority: %+v, %v", status, err)
		}
	})
}

func TestReadableSourceRevisionConflictGapIsScopedAndPersistent(t *testing.T) {
	ctx := context.Background()
	fixture := func(t *testing.T, sameChat bool) (*Store, ArchivePolicy, ChatID, ChatID, time.Time) {
		t.Helper()
		s := testArchive(t)
		p := testPolicy(t, s)
		readable := event("gap-readable", "gapreadablecanary", 1, EventOriginal)
		readable.OriginalSentAt = time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
		r := apply(t, s, readable, p.Generation)
		conflict := event("gap-conflict", "gapconflictcanary", 1, EventOriginal)
		if !sameChat {
			conflict.Key.Chat = "conflict-chat"
		}
		conflict.OriginalSentAt = readable.OriginalSentAt.Add(-time.Hour)
		c := apply(t, s, conflict, p.Generation)
		var cleanChat, conflictChat ChatID
		if err := s.readDB.QueryRowContext(ctx, "SELECT chat_id FROM messages WHERE id=?", r.MessageID).Scan(&cleanChat); err != nil {
			t.Fatal(err)
		}
		if err := s.readDB.QueryRowContext(ctx, "SELECT chat_id FROM messages WHERE id=?", c.MessageID).Scan(&conflictChat); err != nil {
			t.Fatal(err)
		}
		conflict.Body = "conflicting replacement"
		if _, err := s.ApplySourceEvent(ctx, conflict, p.Generation); !errors.Is(err, ErrSourceRevisionConflict) {
			t.Fatalf("expected committed source conflict: %v", err)
		}
		if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", Connection: "connected", Received: 3, Applied: 1}); err != nil {
			t.Fatal(err)
		}
		return s, p, cleanChat, conflictChat, conflict.OriginalSentAt
	}
	check := func(t *testing.T, s *Store, scope ReadScope, wantPartial bool, wantMessages int64) {
		t.Helper()
		status, err := s.Status(ctx, scope)
		wantCoverage, wantCodes := CoverageState("unknown"), []string{}
		if wantPartial {
			wantCoverage, wantCodes = "partial", []string{"source_revision_conflict"}
		}
		if err != nil || status.ReconciliationRequired || status.Messages != wantMessages || status.Keyword.State != "ready" || status.Coverage != wantCoverage || !reflect.DeepEqual(status.Codes, wantCodes) {
			t.Fatalf("scoped conflict status = %+v, %v", status, err)
		}
	}
	t.Run("scope_and_restart", func(t *testing.T) {
		s, p, cleanChat, conflictChat, conflictTime := fixture(t, false)
		all := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
		check(t, s, all, true, 1)
		clean, conflicted := all, all
		clean.AllChats, clean.Chats = false, []ChatID{cleanChat}
		conflicted.AllChats, conflicted.Chats = false, []ChatID{conflictChat}
		check(t, s, clean, false, 1)
		check(t, s, conflicted, true, 0)
		outside := conflicted
		outside.From = conflictTime.Add(time.Nanosecond)
		check(t, s, outside, false, 0)
		outside.From, outside.Until = time.Time{}, conflictTime
		check(t, s, outside, false, 0)
		conflicted.From = conflictTime
		check(t, s, conflicted, true, 0)
		page, guard, err := s.SearchGuarded(ctx, all, SearchRequest{Query: "gapreadablecanary"})
		if err != nil || len(page.Hits) != 1 {
			t.Fatalf("committed conflict blocked another readable message: %v", err)
		}
		if _, err = s.CheckResponse(ctx, all, ResponseCheck{Guard: guard}); err != nil {
			t.Fatal(err)
		}
		page, err = s.Search(ctx, all, SearchRequest{Query: "gapconflictcanary"})
		if err != nil || len(page.Hits) != 0 {
			t.Fatalf("conflicting body stayed searchable: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(s.privateDir, "unsafe.marker")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("committed conflict created a global unsafe marker: %v", err)
		}
		dir := s.privateDir
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(ctx, dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		check(t, reopened, all, true, 1)
		if err := reopened.SetSyncStatus(ctx, SyncUpdate{Account: "account", Connection: "connected", GapCode: "undecryptable_message", CoverageUnknown: true}); err != nil {
			t.Fatal(err)
		}
		status, err := reopened.Status(ctx, all)
		if err != nil || status.Coverage != "partial" || !reflect.DeepEqual(status.Codes, []string{"undecryptable_message", "source_revision_conflict"}) {
			t.Fatalf("independent persisted gaps did not coexist: %+v, %v", status, err)
		}
	})
	t.Run("same_chat_date_scope", func(t *testing.T) {
		s, p, _, _, conflictTime := fixture(t, true)
		scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
		check(t, s, scope, true, 1)
		// A newer readable row in the same chat cannot reveal an older conflict.
		scope.From = conflictTime.Add(time.Nanosecond)
		check(t, s, scope, false, 1)
		p, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateSince, ImportStart: conflictTime.Add(time.Millisecond), ChatMode: ChatsAll}, p.Generation)
		if err != nil {
			t.Fatal(err)
		}
		scope.From, scope.PolicyGeneration = time.Time{}, p.Generation
		check(t, s, scope, false, 1)
	})
	for _, kind := range []string{"chat_policy", "date_policy"} {
		t.Run(kind, func(t *testing.T) {
			s, p, cleanChat, _, conflictTime := fixture(t, false)
			policy := ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective, IncludedChats: []ChatID{cleanChat}}
			if kind == "date_policy" {
				policy = ArchivePolicy{DateMode: DateSince, ImportStart: conflictTime.Add(time.Millisecond), ChatMode: ChatsAll}
			}
			p, err := s.SetPolicy(ctx, policy, p.Generation)
			if err != nil {
				t.Fatal(err)
			}
			check(t, s, ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, false, 1)
		})
	}
	for _, kind := range []string{"expiry", "rolling"} {
		for _, boundary := range []string{"finish", "response"} {
			t.Run(kind+"/"+boundary, func(t *testing.T) {
				now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
				s, p := readerFixture(t, &now)
				e := readerEvent("conflict-boundary", "chat", "private conflict canary", now.Add(-time.Hour+time.Minute))
				if kind == "expiry" {
					e.ExpiresAt = now.Add(time.Minute)
				}
				readerApply(t, s, p, e)
				e.Body = "conflicting replacement"
				if _, err := s.ApplySourceEvent(ctx, e, p.Generation); !errors.Is(err, ErrSourceRevisionConflict) {
					t.Fatalf("expected committed boundary conflict: %v", err)
				}
				scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
				if kind == "rolling" {
					scope.Rolling = time.Hour
				}
				if boundary == "finish" {
					var calls atomic.Int64
					s.options.Now = func() time.Time {
						if calls.Add(1) > 1 {
							return now.Add(2 * time.Minute)
						}
						return now
					}
					if _, err := s.Status(ctx, scope); !errors.Is(err, ErrSourceUnavailable) {
						t.Fatalf("status finish admitted an out-of-scope conflict: %v", err)
					}
					return
				}
				status, guard, err := s.StatusGuarded(ctx, scope)
				if err != nil || status.Messages != 0 || len(status.Chats) != 0 || !reflect.DeepEqual(status.Codes, []string{"source_revision_conflict"}) || len(guard.Sources) != 0 {
					t.Fatalf("conflict-only status exposed content or lacked its gap: %+v, %v", status, err)
				}
				if _, err = s.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); err != nil {
					t.Fatal(err)
				}
				now = now.Add(2 * time.Minute)
				if _, err = s.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
					t.Fatalf("response check admitted an out-of-scope conflict: %v", err)
				}
			})
		}
	}
}

func TestReadableUndecryptableGapPersistsWithoutBlockingArchive(t *testing.T) {
	ctx := context.Background()
	s := testArchive(t)
	p := testPolicy(t, s)
	apply(t, s, event("readable", "readablegapcanary", 1, EventOriginal), p.Generation)
	if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", Connection: "connected"}); err != nil {
		t.Fatal(err)
	}
	// Only the fixed account-wide gap enters the operational status. A chat
	// gap must not become an account-wide disclosure through this indicator.
	if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", Chat: "another-chat", GapCode: "undecryptable_message", CoverageUnknown: true}); err != nil {
		t.Fatal(err)
	}
	status, err := s.DiagnosticStatus(ctx)
	if err != nil || len(status.Codes) != 0 || status.Coverage != "unknown" {
		t.Fatalf("chat gap became a global diagnostic: %v, %v", status.Codes, err)
	}
	observation, err := s.BeginObservation(ctx, "account")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", Connection: "connected", GapCode: "undecryptable_message", CoverageUnknown: true}); err != nil {
		t.Fatal(err)
	}
	// A later ordinary sync update cannot erase the recorded missing coverage.
	if err := s.SetSyncStatus(ctx, SyncUpdate{Account: "account", Connection: "connected", Received: 2, Applied: 1}); err != nil {
		t.Fatal(err)
	}
	check := func(s *Store) {
		t.Helper()
		status, err := s.DiagnosticStatus(ctx)
		if err != nil || status.ReconciliationRequired || status.Connection != "connected" || status.Coverage != "partial" ||
			status.Messages != 1 || status.Keyword.State != "ready" || !reflect.DeepEqual(status.Codes, []string{"undecryptable_message"}) {
			t.Fatalf("readable gap status = %+v, error = %v", status, err)
		}
		scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
		page, guard, err := s.SearchGuarded(ctx, scope, SearchRequest{Query: "readablegapcanary"})
		if err != nil || len(page.Hits) != 1 {
			t.Fatalf("readable source blocked by gap: %v, %v", len(page.Hits), err)
		}
		if _, err := s.CheckResponse(ctx, scope, ResponseCheck{Guard: guard}); err != nil {
			t.Fatalf("readable gap response guard failed: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(s.privateDir, "unsafe.marker")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ordinary gap created an unsafe marker: %v", err)
		}
	}
	check(s)
	if err := s.EndObservation(ctx, observation); err != nil {
		t.Fatalf("ordinary gap prevented clean observation close: %v", err)
	}
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	check(reopened)
}
