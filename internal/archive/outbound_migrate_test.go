//go:build sqlite_fts5

package archive

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestOutboundUpgradePreservesExistingArchiveAndReadGrant(t *testing.T) {
	s, _, now, chat, _ := credentialFixture(t)
	ctx := context.Background()
	c := createCredential(t, s, "existing read connection", CredentialPolicy{Account: "account", Chats: []ChatID{chat}})
	before, err := s.ResolveCredentialID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ListChats(ctx, before.Scope, ChatListRequest{})
	if err != nil || len(page.Chats) != 1 {
		t.Fatal("synthetic existing archive unavailable")
	}
	// Represent the canonical v11 database before the new append-only migration.
	// All existing migration checksums and source/credential rows stay intact.
	for _, statement := range []string{"DROP TABLE send_requests", "DROP TABLE send_permissions", "DELETE FROM schema_migrations WHERE version=12"} {
		if _, err := s.writeDB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	s = reopenCredentialStore(t, s, now)
	after, err := s.ResolveCredentialID(ctx, c.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("outbound migration changed existing read authority")
	}
	updated, err := s.ListChats(ctx, after.Scope, ChatListRequest{})
	if err != nil || !reflect.DeepEqual(page, updated) {
		t.Fatal("outbound migration changed existing archive results")
	}
	for _, table := range []string{"send_permissions", "send_requests"} {
		var count int
		if err := s.readDB.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("upgrade granted or journaled outbound work")
		}
	}
	if _, err := s.SendTarget(ctx, after, chat); !errors.Is(err, ErrSendForbidden) {
		t.Fatal("existing read grant acquired send permission")
	}
}
