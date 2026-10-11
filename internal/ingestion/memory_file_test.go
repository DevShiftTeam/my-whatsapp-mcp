package ingestion

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"testing"

	"go.mau.fi/whatsmeow"
)

func TestMemoryFileRandomAccessBoundaries(t *testing.T) {
	f, err := newMemoryFile(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var contract whatsmeow.File = f
	if n, err := contract.WriteAt([]byte("x"), 3); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	p := make([]byte, 4)
	if n, err := contract.ReadAt(p, 0); n != 4 || err != nil || !bytes.Equal(p, []byte{0, 0, 0, 'x'}) {
		t.Fatal(n, p, err)
	}
	for _, offset := range []int64{-1, 4, math.MaxInt64} {
		if n, err := contract.WriteAt([]byte("x"), offset); n != 0 || !errors.Is(err, errLimitExceeded) {
			t.Fatalf("uncapped random write: offset=%d n=%d err=%v", offset, n, err)
		}
	}
	if n, err := contract.ReadAt(p, -1); n != 0 || err == nil {
		t.Fatal(n, err)
	}
	if n, err := contract.ReadAt(p, 3); n != 1 || !errors.Is(err, io.EOF) || p[0] != 'x' {
		t.Fatal(n, p, err)
	}
	if _, err := contract.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if n, err := contract.Write([]byte("1234")); n != 4 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := contract.Write([]byte("5")); n != 0 || !errors.Is(err, errLimitExceeded) {
		t.Fatal(n, err)
	}
	for _, offset := range []int64{-1, 5, math.MaxInt64} {
		if _, err := contract.Seek(offset, io.SeekStart); !errors.Is(err, errLimitExceeded) {
			t.Fatalf("seek accepted %d: %v", offset, err)
		}
	}
	if _, err := contract.Seek(math.MaxInt64, io.SeekCurrent); !errors.Is(err, errLimitExceeded) {
		t.Fatal(err)
	}
	if _, err := contract.Seek(0, 99); err == nil {
		t.Fatal("invalid whence accepted")
	}
	if offset, err := contract.Seek(-1, io.SeekEnd); offset != 3 || err != nil {
		t.Fatal(offset, err)
	}
	stat, err := contract.Stat()
	if err != nil || stat.Size() != 4 || !stat.Mode().IsRegular() || stat.Mode().Perm() != 0600 {
		t.Fatalf("stat=%v err=%v", stat, err)
	}
}

func TestMemoryFileTruncateAndCloseErasePlaintext(t *testing.T) {
	f, err := newMemoryFile(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("private")); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{-1, 9, math.MaxInt64} {
		if err := f.Truncate(size); !errors.Is(err, errLimitExceeded) {
			t.Fatalf("truncate accepted %d: %v", size, err)
		}
	}
	if err := f.Truncate(2); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(7); err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 7)
	if n, err := f.ReadAt(p, 0); n != 7 || err != nil || !bytes.Equal(p, []byte{'p', 'r', 0, 0, 0, 0, 0}) {
		t.Fatalf("truncated plaintext revived: %q %v", p, err)
	}
	backing := f.data
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backing, make([]byte, 8)) {
		t.Fatal("closed backing array retained plaintext")
	}
	if _, err := f.ReadAt(p, 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed staging readable: %v", err)
	}
	if _, err := f.Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed staging writable: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
}

func TestMemoryFileLimitsAndCancellation(t *testing.T) {
	for _, max := range []int64{0, -1, math.MaxInt64, (256 << 20) + 1} {
		if f, err := newMemoryFile(context.Background(), max); f != nil || !errors.Is(err, errInvalidLimit) {
			t.Fatalf("invalid cap %d: file=%v err=%v", max, f, err)
		}
	}
	if f, err := newMemoryFile(nil, 4); f != nil || !errors.Is(err, errInvalidLimit) {
		t.Fatalf("nil context accepted: file=%v err=%v", f, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if f, err := newMemoryFile(ctx, 4); f != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled allocation: file=%v err=%v", f, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	f, err := newMemoryFile(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cancel()
	if n, err := f.WriteAt([]byte("x"), 0); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal(n, err)
	}
	if n, err := f.Read(make([]byte, 1)); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal(n, err)
	}
	if _, err := f.Seek(0, io.SeekStart); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := f.Truncate(1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := f.Stat(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestMemoryFilePositionalIOPreservesCursor(t *testing.T) {
	f, err := newMemoryFile(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if n, err := f.WriteAt([]byte("abcd"), 0); n != 4 || err != nil {
		t.Fatal(n, err)
	}
	if offset, err := f.Seek(0, io.SeekCurrent); offset != 0 || err != nil {
		t.Fatalf("WriteAt moved cursor: offset=%d err=%v", offset, err)
	}
	p := make([]byte, 1)
	if n, err := f.ReadAt(p, 3); n != 1 || err != nil || p[0] != 'd' {
		t.Fatal(n, p, err)
	}
	if n, err := f.Read(p); n != 1 || err != nil || p[0] != 'a' {
		t.Fatalf("ReadAt moved cursor: n=%d p=%q err=%v", n, p, err)
	}
	if offset, err := f.Seek(4, io.SeekStart); offset != 4 || err != nil {
		t.Fatal(offset, err)
	}
	if n, err := f.Read(nil); n != 0 || err != nil {
		t.Fatalf("empty read at limit: n=%d err=%v", n, err)
	}
	if n, err := f.Write(nil); n != 0 || err != nil {
		t.Fatalf("empty write at limit: n=%d err=%v", n, err)
	}
}
