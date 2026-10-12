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
	for _, statement := range []string{"DROP TABLE unknown_history_codes", "DROP TABLE unknown_history_coverage", "DROP TABLE extension_document_promotions", "ALTER TABLE extension_media_associations DROP COLUMN filename", "DROP TABLE send_permission_modes", "DROP TABLE send_requests", "DROP TABLE send_permissions", "DELETE FROM schema_migrations WHERE version>=12"} {
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

func TestSendPermissionModeUpgradePreservesLegacyFiniteGrantWithoutWidening(t *testing.T) {
	s, _, now, chat, other := credentialFixture(t)
	ctx := context.Background()
	credential := createCredential(t, s, "legacy finite sender", CredentialPolicy{Account: "account", AllChats: true})
	grant, err := s.ResolveCredentialID(ctx, credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetSendPermission(ctx, grant, []ChatID{chat}); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"DROP TABLE unknown_history_codes", "DROP TABLE unknown_history_coverage", "DROP TABLE extension_document_promotions", "ALTER TABLE extension_media_associations DROP COLUMN filename", "DROP TABLE send_permission_modes", "DELETE FROM schema_migrations WHERE version>=13"} {
		if _, err = s.writeDB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	s = reopenCredentialStore(t, s, now)
	current, err := s.ResolveCredentialID(ctx, credential.ID)
	if err != nil || !reflect.DeepEqual(grant, current) {
		t.Fatal("mode migration changed read credential")
	}
	permission, err := s.GetSendPermission(ctx, current)
	if err != nil || permission.Mode != "selected" || permission.Revision != 0 || !reflect.DeepEqual(permission.Chats, []ChatID{chat}) {
		t.Fatalf("legacy permission lost: %#v %v", permission, err)
	}
	if _, err = s.SendTarget(ctx, current, chat); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SendTarget(ctx, current, other); !errors.Is(err, ErrSendForbidden) {
		t.Fatal("mode migration broadened legacy authority")
	}
	if err = s.SetSendPermissionMode(ctx, current, SendPermission{Mode: "none"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SendTarget(ctx, current, chat); !errors.Is(err, ErrSendForbidden) {
		t.Fatal("legacy permission was not removable")
	}
}
