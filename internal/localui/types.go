// Package localui serves the embedded owner interface on a protected loopback listener.
package localui

import (
	"context"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
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
	Configured bool                  `json:"configured"`
	Policy     archive.ArchivePolicy `json:"policy"`
	Demo       bool                  `json:"demo"`
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
	// Origin is the exact HTTP origin of the already-bound literal loopback listener.
	Origin string
	// Demo is the composition layer's validated application mode, not browser input.
	Demo         bool
	SessionTTL   time.Duration
	BootstrapTTL time.Duration
}

const DeleteConfirmation = "DELETE LOCAL DATA"
