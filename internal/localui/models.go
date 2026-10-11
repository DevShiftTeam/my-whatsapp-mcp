package localui

import (
	"context"
	"errors"
	"net/http"

	"github.com/Bodokh/my-whatsapp-mcp/internal/modelstore"
)

func validProfileID(id string) bool {
	if len(id) == 0 || len(id) > 100 || id == "." || id == ".." {
		return false
	}
	for index, char := range id {
		alphanumeric := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
		if !alphanumeric && (index == 0 || char != '.' && char != '_' && char != '-') {
			return false
		}
	}
	return true
}

func (s *Server) models(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	app, ok := s.app.(ModelApplication)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "model_management_unavailable")
		return
	}
	switch r.URL.Path {
	case "/api/models":
		profiles, err := app.Models(ctx)
		if err != nil {
			modelError(w, err)
			return
		}
		public := make([]ModelProfile, 0, len(profiles))
		for _, profile := range profiles {
			var bytes int64
			download := len(profile.Components) > 0
			for _, component := range profile.Components {
				bytes += component.Bytes
				download = download && component.URL != ""
			}
			public = append(public, ModelProfile{ID: profile.ID, Revision: profile.Revision, Selection: profile.Selection, GateStatus: profile.GateStatus,
				Platform: profile.Platform, License: profile.License, Bytes: bytes, DownloadAvailable: download})
		}
		respond(w, struct {
			Profiles []ModelProfile `json:"profiles"`
		}{public})
	case "/api/models/state":
		state, err := app.ModelState(ctx)
		if err != nil {
			modelError(w, err)
			return
		}
		public := ModelStateSnapshot{Items: make([]ModelState, 0, len(state.Items))}
		for _, item := range state.Items {
			public.Items = append(public.Items, publicModelState(item))
		}
		respond(w, public)
	case "/api/models/operations":
		var input ModelOperationRequest
		if !decode(w, r, &input) {
			return
		}
		if !validProfileID(input.ProfileID) || input.Kind != "install" && input.Kind != "verify" && input.Kind != "remove" {
			fail(w, 400, "invalid_request")
			return
		}
		operation, err := app.StartModelOperation(ctx, input)
		if err != nil {
			modelError(w, err)
			return
		}
		respond(w, publicModelOperation(operation))
	case "/api/models/operations/cancel":
		var input struct {
			OperationID string `json:"operation_id"`
		}
		if !decode(w, r, &input) {
			return
		}
		if !validOpaque(input.OperationID) {
			fail(w, 400, "invalid_request")
			return
		}
		if err := app.CancelModelOperation(ctx, input.OperationID); err != nil {
			modelError(w, err)
			return
		}
		respond(w, map[string]bool{"ok": true})
	}
}

func publicModelState(state ModelState) ModelState {
	if !validProfileID(state.ProfileID) {
		state.ProfileID = "unavailable"
	}
	if !oneOf(state.InstallationState, "unknown", "absent", "present", "verified", "unavailable") {
		state.InstallationState = "unknown"
	}
	state.Code = publicModelCode(state.Code)
	if state.Operation != nil {
		operation := publicModelOperation(*state.Operation)
		state.Operation = &operation
	}
	return state
}

func publicModelOperation(operation ModelOperation) ModelOperation {
	if !validOpaque(operation.ID) {
		operation.ID = ""
	}
	if !validProfileID(operation.ProfileID) {
		operation.ProfileID = "unavailable"
	}
	if !oneOf(operation.Kind, "install", "verify", "remove") {
		operation.Kind = ""
	}
	if !oneOf(operation.State, "queued", "running", "completed", "failed", "cancelled", "interrupted") {
		operation.State = "interrupted"
	}
	if !oneOf(operation.Phase, "queued", "preparing", "install", "downloading", "download", "verifying", "verify", "removing", "remove", "activate", "activating", "complete", "completed", "reusing", "installed") {
		operation.Phase = ""
	}
	operation.Code = publicModelCode(operation.Code)
	for _, bytes := range []*int64{&operation.CompletedBytes, &operation.TotalBytes, &operation.AdditionalBytes} {
		if *bytes < 0 {
			*bytes = 0
		}
	}
	return operation
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

func publicModelCode(code string) string {
	if code == "" {
		return ""
	}
	if oneOf(code, "queued", "running", "not_inspected", "model_operation_busy", "model_not_installed", "model_hash_mismatch", "model_size_mismatch", "model_platform_unavailable",
		"model_offline_required", "model_active", "model_space_required", "model_operation_failed", "model_operation_cancelled", "model_operation_interrupted",
		"model_download_failed", "model_verified", "model_removed", "model_management_unavailable") {
		return code
	}
	return "model_operation_failed"
}

func modelError(w http.ResponseWriter, err error) {
	for _, mapping := range []struct {
		err    error
		status int
		code   string
	}{
		{ErrModelOperationBusy, 409, "model_operation_busy"}, {modelstore.ErrUnknownProfile, 404, "model_profile_unknown"},
		{modelstore.ErrNotInstalled, 409, "model_not_installed"}, {modelstore.ErrHashMismatch, 409, "model_hash_mismatch"},
		{modelstore.ErrSizeMismatch, 409, "model_size_mismatch"}, {modelstore.ErrPlatform, 409, "model_platform_unavailable"},
		{modelstore.ErrOfflineOnly, 409, "model_offline_required"}, {modelstore.ErrActiveLease, 409, "model_active"},
	} {
		if errors.Is(err, mapping.err) {
			fail(w, mapping.status, mapping.code)
			return
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		fail(w, 503, "request_interrupted")
		return
	}
	fail(w, http.StatusServiceUnavailable, "model_operation_failed")
}
