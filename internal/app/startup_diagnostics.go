package app

import "errors"

// StartupErrorCode returns a fixed diagnostic code for an application-owned
// startup failure. It never formats the supplied error; unknown errors return "".
func StartupErrorCode(err error) string {
	switch {
	case errors.Is(err, errTranscriptionConfig):
		return "transcription_config_invalid"
	case errors.Is(err, errNativeNamespace):
		return "native_namespace_unavailable"
	case errors.Is(err, errExtractionRecovery):
		return "extraction_recovery_pending"
	case errors.Is(err, errBundledAI):
		return "local_ai_bundle_unavailable"
	default:
		return ""
	}
}
