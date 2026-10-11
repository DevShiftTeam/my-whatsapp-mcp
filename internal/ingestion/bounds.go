package ingestion

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"reflect"
	"sync"

	"go.mau.fi/whatsmeow"
)

var (
	errLimitExceeded  = errors.New("history_limit_exceeded")
	errHistoryCorrupt = errors.New("history_corrupt")
	errInvalidLimit   = errors.New("invalid_limit")
)

const maxReadBytes int64 = 256 << 20

// Authenticated media uses a full CBC padding block at an exact block boundary
// and a ten-byte MAC. Plaintext is independently capped at the runtime limit.
func mediaCiphertextMaximum(plain int64) (int64, error) {
	if plain <= 0 || plain > 64<<20 {
		return 0, errInvalidLimit
	}
	return (plain/16+1)*16 + 10, nil
}

func validReadLimit(max int64) bool { return max > 0 && max <= maxReadBytes }

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return n, contextErr
	}
	return n, err
}

// readCapped reads one oversize sentinel at most and discards partial output on
// failure. Cancellation during a blocking read requires support from the reader.
// Cleanup clears the current allocation; io.ReadAll's prior growth allocations
// remain managed by the runtime. History's fixed file array is cleared on close.
func readCapped(ctx context.Context, reader io.Reader, max int64) ([]byte, error) {
	if ctx == nil || reader == nil || !validReadLimit(max) {
		return nil, errInvalidLimit
	}
	if value := reflect.ValueOf(reader); value.Kind() == reflect.Pointer && value.IsNil() {
		return nil, errInvalidLimit
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, reader}, max+1))
	if err != nil {
		clear(data)
		return nil, err
	}
	if int64(len(data)) > max {
		clear(data)
		return nil, errLimitExceeded
	}
	return data, nil
}

// decodeHistory independently limits the compressed stream and decoded bytes.
// These limits do not establish an aggregate RSS bound or parse a protobuf graph.
func decodeHistory(ctx context.Context, reader io.Reader, compressedMax, decodedMax int64) ([]byte, error) {
	if !validReadLimit(compressedMax) || !validReadLimit(decodedMax) {
		return nil, errInvalidLimit
	}
	compressed, err := readCapped(ctx, reader, compressedMax)
	if err != nil {
		return nil, err
	}
	defer clear(compressed)
	// Check cancellation as flate refills its input, including streams containing
	// long runs of empty blocks that produce no decoded bytes.
	z, err := zlib.NewReader(contextReader{ctx, bytes.NewReader(compressed)})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", errHistoryCorrupt, err)
	}
	defer z.Close()
	decoded, err := readCapped(ctx, z, decodedMax)
	if err != nil {
		if errors.Is(err, errLimitExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", errHistoryCorrupt, err)
	}
	return decoded, nil
}

// capTransport rejects both advertised and actual ciphertext byte counts.
type capTransport struct {
	base http.RoundTripper
	max  int64
}

func (t *capTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || t.max <= 0 {
		return nil, errInvalidLimit
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	// Disable Go's transparent HTTP gzip decoding so the cap observes the actual
	// ciphertext body. Clone headers to preserve the caller's request.
	wireReq := req.Clone(req.Context())
	if wireReq.Header == nil {
		wireReq.Header = make(http.Header)
	}
	wireReq.Header.Set("Accept-Encoding", "identity")
	response, err := base.RoundTrip(wireReq)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > t.max {
		_ = response.Body.Close()
		return nil, errLimitExceeded
	}
	response.Body = &capBody{ReadCloser: response.Body, ctx: req.Context(), left: t.max}
	return response, nil
}

type capBody struct {
	io.ReadCloser
	ctx  context.Context
	left int64
	err  error
}

func (b *capBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if b.err != nil {
		return 0, b.err
	}
	if b.left == 0 {
		var sentinel [1]byte
		n, err := (contextReader{b.ctx, b.ReadCloser}).Read(sentinel[:])
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if n > 0 {
			b.err = errLimitExceeded
			return 0, b.err
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := (contextReader{b.ctx, b.ReadCloser}).Read(p)
	b.left -= int64(n)
	return n, err
}

// cappedFile hides the descriptor from connector preallocation. Its cap limits
// extent, allowing ciphertext to be overwritten during in-place decryption.
// It borrows the file; the file owner retains close and cleanup responsibility.
type cappedFile struct {
	mu   sync.Mutex
	ctx  context.Context
	file whatsmeow.File
	max  int64
}

var _ whatsmeow.File = (*cappedFile)(nil)

func newCappedFile(ctx context.Context, file whatsmeow.File, max int64) (whatsmeow.File, error) {
	if ctx == nil || nilMediaPort(file) || max <= 0 {
		return nil, errInvalidLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if nilMediaPort(stat) {
		return nil, errInvalidLimit
	}
	if !stat.Mode().IsRegular() {
		return nil, errInvalidLimit
	}
	if stat.Size() > max {
		return nil, errLimitExceeded
	}
	return &cappedFile{ctx: ctx, file: file, max: max}, nil
}

func (f *cappedFile) permits(offset int64, length int) error {
	if err := f.ctx.Err(); err != nil {
		return err
	}
	if offset < 0 {
		return errInvalidLimit
	}
	if offset > f.max || int64(length) > f.max-offset {
		return errLimitExceeded
	}
	return nil
}

func (f *cappedFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	return f.file.Read(p)
}

func (f *cappedFile) ReadAt(p []byte, offset int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	return f.file.ReadAt(p, offset)
}

func (f *cappedFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	offset, err := f.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if err := f.permits(offset, len(p)); err != nil {
		return 0, err
	}
	// WriteAt rejects O_APPEND handles instead of bypassing the checked offset.
	n, err := f.file.WriteAt(p, offset)
	if n > 0 {
		if _, seekErr := f.file.Seek(offset+int64(n), io.SeekStart); err == nil {
			err = seekErr
		}
	}
	return n, err
}

func (f *cappedFile) WriteAt(p []byte, offset int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.permits(offset, len(p)); err != nil {
		return 0, err
	}
	return f.file.WriteAt(p, offset)
}

func (f *cappedFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		var err error
		base, err = f.file.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, err
		}
	case io.SeekEnd:
		stat, err := f.file.Stat()
		if err != nil {
			return 0, err
		}
		base = stat.Size()
	default:
		return 0, errInvalidLimit
	}
	if offset > 0 && base > math.MaxInt64-offset {
		return 0, errLimitExceeded
	}
	if offset < 0 && offset < -base {
		return 0, errInvalidLimit
	}
	target := base + offset
	if err := f.permits(target, 0); err != nil {
		return 0, err
	}
	return f.file.Seek(target, io.SeekStart)
}

func (f *cappedFile) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.permits(size, 0); err != nil {
		return err
	}
	return f.file.Truncate(size)
}

func (f *cappedFile) Stat() (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.file.Stat()
}
