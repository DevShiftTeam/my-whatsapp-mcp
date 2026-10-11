package inference

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

func rawFrame(payload string) []byte {
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)))
	copy(frame[4:], payload)
	return frame
}

func TestReadRequestRejectsMalformedFrames(t *testing.T) {
	tooLarge := make([]byte, 4)
	binary.BigEndian.PutUint32(tooLarge, 256*1024+1)
	for name, frame := range map[string][]byte{
		"zero length":             {0, 0, 0, 0},
		"oversized":               tooLarge,
		"partial header":          {0, 1},
		"truncated body":          {0, 0, 0, 4, '{'},
		"invalid JSON":            rawFrame(`{"id":`),
		"unknown field":           rawFrame(`{"id":1,"op":"close","secret":"private"}`),
		"duplicate field":         rawFrame(`{"id":1,"id":2,"op":"close"}`),
		"case duplicate field":    rawFrame(`{"id":1,"ID":2,"op":"close"}`),
		"Unicode duplicate field": rawFrame(`{"id":1,"op":"passages","texts":["a"],"textſ":["b"],"deadline_unix_nano":1}`),
		"lone high surrogate":     rawFrame(`{"id":1,"op":"count","text":"\ud800","deadline_unix_nano":1}`),
		"lone low surrogate":      rawFrame(`{"id":1,"op":"count","text":"\udc00","deadline_unix_nano":1}`),
		"null text":               rawFrame(`{"id":1,"op":"count","text":null,"deadline_unix_nano":1}`),
		"trailing value":          rawFrame(`{"id":1,"op":"close"} {}`),
		"invalid utf8":            rawFrame("{\"id\":1,\"op\":\"query\",\"text\":\"\xff\",\"deadline_unix_nano\":1}"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ReadRequest(bytes.NewReader(frame))
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("got %v, want protocol rejection", err)
			}
		})
	}
	if _, err := ReadRequest(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream: %v", err)
	}
}

func TestRequestBoundsAndRoundTrip(t *testing.T) {
	deadline := time.Now().Add(time.Second).UnixNano()
	valid := Request{ID: 7, Op: OpPassages, DeadlineUnixNano: deadline, Texts: []string{"שלום", "hello"}}
	var buffer bytes.Buffer
	if err := WriteRequest(&buffer, valid); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRequest(&buffer)
	if err != nil || got.ID != 7 || got.Op != OpPassages || len(got.Texts) != 2 || got.Texts[0] != "שלום" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for name, request := range map[string]Request{
		"zero ID":          {Op: OpQuery, DeadlineUnixNano: deadline, Text: "x"},
		"unknown op":       {ID: 1, Op: "exec", DeadlineUnixNano: deadline},
		"too many items":   {ID: 1, Op: OpPassages, DeadlineUnixNano: deadline, Texts: make([]string, 9)},
		"too much text":    {ID: 1, Op: OpQuery, DeadlineUnixNano: deadline, Text: strings.Repeat("x", 16*1024+1)},
		"missing deadline": {ID: 1, Op: OpQuery, Text: "x"},
		"mixed payload":    {ID: 1, Op: OpQuery, DeadlineUnixNano: deadline, Text: "x", Texts: []string{"y"}},
		"split token cap":  {ID: 1, Op: OpSplit, DeadlineUnixNano: deadline, Text: "x", MaxTokens: 257},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := WriteRequest(&output, request); !errors.Is(err, ErrProtocol) || output.Len() != 0 {
				t.Fatalf("got %v, wrote %d bytes", err, output.Len())
			}
		})
	}
}

