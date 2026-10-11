//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestSendPermissionAllPersistsIncludesFutureAndPreservesCredential(t *testing.T) {
	s, policy, now, chat, other := credentialFixture(t)
	ctx := context.Background()
	credential := createCredential(t, s, "all-send", CredentialPolicy{Account: "account", AllChats: true})
	grant, err := s.ResolveCredentialID(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	permission, err := s.GetSendPermission(ctx, grant)
	if err != nil || permission.Mode != "none" || permission.Revision != 0 || len(permission.Chats) != 0 {
		t.Fatalf("default send authority: %#v %v", permission, err)
	}
	if err = s.SetSendPermissionMode(ctx, grant, SendPermission{Mode: "all"}, 0); err != nil {
		t.Fatal(err)
	}
	future := readerChat(t, s, readerApply(t, s, policy, readerEvent("future-send", "future", "synthetic future message", now.Add(-time.Second))))
	for _, target := range []ChatID{chat, other, future} {
		if _, err = s.SendTarget(ctx, grant, target); err != nil {
			t.Fatalf("all grant omitted permitted target: %v", err)
		}
	}
	s = reopenCredentialStore(t, s, now)
	current, err := s.ResolveCredentialID(ctx, credential.ID)
	if err != nil || !reflect.DeepEqual(grant, current) {
		t.Fatalf("permission edit changed read credential: %v", err)
	}
	permission, err = s.GetSendPermission(ctx, current)
	if err != nil || permission.Mode != "all" || permission.Revision != 1 || len(permission.Chats) != 0 {
		t.Fatalf("permission restart: %#v %v", permission, err)
	}
	if _, err = s.SendTarget(ctx, current, future); err != nil {
		t.Fatal(err)
	}
}

