package ingestion

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

type measuredReader struct {
	reader io.Reader
	read   int
}

func (r *measuredReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

type cancellingReader struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r cancellingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.cancel()
	return n, err
}

func TestReadCappedBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		max               int64
		wantErr           error
		consumed          int
	}{
		{"empty", "", "", 4, nil, 0},
		{"exact", "1234", "1234", 4, nil, 4},
		{"sentinel", "123456789", "", 4, errLimitExceeded, 5},
		{"zero", "abc", "", 0, errInvalidLimit, 0},
		{"negative", "abc", "", -1, errInvalidLimit, 0},
		{"overflow", "abc", "", math.MaxInt64, errInvalidLimit, 0},
		{"too_large", "abc", "", (256 << 20) + 1, errInvalidLimit, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &measuredReader{reader: bytes.NewBufferString(tc.input)}
			got, err := readCapped(context.Background(), r, tc.max)
			if string(got) != tc.want || !errors.Is(err, tc.wantErr) || r.read != tc.consumed {
				t.Fatalf("got=%q err=%v consumed=%d", got, err, r.read)
			}
		})
	}
	var nilReader *bytes.Reader
	for _, reader := range []io.Reader{nil, nilReader} {
		if got, err := readCapped(context.Background(), reader, 4); got != nil || !errors.Is(err, errInvalidLimit) {
			t.Fatalf("nil reader accepted: %q %v", got, err)
		}
	}
}

func TestReadCappedCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &measuredReader{reader: bytes.NewBufferString("private")}
	if got, err := readCapped(ctx, r, 8); got != nil || !errors.Is(err, context.Canceled) || r.read != 0 {
		t.Fatalf("pre-cancelled read: %q %v consumed=%d", got, err, r.read)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	if got, err := readCapped(ctx, cancellingReader{bytes.NewBufferString("abc"), cancel}, 4); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during read leaked data: %q %v", got, err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReadCappedDiscardsFailures(t *testing.T) {
	if got, err := readCapped(context.Background(), failingReader{}, 4); got != nil || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("reader failure: %q %v", got, err)
	}
	if got, err := readCapped(nil, bytes.NewBufferString("private"), 8); got != nil || !errors.Is(err, errInvalidLimit) {
		t.Fatalf("nil context accepted: %q %v", got, err)
	}
}

// retainedReader exposes the destination only to inspect cleanup after the read.
// It does not change the buffer or participate in subsequent reads.
type retainedReader struct {
	reader   io.Reader
	fail     bool
	retained []byte
}