func TestResponseRejectsNonfiniteWrongDimensionsAndBounds(t *testing.T) {
	good := make([]float32, 384)
	good[0] = 1
	for name, response := range map[string]Response{
		"short vector":          {ID: 1, Op: OpQuery, Code: CodeOK, Vectors: [][]float32{make([]float32, 383)}},
		"long vector":           {ID: 1, Op: OpQuery, Code: CodeOK, Vectors: [][]float32{make([]float32, 385)}},
		"NaN":                   {ID: 1, Op: OpQuery, Code: CodeOK, Vectors: [][]float32{nonfiniteVector(float32(math.NaN()))}},
		"infinity":              {ID: 1, Op: OpQuery, Code: CodeOK, Vectors: [][]float32{nonfiniteVector(float32(math.Inf(1)))}},
		"batch cap":             {ID: 1, Op: OpPassages, Code: CodeOK, Vectors: make([][]float32, 9)},
		"negative count":        {ID: 1, Op: OpCount, Code: CodeOK, Count: -1},
		"excess count":          {ID: 1, Op: OpCount, Code: CodeOK, Count: MaxTokenCount + 1},
		"chunk cap":             {ID: 1, Op: OpSplit, Code: CodeOK, Chunks: make([]string, 257)},
		"chunk bytes":           {ID: 1, Op: OpSplit, Code: CodeOK, Chunks: []string{strings.Repeat("x", 16*1024+1)}, Counts: []int{1}},
		"aggregate chunk bytes": {ID: 1, Op: OpSplit, Code: CodeOK, Chunks: []string{strings.Repeat("x", 8192), strings.Repeat("y", 8193)}, Counts: []int{1, 1}},
		"missing chunk counts":  {ID: 1, Op: OpSplit, Code: CodeOK, Chunks: []string{"x"}},
		"excess chunk tokens":   {ID: 1, Op: OpSplit, Code: CodeOK, Chunks: []string{"x"}, Counts: []int{257}},
		"error contains result": {ID: 1, Op: OpQuery, Code: CodeBackend, Vectors: [][]float32{good}},
		"arbitrary error":       {ID: 1, Op: OpQuery, Code: "private chat text"},
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if err := WriteResponse(&output, response); !errors.Is(err, ErrProtocol) || output.Len() != 0 {
				t.Fatalf("got %v, wrote %d bytes", err, output.Len())
			}
		})
	}
	var output bytes.Buffer
	if err := WriteResponse(&output, Response{ID: 8, Op: OpQuery, Code: CodeOK, Vectors: [][]float32{good}}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadResponse(&output)
	if err != nil || got.ID != 8 || got.Vectors[0][0] != 1 {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if _, err := ReadResponse(bytes.NewReader(rawFrame(`{"id":1,"op":"query","code":"ok","vectors":[[1]]}`))); !errors.Is(err, ErrProtocol) {
		t.Fatalf("untrusted vector: %v", err)
	}
	if _, err := ReadResponse(bytes.NewReader(rawFrame(`{"id":1,"op":"query","code":"ok","vectors":[[` + strings.Repeat("null,", 383) + `null]]}`))); !errors.Is(err, ErrProtocol) {
		t.Fatalf("null vector: %v", err)
	}
}

func nonfiniteVector(value float32) []float32 {
	vector := make([]float32, 384)
	vector[17] = value
	return vector
}

func TestRequestPreservesValidUnicodeEscapes(t *testing.T) {
	for name, fixture := range map[string]struct{ json, want string }{
		"paired surrogate":          {`{"id":1,"op":"count","text":"\ud83d\ude00","deadline_unix_nano":1}`, "😀"},
		"literal escaped backslash": {`{"id":1,"op":"count","text":"\\ud800","deadline_unix_nano":1}`, `\ud800`},
	} {
		t.Run(name, func(t *testing.T) {
			request, err := ReadRequest(bytes.NewReader(rawFrame(fixture.json)))
			if err != nil || request.Text != fixture.want {
				t.Fatalf("got %q %v want %q", request.Text, err, fixture.want)
			}
		})
	}
}

type shortFrameWriter struct{ bytes.Buffer }

func (writer *shortFrameWriter) Write(data []byte) (int, error) {
	if len(data) > 3 {
		data = data[:3]
	}
	return writer.Buffer.Write(data)
}

type stalledFrameWriter struct{}

func (stalledFrameWriter) Write([]byte) (int, error) { return 0, nil }

func TestWriteFrameCompletesShortWritesAndRejectsStalledWriter(t *testing.T) {
	var writer shortFrameWriter
	if err := WriteRequest(&writer, Request{ID: 1, Op: OpClose}); err != nil {
		t.Fatal(err)
	}
	if request, err := ReadRequest(&writer); err != nil || request.ID != 1 {
		t.Fatalf("short writes corrupted frame: %+v %v", request, err)
	}
	if err := WriteRequest(stalledFrameWriter{}, Request{ID: 1, Op: OpClose}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("stalled writer: %v", err)
	}
}
