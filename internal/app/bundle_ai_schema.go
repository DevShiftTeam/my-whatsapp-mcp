package app

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"path"
	"sort"
	"strconv"
	"strings"
)

var errBundledAI = errors.New("local_ai_bundle_unavailable")

// AIClosureArtifact contains only declared immutable bundle metadata. It is a
// producer/verifier port, never decoded from owner HTTP requests.
type AIClosureArtifact struct {
	ProducingRole string `json:"producing_role"`
	Role          string `json:"role"`
	Path          string `json:"path"`
	Bytes         int64  `json:"bytes"`
	SHA256        string `json:"sha256"`
}
type aiQualification struct {
	Version              int    `json:"version"`
	State                string `json:"state"`
	Smoke                string `json:"smoke"`
	SourceRevision       string `json:"source_revision"`
	SourceSnapshotSHA256 string `json:"source_snapshot_sha256"`
	ClosureSHA256        string `json:"closure_sha256"`
	SmokeRecordPath      string `json:"smoke_record_path"`
}
type aiProfileMapping struct {
	Kind                string            `json:"kind"`
	ProfileID           string            `json:"profile_id"`
	ModelProfileID      string            `json:"model_profile_id"`
	ProfileManifestPath string            `json:"profile_manifest_path,omitempty"`
	WorkerPath          string            `json:"worker_path,omitempty"`
	RuntimeArtifacts    map[string]string `json:"runtime_artifacts"`
	Language            string            `json:"language,omitempty"`
	Qualification       aiQualification   `json:"qualification"`
}
type aiSmokeRecord struct {
	Version              int    `json:"version"`
	State                string `json:"state"`
	Smoke                string `json:"smoke"`
	Kind                 string `json:"kind"`
	ProfileID            string `json:"profile_id"`
	ModelProfileID       string `json:"model_profile_id"`
	Language             string `json:"language,omitempty"`
	SourceRevision       string `json:"source_revision"`
	SourceSnapshotSHA256 string `json:"source_snapshot_sha256"`
	ClosureSHA256        string `json:"closure_sha256"`
}

func canonicalBundlePath(value string) bool {
	return value != "" && len(value) <= 512 && path.Clean(value) == value && !path.IsAbs(value) && strings.HasPrefix(value, "Contents/") && !strings.ContainsAny(value, "\\\x00") && !strings.Contains(value, "/../")
}
func canonicalHex(value string, bytes int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == bytes && hex.EncodeToString(decoded) == value
}
func closureFrame(h hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	h.Write(length[:])
	h.Write([]byte(value))
}

// AIClosureDigest is the single canonical framing implementation for measured
// producer evidence and the bundle loader. Artifact order does not affect it.
// Domain bytes, four framed selectors, framed decimal artifact count, then
// sorted producing-role/artifact-role/path/decimal bytes/digest fields are inputs.
func AIClosureDigest(profileID, modelProfileID, language, sourceSnapshotSHA256 string, artifacts []AIClosureArtifact) (string, error) {
	if profileID == "" || modelProfileID == "" || len(profileID) > 100 || len(modelProfileID) > 100 || !canonicalHex(sourceSnapshotSHA256, 32) || (language != "" && language != "he" && language != "en" && language != "mixed") || len(artifacts) == 0 || len(artifacts) > 64 {
		return "", errBundledAI
	}
	items := append([]AIClosureArtifact(nil), artifacts...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].ProducingRole != items[j].ProducingRole {
			return items[i].ProducingRole < items[j].ProducingRole
		}
		if items[i].Role == items[j].Role {
			return items[i].Path < items[j].Path
		}
		return items[i].Role < items[j].Role
	})
	seen := map[string]bool{}
	h := sha256.New()
	h.Write([]byte("wa-indexer/ai-closure/v1\x00"))
	closureFrame(h, profileID)
	closureFrame(h, modelProfileID)
	closureFrame(h, language)
	closureFrame(h, sourceSnapshotSHA256)
	closureFrame(h, strconv.Itoa(len(items)))
	for _, item := range items {
		validProducer := item.ProducingRole == "worker" || item.ProducingRole == "profile" || item.ProducingRole == "ort" || item.ProducingRole == "ocr" || item.ProducingRole == "caption" || item.ProducingRole == "speech" || item.ProducingRole == "decoder"
		if !validProducer || item.Role == "" || len(item.Role) > 64 || !canonicalBundlePath(item.Path) || item.Bytes <= 0 || item.Bytes > 8<<30 || !canonicalHex(item.SHA256, 32) || seen[item.Path] {
			return "", errBundledAI
		}
		seen[item.Path] = true
		closureFrame(h, item.ProducingRole)
		closureFrame(h, item.Role)
		closureFrame(h, item.Path)
		closureFrame(h, strconv.FormatInt(item.Bytes, 10))
		closureFrame(h, item.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
