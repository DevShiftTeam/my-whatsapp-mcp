// Package localui serves the embedded owner interface on a protected loopback listener.
package localui

import (
	"context"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/remoteaccess"
)

// Application keeps account scope, connector credentials and owned paths server-side.
// Chats is an owner-only metadata discovery view, including unselected chats.
type Application interface {
	Setup(context.Context) (SetupState, error)
	SetPolicy(context.Context, archive.ArchivePolicy, archive.Generation) (archive.ArchivePolicy, error)
	StartPairing(context.Context) error
	PairingState(context.Context) (PairingState, error)
	QRPNG(context.Context) ([]byte, error)
	Pause(context.Context) error
	Resume(context.Context) error
	Disconnect(context.Context) error
	DeleteLocalData(context.Context, bool) error
	Search(context.Context, archive.SearchRequest) (archive.SearchPage, error)
	Context(context.Context, archive.ContextRequest) (archive.ContextPage, error)
	Chats(context.Context, archive.ChatListRequest) (archive.DiscoveryPage, error)
	Status(context.Context) (archive.StatusSnapshot, error)
}

type SetupState struct {
	HistoricalSnapshot *archive.HistoricalSnapshot `json:"historical_snapshot,omitempty"`
	Configured         bool                        `json:"configured"`
	Policy             archive.ArchivePolicy       `json:"policy"`
	Demo               bool                        `json:"demo"`
}

// PairingState contains no QR contents, raw source IDs or local filenames.
type PairingState struct {
	State                string                       `json:"state"`
	QRAvailable          bool                         `json:"qr_available"`
	ExpiresAt            time.Time                    `json:"expires_at"`
	Code                 string                       `json:"code,omitempty"`
	HasSession           bool                         `json:"has_session"`
	ConnectionDiagnostic archive.ConnectionDiagnostic `json:"connection_diagnostic"`
}

type Config struct {
	// Origin is the exact canonical HTTP loopback browser origin. In native
	// mode it is computed from the already-bound literal loopback listener.
	Origin string
	// ContainerListen explicitly authorizes the canonical internal container listener.
	// Origin remains the exact trusted literal loopback browser origin.
	ContainerListen string
	// Demo is the composition layer's validated application mode, not browser input.
	Demo         bool
	SessionTTL   time.Duration
	BootstrapTTL time.Duration
}

const DeleteConfirmation = "DELETE LOCAL DATA"

// RemoteAccessApplication is available only to the authenticated local owner.
// Provider credentials never enter the public snapshot or sign-in response.
type RemoteAccessApplication interface {
	RemoteAccessStatus(context.Context) (RemoteAccessSnapshot, error)
	ConfigureRemoteAccess(context.Context, RemoteAccessSubmission) error
	DisableRemoteAccess(context.Context) error
	RemoveRemoteAccess(context.Context) error
	SetRemoteOwnerPassword(context.Context, []byte) error
}
type RemoteAccessSignInApplication interface {
	RemoteAccessSignIn(context.Context) (string, error)
}
type RemoteAccessSubmission struct {
	Intent remoteaccess.Intent
	Secret remoteaccess.Secret
}
type RemoteAccessSnapshot struct {
	Provider           remoteaccess.Provider `json:"provider"`
	State              string                `json:"state"`
	Code               string                `json:"code"`
	SavedOrigin        string                `json:"saved_origin"`
	NodeName           string                `json:"node_name"`
	BrowserEnabled     bool                  `json:"browser_enabled"`
	MCPReady           bool                  `json:"mcp_ready"`
	AuthorizationReady bool                  `json:"authorization_ready"`
	PasswordConfigured bool                  `json:"password_configured"`
	Manual             bool                  `json:"manual"`
}