func (r *retainedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 && r.retained == nil {
		r.retained = p[:n]
	}
	if r.fail && n > 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestReadCappedClearsFailedBuffers(t *testing.T) {
	for _, fail := range []bool{false, true} {
		r := &retainedReader{reader: bytes.NewBufferString("private"), fail: fail}
		got, err := readCapped(context.Background(), r, 4)
		if got != nil || err == nil {
			t.Fatalf("failure returned data: %q %v", got, err)
		}
		if !bytes.Equal(r.retained, make([]byte, len(r.retained))) {
			t.Fatalf("failed buffer retained plaintext: %q", r.retained)
		}
	}
}

func TestDecodeHistoryClearsCompressedStagingCopy(t *testing.T) {
	compressed := historyFixture(t, []byte("private"))
	r := &retainedReader{reader: bytes.NewReader(compressed)}
	got, err := decodeHistory(context.Background(), r, 128, 128)
	if err != nil || string(got) != "private" {
		t.Fatalf("decode: %q %v", got, err)
	}
	if !bytes.Equal(r.retained, make([]byte, len(r.retained))) {
		t.Fatal("completed decode retained compressed plaintext copy")
	}
}

func historyFixture(t *testing.T, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	w := zlib.NewWriter(&output)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestDecodeHistoryIndependentLimits(t *testing.T) {
	compressed := historyFixture(t, bytes.Repeat([]byte("x"), 10000))
	got, err := decodeHistory(context.Background(), bytes.NewReader(compressed), 1024, 100)
	if got != nil || !errors.Is(err, errLimitExceeded) {
		t.Fatalf("expanded beyond decoded cap: len=%d err=%v", len(got), err)
	}
	r := &measuredReader{reader: bytes.NewReader(compressed)}
	if got, err := decodeHistory(context.Background(), r, 4, 10000); got != nil || !errors.Is(err, errLimitExceeded) || r.read != 5 {
		t.Fatalf("compressed cap: %q %v consumed=%d", got, err, r.read)
	}
	exact := historyFixture(t, []byte("שלום"))
	if got, err := decodeHistory(context.Background(), bytes.NewReader(exact), int64(len(exact)), 8); err != nil || string(got) != "שלום" {
		t.Fatalf("exact limits: %q %v", got, err)
	}
	for _, decodedMax := range []int64{0, -1, math.MaxInt64, (256 << 20) + 1} {
		r := &measuredReader{reader: bytes.NewReader(compressed)}
		if got, err := decodeHistory(context.Background(), r, 1024, decodedMax); got != nil || !errors.Is(err, errInvalidLimit) || r.read != 0 {
			t.Fatalf("invalid decoded cap read input: %q %v consumed=%d", got, err, r.read)
		}
	}
}

func TestDecodeHistoryCorruptionAndCancellation(t *testing.T) {
	valid := historyFixture(t, []byte("private"))
	badChecksum := append([]byte(nil), valid...)
	badChecksum[len(badChecksum)-1] ^= 1
	for _, bad := range [][]byte{[]byte("not-zlib"), valid[:len(valid)-1], badChecksum} {
		if got, err := decodeHistory(context.Background(), bytes.NewReader(bad), 128, 128); got != nil || !errors.Is(err, errHistoryCorrupt) {
			t.Fatalf("corruption accepted: %q %v", got, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &measuredReader{reader: bytes.NewReader(valid)}
	if got, err := decodeHistory(ctx, r, 128, 128); got != nil || !errors.Is(err, context.Canceled) || r.read != 0 {
		t.Fatalf("cancelled decode: %q %v consumed=%d", got, err, r.read)
	}
}

type stagedReader struct {
	reader io.Reader
	staged *bool
}

func (r stagedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if errors.Is(err, io.EOF) {
		*r.staged = true
	}
	return n, err
}

// Cancel at the next phase after the compressed reader's final post-read check.
type stagedCancellation struct {
	context.Context
	cancel  context.CancelFunc
	staged  *bool
	checked bool
}

func (c *stagedCancellation) Err() error {
	if *c.staged {
		if c.checked {
			c.cancel()
		}
		c.checked = true
	}
	return c.Context.Err()
}

func TestDecodeHistoryCancellationDuringZlibHeader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	staged := false
	controlled := &stagedCancellation{Context: ctx, cancel: cancel, staged: &staged}
	r := stagedReader{reader: bytes.NewReader(historyFixture(t, []byte("private"))), staged: &staged}
	if got, err := decodeHistory(controlled, r, 128, 128); got != nil || !errors.Is(err, context.Canceled) || errors.Is(err, errHistoryCorrupt) {
		t.Fatalf("cancellation misclassified as corruption: %q %v", got, err)
	}
}

func TestCapTransportNetworkLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/declared_oversize":
			w.Header().Set("Content-Length", "5")
			_, _ = io.WriteString(w, "12345")
		case "/body_oversize":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "12345")
		default:
			_, _ = io.WriteString(w, "1234")
		}
	}))
	defer server.Close()
	base := http.DefaultTransport.(*http.Transport).Clone()
	defer base.CloseIdleConnections()
	client := &http.Client{Transport: &capTransport{base: base, max: 4}}
	if resp, err := client.Get(server.URL + "/declared_oversize"); !errors.Is(err, errLimitExceeded) || resp != nil {
		t.Fatalf("oversized declared body accepted: resp=%v err=%v", resp, err)
	}
	resp, err := client.Get(server.URL + "/body_oversize")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !errors.Is(err, errLimitExceeded) || len(got) > 4 {
		t.Fatalf("oversized streamed body: %q %v", got, err)
	}
	resp, err = client.Get(server.URL + "/exact")
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(got) != "1234" {
		t.Fatalf("exact network body rejected: %q %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := client.Do(req); !errors.Is(err, context.Canceled) || resp != nil {
		t.Fatalf("cancelled request accepted: resp=%v err=%v", resp, err)
	}
}

func TestCapBodyConsumesOneOversizeSentinel(t *testing.T) {
	r := &measuredReader{reader: bytes.NewBufferString("123456789")}
	body := &capBody{ReadCloser: io.NopCloser(r), ctx: context.Background(), left: 4}
	defer body.Close()
	got, err := io.ReadAll(body)
	if string(got) != "1234" || !errors.Is(err, errLimitExceeded) || r.read != 5 {
		t.Fatalf("cap: %q %v consumed=%d", got, err, r.read)
	}
	for range 3 {
		if n, err := body.Read(make([]byte, 8)); n != 0 || !errors.Is(err, errLimitExceeded) || r.read != 5 {
			t.Fatalf("repeated overflow consumed more: n=%d err=%v consumed=%d", n, err, r.read)
		}
	}
}

