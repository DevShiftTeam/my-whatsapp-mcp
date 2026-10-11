package app

import (
	"errors"
	"testing"
)

// The diagnostic must use identity and unwrapping, never private error text.
type startupDiagnosticOpaqueError struct {
	cause error
}

func (startupDiagnosticOpaqueError) Error() string {
	panic("startup diagnostic formatted a private error")
}

func (e startupDiagnosticOpaqueError) Unwrap() error {
	return e.cause
}

func TestStartupErrorCodePrivateGates(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{"native_namespace", errNativeNamespace, "native_namespace_unavailable"},
		{"extraction_recovery", errExtractionRecovery, "extraction_recovery_pending"},
		{"local_ai_bundle", errBundledAI, "local_ai_bundle_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := StartupErrorCode(test.err); got != test.code {
				t.Fatalf("direct code = %q, want %q", got, test.code)
			}
			wrapped := startupDiagnosticOpaqueError{cause: startupDiagnosticOpaqueError{cause: test.err}}
			if got := StartupErrorCode(wrapped); got != test.code {
				t.Fatalf("wrapped code = %q, want %q", got, test.code)
			}
		})
	}
}

func TestStartupErrorCodeUnknownDoesNotFormat(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"opaque", startupDiagnosticOpaqueError{}},
		{"same_namespace_text", errors.New("native_executable_namespace_unavailable")},
		{"same_recovery_text", errors.New("extraction_recovery_pending")},
		{"same_bundle_text", errors.New("local_ai_bundle_unavailable")},
		{"nonstartup_client_recovery", errNativeRecovery},
		{"nonstartup_remote_recovery", errRemoteRecovery},
		{"nonstartup_model_operation", errModelOperation},
		{"nonstartup_demo_config", errDemoAcceptance},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := StartupErrorCode(test.err); got != "" {
				t.Fatalf("unknown code = %q, want empty", got)
			}
		})
	}
}
