//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"testing"
	"time"
)

func appStateGapRecord(t *testing.T, s *Store, account string) {
	t.Helper()
	if err := s.SetSyncStatus(context.Background(), SyncUpdate{Account: account, Connection: "connected", GapCode: "app_state_sync_error", CoverageUnknown: true}); err != nil {
		t.Fatal(err)
	}
}

func appStateGapAssertStatus(t *testing.T, status StatusSnapshot) {
	t.Helper()
	if status.Coverage != "partial" || status.Paused || status.ReconciliationRequired || status.Messages != 1 || len(status.Codes) != 1 || status.Codes[0] != "app_state_sync_error" {
		t.Fatalf("readable archive did not expose its state gap: %#v", status)
	}
}

func TestAppStateGapOwnerReadAndStatus(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	event := readerEvent("readable", "allowed", "synthetic original keyword שלום", now.Add(-time.Minute))
	id := readerApply(t, s, p, event)
	deniedID := readerApply(t, s, p, readerEvent("private", "denied", "synthetic original keyword private", now))
	scope := readerScope(p, readerChat(t, s, id))
	appStateGapRecord(t, s, scope.Account)
	status, err := s.Status(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	appStateGapAssertStatus(t, status)
	for _, kind := range []string{"search", "context"} {
		_, guard, _ := guardedBodyRead(t, s, scope, kind, event, id)
		if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
			t.Fatalf("local owner %s response blocked: %v", kind, err)
		}
	}
	if _, err = s.Context(context.Background(), scope, ContextRequest{MessageID: deniedID}); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("state gap broadened local chat scope: %v", err)
	}
	scope.Principal = ""
	if _, err = s.Search(context.Background(), scope, SearchRequest{Query: "synthetic"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("state gap authorized a missing local principal: %v", err)
	}
}

func TestAppStateGapCredentialStoredSnapshotReads(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	event := readerEvent("readable", "allowed", "synthetic original keyword שלום", now.Add(-time.Minute))
	id := readerApply(t, s, p, event)
	credential := createCredential(t, s, "state-gap", CredentialPolicy{Account: event.Key.Account, Chats: []ChatID{readerChat(t, s, id)}})
	grant, err := s.ResolveCredentialID(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	appStateGapRecord(t, s, grant.Scope.Account)
	for _, kind := range []string{"search", "context", "empty_search", "list"} {
		t.Run(kind, func(t *testing.T) {
			var guard ResponseGuard
			if kind == "empty_search" {
				page, g, err := s.SearchGuarded(context.Background(), grant.Scope, SearchRequest{Query: "absent"})
				if err != nil || len(page.Hits) != 0 {
					t.Fatalf("empty content search: %#v %v", page, err)
				}
				guard = g
			} else if kind == "list" {
				_, _, guard, _ = guardedMetadataRead(t, s, grant.Scope, kind)
			} else {
				_, guard, _ = guardedBodyRead(t, s, grant.Scope, kind, event, id)
			}
			if _, err := s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); err != nil {
				t.Fatalf("credential %s readable stored snapshot blocked by diagnostic gap: %v", kind, err)
			}
		})
	}
	status, guard, err := s.StatusGuarded(context.Background(), grant.Scope)
	if err != nil {
		t.Fatal(err)
	}
	appStateGapAssertStatus(t, status)
	if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); err != nil {
		t.Fatalf("credential status response blocked: %v", err)
	}
	if err = s.RevokeCredential(context.Background(), credential.ID, credential.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("stored snapshot bypassed credential revocation: %v", err)
	}
}

func TestAppStateGapCredentialFinalCheckAllowsNewDiagnosticGap(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	event := readerEvent("readable", "allowed", "synthetic original keyword", now.Add(-time.Minute))
	id := readerApply(t, s, p, event)
	credential := createCredential(t, s, "late-state-gap", CredentialPolicy{Account: event.Key.Account, Chats: []ChatID{readerChat(t, s, id)}})
	grant, err := s.ResolveCredentialID(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, guard, _ := guardedBodyRead(t, s, grant.Scope, "search", event, id)
	if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); err != nil {
		t.Fatalf("safe credential response blocked: %v", err)
	}
	appStateGapRecord(t, s, grant.Scope.Account)
	if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); err != nil {
		t.Fatalf("new diagnostic gap blocked readable stored snapshot: %v", err)
	}
}

func TestAppStateGapCredentialStatusFinalCheckAllowsNewDiagnosticGap(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	event := readerEvent("readable", "allowed", "synthetic original keyword", now.Add(-time.Minute))
	id := readerApply(t, s, p, event)
	credential := createCredential(t, s, "late-status-state-gap", CredentialPolicy{Account: event.Key.Account, Chats: []ChatID{readerChat(t, s, id)}})
	grant, err := s.ResolveCredentialID(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	status, guard, err := s.StatusGuarded(context.Background(), grant.Scope)
	if err != nil || len(status.Chats) != 1 || len(status.Codes) != 0 {
		t.Fatalf("normal credential status lost scoped metadata: %#v %v", status, err)
	}
	if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); err != nil {
		t.Fatalf("safe credential status blocked: %v", err)
	}
	appStateGapRecord(t, s, grant.Scope.Account)
	if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); err != nil {
		t.Fatalf("new diagnostic gap blocked scoped stored metadata: %v", err)
	}
}

