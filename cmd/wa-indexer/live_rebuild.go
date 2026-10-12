package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/Bodokh/my-whatsapp-mcp/internal/app"
)

var errLiveRebuild = errors.New("live_rebuild_unavailable")

// Use the same no-follow, owner-only, bounded reader as other private launch
// settings. Neither identifiers nor the detailed private receipt reach stdout.
func executeLiveRebuild(ctx context.Context, path string, stdout io.Writer) error {
	data, err := readPrivateEnvFile(path)
	if err != nil {
		return errLiveRebuild
	}
	var cfg app.LiveRebuildBootstrapConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil {
		return errLiveRebuild
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errLiveRebuild
	}
	if _, err := app.BootstrapLiveRebuild(ctx, cfg); err != nil {
		return errLiveRebuild
	}
	return json.NewEncoder(stdout).Encode(struct {
		Mode              string `json:"mode"`
		Coverage          string `json:"coverage"`
		CurrentPhoneState string `json:"current_phone_state"`
		Connected         bool   `json:"connected"`
	}{"live_rebuild_prepared", "partial", "unknown", false})
}
