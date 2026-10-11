package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/Bodokh/wa-indexer/internal/localui"
)

// Desired AI selection shares the existing private application state. It is
// neither installation authority nor evidence of native/quality acceptance.
type localAIRecord struct {
	Version  int                   `json:"version"`
	Revision uint64                `json:"revision"`
	Choice   localui.LocalAIChoice `json:"choice"`
}

func defaultLocalAIRecord() localAIRecord {
	return localAIRecord{Version: 1, Revision: 1, Choice: localui.LocalAIChoice{Mode: "off"}}
}

func validLocalAIChoice(choice localui.LocalAIChoice) bool {
	if choice.Mode == "off" {
		return choice.EncoderProfile == "" && choice.OCRProfile == "" && !choice.Caption && choice.SpeechLanguage == ""
	}
	if choice.Mode != "preview" && choice.Mode != "accepted" {
		return false
	}
	if choice.EncoderProfile == "" && choice.OCRProfile == "" && !choice.Caption && choice.SpeechLanguage == "" {
		return false
	}
	for _, value := range []string{choice.EncoderProfile, choice.OCRProfile} {
		if len(value) > 100 {
			return false
		}
		for _, char := range value {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.') {
				return false
			}
		}
	}
	return choice.SpeechLanguage == "" || choice.SpeechLanguage == "he" || choice.SpeechLanguage == "en" || choice.SpeechLanguage == "mixed"
}

func decodeLocalAIRecord(raw json.RawMessage) (localAIRecord, error) {
	if len(raw) == 0 {
		return defaultLocalAIRecord(), nil
	}
	var record localAIRecord
	if len(raw) > 1024 || !strictLocalAIKeys(raw, false) {
		return record, localui.ErrLocalAIInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, localui.ErrLocalAIInvalid
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return record, localui.ErrLocalAIInvalid
	}
	if record.Version != 1 || record.Revision == 0 || !validLocalAIChoice(record.Choice) {
		return record, localui.ErrLocalAIInvalid
	}
	return record, nil
}

func strictLocalAIKeys(raw json.RawMessage, choice bool) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return false
	}
	allowed := map[string]bool{"version": true, "revision": true, "choice": true}
	if choice {
		allowed = map[string]bool{"mode": true, "encoder_profile": true, "ocr_profile": true, "caption": true, "speech_language": true}
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] || seen[key] {
			return false
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return false
		}
		if !choice && key == "choice" && !strictLocalAIKeys(value, true) {
			return false
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return false
	}
	return errors.Is(decoder.Decode(new(any)), io.EOF)
}

func encodeLocalAIRecord(record localAIRecord) (json.RawMessage, error) {
	if record == (localAIRecord{}) {
		record = defaultLocalAIRecord()
	}
	if record.Version != 1 || record.Revision == 0 || !validLocalAIChoice(record.Choice) {
		return nil, localui.ErrLocalAIInvalid
	}
	data, err := json.Marshal(record)
	if err != nil || len(data) > 1024 {
		return nil, localui.ErrLocalAIInvalid
	}
	return data, nil
}
