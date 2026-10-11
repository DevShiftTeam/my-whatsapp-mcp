package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/Bodokh/wa-indexer/internal/compute"
	"github.com/Bodokh/wa-indexer/internal/inference"
	"github.com/Bodokh/wa-indexer/internal/media"
)

type bundledAI struct {
	assets   *bundledAIAssets
	rootPath string
	entries  map[string]aiProfileMapping
	profiles map[string]inference.Profile
}

func aiMappingKey(kind, profile, language string) string {
	return kind + "\x00" + profile + "\x00" + language
}

// LoadBundledAI resolves only the launcher-owned executable's audited bundle.
// Returned templates are inactive. Installation/catalog strings alone never
// create an eligible preview, and the accepted allowlist is deliberately empty.
func LoadBundledAI(executablePath string) (native *NativeConfig, mediaConfig *MediaConfig, result error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" || !filepath.IsAbs(executablePath) || filepath.Clean(executablePath) != executablePath {
		return nil, nil, errBundledAI
	}
	if filepath.Base(executablePath) != "wa-indexer" || filepath.Base(filepath.Dir(executablePath)) != "MacOS" || filepath.Base(filepath.Dir(filepath.Dir(executablePath))) != "Contents" {
		return nil, nil, errBundledAI
	}
	rootPath := filepath.Dir(filepath.Dir(filepath.Dir(executablePath)))
	resolved, err := filepath.EvalSymlinks(rootPath)
	if err != nil || resolved != rootPath || filepath.Ext(rootPath) != ".app" {
		return nil, nil, errBundledAI
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, nil, errBundledAI
	}
	assets := &bundledAIAssets{root: root, files: map[string]*os.File{}, artifacts: map[string]bundleArtifact{}}
	success := false
	defer func() {
		if !success {
			_ = assets.Close()
		}
	}()
	parent, err := root.Open(".")
	if err != nil {
		return nil, nil, errBundledAI
	}
	parentInfo, statErr := parent.Stat()
	validRoot := statErr == nil && privateBundleEntry(parentInfo, true, false) && stateNoExtendedACL(parent)
	if closeErr := parent.Close(); !validRoot || closeErr != nil {
		return nil, nil, errBundledAI
	}
	manifestFile, err := assets.open("Contents/release-manifest.json", nil, 65536)
	if err != nil {
		return nil, nil, errBundledAI
	}
	manifestBytes, readErr := readBundleBytes(manifestFile, 65536)
	closeErr := manifestFile.Close()
	if readErr != nil || closeErr != nil || !uniqueBundleJSON(manifestBytes) {
		return nil, nil, errBundledAI
	}
	var manifest struct {
		SchemaVersion  int               `json:"schema_version"`
		Platform       string            `json:"platform"`
		BundleReady    bool              `json:"bundle_ready"`
		SourceRevision string            `json:"source_revision"`
		Artifacts      []bundleArtifact  `json:"artifacts"`
		AIProfiles     []json.RawMessage `json:"ai_profiles"`
	}
	if json.Unmarshal(manifestBytes, &manifest) != nil || manifest.SchemaVersion != 1 || manifest.Platform != "darwin-arm64" || !manifest.BundleReady || !canonicalHex(manifest.SourceRevision, 20) || len(manifest.AIProfiles) == 0 || len(manifest.AIProfiles) > 8 || len(manifest.Artifacts) == 0 || len(manifest.Artifacts) > 256 {
		return nil, nil, errBundledAI
	}
	seen := map[string]bool{}
	for _, artifact := range manifest.Artifacts {
		folded := strings.ToLower(artifact.RelativePath)
		if !canonicalBundlePath(artifact.RelativePath) || folded == "contents/info.plist" || folded == "contents/release-manifest.json" || seen[folded] || artifact.Role == "" || len(artifact.Role) > 64 || artifact.SizeBytes <= 0 || artifact.SizeBytes > 8<<30 || !canonicalHex(artifact.SHA256, 32) {
			return nil, nil, errBundledAI
		}
		seen[folded] = true
		assets.artifacts[artifact.RelativePath] = artifact
	}
	application, exists := assets.artifacts["Contents/MacOS/wa-indexer"]
	if !exists || application.Role != "application" || !application.MachO || !application.Executable {
		return nil, nil, errBundledAI
	}
	applicationFile, err := assets.open(application.RelativePath, &application, 64<<20)
	if err != nil {
		return nil, nil, errBundledAI
	}
	assets.files[application.RelativePath] = applicationFile
	bundle := &bundledAI{assets: assets, rootPath: rootPath, entries: map[string]aiProfileMapping{}, profiles: map[string]inference.Profile{}}
	if err := bundle.auditNative(application.RelativePath, map[string]bool{}); err != nil {
		return nil, nil, errBundledAI
	}
	for _, raw := range manifest.AIProfiles {
		var mapping aiProfileMapping
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&mapping) != nil || decoder.Decode(new(any)) != io.EOF {
			return nil, nil, errBundledAI
		}
		key := aiMappingKey(mapping.Kind, mapping.ProfileID, mapping.Language)
		if _, duplicate := bundle.entries[key]; duplicate {
			return nil, nil, errBundledAI
		}
		selected, err := aiSelectedArtifacts(mapping, assets.artifacts)
		if err != nil {
			return nil, nil, errBundledAI
		}
		selected = append(selected, AIClosureArtifact{ProducingRole: "worker", Role: application.Role, Path: application.RelativePath, Bytes: application.SizeBytes, SHA256: application.SHA256})
		for _, tuple := range selected {
			artifact := assets.artifacts[tuple.Path]
			if assets.files[tuple.Path] == nil {
				file, err := assets.open(tuple.Path, &artifact, 256<<20)
				if err != nil {
					return nil, nil, errBundledAI
				}
				assets.files[tuple.Path] = file
			}
			if artifact.MachO {
				if err := bundle.auditNative(tuple.Path, map[string]bool{}); err != nil {
					return nil, nil, errBundledAI
				}
			}
		}
		// Bind every verified transitive native dependency to its producing role.
		selected, err = bundle.completeClosure(selected)
		if err != nil {
			return nil, nil, errBundledAI
		}
		evidenceArtifact, exists := assets.artifacts[mapping.Qualification.SmokeRecordPath]
		if !exists || evidenceArtifact.Role != "smoke_evidence" || evidenceArtifact.MachO || evidenceArtifact.Executable || evidenceArtifact.SizeBytes > 16<<10 {
			return nil, nil, errBundledAI
		}
		evidenceFile, err := assets.open(evidenceArtifact.RelativePath, &evidenceArtifact, 16<<10)
		if err != nil {
			return nil, nil, errBundledAI
		}
		evidence, readErr := readBundleBytes(evidenceFile, 16<<10)
		closeErr := evidenceFile.Close()
		if readErr != nil || closeErr != nil || verifyAISmoke(mapping, selected, manifest.SourceRevision, evidence) != nil {
			return nil, nil, errBundledAI
		}
		if mapping.Kind == "encoder" {
			profileFile := assets.files[mapping.ProfileManifestPath]
			profile, err := inference.LoadProfile(fmt.Sprintf("/dev/fd/%d", profileFile.Fd()))
			if err != nil || profile.ID != mapping.ProfileID || profile.RuntimeSHA256 != assets.artifacts[mapping.RuntimeArtifacts["ort"]].SHA256 {
				return nil, nil, errBundledAI
			}
			bundle.profiles[key] = profile
			if native == nil {
				native = &NativeConfig{Profile: profile, ModelProfileID: mapping.ModelProfileID, bundle: bundle}
			}
		} else if mediaConfig == nil {
			mediaConfig = &MediaConfig{Residency: compute.SerialModels, bundle: bundle}
		}
		bundle.entries[key] = mapping
	}
	if native == nil && mediaConfig == nil {
		return nil, nil, errBundledAI
	}
	success = true
	return native, mediaConfig, nil
}

