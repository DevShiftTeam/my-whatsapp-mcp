package localui

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/remoteaccess"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidateRemoteSubmission is shared by the local wire and app-owned dispatch.
// It admits no filesystem, executable, helper-version, or SDK configuration.
func ValidateRemoteSubmission(v RemoteAccessSubmission) error {
	i := v.Intent
	if i.Version != 1 || !i.Enabled || len(v.Secret) > 8192 || !utf8.Valid(v.Secret) {
		return remoteaccess.ErrInvalidIntent
	}
	for _, c := range string(v.Secret) {
		if unicode.IsControl(c) {
			return remoteaccess.ErrInvalidIntent
		}
	}
	if i.SavedOrigin != "" {
		if _, e := remoteaccess.PinOrigin("", i.SavedOrigin); e != nil {
			return remoteaccess.ErrInvalidIntent
		}
	}
	switch i.Provider {
	case remoteaccess.Ngrok, remoteaccess.Cloudflare:
		if i.SavedOrigin == "" || i.NodeName != "" {
			return remoteaccess.ErrInvalidIntent
		}
	case remoteaccess.TailscalePrivate, remoteaccess.TailscaleFunnel:
		if len(v.Secret) != 0 || len(i.NodeName) < 1 || len(i.NodeName) > 63 || strings.HasPrefix(i.NodeName, "-") || strings.HasSuffix(i.NodeName, "-") {
			return remoteaccess.ErrInvalidIntent
		}
		for _, c := range i.NodeName {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return remoteaccess.ErrInvalidIntent
			}
		}
	default:
		return remoteaccess.ErrInvalidIntent
	}
	return nil
}
func remoteAccessFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, remoteaccess.ErrBusy):
		fail(w, 409, "remote_operation_busy")
	case errors.Is(err, remoteaccess.ErrIdentityChanged):
		fail(w, 409, "remote_identity_changed")
	case errors.Is(err, ErrRemoteManual):
		fail(w, 409, "remote_manual_mode")
	case errors.Is(err, ErrRemotePasswordRequired):
		fail(w, 409, "remote_password_required")
	case errors.Is(err, remoteaccess.ErrInvalidIntent):
		fail(w, 400, "remote_invalid_intent")
	default:
		fail(w, 503, "remote_state_unavailable")
	}
}

var ErrRemoteManual = errors.New("remote_manual_mode")
var ErrRemotePasswordRequired = errors.New("remote_password_required")

func (s *Server) remoteAccess(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if s.realm != realmNative {
		fail(w, 404, "route_unavailable")
		return
	}
	app, ok := s.app.(RemoteAccessApplication)
	if !ok {
		fail(w, 503, "remote_state_unavailable")
		return
	}
	if r.URL.Path == "/api/remote/status" {
		v, e := app.RemoteAccessStatus(ctx)
		if e != nil {
			remoteAccessFailure(w, e)
			return
		}
		respond(w, v)
		return
	}
	// Exact keys, non-null values, bounded body, duplicate rejection. No Go
	// Intent decoding: adding fields to the internal type grants no wire power.
	var fields map[string]json.RawMessage
	if !decode(w, r, &fields) {
		return
	}
	defer func() {
		for _, b := range fields {
			clear(b)
		}
	}()
	var e error
	switch r.URL.Path {
	case "/api/remote/configure":
		var provider, origin, node, secret string
		browser := false
		for key, b := range fields {
			if string(b) == "null" {
				fail(w, 400, "invalid_request")
				return
			}
			var target any
			switch key {
			case "provider":
				target = &provider
			case "origin":
				target = &origin
			case "node_name":
				target = &node
			case "secret":
				target = &secret
			case "expose_browser":
				target = &browser
			default:
				fail(w, 400, "invalid_request")
				return
			}
			if json.Unmarshal(b, target) != nil {
				fail(w, 400, "invalid_request")
				return
			}
		}
		v := RemoteAccessSubmission{Intent: remoteaccess.Intent{Version: 1, Provider: remoteaccess.Provider(provider), Enabled: true, ExposeBrowser: browser, SavedOrigin: origin, NodeName: node}, Secret: remoteaccess.Secret(secret)}
		defer clear(v.Secret)
		if e = ValidateRemoteSubmission(v); e == nil {
			e = app.ConfigureRemoteAccess(ctx, v)
		}
	case "/api/remote/disable", "/api/remote/remove":
		var confirm string
		if len(fields) != 1 || json.Unmarshal(fields["confirm"], &confirm) != nil || confirm != strings.TrimPrefix(r.URL.Path, "/api/remote/") {
			fail(w, 400, "invalid_request")
			return
		}
		if confirm == "disable" {
			e = app.DisableRemoteAccess(ctx)
		} else {
			e = app.RemoveRemoteAccess(ctx)
		}
	case "/api/remote/sign-in":
		if len(fields) != 0 {
			fail(w, 400, "invalid_request")
			return
		}
		signin, ok := s.app.(RemoteAccessSignInApplication)
		if !ok {
			fail(w, 503, "remote_state_unavailable")
			return
		}
		var authURL string
		authURL, e = signin.RemoteAccessSignIn(ctx)
		if e == nil {
			u, err := url.Parse(authURL)
			if authURL != "" && (err != nil || u.Scheme != "https" || u.Host != "login.tailscale.com" || u.User != nil || u.Fragment != "") {
				fail(w, 503, "remote_state_unavailable")
				return
			}
			respond(w, struct {
				URL string `json:"url"`
			}{authURL})
			return
		}
	default:
		fail(w, 404, "route_unavailable")
		return
	}
	if e != nil {
		remoteAccessFailure(w, e)
		return
	}
	respond(w, struct {
		OK bool `json:"ok"`
	}{true})
}

// Provider state and password operations require explicit native owner authority.
func nativeOnlyRoute(path string) bool {
	switch path {
	case "/api/transcription/status", "/api/remote/password", "/api/remote/status", "/api/remote/configure", "/api/remote/disable", "/api/remote/remove", "/api/remote/sign-in":
		return true
	}
	return false
}
