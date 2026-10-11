// Package mcpserver exposes a bounded, read-only view of an existing archive.
// It never owns a WhatsApp connection or starts a model runtime.
package mcpserver

import (
	"context"
	"errors"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
)

type AuthMode string

const (
	LocalAdapter AuthMode = "local_adapter"
	Bearer       AuthMode = "bearer"
	Access       AuthMode = "access"
)

// Credentials remains a narrow typed persistence port, with no SQL escape hatch.
type Credentials = archive.CredentialStore

// NativeClientBinding identifies a durably published native-client handoff.
// These private authority values never come from MCP request parameters.
type NativeClientBinding struct {
	Account, Principal string
	CredentialID       archive.CredentialID
}

// NativeClientRegistry is a trusted app-owned bounded in-memory lookup. It must
// perform no filesystem, database or network work. Publication follows durable
// ready-state persistence; removal follows durable credential revocation.
type NativeClientRegistry interface {
	PublishedNativeClient(NativeClientBinding) bool
}

// RemoteClientBinding identifies a durably published remote bearer handoff.
// The application owns this authority; a request cannot select its namespace.
type RemoteClientBinding struct {
	Account, Principal string
	CredentialID       archive.CredentialID
}

// RemoteClientRegistry follows the same bounded, in-memory publication contract
// as NativeClientRegistry. Pending or unjournaled reserved grants must deny.
type RemoteClientRegistry interface {
	PublishedRemoteClient(RemoteClientBinding) bool
}

type Config struct {
	Mode AuthMode
	// Required for LocalAdapter; checked on every fresh credential resolution.
	NativeClients NativeClientRegistry
	// Required on each fresh resolve of a reserved remote bearer principal.
	RemoteClients RemoteClientRegistry
	// Optional compatibility pin. It cannot replace the published native registry.
	// Remote modes also reject this ID when configured; native principal isolation
	// applies independently of this field and rejects orphan/unpublished grants.
	LocalCredentialID archive.CredentialID
	// Hosts match r.Host exactly, including the configured port.
	Hosts []string
	// Origins match exactly. Requests without Origin (native clients) are allowed.
	Origins          []string
	MaxRequestBytes  int64
	MaxResponseBytes int
	MaxConcurrent    int
	RequestTimeout   time.Duration
	Access           AccessConfig
}

// AccessConfig pins one issuer and audience to explicit subject/client grants.
// Identities are issuer-authenticated subject values, never caller email headers.
type AccessConfig struct {
	Issuer, Audience, JWKSURL string
	Identities                map[string]archive.CredentialID
}

type accessVerifier interface {
	Verify(context.Context, string) (archive.CredentialID, error)
}

var (
	ErrConfiguration     = errors.New("mcp_configuration_invalid")
	ErrUnauthorized      = errors.New("mcp_unauthorized")
	ErrUnavailable       = errors.New("mcp_unavailable")
	ErrResponseTooLarge  = errors.New("mcp_response_too_large")
	ErrCredentialFile    = errors.New("mcp_credential_file_unavailable")
	ErrCredentialCleanup = errors.New("mcp_credential_cleanup_required")
)

const (
	localAdapterHeader         = "X-WA-Local-Adapter"
	accessHeader               = "Cf-Access-Jwt-Assertion"
	defaultRequestBytes  int64 = 64 << 10
	defaultResponseBytes       = 256 << 10
)
