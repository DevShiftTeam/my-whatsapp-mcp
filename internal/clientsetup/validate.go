// Package clientsetup renders client-specific, credential-free setup guides.
// It does not authenticate, register clients, write settings, or grant access.
package clientsetup

import (
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalidConfig = errors.New("client_setup_invalid_configuration")
	ErrUnsupported   = errors.New("client_setup_unsupported_combination")
)

// Validate checks the selected mode without reading any client configuration.
// Unsupported client capabilities are explained by Render's returned Guide.
func Validate(client Client, mode Mode, cfg SetupConfig) error {
	_, err := normalizedConfig(client, mode, cfg)
	return err
}

func normalizedConfig(client Client, mode Mode, cfg SetupConfig) (SetupConfig, error) {
	if !knownClient(client) || (mode != Local && mode != Remote) || len(cfg.Token) > 512 {
		return SetupConfig{}, ErrInvalidConfig
	}
	fields := []string{cfg.ExecutablePath, cfg.LocalConfigPath, cfg.LocalCredentialID, cfg.RemoteURL, cfg.BearerTokenEnv, cfg.CredentialID, cfg.BearerURL, cfg.OAuthURL}
	if cfg.Docker != nil {
		value := *cfg.Docker
		cfg.Docker = &value
		fields = append(fields, value.Container)
	}
	for _, value := range fields {
		if !safeText(value, 4096) || (cfg.Token != "" && strings.Contains(value, cfg.Token)) {
			return SetupConfig{}, ErrInvalidConfig
		}
	}
	if mode == Local {
		if cfg.Auth != "" || cfg.RemoteURL != "" || cfg.BearerURL != "" || cfg.OAuthURL != "" || cfg.BearerTokenEnv != "" || cfg.CredentialID != "" || !validConfigPath(cfg.LocalConfigPath) || !validCredentialID(cfg.LocalCredentialID) {
			return SetupConfig{}, ErrInvalidConfig
		}
		if cfg.Docker != nil {
			if cfg.ExecutablePath != "" {
				return SetupConfig{}, ErrInvalidConfig
			}
			if _, _, err := DockerStdioCommand(cfg.Docker.Container, cfg.LocalConfigPath); err != nil {
				return SetupConfig{}, err
			}
		} else if !validPath(cfg.ExecutablePath) || filepath.Clean(cfg.ExecutablePath) == filepath.Clean(cfg.LocalConfigPath) {
			return SetupConfig{}, ErrInvalidConfig
		}
		return cfg, nil
	}
	if cfg.Docker != nil || cfg.ExecutablePath != "" || cfg.LocalConfigPath != "" || cfg.LocalCredentialID != "" || (cfg.Auth != Bearer && cfg.Auth != OAuth) {
		return SetupConfig{}, ErrInvalidConfig
	}
	endpoint, host, err := normalizeRemoteURL(cfg.RemoteURL)
	if err != nil {
		return SetupConfig{}, err
	}
	cfg.RemoteURL = endpoint
	var bearerHost, oauthHost string
	if cfg.BearerURL != "" {
		cfg.BearerURL, bearerHost, err = normalizeRemoteURL(cfg.BearerURL)
		if err != nil {
			return SetupConfig{}, err
		}
	}
	if cfg.OAuthURL != "" {
		cfg.OAuthURL, oauthHost, err = normalizeRemoteURL(cfg.OAuthURL)
		if err != nil {
			return SetupConfig{}, err
		}
	}
	// The two public modes require separate hostnames, not merely ports or paths.
	if bearerHost != "" && bearerHost == oauthHost {
		return SetupConfig{}, ErrInvalidConfig
	}
	if cfg.Auth == Bearer {
		if !validCredentialID(cfg.CredentialID) || (bearerHost != "" && host != bearerHost) || (oauthHost != "" && host == oauthHost) {
			return SetupConfig{}, ErrInvalidConfig
		}
		if cfg.BearerTokenEnv == "" {
			cfg.BearerTokenEnv = "WA_INDEXER_CLIENT_TOKEN"
		}
		if !validTokenEnv(cfg.BearerTokenEnv) {
			return SetupConfig{}, ErrInvalidConfig
		}
	} else if cfg.BearerTokenEnv != "" || cfg.Token != "" || (cfg.CredentialID != "" && !validCredentialID(cfg.CredentialID)) || (oauthHost != "" && host != oauthHost) || (bearerHost != "" && host == bearerHost) {
		return SetupConfig{}, ErrInvalidConfig
	}
	// Normalization and defaulting may introduce a previously absent token
	// match (for example an uppercase hostname normalized to lowercase).
	if containsConfigToken(cfg) {
		return SetupConfig{}, ErrInvalidConfig
	}
	return cfg, nil
}

