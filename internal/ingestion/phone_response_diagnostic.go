package ingestion

import (
	"encoding/json"
	"os"

	waLog "go.mau.fi/whatsmeow/util/log"
)

func phoneResponseDiagnostic(level, format string, args ...any) []byte {
	code := ""
	if level == "debug" && format == "Handling response to placeholder resend request %s with %d items" {
		code = "response_received"
	} else if level == "warn" {
		switch format {
		case "Missing response in item #%d of response to %s":
			code = "missing_payload"
		case "Failed to unmarshal protobuf web message in item #%d of response to %s: %v":
			code = "invalid_payload"
		case "Failed to parse web message info in item #%d of response to %s: %v":
			code = "invalid_message"
		}
	}
	if code == "" {
		return nil
	}
	var items *int
	if code == "response_received" && len(args) > 1 {
		if count, ok := args[1].(int); ok && count >= 0 && count <= 8 {
			items = &count
		}
	}
	encoded, _ := json.Marshal(struct {
		Event   string `json:"event"`
		Context string `json:"context"`
		Code    string `json:"code"`
		Items   *int   `json:"items,omitempty"`
	}{"phone_response", "placeholder_redelivery", code, items})
	return append(encoded, '\n')
}

func newPhoneResponseLogger(previous waLog.Logger) waLog.Logger {
	if previous == nil {
		previous = waLog.Noop
	}
	if _, installed := previous.(*phoneResponseLogger); installed {
		return previous
	}
	return &phoneResponseLogger{Logger: previous}
}

// The pinned SDK has no typed placeholder-response callback. Match only its
// exact format literals; never format request IDs, payloads, or error arguments.
type phoneResponseLogger struct{ waLog.Logger }

func (l *phoneResponseLogger) record(level, format string, args ...any) {
	if encoded := phoneResponseDiagnostic(level, format, args...); len(encoded) != 0 {
		_, _ = os.Stderr.Write(encoded)
	}
}

func (l *phoneResponseLogger) Debugf(format string, args ...any) {
	l.record("debug", format, args...)
	l.Logger.Debugf(format, args...)
}

func (l *phoneResponseLogger) Warnf(format string, args ...any) {
	l.record("warn", format, args...)
	l.Logger.Warnf(format, args...)
}

func (l *phoneResponseLogger) Sub(module string) waLog.Logger {
	return newPhoneResponseLogger(l.Logger.Sub(module))
}
