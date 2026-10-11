package inference

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxFrameBytes      = 256 * 1024
	MaxBatchItems      = 8
	MaxTextBytes       = 16 * 1024
	MaxSplitChunks     = 256
	MaxTokenCount      = 32768
	MaxWorkerRequests  = 256
	MaxRequestDuration = 30 * time.Second
	ProtocolDimension  = 384
	OpInit             = "init"
	OpQuery            = "query"
	OpPassages         = "passages"
	OpCount            = "count"
	OpSplit            = "split"
	OpClose            = "close"
	CodeOK             = "ok"
	CodeBackend        = "backend_failed"
	CodeDeadline       = "deadline_exceeded"
	CodeUnavailable    = "worker_unavailable"
	CodeLimit          = "request_limit"
	CodeInvalid        = "invalid_request"
)

type WorkerInit struct {
	Profile Profile        `json:"profile"`
	Paths   ComponentPaths `json:"paths"`
}

// Request is private process IPC, never a network or logging representation.
type Request struct {
	ID               uint64      `json:"id"`
	Op               string      `json:"op"`
	DeadlineUnixNano int64       `json:"deadline_unix_nano,omitempty"`
	Init             *WorkerInit `json:"init,omitempty"`
	Text             string      `json:"text,omitempty"`
	Texts            []string    `json:"texts,omitempty"`
	MaxTokens        int         `json:"max_tokens,omitempty"`
}

type Response struct {
	ID      uint64      `json:"id"`
	Op      string      `json:"op"`
	Code    string      `json:"code"`
	Vectors [][]float32 `json:"vectors,omitempty"`
	Count   int         `json:"count,omitempty"`
	Chunks  []string    `json:"chunks,omitempty"`
	Counts  []int       `json:"counts,omitempty"`
}

func ReadRequest(reader io.Reader) (Request, error) {
	var request Request
	if err := readFrame(reader, &request); err != nil {
		return Request{}, err
	}
	if err := validateRequest(request); err != nil {
		return Request{}, err
	}
	return request, nil
}

func WriteRequest(writer io.Writer, request Request) error {
	if err := validateRequest(request); err != nil {
		return err
	}
	return writeFrame(writer, request)
}

func ReadResponse(reader io.Reader) (Response, error) {
	var response Response
	if err := readFrame(reader, &response); err != nil {
		return Response{}, err
	}
	if err := validateResponse(response); err != nil {
		return Response{}, err
	}
	return response, nil
}

func WriteResponse(writer io.Writer, response Response) error {
	if err := validateResponse(response); err != nil {
		return err
	}
	return writeFrame(writer, response)
}

func readFrame(reader io.Reader, target any) error {
	var header [4]byte
	n, err := io.ReadFull(reader, header[:])
	if err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			return io.EOF
		}
		return ErrProtocol
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > MaxFrameBytes {
		return ErrProtocol
	}
	body := make([]byte, int(length))
	if _, err = io.ReadFull(reader, body); err != nil || !utf8.Valid(body) {
		return ErrProtocol
	}
	if !validUTF16Escapes(body) || !uniqueJSON(body) {
		return ErrProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ErrProtocol
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return ErrProtocol
	}
	return nil
}

// uniqueJSON rejects ambiguous duplicate object fields before typed decoding.
func uniqueJSON(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 32 {
			return false
		}
		token, err := decoder.Token()
		if err != nil || token == nil {
			return false
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return true
		}
		switch delim {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return false
				}
				name = foldJSONKey(name)
				if _, exists := seen[name]; exists {
					return false
				}
				seen[name] = struct{}{}
				if !value(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for decoder.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !value(0) {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}

// encoding/json accepts Unicode case aliases for struct field names. Folding
// through the complete SimpleFold cycle rejects aliases before they overwrite.
func foldJSONKey(name string) string {
	var folded strings.Builder
	for _, r := range name {
		minimum := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < minimum {
				minimum = next
			}
		}
		folded.WriteRune(minimum)
	}
	return folded.String()
}

// encoding/json replaces unmatched UTF-16 surrogate escapes with U+FFFD;
// reject them instead so message text cannot silently change in transport.
func validUTF16Escapes(body []byte) bool {
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		i++
		if i >= len(body) {
			return false
		}
		if body[i] != 'u' {
			continue
		}
		value, ok := jsonHex4(body[i+1:])
		if !ok {
			return false
		}
		if value >= 0xD800 && value <= 0xDBFF {
			if i+11 > len(body) || body[i+5] != '\\' || body[i+6] != 'u' {
				return false
			}
			low, ok := jsonHex4(body[i+7:])
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i += 10
		} else {
			if value >= 0xDC00 && value <= 0xDFFF {
				return false
			}
			i += 4
		}
	}
	return true
}