func (b *bundledAI) imported(name string) ([]string, error) {
	file := b.assets.files[name]
	if file == nil {
		return nil, errBundledAI
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errBundledAI
	}
	return boundedMachOLibraries(file, info.Size(), b.assets.artifacts[name].Executable)
}

func bundleDependency(name, dependency string) (string, error) {
	if strings.HasPrefix(dependency, "/usr/lib/") || strings.HasPrefix(dependency, "/System/Library/") {
		if path.Clean(dependency) != dependency || strings.Contains(dependency, "\\") {
			return "", errBundledAI
		}
		return "", nil
	}
	if !strings.HasPrefix(dependency, "@loader_path/") {
		return "", errBundledAI
	}
	resolved := path.Clean(path.Join(path.Dir(name), strings.TrimPrefix(dependency, "@loader_path/")))
	if !canonicalBundlePath(resolved) {
		return "", errBundledAI
	}
	return resolved, nil
}

func (b *bundledAI) auditNative(name string, seen map[string]bool) error {
	if seen[name] {
		return nil
	}
	seen[name] = true
	dependencies, err := b.imported(name)
	if err != nil {
		return err
	}
	for _, dependency := range dependencies {
		resolved, err := bundleDependency(name, dependency)
		if err != nil {
			return err
		}
		if resolved == "" {
			continue
		}
		artifact, exists := b.assets.artifacts[resolved]
		if !exists || artifact.Role != "native_runtime" || !artifact.MachO || artifact.Executable {
			return errBundledAI
		}
		if b.assets.files[resolved] == nil {
			file, err := b.assets.open(resolved, &artifact, 256<<20)
			if err != nil {
				return err
			}
			b.assets.files[resolved] = file
		}
		if err := b.auditNative(resolved, seen); err != nil {
			return err
		}
	}
	return nil
}