func TestAppStateGapCredentialFinalCheckDeniesActualUnsafeWithSameCode(t *testing.T) {
	for _, kind := range []string{"search", "context", "list", "status"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			event := readerEvent("readable", "allowed", "synthetic original keyword", now.Add(-time.Minute))
			id := readerApply(t, s, p, event)
			credential := createCredential(t, s, "unsafe-state-gap", CredentialPolicy{Account: event.Key.Account, Chats: []ChatID{readerChat(t, s, id)}})
			grant, err := s.ResolveCredentialID(context.Background(), credential.ID)
			if err != nil {
				t.Fatal(err)
			}
			var guard ResponseGuard
			if kind == "list" || kind == "status" {
				_, _, guard, _ = guardedMetadataRead(t, s, grant.Scope, kind)
			} else {
				_, guard, _ = guardedBodyRead(t, s, grant.Scope, kind, event, id)
			}
			if err := s.MarkUnsafe(context.Background(), grant.Scope.Account, "app_state_sync_error"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); !errors.Is(err, ErrReconciliationRequired) {
				t.Fatalf("stored snapshot ignored actual unsafe state with the diagnostic code: %v", err)
			}
		})
	}
}

func TestAppStateGapCredentialFinalCheckHonorsDeleteAndExpiry(t *testing.T) {
	for _, mutation := range []string{"delete", "expiry"} {
		t.Run(mutation, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			event := readerEvent("readable", "allowed", "synthetic original keyword", now.Add(-time.Minute))
			if mutation == "expiry" {
				event.ExpiresAt = now.Add(time.Minute)
			}
			id := readerApply(t, s, p, event)
			credential := createCredential(t, s, "source-state-gap", CredentialPolicy{Account: event.Key.Account, Chats: []ChatID{readerChat(t, s, id)}})
			grant, err := s.ResolveCredentialID(context.Background(), credential.ID)
			if err != nil {
				t.Fatal(err)
			}
			appStateGapRecord(t, s, grant.Scope.Account)
			_, guard, _ := guardedBodyRead(t, s, grant.Scope, "context", event, id)
			if mutation == "delete" {
				event.Kind, event.SourceRevision, event.Body = EventDelete, 2, ""
				readerApply(t, s, p, event)
			} else {
				now = event.ExpiresAt
			}
			if _, err := s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("credential stored snapshot retained %s source authority: %v", mutation, err)
			}
		})
	}
}

func TestAppStateGapLocalReadHonorsDeleteAndExpiry(t *testing.T) {
	for _, mutation := range []string{"delete", "expiry"} {
		t.Run(mutation, func(t *testing.T) {
			now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			event := readerEvent("readable", "allowed", "synthetic original keyword", now.Add(-time.Minute))
			if mutation == "expiry" {
				event.ExpiresAt = now.Add(time.Minute)
			}
			id := readerApply(t, s, p, event)
			scope := readerScope(p, readerChat(t, s, id))
			appStateGapRecord(t, s, scope.Account)
			_, guard, _ := guardedBodyRead(t, s, scope, "context", event, id)
			if mutation == "delete" {
				event.Kind, event.SourceRevision, event.Body = EventDelete, 2, ""
				readerApply(t, s, p, event)
			} else {
				now = event.ExpiresAt
			}
			if _, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("local state-gap read retained %s authority: %v", mutation, err)
			}
			page, err := s.Search(context.Background(), scope, SearchRequest{Query: "synthetic"})
			if err != nil || len(page.Hits) != 0 {
				t.Fatalf("local search exposed %s source: %#v %v", mutation, page, err)
			}
			if _, err = s.Context(context.Background(), scope, ContextRequest{MessageID: id}); !errors.Is(err, ErrSourceUnavailable) {
				t.Fatalf("local context exposed %s source: %v", mutation, err)
			}
		})
	}
}

func TestAppStateGapOtherArchiveCredentialUnaffected(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	gapped, _ := readerFixture(t, &now)
	appStateGapRecord(t, gapped, "account")
	s, p := readerFixture(t, &now)
	event := readerEvent("readable", "allowed", "synthetic original keyword", now.Add(-time.Minute))
	event.Key.Account = "other-account"
	id := readerApply(t, s, p, event)
	credential := createCredential(t, s, "other-state-gap-account", CredentialPolicy{Account: event.Key.Account, Chats: []ChatID{readerChat(t, s, id)}})
	grant, err := s.ResolveCredentialID(context.Background(), credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, guard, _ := guardedBodyRead(t, s, grant.Scope, "search", event, id)
	if _, err = s.CheckResponse(context.Background(), grant.Scope, ResponseCheck{Guard: guard, Credential: &grant}); err != nil {
		t.Fatalf("other account inherited a state gap: %v", err)
	}
	status, err := s.Status(context.Background(), grant.Scope)
	if err != nil || status.Coverage != "unknown" || len(status.Codes) != 0 || status.Messages != 1 {
		t.Fatalf("other account inherited gap status: %#v %v", status, err)
	}
}
