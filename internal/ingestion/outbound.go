package ingestion

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/outbound"
)

// SendOutbound shares the live linked client and serializes against connector lifecycle.
func (r *Runtime) SendOutbound(ctx context.Context, store outbound.Store, g archive.CredentialGrant, request outbound.Request) (outbound.Receipt, error) {
	r.op.Lock()
	defer r.op.Unlock()
	if ctx.Err() != nil {
		return outbound.Receipt{}, ctx.Err()
	}
	r.mu.Lock()
	ready := !r.closed && !r.status.Paused && !r.status.ReconciliationRequired && r.status.Connection == "connected"
	r.mu.Unlock()
	if !ready || r.client == nil || r.SourceAccount() != g.Scope.Account {
		return outbound.Receipt{}, outbound.ErrUnavailable
	}
	return outbound.Send(ctx, store, r.client, g, request)
}
