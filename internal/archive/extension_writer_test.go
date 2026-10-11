//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

func TestCurrentSourceUsesAuthoritativeHashAndActiveState(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("current", "current source", 1, EventOriginal)
	r := apply(t, s, e, p.Generation)
	ref, err := s.CurrentSource(context.Background(), e.Key)
	if err != nil || ref.Version != r.Version || ref.Account != e.Key.Account {
		t.Fatalf("current source: %#v %v", ref, err)
	}
	var hash string
	if err = s.readDB.QueryRow("SELECT content_hash FROM messages WHERE id=?", r.MessageID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if ref.ContentHash != hash || len(hash) != 64 {
		t.Fatal("reconstructed rather than authoritative hash")
	}
	apply(t, s, event("current", "", 2, EventEditInvalidation), p.Generation)
	if _, err = s.CurrentSource(context.Background(), e.Key); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("invalidated authorized: %v", err)
	}
}

type extensionCancellationTestCommand struct{}

func (extensionCancellationTestCommand) run(_ *Store, ctx context.Context) (any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (extensionCancellationTestCommand) priority() bool  { return true }
func (extensionCancellationTestCommand) account() string { return "account" }
func (extensionCancellationTestCommand) sensitivity() extensionSensitivity {
	return extensionBackground
}
func TestBackgroundCancellationDoesNotDenySourceArchive(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	apply(t, s, event("survives", "searchable survives", 1, EventOriginal), p.Generation)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := s.submitExtension(ctx, extensionCancellationTestCommand{}, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled command: %v", err)
	}
	if _, err = s.readSafety("account"); err != nil {
		t.Fatalf("background cancellation poisoned archive: %v", err)
	}
	scope := ReadScope{Account: "account", Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}
	page, err := s.Search(context.Background(), scope, SearchRequest{Query: "searchable", Limit: 10})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("source search unavailable: %v %d", err, len(page.Hits))
	}
}
func TestExtensionCommitUnsafeTransitionRollsBack(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("fenced", "source", 1, EventOriginal)
	apply(t, s, e, p.Generation)
	ref, err := s.CurrentSource(context.Background(), e.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.writeDB.Exec("CREATE TABLE extension_test_commit(value INTEGER)"); err != nil {
		t.Fatal(err)
	}
	err = s.extensionTransaction(context.Background(), "account", func(tx *sql.Tx) error {
		if _, e := s.extensionCheckSources(context.Background(), tx, []SourceRef{ref}, false); e != nil {
			return e
		}
		if _, e := tx.Exec("INSERT INTO extension_test_commit VALUES(1)"); e != nil {
			return e
		}
		s.denyImmediate("account", "control_admission_failed")
		return nil
	})
	if !errors.Is(err, ErrReconciliationRequired) {
		t.Fatalf("unsafe publication accepted: %v", err)
	}
	var n int
	if err = s.readDB.QueryRow("SELECT COUNT(*) FROM extension_test_commit").Scan(&n); err != nil || n != 0 {
		t.Fatalf("unsafe publication committed: %d %v", n, err)
	}
}

func TestExtensionCommitChecksEarlierSourceAfterLaterSQLExpiryCrossing(t *testing.T) {
	s, clock, _ := indexTestStore(t)
	p := testPolicy(t, s)
	ctx := context.Background()
	refs := make([]SourceRef, 0, 2)
	for _, id := range []string{"first", "second"} {
		e := event(id, "original source", 1, EventOriginal)
		apply(t, s, e, p.Generation)
		ref, err := s.CurrentSource(ctx, e.Key)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Version.MessageID < refs[j].Version.MessageID })
	expiry := time.UnixMilli(clock.Load()).Add(10 * time.Millisecond).UTC()
	if _, err := s.writeDB.Exec("UPDATE messages SET expires_at=? WHERE id=?", unixms(expiry), refs[0].Version.MessageID); err != nil {
		t.Fatal(err)
	}
	refs[0].Version.ExpiresAt = expiry
	if _, err := s.writeDB.Exec("CREATE TABLE extension_test_horizon(value INTEGER)"); err != nil {
		t.Fatal(err)
	}
	connection, err := s.writeDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var armed atomic.Bool
	var queries atomic.Int32
	err = connection.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, table, column, database string) int {
			if armed.Load() && op == sqlite3.SQLITE_READ && table == "messages" && column == "local_revision" && queries.Add(1) == 2 {
				time.Sleep(25 * time.Millisecond)
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	})
	connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	err = s.extensionTransaction(ctx, "account", func(tx *sql.Tx) error {
		if _, err := s.extensionCheckSources(ctx, tx, refs, false); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO extension_test_horizon VALUES(1)"); err != nil {
			return err
		}
		armed.Store(true)
		return nil
	})
	if !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("earlier source expired during later final SQL but published: %v", err)
	}
	if queries.Load() < 2 {
		t.Fatal("real final SQL interleaving was not exercised")
	}
	var published int
	if err := s.readDB.QueryRow("SELECT COUNT(*) FROM extension_test_horizon").Scan(&published); err != nil || published != 0 {
		t.Fatalf("expired source transaction escaped rollback: %d %v", published, err)
	}
}

