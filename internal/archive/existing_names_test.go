//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

func existingNamesRevision(t *testing.T, s *Store) ArchiveRevision {
	t.Helper()
	var revision ArchiveRevision
	if err := s.readDB.QueryRow("SELECT archive_revision FROM archive_meta WHERE id=1").Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}

func existingNamesCounts(t *testing.T, s *Store) [5]int {
	t.Helper()
	var counts [5]int
	if err := s.readDB.QueryRow(`SELECT (SELECT count(*) FROM accounts),
 (SELECT count(*) FROM chats),(SELECT count(*) FROM identities),
 (SELECT count(*) FROM identity_aliases),(SELECT count(*) FROM chat_aliases)`).
		Scan(&counts[0], &counts[1], &counts[2], &counts[3], &counts[4]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestExistingNameTargetsKeysetVisitsOnlyExistingRows(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	for i := 0; i < 8; i++ {
		e := event(fmt.Sprintf("name-message-%d", i), "retained text", 1, EventOriginal)
		e.Key.Chat = fmt.Sprintf("synthetic-chat-%d@g.us", i)
		e.Key.Sender = fmt.Sprintf("synthetic-sender-%d@s.whatsapp.net", i)
		apply(t, s, e, p.Generation)
		if err := s.SetSourceNames(context.Background(), SourceNames{Account: e.Key.Account, Chat: e.Key.Chat, Sender: e.Key.Sender, ChatName: fmt.Sprintf("Group %d", i), SenderName: fmt.Sprintf("Contact %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	before := existingNamesCounts(t, s)
	seen := make(map[string]bool)
	previous, after := "", ""
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber > 6 {
			t.Fatal("keyset pagination did not terminate")
		}
		page, next, err := s.ExistingNameTargets(context.Background(), "account", after, 3)
		if err != nil || len(page) == 0 || len(page) > 3 {
			t.Fatalf("page %d count %d: %v", pageNumber, len(page), err)
		}
		for _, target := range page {
			if target.Key <= previous || seen[target.Key] || (target.Chat == "") == (target.Sender == "") || target.Name == "" {
				t.Fatalf("invalid target shape or ordering: %#v", target)
			}
			if target.Chat != "" && !strings.HasPrefix(target.Key, "c:") || target.Sender != "" && !strings.HasPrefix(target.Key, "i:") {
				t.Fatalf("target kind disagrees with key: %#v", target)
			}
			seen[target.Key], previous = true, target.Key
		}
		if next == "" {
			break
		}
		if next != previous {
			t.Fatalf("continuation skipped the last returned row: %q != %q", next, previous)
		}
		after = next
	}
	if len(seen) != before[1]+before[2] || existingNamesCounts(t, s) != before {
		t.Fatal("name lookup skipped existing rows or created new metadata")
	}
	page, next, err := s.ExistingNameTargets(context.Background(), "account", previous, 3)
	if err != nil || len(page) != 0 || next != "" {
		t.Fatalf("exhausted page retained a continuation: %d %q %v", len(page), next, err)
	}
}

func TestExistingNameTargetsUsesBoundedPrimaryKeyRangeWindows(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	apply(t, s, event("name-range-plan", "existing", 1, EventOriginal), p.Generation)
	rows, err := s.readDB.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+existingNameTargetsSQL,
		true, "", 65, "", 65, "account", "account", 65)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	chatRange, identityRange := false, false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "(id>?)") {
			chatRange = chatRange || strings.HasPrefix(detail, "SEARCH chats ")
			identityRange = identityRange || strings.HasPrefix(detail, "SEARCH identities ")
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !chatRange || !identityRange {
		t.Fatalf("name keyset did not use both bounded primary-key ranges: chats=%v identities=%v", chatRange, identityRange)
	}
}

func TestExistingNameTargetsRejectsUnboundWrongAndUnsafeAccounts(t *testing.T) {
	s := testArchive(t)
	ctx := context.Background()
	if _, _, err := s.ExistingNameTargets(ctx, "account", "", 64); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unbound lookup: %v", err)
	}
	if err := s.RefreshExistingSourceNames(ctx, []SourceNames{{Account: "account", Chat: "missing", ChatName: "Must Not Bind"}}); !errors.Is(err, ErrUnauthorized) || existingNamesCounts(t, s) != [5]int{} {
		t.Fatalf("unbound refresh created account metadata: %v", err)
	}
	p := testPolicy(t, s)
	apply(t, s, event("name-account", "existing", 1, EventOriginal), p.Generation)
	if _, _, err := s.ExistingNameTargets(ctx, "other-account", "", 64); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong account lookup: %v", err)
	}
	if err := s.denyImmediate("account", "mutation_metadata_missing"); err != nil {
		t.Fatal(err)
	}
	if targets, next, err := s.ExistingNameTargets(ctx, "account", "", 64); !errors.Is(err, ErrReconciliationRequired) || targets != nil || next != "" {
		t.Fatalf("unsafe lookup returned targets: %d %q %v", len(targets), next, err)
	}
}

func TestExistingNamesBoundsAndLabelValidation(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	apply(t, s, event("name-bounds", "existing", 1, EventOriginal), p.Generation)
	for _, limit := range []int{-1, 65} {
		if _, _, err := s.ExistingNameTargets(context.Background(), "account", "", limit); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("lookup limit %d: %v", limit, err)
		}
	}
	for _, after := range []string{"raw-private-source@s.whatsapp.net", "c:", "i:\x00", "c:" + strings.Repeat("a", 257)} {
		if _, _, err := s.ExistingNameTargets(context.Background(), "account", after, 2); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("invalid internal continuation accepted: %v", err)
		}
	}
	valid := SourceNames{Account: "account", Chat: "chat", ChatName: "ליאור בודוך"}
	for _, bad := range []SourceNames{
		{Account: "account", Chat: "chat", ChatName: strings.Repeat("x", 257)},
		{Account: "account", Sender: "sender", SenderName: "\x00"},
		{Account: "account", Sender: "sender", SenderName: string([]byte{0xff})},
		{Account: "account", ChatName: "missing source"},
	} {
		if err := s.RefreshExistingSourceNames(context.Background(), []SourceNames{valid, bad}); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("invalid batch accepted: %v", err)
		}
	}
	tooMany := make([]SourceNames, 65)
	for i := range tooMany {
		tooMany[i] = valid
	}
	if err := s.RefreshExistingSourceNames(context.Background(), tooMany); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("oversized refresh: %v", err)
	}
	var name string
	if err := s.readDB.QueryRow("SELECT name FROM chats WHERE source_chat='chat'").Scan(&name); err != nil || name != "" {
		t.Fatalf("invalid batch partially updated names: %q %v", name, err)
	}
}

