package remoteaccess

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// PinOrigin accepts canonical HTTPS DNS authorities only. It does no network IO
// and never changes an already saved identity (including when reconnecting).
func PinOrigin(saved, candidate string) (string, error) {
	if !canonicalOrigin(candidate) || (saved != "" && !canonicalOrigin(saved)) {
		return "", ErrInvalidOrigin
	}
	if saved != "" && saved != candidate {
		return "", ErrIdentityChanged
	}
	return candidate, nil
}
func canonicalOrigin(origin string) bool {
	if len(origin) > 261 {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || origin != "https://"+u.Host {
		return false
	}
	host := u.Host
	if strings.ToLower(host) != host || strings.ContainsAny(host, ":%\\") || net.ParseIP(host) != nil || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	// Numeric final labels cover legacy dotted decimal/octal/hex inet aliases.
	last := labels[len(labels)-1]
	if _, err := strconv.ParseUint(last, 0, 64); err == nil {
		return false
	}
	numeric := true
	for _, c := range last {
		numeric = numeric && c >= '0' && c <= '9'
	}
	if numeric {
		return false
	}
	switch last {
	case "localhost", "local", "internal", "invalid", "test", "onion":
		return false
	}
	return true
}
func validateIntent(i Intent) error {
	if i.Version != 1 {
		return ErrInvalidIntent
	}
	switch i.Provider {
	case TailscalePrivate, TailscaleFunnel, Ngrok, Cloudflare:
	default:
		return ErrInvalidIntent
	}
	if i.Provider == Ngrok && (i.SavedOrigin == "" || i.NodeName != "") {
		return ErrInvalidIntent
	}
	if i.SavedOrigin != "" && !canonicalOrigin(i.SavedOrigin) {
		return ErrInvalidIntent
	}
	if len(i.NodeName) > 63 {
		return ErrInvalidIntent
	}
	if i.NodeName != "" {
		if i.NodeName[0] == '-' || i.NodeName[len(i.NodeName)-1] == '-' {
			return ErrInvalidIntent
		}
		for _, c := range i.NodeName {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return ErrInvalidIntent
			}
		}
	}
	return nil
}
