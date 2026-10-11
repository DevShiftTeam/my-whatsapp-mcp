package inference

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

// A deterministic backend replaces only the native-library boundary; the real
// worker parsing, dispatch, deadline, output validation and teardown are tested.
type testWorkerBackend struct {
	query    func(context.Context, string) ([]float32, error)
	passages func(context.Context, []string) ([][]float32, error)
	count    func(context.Context, string) (int, error)
	split    func(context.Context, string, int) ([]string, error)
	closed   bool
}

func (backend *testWorkerBackend) EncodeQuery(ctx context.Context, text string) ([]float32, error) {
	if backend.query != nil {
		return backend.query(ctx, text)
	}
	vector := make([]float32, 384)
	vector[0] = 1
	return vector, nil
}
func (backend *testWorkerBackend) EncodePassages(ctx context.Context, texts []string) ([][]float32, error) {
	if backend.passages != nil {
		return backend.passages(ctx, texts)
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vectors[i], _ = backend.EncodeQuery(ctx, text)
	}
	return vectors, nil
}
func (backend *testWorkerBackend) Count(ctx context.Context, text string) (int, error) {
	if backend.count != nil {
		return backend.count(ctx, text)
	}
	return len([]rune(text)) + 2, nil
}
func (backend *testWorkerBackend) Split(ctx context.Context, text string, max int) ([]string, error) {
	if backend.split != nil {
		return backend.split(ctx, text, max)
	}
	if text == "" {
		return nil, nil
	}
	return []string{text}, nil
}
func (backend *testWorkerBackend) Close() error { backend.closed = true; return nil }

func workerFrames(t *testing.T, requests ...Request) *bytes.Buffer {
	t.Helper()
	var input bytes.Buffer
	for _, request := range requests {
		if err := WriteRequest(&input, request); err != nil {
			t.Fatal(err)
		}
	}
	return &input
}
func initRequest() Request {
	return Request{ID: 1, Op: OpInit, DeadlineUnixNano: time.Now().Add(time.Second).UnixNano(), Init: &WorkerInit{Profile: ReferenceProfile(), Paths: ComponentPaths{Model: "/dev/fd/5", Tokenizer: "/dev/fd/6", Runtime: "/dev/fd/7"}}}
}
func operationRequest(id uint64, op, text string) Request {
	return Request{ID: id, Op: op, DeadlineUnixNano: time.Now().Add(time.Second).UnixNano(), Text: text}
}
func workerResponses(t *testing.T, buffer *bytes.Buffer) []Response {
	t.Helper()
	var responses []Response
	for {
		response, err := ReadResponse(buffer)
		if errors.Is(err, io.EOF) {
			return responses
		}
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, response)
	}
}

func TestWorkerRequiresInitializationAndClosesBackend(t *testing.T) {
	var output bytes.Buffer
	err := RunWorker(context.Background(), workerFrames(t, operationRequest(1, OpQuery, "private")), &output, nil)
	if !errors.Is(err, ErrProtocol) || output.Len() != 0 {
		t.Fatalf("uninitialized worker: %v bytes=%d", err, output.Len())
	}
	backend := &testWorkerBackend{}
	err = RunWorker(context.Background(), workerFrames(t, initRequest(), operationRequest(2, OpQuery, "שלום"), Request{ID: 3, Op: OpClose}), &output, func(context.Context, Profile, ComponentPaths) (Backend, error) { return backend, nil })
	if err != nil || !backend.closed {
		t.Fatalf("worker close: %v closed=%v", err, backend.closed)
	}
	responses := workerResponses(t, &output)
	if len(responses) != 3 || responses[0].Code != CodeOK || len(responses[1].Vectors) != 1 || responses[1].Vectors[0][0] != 1 || responses[2].Op != OpClose {
		t.Fatalf("responses: %+v", responses)
	}
}