func TestRefreshExistingNamesNeverCreatesRowsOrBlanksLabels(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("name-existing", "original content", 1, EventOriginal)
	apply(t, s, e, p.Generation)
	ctx := context.Background()
	beforeCounts := existingNamesCounts(t, s)
	beforeRef, err := s.CurrentSource(ctx, e.Key)
	if err != nil {
		t.Fatal(err)
	}
	names := SourceNames{Account: "account", Chat: "chat", Sender: "sender", ChatName: "Family Group", SenderName: "ליאור בודוך"}
	before := existingNamesRevision(t, s)
	if err = s.RefreshExistingSourceNames(ctx, []SourceNames{names, {Account: "account", Chat: "unknown@g.us", Sender: "unknown@s.whatsapp.net", ChatName: "Unknown", SenderName: "Unknown"}}); err != nil {
		t.Fatal(err)
	}
	changed := existingNamesRevision(t, s)
	if changed != before+1 || existingNamesCounts(t, s) != beforeCounts {
		t.Fatal("refresh created metadata rows or failed to bump one batch revision")
	}
	afterRef, err := s.CurrentSource(ctx, e.Key)
	if err != nil || afterRef != beforeRef {
		t.Fatalf("label refresh changed source content authority: %#v %v", afterRef, err)
	}
	changedEpoch, err := s.readSafety(e.Key.Account)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RefreshExistingSourceNames(ctx, []SourceNames{names, {Account: "account", Chat: "chat", Sender: "sender"}}); err != nil {
		t.Fatal(err)
	}
	if got := existingNamesRevision(t, s); got != changed {
		t.Fatalf("blank or identical refresh changed revision: %d -> %d", changed, got)
	}
	if epoch, err := s.readSafety(e.Key.Account); err != nil || epoch != changedEpoch {
		t.Fatalf("blank or identical refresh invalidated current metadata: %d -> %d, %v", changedEpoch, epoch, err)
	}
	var chatName, senderName string
	if err = s.readDB.QueryRow(`SELECT c.name,i.name FROM chats c CROSS JOIN identities i WHERE c.source_chat='chat' AND i.source_identity='sender'`).Scan(&chatName, &senderName); err != nil || chatName != names.ChatName || senderName != names.SenderName {
		t.Fatalf("blank refresh erased labels: %q %q %v", chatName, senderName, err)
	}
}

