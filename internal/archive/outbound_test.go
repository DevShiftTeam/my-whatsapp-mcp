//go:build sqlite_fts5

package archive

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestSendPermissionIsExplicitFiniteAndRevocable(t *testing.T) {
	s, _, _, a, b := credentialFixture(t)
	ctx := context.Background()
	c := createCredential(t, s, "send", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
	g, err := s.ResolveCredentialID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SendTarget(ctx, g, a); !errors.Is(err, ErrSendForbidden) {
		t.Fatalf("read grant allowed sending: %v", err)
	}
	if err = s.SetSendPermission(ctx, g, []ChatID{b}); !errors.Is(err, ErrSendForbidden) {
		t.Fatalf("out of scope granted: %v", err)
	}
	if err = s.SetSendPermission(ctx, g, []ChatID{a}); err != nil {
		t.Fatal(err)
	}
	if target, err := s.SendTarget(ctx, g, a); err != nil || target == "" {
		t.Fatalf("allowed target: %v", err)
	}
	if err = s.SetSendPermission(ctx, g, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SendTarget(ctx, g, a); !errors.Is(err, ErrSendForbidden) {
		t.Fatal("revoked sending survived")
	}
}
func TestSendJournalSurvivesRestartAndRejectsChangedPayload(t *testing.T) {
	s, _, now, a, _ := credentialFixture(t)
	ctx := context.Background()
	c := createCredential(t, s, "journal", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
	g, _ := s.ResolveCredentialID(ctx, c.ID)
	if err := s.SetSendPermission(ctx, g, []ChatID{a}); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("hello"))
	r, fresh, err := s.ClaimSend(ctx, g, a, "request-1", hash, "protocol-1")
	if err != nil || !fresh || r.State != "outcome_unknown" {
		t.Fatalf("claim: %#v %v", r, err)
	}
	if err = s.FinishSend(ctx, g, "request-1", "accepted", time.Now()); err != nil {
		t.Fatal(err)
	}
	s = reopenCredentialStore(t, s, now)
	g, _ = s.ResolveCredentialID(ctx, c.ID)
	r, fresh, err = s.ClaimSend(ctx, g, a, "request-1", hash, "protocol-2")
	if err != nil || fresh || r.ProtocolID != "protocol-1" || r.State != "accepted" {
		t.Fatalf("restart replay: %#v %v", r, err)
	}
	if _, _, err = s.ClaimSend(ctx, g, a, "request-1", sha256.Sum256([]byte("changed")), "protocol-3"); !errors.Is(err, ErrSendConflict) {
		t.Fatalf("changed payload replay accepted: %v", err)
	}
	if err = s.RevokeCredential(ctx, c.ID, c.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SendStatus(ctx, g, "request-1"); err == nil {
		t.Fatal("revoked credential read journal")
	}
}

func TestSendCommandsFenceConcurrentImmediateUnsafeMarks(t *testing.T) {
	s, _, _, a, _ := credentialFixture(t)
	ctx := context.Background()
	c := createCredential(t, s, "fence", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
	g, _ := s.ResolveCredentialID(ctx, c.ID)
	if err := s.SetSendPermission(ctx, g, []ChatID{a}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 30; i++ {
			_ = s.SetSendPermission(ctx, g, []ChatID{a})
			_, _, _ = s.ClaimSend(ctx, g, a, fmt.Sprintf("claim-%d", i), sha256.Sum256([]byte("hello")), fmt.Sprintf("protocol-%d", i))
		}
	}()
	if err := s.denyImmediate("account", "reconciliation_required"); err != nil {
		t.Fatal(err)
	}
	<-done
	if _, err := s.SendTarget(ctx, g, a); err == nil {
		t.Fatal("unsafe account retained send authority")
	}
}

func TestCanceledAcceptedPermissionWithdrawalPersistsDenyFence(t *testing.T) {
	s, _, now, a, _ := credentialFixture(t)
	ctx := context.Background()
	c := createCredential(t, s, "withdraw-cancel", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
	g, _ := s.ResolveCredentialID(ctx, c.ID)
	if err := s.SetSendPermission(ctx, g, []ChatID{a}); err != nil {
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
	go func() { done <- s.SetSendPermission(mutationCtx, g, nil) }()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	accepted := false
	for !accepted {
		select {
		case <-ticker.C:
			accepted = len(s.controls) > 0
		case <-timeout.C:
			t.Fatal("withdrawal not admitted")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cancelled withdrawal: %v", err)
	}
	if _, err := s.SendTarget(ctx, g, a); err == nil {
		t.Fatal("uncertain accepted withdrawal retained send authority")
	}
	close(release)
	released = true
	if err := <-barrierDone; err != nil {
		t.Fatal(err)
	}
	s = reopenCredentialStore(t, s, now)
	if _, err := s.ResolveCredentialID(ctx, c.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("withdrawal fence did not survive restart: %v", err)
	}
}