func TestWorkerReturnsOnlyFixedCodesForBackendAndVectorFailures(t *testing.T) {
	for name, backend := range map[string]*testWorkerBackend{
		"native error":    {query: func(context.Context, string) ([]float32, error) { return nil, errors.New("secret WhatsApp message") }},
		"wrong dimension": {query: func(context.Context, string) ([]float32, error) { return []float32{1}, nil }},
		"nonfinite":       {query: func(context.Context, string) ([]float32, error) { return nonfiniteVector(float32(math.NaN())), nil }},
		"panic":           {query: func(context.Context, string) ([]float32, error) { panic("secret WhatsApp message") }},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			err := RunWorker(context.Background(), workerFrames(t, initRequest(), operationRequest(2, OpQuery, "private text")), &output, func(context.Context, Profile, ComponentPaths) (Backend, error) { return backend, nil })
			if err != nil || !backend.closed {
				t.Fatalf("worker: %v closed=%v", err, backend.closed)
			}
			if strings.Contains(output.String(), "secret") || strings.Contains(output.String(), "private") {
				t.Fatal("private content leaked in error output")
			}
			responses := workerResponses(t, &output)
			if len(responses) != 2 || responses[1].Code != CodeBackend || len(responses[1].Vectors) != 0 {
				t.Fatalf("responses: %+v", responses)
			}
		})
	}
}

func TestWorkerBackendTypedCodes(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
		code string
	}{
		{"limit", ErrLimit, CodeLimit},
		{"unavailable", ErrUnavailable, CodeUnavailable},
		{"artifact", ErrArtifact, CodeUnavailable},
		{"closed", ErrClosed, CodeUnavailable},
		{"protocol", ErrProtocol, CodeInvalid},
		{"arbitrary", errors.New("secret WhatsApp message"), CodeBackend},
	} {
		for _, operation := range []string{OpQuery, OpPassages, OpCount, OpSplit, "split_count"} {
			t.Run(failure.name+"/"+operation, func(t *testing.T) {
				backendError := fmt.Errorf("secret WhatsApp message: %w", failure.err)
				backend := &testWorkerBackend{}
				request := operationRequest(2, operation, "private text")
				switch operation {
				case OpQuery:
					backend.query = func(context.Context, string) ([]float32, error) { return nil, backendError }
				case OpPassages:
					request.Text, request.Texts = "", []string{"private text"}
					backend.passages = func(context.Context, []string) ([][]float32, error) { return nil, backendError }
				case OpCount:
					backend.count = func(context.Context, string) (int, error) { return 0, backendError }
				case OpSplit:
					request.MaxTokens = 256
					backend.split = func(context.Context, string, int) ([]string, error) { return nil, backendError }
				case "split_count":
					request.Op, request.MaxTokens = OpSplit, 256
					backend.count = func(context.Context, string) (int, error) { return 0, backendError }
				}
				var output bytes.Buffer
				if err := RunWorker(context.Background(), workerFrames(t, initRequest(), request), &output, func(context.Context, Profile, ComponentPaths) (Backend, error) { return backend, nil }); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(output.String(), "secret") || strings.Contains(output.String(), "private") {
					t.Fatal("backend error content leaked into the response")
				}
				responses := workerResponses(t, &output)
				if len(responses) != 2 || responses[1].Code != failure.code || len(responses[1].Vectors) != 0 || len(responses[1].Chunks) != 0 || len(responses[1].Counts) != 0 || responses[1].Count != 0 {
					t.Fatalf("got %+v want code %s with no result payload", responses, failure.code)
				}
			})
		}
	}
}

func TestWorkerDeadlineOverridesTypedBackendError(t *testing.T) {
	backend := &testWorkerBackend{query: func(ctx context.Context, _ string) ([]float32, error) {
		<-ctx.Done()
		return nil, ErrLimit
	}}
	request := operationRequest(2, OpQuery, "private text")
	request.DeadlineUnixNano = time.Now().Add(20 * time.Millisecond).UnixNano()
	var output bytes.Buffer
	if err := RunWorker(context.Background(), workerFrames(t, initRequest(), request), &output, func(context.Context, Profile, ComponentPaths) (Backend, error) { return backend, nil }); err != nil {
		t.Fatal(err)
	}
	responses := workerResponses(t, &output)
	if len(responses) != 2 || responses[1].Code != CodeDeadline {
		t.Fatalf("deadline result: %+v", responses)
	}
}

