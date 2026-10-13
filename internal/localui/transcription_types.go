package localui

import "context"

// TranscriptionApplication is optional and accessible only to the native owner.
// It reports the running adapter, not installation intent or private settings.
type TranscriptionApplication interface {
	TranscriptionStatus(context.Context) (TranscriptionStatus, error)
}

type TranscriptionStatus struct {
	Configured bool   `json:"configured"`
	State      string `json:"state"`
	// In busy state, HebrewReady records prior adapter verification, not a
	// fresh check of the service identity. A ready state requires a fresh probe.
	HebrewReady bool `json:"hebrew_ready"`
}
