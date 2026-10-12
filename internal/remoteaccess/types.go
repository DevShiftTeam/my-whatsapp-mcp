// Package remoteaccess owns persistent remote-provider intent and ingress.
// Application authorization and credentials remain the MCP server's concern.
package remoteaccess

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

type Provider string

const (
	TailscalePrivate Provider = "tailscale_private"
	TailscaleFunnel  Provider = "tailscale_funnel"
	Ngrok            Provider = "ngrok"
	Cloudflare       Provider = "cloudflare"
)

type Intent struct {
	Version                int
	Provider               Provider
	Enabled, ExposeBrowser bool
	SavedOrigin, NodeName  string
}
type Secret []byte

func (Secret) String() string                { return "[redacted]" }
func (Secret) GoString() string              { return "[redacted]" }
func (Secret) Format(s fmt.State, verb rune) { _, _ = s.Write([]byte("[redacted]")) }
func (Secret) MarshalJSON() ([]byte, error)  { return []byte("null"), nil }

type Snapshot struct {
	Provider                 Provider
	State, Code, SavedOrigin string
	BrowserEnabled, MCPReady bool
}
type Notice struct{ State, Code, AuthURL string }

// An AuthURL is an owner-only capability. Ordinary formatting/JSON is redacted.
func (n Notice) String() string               { return "[remote notice]" }
func (n Notice) GoString() string             { return n.String() }
func (n Notice) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

type Session interface {
	Origin() string
	Listener() net.Listener
	Done() <-chan error
	// Close must be idempotent: failed cleanup is retried before replacement.
	Close(context.Context) error
}

// Open and Close must honor cancellation/deadlines. Open transfers ownership of
// any returned Session even when it also returns an error. Adapters never Serve.
type Adapter interface {
	Open(context.Context, Intent, Secret, func(Notice)) (Session, error)
}
type Store interface {
	Load(context.Context) (Intent, Secret, error)
	Save(context.Context, Intent, Secret) error
	Remove(context.Context) error
}
type Handlers struct {
	MCP   http.Handler
	Owner http.Handler
	// Release must be idempotent and clear only its captured generation.
	Release func(context.Context) error
}
type ManagerConfig struct {
	Store    Store
	Adapters map[Provider]Adapter
	Prepare  func(context.Context, Intent, string, uint64) (Handlers, error)
	MCPReady func() bool
}

var (
	ErrInvalidOrigin       = errors.New("remote_invalid_origin")
	ErrIdentityChanged     = errors.New("remote_identity_changed")
	ErrInvalidIntent       = errors.New("remote_invalid_intent")
	ErrState               = errors.New("remote_state_unavailable")
	ErrClosed              = errors.New("remote_closed")
	ErrBusy                = errors.New("remote_operation_busy")
	ErrPermissionRequired  = errors.New("remote_permission_required")
	ErrCredentialsRejected = errors.New("remote_credentials_rejected")
	ErrServiceLimit        = errors.New("remote_service_limit_reached")
	ErrTransient           = errors.New("remote_connection_unavailable")
	ErrPrepare             = errors.New("remote_prepare_failed")
)
