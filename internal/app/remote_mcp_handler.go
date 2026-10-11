package app

import (
	"context"
	"io"
	"net/http"
)

// RemoteMCPHandler borrows the app-owned, scoped remote transport. It resolves
// publication on every request, sharing the dedicated listener's exact handler
// and admission budget rather than caching an archive reader across reset.
func (a *Application) RemoteMCPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Lifecycle contention must not queue anonymous public requests ahead
		// of the existing MCP admission and deadline checks.
		if !a.data.TryRLock() {
			remoteMCPUnavailable(w)
			return
		}
		defer a.data.RUnlock()
		if a.closed || !a.mcpEnabled || !a.remoteRouteReady() || a.mcp.remoteHandler == nil {
			remoteMCPUnavailable(w)
			return
		}
		// Close and reset cancel this query owner before waiting for data.Lock.
		// Joining its cancellation keeps borrowed requests within the same
		// lifecycle as the dedicated listener, while the read lock fences store
		// and reader replacement until this bounded handler has returned.
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(a.queryContext, cancel)
		defer stop()
		defer cancel()
		a.mcp.remoteHandler.ServeHTTP(w, r.WithContext(ctx))
	})
}

func remoteMCPUnavailable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `{"error":"mcp_unavailable"}`)
}