func TestWorkerEnforcesRequestCapAndIncreasingIDs(t *testing.T) {
	backend := &testWorkerBackend{}
	requests := []Request{initRequest()}
	for id := uint64(2); id <= 258; id++ {
		requests = append(requests, operationRequest(id, OpCount, "x"))
	}
	var output bytes.Buffer
	err := RunWorker(context.Background(), workerFrames(t, requests...), &output, func(context.Context, Profile, ComponentPaths) (Backend, error) { return backend, nil })
	if err != nil || !backend.closed {
		t.Fatalf("worker: %v closed=%v", err, backend.closed)
	}
	responses := workerResponses(t, &output)
	if len(responses) != 258 || responses[len(responses)-1].Code != CodeLimit {
		t.Fatalf("request cap: count=%d last=%+v", len(responses), responses[len(responses)-1])
	}
	output.Reset()
	err = RunWorker(context.Background(), workerFrames(t, initRequest(), operationRequest(1, OpCount, "x")), &output, func(context.Context, Profile, ComponentPaths) (Backend, error) { return &testWorkerBackend{}, nil })
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("replayed ID: %v", err)
	}
}

func TestWorkerEnforcesDeadlinesBeforeAndAfterBackend(t *testing.T) {
	for name, request := range map[string]Request{
		"expired": {ID: 2, Op: OpQuery, Text: "private", DeadlineUnixNano: time.Now().Add(-time.Second).UnixNano()},
		"too far": {ID: 2, Op: OpQuery, Text: "private", DeadlineUnixNano: time.Now().Add(time.Minute).UnixNano()},
	} {
		t.Run(name, func(t *testing.T) {
			backend := &testWorkerBackend{query: func(context.Context, string) ([]float32, error) {
				t.Fatal("backend called for invalid deadline")
				return nil, nil
			}}
			var output bytes.Buffer
			if err := RunWorker(context.Background(), workerFrames(t, initRequest(), request), &output, func(context.Context, Profile, ComponentPaths) (Backend, error) { return backend, nil }); err != nil {
				t.Fatal(err)
			}
			responses := workerResponses(t, &output)
			want := CodeDeadline
			if name == "too far" {
				want = CodeInvalid
			}
			if responses[1].Code != want {
				t.Fatalf("got %s want %s", responses[1].Code, want)
			}
		})
	}
	backend := &testWorkerBackend{query: func(ctx context.Context, _ string) ([]float32, error) { <-ctx.Done(); return make([]float32, 384), nil }}
	request := operationRequest(2, OpQuery, "private")
	request.DeadlineUnixNano = time.Now().Add(20 * time.Millisecond).UnixNano()
	var output bytes.Buffer
	if err := RunWorker(context.Background(), workerFrames(t, initRequest(), request), &output, func(context.Context, Profile, ComponentPaths) (Backend, error) { return backend, nil }); err != nil {
		t.Fatal(err)
	}
	if responses := workerResponses(t, &output); responses[1].Code != CodeDeadline {
		t.Fatalf("deadline result: %+v", responses[1])
	}
}

func TestWorkerVerifiesSplitReconstructionAndTokenCounts(t *testing.T) {
	for name, chunks := range map[string][]string{"valid": {"שלום ", "hello"}, "dropped": {"שלום "}, "too many tokens": {"שלום hello"}, "invalid UTF8": {"\xff"}} {
		t.Run(name, func(t *testing.T) {
			backend := &testWorkerBackend{split: func(context.Context, string, int) ([]string, error) { return chunks, nil }}
			request := operationRequest(2, OpSplit, "שלום hello")
			request.MaxTokens = 8
			var output bytes.Buffer
			if err := RunWorker(context.Background(), workerFrames(t, initRequest(), request), &output, func(context.Context, Profile, ComponentPaths) (Backend, error) { return backend, nil }); err != nil {
				t.Fatal(err)
			}
			response := workerResponses(t, &output)[1]
			if name == "valid" {
				if response.Code != CodeOK || len(response.Counts) != 2 || response.Counts[0] != 7 || response.Counts[1] != 7 {
					t.Fatalf("valid split: %+v", response)
				}
			} else if response.Code != CodeBackend {
				t.Fatalf("bad split: %+v", response)
			}
		})
	}
}