func (b *bundledAI) completeClosure(selected []AIClosureArtifact) ([]AIClosureArtifact, error) {
	selected = append([]AIClosureArtifact(nil), selected...)
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].ProducingRole != selected[j].ProducingRole {
			return selected[i].ProducingRole < selected[j].ProducingRole
		}
		if selected[i].Role != selected[j].Role {
			return selected[i].Role < selected[j].Role
		}
		return selected[i].Path < selected[j].Path
	})
	result := append([]AIClosureArtifact(nil), selected...)
	seen := map[string]bool{}
	for _, item := range selected {
		seen[item.Path] = true
	}
	var appendDependencies func(AIClosureArtifact) error
	appendDependencies = func(parent AIClosureArtifact) error {
		if !b.assets.artifacts[parent.Path].MachO {
			return nil
		}
		dependencies, err := b.imported(parent.Path)
		if err != nil {
			return err
		}
		for _, dependency := range dependencies {
			resolved, err := bundleDependency(parent.Path, dependency)
			if err != nil {
				return err
			}
			if resolved == "" || seen[resolved] {
				continue
			}
			seen[resolved] = true
			artifact := b.assets.artifacts[resolved]
			item := AIClosureArtifact{ProducingRole: parent.ProducingRole, Role: artifact.Role, Path: resolved, Bytes: artifact.SizeBytes, SHA256: artifact.SHA256}
			result = append(result, item)
			if len(result) > 64 {
				return errBundledAI
			}
			if err := appendDependencies(item); err != nil {
				return err
			}
		}
		return nil
	}
	for _, item := range selected {
		if err := appendDependencies(item); err != nil {
			return nil, err
		}
	}
	return result, nil
}

type bundledRuntimeLease struct{ file *os.File }

func (l *bundledRuntimeLease) Open(name string) (*os.File, error) {
	if name != "runtime" || l.file == nil {
		return nil, inference.ErrUnavailable
	}
	return duplicateBundleFile(l.file)
}
func (l *bundledRuntimeLease) WorkerGuard() (*os.File, error) {
	if l.file == nil {
		return nil, inference.ErrUnavailable
	}
	return duplicateBundleFile(l.file)
}
func (l *bundledRuntimeLease) Close() error {
	if l.file == nil {
		return nil
	}
	if err := l.file.Close(); err != nil {
		return err
	}
	l.file = nil
	return nil
}
func (b *bundledAI) runtimeLease(name string) func(context.Context) (inference.ComponentLease, error) {
	return func(ctx context.Context) (inference.ComponentLease, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := b.assets.duplicate(name)
		if err != nil {
			return nil, err
		}
		return &bundledRuntimeLease{file: file}, nil
	}
}

func (b *bundledAI) runtimeSpec(name string) media.RuntimeSpec {
	artifact := b.assets.artifacts[name]
	return media.RuntimeSpec{Path: filepath.Join(b.rootPath, filepath.FromSlash(name)), SHA256: artifact.SHA256, Bytes: artifact.SizeBytes}
}

// CloseBundledAI releases inactive templates that were not transferred to a
// successfully constructed Application. Application.Close owns live templates.
func CloseBundledAI(native *NativeConfig, mediaConfig *MediaConfig) error {
	var first *bundledAI
	if native != nil {
		first = native.bundle
	}
	if first != nil {
		if err := first.assets.Close(); err != nil {
			return err
		}
	}
	if mediaConfig != nil && mediaConfig.bundle != nil && mediaConfig.bundle != first {
		return mediaConfig.bundle.assets.Close()
	}
	return nil
}
