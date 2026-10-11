package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/transcription"
)

var errTranscriptionConfig = errors.New("transcription_config_invalid")

type transcriptionConfigFailure struct{ reason string }

func (e *transcriptionConfigFailure) Error() string { return errTranscriptionConfig.Error() }
func (e *transcriptionConfigFailure) Unwrap() error { return errTranscriptionConfig }
func voiceConfigFailure(reason string) error        { return &transcriptionConfigFailure{reason: reason} }

const (
	transcriptionURLEnv      = "WA_INDEXER_TRANSCRIPTION_URL"
	transcriptionTokenEnv    = "WA_INDEXER_TRANSCRIPTION_TOKEN"
	transcriptionLanguageEnv = "WA_INDEXER_TRANSCRIPTION_LANGUAGE"
	transcriptionTimeoutEnv  = "WA_INDEXER_TRANSCRIPTION_TIMEOUT_SECONDS"
)

var transcriptionAppEnv = []string{transcriptionURLEnv, transcriptionTokenEnv, transcriptionLanguageEnv, transcriptionTimeoutEnv}

func transcriptionLaunchConfig(opts options) (*transcription.Options, error) {
	values := make(map[string]string)
	for _, key := range transcriptionAppEnv {
		if value, present := os.LookupEnv(key); present && value != "" {
			values[key] = value
		}
	}
	if opts.transcriptionEnvFile != "" {
		data, err := readPrivateEnvFile(opts.transcriptionEnvFile)
		if err != nil {
			return nil, voiceConfigFailure("transcription_env_unsafe")
		}
		fromFile, err := parseTranscriptionEnv(data)
		if err != nil {
			return nil, err
		}
		for _, key := range transcriptionAppEnv {
			value, present := fromFile[key]
			if !present {
				continue
			}
			if ambient, set := values[key]; set && ambient != "" && ambient != value {
				return nil, voiceConfigFailure("transcription_env_conflict")
			}
			values[key] = value
		}
	}
	if len(values) == 0 && opts.transcriptionEnvFile == "" {
		return nil, nil
	}
	if values[transcriptionURLEnv] == "" {
		return nil, voiceConfigFailure("transcription_url_missing")
	}
	if values[transcriptionTokenEnv] == "" {
		return nil, voiceConfigFailure("transcription_token_missing")
	}
	language := values[transcriptionLanguageEnv]
	if language == "" {
		language = "auto"
	}
	timeout := 600
	if value, present := values[transcriptionTimeoutEnv]; present {
		var err error
		if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) != -1 {
			return nil, voiceConfigFailure("transcription_timeout_invalid")
		}
		timeout, err = strconv.Atoi(value)
		if err != nil || timeout < 10 || timeout > 1800 {
			return nil, voiceConfigFailure("transcription_timeout_invalid")
		}
	}
	cfg := transcription.Options{URL: values[transcriptionURLEnv], Token: values[transcriptionTokenEnv], Language: language, Timeout: time.Duration(timeout) * time.Second}
	if transcription.ValidateOptions(cfg) != nil {
		return nil, voiceConfigFailure("transcription_connection_invalid")
	}
	return &cfg, nil
}

// The installer writes one private file for both processes. Only this fixed
// allowlist is accepted; values remain data and are never evaluated by a shell.
func parseTranscriptionEnv(data []byte) (map[string]string, error) {
	allowed := map[string]bool{
		transcriptionURLEnv: true, transcriptionTokenEnv: true, transcriptionLanguageEnv: true, transcriptionTimeoutEnv: true,
		"WA_TRANSCRIPTION_TOKEN": true, "WA_TRANSCRIPTION_LISTEN": true, "WA_TRANSCRIPTION_MODEL_DIR": true,
		"WA_TRANSCRIPTION_LANGUAGE": true, "WA_TRANSCRIPTION_CPU_THREADS": true, "WA_TRANSCRIPTION_BODY_TIMEOUT_SECONDS": true,
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		_, seen := values[key]
		if !ok || !allowed[key] || seen || value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n\"'`$") {
			return nil, voiceConfigFailure("transcription_env_syntax_invalid")
		}
		values[key] = value
	}
	return values, nil
}

func voiceConfigErrorMessage(err error) (string, bool) {
	var failure *transcriptionConfigFailure
	if !errors.As(err, &failure) {
		return "", false
	}
	switch failure.reason {
	case "transcription_env_unsafe":
		return "My WhatsApp MCP could not run (transcription_env_unsafe). Use an owned 0600 env file in an owned 0700 directory, without links or extended ACLs.", true
	case "transcription_env_conflict":
		return "My WhatsApp MCP could not run (transcription_env_conflict). Remove conflicting voice settings from the process environment or private env file.", true
	case "transcription_url_missing":
		return "My WhatsApp MCP could not run (transcription_url_missing). Set WA_INDEXER_TRANSCRIPTION_URL in the private voice configuration.", true
	case "transcription_token_missing":
		return "My WhatsApp MCP could not run (transcription_token_missing). Set WA_INDEXER_TRANSCRIPTION_TOKEN in the private voice configuration.", true
	case "transcription_timeout_invalid":
		return "My WhatsApp MCP could not run (transcription_timeout_invalid). Set WA_INDEXER_TRANSCRIPTION_TIMEOUT_SECONDS to an integer from 10 through 1800.", true
	case "transcription_env_syntax_invalid":
		return "My WhatsApp MCP could not run (transcription_env_syntax_invalid). Use the installer-generated env file with one unquoted KEY=value per line and no duplicate or unknown keys.", true
	case "transcription_connection_invalid":
		return "My WhatsApp MCP could not run (transcription_connection_invalid). Check the literal loopback HTTP URL, service token, and language setting.", true
	default:
		return "", false
	}
}
