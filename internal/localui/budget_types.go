package localui

import (
	"context"
	"errors"
)

var ErrOriginalBudgetConflict = errors.New("original_budget_changed")
var ErrOriginalBudgetUnavailable = errors.New("original_budget_unavailable")

// OriginalBudgetApplication exposes the archive's sole persisted original-file
// budget. ChargedBytes includes outstanding reservations as well as originals.
// Updating a limit does not itself delete files or revoke live worker guards.
type OriginalBudgetApplication interface {
	OriginalBudget(context.Context) (OriginalBudgetSnapshot, error)
	SetOriginalBudget(context.Context, OriginalBudgetRequest) (OriginalBudgetSnapshot, error)
}

type OriginalBudgetSnapshot struct {
	MaximumBytes int64  `json:"maximum_bytes"`
	ChargedBytes int64  `json:"charged_bytes"`
	Revision     uint64 `json:"revision"`
	Pressure     bool   `json:"pressure"`
}

type OriginalBudgetRequest struct {
	MaximumBytes     int64  `json:"maximum_bytes"`
	ExpectedRevision uint64 `json:"expected_revision"`
}
