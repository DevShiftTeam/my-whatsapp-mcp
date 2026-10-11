//go:build sqlite_fts5

package app

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"github.com/Bodokh/my-whatsapp-mcp/internal/outbound"
	"testing"
	"time"
)

type blockedSendConnector struct {
	*syntheticConnector
	started, release chan struct{}
}

func (c *blockedSendConnector) SendOutbound(ctx context.Context, s outbound.Store, g archive.CredentialGrant, r outbound.Request) (outbound.Receipt, error) {
	close(c.started)
	select {
	case <-c.release:
	case <-ctx.Done():
		return outbound.Receipt{}, ctx.Err()
	}
	_, err := s.SendTarget(ctx, g, r.ChatID)
	return outbound.Receipt{}, err
}
func TestOwnerWithdrawalCanCommitWhileOutboundIsBlocked(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "permission", true: "connection"}[revoke], func(t *testing.T) {
			a := pairedNativeTestApp(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			id, _, err := a.IssueNativeClientConfig(ctx, archive.CredentialCreate{Label: "Sender", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, "fixture", mcpserver.NativeAdapterEndpoint)
			if err != nil {
				t.Fatal(err)
			}
			g, err := a.store.ResolveCredentialID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			page, err := a.store.ListChats(ctx, g.Scope, archive.ChatListRequest{})
			if err != nil || len(page.Chats) == 0 {
				t.Fatal(err)
			}
			chat := page.Chats[0].ID
			a.demo = false
			defer func() { a.demo = true; a.runtime = nil }()
			record := a.authState.NativeClients[0]
			if err = a.SetSendPermission(ctx, record.IntentID, g.CredentialRevision, []archive.ChatID{chat}); err != nil {
				t.Fatal(err)
			}
			connector := &blockedSendConnector{syntheticConnector: &syntheticConnector{account: demoAccount, snapshot: ingestion.Status{Connection: "connected"}}, started: make(chan struct{}), release: make(chan struct{})}
			a.runtime = connector
			sent := make(chan error, 1)
			go func() {
				_, err := a.Send(ctx, g, outbound.Request{ChatID: chat, RequestID: "blocked", Text: "hello"})
				sent <- err
			}()
			select {
			case <-connector.started:
			case <-ctx.Done():
				t.Fatal("send did not begin")
			}
			if _, err = a.Connections(ctx); err != nil {
				t.Fatal(err)
			}
			withdrawn := make(chan error, 1)
			if revoke {
				go func() {
					withdrawn <- a.RevokeConnection(ctx, localui.ConnectionRevokeRequest{ID: record.IntentID, Revision: g.CredentialRevision})
				}()
			} else {
				go func() { withdrawn <- a.SetSendPermission(ctx, record.IntentID, g.CredentialRevision, nil) }()
			}
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			removed := false
			for !removed {
				select {
				case <-ticker.C:
					_, err = a.store.SendTarget(ctx, g, chat)
					removed = err != nil
				case <-timer.C:
					close(connector.release)
					t.Fatal("owner withdrawal waited behind provider work")
				}
			}
			close(connector.release)
			if err = <-sent; err == nil {
				t.Fatal("withdrawn authority reached send boundary")
			}
			if err = <-withdrawn; err != nil {
				t.Fatal(err)
			}
		})
	}
}
