package localui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

const maxLocalAIOptions = 64

func validLocalAIChoice(choice LocalAIChoice) bool {
	if !oneOf(choice.Mode, "off", "preview", "accepted") || !oneOf(choice.SpeechLanguage, "", "he", "en", "mixed") {
		return false
	}
	for _, id := range []string{choice.EncoderProfile, choice.OCRProfile} {
		if id != "" && !validProfileID(id) {
			return false
		}
	}
	enabled := choice.EncoderProfile != "" || choice.OCRProfile != "" || choice.Caption || choice.SpeechLanguage != ""
	if (choice.Mode == "off") == enabled {
		return false
	}
	data, err := json.Marshal(choice)
	return err == nil && len(data) <= 1024
}

func publicLocalAICode(code string) string {
	if oneOf(code, "", "local_ai_off", "local_ai_applying", "local_ai_active", "local_ai_unavailable", "local_ai_cleanup_pending",
		"local_ai_bundle_unavailable", "local_ai_model_missing", "local_ai_profile_unqualified", "local_ai_operation_failed") {
		return code
	}
	return "local_ai_unavailable"
}

func publicLocalAI(snapshot LocalAISnapshot) (LocalAISnapshot, bool) {
	if snapshot.Revision == 0 || !validLocalAIChoice(snapshot.Choice) || len(snapshot.Options) > maxLocalAIOptions {
		return LocalAISnapshot{}, false
	}
	public := snapshot
	if !oneOf(public.State, "off", "applying", "active", "unavailable", "cleanup_pending") {
		public.State = "unavailable"
	}
	public.Code = publicLocalAICode(public.Code)
	public.Options = make([]LocalAIOption, 0, len(snapshot.Options))
	seen := make(map[string]bool)
	for _, option := range snapshot.Options {
		if !oneOf(option.Kind, "encoder", "ocr", "caption", "speech") || !validProfileID(option.ProfileID) || !validProfileID(option.ModelProfileID) {
			continue
		}
		if option.Kind == "speech" && !oneOf(option.ProfileID, "he", "en", "mixed") {
			continue
		}
		if !oneOf(option.State, "preview", "accepted", "unavailable") {
			option.State = "unavailable"
		}
		option.Code = publicLocalAICode(option.Code)
		key := option.Kind + ":" + option.ProfileID + ":" + option.State
		if !seen[key] {
			public.Options = append(public.Options, option)
			seen[key] = true
		}
	}
	return public, true
}

func (s *Server) localAI(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	app, ok := s.app.(LocalAIApplication)
	if !ok {
		fail(w, 503, "local_ai_unavailable")
		return
	}
	var snapshot LocalAISnapshot
	var err error
	if r.URL.Path == "/api/local-ai/select" {
		var input LocalAIUpdate
		if !decode(w, r, &input) {
			return
		}
		if input.ExpectedRevision == 0 || !validLocalAIChoice(input.Choice) {
			fail(w, 400, "local_ai_invalid")
			return
		}
		// The app proves that each selected mapping is currently eligible and
		// durably applies CAS before its joined transition. HTTP never activates
		// a model based on catalog metadata or successful installation alone.
		snapshot, err = app.SetLocalAI(ctx, input)
	} else {
		snapshot, err = app.LocalAIState(ctx)
	}
	if err != nil {
		localAIError(w, err)
		return
	}
	public, ok := publicLocalAI(snapshot)
	if !ok {
		fail(w, 503, "local_ai_unavailable")
		return
	}
	respond(w, public)
}

func localAIError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrLocalAIConflict):
		fail(w, 409, "local_ai_conflict")
	case errors.Is(err, ErrLocalAIInvalid):
		fail(w, 400, "local_ai_invalid")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		fail(w, 503, "request_interrupted")
	default:
		fail(w, 503, "local_ai_unavailable")
	}
}
