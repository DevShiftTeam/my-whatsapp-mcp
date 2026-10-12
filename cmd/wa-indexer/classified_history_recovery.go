package main

import (
	"context"
	"encoding/json"
	"github.com/Bodokh/my-whatsapp-mcp/internal/app"
	"io"
)

func executeClassifiedHistoryRecovery(ctx context.Context, path string, stdout io.Writer) error {
	return executeHistoryRecovery(ctx, path, stdout, false)
}
func executeAcceptedPartialHistoryRecovery(ctx context.Context, path string, stdout io.Writer) error {
	return executeHistoryRecovery(ctx, path, stdout, true)
}
func executeHistoryRecovery(ctx context.Context, path string, stdout io.Writer, acceptUnknown bool) error {
	raw, err := readPrivateEnvFile(path)
	if err != nil {
		return app.ErrClassifiedHistoryRecovery
	}
	cfg, err := app.ParseClassifiedHistoryRecoveryConfig(raw)
	if err != nil {
		return app.ErrClassifiedHistoryRecovery
	}
	recoverSnapshot := app.RecoverClassifiedHistorySnapshot
	if acceptUnknown {
		recoverSnapshot = app.RecoverAcceptedPartialHistorySnapshot
	}
	receipt, err := recoverSnapshot(ctx, cfg)
	if err != nil {
		return app.ErrClassifiedHistoryRecovery
	}
	return json.NewEncoder(stdout).Encode(struct {
		Mode                      string `json:"mode"`
		Code                      string `json:"code"`
		Coverage                  string `json:"coverage"`
		CurrentPhoneStateVerified bool   `json:"current_phone_state_verified"`
		RecoveryEpoch             uint64 `json:"recovery_epoch"`
	}{receipt.Mode, receipt.Code, "partial", false, receipt.RecoveryEpoch})
}
