package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/transcription"
)

type appStatusService struct {
	status  atomic.Int64
	blocked atomic.Bool
	info    atomic.Value
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
	server  *httptest.Server
}

func appStatusInfo(t *testing.T, change bool) []byte {
	t.Helper()
	threads := 4
	if change {
		threads++
	}
	modelSHA := "db2a2265aa012c16c7db9edda3d699c99f984efdd3f2e22a72a8ce7e9720f3a2"
	r := map[string]any{
		"protocol_version": 1, "backend": "faster-whisper", "model": "ivrit-ai/whisper-large-v3-turbo-ct2",
		"revision": "72ad623a37947395efcc3933132353790e5a12f5", "model_sha256": modelSHA,
		"model_files": []map[string]string{{"path": "config.json", "sha256": strings.Repeat("c", 64)}, {"path": "model.bin", "sha256": modelSHA}, {"path": "tokenizer.json", "sha256": strings.Repeat("d", 64)}},
		"runtime":     map[string]any{"faster_whisper": "1.2.1", "ctranslate2": "4.8.2", "av": "18.0.0", "onnxruntime": "1.31.0", "tokenizers": "0.22.1", "compute_type": "int8", "cpu_threads": threads, "num_workers": 1, "language": "he", "vad_filter": true},
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal("synthetic recipe")
	}
	sum := sha256.Sum256(encoded)
	info, err := json.Marshal(map[string]any{"recipe": r, "identity": hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatal("synthetic information")
	}
	return info
}

func newAppStatusFixture(t *testing.T, beforeInfo ...func()) (*Application, *voiceOwner, *appStatusService) {
	t.Helper()
	const token = "synthetic-status-service-token-0123456789"
	f := &appStatusService{entered: make(chan struct{}, 1), release: make(chan struct{})}
	f.info.Store(appStatusInfo(t, false))
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/info" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("application status crossed the information-only boundary")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.calls.Add(1) > 1 && len(beforeInfo) > 0 {
			beforeInfo[0]()
		}
		if f.blocked.Load() {
			select {
			case f.entered <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
				return
			case <-f.release:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if status := f.status.Load(); status != 0 {
			w.WriteHeader(int(status))
			return
		}
		_, _ = w.Write(f.info.Load().([]byte))
	}))
	t.Cleanup(f.server.Close)
	opts := &transcription.Options{URL: f.server.URL, Token: token, Language: "he"}
	b, err := transcription.New(context.Background(), *opts)
	if err != nil {
		t.Fatal("synthetic application adapter", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	owner := &voiceOwner{ctx: ctx, cancel: cancel, done: make(chan struct{}), backend: b}
	r := &resources{voice: owner}
	r.voiceStatus.Store(owner)
	a := &Application{transcriptionConfig: opts, resources: r, queryContext: context.Background()}
	t.Cleanup(func() { cancel(); _ = b.Close(context.Background()) })
	return a, owner, f
}

func TestTranscriptionStatusReportsOnlyCurrentRunningVoiceOwner(t *testing.T) {
	a, owner, f := newAppStatusFixture(t)
	check := func(state string, ready bool) {
		t.Helper()
		value, err := a.TranscriptionStatus(context.Background())
		if err != nil || !value.Configured || value.State != state || value.HebrewReady != ready {
			t.Fatal("unexpected running transcription status", value, err)
		}
		encoded, _ := json.Marshal(value)
		if strings.Contains(string(encoded), "synthetic") || strings.Contains(string(encoded), "http") || strings.Contains(string(encoded), "ivrit") {
			t.Fatal("status exposed private runtime configuration")
		}
	}
	check("ready", true)
	f.status.Store(http.StatusTooManyRequests)
	check("busy", true)
	f.status.Store(http.StatusInternalServerError)
	check("unavailable", false)
	f.status.Store(http.StatusTooManyRequests)
	check("busy", false)
	f.status.Store(0)
	check("ready", true)
	f.info.Store(appStatusInfo(t, true))
	check("mismatch", false)
	f.status.Store(http.StatusTooManyRequests)
	check("busy", false)
	f.status.Store(0)
	f.info.Store(appStatusInfo(t, false))
	check("ready", true)
	a.transcriptionConfig.Language = "auto"
	check("mismatch", false)
	a.transcriptionConfig.Language = "he"
	backend := owner.backend
	owner.backend = nil
	before := f.calls.Load()
	check("connecting", false)
	if f.calls.Load() != before {
		t.Fatal("status constructed a fresh adapter for a disconnected worker")
	}
	owner.backend = backend
	a.resources.voiceStatus.Store(&voiceOwner{})
	check("connecting", false)
	if f.calls.Load() != before {
		t.Fatal("status probed a stale published owner")
	}
	a.resources.voiceStatus.Store(owner)
	a.transcriptionConfig = nil
	value, err := a.TranscriptionStatus(context.Background())
	if err != nil || value.Configured || value.State != "not_configured" || value.HebrewReady || f.calls.Load() != before {
		t.Fatal("an unwired configuration borrowed an existing adapter")
	}
	a.transcriptionConfig = &transcription.Options{Language: "he"}
	_ = backend.Close(context.Background())
	check("stopped", false)
}

func TestTranscriptionStatusUnconfiguredAndStoppedModes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		app        *Application
		configured bool
		state      string
	}{
		{"unconfigured", &Application{}, false, "not_configured"},
		{"waiting_for_worker", &Application{transcriptionConfig: &transcription.Options{Language: "he"}}, true, "connecting"},
		{"closed_unconfigured", &Application{closed: true}, false, "stopped"},
		{"closed", &Application{closed: true, transcriptionConfig: &transcription.Options{Language: "he"}}, true, "stopped"},
		{"offline", &Application{offline: true, transcriptionConfig: &transcription.Options{Language: "he"}}, true, "stopped"},
		{"snapshot", &Application{snapshot: &SnapshotConfig{}, transcriptionConfig: &transcription.Options{Language: "he"}}, true, "stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := tc.app.TranscriptionStatus(context.Background())
			if err != nil || value.Configured != tc.configured || value.State != tc.state || value.HebrewReady {
				t.Fatal("status invented a configured active worker", value, err)
			}
		})
	}
}

