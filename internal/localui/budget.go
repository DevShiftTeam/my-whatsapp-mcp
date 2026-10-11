package localui

import (
	"context"
	"errors"
	"net/http"
)

const minimumOriginalBudget int64 = 64 << 20
const maximumOriginalBudget int64 = 1 << 40

func validOriginalBudget(value OriginalBudgetSnapshot) bool {
	return value.MaximumBytes >= minimumOriginalBudget && value.MaximumBytes <= maximumOriginalBudget && value.ChargedBytes >= 0 && value.Revision > 0 && value.Pressure == (value.ChargedBytes >= value.MaximumBytes)
}

func (s *Server) originalBudget(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	app, ok := s.app.(OriginalBudgetApplication)
	if !ok {
		fail(w, 503, "original_budget_unavailable")
		return
	}
	var value OriginalBudgetSnapshot
	var err error
	if r.Method == http.MethodGet {
		value, err = app.OriginalBudget(ctx)
	} else {
		var input OriginalBudgetRequest
		if !decode(w, r, &input) {
			return
		}
		if input.MaximumBytes < minimumOriginalBudget || input.MaximumBytes > maximumOriginalBudget || input.ExpectedRevision == 0 {
			fail(w, 400, "invalid_request")
			return
		}
		value, err = app.SetOriginalBudget(ctx, input)
	}
	if err != nil {
		if errors.Is(err, ErrOriginalBudgetConflict) {
			fail(w, 409, "original_budget_changed")
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fail(w, 503, "request_interrupted")
		} else {
			fail(w, 503, "original_budget_unavailable")
		}
		return
	}
	if !validOriginalBudget(value) {
		fail(w, 503, "original_budget_unavailable")
		return
	}
	respond(w, value)
}
