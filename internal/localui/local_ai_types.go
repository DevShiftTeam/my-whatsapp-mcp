package localui

import (
	"context"
	"errors"
)

var (
	ErrLocalAIConflict    = errors.New("local_ai_conflict")
	ErrLocalAIUnavailable = errors.New("local_ai_unavailable")
	ErrLocalAIInvalid     = errors.New("local_ai_invalid")
)

type LocalAIChoice struct {
	Mode           string `json:"mode"`
	EncoderProfile string `json:"encoder_profile,omitempty"`
	OCRProfile     string `json:"ocr_profile,omitempty"`
	Caption        bool   `json:"caption"`
	SpeechLanguage string `json:"speech_language,omitempty"`
}

type LocalAIUpdate struct {
	Choice           LocalAIChoice `json:"choice"`
	ExpectedRevision uint64        `json:"expected_revision"`
}

type LocalAIOption struct {
	Kind           string `json:"kind"`
	ProfileID      string `json:"profile_id"`
	ModelProfileID string `json:"model_profile_id"`
	State          string `json:"state"`
	Code           string `json:"code"`
}

type LocalAISnapshot struct {
	Revision uint64          `json:"revision"`
	Choice   LocalAIChoice   `json:"choice"`
	State    string          `json:"state"`
	Code     string          `json:"code"`
	Options  []LocalAIOption `json:"options"`
}

// LocalAIApplication owns the persisted desired choice, CAS and joined native
// transition. HTTP only selects trusted identifiers; install does not activate.
type LocalAIApplication interface {
	LocalAIState(context.Context) (LocalAISnapshot, error)
	SetLocalAI(context.Context, LocalAIUpdate) (LocalAISnapshot, error)
}