func TestTranscriptionStatusWithholdsResultsAfterRuntimeTransition(t *testing.T) {
	for _, transition := range []string{"cancel_owner", "close_app", "replace_resources", "replace_backend"} {
		t.Run(transition, func(t *testing.T) {
			a, owner, f := newAppStatusFixture(t)
			f.blocked.Store(true)
			done := make(chan localui.TranscriptionStatus, 1)
			go func() {
				value, _ := a.TranscriptionStatus(context.Background())
				done <- value
			}()
			select {
			case <-f.entered:
			case <-time.After(time.Second):
				t.Fatal("probe did not start")
			}
			a.data.Lock()
			switch transition {
			case "cancel_owner":
				owner.cancel()
			case "close_app":
				a.closed = true
			case "replace_resources":
				a.resources = &resources{}
			case "replace_backend":
				owner.mu.Lock()
				owner.backend = nil
				owner.mu.Unlock()
			}
			a.data.Unlock()
			close(f.release)
			select {
			case value := <-done:
				if value.State != "stopped" || value.HebrewReady {
					t.Fatal("a retired runtime published readiness", value)
				}
			case <-time.After(time.Second):
				t.Fatal("status retained lifecycle ownership")
			}
		})
	}
}

func TestTranscriptionStatusCancellationAndLockContentionAreBounded(t *testing.T) {
	a, owner, f := newAppStatusFixture(t)
	f.blocked.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	value, err := a.TranscriptionStatus(ctx)
	if err == nil || value.HebrewReady {
		t.Fatal("cancelled status published successful readiness")
	}
	a.data.Lock()
	started := time.Now()
	value, err = a.TranscriptionStatus(context.Background())
	a.data.Unlock()
	if err == nil || value.HebrewReady || time.Since(started) > time.Second {
		t.Fatal("status waited indefinitely behind a lifecycle mutation")
	}
	close(owner.done)
	before := f.calls.Load()
	value, err = a.TranscriptionStatus(context.Background())
	if err != nil || value.State != "stopped" || value.HebrewReady || f.calls.Load() != before {
		t.Fatal("completed voice owner was treated as running")
	}
}

