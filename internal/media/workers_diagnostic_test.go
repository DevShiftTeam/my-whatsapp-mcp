//go:build darwin || linux

package media

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func diagnosticTestFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "private.stderr"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error("diagnostic close failed")
		}
	})
	return f
}

func diagnosticTestRuntime() RuntimeSpec {
	return RuntimeSpec{Path: "/private/caption", Bytes: 1, SHA256: strings.Repeat("c", 64)}
}

func TestMediaDiagnosticDefaultAndOtherRuntimeDiscard(t *testing.T) {
	spec := diagnosticTestRuntime()
	writer, capture := nativeDiagnosticStderr(context.Background(), spec)
	if writer != io.Discard || capture != nil {
		t.Fatal("default extraction gained a stderr sink")
	}
	file := diagnosticTestFile(t)
	ctx, diagnostic, err := withNativeDiagnostic(context.Background(), spec, file)
	if err != nil {
		t.Fatal(err)
	}
	wrong := spec
	wrong.SHA256 = strings.Repeat("d", 64)
	writer, capture = nativeDiagnosticStderr(ctx, wrong)
	if writer != io.Discard || capture != nil {
		t.Fatal("diagnostic authority crossed a runtime identity")
	}
	if n, err := writer.Write([]byte("private child text")); err != nil || n != 18 {
		t.Fatal("default discard did not consume stderr")
	}
	if info, err := file.Stat(); err != nil || info.Size() != 0 || diagnostic.snapshot().StderrBytes != 0 {
		t.Fatal("discarded bytes reached the private file")
	}
}

func TestMediaDiagnosticCapsFileAndDrainsExcess(t *testing.T) {
	spec, file := diagnosticTestRuntime(), diagnosticTestFile(t)
	ctx, capture, err := withNativeDiagnostic(context.Background(), spec, file)
	if err != nil {
		t.Fatal(err)
	}
	writer, got := nativeDiagnosticStderr(ctx, spec)
	if got != capture {
		t.Fatal("exact runtime lost its local capture")
	}
	first := bytes.Repeat([]byte{'A'}, 32760)
	second := bytes.Repeat([]byte{'B'}, 4096)
	if n, err := writer.Write(first); n != len(first) || err != nil {
		t.Fatal("capped capture blocked stderr draining")
	}
	var output strings.Builder
	output.Write(second)
	// Before native/stdout/holder cleanup, the builder may still be written.
	// The diagnostic must not read it or acknowledge failed stdout yet.
	capture.captureFailedOutput(&output)
	if status := capture.snapshot(); status.StderrBytes != int64(len(first)) || status.StdoutOnFailureCaptured {
		t.Fatal("failed stdout was inspected before cleanup acknowledgement")
	}
	capture.observe(6, nil)
	capture.captureFailedOutput(&output)
	if output.String() != string(second) {
		t.Fatal("diagnostic changed the ordinary stdout parser input")
	}
	if n, err := writer.Write(second); n != len(second) || err != nil {
		t.Fatal("combined capped capture blocked stderr draining")
	}
	status := capture.snapshot()
	if status.StderrBytes != 32768 || !status.Truncated || status.WriteFailed || !status.StdoutOnFailureCaptured {
		t.Fatal("combined 32KiB boundary or failed stdout status lost")
	}
	if info, err := file.Stat(); err != nil || info.Size() != 32768 {
		t.Fatal("physical diagnostic extent escaped cap")
	}
	body := make([]byte, 32768)
	if n, err := file.ReadAt(body, 0); n != len(body) || err != nil {
		t.Fatal("bounded diagnostic unavailable")
	}
	if !bytes.Equal(body[:32760], first) || !bytes.Equal(body[32760:], bytes.Repeat([]byte{'B'}, 8)) {
		t.Fatal("bounded diagnostic no longer preserves the exact prefix")
	}
	fullFile := diagnosticTestFile(t)
	_, full, err := withNativeDiagnostic(context.Background(), spec, fullFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = full.Write(bytes.Repeat([]byte{'C'}, 32768)); err != nil {
		t.Fatal("full stderr capture failed")
	}
	full.observe(6, nil)
	full.captureFailedOutput(&output)
	if status := full.snapshot(); status.StderrBytes != 32768 || status.StdoutOnFailureCaptured || !status.Truncated {
		t.Fatal("stderr-filled cap falsely acknowledged stored stdout")
	}
}

func TestMediaDiagnosticIndependentContexts(t *testing.T) {
	spec := diagnosticTestRuntime()
	a, b := diagnosticTestFile(t), diagnosticTestFile(t)
	ca, da, err := withNativeDiagnostic(context.Background(), spec, a)
	if err != nil {
		t.Fatal(err)
	}
	cb, db, err := withNativeDiagnostic(context.Background(), spec, b)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for _, row := range []struct {
		ctx  context.Context
		text string
	}{{ca, "only-a"}, {cb, "only-b"}} {
		group.Add(1)
		go func(row struct {
			ctx  context.Context
			text string
		}) {
			defer group.Done()
			writer, _ := nativeDiagnosticStderr(row.ctx, spec)
			if _, err := writer.Write([]byte(row.text)); err != nil {
				t.Error("independent sink write failed")
			}
		}(row)
	}
	group.Wait()
	for _, row := range []struct {
		file    *os.File
		capture *nativeRuntimeDiagnostic
		text    string
	}{{a, da, "only-a"}, {b, db, "only-b"}} {
		body := make([]byte, len(row.text))
		if n, err := row.file.ReadAt(body, 0); err != nil || n != len(body) || string(body) != row.text || row.capture.snapshot().StderrBytes != int64(len(body)) {
			t.Fatal("per-call captures shared authority or bytes")
		}
	}
}