func TestSendPermissionAllRetainsReadScopeAndArchivePolicy(t *testing.T) {
	s, policy, _, chat, other := credentialFixture(t)
	ctx := context.Background()
	finite := createCredential(t, s, "finite-all-send", CredentialPolicy{Account: "account", Chats: []ChatID{chat}})
	grant, _ := s.ResolveCredentialID(ctx, finite.ID)
	if err := s.SetSendPermissionMode(ctx, grant, SendPermission{Mode: "all"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendTarget(ctx, grant, other); !errors.Is(err, ErrSendForbidden) {
		t.Fatalf("all escaped credential chat scope: %v", err)
	}
	all := createCredential(t, s, "policy-all-send", CredentialPolicy{Account: "account", AllChats: true})
	allGrant, _ := s.ResolveCredentialID(ctx, all.ID)
	if err := s.SetSendPermissionMode(ctx, allGrant, SendPermission{Mode: "all"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPolicy(ctx, ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective, IncludedChats: []ChatID{chat}}, policy.Generation); err != nil {
		t.Fatal(err)
	}
	current, err := s.ResolveCredentialID(ctx, all.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SendTarget(ctx, current, other); !errors.Is(err, ErrSendForbidden) {
		t.Fatalf("all escaped archive policy: %v", err)
	}
	if _, err = s.SendTarget(ctx, current, chat); err != nil {
		t.Fatal(err)
	}
}

func TestSendPermissionModesCASAndRevisionInvalidation(t *testing.T) {
	s, _, _, chat, other := credentialFixture(t)
	ctx := context.Background()
	credential := createCredential(t, s, "send-cas", CredentialPolicy{Account: "account", AllChats: true})
	grant, _ := s.ResolveCredentialID(ctx, credential.ID)
	if err := s.SetSendPermissionMode(ctx, grant, SendPermission{Mode: "all"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSendPermissionMode(ctx, grant, SendPermission{Mode: "none"}, 0); !errors.Is(err, ErrSendConflict) {
		t.Fatalf("stale permission edit accepted: %v", err)
	}
	if _, err := s.SendTarget(ctx, grant, other); err != nil {
		t.Fatalf("known CAS rejection poisoned grant: %v", err)
	}
	if err := s.SetSendPermissionMode(ctx, grant, SendPermission{Mode: "selected", Chats: []ChatID{chat}}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendTarget(ctx, grant, other); !errors.Is(err, ErrSendForbidden) {
		t.Fatalf("selected retained all authority: %v", err)
	}
	if err := s.SetSendPermissionMode(ctx, grant, SendPermission{Mode: "none"}, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendTarget(ctx, grant, chat); !errors.Is(err, ErrSendForbidden) {
		t.Fatalf("none retained authority: %v", err)
	}
	if err := s.SetSendPermissionMode(ctx, grant, SendPermission{Mode: "all"}, 3); err != nil {
		t.Fatal(err)
	}
	updated, err := s.SetCredentialPolicy(ctx, credential.ID, CredentialPolicy{Account: "account", Chats: []ChatID{chat}}, credential.Revision)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.ResolveCredentialID(ctx, updated.ID)
	if err != nil {
		t.Fatal(err)
	}
	permission, err := s.GetSendPermission(ctx, current)
	if err != nil || permission.Mode != "none" || permission.Revision != 4 {
		t.Fatalf("old credential revision retained mode: %#v %v", permission, err)
	}
	if _, err = s.SendTarget(ctx, current, chat); !errors.Is(err, ErrSendForbidden) {
		t.Fatalf("changed credential retained all grant: %v", err)
	}
}

func TestSendPermissionModeValidationDoesNotBroadenAuthority(t *testing.T) {
	s, _, _, chat, _ := credentialFixture(t)
	ctx := context.Background()
	credential := createCredential(t, s, "mode-validation", CredentialPolicy{Account: "account", Chats: []ChatID{chat}})
	grant, _ := s.ResolveCredentialID(ctx, credential.ID)
	for _, permission := range []SendPermission{{Mode: ""}, {Mode: "everything"}, {Mode: "all", Chats: []ChatID{chat}}, {Mode: "none", Chats: []ChatID{chat}}, {Mode: "selected"}, {Mode: "selected", Chats: []ChatID{chat, chat}}} {
		if err := s.SetSendPermissionMode(ctx, grant, permission, 0); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("invalid permission accepted: %#v %v", permission, err)
		}
	}
	if _, err := s.SendTarget(ctx, grant, chat); !errors.Is(err, ErrSendForbidden) {
		t.Fatalf("validation granted sending: %v", err)
	}
}

func TestSendPermissionConcurrentOwnerEditsHaveOneCASWinner(t *testing.T) {
	s, _, _, chat, _ := credentialFixture(t)
	ctx := context.Background()
	credential := createCredential(t, s, "concurrent-send-edit", CredentialPolicy{Account: "account", AllChats: true})
	grant, _ := s.ResolveCredentialID(ctx, credential.ID)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, permission := range []SendPermission{{Mode: "all"}, {Mode: "selected", Chats: []ChatID{chat}}} {
		go func(permission SendPermission) {
			<-start
			results <- s.SetSendPermissionMode(ctx, grant, permission, 0)
		}(permission)
	}
	close(start)
	winners, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			winners++
		} else if errors.Is(err, ErrSendConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent edit result: %v", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("CAS winners=%d conflicts=%d", winners, conflicts)
	}
	permission, err := s.GetSendPermission(ctx, grant)
	if err != nil || permission.Revision != 1 {
		t.Fatalf("concurrent edit state: %#v %v", permission, err)
	}
	if _, err = s.SendTarget(ctx, grant, chat); err != nil {
		t.Fatalf("known concurrent CAS rejection poisoned authority: %v", err)
	}
}

func TestSendPermissionMalformedPersistedRevisionDeniesTargets(t *testing.T) {
	for _, mode := range []string{"all", "selected"} {
		for _, malformed := range []any{1.5, "broken", []byte{1}, []byte{'1'}, 0, -1} {
			s, _, _, chat, _ := credentialFixture(t)
			ctx := context.Background()
			credential := createCredential(t, s, "malformed-send", CredentialPolicy{Account: "account", AllChats: true})
			grant, _ := s.ResolveCredentialID(ctx, credential.ID)
			permission := SendPermission{Mode: mode}
			if mode == "selected" {
				permission.Chats = []ChatID{chat}
			}
			if err := s.SetSendPermissionMode(ctx, grant, permission, 0); err != nil {
				t.Fatal(err)
			}
			// Simulate corruption outside the owner API and its integer-only writes.
			for _, statement := range []string{"PRAGMA ignore_check_constraints=ON", "UPDATE send_permission_modes SET revision=?", "PRAGMA ignore_check_constraints=OFF"} {
				args := []any{}
				if statement[0] == 'U' {
					args = append(args, malformed)
				}
				if _, err := s.writeDB.Exec(statement, args...); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.GetSendPermission(ctx, grant); err == nil {
				t.Fatal("malformed permission summary was accepted")
			}
			if _, err := s.SendTarget(ctx, grant, chat); !errors.Is(err, ErrSendForbidden) {
				t.Fatalf("malformed persisted permission allowed sending: %v", err)
			}
		}
	}
}

func TestCanceledAcceptedAllPermissionWithdrawalPersistsDenyFence(t *testing.T) {
	s, _, now, chat, _ := credentialFixture(t)
	ctx := context.Background()
	credential := createCredential(t, s, "cancel-all", CredentialPolicy{Account: "account", AllChats: true})
	grant, _ := s.ResolveCredentialID(ctx, credential.ID)
	if err := s.SetSendPermissionMode(ctx, grant, SendPermission{Mode: "all"}, 0); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	barrierDone := make(chan error, 1)
	go func() {
		_, err := s.submitExtension(ctx, credentialRecoveryBarrierCommand{entered, release}, 0)
		barrierDone <- err
	}()
	<-entered
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	mutationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.SetSendPermissionMode(mutationCtx, grant, SendPermission{Mode: "none"}, 1) }()
	deadline := time.Now().Add(time.Second)
	for len(s.controls) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.controls) == 0 {
		t.Fatal("withdrawal not admitted")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled withdrawal: %v", err)
	}
	if _, err := s.SendTarget(ctx, grant, chat); err == nil {
		t.Fatal("uncertain all withdrawal retained send authority")
	}
	close(release)
	released = true
	if err := <-barrierDone; err != nil {
		t.Fatal(err)
	}
	s = reopenCredentialStore(t, s, now)
	if _, err := s.ResolveCredentialID(ctx, credential.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("all withdrawal fence did not survive restart: %v", err)
	}
}
