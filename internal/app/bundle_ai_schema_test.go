package app

import (
	"strings"
	"testing"
)

func TestAIClosureDigestCanonicalOrderAndExactInputs(t *testing.T) {
	assets := []AIClosureArtifact{{ProducingRole: "ort", Role: "native_runtime", Path: "Contents/Frameworks/runtime.dylib", Bytes: 12, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{ProducingRole: "worker", Role: "native_worker", Path: "Contents/MacOS/native-worker", Bytes: 7, SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}
	one, err := AIClosureDigest("encoder-profile", "model-profile", "", strings.Repeat("c", 64), assets)
	if err != nil {
		t.Fatal(err)
	}
	if one != "33f501b8e98c6a77b14ffa60480fb2a87bce74acbef9448601efa14ec84758d9" {
		t.Fatal("producer framing golden changed")
	}
	two, err := AIClosureDigest("encoder-profile", "model-profile", "", strings.Repeat("c", 64), []AIClosureArtifact{assets[1], assets[0]})
	if err != nil || one != two {
		t.Fatal("closure ordering changed identity")
	}
	for _, changed := range []AIClosureArtifact{
		{ProducingRole: assets[0].ProducingRole, Role: assets[0].Role, Path: assets[0].Path, Bytes: 13, SHA256: assets[0].SHA256},
		{ProducingRole: assets[0].ProducingRole, Role: assets[0].Role, Path: assets[0].Path, Bytes: 12, SHA256: assets[1].SHA256},
		{ProducingRole: assets[0].ProducingRole, Role: "different", Path: assets[0].Path, Bytes: 12, SHA256: assets[0].SHA256},
	} {
		digest, err := AIClosureDigest("encoder-profile", "model-profile", "", strings.Repeat("c", 64), []AIClosureArtifact{changed, assets[1]})
		if err != nil || digest == one {
			t.Fatal("audited closure change lost")
		}
	}
}

func TestAIClosureDigestBindsProducingRoleAssignmentAndSourceSnapshot(t *testing.T) {
	assets := []AIClosureArtifact{
		{ProducingRole: "speech", Role: "native_runtime", Path: "Contents/MacOS/speech", Bytes: 12, SHA256: strings.Repeat("a", 64)},
		{ProducingRole: "decoder", Role: "native_runtime", Path: "Contents/MacOS/decoder", Bytes: 7, SHA256: strings.Repeat("b", 64)},
	}
	one, err := AIClosureDigest("he", "speech-model", "he", strings.Repeat("c", 64), assets)
	if err != nil {
		t.Fatal(err)
	}
	assets[0].ProducingRole, assets[1].ProducingRole = assets[1].ProducingRole, assets[0].ProducingRole
	swapped, err := AIClosureDigest("he", "speech-model", "he", strings.Repeat("c", 64), assets)
	if err != nil || swapped == one {
		t.Fatal("role assignment swap kept the old closure")
	}
	changedSource, err := AIClosureDigest("he", "speech-model", "he", strings.Repeat("d", 64), assets)
	if err != nil || changedSource == swapped {
		t.Fatal("source snapshot change kept the old closure")
	}
}

func TestAIClosureDigestRejectsAliasesAndUnboundedInputs(t *testing.T) {
	valid := AIClosureArtifact{ProducingRole: "worker", Role: "native_worker", Path: "Contents/MacOS/native-worker", Bytes: 7, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	for _, changed := range []AIClosureArtifact{{ProducingRole: "ort", Role: valid.Role, Path: "../outside", Bytes: 7, SHA256: valid.SHA256},
		{ProducingRole: valid.ProducingRole, Role: valid.Role, Path: valid.Path, Bytes: -1, SHA256: valid.SHA256},
		{ProducingRole: valid.ProducingRole, Role: valid.Role, Path: valid.Path, Bytes: 7, SHA256: "AAAA"}} {
		if _, err := AIClosureDigest("profile", "model", "", strings.Repeat("c", 64), []AIClosureArtifact{changed}); err == nil {
			t.Fatal("invalid closure accepted")
		}
	}
	if _, err := AIClosureDigest("profile", "model", "", strings.Repeat("c", 64), []AIClosureArtifact{valid, valid}); err == nil {
		t.Fatal("duplicate artifact accepted")
	}
}
