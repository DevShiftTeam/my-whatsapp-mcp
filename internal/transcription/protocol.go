package transcription

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

type modelFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type runtimeRecipe struct {
	FasterWhisper string `json:"faster_whisper"`
	CTranslate2   string `json:"ctranslate2"`
	AV            string `json:"av"`
	ONNXRuntime   string `json:"onnxruntime"`
	Tokenizers    string `json:"tokenizers"`
	ComputeType   string `json:"compute_type"`
	NumWorkers    int    `json:"num_workers"`
	Language      string `json:"language"`
	VADFilter     bool   `json:"vad_filter"`
}
type recipe struct {
	ProtocolVersion int           `json:"protocol_version"`
	Backend         string        `json:"backend"`
	Model           string        `json:"model"`
	Revision        string        `json:"revision"`
	ModelSHA256     string        `json:"model_sha256"`
	ModelFiles      []modelFile   `json:"model_files"`
	Runtime         runtimeRecipe `json:"runtime"`
}
type infoResponse struct {
	Recipe   recipe `json:"recipe"`
	Identity string `json:"identity"`
}
type transcribeResponse struct {
	Identity    string  `json:"identity"`
	AudioSHA256 string  `json:"audio_sha256"`
	Language    string  `json:"language"`
	Text        string  `json:"text"`
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	Silent      bool    `json:"silent"`
}

var (
	modelPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}/[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	filePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$`)
	jsonKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

func newDigest() hash.Hash           { return sha256.New() }
func digestString(sum []byte) string { return hex.EncodeToString(sum) }
func digest(data []byte) string      { sum := sha256.Sum256(data); return digestString(sum[:]) }
func validDigest(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == sha256.Size && hex.EncodeToString(raw) == value
}

// canonicalJSON matches Python's sorted-key, compact UTF-8 encoding. Recipe
// strings are constrained to ASCII; text uses logical-order UTF-8 unchanged.
func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(generic); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func validateRecipe(info infoResponse, language string) error {
	r := info.Recipe
	revision, err := hex.DecodeString(r.Revision)
	if r.ProtocolVersion != 1 || r.Backend != "faster-whisper" || !modelPattern.MatchString(r.Model) || err != nil || len(revision) != 20 || hex.EncodeToString(revision) != r.Revision || !validDigest(r.ModelSHA256) || !validDigest(info.Identity) || len(r.ModelFiles) < 3 || len(r.ModelFiles) > 32 {
		return ErrInvalidResponse
	}
	files := make(map[string]string, len(r.ModelFiles))
	previous := ""
	for _, file := range r.ModelFiles {
		if !filePattern.MatchString(file.Path) || file.Path <= previous || path.Clean(file.Path) != file.Path || strings.Contains(file.Path, "..") || !validDigest(file.SHA256) {
			return ErrInvalidResponse
		}
		files[file.Path] = file.SHA256
		previous = file.Path
	}
	if files["model.bin"] != r.ModelSHA256 || files["config.json"] == "" || files["tokenizer.json"] == "" {
		return ErrInvalidResponse
	}
	x := r.Runtime
	if !versionPattern.MatchString(x.FasterWhisper) || !versionPattern.MatchString(x.CTranslate2) || !versionPattern.MatchString(x.AV) || !versionPattern.MatchString(x.ONNXRuntime) || !versionPattern.MatchString(x.Tokenizers) || x.ComputeType != "int8" || x.NumWorkers != 1 || x.Language != language || (x.Language != "auto" && !languagePattern.MatchString(x.Language)) || !x.VADFilter {
		return ErrInvalidResponse
	}
	canonical, err := canonicalJSON(r)
	if err != nil || digest(canonical) != info.Identity {
		return ErrInvalidResponse
	}
	return nil
}

func extractionVersion(info infoResponse) (archive.ExtractionVersion, error) {
	runtime, err := canonicalJSON(info.Recipe.Runtime)
	if err != nil {
		return archive.ExtractionVersion{}, err
	}
	v := archive.ExtractionVersion{Kind: archive.JobTranscription, ProfileID: "faster-whisper-http", ProfileIdentity: info.Identity, RuntimeSHA256: digest(runtime), ExtractorVersion: "faster-whisper-http-v1", PromptVersion: "language-vad-default-v1", Language: info.Recipe.Runtime.Language}
	encoded, err := canonicalJSON(v)
	if err != nil {
		return archive.ExtractionVersion{}, err
	}
	v.ID = "faster-whisper-http-" + digest(encoded)
	return v, nil
}

func (b *Backend) fetchInfo(ctx context.Context) (infoResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/v1/info", nil)
	if err != nil {
		return infoResponse{}, ErrInvalidRequest
	}
	request.Header.Set("Authorization", "Bearer "+b.options.Token)
	response, err := b.client.Do(request)
	if err != nil {
		return infoResponse{}, requestError(ctx)
	}
	defer response.Body.Close()
	if err := responseStatus(response.StatusCode); err != nil {
		return infoResponse{}, err
	}
	var info infoResponse
	if err := readJSON(response, maximumInfoBytes, &info); err != nil {
		if ctx.Err() != nil {
			return infoResponse{}, ctx.Err()
		}
		return infoResponse{}, err
	}
	if err := validateRecipe(info, b.options.Language); err != nil {
		return infoResponse{}, err
	}
	return info, nil
}

func readJSON(response *http.Response, limit int64, target any) error {
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" || response.Header.Get("Content-Encoding") != "" || response.ContentLength > limit {
		return ErrInvalidResponse
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit || !utf8.Valid(data) || !validUnicodeEscapes(data) {
		return ErrInvalidResponse
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := validateJSONValue(decoder, 0); err != nil {
		return ErrInvalidResponse
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrInvalidResponse
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return ErrInvalidResponse
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ErrInvalidResponse
	}
	if _, transcript := target.(*transcribeResponse); transcript {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return ErrInvalidResponse
		}
		for _, name := range []string{"identity", "audio_sha256", "language", "text", "start", "end", "silent"} {
			value, exists := fields[name]
			if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return ErrInvalidResponse
			}
		}
	}
	return nil
}

// encoding/json otherwise accepts duplicate keys, which can bind a recipe to
// one value while an upstream parser reads another.
func validateJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 8 {
		return ErrInvalidResponse
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrInvalidResponse
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return ErrInvalidResponse
			}
			name, ok := key.(string)
			if !ok || !jsonKeyPattern.MatchString(name) || seen[name] {
				return ErrInvalidResponse
			}
			seen[name] = true
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return ErrInvalidResponse
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return ErrInvalidResponse
		}
	default:
		return ErrInvalidResponse
	}
	return nil
}

// Go's JSON decoder replaces a lone escaped UTF-16 surrogate with U+FFFD.
// Reject that lossy normalization while permitting valid pairs and a literal
// replacement character supplied by the service.
func validUnicodeEscapes(data []byte) bool {
	for i := 0; i < len(data); i++ {
		if data[i] != '"' {
			continue
		}
		for i++; i < len(data) && data[i] != '"'; i++ {
			if data[i] != '\\' {
				continue
			}
			i++
			if i >= len(data) {
				return false
			}
			if data[i] != 'u' {
				continue
			}
			if i+4 >= len(data) {
				return false
			}
			unit, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
			if err != nil {
				return false
			}
			i += 4
			if unit >= 0xdc00 && unit <= 0xdfff {
				return false
			}
			if unit < 0xd800 || unit > 0xdbff {
				continue
			}
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}
