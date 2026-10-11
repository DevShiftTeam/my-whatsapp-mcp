package localui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/clientsetup"
)

func validConnectionClient(client string) bool {
	return oneOf(client, string(clientsetup.CodexCLI), string(clientsetup.CodexDesktop), string(clientsetup.ClaudeCode), string(clientsetup.ClaudeConnectors), string(clientsetup.Composio))
}

func validConnectionLabel(label string) bool {
	if len(label) == 0 || len(label) > 128 || !utf8.ValidString(label) || strings.TrimSpace(label) != label {
		return false
	}
	for _, char := range label {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func validConnectionTime(value *time.Time) bool {
	return value == nil || !value.IsZero() && value.Year() >= 1970 && value.Year() <= 9999
}

func validConnectionRequest(input ConnectionRequest) bool {
	if !validConnectionClient(input.Client) || !oneOf(input.Mode, "local", "remote") || !validConnectionLabel(input.Label) || input.ExpiresAt == nil || !validConnectionTime(input.ExpiresAt) || !input.ExpiresAt.After(time.Now()) || !validConnectionTime(input.StartDate) || !validConnectionTime(input.EndDate) {
		return false
	}
	if input.StartDate != nil && input.EndDate != nil && !input.StartDate.Before(*input.EndDate) {
		return false
	}
	if input.AllChats {
		return len(input.Chats) == 0
	}
	if len(input.Chats) == 0 || len(input.Chats) > 256 {
		return false
	}
	seen := make(map[string]bool, len(input.Chats))
	for _, id := range input.Chats {
		if !validOpaque(string(id)) || seen[string(id)] {
			return false
		}
		seen[string(id)] = true
	}
	return true
}

func (s *Server) connections(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	app, ok := s.app.(ConnectionApplication)
	if !ok {
		fail(w, 503, "client_connections_unavailable")
		return
	}
	switch r.URL.Path {
	case "/api/connections":
		value, err := app.Connections(ctx)
		if err != nil {
			connectionError(w, err)
			return
		}
		public := ConnectionSnapshot{Items: make([]Connection, 0, len(value.Items)), LocalState: publicTransportState(value.LocalState), RemoteState: publicTransportState(value.RemoteState), RecoveryState: publicRecoveryState(value.RecoveryState)}
		public.LocalRecoveryState = publicFamilyRecoveryState(value.LocalRecoveryState, public.RecoveryState)
		public.RemoteRecoveryState = publicFamilyRecoveryState(value.RemoteRecoveryState, public.RecoveryState)
		for _, item := range value.Items {
			if item, ok := publicConnection(item); ok {
				public.Items = append(public.Items, item)
			}
		}
		respond(w, public)
	case "/api/connections/create":
		var wire struct {
			Client    string           `json:"client"`
			Label     string           `json:"label"`
			Mode      string           `json:"mode"`
			AllChats  *bool            `json:"all_chats"`
			Chats     []archive.ChatID `json:"chats"`
			StartDate *time.Time       `json:"start_date,omitempty"`
			EndDate   *time.Time       `json:"end_date,omitempty"`
			ExpiresAt *time.Time       `json:"expires_at"`
		}
		if !decode(w, r, &wire) {
			return
		}
		if wire.AllChats == nil {
			fail(w, 400, "invalid_request")
			return
		}
		input := ConnectionRequest{Client: wire.Client, Label: wire.Label, Mode: wire.Mode, AllChats: *wire.AllChats, Chats: wire.Chats, StartDate: wire.StartDate, EndDate: wire.EndDate, ExpiresAt: wire.ExpiresAt}
		if !validConnectionRequest(input) {
			fail(w, 400, "invalid_request")
			return
		}
		value, err := app.CreateConnection(ctx, input)
		if err != nil {
			connectionError(w, err)
			return
		}
		public, ok := publicConnection(value)
		if !ok || public.Client != input.Client || public.Mode != input.Mode || public.State != "ready" || public.Revision == 0 || !connectionWithinRequest(public, input) {
			fail(w, 503, "client_connections_unavailable")
			return
		}
		respond(w, public)
	case "/api/connections/guide":
		var input ConnectionGuideRequest
		if !decode(w, r, &input) {
			return
		}
		if !validOpaque(input.ID) || !validConnectionClient(input.Client) || !oneOf(input.Mode, "local", "remote") {
			fail(w, 400, "invalid_request")
			return
		}
		guide, err := app.ConnectionGuide(ctx, input)
		if err != nil && !errors.Is(err, clientsetup.ErrUnsupported) && !errors.Is(err, ErrConnectionUnsupported) {
			connectionError(w, err)
			return
		}
		if !validConnectionGuide(guide, input) || err != nil && guide.Status != clientsetup.Unsupported {
			fail(w, 503, "client_connections_unavailable")
			return
		}
		respond(w, guide)
	case "/api/connections/revoke":
		var input ConnectionRevokeRequest
		if !decode(w, r, &input) {
			return
		}
		if !validOpaque(input.ID) || input.Revision == 0 {
			fail(w, 400, "invalid_request")
			return
		}
		if err := app.RevokeConnection(ctx, input); err != nil {
			connectionError(w, err)
			return
		}
		respond(w, map[string]bool{"ok": true})
	}
}

func publicTransportState(state string) string {
	if oneOf(state, "ready", "not_configured", "unavailable") {
		return state
	}
	return "unavailable"
}

func publicRecoveryState(state string) string {
	if oneOf(state, "ready", "recovery_pending", "invalid") {
		return state
	}
	return "recovery_pending"
}

func publicFamilyRecoveryState(state, legacy string) string {
	if state == "" {
		return legacy
	}
	return publicRecoveryState(state)
}

func connectionWithinRequest(value Connection, input ConnectionRequest) bool {
	if value.ExpiresAt == nil || input.ExpiresAt == nil || !value.ExpiresAt.After(time.Now()) || value.ExpiresAt.After(*input.ExpiresAt) {
		return false
	}
	if !input.AllChats && (value.Scope.AllChats || value.Scope.ChatCount > len(input.Chats)) {
		return false
	}
	if input.StartDate != nil && (value.Scope.StartDate == nil || value.Scope.StartDate.Before(*input.StartDate)) {
		return false
	}
	if input.EndDate != nil && (value.Scope.EndDate == nil || value.Scope.EndDate.After(*input.EndDate)) {
		return false
	}
	return true
}

func publicConnection(value Connection) (Connection, bool) {
	if !validOpaque(value.ID) {
		return Connection{}, false
	}
	if !validConnectionClient(value.Client) {
		value.Client = "unknown"
	}
	if !validConnectionLabel(value.Label) {
		value.Label = "Client connection"
	}
	if !oneOf(value.Mode, "local", "remote") {
		value.Mode = ""
	}
	if !oneOf(value.State, "ready", "revoked", "expired", "recovery_pending") {
		value.State = "recovery_pending"
	}
	if !validConnectionTime(value.ExpiresAt) {
		value.ExpiresAt = nil
	}
	if !validConnectionTime(value.Scope.StartDate) {
		value.Scope.StartDate = nil
	}
	if !validConnectionTime(value.Scope.EndDate) {
		value.Scope.EndDate = nil
	}
	if value.Scope.ChatCount < 0 {
		value.Scope.ChatCount = 0
	}
	return value, true
}

// Guides are inert server-rendered instructions. Only this response may carry
// validated local paths; no issuance token is part of the connection interface.
func validConnectionGuide(guide clientsetup.Guide, input ConnectionGuideRequest) bool {
	if string(guide.Client) != input.Client || string(guide.Mode) != input.Mode || guide.Status != clientsetup.NotYetVerified && guide.Status != clientsetup.Unsupported || input.Mode == "local" && guide.Endpoint != "" {
		return false
	}
	if !oneOf(guide.ConfigurationFormat, "", "json", "toml", "connector") || len(guide.Warnings) > 4 || len(guide.Sources) > 2 {
		return false
	}
	fields := []string{guide.SupportLabel, guide.Explanation, guide.Endpoint, guide.Configuration, guide.Command, guide.CredentialPlacement, guide.TestAction, guide.RevokeAction, guide.DocumentationDate, guide.ObservedVersion}
	fields = append(fields, guide.Warnings...)
	fields = append(fields, guide.Sources...)
	total := 0
	for _, field := range fields {
		total += len(field)
		if len(field) > 65536 || total > 131072 || !utf8.ValidString(field) || strings.ContainsRune(field, 0) {
			return false
		}
	}
	if guide.Status == clientsetup.Unsupported && (guide.Command != "" || guide.Configuration != "") {
		return false
	}
	encoded, err := json.Marshal(guide)
	if err != nil || len(encoded) > 256*1024 {
		return false
	}
	return true
}

func connectionError(w http.ResponseWriter, err error) {
	for _, mapping := range []struct {
		err    error
		status int
		code   string
	}{
		{ErrConnectionRecovery, 409, "client_recovery_pending"}, {ErrConnectionConflict, 409, "client_connection_changed"}, {ErrConnectionUnsupported, 409, "client_connection_unsupported"}, {clientsetup.ErrUnsupported, 409, "client_connection_unsupported"},
		{ErrConnectionNotFound, 404, "client_connection_not_found"}, {ErrConnectionInvalid, 400, "client_connection_invalid"},
	} {
		if errors.Is(err, mapping.err) {
			fail(w, mapping.status, mapping.code)
			return
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		fail(w, 503, "request_interrupted")
		return
	}
	fail(w, 503, "client_connections_unavailable")
}