func TestCapTransportDisablesTransparentHTTPDecompression(t *testing.T) {
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := io.WriteString(w, "1234"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	encoding := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoding <- r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	defer server.Close()
	base := http.DefaultTransport.(*http.Transport).Clone()
	defer base.CloseIdleConnections()
	transport := &capTransport{base: base, max: 4}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if resp != nil || !errors.Is(err, errLimitExceeded) {
		t.Fatalf("wire body hidden by gzip decoding: resp=%v err=%v", resp, err)
	}
	if got := <-encoding; got != "identity" {
		t.Fatalf("requested HTTP decoding: %q", got)
	}
	if got := req.Header.Get("Accept-Encoding"); got != "" {
		t.Fatalf("caller header mutated: %q", got)
	}
}

func TestCappedFilePreventsGrowth(t *testing.T) {
	raw, err := os.CreateTemp(t.TempDir(), "media")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	f, err := newCappedFile(context.Background(), raw, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, unsafe := f.(*os.File); unsafe {
		t.Fatal("raw file allows connector preallocation")
	}
	if n, err := f.Write([]byte("1234")); n != 4 || err != nil {
		t.Fatal(n, err)
	}
	for _, offset := range []int64{-1, 4, math.MaxInt64} {
		if n, err := f.WriteAt([]byte("x"), offset); n != 0 || err == nil {
			t.Fatalf("uncapped random write: offset=%d n=%d err=%v", offset, n, err)
		}
	}
	if n, err := f.Write([]byte("5")); n != 0 || !errors.Is(err, errLimitExceeded) {
		t.Fatal(n, err)
	}
	for _, size := range []int64{-1, 5, math.MaxInt64} {
		if err := f.Truncate(size); err == nil {
			t.Fatalf("truncate accepted %d", size)
		}
	}
	for _, offset := range []int64{-1, 5, math.MaxInt64} {
		if _, err := f.Seek(offset, io.SeekStart); err == nil {
			t.Fatalf("seek accepted %d", offset)
		}
	}
	if _, err := f.Seek(math.MaxInt64, io.SeekCurrent); err == nil {
		t.Fatal("overflowing relative seek accepted")
	}
	if _, err := f.Seek(0, 99); err == nil {
		t.Fatal("invalid whence accepted")
	}
	stat, err := raw.Stat()
	if err != nil || stat.Size() != 4 {
		t.Fatalf("file grew: stat=%v err=%v", stat, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(f); err != nil || string(got) != "1234" {
		t.Fatal(string(got), err)
	}
}

func TestCappedFileRejectsInvalidAndCancelledInput(t *testing.T) {
	raw, err := os.CreateTemp(t.TempDir(), "media")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, max := range []int64{0, -1} {
		if _, err := newCappedFile(context.Background(), raw, max); !errors.Is(err, errInvalidLimit) {
			t.Fatalf("invalid maximum %d: %v", max, err)
		}
	}
	if _, err := newCappedFile(context.Background(), nil, 4); !errors.Is(err, errInvalidLimit) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f, err := newCappedFile(ctx, raw, 4)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if n, err := f.Write([]byte("private")); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal(n, err)
	}
	stat, _ := raw.Stat()
	if stat.Size() != 0 {
		t.Fatalf("cancelled write grew file: %d", stat.Size())
	}
	if _, err := raw.WriteString("12345"); err != nil {
		t.Fatal(err)
	}
	if _, err := newCappedFile(context.Background(), raw, 4); !errors.Is(err, errLimitExceeded) {
		t.Fatal(err)
	}
}

func TestCappedFileRejectsAppendModeBypass(t *testing.T) {
	path := t.TempDir() + "/media"
	if err := os.WriteFile(path, []byte("1234"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	f, err := newCappedFile(context.Background(), raw, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if n, err := f.Write([]byte("x")); n != 0 || err == nil || errors.Is(err, errLimitExceeded) {
		t.Fatalf("append mode bypassed checked extent: n=%d err=%v", n, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "1234" {
		t.Fatalf("append grew file: %q %v", got, err)
	}
}
