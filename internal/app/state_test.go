package app

import (
	"bytes"
	"encoding/json"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"testing"
)

func TestMalformedNativeJournalPreservesCoreAndRawBytes(t *testing.T) {
	raw := []byte("{ \"unexpected\": [1, 2] }")
	input := append([]byte("{\"version\":1,\"mode\":\"demo\",\"paused\":true,\"native_clients\":"), raw...)
	input = append(input, '}')
	var state applicationState
	if err := json.Unmarshal(input, &state); err != nil {
		t.Fatal(err)
	}
	if !state.NativeInvalid || !state.Paused || state.Mode != "demo" {
		t.Fatal("invalid auth discarded valid core")
	}
	state.Paused = false
	encoded, err := encodeState(state)
	if err != nil || !bytes.Contains(encoded, raw) {
		t.Fatal("pause persistence replaced malformed auth bytes")
	}
	var restored applicationState
	if err := json.Unmarshal(encoded, &restored); err != nil || !restored.NativeInvalid || restored.Paused {
		t.Fatal("core transition or auth fence lost")
	}
}
func TestNativeJournalRejectsIdentityAndAllocationAmbiguity(t *testing.T) {
	nonce := "0123456789abcdef0123456789abcdef"
	valid := nativeClientRecord{Version: 1, IntentID: nonce, Account: "account", Principal: archive.NativeClientPrincipalPrefix + "v1/" + nonce, ConfigName: "native-" + nonce + ".json", State: "ready", CredentialID: "opaque-id"}
	if !validNativeRecords([]nativeClientRecord{valid}, "") {
		t.Fatal("valid record refused")
	}
	for _, change := range []func(*nativeClientRecord){
		func(r *nativeClientRecord) { r.Principal = archive.NativeClientPrincipalPrefix + "v1/another" },
		func(r *nativeClientRecord) { r.ConfigName = "../outside" },
		func(r *nativeClientRecord) { r.CredentialID = "" },
		func(r *nativeClientRecord) { r.State = "published" },
	} {
		record := valid
		change(&record)
		if validNativeRecords([]nativeClientRecord{record}, "") {
			t.Fatal("ambiguous identity accepted")
		}
	}
	if validNativeRecords([]nativeClientRecord{valid, valid}, "") {
		t.Fatal("duplicate intent accepted")
	}
	records := make([]nativeClientRecord, 65)
	if validNativeRecords(records, "") {
		t.Fatal("unbounded journal accepted")
	}
}