func TestRefreshExistingNamesRejectsWrongAccountAndStaleAliasAtomically(t *testing.T) {
	s, _, alias, _ := aliasNoopFixture(t)
	ctx := context.Background()
	beforeCounts := existingNamesCounts(t, s)
	beforeRevision := existingNamesRevision(t, s)
	good := SourceNames{Account: alias.Account, Chat: alias.PhoneJID, ChatName: "Current Contact"}
	for _, batch := range [][]SourceNames{
		{{Account: "other-account", Chat: alias.PhoneJID, ChatName: "Wrong Account"}},
		{good, {Account: "other-account", Sender: alias.PhoneJID, SenderName: "Wrong Account"}},
		{good, {Account: alias.Account, Sender: alias.LID, SenderName: "Stale Alias"}},
	} {
		want := error(ErrUnauthorized)
		if batch[len(batch)-1].Sender == alias.LID {
			want = ErrSourceUnavailable
		}
		if err := s.RefreshExistingSourceNames(ctx, batch); !errors.Is(err, want) {
			t.Fatalf("wrong-account/stale-alias batch: %v, want %v", err, want)
		}
		if existingNamesCounts(t, s) != beforeCounts || existingNamesRevision(t, s) != beforeRevision {
			t.Fatal("rejected batch changed metadata or revision")
		}
	}
	var name string
	if err := s.readDB.QueryRow("SELECT name FROM chats WHERE source_chat=?", alias.PhoneJID).Scan(&name); err != nil || name != "" {
		t.Fatalf("later rejected target failed to roll back earlier update: %q %v", name, err)
	}
	targets, _, err := s.ExistingNameTargets(ctx, alias.Account, "", 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if target.Chat == alias.LID || target.Sender == alias.LID {
			t.Fatal("lookup returned a stale alias rather than the canonical source")
		}
	}
}

func TestRefreshExistingNamesInvalidatesOldMetadataWithoutRestoringRevokedGrant(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	e := readerEvent("name-guard", "allowed", "original keyword label test", now.Add(-time.Minute))
	id := readerApply(t, s, p, e)
	scope := readerScope(p, readerChat(t, s, id))
	credential := createCredential(t, s, "name-refresh-client", CredentialPolicy{Account: e.Key.Account, Chats: []ChatID{readerChat(t, s, id)}})
	if err := s.RevokeCredential(context.Background(), credential.ID, credential.Revision); err != nil {
		t.Fatal(err)
	}
	_, _, listGuard, _ := guardedMetadataRead(t, s, scope, "list")
	_, bodyGuard, _ := guardedBodyRead(t, s, scope, "search", e, id)
	for _, guard := range []ResponseGuard{listGuard, bodyGuard} {
		if _, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err != nil {
			t.Fatalf("current owner metadata already denied before names changed: %v", err)
		}
	}
	if err := s.RefreshExistingSourceNames(context.Background(), []SourceNames{{Account: e.Key.Account, Chat: e.Key.Chat, Sender: e.Key.Sender, ChatName: "Updated Group", SenderName: "Updated Contact"}}); err != nil {
		t.Fatal(err)
	}
	for _, guard := range []ResponseGuard{listGuard, bodyGuard} {
		if _, err := s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: guard}); err == nil {
			t.Fatal("old response retained stale display names")
		}
	}
	if _, err := s.ResolveCredentialID(context.Background(), credential.ID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("name refresh restored revoked credential: %v", err)
	}
	page, err := s.Search(context.Background(), scope, SearchRequest{Query: "original keyword"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].SenderName != "Updated Contact" || page.Hits[0].ChatName != "Updated Group" {
		t.Fatalf("fresh scoped search lost source or labels: %#v %v", page, err)
	}
}

type existingNamesAmbiguousCommitDriver struct {
	driver.Driver
	armed, committed *atomic.Bool
}

type existingNamesAmbiguousCommitConn struct {
	originalDownloadCommitConn
	owner *existingNamesAmbiguousCommitDriver
}

type existingNamesAmbiguousCommitTx struct {
	driver.Tx
	owner *existingNamesAmbiguousCommitDriver
}

func (d *existingNamesAmbiguousCommitDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return existingNamesAmbiguousCommitConn{originalDownloadCommitConn: originalDownloadCommitConn{Conn: conn}, owner: d}, nil
}

func (c existingNamesAmbiguousCommitConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, ErrArchiveUnavailable
	}
	tx, err := beginner.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return existingNamesAmbiguousCommitTx{Tx: tx, owner: c.owner}, nil
}

func (tx existingNamesAmbiguousCommitTx) Commit() error {
	err := tx.Tx.Commit()
	if err == nil && tx.owner.armed.Load() {
		tx.owner.committed.Store(true)
		return ErrArchiveUnavailable
	}
	return err
}

