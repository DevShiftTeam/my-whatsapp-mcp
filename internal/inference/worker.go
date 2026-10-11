package inference

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

// WorkerFactory receives a catalog-validated profile. The executable further
// restricts paths to readonly inherited component descriptors.
type WorkerFactory func(context.Context, Profile, ComponentPaths) (Backend, error)

// RunWorker uses only private inherited pipes. It neither logs nor returns
// backend errors, which may contain message text. Native calls remain serial;
// the supervising process enforces hard timeouts by killing the process group
// if a native call ignores its context, avoiding concurrent native destruction.
func RunWorker(ctx context.Context, input io.Reader, output io.Writer, factory WorkerFactory) (result error) {
	request, err := ReadRequest(input)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil || request.Op != OpInit {
		return ErrProtocol
	}
	requestContext, cancel, code := workerRequestContext(ctx, request.DeadlineUnixNano)
	if code != CodeOK {
		return WriteResponse(output, Response{ID: request.ID, Op: request.Op, Code: code})
	}
	defer cancel()
	if request.Init.Profile.Validate() != nil {
		return WriteResponse(output, Response{ID: request.ID, Op: request.Op, Code: CodeInvalid})
	}
	backend, initErr := createWorkerBackend(requestContext, request.Init, factory)
	if backend != nil {
		defer func() {
			if err := closeWorkerBackend(backend); result == nil && err != nil {
				result = ErrWorkerFailed
			}
		}()
	}
	code = CodeOK
	if requestContext.Err() != nil {
		code = CodeDeadline
	} else if initErr != nil || backend == nil {
		code = CodeUnavailable
	}
	if err := WriteResponse(output, Response{ID: request.ID, Op: request.Op, Code: code}); err != nil {
		return ErrWorkerFailed
	}
	if code != CodeOK {
		return nil
	}
	cancel()
	lastID := request.ID
	requests := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		request, err = ReadRequest(input)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil || request.ID <= lastID || request.Op == OpInit {
			return ErrProtocol
		}
		lastID = request.ID
		if request.Op == OpClose {
			return WriteResponse(output, Response{ID: request.ID, Op: request.Op, Code: CodeOK})
		}
		if requests >= MaxWorkerRequests {
			return WriteResponse(output, Response{ID: request.ID, Op: request.Op, Code: CodeLimit})
		}
		requests++
		requestContext, cancel, code = workerRequestContext(ctx, request.DeadlineUnixNano)
		response := Response{ID: request.ID, Op: request.Op, Code: code}
		if code == CodeOK {
			response = executeWorkerRequest(requestContext, backend, request)
		}
		cancel()
		if err := WriteResponse(output, response); err != nil {
			return ErrWorkerFailed
		}
	}
}

func workerRequestContext(ctx context.Context, deadline int64) (context.Context, context.CancelFunc, string) {
	when := time.Unix(0, deadline)
	remaining := time.Until(when)
	if remaining <= 0 || ctx.Err() != nil {
		return ctx, func() {}, CodeDeadline
	}
	if remaining > MaxRequestDuration {
		return ctx, func() {}, CodeInvalid
	}
	requestContext, cancel := context.WithDeadline(ctx, when)
	return requestContext, cancel, CodeOK
}

func createWorkerBackend(ctx context.Context, init *WorkerInit, factory WorkerFactory) (backend Backend, err error) {
	defer func() {
		if recover() != nil {
			backend = nil
			err = ErrUnavailable
		}
	}()
	if factory == nil {
		return nil, ErrUnavailable
	}
	backend, err = factory(ctx, init.Profile, init.Paths)
	if err != nil {
		return backend, ErrUnavailable
	}
	return backend, nil
}

func closeWorkerBackend(backend Backend) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrWorkerFailed
		}
	}()
	if backend.Close() != nil {
		return ErrWorkerFailed
	}
	return nil
}

func executeWorkerRequest(ctx context.Context, backend Backend, request Request) (response Response) {
	response = Response{ID: request.ID, Op: request.Op, Code: CodeBackend}
	defer func() {
		if recover() != nil {
			response = Response{ID: request.ID, Op: request.Op, Code: CodeBackend}
		}
		if ctx.Err() != nil {
			response = Response{ID: request.ID, Op: request.Op, Code: CodeDeadline}
		}
	}()
	var err error
	switch request.Op {
	case OpQuery:
		var vector []float32
		vector, err = backend.EncodeQuery(ctx, request.Text)
		response.Vectors = [][]float32{vector}
	case OpPassages:
		response.Vectors, err = backend.EncodePassages(ctx, request.Texts)
		if err == nil && len(response.Vectors) != len(request.Texts) {
			err = ErrWorkerFailed
		}
	case OpCount:
		response.Count, err = backend.Count(ctx, request.Text)
	case OpSplit:
		response.Chunks, err = backend.Split(ctx, request.Text, request.MaxTokens)
		if err == nil {
			err = validateWorkerSplit(ctx, backend, request, &response)
		}
	default:
		err = ErrProtocol
	}
	if err != nil {
		return Response{ID: request.ID, Op: request.Op, Code: workerBackendCode(err)}
	}
	response.Code = CodeOK
	if validateResponse(response) != nil {
		return Response{ID: request.ID, Op: request.Op, Code: CodeBackend}
	}
	return response
}

func workerBackendCode(err error) string {
	switch {
	case errors.Is(err, ErrLimit):
		return CodeLimit
	case errors.Is(err, ErrUnavailable), errors.Is(err, ErrArtifact), errors.Is(err, ErrClosed):
		return CodeUnavailable
	case errors.Is(err, ErrProtocol):
		return CodeInvalid
	default:
		return CodeBackend
	}
}

func validateWorkerSplit(ctx context.Context, backend Backend, request Request, response *Response) error {
	if len(response.Chunks) > MaxSplitChunks {
		return ErrWorkerFailed
	}
	total := 0
	for _, chunk := range response.Chunks {
		if chunk == "" || !boundedText(chunk) {
			return ErrWorkerFailed
		}
		total += len(chunk)
		if total > MaxTextBytes {
			return ErrWorkerFailed
		}
	}
	if strings.Join(response.Chunks, "") != request.Text {
		return ErrWorkerFailed
	}
	response.Counts = make([]int, len(response.Chunks))
	for i, chunk := range response.Chunks {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		count, err := backend.Count(ctx, chunk)
		if err != nil {
			return err
		}
		if count < 1 || count > request.MaxTokens {
			return ErrWorkerFailed
		}
		response.Counts[i] = count
	}
	return nil
}
