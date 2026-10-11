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
		if a.closed || !a.mcpEnabled || !a.remoteRouteReady() || a.mcp.remoteHandler == nil {
			a.data.RUnlock()
			remoteMCPUnavailable(w)
			return
		}
		m, handler, root := a.mcp, a.mcp.remoteHandler, a.queryContext
		if root.Err() != nil {
			a.data.RUnlock()
			remoteMCPUnavailable(w)
			return
		}
		// Admission and unpublication share data.Lock. The worker lease keeps
		// captured readers and stores alive until Close/reset joins the handler,
		// without preventing an owner from withdrawing authority during I/O.
		m.workers.Add(1)
		a.data.RUnlock()
		defer m.workers.Done()
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(root, cancel)
		defer stop()
		defer cancel()
		handler.ServeHTTP(w, r.WithContext(ctx))
	})
}

func remoteMCPUnavailable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `{"error":"mcp_unavailable"}`)
}
