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
	a.data.RLock()
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
	a.data.RLock()
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

// SetSendPermission is owner-only; MCP cannot grant itself outbound authority.
func (a *Application) SetSendPermission(ctx context.Context, id string, revision uint64, chats []archive.ChatID) error {
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
	return a.store.SetSendPermission(ctx, g, chats)
}