func TestTranscriptionStatusOwnedQueryCancellationStopsProbe(t *testing.T) {
	a, _, f := newAppStatusFixture(t)
	queries, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.queryContext = queries
	f.blocked.Store(true)
	done := make(chan error, 1)
	go func() {
		value, err := a.TranscriptionStatus(context.Background())
		if value.HebrewReady {
			done <- nil
			return
		}
		done <- err
	}()
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("owned status probe did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled application query published success")
		}
	case <-time.After(time.Second):
		t.Fatal("application cancellation did not stop the probe")
	}
}

// This value-free context preserves the underlying real Done/Err pair while
// queuing AfterFunc callbacks. Cancellation is observable before the callback
// that would cancel the request has been scheduled.
type queuedTranscriptionQueryContext struct {
	context.Context
	mu         sync.Mutex
	callback   func()
	registered atomic.Bool
}

func (*queuedTranscriptionQueryContext) Value(any) any { return nil }

func (q *queuedTranscriptionQueryContext) AfterFunc(callback func()) func() bool {
	q.mu.Lock()
	q.callback = callback
	q.registered.Store(true)
	q.mu.Unlock()
	return func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.callback == nil {
			return false
		}
		q.callback = nil
		return true
	}
}

func TestTranscriptionStatusSuccessfulProbeRejectsQueuedQueryCancellation(t *testing.T) {
	queries, cancel := context.WithCancel(context.Background())
	defer cancel()
	queued := &queuedTranscriptionQueryContext{Context: queries}
	a, _, f := newAppStatusFixture(t, func() {
		cancel()
		queued.mu.Lock()
		pending := queued.callback != nil
		queued.mu.Unlock()
		if !pending || queued.Err() != context.Canceled {
			t.Error("query cancellation did not leave a valid callback pending")
		}
		select {
		case <-queued.Done():
		default:
			t.Error("cancelled context did not close its real Done channel")
		}
	})
	a.queryContext = queued
	value, err := a.TranscriptionStatus(context.Background())
	if f.calls.Load() != 2 || !queued.registered.Load() {
		t.Fatal("successful information probe did not exercise delayed AfterFunc propagation")
	}
	if value.HebrewReady || !errors.Is(err, context.Canceled) {
		t.Fatal("successful probe published readiness before query cancellation propagated", value, err)
	}
}

type nonComparableTranscriptionQueryContext struct {
	context.Context
	marker []byte
}

func TestTranscriptionStatusSuccessfulProbeRejectsReplacedQueryContext(t *testing.T) {
	for _, replacement := range []string{"live", "nil", "noncomparable"} {
		t.Run(replacement, func(t *testing.T) {
			queries, cancel := context.WithCancel(context.Background())
			defer cancel()
			replaced, stop := context.WithCancel(context.Background())
			defer stop()
			var next context.Context
			switch replacement {
			case "live":
				next = replaced
			case "noncomparable":
				next = nonComparableTranscriptionQueryContext{Context: replaced, marker: []byte("synthetic")}
			}
			var a *Application
			var f *appStatusService
			a, _, f = newAppStatusFixture(t, func() {
				a.data.Lock()
				a.queryContext = next
				a.data.Unlock()
			})
			a.queryContext = queries
			value, err := a.TranscriptionStatus(context.Background())
			if f.calls.Load() != 2 || queries.Err() != nil || replaced.Err() != nil {
				t.Fatal("replacement regression did not preserve both live context lifetimes")
			}
			if value.HebrewReady || !errors.Is(err, errTranscriptionStatusUnavailable) {
				t.Fatal("successful probe borrowed a replacement query lifetime", value, err)
			}
		})
	}
}

func TestTranscriptionStatusUncomparableQueryContextFailsClosed(t *testing.T) {
	a, _, _ := newAppStatusFixture(t)
	queries, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.queryContext = nonComparableTranscriptionQueryContext{Context: queries, marker: []byte("synthetic")}
	value, err := a.TranscriptionStatus(context.Background())
	if value.HebrewReady || !errors.Is(err, errTranscriptionStatusUnavailable) {
		t.Fatal("query context without a safely comparable identity claimed readiness", value, err)
	}
}
