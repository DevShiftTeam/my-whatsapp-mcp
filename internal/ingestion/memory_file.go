package ingestion

import (
	"context"
	"io"
	"os"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
)

// boundedMemoryFile stages ciphertext and decrypted compressed history in one
// fixed array. It never falls back to disk. The allocation cap is not an RSS cap.
type boundedMemoryFile struct {
	mu           sync.Mutex
	ctx          context.Context
	data         []byte
	size, offset int64
	closed       bool
}

var _ whatsmeow.File = (*boundedMemoryFile)(nil)

func newMemoryFile(ctx context.Context, max int64) (*boundedMemoryFile, error) {
	if ctx == nil || !validReadLimit(max) {
		return nil, errInvalidLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &boundedMemoryFile{ctx: ctx, data: make([]byte, int(max))}, nil
}

func (f *boundedMemoryFile) check() error {
	if f.closed {
		return os.ErrClosed
	}
	return f.ctx.Err()
}

func (f *boundedMemoryFile) extent(offset int64, length int) error {
	if offset < 0 || offset > int64(len(f.data)) || int64(length) > int64(len(f.data))-offset {
		return errLimitExceeded
	}
	return nil
}

func (f *boundedMemoryFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return 0, err
	}
	n, err := f.readAt(p, f.offset)
	f.offset += int64(n)
	return n, err
}

func (f *boundedMemoryFile) ReadAt(p []byte, offset int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return 0, err
	}
	return f.readAt(p, offset)
}

func (f *boundedMemoryFile) readAt(p []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errInvalidLimit
	}
	if len(p) == 0 {
		return 0, nil
	}
	if offset >= f.size {
		return 0, io.EOF
	}
	n := copy(p, f.data[offset:f.size])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *boundedMemoryFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return 0, err
	}
	n, err := f.writeAt(p, f.offset)
	f.offset += int64(n)
	return n, err
}

func (f *boundedMemoryFile) WriteAt(p []byte, offset int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return 0, err
	}
	return f.writeAt(p, offset)
}

func (f *boundedMemoryFile) writeAt(p []byte, offset int64) (int, error) {
	if err := f.extent(offset, len(p)); err != nil {
		return 0, err
	}
	n := copy(f.data[offset:], p)
	if end := offset + int64(n); end > f.size {
		f.size = end
	}
	return n, nil
}

func (f *boundedMemoryFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return 0, err
	}
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = f.offset
	case io.SeekEnd:
		base = f.size
	default:
		return 0, errInvalidLimit
	}
	if offset < -base || offset > int64(len(f.data))-base {
		return f.offset, errLimitExceeded
	}
	f.offset = base + offset
	return f.offset, nil
}

func (f *boundedMemoryFile) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return err
	}
	if err := f.extent(size, 0); err != nil {
		return err
	}
	if size < f.size {
		clear(f.data[size:f.size])
	}
	f.size = size
	return nil
}

func (f *boundedMemoryFile) Stat() (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(); err != nil {
		return nil, err
	}
	return memoryFileInfo{size: f.size}, nil
}

func (f *boundedMemoryFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		clear(f.data)
		f.data = nil
		f.size, f.offset = 0, 0
		f.closed = true
	}
	return nil
}

type memoryFileInfo struct{ size int64 }

func (i memoryFileInfo) Name() string       { return "history-memory" }
func (i memoryFileInfo) Size() int64        { return i.size }
func (i memoryFileInfo) Mode() os.FileMode  { return 0600 }
func (i memoryFileInfo) ModTime() time.Time { return time.Time{} }
func (i memoryFileInfo) IsDir() bool        { return false }
func (i memoryFileInfo) Sys() any           { return nil }
