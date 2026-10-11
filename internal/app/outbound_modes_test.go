//go:build sqlite_fts5

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
)

func TestOwnerSendModeEditPreservesExistingRemoteTokenAndReadScope(t *testing.T) {
	app, connection, grant, token, chat := outboundBridgeFixture(t)
	ctx := context.Background()
	before, err := app.Connections(ctx)
	if err != nil || len(before.Items) != 1 {
		t.Fatal("synthetic existing connection unavailable")
	}
	item := before.Items[0]
	if item.SendPermission.Mode != "selected" || item.SendPermission.Revision != 1 || !reflect.DeepEqual(item.SendPermission.Chats, []archive.ChatID{chat}) || !reflect.DeepEqual(item.Scope.Chats, []archive.ChatID{chat}) {
		t.Fatalf("owner summary incomplete: %#v", item)
	}
	if err = app.SetSendPermissionMode(ctx, connection.ID, connection.Revision, archive.SendPermission{Mode: "all"}, 1); err != nil {
		t.Fatal(err)
	}
	if err = app.SetSendPermissionMode(ctx, connection.ID, connection.Revision, archive.SendPermission{Mode: "none"}, 1); !errors.Is(err, archive.ErrSendConflict) {
		t.Fatalf("stale owner edit accepted: %v", err)
	}
	after, err := app.Connections(ctx)
	if err != nil || len(after.Items) != 1 {
		t.Fatal("edited connection unavailable")
	}
	updated := after.Items[0]
	if updated.SendPermission.Mode != "all" || updated.SendPermission.Revision != 2 || len(updated.SendPermission.Chats) != 0 {
		t.Fatalf("edited permission summary: %#v", updated.SendPermission)
	}
	updated.SendPermission = item.SendPermission
	if !reflect.DeepEqual(item, updated) {
		t.Fatal("permission edit changed connection identity, expiry or read scope")
	}
	secret, err := app.RevealRemoteCredential(ctx, localui.ConnectionRevealRequest{ID: connection.ID, Revision: connection.Revision})
	if err != nil || secret.Token != token {
		t.Fatal("permission edit rotated existing token")
	}
	current, err := app.store.ResolveCredentialID(ctx, grant.CredentialID)
	if err != nil || !reflect.DeepEqual(grant, current) {
		t.Fatal("permission edit changed read grant")
	}
	if _, err = app.store.SendTarget(ctx, grant, chat); err != nil {
		t.Fatal(err)
	}
	if err = app.SetSendPermissionMode(ctx, connection.ID, connection.Revision, archive.SendPermission{Mode: "none"}, 2); err != nil {
		t.Fatal(err)
	}
	if _, err = app.store.SendTarget(ctx, grant, chat); !errors.Is(err, archive.ErrSendForbidden) {
		t.Fatal("owner none edit retained sending")
	}
}
