package app

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/Bodokh/my-whatsapp-mcp/internal/media"
)

type bundleArtifact struct {
	Role         string `json:"role"`
	RelativePath string `json:"relative_path"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA256       string `json:"sha256"`
	MachO        bool   `json:"macho"`
	Executable   bool   `json:"executable"`
}

func knownAIProfile(mapping aiProfileMapping) bool {
	switch mapping.Kind {
	case "encoder":
		if mapping.Language != "" {
			return false
		}
		switch mapping.ProfileID {
		case "darwin-arm64-fp32-feasibility", "darwin-arm64-fp32-bounded-experimental":
			return mapping.ModelProfileID == "darwin-arm64-fp32-feasibility"
		case "darwin-arm64-dynamic-int8-feasibility":
			return mapping.ModelProfileID == mapping.ProfileID
		}
	case "ocr":
		return mapping.Language == "" && (mapping.ProfileID == "tesseract-fast" || mapping.ProfileID == "tesseract-best") && mapping.ModelProfileID == mapping.ProfileID
	case "caption":
		return mapping.Language == "" && mapping.ProfileID == "qwen3.5-0.8b-vision" && mapping.ModelProfileID == mapping.ProfileID
	case "speech":
		if mapping.Language == "en" {
			return mapping.ModelProfileID == media.EnglishSpeechProfile && mapping.ProfileID == mapping.ModelProfileID
		}
		return (mapping.Language == "he" || mapping.Language == "mixed") && mapping.ProfileID == "ivrit-whisper-large-v3-turbo" && mapping.ModelProfileID == mapping.ProfileID
	}
	return false
}

// Mapping selection is closed and contains no executable/path authority outside
// the already declared bundle inventory. Qualification never accepts a release
// profile merely because a manifest asserts acceptance.
func aiSelectedArtifacts(mapping aiProfileMapping, inventory map[string]bundleArtifact) ([]AIClosureArtifact, error) {
	if !knownAIProfile(mapping) {
		return nil, errBundledAI
	}
	roles := map[string]string{}
	switch mapping.Kind {
	case "encoder":
		roles["ort"] = "native_runtime"
	case "ocr":
		roles["ocr"] = "native_runtime"
	case "caption":
		roles["caption"] = "native_runtime"
	case "speech":
		roles["speech"] = "native_runtime"
		roles["decoder"] = "native_runtime"
	}
	if len(mapping.RuntimeArtifacts) != len(roles) {
		return nil, errBundledAI
	}
	selected := []AIClosureArtifact{}
	add := func(producingRole, declaredPath, artifactRole string, maximum int64, executable bool) error {
		artifact, exists := inventory[declaredPath]
		if !exists || !canonicalBundlePath(declaredPath) || artifact.RelativePath != declaredPath || artifact.Role != artifactRole || artifact.SizeBytes <= 0 || artifact.SizeBytes > maximum || !canonicalHex(artifact.SHA256, 32) || artifact.Executable != executable {
			return errBundledAI
		}
		if artifactRole == "native_runtime" || artifactRole == "native_worker" {
			if !artifact.MachO {
				return errBundledAI
			}
		} else if artifact.MachO {
			return errBundledAI
		}
		selected = append(selected, AIClosureArtifact{ProducingRole: producingRole, Role: artifact.Role, Path: declaredPath, Bytes: artifact.SizeBytes, SHA256: artifact.SHA256})
		return nil
	}
	if mapping.Kind == "encoder" {
		if err := add("worker", mapping.WorkerPath, "native_worker", 64<<20, true); err != nil {
			return nil, err
		}
		if err := add("profile", mapping.ProfileManifestPath, "encoder_profile", 1<<20, false); err != nil {
			return nil, err
		}
	} else if mapping.WorkerPath != "" || mapping.ProfileManifestPath != "" {
		return nil, errBundledAI
	}
	for producingRole, artifactRole := range roles {
		value, exists := mapping.RuntimeArtifacts[producingRole]
		if !exists {
			return nil, errBundledAI
		}
		if err := add(producingRole, value, artifactRole, 256<<20, producingRole != "ort"); err != nil {
			return nil, err
		}
	}
	return selected, nil
}

func verifyAISmoke(mapping aiProfileMapping, selected []AIClosureArtifact, sourceRevision string, evidence []byte) error {
	qualification := mapping.Qualification
	if qualification.Version != 1 || qualification.State != "preview" || qualification.Smoke != "PASS" || !canonicalHex(sourceRevision, 20) || qualification.SourceRevision != sourceRevision || !canonicalHex(qualification.SourceSnapshotSHA256, 32) || !canonicalHex(qualification.ClosureSHA256, 32) || !canonicalBundlePath(qualification.SmokeRecordPath) || len(evidence) == 0 || len(evidence) > 16<<10 || !uniqueBundleJSON(evidence) {
		return errBundledAI
	}
	digest, err := AIClosureDigest(mapping.ProfileID, mapping.ModelProfileID, mapping.Language, qualification.SourceSnapshotSHA256, selected)
	if err != nil || digest != qualification.ClosureSHA256 {
		return errBundledAI
	}
	var record aiSmokeRecord
	decoder := json.NewDecoder(bytes.NewReader(evidence))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil || decoder.Decode(new(any)) != io.EOF {
		return errBundledAI
	}
	if record.Version != 1 || record.State != "preview" || record.Smoke != "PASS" || record.Kind != mapping.Kind || record.ProfileID != mapping.ProfileID || record.ModelProfileID != mapping.ModelProfileID || record.Language != mapping.Language || record.SourceRevision != qualification.SourceRevision || record.SourceSnapshotSHA256 != qualification.SourceSnapshotSHA256 || record.ClosureSHA256 != qualification.ClosureSHA256 {
		return errBundledAI
	}
	return nil
}

func uniqueBundleJSON(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 16 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delim, compound := token.(json.Delim)
		if !compound {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				key, ok := keyToken.(string)
				if err != nil || !ok || key != strings.ToLower(key) || seen[key] {
					return false
				}
				seen[key] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			count := 0
			for decoder.More() {
				count++
				if count > 1024 || !value(depth+1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim(']')
		}
		return false
	}
	return value(0) && decoder.Decode(new(any)) == io.EOF
}