func TestRefreshExistingNamesAmbiguousCommitFencesMetadataWithoutSourceDenial(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	e := readerEvent("name-ambiguous", "allowed", "original keyword source", now.Add(-time.Minute))
	id := readerApply(t, s, p, e)
	scope := readerScope(p, readerChat(t, s, id))
	_, _, oldGuard, _ := guardedMetadataRead(t, s, scope, "list")
	armed, committed := new(atomic.Bool), new(atomic.Bool)
	name := fmt.Sprintf("existing_names_ambiguous_commit_%d", originalDownloadCommitDriverSequence.Add(1))
	sql.Register(name, &existingNamesAmbiguousCommitDriver{Driver: s.writeDB.Driver(), armed: armed, committed: committed})
	if err := s.writeDB.Close(); err != nil {
		t.Fatal(err)
	}
	dsn := url.URL{Scheme: "file", Path: filepath.Join(s.privateDir, "archive.sqlite"), RawQuery: "_foreign_keys=on&_journal_mode=WAL&_synchronous=FULL&_busy_timeout=500&_txlock=immediate"}
	var err error
	s.writeDB, err = sql.Open(name, dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	s.writeDB.SetMaxOpenConns(1)
	armed.Store(true)
	err = s.RefreshExistingSourceNames(context.Background(), []SourceNames{{Account: e.Key.Account, Chat: e.Key.Chat, ChatName: "Committed New Label"}})
	armed.Store(false)
	if !errors.Is(err, ErrArchiveUnavailable) || !committed.Load() {
		t.Fatalf("fixture did not return an error after a real metadata COMMIT: %v %v", err, committed.Load())
	}
	var label string
	if err = s.readDB.QueryRow("SELECT name FROM chats WHERE source_chat=?", e.Key.Chat).Scan(&label); err != nil || label != "Committed New Label" {
		t.Fatalf("real COMMIT did not preserve new label: %q %v", label, err)
	}
	if _, err = s.CheckResponse(context.Background(), scope, ResponseCheck{Guard: oldGuard}); !errors.Is(err, ErrPolicyChanged) {
		t.Fatalf("ambiguous commit retained old metadata authority: %v", err)
	}
	if _, err = s.readSafety(e.Key.Account); err != nil {
		t.Fatalf("optional ambiguous name commit poisoned source access: %v", err)
	}
	page, err := s.Search(context.Background(), scope, SearchRequest{Query: "original keyword"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].ChatName != label {
		t.Fatalf("current source failed after ambiguous metadata commit: %#v %v", page, err)
	}
}

func TestRefreshExistingNamesTimeoutDoesNotDenyReadableArchive(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	e := event("name-timeout", "still searchable", 1, EventOriginal)
	apply(t, s, e, p.Generation)
	before := existingNamesRevision(t, s)
	conn, err := s.writeDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = s.RefreshExistingSourceNames(ctx, []SourceNames{{Account: e.Key.Account, Chat: e.Key.Chat, ChatName: "Cannot Commit"}})
	cancel()
	if closeErr := conn.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked name write did not time out: %v", err)
	}
	if err = s.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.readSafety(e.Key.Account); err != nil || existingNamesRevision(t, s) != before {
		t.Fatalf("optional name timeout denied archive or committed stale work: %v", err)
	}
	page, err := s.Search(context.Background(), ReadScope{Account: e.Key.Account, Principal: "owner", AllChats: true, PolicyGeneration: p.Generation}, SearchRequest{Query: "searchable"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].ChatName != "" {
		t.Fatalf("optional name timeout changed readable archive: %#v %v", page, err)
	}
}

func TestExistingNameTargetsRejectsDenialDuringLookup(t *testing.T) {
	s := testArchive(t)
	p := testPolicy(t, s)
	apply(t, s, event("name-race", "retained", 1, EventOriginal), p.Generation)
	s.readDB.SetMaxOpenConns(1)
	s.readDB.SetMaxIdleConns(1)
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	defer resume()
	conn, err := s.readDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var armed atomic.Bool
	err = conn.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, table, column, database string) int {
			if armed.Load() && op == sqlite3.SQLITE_READ && table == "chats" {
				enteredOnce.Do(func() { close(entered); <-release })
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
	armed.Store(true)
	done := make(chan error, 1)
	go func() {
		_, _, lookupErr := s.ExistingNameTargets(context.Background(), "account", "", 64)
		done <- lookupErr
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("lookup did not reach metadata SQL")
	}
	if err = s.denyImmediate("account", "mutation_metadata_missing"); err != nil {
		t.Fatal(err)
	}
	resume()
	select {
	case err = <-done:
		if !errors.Is(err, ErrReconciliationRequired) {
			t.Fatalf("lookup returned metadata after concurrent denial: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lookup did not join after metadata SQL resumed")
	}
}