func jsonHex4(data []byte) (uint16, bool) {
	if len(data) < 4 {
		return 0, false
	}
	var value uint16
	for _, digit := range data[:4] {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value += uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value += uint16(digit - 'a' + 10)
		case digit >= 'A' && digit <= 'F':
			value += uint16(digit - 'A' + 10)
		default:
			return 0, false
		}
	}
	return value, true
}

func writeFrame(writer io.Writer, value any) error {
	body, err := json.Marshal(value)
	if err != nil || len(body) == 0 || len(body) > MaxFrameBytes {
		return ErrProtocol
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if err := writeAll(writer, header[:]); err != nil {
		return err
	}
	return writeAll(writer, body)
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := writer.Write(data)
		if n < 0 || n > len(data) {
			return ErrProtocol
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func knownOperation(op string) bool {
	switch op {
	case OpInit, OpQuery, OpPassages, OpCount, OpSplit, OpClose:
		return true
	}
	return false
}

func boundedText(text string) bool { return len(text) <= MaxTextBytes && utf8.ValidString(text) }

func validateRequest(request Request) error {
	if request.ID == 0 || !knownOperation(request.Op) || !boundedText(request.Text) {
		return ErrProtocol
	}
	if request.Op != OpClose && request.DeadlineUnixNano <= 0 {
		return ErrProtocol
	}
	if request.Op != OpInit && request.Init != nil {
		return ErrProtocol
	}
	if request.Op != OpPassages && len(request.Texts) != 0 {
		return ErrProtocol
	}
	if request.Op != OpSplit && request.MaxTokens != 0 {
		return ErrProtocol
	}
	switch request.Op {
	case OpInit:
		if request.Init == nil || request.Text != "" {
			return ErrProtocol
		}
	case OpClose:
		if request.Text != "" {
			return ErrProtocol
		}
	case OpQuery:
		if request.Text == "" {
			return ErrProtocol
		}
	case OpPassages:
		if request.Text != "" || len(request.Texts) == 0 || len(request.Texts) > MaxBatchItems {
			return ErrProtocol
		}
		for _, text := range request.Texts {
			if text == "" || !boundedText(text) {
				return ErrProtocol
			}
		}
	case OpSplit:
		if request.MaxTokens < 1 || request.MaxTokens > 256 {
			return ErrProtocol
		}
	}
	return nil
}

func validateResponse(response Response) error {
	if response.ID == 0 || !knownOperation(response.Op) {
		return ErrProtocol
	}
	switch response.Code {
	case CodeOK, CodeBackend, CodeDeadline, CodeUnavailable, CodeLimit, CodeInvalid:
	default:
		return ErrProtocol
	}
	if response.Code != CodeOK {
		if len(response.Vectors) != 0 || response.Count != 0 || len(response.Chunks) != 0 || len(response.Counts) != 0 {
			return ErrProtocol
		}
		return nil
	}
	if response.Op != OpCount && response.Count != 0 {
		return ErrProtocol
	}
	if response.Op != OpSplit && (len(response.Chunks) != 0 || len(response.Counts) != 0) {
		return ErrProtocol
	}
	if response.Op != OpQuery && response.Op != OpPassages && len(response.Vectors) != 0 {
		return ErrProtocol
	}
	switch response.Op {
	case OpQuery, OpPassages:
		if len(response.Vectors) == 0 || len(response.Vectors) > MaxBatchItems || (response.Op == OpQuery && len(response.Vectors) != 1) {
			return ErrProtocol
		}
		for _, vector := range response.Vectors {
			if len(vector) != ProtocolDimension {
				return ErrProtocol
			}
			for _, value := range vector {
				if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
					return ErrProtocol
				}
			}
		}
	case OpCount:
		if response.Count < 0 || response.Count > MaxTokenCount {
			return ErrProtocol
		}
	case OpSplit:
		if len(response.Chunks) > MaxSplitChunks || len(response.Counts) != len(response.Chunks) {
			return ErrProtocol
		}
		total := 0
		for i, chunk := range response.Chunks {
			if chunk == "" || !boundedText(chunk) {
				return ErrProtocol
			}
			if response.Counts[i] < 1 || response.Counts[i] > 256 {
				return ErrProtocol
			}
			total += len(chunk)
			if total > MaxTextBytes {
				return ErrProtocol
			}
		}
	}
	return nil
}
