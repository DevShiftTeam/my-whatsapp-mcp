//go:build sqlite_fts5

package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

func credentialFixture(t *testing.T) (*Store, ArchivePolicy, *time.Time, ChatID, ChatID) {
	t.Helper()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	a := readerApply(t, s, p, readerEvent("credential-a", "a", "credential canary", now.Add(-time.Minute)))
	b := readerApply(t, s, p, readerEvent("credential-b", "b", "credential canary", now.Add(-time.Minute)))
	return s, p, &now, readerChat(t, s, a), readerChat(t, s, b)
}

func createCredential(t *testing.T, s *Store, suffix string, policy CredentialPolicy) Credential {
	t.Helper()
	c, err := s.CreateCredential(context.Background(), CredentialCreate{Label: "Synthetic " + suffix, Principal: "client:" + suffix, Digest: sha256.Sum256([]byte("synthetic-bearer-" + suffix)), Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func reopenCredentialStore(t *testing.T, s *Store, now *time.Time) *Store {
	t.Helper()
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), dir, Options{Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	return reopened
}

func TestCredentialDigestPersistenceAndFiniteScope(t *testing.T) {
	s, _, now, a, b := credentialFixture(t)
	request := CredentialCreate{Label: "Synthetic client", Principal: "client:finite", Digest: sha256.Sum256([]byte("synthetic-private-token")), Policy: CredentialPolicy{Account: "account", Chats: []ChatID{a, a}}}
	c, err := s.CreateCredential(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Policy.Chats[0] = b
	if c.Revision != 1 || len(c.Policy.Chats) != 1 || c.Policy.Chats[0] != a || c.CreatedAt.IsZero() || c.Revoked {
		t.Fatalf("invalid persisted credential: %#v", c)
	}
	encoded, err := json.Marshal(c)
	if err != nil || strings.Contains(strings.ToLower(string(encoded)), `"digest"`) || strings.Contains(string(encoded), "synthetic-private-token") {
		t.Fatalf("credential output contains secret material: %v", err)
	}
	var digest []byte
	var kind string
	if err = s.readDB.QueryRow("SELECT digest,typeof(digest) FROM credentials WHERE id=?", c.ID).Scan(&digest, &kind); err != nil || kind != "blob" || len(digest) != 32 || string(digest) != string(request.Digest[:]) {
		t.Fatalf("expected only fixed digest persistence: %v", err)
	}
	s = reopenCredentialStore(t, s, now)
	grant, err := s.ResolveCredential(context.Background(), request.Digest)
	if err != nil || grant.CredentialID != c.ID || grant.CredentialRevision != 1 || grant.Scope.AllChats || len(grant.Scope.Chats) != 1 || grant.Scope.Chats[0] != a {
		t.Fatalf("restart resolved wrong authority: %#v %v", grant, err)
	}
	page, err := s.Search(context.Background(), grant.Scope, SearchRequest{Query: "credential canary"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].ChatID != a {
		t.Fatalf("finite credential escaped scope: %#v %v", page, err)
	}
	if _, err = s.ResolveCredential(context.Background(), sha256.Sum256([]byte("wrong-token"))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown digest authorized: %v", err)
	}
}

func TestCredentialEmptyDeniesAndExplicitAllChatsIncludesFutureChats(t *testing.T) {
	s, p, now, a, _ := credentialFixture(t)
	empty := createCredential(t, s, "empty", CredentialPolicy{Account: "account"})
	if _, err := s.ResolveCredentialID(context.Background(), empty.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty finite policy became wildcard: %v", err)
	}
	finite := createCredential(t, s, "finite", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
	all := createCredential(t, s, "all", CredentialPolicy{Account: "account", AllChats: true})
	future := readerApply(t, s, p, readerEvent("credential-future", "future", "future credential canary", now.Add(-time.Second)))
	futureChat := readerChat(t, s, future)
	for _, test := range []struct {
		credential Credential
		want       int
	}{{finite, 0}, {all, 1}} {
		grant, err := s.ResolveCredentialID(context.Background(), test.credential.ID)
		if err != nil {
			t.Fatal(err)
		}
		page, err := s.Search(context.Background(), grant.Scope, SearchRequest{Query: "future credential canary"})
		if err != nil || len(page.Hits) != test.want || (test.want > 0 && page.Hits[0].ChatID != futureChat) {
			t.Fatalf("future chat grant: %#v %v", page, err)
		}
	}
}

func TestCredentialGrantRejectsForgedAuthorityAndRevisionCAS(t *testing.T) {
	s, _, _, a, b := credentialFixture(t)
	c := createCredential(t, s, "cas", CredentialPolicy{Account: "account", Chats: []ChatID{a}, Rolling: time.Hour})
	grant, err := s.ResolveCredentialID(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*CredentialGrant){
		"all chats":  func(g *CredentialGrant) { g.Scope.AllChats = true },
		"chat":       func(g *CredentialGrant) { g.Scope.Chats = []ChatID{b} },
		"principal":  func(g *CredentialGrant) { g.Scope.Principal = "local_owner" },
		"account":    func(g *CredentialGrant) { g.Scope.Account = "foreign" },
		"date":       func(g *CredentialGrant) { g.Scope.From = time.Now() },
		"rolling":    func(g *CredentialGrant) { g.Scope.Rolling = 0 },
		"revision":   func(g *CredentialGrant) { g.CredentialRevision++ },
		"generation": func(g *CredentialGrant) { g.Scope.PolicyGeneration++ },
	} {
		t.Run(name, func(t *testing.T) {
			forged := grant
			mutate(&forged)
			if err := s.CheckCredentialGrant(context.Background(), forged); err == nil {
				t.Fatal("forged grant accepted")
			}
		})
	}
	if _, err = s.SetCredentialPolicy(context.Background(), c.ID, CredentialPolicy{Account: "account"}, c.Revision+1); !errors.Is(err, ErrPolicyChanged) {
		t.Fatalf("stale CAS accepted: %v", err)
	}
	if err = s.CheckCredentialGrant(context.Background(), grant); err != nil {
		t.Fatalf("known CAS rejection poisoned credential: %v", err)
	}
	updated, err := s.SetCredentialPolicy(context.Background(), c.ID, CredentialPolicy{Account: "account", Chats: []ChatID{b}}, c.Revision)
	if err != nil || updated.Revision != 2 {
		t.Fatalf("policy CAS did not persist: %#v %v", updated, err)
	}
	if err = s.CheckCredentialGrant(context.Background(), grant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("late response retained old credential revision: %v", err)
	}
	current, err := s.ResolveCredentialID(context.Background(), c.ID)
	if err != nil || current.Scope.Chats[0] != b {
		t.Fatalf("new credential scope unavailable: %#v %v", current, err)
	}
	if err = s.RevokeCredential(context.Background(), c.ID, c.Revision); !errors.Is(err, ErrPolicyChanged) {
		t.Fatalf("revoke CAS accepted stale revision: %v", err)
	}
	if err = s.RevokeCredential(context.Background(), c.ID, updated.Revision); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckCredentialGrant(context.Background(), current); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked late response authorized: %v", err)
	}
	if _, err = s.ResolveCredentialID(context.Background(), c.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked ID resolved: %v", err)
	}
}

func TestCredentialResolutionUsesCurrentClockAndArchivePolicy(t *testing.T) {
	s, p, now, a, b := credentialFixture(t)
	c, err := s.CreateCredential(context.Background(), CredentialCreate{Label: "Clock", Principal: "client:clock", Digest: sha256.Sum256([]byte("clock-token")), ExpiresAt: now.Add(3 * time.Minute), Policy: CredentialPolicy{Account: "account", Chats: []ChatID{a, b}, Rolling: 2 * time.Minute}})
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.ResolveCredentialID(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Minute)
	fresh, err := s.ResolveCredentialID(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.Search(context.Background(), fresh.Scope, SearchRequest{Query: "credential canary"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("rolling credential reused old cutoff: %#v %v", page, err)
	}
	p2, err := s.SetPolicy(context.Background(), ArchivePolicy{DateMode: DateSince, ImportStart: now.Add(-time.Hour), ChatMode: ChatsSelective, IncludedChats: []ChatID{a}}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CheckCredentialGrant(context.Background(), old); !errors.Is(err, ErrPolicyChanged) {
		t.Fatalf("stale archive policy grant accepted: %v", err)
	}
	fresh, err = s.ResolveCredentialID(context.Background(), c.ID)
	if err != nil || fresh.Scope.PolicyGeneration != p2.Generation || len(fresh.Scope.Chats) != 1 || fresh.Scope.Chats[0] != a || !fresh.Scope.From.Equal(p2.ImportStart) {
		t.Fatalf("current archive policy not intersected: %#v %v", fresh, err)
	}
	*now = c.ExpiresAt
	if _, err = s.ResolveCredentialID(context.Background(), c.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("actual clock expiry ignored: %v", err)
	}
}

func TestCredentialCanceledReadsDoNotDenyCredentialOrOwner(t *testing.T) {
	s, p, _, a, _ := credentialFixture(t)
	c := createCredential(t, s, "read-cancel", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ResolveCredentialID(ctx, c.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("read cancellation result: %v", err)
	}
	grant, err := s.ResolveCredentialID(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("canceled read poisoned credential: %v", err)
	}
	if err = s.CheckCredentialGrant(ctx, grant); !errors.Is(err, context.Canceled) {
		t.Fatalf("grant read cancellation result: %v", err)
	}
	if err = s.CheckCredentialGrant(context.Background(), grant); err != nil {
		t.Fatalf("canceled grant read poisoned credential: %v", err)
	}
	if _, err = s.Search(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "credential canary"}); err != nil {
		t.Fatalf("credential read poisoned owner archive: %v", err)
	}
}

func TestCredentialUncertaintyFencePersistsWithoutDenyingOwnerOrOtherCredentials(t *testing.T) {
	s, p, now, a, _ := credentialFixture(t)
	c := createCredential(t, s, "uncertain", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
	other := createCredential(t, s, "certain", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
	grant, err := s.ResolveCredentialID(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.denyCredentialImmediate(c.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckCredentialGrant(context.Background(), grant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("uncertain mutation left late grant active: %v", err)
	}
	s = reopenCredentialStore(t, s, now)
	if _, err = s.ResolveCredentialID(context.Background(), c.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("uncertainty denial did not survive restart: %v", err)
	}
	if _, err = s.ResolveCredentialID(context.Background(), other.ID); err != nil {
		t.Fatalf("affected credential fence denied independent client: %v", err)
	}
	if _, err = s.Search(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "credential canary"}); err != nil {
		t.Fatalf("credential uncertainty globally quarantined source: %v", err)
	}
	if _, err = os.Lstat(filepath.Join(s.privateDir, "unsafe.marker")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("auth uncertainty wrote source marker: %v", err)
	}
}

func TestCredentialCorruptFenceDeniesAuthOnly(t *testing.T) {
	s, p, now, a, _ := credentialFixture(t)
	c := createCredential(t, s, "corrupt", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
	dir := s.privateDir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials-denied.marker.pending"), []byte("partial private fence"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), dir, Options{Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err = s.ResolveCredentialID(context.Background(), c.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("corrupt auth fence authorized credential: %v", err)
	}
	if _, err = s.Search(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "credential canary"}); err != nil {
		t.Fatalf("corrupt auth fence denied owner archive: %v", err)
	}
}

func TestCredentialRejectsInvalidPersistentAuthority(t *testing.T) {
	s, _, now, a, _ := credentialFixture(t)
	valid := CredentialCreate{Label: "Validation", Principal: "client:validation", Digest: sha256.Sum256([]byte("validation-token")), Policy: CredentialPolicy{Account: "account", Chats: []ChatID{a}}}
	for name, mutate := range map[string]func(*CredentialCreate){
		"empty label":             func(c *CredentialCreate) { c.Label = "" },
		"large label":             func(c *CredentialCreate) { c.Label = strings.Repeat("x", maxCredentialLabel+1) },
		"control principal":       func(c *CredentialCreate) { c.Principal = "client\nsecret" },
		"zero digest":             func(c *CredentialCreate) { c.Digest = [32]byte{} },
		"unknown account":         func(c *CredentialCreate) { c.Policy.Account = "foreign" },
		"unknown chat":            func(c *CredentialCreate) { c.Policy.Chats = []ChatID{"unknown"} },
		"ambiguous chats":         func(c *CredentialCreate) { c.Policy.AllChats = true },
		"negative rolling":        func(c *CredentialCreate) { c.Policy.Rolling = -time.Second },
		"reversed range":          func(c *CredentialCreate) { c.Policy.From, c.Policy.Until = *now, now.Add(-time.Hour) },
		"epoch expiry":            func(c *CredentialCreate) { c.ExpiresAt = time.UnixMilli(0).UTC() },
		"expired":                 func(c *CredentialCreate) { c.ExpiresAt = *now },
		"overflowed rounded date": func(c *CredentialCreate) { c.Policy.From = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC) },
	} {
		t.Run(name, func(t *testing.T) {
			request := valid
			mutate(&request)
			if _, err := s.CreateCredential(context.Background(), request); !errors.Is(err, ErrInvalidExtension) {
				t.Fatalf("invalid credential input accepted: %v", err)
			}
		})
	}
	if _, err := s.CreateCredential(context.Background(), valid); err != nil {
		t.Fatalf("known validation rejection denied subsequent valid create: %v", err)
	}
	if _, err := s.CreateCredential(context.Background(), valid); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("duplicate digest accepted: %v", err)
	}
}

func TestCredentialFailedFenceWriteDeniesAuthDurablyWithoutSourceQuarantine(t *testing.T) {
	s, p, now, a, _ := credentialFixture(t)
	c := createCredential(t, s, "write-failure", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
	other := createCredential(t, s, "write-failure-other", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
	if err := os.Mkdir(filepath.Join(s.privateDir, credentialFenceMarker+".pending"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.denyCredentialImmediate(c.ID); err == nil {
		t.Fatal("unwritable pending marker unexpectedly acknowledged")
	}
	if _, err := s.CreateCredential(context.Background(), CredentialCreate{Label: "Uncertain", Principal: "client:uncertain", Digest: sha256.Sum256([]byte("uncertain-create")), Policy: CredentialPolicy{Account: "account", AllChats: true}}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("uncertain auth storage issued new credential: %v", err)
	}
	// Remove the obstruction: the existing final marker must independently
	// retain deny-all recovery after the pending file could not be opened.
	if err := os.Remove(filepath.Join(s.privateDir, credentialFenceMarker+".pending")); err != nil {
		t.Fatal(err)
	}
	s = reopenCredentialStore(t, s, now)
	if _, err := s.ResolveCredentialID(context.Background(), other.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("failed fence write lost durable auth denial: %v", err)
	}
	if _, err := s.Search(context.Background(), ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "credential canary"}); err != nil {
		t.Fatalf("auth fence write failure denied source archive: %v", err)
	}
}

func TestCredentialReadPoolAdmissionDoesNotHoldSafetyFence(t *testing.T) {
	for _, check := range []bool{false, true} {
		t.Run(fmt.Sprintf("check=%t", check), func(t *testing.T) {
			s, _, _, a, _ := credentialFixture(t)
			c := createCredential(t, s, "pool", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
			grant, err := s.ResolveCredentialID(context.Background(), c.ID)
			if err != nil {
				t.Fatal(err)
			}
			s.readDB.SetMaxOpenConns(1)
			held, err := s.readDB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			waits := s.readDB.Stats().WaitCount
			result := make(chan error, 1)
			go func() {
				if check {
					result <- s.CheckCredentialGrant(context.Background(), grant)
					return
				}
				_, err := s.ResolveCredentialID(context.Background(), c.ID)
				result <- err
			}()
			deadline := time.Now().Add(time.Second)
			for s.readDB.Stats().WaitCount == waits && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if s.readDB.Stats().WaitCount == waits {
				t.Fatal("credential read did not wait for pool admission")
			}
			writerLocked, releaseWriter := make(chan struct{}), make(chan struct{})
			go func() { s.safetyMu.Lock(); close(writerLocked); <-releaseWriter; s.safetyMu.Unlock() }()
			select {
			case <-writerLocked:
			case <-time.After(200 * time.Millisecond):
				t.Error("waiting credential read held safety fence and blocked writer")
			}
			close(releaseWriter)
			held.Close()
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("read after pool admission: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("credential read failed to resume after pool release")
			}
		})
	}
}

var credentialDelayDriverSequence atomic.Uint64

type credentialDelayDriver struct{ driver.Driver }
type credentialDelayConn struct{ driver.Conn }

func (c credentialDelayConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, ErrArchiveUnavailable
	}
	return beginner.BeginTx(ctx, options)
}

func (d credentialDelayDriver) Open(name string) (driver.Conn, error) {
	c, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return credentialDelayConn{c}, nil
}

func (c credentialDelayConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err == nil && strings.HasPrefix(query, "SELECT generation,date_mode,import_start") {
		select {
		case <-time.After(80 * time.Millisecond):
		case <-ctx.Done():
			rows.Close()
			return nil, ctx.Err()
		}
	}
	return rows, err
}

func TestCredentialGrantExpiryDuringFinalSQLReadIsDenied(t *testing.T) {
	s, _, now, a, _ := credentialFixture(t)
	c, err := s.CreateCredential(context.Background(), CredentialCreate{Label: "Expiring", Principal: "client:expiring", Digest: sha256.Sum256([]byte("expiring-token")), Policy: CredentialPolicy{Account: "account", Chats: []ChatID{a}}, ExpiresAt: now.Add(50 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := s.ResolveCredentialID(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	driverName := fmt.Sprintf("credential_delay_%d", credentialDelayDriverSequence.Add(1))
	sql.Register(driverName, credentialDelayDriver{s.readDB.Driver()})
	if err = s.readDB.Close(); err != nil {
		t.Fatal(err)
	}
	dsn := url.URL{Scheme: "file", Path: filepath.Join(s.privateDir, "archive.sqlite"), RawQuery: "mode=ro&_foreign_keys=on&_busy_timeout=500"}
	s.readDB, err = sql.Open(driverName, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CheckCredentialGrant(context.Background(), grant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("credential expired during real SQLite final read was authorized: %v", err)
	}
}

func TestCredentialCreationRejectsAuthFailureDuringClockCallback(t *testing.T) {
	s, _, now, a, _ := credentialFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	originalClock := s.options.Now
	s.options.Now = func() time.Time {
		close(entered)
		<-release
		return *now
	}
	result := make(chan error, 1)
	go func() {
		_, err := s.CreateCredential(context.Background(), CredentialCreate{Label: "Concurrent failure", Principal: "client:concurrent-failure", Digest: sha256.Sum256([]byte("concurrent-failure-token")), Policy: CredentialPolicy{Account: "account", Chats: []ChatID{a}}})
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("credential creation did not reach service clock")
	}
	// Global authentication uncertainty still leaves the source archive safe.
	if err := s.denyCredentialImmediate(""); err != nil {
		t.Error(err)
	}
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("creation committed after concurrent auth denial: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("credential creation did not complete")
	}
	s.options.Now = originalClock
	var count int
	if err := s.readDB.QueryRow("SELECT count(*) FROM credentials WHERE principal='client:concurrent-failure'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed credential creation persisted new authority: %d %v", count, err)
	}
}

func TestCredentialFiniteAliasMembershipMovesAtomicallyAndInvalidatesOnlyAffectedGrants(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, policy := readerFixture(t, &now)
	phone := "123@s.whatsapp.net"
	lid := "456@lid"
	phoneID := readerApply(t, s, policy, readerEvent("alias-phone", phone, "alias credential canary", now.Add(-time.Minute)))
	lidID := readerApply(t, s, policy, readerEvent("alias-lid", lid, "alias credential canary", now.Add(-time.Minute)))
	phoneChat, lidChat := readerChat(t, s, phoneID), readerChat(t, s, lidID)
	affected := createCredential(t, s, "alias-finite", CredentialPolicy{Account: "account", Chats: []ChatID{lidChat}})
	duplicate := createCredential(t, s, "alias-duplicate", CredentialPolicy{Account: "account", Chats: []ChatID{lidChat, phoneChat}})
	unaffected := createCredential(t, s, "alias-unaffected", CredentialPolicy{Account: "account", Chats: []ChatID{phoneChat}})
	before := make(map[CredentialID]CredentialGrant)
	for _, credential := range []Credential{affected, duplicate, unaffected} {
		grant, err := s.ResolveCredentialID(context.Background(), credential.ID)
		if err != nil {
			t.Fatal(err)
		}
		before[credential.ID] = grant
	}
	if err := s.ApplyAlias(context.Background(), SourceAlias{Account: "account", PhoneJID: phone, LID: lid, Provenance: AliasConnectorMapping}); err != nil {
		t.Fatalf("credential membership blocked trusted alias reconciliation: %v", err)
	}
	for _, credential := range []Credential{affected, duplicate} {
		if err := s.CheckCredentialGrant(context.Background(), before[credential.ID]); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("old finite alias scope retained authority: %v", err)
		}
		grant, err := s.ResolveCredentialID(context.Background(), credential.ID)
		if err != nil || grant.CredentialRevision != credential.Revision+1 || len(grant.Scope.Chats) != 1 || grant.Scope.Chats[0] != phoneChat {
			t.Fatalf("finite alias membership did not move once to canonical chat: %#v %v", grant, err)
		}
		page, err := s.Search(context.Background(), grant.Scope, SearchRequest{Query: "alias credential canary"})
		if err != nil || len(page.Hits) != 2 {
			t.Fatalf("canonical finite grant cannot read reconciled chat: %#v %v", page, err)
		}
	}
	if err := s.CheckCredentialGrant(context.Background(), before[unaffected.ID]); err != nil {
		t.Fatalf("unaffected canonical credential was invalidated: %v", err)
	}
	var oldMembers int
	if err := s.readDB.QueryRow("SELECT count(*) FROM credential_chats WHERE chat_id=?", lidChat).Scan(&oldMembers); err != nil || oldMembers != 0 {
		t.Fatalf("old alias credential foreign keys remain: %d %v", oldMembers, err)
	}
}

func TestCredentialAliasMembershipRejectsCrossAccountMoves(t *testing.T) {
	s, _, _, a, _ := credentialFixture(t)
	c := createCredential(t, s, "foreign-alias", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
	if _, err := s.writeDB.Exec("INSERT INTO accounts(account) VALUES('foreign')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writeDB.Exec("INSERT INTO chats(id,account,source_chat) VALUES('foreign-chat','foreign','foreign-source')"); err != nil {
		t.Fatal(err)
	}
	if err := s.transaction(context.Background(), func(tx *sql.Tx) error {
		return extensionReconcileCredentialChat(context.Background(), tx, a, "foreign-chat")
	}); !errors.Is(err, ErrInvalidExtension) {
		t.Fatalf("credential alias helper accepted cross-account authority: %v", err)
	}
	grant, err := s.ResolveCredentialID(context.Background(), c.ID)
	if err != nil || grant.CredentialRevision != c.Revision || len(grant.Scope.Chats) != 1 || grant.Scope.Chats[0] != a {
		t.Fatalf("rejected alias move changed credential: %#v %v", grant, err)
	}
}

func TestCredentialRecoveryListsBoundedMetadataForClosedNativeFamily(t *testing.T) {
	s, _, now, a, _ := credentialFixture(t)
	principals := []string{
		NativeClientPrincipalPrefix + "v1/fixture-one",
		NativeClientPrincipalPrefix + "v42/future-client",
		NativeClientPrincipalPrefix + "malformed-future",
		NativeClientPrincipalPrefix,
		LegacyNativeClientPrincipal,
	}
	want := make(map[CredentialID]Credential)
	for i, principal := range principals {
		c, err := s.CreateCredential(context.Background(), CredentialCreate{Label: "private recovery label", Principal: principal, Digest: sha256.Sum256([]byte(fmt.Sprintf("recovery-family-%d", i))), Policy: CredentialPolicy{Account: "account", Chats: []ChatID{a}}, ExpiresAt: now.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		want[c.ID] = c
	}
	unrelated := createCredential(t, s, "unrelated-recovery", CredentialPolicy{Account: "account", AllChats: true})
	var after CredentialID
	seen := make(map[CredentialID]bool)
	for pages := 0; pages < 4; pages++ {
		page, err := s.ListCredentials(context.Background(), CredentialListRequest{Account: "account", Namespace: CredentialNamespaceNativeClients, After: after, Limit: 2})
		if err != nil || len(page.Items) > 2 {
			t.Fatalf("bounded recovery page: %#v %v", page, err)
		}
		for _, metadata := range page.Items {
			c, exists := want[metadata.ID]
			if !exists || seen[metadata.ID] || metadata.ID <= after || metadata.Account != "account" || metadata.Principal != c.Principal || metadata.Revision != c.Revision || !metadata.CreatedAt.Equal(c.CreatedAt) || !metadata.ExpiresAt.Equal(c.ExpiresAt) || metadata.Revoked {
				t.Fatalf("incorrect recovery metadata: %#v", metadata)
			}
			seen[metadata.ID] = true
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{`"digest"`, `"token"`, `"body"`, `"policy"`, `"label"`, "private recovery label", "credential canary"} {
			if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
				t.Fatalf("recovery leaked non-metadata field %q", forbidden)
			}
		}
		if page.NextAfter == "" {
			break
		}
		if len(page.Items) == 0 || page.NextAfter != page.Items[len(page.Items)-1].ID {
			t.Fatal("recovery continuation is not last emitted opaque ID")
		}
		after = page.NextAfter
	}
	if len(seen) != len(want) || seen[unrelated.ID] {
		t.Fatalf("closed native family incomplete or expanded: %d/%d", len(seen), len(want))
	}
	exact, err := s.ListCredentials(context.Background(), CredentialListRequest{Account: "account", Principal: unrelated.Principal})
	if err != nil || len(exact.Items) != 1 || exact.Items[0].ID != unrelated.ID {
		t.Fatalf("exact principal recovery expanded: %#v %v", exact, err)
	}
	foreign, err := s.ListCredentials(context.Background(), CredentialListRequest{Account: "foreign", Namespace: CredentialNamespaceNativeClients})
	if err != nil || len(foreign.Items) != 0 {
		t.Fatalf("recovery crossed account binding: %#v %v", foreign, err)
	}
}

func TestCredentialRemoteRecoveryIsolatedPaginationIncludesExpiredRevokedAndFutureRows(t *testing.T) {
	s, _, now, a, _ := credentialFixture(t)
	principals := []string{
		RemoteClientPrincipalPrefix + "v1/fixture-one",
		RemoteClientPrincipalPrefix + "v42/future-client",
		RemoteClientPrincipalPrefix + "malformed-future",
		RemoteClientPrincipalPrefix,
		RemoteClientPrincipalPrefix + "v1/not-a-canonical-intent",
	}
	want := make(map[CredentialID]Credential)
	for i, principal := range principals {
		expiresAt := now.Add(time.Hour)
		if i == 0 {
			expiresAt = now.Add(time.Minute)
		}
		c, err := s.CreateCredential(context.Background(), CredentialCreate{Label: "private remote recovery label", Principal: principal, Digest: sha256.Sum256([]byte(fmt.Sprintf("remote-recovery-family-%d", i))), Policy: CredentialPolicy{Account: "account", Chats: []ChatID{a}}, ExpiresAt: expiresAt})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if err = s.RevokeCredential(context.Background(), c.ID, c.Revision); err != nil {
				t.Fatal(err)
			}
			c.Revision++
			c.Revoked = true
		}
		want[c.ID] = c
	}
	createControl := func(suffix, principal string) Credential {
		t.Helper()
		c, err := s.CreateCredential(context.Background(), CredentialCreate{Label: suffix, Principal: principal, Digest: sha256.Sum256([]byte(suffix)), Policy: CredentialPolicy{Account: "account", AllChats: true}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	native := createControl("remote-isolation-native", NativeClientPrincipalPrefix+"v1/native-control")
	legacy := createControl("remote-isolation-legacy", LegacyNativeClientPrincipal)
	outside := createControl("remote-isolation-outside", "wa-indexer/remote-clientx/v1/outside")
	*now = now.Add(2 * time.Minute)
	var after CredentialID
	seen := make(map[CredentialID]bool)
	for pages := 0; pages < 4; pages++ {
		page, err := s.ListCredentials(context.Background(), CredentialListRequest{Account: "account", Namespace: CredentialNamespaceRemoteClients, After: after, Limit: 2})
		if err != nil || len(page.Items) > 2 {
			t.Fatalf("bounded remote recovery page: %#v %v", page, err)
		}
		for _, metadata := range page.Items {
			c, exists := want[metadata.ID]
			if !exists || seen[metadata.ID] || metadata.ID <= after || metadata.Account != "account" || metadata.Principal != c.Principal || metadata.Revision != c.Revision || !metadata.CreatedAt.Equal(c.CreatedAt) || !metadata.ExpiresAt.Equal(c.ExpiresAt) || metadata.Revoked != c.Revoked {
				t.Fatalf("incorrect remote recovery metadata: %#v", metadata)
			}
			seen[metadata.ID] = true
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{`"digest"`, `"token"`, `"body"`, `"policy"`, `"label"`, `"path"`, `"scope"`, "private remote recovery label", "credential canary"} {
			if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
				t.Fatalf("remote recovery leaked non-metadata field %q", forbidden)
			}
		}
		if page.NextAfter == "" {
			break
		}
		if len(page.Items) == 0 || page.NextAfter != page.Items[len(page.Items)-1].ID {
			t.Fatal("remote recovery continuation is not last emitted opaque ID")
		}
		after = page.NextAfter
	}
	if len(seen) != len(want) || seen[native.ID] || seen[legacy.ID] || seen[outside.ID] {
		t.Fatalf("remote recovery was incomplete or escaped its namespace: %d/%d", len(seen), len(want))
	}
	nativePage, err := s.ListCredentials(context.Background(), CredentialListRequest{Account: "account", Namespace: CredentialNamespaceNativeClients})
	if err != nil || len(nativePage.Items) != 2 {
		t.Fatalf("remote addition changed native family selection: %#v %v", nativePage, err)
	}
	for _, metadata := range nativePage.Items {
		if metadata.ID != native.ID && metadata.ID != legacy.ID {
			t.Fatalf("native selector included remote or unrelated metadata: %#v", metadata)
		}
	}
	foreign, err := s.ListCredentials(context.Background(), CredentialListRequest{Account: "foreign", Namespace: CredentialNamespaceRemoteClients})
	if err != nil || len(foreign.Items) != 0 {
		t.Fatalf("remote recovery crossed account binding: %#v %v", foreign, err)
	}
	if err = s.MarkUnsafe(context.Background(), "account", "control_admission_failed"); err != nil {
		t.Fatal(err)
	}
	if err = s.denyCredentialImmediate(""); err != nil {
		t.Fatal(err)
	}
	deniedPage, err := s.ListCredentials(context.Background(), CredentialListRequest{Account: "account", Namespace: CredentialNamespaceRemoteClients})
	if err != nil || len(deniedPage.Items) != len(want) {
		t.Fatalf("remote metadata recovery inherited source/auth denial: %#v %v", deniedPage, err)
	}
}

func TestCredentialRecoveryRemainsUsableWhenSourceAndAuthAreDenied(t *testing.T) {
	s, _, now, a, _ := credentialFixture(t)
	c := createCredential(t, s, "unsafe-recovery", CredentialPolicy{Account: "account", Chats: []ChatID{a}})
	if err := s.RevokeCredential(context.Background(), c.ID, c.Revision); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUnsafe(context.Background(), "account", "control_admission_failed"); err != nil {
		t.Fatal(err)
	}
	if err := s.denyCredentialImmediate(""); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListCredentials(context.Background(), CredentialListRequest{Account: "account", Principal: c.Principal})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != c.ID || page.Items[0].Revision != c.Revision+1 || !page.Items[0].Revoked {
		t.Fatalf("owner metadata recovery inherited availability denial: %#v %v", page, err)
	}
	s = reopenCredentialStore(t, s, now)
	page, err = s.ListCredentials(context.Background(), CredentialListRequest{Account: "account", Principal: c.Principal})
	if err != nil || len(page.Items) != 1 || !page.Items[0].Revoked {
		t.Fatalf("owner recovery unavailable after denied reopen: %#v %v", page, err)
	}
	if _, err = s.ResolveCredentialID(context.Background(), c.ID); err == nil {
		t.Fatal("recovery metadata created availability authority")
	}
}

func TestCredentialRecoveryRejectsUnboundedOrAmbiguousSelection(t *testing.T) {
	s, _, _, _, _ := credentialFixture(t)
	for name, request := range map[string]CredentialListRequest{
		"empty account":              {Principal: "exact"},
		"implicit wildcard":          {Account: "account"},
		"arbitrary namespace":        {Account: "account", Namespace: CredentialNamespace(3)},
		"ambiguous namespace":        {Account: "account", Principal: "exact", Namespace: CredentialNamespaceNativeClients},
		"ambiguous remote namespace": {Account: "account", Principal: "exact", Namespace: CredentialNamespaceRemoteClients},
		"invalid after":              {Account: "account", Principal: "exact", After: "not-an-opaque-credential"},
		"negative limit":             {Account: "account", Principal: "exact", Limit: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.ListCredentials(context.Background(), request); !errors.Is(err, ErrInvalidExtension) {
				t.Fatalf("invalid recovery request accepted: %v", err)
			}
		})
	}
	if _, err := s.ListCredentials(context.Background(), CredentialListRequest{Account: "account", Principal: "exact", Limit: 65}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("unbounded recovery limit accepted: %v", err)
	}
	if _, err := s.ListCredentials(context.Background(), CredentialListRequest{Account: "account", Namespace: CredentialNamespaceRemoteClients, Limit: 65}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("unbounded remote recovery limit accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ListCredentials(ctx, CredentialListRequest{Account: "account", Principal: "exact"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("recovery cancellation not returned: %v", err)
	}
	if _, err := s.ListCredentials(context.Background(), CredentialListRequest{Account: "account", Principal: "exact"}); err != nil {
		t.Fatalf("canceled metadata read poisoned recovery: %v", err)
	}
}

type credentialRecoveryBarrierCommand struct{ entered, release chan struct{} }

func (c credentialRecoveryBarrierCommand) run(_ *Store, _ context.Context) (any, error) {
	close(c.entered)
	<-c.release
	return nil, nil
}
func (credentialRecoveryBarrierCommand) priority() bool  { return false }
func (credentialRecoveryBarrierCommand) account() string { return "account" }
func (credentialRecoveryBarrierCommand) sensitivity() extensionSensitivity {
	return extensionBackground
}

func TestCredentialRecoveryFIFOFindsCreateCommittedAfterCallerCancellation(t *testing.T) {
	testCredentialRecoveryFIFOFindsCreateCommittedAfterCallerCancellation(t, CredentialNamespaceNativeClients, NativeClientPrincipalPrefix)
}

func TestCredentialRemoteRecoveryFIFOFindsCreateCommittedAfterCallerCancellation(t *testing.T) {
	testCredentialRecoveryFIFOFindsCreateCommittedAfterCallerCancellation(t, CredentialNamespaceRemoteClients, RemoteClientPrincipalPrefix)
}

func testCredentialRecoveryFIFOFindsCreateCommittedAfterCallerCancellation(t *testing.T, namespace CredentialNamespace, prefix string) {
	t.Helper()
	s, _, _, a, _ := credentialFixture(t)
	commitEntered, releaseCommit := make(chan struct{}), make(chan struct{})
	barrierEntered, releaseBarrier := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-releaseCommit:
		default:
			close(releaseCommit)
		}
		select {
		case <-releaseBarrier:
		default:
			close(releaseBarrier)
		}
	}()
	conn, err := s.writeDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var armed atomic.Bool
	err = conn.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).RegisterCommitHook(func() int {
			if armed.CompareAndSwap(true, false) {
				close(commitEntered)
				<-releaseCommit
			}
			return 0
		})
		return nil
	})
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	barrierResult := make(chan error, 1)
	go func() {
		_, err := s.submitExtension(context.Background(), credentialRecoveryBarrierCommand{barrierEntered, releaseBarrier}, 0)
		barrierResult <- err
	}()
	select {
	case <-barrierEntered:
	case <-time.After(time.Second):
		t.Fatal("writer barrier did not start")
	}
	createCtx, cancelCreate := context.WithCancel(context.Background())
	defer cancelCreate()
	createResult := make(chan error, 1)
	principal := prefix + "v1/uncertain-create"
	armed.Store(true)
	go func() {
		_, err := s.CreateCredential(createCtx, CredentialCreate{Label: "Uncertain client", Principal: principal, Digest: sha256.Sum256([]byte("uncertain-create-recovery")), Policy: CredentialPolicy{Account: "account", Chats: []ChatID{a}}})
		createResult <- err
	}()
	deadline := time.Now().Add(time.Second)
	for len(s.bodies) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.bodies) != 1 {
		t.Fatal("create did not enter FIFO body lane")
	}
	type recovered struct {
		page CredentialMetadataPage
		err  error
	}
	listResult := make(chan recovered, 1)
	go func() {
		page, err := s.ListCredentials(context.Background(), CredentialListRequest{Account: "account", Namespace: namespace})
		listResult <- recovered{page, err}
	}()
	deadline = time.Now().Add(time.Second)
	for len(s.bodies) != 2 && len(s.controls) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.bodies) != 2 || len(s.controls) != 0 {
		t.Fatal("recovery did not join the same FIFO body lane as Create")
	}
	close(releaseBarrier)
	select {
	case <-commitEntered:
	case result := <-listResult:
		t.Fatalf("recovery overtook accepted Create: %#v %v", result.page, result.err)
	case <-time.After(time.Second):
		t.Fatal("real SQLite Create commit boundary not reached")
	}
	cancelCreate()
	select {
	case err := <-createResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("uncertain Create caller did not cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not return while SQLite commit was in flight")
	}
	select {
	case result := <-listResult:
		t.Fatalf("recovery returned before uncertain commit completed: %#v %v", result.page, result.err)
	default:
	}
	close(releaseCommit)
	select {
	case result := <-listResult:
		if result.err != nil || len(result.page.Items) != 1 || result.page.Items[0].Principal != principal || result.page.Items[0].Revision != 1 {
			t.Fatalf("FIFO recovery lost uncertain committed Create: %#v %v", result.page, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO recovery failed to finish")
	}
	select {
	case err := <-barrierResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer barrier result not drained")
	}
}
