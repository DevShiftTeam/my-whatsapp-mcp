package localui

import (
	"context"
	"errors"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/clientsetup"
)

var (
	ErrConnectionUnavailable = errors.New("client_connections_unavailable")
	ErrConnectionRecovery    = errors.New("client_recovery_pending")
	ErrConnectionConflict    = errors.New("client_connection_changed")
	ErrConnectionUnsupported = errors.New("client_connection_unsupported")
	ErrConnectionNotFound    = errors.New("client_connection_not_found")
	ErrConnectionInvalid     = errors.New("client_connection_invalid")
)

// ConnectionApplication is an optional owner capability. The application binds
// the account, principal, listener and private config path, and publishes a
// connection only after its issuance journal and native grant are durable.
type ConnectionApplication interface {
	Connections(context.Context) (ConnectionSnapshot, error)
	CreateConnection(context.Context, ConnectionRequest) (Connection, error)
	ConnectionGuide(context.Context, ConnectionGuideRequest) (clientsetup.Guide, error)
	RevokeConnection(context.Context, ConnectionRevokeRequest) error
}

type ConnectionSnapshot struct {
	Items               []Connection `json:"items"`
	LocalState          string       `json:"local_state"`
	RemoteState         string       `json:"remote_state"`
	RecoveryState       string       `json:"recovery_state"`
	LocalRecoveryState  string       `json:"local_recovery_state"`
	RemoteRecoveryState string       `json:"remote_recovery_state"`
}

type Connection struct {
	ID        string                 `json:"id"`
	Client    string                 `json:"client"`
	Label     string                 `json:"label"`
	Mode      string                 `json:"mode"`
	Revision  uint64                 `json:"revision"`
	State     string                 `json:"state"`
	ExpiresAt *time.Time             `json:"expires_at,omitempty"`
	Scope     ConnectionScopeSummary `json:"scope"`
}

type ConnectionScopeSummary struct {
	AllChats  bool `json:"all_chats"`
	ChatCount int  `json:"chat_count"`
	// StartDate is inclusive; EndDate is an exclusive instant.
	StartDate *time.Time `json:"start_date,omitempty"`
	EndDate   *time.Time `json:"end_date,omitempty"`
}

type ConnectionRequest struct {
	Client    string           `json:"client"`
	Label     string           `json:"label"`
	Mode      string           `json:"mode"`
	AllChats  bool             `json:"all_chats"`
	Chats     []archive.ChatID `json:"chats"`
	StartDate *time.Time       `json:"start_date,omitempty"`
	EndDate   *time.Time       `json:"end_date,omitempty"`
	ExpiresAt *time.Time       `json:"expires_at"`
}

type ConnectionGuideRequest struct {
	ID     string `json:"id"`
	Client string `json:"client"`
	Mode   string `json:"mode"`
}

type ConnectionRevokeRequest struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
}

type ConnectionRevealRequest struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
}

// RemoteCredentialApplication is the explicit owner-only reveal capability.
// The app proves the durable remote record, protected file, current grant and
// configured route before returning this transient secret; it is never a tool.
type RemoteCredentialApplication interface {
	RevealRemoteCredential(context.Context, ConnectionRevealRequest) (RemoteCredentialSecret, error)
}

type RemoteCredentialSecret struct {
	ID        string    `json:"id"`
	Revision  uint64    `json:"revision"`
	PublicURL string    `json:"public_url"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (RemoteCredentialSecret) String() string   { return "localui.RemoteCredentialSecret{redacted}" }
func (RemoteCredentialSecret) GoString() string { return "localui.RemoteCredentialSecret{redacted}" }
