package transcription

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func hebrewStatusRecipe() recipe {
	r := testRecipe()
	r.Revision = "72ad623a37947395efcc3933132353790e5a12f5"
	r.ModelSHA256 = "db2a2265aa012c16c7db9edda3d699c99f984efdd3f2e22a72a8ce7e9720f3a2"
	r.ModelFiles[1].SHA256 = r.ModelSHA256
	return r
}

type statusService struct {
	info    atomic.Pointer[infoResponse]
	status  atomic.Int64
	blocked atomic.Bool
	calls   atomic.Int64
	entered chan struct{}
	server  *httptest.Server
}

func newStatusService(t *testing.T, r recipe) *statusService {
	t.Helper()
	f := &statusService{entered: make(chan struct{}, 1)}
	info := testInfo(t, r)
	f.info.Store(&info)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/info" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("status probe lost its authenticated information-only boundary")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.calls.Add(1)
		if f.blocked.Load() {
			select {
			case f.entered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if status := f.status.Load(); status != 0 {
			w.WriteHeader(int(status))
			_, _ = w.Write([]byte(`{"private":"STATUS_SECRET_CANARY"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(f.info.Load())
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *statusService) backend(t *testing.T, language string) *Backend {
	t.Helper()
	b, err := New(context.Background(), Options{URL: f.server.URL, Token: testToken, Language: language})
	if err != nil {
		t.Fatal("synthetic status adapter construction failed", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

func TestHebrewStatusRequiresExactFrozenSupportedRecipe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*recipe)
	}{
		{"supported", func(*recipe) {}},
		{"model", func(r *recipe) { r.Model = "synthetic/other-model" }},
		{"revision", func(r *recipe) { r.Revision = testRecipe().Revision }},
		{"digest", func(r *recipe) { r.ModelSHA256 = testRecipe().ModelSHA256; r.ModelFiles[1].SHA256 = r.ModelSHA256 }},
		{"faster_whisper", func(r *recipe) { r.Runtime.FasterWhisper = "1.2.0" }},
		{"ctranslate2", func(r *recipe) { r.Runtime.CTranslate2 = "4.8.1" }},
		{"av", func(r *recipe) { r.Runtime.AV = "17.0.0" }},
		{"onnxruntime", func(r *recipe) { r.Runtime.ONNXRuntime = "1.30.0" }},
		{"tokenizers", func(r *recipe) { r.Runtime.Tokenizers = "0.22.0" }},
		{"automatic_language", func(r *recipe) { r.Runtime.Language = "auto" }},
		{"other_language", func(r *recipe) { r.Runtime.Language = "en" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := hebrewStatusRecipe()
			tc.mutate(&r)
			f := newStatusService(t, r)
			b := f.backend(t, r.Runtime.Language)
			version := b.Version()
			ready, err := b.HebrewStatus(context.Background())
			if tc.name == "supported" {
				if !ready || err != nil || f.calls.Load() != 2 {
					t.Fatal("supported status did not freshly verify the running adapter")
				}
			} else if ready || !errors.Is(err, ErrModelChanged) {
				t.Fatal("unsupported frozen recipe claimed Hebrew readiness")
			}
			if b.Version() != version {
				t.Fatal("status changed the extraction recipe")
			}
		})
	}
}

func TestHebrewStatusBusyRequiresUnbrokenVerifiedIdentity(t *testing.T) {
	f := newStatusService(t, hebrewStatusRecipe())
	b := f.backend(t, "he")
	check := func(want bool, wantErr error) {
		t.Helper()
		ready, err := b.HebrewStatus(context.Background())
		if ready != want || !errors.Is(err, wantErr) {
			t.Fatal("status incorrectly promoted busy, changed, or unavailable service", ready, err)
		}
	}
	f.status.Store(http.StatusTooManyRequests)
	check(true, ErrBusy)
	f.status.Store(http.StatusInternalServerError)
	check(false, ErrUnavailable)
	f.status.Store(http.StatusTooManyRequests)
	check(false, ErrBusy)
	f.status.Store(0)
	check(true, nil)
	r := hebrewStatusRecipe()
	r.Runtime.FasterWhisper = "1.2.2"
	changed := testInfo(t, r)
	f.info.Store(&changed)
	check(false, ErrModelChanged)
	f.status.Store(http.StatusTooManyRequests)
	check(false, ErrBusy)
	f.status.Store(0)
	original := testInfo(t, hebrewStatusRecipe())
	f.info.Store(&original)
	check(true, nil)
	f.status.Store(http.StatusUnauthorized)
	check(false, ErrUnauthorized)
	_ = b.Close(context.Background())
	check(false, ErrStopped)
}

func TestHebrewStatusWorkerProbeAlsoInvalidatesBusyQualification(t *testing.T) {
	f := newStatusService(t, hebrewStatusRecipe())
	b := f.backend(t, "he")
	changed := testInfo(t, hebrewStatusRecipe())
	changed.Identity = testInfo(t, testRecipe()).Identity
	f.info.Store(&changed)
	if !errors.Is(b.Ready(context.Background()), ErrInvalidResponse) {
		t.Fatal("worker accepted unauthenticated recipe identity")
	}
	f.status.Store(http.StatusTooManyRequests)
	ready, err := b.HebrewStatus(context.Background())
	if ready || !errors.Is(err, ErrBusy) {
		t.Fatal("owner status ignored a failed worker verification")
	}
	if ready, err := b.HebrewStatus(nil); ready || !errors.Is(err, ErrInvalidRequest) {
		t.Fatal("nil caller context accepted")
	}
}

func TestHebrewStatusHonorsCancellationAndCloseDuringProbe(t *testing.T) {
	for _, stop := range []string{"caller", "backend"} {
		t.Run(stop, func(t *testing.T) {
			f := newStatusService(t, hebrewStatusRecipe())
			b := f.backend(t, "he")
			f.blocked.Store(true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				ready, err := b.HebrewStatus(ctx)
				if ready {
					done <- errors.New("cancelled status claimed readiness")
					return
				}
				done <- err
			}()
			select {
			case <-f.entered:
			case <-time.After(time.Second):
				t.Fatal("status did not start the owned probe")
			}
			want := error(context.Canceled)
			if stop == "caller" {
				cancel()
			} else {
				_ = b.Close(context.Background())
				want = ErrStopped
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatal("probe lost its cancellation boundary", err)
				}
			case <-time.After(time.Second):
				t.Fatal("probe retained a cancelled runtime")
			}
		})
	}
}

func TestHebrewStatusBoundsProbeAndAdmissionWait(t *testing.T) {
	f := newStatusService(t, hebrewStatusRecipe())
	b := f.backend(t, "he")
	f.blocked.Store(true)
	first, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = b.HebrewStatus(first); close(done) }()
	select {
	case <-f.entered:
	case <-time.After(time.Second):
		t.Fatal("first probe did not start")
	}
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	started := time.Now()
	ready, err := b.HebrewStatus(ctx)
	if ready || !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second || f.calls.Load() != 2 {
		t.Fatal("queued status ignored the caller deadline or opened another probe")
	}
	cancel()
	<-done
	started = time.Now()
	ready, err = b.HebrewStatus(context.Background())
	if ready || !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 4*time.Second {
		t.Fatal("status did not impose its own finite deadline")
	}
}
