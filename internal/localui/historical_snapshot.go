package localui

import (
	"bytes"
	"html"
	"net/http"
	"time"
)

type historicalSnapshotApplication interface{ HistoricalSnapshotTime() time.Time }

func historicalSnapshotTime(app Application) time.Time {
	if value, ok := app.(historicalSnapshotApplication); ok {
		return value.HistoricalSnapshotTime()
	}
	return time.Time{}
}
func historicalSnapshotRead(method, path string) bool {
	if method == http.MethodGet {
		switch path {
		case "/api/session", "/api/setup", "/api/status", "/api/chats", "/api/models", "/api/models/state", "/api/local-ai", "/api/connections", "/api/original-budget":
			return true
		}
	}
	if method == http.MethodPost {
		switch path {
		case "/api/search", "/api/context", "/api/stats", "/api/archive/browse":
			return true
		}
	}
	return false
}
func historicalSnapshotHTML(data []byte, app Application) []byte {
	at := historicalSnapshotTime(app)
	if at.IsZero() {
		return data
	}
	data = bytes.Replace(data, []byte("<html "), []byte(`<html data-historical-snapshot="true" `), 1)
	banner := `<aside id="historical-snapshot-banner" role="note">Historical snapshot — captured ` + html.EscapeString(at.UTC().Format(time.RFC3339)) + `. Read-only. Newer messages excluded. Sending and syncing disabled.</aside>`
	// Outside both login and workspace so authentication never hides the warning.
	return bytes.Replace(data, []byte("<body>"), []byte("<body>"+banner), 1)
}
