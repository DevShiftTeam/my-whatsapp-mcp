//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

type aliasNoopBarrierCommand struct{ entered, release chan struct{} }

func (c aliasNoopBarrierCommand) run(_ *Store, _ context.Context) (any, error) {
	close(c.entered)
	<-c.release
	return nil, nil
}
func (aliasNoopBarrierCommand) priority() bool                    { return false }
func (aliasNoopBarrierCommand) account() string                   { return "account" }
func (aliasNoopBarrierCommand) sensitivity() extensionSensitivity { return extensionBackground }

func aliasNoopFixture(t *testing.T) (*Store, ArchivePolicy, SourceAlias, SourceKey) {
	t.Helper()
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("alias-noop-source", "alias stays readable", 1, EventOriginal)
	e.Key.Chat = "123@s.whatsapp.net"
	e.Key.Sender = e.Key.Chat
	apply(t, s, e, p.Generation)
	a := SourceAlias{Account: "account", PhoneJID: e.Key.Chat, LID: "456@lid", Provenance: AliasConnectorMapping}
	if err := s.ApplyAlias(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return s, p, a, e.Key
}

func TestCommittedAliasNoopDoesNotWaitForWriter(t *testing.T) {
	s, p, a, key := aliasNoopFixture(t)
	ctx := context.Background()
	beforeSource, err := s.CurrentSource(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	beforeEpoch, err := s.readSafety(a.Account)
	if err != nil {
		t.Fatal(err)
	}
	var beforeRevision ArchiveRevision
	if err = s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&beforeRevision); err != nil {
		t.Fatal(err)
	}

	barrier := aliasNoopBarrierCommand{entered: make(chan struct{}), release: make(chan struct{})}
	blocked := make(chan error, 1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	defer release()
	go func() { _, e := s.submitExtension(ctx, barrier, 0); blocked <- e }()
	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not enter test barrier")
	}

	done := make(chan error, 1)
	go func() { done <- s.ApplyAlias(ctx, a) }()
	joined := false
	select {
	case err = <-done:
		joined = true
		if err != nil {
			t.Errorf("committed no-op alias failed: %v", err)
		}
	case queued := <-s.controls:
		// The writer cannot consume controls while the barrier is held. Seeing
		// this exact command proves the regression without waiting for a timer.
		s.controls <- queued
		t.Error("committed no-op alias entered the stalled writer queue")
	case <-time.After(2 * time.Second):
		t.Error("alias returned neither a result nor a queued command")
	}
	release()
	if !joined {
		select {
		case err = <-done:
			if err != nil {
				t.Errorf("alias cleanup failed: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("alias cleanup did not join")
		}
	}
	select {
	case err = <-blocked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer barrier cleanup did not join")
	}
	if err = s.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	afterSource, err := s.CurrentSource(ctx, key)
	if err != nil || afterSource != beforeSource {
		t.Fatalf("no-op changed source authority: %#v %v", afterSource, err)
	}
	afterEpoch, err := s.readSafety(a.Account)
	if err != nil || afterEpoch != beforeEpoch {
		t.Fatalf("no-op changed safety: %d -> %d, %v", beforeEpoch, afterEpoch, err)
	}
	var afterRevision ArchiveRevision
	if err = s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&afterRevision); err != nil || afterRevision != beforeRevision {
		t.Fatalf("no-op changed archive revision: %d -> %d, %v", beforeRevision, afterRevision, err)
	}
	page, err := s.Search(ctx, ReadScope{Account: a.Account, Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "readable"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("no-op made source unreadable: %d, %v", len(page.Hits), err)
	}
}

func TestCommittedAliasNoopPreservesAccountAndSafetyGuards(t *testing.T) {
	for _, name := range []string{"wrong_account", "unsafe_account", "unclean_archive", "closed", "canceled", "invalid_phone", "invalid_provenance"} {
		t.Run(name, func(t *testing.T) {
			s, _, a, _ := aliasNoopFixture(t)
			ctx := context.Background()
			want := error(ErrInvalidEvent)
			switch name {
			case "wrong_account":
				a.Account = "other-account"
				want = ErrUnauthorized
			case "unsafe_account":
				if err := s.denyImmediate(a.Account, "mutation_metadata_missing"); err != nil {
					t.Fatal(err)
				}
				want = ErrReconciliationRequired
			case "unclean_archive":
				if err := s.denyImmediate("", "unclean_shutdown"); err != nil {
					t.Fatal(err)
				}
				want = ErrReconciliationRequired
			case "closed":
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				want = ErrArchiveUnavailable
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "invalid_phone":
				a.PhoneJID = "123@lid"
			case "invalid_provenance":
				a.Provenance = "untrusted"
			}
			if err := s.ApplyAlias(ctx, a); !errors.Is(err, want) {
				t.Fatalf("%s accepted committed no-op: %v, want %v", name, err, want)
			}
		})
	}
}

func TestAliasMissingOrConflictingMappingRetainsWriterAuthority(t *testing.T) {
	for _, name := range []string{"new_pair", "missing_row", "conflict"} {
		t.Run(name, func(t *testing.T) {
			s, _, a, _ := aliasNoopFixture(t)
			ctx := context.Background()
			var want error
			switch name {
			case "new_pair":
				a.PhoneJID, a.LID = "789@s.whatsapp.net", "890@lid"
			case "missing_row":
				if _, err := s.writeDB.Exec("DELETE FROM identity_aliases WHERE account=? AND alias=?", a.Account, a.LID); err != nil {
					t.Fatal(err)
				}
			case "conflict":
				a.PhoneJID = "999@s.whatsapp.net"
				want = ErrIdentityAliasConflict
			}
			barrier := aliasNoopBarrierCommand{entered: make(chan struct{}), release: make(chan struct{})}
			blocked := make(chan error, 1)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
			defer release()
			go func() { _, err := s.submitExtension(ctx, barrier, 0); blocked <- err }()
			select {
			case <-barrier.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("writer did not enter test barrier")
			}
			done := make(chan error, 1)
			go func() { done <- s.ApplyAlias(ctx, a) }()
			select {
			case queued := <-s.controls:
				command, ok := queued.payload.(aliasCommand)
				if !ok || command.alias != a {
					t.Error("alias did not retain its exact authoritative command")
				}
				s.controls <- queued
			case err := <-done:
				t.Fatalf("%s bypassed the authoritative writer: %v", name, err)
			case <-time.After(2 * time.Second):
				t.Fatal("alias was not admitted to authoritative writer")
			}
			release()
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("%s authoritative result: %v, want %v", name, err, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("alias did not join after writer resumed")
			}
			select {
			case err := <-blocked:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("writer barrier cleanup did not join")
			}
			if name == "conflict" {
				if _, err := s.readSafety(a.Account); !errors.Is(err, ErrReconciliationRequired) {
					t.Fatalf("conflicting alias did not deny account: %v", err)
				}
			} else {
				var mappings int
				if err := s.readDB.QueryRow("SELECT count(*) FROM identity_aliases WHERE account=? AND canonical=? AND alias IN (?,?)", a.Account, a.PhoneJID, a.PhoneJID, a.LID).Scan(&mappings); err != nil || mappings != 2 {
					t.Fatalf("authoritative mapping incomplete: %d, %v", mappings, err)
				}
			}
		})
	}
}

func TestCommittedAliasNoopRejectsDenialDuringLookup(t *testing.T) {
	s, _, a, _ := aliasNoopFixture(t)
	ctx := context.Background()
	entered, resume := make(chan struct{}), make(chan struct{})
	var enterOnce, resumeOnce sync.Once
	release := func() { resumeOnce.Do(func() { close(resume) }) }
	defer release()

	// Hook both pools so the same race exercises the old writer lookup and
	// the proposed guarded read. No production-only test hook is needed.
	for _, db := range []*sql.DB{s.readDB, s.writeDB} {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = conn.Raw(func(raw any) error {
			raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, table, column, database string) int {
				if op == sqlite3.SQLITE_READ && table == "identity_aliases" {
					enterOnce.Do(func() { close(entered); <-resume })
				}
				return sqlite3.SQLITE_OK
			})
			return nil
		})
		if closeErr := conn.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan error, 1)
	go func() { done <- s.ApplyAlias(ctx, a) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("alias did not reach mapping lookup")
	}
	if err := s.denyImmediate(a.Account, "mutation_metadata_missing"); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err := <-done:
		if !errors.Is(err, ErrReconciliationRequired) {
			t.Fatalf("alias acknowledged after concurrent denial: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("alias did not join after mapping lookup resumed")
	}
}
