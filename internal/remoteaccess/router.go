package remoteaccess

import (
	"net/http"
	"net/url"

	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
)

// Exact paths only: no ServeMux cleaning, redirects, decoded path aliases, or
// forwarding-header trust. Owner must already authenticate remote principals.
func newRouter(origin string, browser bool, h Handlers) (http.Handler, error) {
	if !canonicalOrigin(origin) || h.MCP == nil || browser && h.Owner == nil {
		return nil, ErrPrepare
	}
	u, _ := url.Parse(origin)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Host != u.Host || (r.URL.Host != "" && r.URL.Host != u.Host) || (r.URL.Scheme != "" && r.URL.Scheme != "https") {
			http.Error(w, "host_denied", 403)
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 || len(origins) == 1 && origins[0] != origin {
			http.Error(w, "origin_denied", 403)
			return
		}
		if r.URL.RawPath != "" || r.URL.EscapedPath() != r.URL.Path || r.URL.Fragment != "" || r.URL.ForceQuery || r.URL.RawQuery != "" && r.URL.Path != "/api/chats" && r.URL.Path != "/api/voice/recovery" {
			http.NotFound(w, r)
			return
		}
		if mcpPaths[r.URL.Path] || mcpserver.AttachmentDownloadRoute(r.URL.Path) {
			h.MCP.ServeHTTP(w, r)
			return
		}
		if browser && ownerPaths[r.URL.Path] {
			h.Owner.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}), nil
}

var mcpPaths = map[string]bool{
	"/mcp":              true,
	"/api/send/message": true, "/api/send/media": true,
	"/api/send/voice": true, "/api/send/status": true,
}

var ownerPaths = map[string]bool{
	"/": true, "/assets/app.css": true, "/assets/app.js": true,
	"/assets/theme.js": true, "/assets/selects.js": true, "/assets/logo.png": true,
	"/api/interface": true, "/api/remote/login": true, "/api/remote/logout": true,
	"/api/session": true, "/api/setup": true, "/api/policy": true,
	"/api/pairing/start": true, "/api/pairing": true, "/api/pairing/qr": true,
	"/api/status": true, "/api/chats": true, "/api/search": true, "/api/context": true,
	"/api/pause": true, "/api/resume": true, "/api/disconnect": true,
	"/api/models": true, "/api/models/state": true, "/api/models/operations": true, "/api/models/operations/cancel": true,
	"/api/local-ai": true, "/api/local-ai/select": true,
	"/api/connections": true, "/api/connections/create": true, "/api/connections/guide": true, "/api/connections/revoke": true,
	"/api/original-budget": true,
	"/api/stats":           true, "/api/voice/retry": true, "/api/voice/recovery": true, "/api/documents/retry": true,
	"/api/connections/send-permission": true,
	// Data deletion, bootstrap/attach, provider/password configuration and reset
	// are deliberately absent. Adding an API never implicitly publishes it.
}
