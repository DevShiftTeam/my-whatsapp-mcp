package archive

import (
	"context"
	"encoding/json"
	"errors"
	"os"
)

// Alias failures expose only fixed operational values, never source identities
// or error text. The lookup and write paths have different bounded contexts.
func aliasFailureDiagnostic(ctx context.Context, stage string, err error) []byte {
	switch stage {
	case "alias_lookup", "alias_write", "alias_writer_result":
	default:
		stage = "unknown"
	}
	code := "alias_operation_failed"
	if ctx != nil {
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			code = "alias_parent_canceled"
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			code = "alias_parent_deadline"
		}
	}
	class := "unknown"
	switch {
	case err == nil:
		class = "none"
	case errors.Is(err, context.Canceled):
		class = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		class = "deadline"
	case errors.Is(err, ErrReconciliationRequired):
		class = "reconciliation_required"
	case errors.Is(err, ErrQueueLimitExceeded):
		class = "queue_limit_exceeded"
	case errors.Is(err, ErrArchiveUnavailable):
		class = "archive_unavailable"
	case errors.Is(err, ErrIdentityAliasConflict):
		class = "identity_alias_conflict"
	case errors.Is(err, ErrUnauthorized):
		class = "unauthorized"
	case errors.Is(err, ErrPolicyChanged):
		class = "policy_changed"
	case errors.Is(err, ErrInvalidEvent):
		class = "invalid_event"
	}
	encoded, _ := json.Marshal(struct {
		Event string `json:"event"`
		Stage string `json:"stage"`
		Code  string `json:"code"`
		Class string `json:"class"`
	}{"archive_failure", stage, code, class})
	return append(encoded, '\n')
}

func recordAliasFailure(ctx context.Context, stage string, err error) {
	if err != nil {
		_, _ = os.Stderr.Write(aliasFailureDiagnostic(ctx, stage, err))
	}
}
