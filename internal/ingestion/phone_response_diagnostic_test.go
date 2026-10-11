package ingestion

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"
)

type privatePhoneDiagnosticValue struct{ calls *int }

func (v privatePhoneDiagnosticValue) Error() string {
	if v.calls != nil {
		*v.calls = *v.calls + 1
	}
	panic("formatted private SDK error")
}
func (v privatePhoneDiagnosticValue) String() string {
	if v.calls != nil {
		*v.calls = *v.calls + 1
	}
	panic("formatted private SDK argument")
}

func TestPhoneResponseDiagnosticMatchesOnlyPinnedSDKLiterals(t *testing.T) {
	for _, item := range []struct {
		level, format, code string
		args                []any
	}{
		{"debug", "Handling response to placeholder resend request %s with %d items", "response_received", []any{privatePhoneDiagnosticValue{}, 0}},
		{"warn", "Missing response in item #%d of response to %s", "missing_payload", []any{1, privatePhoneDiagnosticValue{}}},
		{"warn", "Failed to unmarshal protobuf web message in item #%d of response to %s: %v", "invalid_payload", []any{1, privatePhoneDiagnosticValue{}, privatePhoneDiagnosticValue{}}},
		{"warn", "Failed to parse web message info in item #%d of response to %s: %v", "invalid_message", []any{1, privatePhoneDiagnosticValue{}, privatePhoneDiagnosticValue{}}},
	} {
		t.Run(item.code, func(t *testing.T) {
			encoded := phoneResponseDiagnostic(item.level, item.format, item.args...)
			var got map[string]any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal("recognized SDK response did not produce a diagnostic")
			}
			if got["event"] != "phone_response" || got["context"] != "placeholder_redelivery" || got["code"] != item.code {
				t.Fatal("SDK response diagnostic lost its closed context or code")
			}
			if item.code == "response_received" {
				if got["items"] != float64(0) || len(got) != 4 {
					t.Fatal("zero-item response was not identified precisely")
				}
			} else if len(got) != 3 {
				t.Fatal("payload error diagnostic included private SDK arguments")
			}
		})
	}
}

func TestPhoneResponseDiagnosticDiscardsUnknownFormatsAndUnsafeCounts(t *testing.T) {
	format := "Handling response to placeholder resend request %s with %d items"
	for _, level := range []string{"warn", "info", "error", "private-level"} {
		if encoded := phoneResponseDiagnostic(level, format, privatePhoneDiagnosticValue{}, 1); len(encoded) != 0 {
			t.Fatal("wrong SDK level matched a response literal")
		}
	}
	for _, format := range []string{"private-account private-message private-url", "prefix Handling response to placeholder resend request %s with %d items", "Failed to parse web message info: private error"} {
		if encoded := phoneResponseDiagnostic("debug", format, privatePhoneDiagnosticValue{}); len(encoded) != 0 {
			t.Fatal("arbitrary SDK format produced a diagnostic")
		}
	}
	for _, count := range []any{nil, -1, 9, "private-count", privatePhoneDiagnosticValue{}, int64(2)} {
		encoded := phoneResponseDiagnostic("debug", format, privatePhoneDiagnosticValue{}, count)
		var got map[string]any
		if err := json.Unmarshal(encoded, &got); err != nil || len(got) != 3 || got["code"] != "response_received" {
			t.Fatal("untrusted response count was not omitted")
		}
	}
	for _, count := range []int{1, 8} {
		var got map[string]any
		if err := json.Unmarshal(phoneResponseDiagnostic("debug", format, privatePhoneDiagnosticValue{}, count), &got); err != nil || got["items"] != float64(count) {
			t.Fatal("bounded typed response count was lost")
		}
	}
	for _, args := range [][]any{nil, {privatePhoneDiagnosticValue{}}} {
		var got map[string]any
		if err := json.Unmarshal(phoneResponseDiagnostic("debug", format, args...), &got); err != nil || len(got) != 3 {
			t.Fatal("short SDK header arguments were not handled safely")
		}
	}
	calls := 0
	private := privatePhoneDiagnosticValue{calls: &calls}
	_ = phoneResponseDiagnostic("debug", format, private, private)
	_ = phoneResponseDiagnostic("warn", "Failed to parse web message info in item #%d of response to %s: %v", 1, private, private)
	if calls != 0 {
		t.Fatal("diagnostic invoked a private formatter")
	}
}

