package app

import (
	"encoding/json"
	"testing"

	"github.com/Bodokh/wa-indexer/internal/localui"
)

func TestLocalAIDesiredChoiceDefaultsOffAndPreservesCore(t *testing.T) {
	var state applicationState
	if err := json.Unmarshal([]byte(`{"version":1,"mode":"demo","paused":true}`), &state); err != nil {
		t.Fatal(err)
	}
	if state.LocalAI.Revision != 1 || state.LocalAI.Choice.Mode != "off" || !state.Paused {
		t.Fatalf("unsafe default: %+v", state.LocalAI)
	}
	state.LocalAI = localAIRecord{Version: 1, Revision: 2, Choice: localui.LocalAIChoice{Mode: "preview", OCRProfile: "tesseract-fast", SpeechLanguage: "en"}}
	encoded, err := encodeState(state)
	if err != nil {
		t.Fatal(err)
	}
	var reopened applicationState
	if err := json.Unmarshal(encoded, &reopened); err != nil || reopened.LocalAI != state.LocalAI || !reopened.Paused {
		t.Fatalf("desired choice lost: %v", err)
	}
}

func TestLocalAIStateRejectsMalformedChoiceRatherThanEnabling(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"revision":0,"choice":{"mode":"off"}}`,
		`{"version":1,"revision":1,"choice":{"mode":"off","caption":true}}`,
		`{"version":1,"revision":1,"choice":{"mode":"preview"}}`,
		`{"version":1,"revision":1,"choice":{"mode":"preview","speech_language":"auto"}}`,
		`{"version":1,"revision":1,"choice":{"mode":"preview","ocr_profile":"../../runtime"}}`,
		`{"version":1,"revision":1,"choice":{"mode":"off","mode":"preview","caption":true}}`,
		`{"version":1,"revision":1,"choice":{"Mode":"off"}}`,
	} {
		var state applicationState
		if err := json.Unmarshal([]byte(`{"version":1,"mode":"real","paused":true,"local_ai":`+raw+`}`), &state); err == nil {
			t.Fatalf("invalid desired state accepted: %s", raw)
		}
	}
}

func TestLocalAIPersistencePreservesMalformedCredentialJournal(t *testing.T) {
	var state applicationState
	if err := json.Unmarshal([]byte(`{"version":1,"mode":"real","paused":true,"remote_clients":{"unknown":"retain"}}`), &state); err != nil {
		t.Fatal(err)
	}
	state.LocalAI.Revision = 2
	encoded, err := encodeState(state)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire["remote_clients"]) != `{"unknown":"retain"}` {
		t.Fatal("credential recovery bytes changed")
	}
	var reopened applicationState
	if err := json.Unmarshal(encoded, &reopened); err != nil || reopened.LocalAI.Revision != 2 || !reopened.RemoteInvalid || !reopened.Paused {
		t.Fatalf("core/AI state changed: %v", err)
	}
}