func TestExtensionCommitRootOnlyRegistrationChecksExpiryAfterLaterSQL(t *testing.T) {
	s, clock, _ := indexTestStore(t)
	p := testPolicy(t, s)
	ctx := context.Background()
	e := event("root-horizon", "synthetic image source", 1, EventOriginal)
	e.ExpiresAt = time.UnixMilli(clock.Load()).Add(time.Minute).UTC()
	e.Media = []MediaReference{{Kind: "image", ProtocolReference: []byte("synthetic root horizon media"), Availability: "not_retained"}}
	applied := apply(t, s, e, p.Generation)
	ref, err := s.CurrentSource(ctx, e.Key)
	if err != nil || len(applied.MediaIDs) != 1 {
		t.Fatalf("root fixture source: %v", err)
	}
	reservation := extensionAdmit(t, s, ref, applied.MediaIDs[0], BudgetOriginals, 128)
	extensionPublish(t, s, reservation, 64)
	work, err := s.ClaimExtraction(ctx, ExtractionClaim{Account: "account", Version: extractionTestVersion(JobOCR), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	document := extractionTestDocument(work, "root-only-horizon", "actual bounded extraction root")
	if ok, err := s.CommitExtraction(ctx, ExtractionResult{Lease: work.Lease, ContentHash: work.ContentHash, Outcome: ExtractionText, Documents: []Document{document}}); err != nil || !ok {
		t.Fatalf("root fixture publication: %v %v", ok, err)
	}
	record, err := s.extensionAuthorizeRootAt(ctx, s.readDB, document.ID, s.now())
	if err != nil {
		t.Fatal(err)
	}
	clock.Store(e.ExpiresAt.Add(-10 * time.Millisecond).UnixMilli())
	if _, err = s.writeDB.Exec("CREATE TABLE extension_test_root_horizon(value INTEGER)"); err != nil {
		t.Fatal(err)
	}
	connection, err := s.writeDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var armed, delayed atomic.Bool
	err = connection.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, table, column, database string) int {
			if armed.Load() && op == sqlite3.SQLITE_READ && table == "extension_roots" && column == "id" && delayed.CompareAndSwap(false, true) {
				time.Sleep(25 * time.Millisecond)
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	})
	connection.Close()
	if err != nil {
		t.Fatal(err)
	}
	err = s.extensionTransaction(ctx, "account", func(tx *sql.Tx) error {
		// This caller registers only its root, without relying on a preceding
		// source-authorizer wrapper to supply the final clock horizons.
		if err := s.extensionRegisterRoot(tx, record); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO extension_test_root_horizon VALUES(1)"); err != nil {
			return err
		}
		armed.Store(true)
		return nil
	})
	if !delayed.Load() || !errors.Is(err, ErrSourceUnavailable) {
		t.Fatalf("root expired during final SQL but escaped its common horizon: delayed=%v err=%v", delayed.Load(), err)
	}
	var committed int
	if err := s.readDB.QueryRow("SELECT COUNT(*) FROM extension_test_root_horizon").Scan(&committed); err != nil || committed != 0 {
		t.Fatalf("expired root transaction escaped rollback: %d %v", committed, err)
	}
}

func TestKnownCompletedCASRejectionCannotBecomeUncertainDenial(t *testing.T) {
	s, policy, _, chat, _ := credentialFixture(t)
	credential := createCredential(t, s, "known-cas", CredentialPolicy{Account: "account", Chats: []ChatID{chat}})
	for _, payload := range []any{policyCommand{policy: &policy, expected: policy.Generation + 1}, setCredentialPolicyCommand{id: credential.ID, policy: credential.Policy, expected: credential.Revision + 1}} {
		for i := 0; i < 100; i++ {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			c := command{ctx: ctx, payload: payload, account: "account", control: true, sourceSensitive: commandSourceSensitive(payload, true), reply: make(chan commandResult, 1)}
			c.reply <- commandResult{err: ErrPolicyChanged}
			if _, err := s.awaitCommandResult(c); !errors.Is(err, ErrPolicyChanged) {
				t.Fatalf("known completed CAS rejection lost to cancellation: %v", err)
			}
		}
	}
	if _, err := s.readSafety("account"); err != nil {
		t.Fatalf("known source rejection denied archive: %v", err)
	}
	if _, err := s.ResolveCredentialID(context.Background(), credential.ID); err != nil {
		t.Fatalf("known credential rejection denied grant: %v", err)
	}
}

type extensionBlockedAdmissionCommand struct{ entered chan struct{} }

func (c extensionBlockedAdmissionCommand) run(_ *Store, ctx context.Context) (any, error) {
	close(c.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (extensionBlockedAdmissionCommand) priority() bool  { return true }
func (extensionBlockedAdmissionCommand) account() string { return "account" }
func (extensionBlockedAdmissionCommand) sensitivity() extensionSensitivity {
	return extensionBackground
}

func TestKnownCredentialNonAdmissionDoesNotRevokeGrant(t *testing.T) {
	s, _, _, chat, _ := credentialFixture(t)
	credential := createCredential(t, s, "not-admitted", CredentialPolicy{Account: "account", Chats: []ChatID{chat}})
	ctx, release := context.WithCancel(context.Background())
	defer release()
	entered := make(chan struct{})
	result := make(chan error, 1)
	go func() { _, err := s.submitExtension(ctx, extensionBlockedAdmissionCommand{entered}, 0); result <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not reach bounded admission fixture")
	}
	for i := 0; i < cap(s.controls); i++ {
		s.controls <- command{ctx: context.Background(), payload: drainCommand{}, control: true, reply: make(chan commandResult, 1)}
	}
	bounded, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := s.RevokeCredential(bounded, credential.ID, credential.Revision); !errors.Is(err, ErrQueueLimitExceeded) {
		t.Fatalf("known non-admission: %v", err)
	}
	release()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("bounded writer fixture failed to release")
	}
	if _, err := s.ResolveCredentialID(context.Background(), credential.ID); err != nil {
		t.Fatalf("known non-admitted revoke denied credential: %v", err)
	}
}
