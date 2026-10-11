package localui

import (
	"context"
	"errors"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/modelstore"
)

var ErrModelOperationBusy = errors.New("model_operation_busy")

type ModelProfile struct {
	ID                string `json:"id"`
	Revision          string `json:"revision"`
	Selection         string `json:"selection"`
	GateStatus        string `json:"gate_status"`
	Platform          string `json:"platform"`
	License           string `json:"license"`
	Bytes             int64  `json:"bytes"`
	DownloadAvailable bool   `json:"download_available"`
}

// ModelApplication is an optional owner capability. The app owns operation
// admission, worker lifetime and cancellation; HTTP requests do not install
// models synchronously or receive private installed-component paths.
type ModelApplication interface {
	Models(context.Context) ([]modelstore.Profile, error)
	ModelState(context.Context) (ModelStateSnapshot, error)
	StartModelOperation(context.Context, ModelOperationRequest) (ModelOperation, error)
	CancelModelOperation(context.Context, string) error
}

type ModelStateSnapshot struct {
	Items []ModelState `json:"items"`
}

type ModelState struct {
	ProfileID         string          `json:"profile_id"`
	InstallationState string          `json:"installation_state"`
	Code              string          `json:"code"`
	VerifiedAt        *time.Time      `json:"verified_at,omitempty"`
	Operation         *ModelOperation `json:"operation,omitempty"`
}

type ModelOperationRequest struct {
	ProfileID string `json:"profile_id"`
	Kind      string `json:"kind"`
}

// Operation fields contain body-free progress only. No paths, download URLs,
// raw adapter errors, prompts, message contents or model credentials cross UI.
type ModelOperation struct {
	ID              string `json:"id"`
	ProfileID       string `json:"profile_id"`
	Kind            string `json:"kind"`
	State           string `json:"state"`
	Phase           string `json:"phase"`
	CompletedBytes  int64  `json:"completed_bytes"`
	TotalBytes      int64  `json:"total_bytes"`
	AdditionalBytes int64  `json:"additional_bytes"`
	Code            string `json:"code"`
}
