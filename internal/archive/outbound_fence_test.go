//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSendTargetPoolAdmissionDoesNotDelayAuthorityWithdrawal(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(context.Context, *Store, CredentialGrant) error
		denied error
	}{
		{
			name: "source unsafe",
			change: func(_ context.Context, s *Store, _ CredentialGrant) error {
				return s.denyImmediate("account", "reconciliation_required")
			},
			denied: ErrReconciliationRequired,
		},
		{
			name: "send permission withdrawn",
			change: func(ctx context.Context, s *Store, grant CredentialGrant) error {
				return s.SetSendPermission(ctx, grant, nil)
			},
			denied: ErrSendForbidden,
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s, _, _, chat, _ := credentialFixture(t)
			ctx := context.Background()
			credential := createCredential(t, s, "send-pool", CredentialPolicy{Account: "account", Chats: []ChatID{chat}})
			grant, err := s.ResolveCredentialID(ctx, credential.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.SetSendPermission(ctx, grant, []ChatID{chat}); err != nil {
				t.Fatal(err)
			}
			s.readDB.SetMaxOpenConns(1)
			held, err := s.readDB.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			sendCtx, cancelSend := context.WithCancel(ctx)
			result := make(chan error, 1)
			changed := make(chan error, 1)
			sendJoined, changeStarted, changeJoined := false, false, false
			defer func() {
				cancelSend()
				held.Close()
				if !sendJoined {
					select {
					case <-result:
					case <-time.After(time.Second):
						t.Error("waiting target read did not join")
					}
				}
				if changeStarted && !changeJoined {
					select {
					case <-changed:
					case <-time.After(time.Second):
						t.Error("authority withdrawal did not join")
					}
				}
			}()
			waits := s.readDB.Stats().WaitCount
			go func() {
				_, err := s.SendTarget(sendCtx, grant, chat)
				result <- err
			}()
			deadline := time.Now().Add(time.Second)
			for s.readDB.Stats().WaitCount == waits && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if s.readDB.Stats().WaitCount == waits {
				t.Fatal("target read did not wait for pool admission")
			}
			control, cancelControl := context.WithTimeout(ctx, 250*time.Millisecond)
			defer cancelControl()
			changeStarted = true
			go func() { changed <- scenario.change(control, s, grant) }()
			select {
			case err = <-changed:
				changeJoined = true
				if err != nil {
					t.Fatalf("waiting target read prevented authority withdrawal: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("waiting target read blocked authority withdrawal")
			}
			select {
			case err = <-result:
				sendJoined = true
				t.Fatalf("target read returned before pool release: %v", err)
			default:
			}
			if err = held.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-result:
				sendJoined = true
				if !errors.Is(err, scenario.denied) {
					t.Fatalf("target read missed withdrawn authority: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("target read did not finish after pool release")
			}
		})
	}
}