func capturePhoneDiagnostic(t *testing.T, call func()) []byte {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = previous; _ = reader.Close(); _ = writer.Close() }()
	call()
	os.Stderr = previous
	_ = writer.Close()
	encoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestPhoneResponseLoggerIsInstalledOnRuntimeAndSubloggers(t *testing.T) {
	format := "Handling response to placeholder resend request %s with %d items"
	runtime := testRuntime(fixtureStore())
	encoded := capturePhoneDiagnostic(t, func() { runtime.client.Log.Debugf(format, privatePhoneDiagnosticValue{}, 0) })
	if !strings.Contains(string(encoded), `"code":"response_received"`) || !strings.Contains(string(encoded), `"items":0`) {
		t.Fatal("runtime SDK logger did not expose a zero-item phone response")
	}
	logger := newPhoneResponseLogger(waLog.Noop).Sub("private-module")
	encoded = capturePhoneDiagnostic(t, func() {
		logger.Warnf("Missing response in item #%d of response to %s", 1, privatePhoneDiagnosticValue{})
		logger.Errorf("private-error %v", privatePhoneDiagnosticValue{})
		logger.Infof("private-info %v", privatePhoneDiagnosticValue{})
		logger.Debugf("private-debug %v", privatePhoneDiagnosticValue{})
	})
	if strings.Count(string(encoded), "\n") != 1 || !strings.Contains(string(encoded), `"code":"missing_payload"`) || strings.Contains(string(encoded), "private") {
		t.Fatal("SDK sublogger emitted private or unrelated diagnostic data")
	}
}

type phoneDiagnosticLogCall struct {
	level, format string
	args          []any
}

type phoneDiagnosticDelegate struct {
	calls  []phoneDiagnosticLogCall
	module string
	child  *phoneDiagnosticDelegate
}

func (l *phoneDiagnosticDelegate) Debugf(format string, args ...any) {
	l.calls = append(l.calls, phoneDiagnosticLogCall{"debug", format, args})
}
func (l *phoneDiagnosticDelegate) Warnf(format string, args ...any) {
	l.calls = append(l.calls, phoneDiagnosticLogCall{"warn", format, args})
}
func (l *phoneDiagnosticDelegate) Errorf(format string, args ...any) {
	l.calls = append(l.calls, phoneDiagnosticLogCall{"error", format, args})
}
func (l *phoneDiagnosticDelegate) Infof(format string, args ...any) {
	l.calls = append(l.calls, phoneDiagnosticLogCall{"info", format, args})
}
func (l *phoneDiagnosticDelegate) Sub(module string) waLog.Logger {
	l.module = module
	return l.child
}

func TestPhoneResponseLoggerPreservesExistingDelegate(t *testing.T) {
	child := &phoneDiagnosticDelegate{}
	previous := &phoneDiagnosticDelegate{child: child}
	logger := newPhoneResponseLogger(previous)
	format := "unknown SDK format %v"
	calls := 0
	private := privatePhoneDiagnosticValue{calls: &calls}
	encoded := capturePhoneDiagnostic(t, func() {
		logger.Debugf(format, private)
		logger.Warnf(format, private)
		logger.Errorf(format, private)
		logger.Infof(format, private)
		logger.Sub("private-module").Debugf(format, private)
	})
	if len(encoded) != 0 || calls != 0 || len(previous.calls) != 4 || len(child.calls) != 1 || previous.module != "private-module" {
		t.Fatal("diagnostic changed previous SDK logging or sublogger routing")
	}
	for i, level := range []string{"debug", "warn", "error", "info"} {
		got := previous.calls[i]
		if got.level != level || got.format != format || len(got.args) != 1 || got.args[0] != private {
			t.Fatal("diagnostic changed a delegated log call")
		}
	}
	if newPhoneResponseLogger(logger) != logger {
		t.Fatal("SDK diagnostic decorator was installed twice")
	}
	encoded = capturePhoneDiagnostic(t, func() { newPhoneResponseLogger(nil).Debugf(format, private) })
	if len(encoded) != 0 {
		t.Fatal("missing SDK delegate emitted an unrelated log")
	}
}
