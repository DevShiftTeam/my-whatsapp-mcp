package app

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/outbound"
	"time"
)

type outboundRuntime interface {
	SendOutbound(context.Context, outbound.Store, archive.CredentialGrant, outbound.Request) (outbound.Receipt, error)
}

func (a *Application) Send(ctx context.Context, g archive.CredentialGrant, request outbound.Request) (outbound.Receipt, error) {
	if a.snapshot != nil {
		return outbound.Receipt{}, archive.ErrHistoricalSnapshotReadOnly
	}
	if !a.data.TryRLock() {
		return outbound.Receipt{}, outbound.ErrUnavailable
	}
	if a.closed || a.demo || a.paused || a.runtime == nil || ctx.Err() != nil {
		a.data.RUnlock()
		return outbound.Receipt{}, outbound.ErrUnavailable
	}
	runtime, ok := a.runtime.(outboundRuntime)
	store, root := a.store, a.queryContext
	a.data.RUnlock()
	if !ok {
		return outbound.Receipt{}, outbound.ErrUnavailable
	}
	bounded, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	stop := context.AfterFunc(root, cancel)
	defer stop()
	// Runtime.op owns the captured session/store until provider work and journaling
	// finish. Close/reset stop and close that runtime before closing its store.
	// No app data lock is held during I/O, so owner polling/revocation can proceed.
	return runtime.SendOutbound(bounded, store, g, request)
}
func (a *Application) SendStatus(ctx context.Context, g archive.CredentialGrant, id string) (outbound.Receipt, error) {
	if !a.data.TryRLock() {
		return outbound.Receipt{}, outbound.ErrUnavailable
	}
	defer a.data.RUnlock()
	if a.closed || !outbound.ValidRequestID(id) {
		return outbound.Receipt{}, outbound.ErrInvalid
	}
	r, err := a.store.SendStatus(ctx, g, id)
	if err != nil {
		return outbound.Receipt{}, err
	}
	return outbound.ReceiptFrom(r, false), nil
}

// CheckSendResponse makes receipt publication subject to the original grant and
// its current send permission. It performs no provider or journal work.
func (a *Application) CheckSendResponse(ctx context.Context, g archive.CredentialGrant, receipt outbound.Receipt) error {
	if !a.data.TryRLock() {
		return outbound.ErrUnavailable
	}
	defer a.data.RUnlock()
	if a.closed || a.store == nil || !outbound.ValidRequestID(receipt.RequestID) {
		return outbound.ErrUnavailable
	}
	_, err := a.store.SendTarget(ctx, g, receipt.ChatID)
	return err
}

// SetSendPermission is owner-only; MCP cannot grant itself outbound authority.
func (a *Application) SetSendPermission(ctx context.Context, id string, revision uint64, chats []archive.ChatID) error {
	if a.snapshot != nil {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	return a.setSendPermission(ctx, id, revision, func(g archive.CredentialGrant) error {
		return a.store.SetSendPermission(ctx, g, chats)
	})
}

func (a *Application) SetSendPermissionMode(ctx context.Context, id string, revision uint64, permission archive.SendPermission, expected uint64) error {
	if a.snapshot != nil {
		return archive.ErrHistoricalSnapshotReadOnly
	}
	return a.setSendPermission(ctx, id, revision, func(g archive.CredentialGrant) error {
		return a.store.SetSendPermissionMode(ctx, g, permission, expected)
	})
}

func (a *Application) setSendPermission(ctx context.Context, id string, revision uint64, write func(archive.CredentialGrant) error) error {
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.RLock()
	defer a.data.RUnlock()
	if a.closed || a.demo || revision == 0 {
		return outbound.ErrUnavailable
	}
	var credential archive.CredentialID
	for _, r := range a.authState.NativeClients {
		if r.IntentID == id && r.State == "ready" {
			credential = r.CredentialID
		}
	}
	for _, r := range a.authState.RemoteClients {
		if r.IntentID == id && r.State == "ready" {
			credential = r.CredentialID
		}
	}
	if credential == "" {
		return archive.ErrSendForbidden
	}
	g, err := a.store.ResolveCredentialID(ctx, credential)
	if err != nil {
		return err
	}
	if g.CredentialRevision != revision {
		return archive.ErrSendConflict
	}
	return write(g)
}
