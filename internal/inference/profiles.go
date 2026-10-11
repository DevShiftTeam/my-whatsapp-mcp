package inference

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
)

const (
	QueryPrefix        = "query: "
	PassagePrefix      = "passage: "
	EmbeddingDimension = 384
	EncoderTokenLimit  = 256
	E5Revision         = "614241f622f53c4eeff9890bdc4f31cfecc418b3"
	FP32ModelSHA256    = "ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665"
	INT8ModelSHA256    = "8da4c9ba0ad59f58e8566839425d7fd6339d31414d0ce5cba2d7d0afb75dd8b6"
	TokenizerSHA256    = "0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39"
	RuntimeSHA256      = "d306d2bc768540766c7ed8a1e0ff05d2870c77a934ebeee4a7bafa1b732ef299"
)

type Profile struct {
	ID, Precision, ModelSHA256, TokenizerSHA256, RuntimeSHA256    string
	Dimension, MaximumTokens                                      int
	QueryPrefix, PassagePrefix, Pooling, Normalization, Allocator string
	ReleaseSelected                                               bool
}

func ReferenceProfile() Profile {
	return Profile{ID: "darwin-arm64-fp32-feasibility", Precision: "fp32", ModelSHA256: FP32ModelSHA256, TokenizerSHA256: TokenizerSHA256, RuntimeSHA256: RuntimeSHA256, Dimension: 384, MaximumTokens: 256, QueryPrefix: QueryPrefix, PassagePrefix: PassagePrefix, Pooling: "masked_mean", Normalization: "l2", Allocator: "default"}
}
func (p Profile) Validate() error {
	// The public profile identity is also the index's model-space identity.
	// Never allow a caller to reuse one label for different weights or settings.
	precision, model, allocator := "fp32", FP32ModelSHA256, "default"
	switch p.ID {
	case "darwin-arm64-fp32-feasibility":
	case "darwin-arm64-fp32-bounded-experimental":
		allocator = "bounded"
	case "darwin-arm64-dynamic-int8-feasibility":
		precision, model = "int8", INT8ModelSHA256
	default:
		return ErrArtifact
	}
	if p.Precision != precision || p.ModelSHA256 != model || p.Allocator != allocator || p.TokenizerSHA256 != TokenizerSHA256 || p.RuntimeSHA256 != RuntimeSHA256 || p.Dimension != 384 || p.MaximumTokens != 256 || p.QueryPrefix != QueryPrefix || p.PassagePrefix != PassagePrefix || p.Pooling != "masked_mean" || p.Normalization != "l2" {
		return ErrArtifact
	}
	// No recorded E5 profile passes the release gates. A caller cannot promote
	// one by flipping a manifest bit; new acceptance requires a reviewed catalog.
	if p.ReleaseSelected {
		return ErrArtifact
	}
	return nil
}
func LoadProfile(path string) (Profile, error) {
	f, e := os.Open(path)
	if e != nil {
		return Profile{}, ErrArtifact
	}
	defer f.Close()
	var m struct {
		SchemaVersion                                     int `json:"schema_version"`
		Model, Precision, Profile, Pooling, Normalization string
		Revision                                          string `json:"model_revision"`
		Dimension                                         int
		MaximumTokens                                     int    `json:"maximum_tokens"`
		QueryPrefix                                       string `json:"query_prefix"`
		PassagePrefix                                     string `json:"passage_prefix"`
		Allocator                                         string
		Artifacts                                         map[string]struct {
			SHA256 string `json:"sha256"`
		}
		Threads struct {
			Intra    int  `json:"intra_op"`
			Inter    int  `json:"inter_op"`
			Spinning bool `json:"allow_spinning"`
		}
		Graph struct {
			InputNames        []string `json:"input_names"`
			InputElementType  string   `json:"input_element_type"`
			HiddenOutputName  string   `json:"hidden_output_name"`
			OutputElementType string   `json:"output_element_type"`
			OutputRank        int      `json:"output_rank"`
		}
	}
	body, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return Profile{}, ErrArtifact
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if dec.Decode(&m) != nil {
		return Profile{}, ErrArtifact
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return Profile{}, ErrArtifact
	}
	if m.SchemaVersion != 1 || m.Model != "intfloat/multilingual-e5-small" || m.Revision != E5Revision || m.Threads.Intra != 2 || m.Threads.Inter != 1 || m.Threads.Spinning || len(m.Graph.InputNames) != 3 || m.Graph.InputNames[0] != "input_ids" || m.Graph.InputNames[1] != "attention_mask" || m.Graph.InputNames[2] != "token_type_ids" || m.Graph.InputElementType != "int64" || m.Graph.HiddenOutputName != "last_hidden_state" || m.Graph.OutputElementType != "float32" || m.Graph.OutputRank != 3 {
		return Profile{}, ErrArtifact
	}
	if m.Allocator == "" {
		m.Allocator = "default"
	}
	p := Profile{ID: m.Profile, Precision: m.Precision, ModelSHA256: m.Artifacts["model"].SHA256, TokenizerSHA256: m.Artifacts["tokenizer"].SHA256, RuntimeSHA256: m.Artifacts["onnx_runtime"].SHA256, Dimension: m.Dimension, MaximumTokens: m.MaximumTokens, QueryPrefix: m.QueryPrefix, PassagePrefix: m.PassagePrefix, Pooling: m.Pooling, Normalization: m.Normalization, Allocator: m.Allocator}
	return p, p.Validate()
}