func containsConfigToken(cfg SetupConfig) bool {
	if cfg.Token == "" {
		return false
	}
	for _, value := range []string{cfg.ExecutablePath, cfg.LocalConfigPath, cfg.LocalCredentialID, cfg.RemoteURL, cfg.BearerTokenEnv, cfg.CredentialID, cfg.BearerURL, cfg.OAuthURL, string(cfg.Auth)} {
		if strings.Contains(value, cfg.Token) {
			return true
		}
	}
	return false
}

func knownClient(client Client) bool {
	switch client {
	case CodexCLI, CodexDesktop, ClaudeCode, ClaudeConnectors, Composio:
		return true
	}
	return false
}

func safeText(value string, limit int) bool {
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validPath(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && filepath.IsAbs(value)
}

// Match the adapter CLI without rewriting traversal across possible symlinks.
func validConfigPath(value string) bool {
	return validPath(value) && filepath.Clean(value) == value
}

func validCredentialID(value string) bool {
	if len(value) != 32 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validTokenEnv(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for i, r := range value {
		if !(r == '_' || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')) {
			return false
		}
	}
	switch value {
	case "HOME", "PATH", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_API_KEY", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AZURE_OPENAI_API_KEY", "GOOGLE_API_KEY", "GEMINI_API_KEY", "AWS_BEARER_TOKEN_BEDROCK", "HTTPS_PROXY", "NPM_TOKEN":
		return false
	}
	return true
}

// NormalizeRemoteURL returns the canonical public HTTPS /mcp route for trusted
// server configuration. It validates syntax only: it never resolves DNS, dials
// the endpoint or proves deployment ownership. Invalid inputs return the fixed
// ErrInvalidConfig without including any supplied URL or credential values.
func NormalizeRemoteURL(value string) (string, error) {
	endpoint, _, err := normalizeRemoteURL(value)
	return endpoint, err
}

func normalizeRemoteURL(value string) (endpoint, host string, err error) {
	if !safeText(value, 4096) {
		return "", "", ErrInvalidConfig
	}
	u, parseErr := url.Parse(value)
	if parseErr != nil || u == nil || strings.TrimSpace(value) != value || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") || u.Fragment != "" || u.RawFragment != "" || u.Host == "" || u.EscapedPath() != "/mcp" {
		return "", "", ErrInvalidConfig
	}
	host = strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if !publicHost(host) || strings.HasSuffix(u.Host, ":") {
		return "", "", ErrInvalidConfig
	}
	port := u.Port()
	if port != "" {
		n, parseErr := strconv.Atoi(port)
		if parseErr != nil || n < 1 || n > 65535 {
			return "", "", ErrInvalidConfig
		}
		port = strconv.Itoa(n)
	}
	if port == "443" {
		port = ""
	}
	u.Host = host
	if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	return u.String(), host, nil
}

func publicHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()
	}
	if len(host) > 253 || !strings.Contains(host, ".") || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return false
	}
	// WHATWG clients interpret hosts ending in a number as IPv4, including
	// abbreviated/octal/hex forms that net.ParseIP rejects. Only canonical
	// literals may reach the public-IP branch above; never treat aliases as DNS.
	if numericHostLabel(host[strings.LastIndex(host, ".")+1:]) {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				return false
			}
		}
	}
	return true
}

// numericHostLabel implements the relevant WHATWG ends-in-a-number check.
// Hosts have already been lowercased. A bare 0x also parses as numeric zero.
func numericHostLabel(label string) bool {
	if label == "" {
		return false
	}
	if strings.HasPrefix(label, "0x") {
		for _, r := range label[2:] {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				return false
			}
		}
		return true
	}
	for _, r := range label {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
