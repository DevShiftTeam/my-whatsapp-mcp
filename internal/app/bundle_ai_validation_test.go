package app

import (
	"encoding/json"
	"strings"
	"testing"
)

// Pure metadata evidence cannot qualify an actual bundle: the loader must also
// hash held asset descriptors and audit native closure before publishing it.
func TestAISmokeMetadataBindsMappingAndRejectsRoleSwap(t *testing.T) {
	mapping := aiProfileMapping{Kind: "speech", ProfileID: "ivrit-whisper-large-v3-turbo", ModelProfileID: "ivrit-whisper-large-v3-turbo", Language: "he", RuntimeArtifacts: map[string]string{"speech": "Contents/MacOS/speech", "decoder": "Contents/MacOS/decoder"}}
	inventory := map[string]bundleArtifact{}
	for role, declaredPath := range mapping.RuntimeArtifacts {
		inventory[declaredPath] = bundleArtifact{Role: "native_runtime", RelativePath: declaredPath, SizeBytes: 10, SHA256: strings.Repeat("a", 64), MachO: true, Executable: true}
		_ = role
	}
	selected, err := aiSelectedArtifacts(mapping, inventory)
	if err != nil {
		t.Fatal(err)
	}
	qualification := aiQualification{Version: 1, State: "preview", Smoke: "PASS", SourceRevision: strings.Repeat("b", 40), SourceSnapshotSHA256: strings.Repeat("c", 64), SmokeRecordPath: "Contents/Resources/smoke.json"}
	qualification.ClosureSHA256, err = AIClosureDigest(mapping.ProfileID, mapping.ModelProfileID, mapping.Language, qualification.SourceSnapshotSHA256, selected)
	if err != nil {
		t.Fatal(err)
	}
	mapping.Qualification = qualification
	record := aiSmokeRecord{Version: 1, State: "preview", Smoke: "PASS", Kind: mapping.Kind, ProfileID: mapping.ProfileID, ModelProfileID: mapping.ModelProfileID, Language: mapping.Language, SourceRevision: qualification.SourceRevision, SourceSnapshotSHA256: qualification.SourceSnapshotSHA256, ClosureSHA256: qualification.ClosureSHA256}
	evidence, _ := json.Marshal(record)
	if err := verifyAISmoke(mapping, selected, qualification.SourceRevision, evidence); err != nil {
		t.Fatal(err)
	}
	mapping.RuntimeArtifacts["speech"], mapping.RuntimeArtifacts["decoder"] = mapping.RuntimeArtifacts["decoder"], mapping.RuntimeArtifacts["speech"]
	swapped, err := aiSelectedArtifacts(mapping, inventory)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyAISmoke(mapping, swapped, qualification.SourceRevision, evidence); err == nil {
		t.Fatal("swapped runtime assignment accepted old smoke")
	}
	if err := verifyAISmoke(mapping, selected, strings.Repeat("d", 40), evidence); err == nil {
		t.Fatal("source revision mismatch accepted")
	}
	for _, raw := range []string{`{"version":1,"version":1}`, `{"Version":1}`} {
		if uniqueBundleJSON([]byte(raw)) {
			t.Fatal("alias/duplicate evidence accepted")
		}
	}
	mapping.Qualification.State = "accepted"
	if err := verifyAISmoke(mapping, selected, qualification.SourceRevision, evidence); err == nil {
		t.Fatal("manifest string manufactured accepted eligibility")
	}
}
