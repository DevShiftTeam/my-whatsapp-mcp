package app

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestMalformedRemoteJournalPreservesNativeAndCore(t *testing.T) {
	raw := []byte(`{ "unexpected": [1, 2] }`)
	input := append([]byte(`{"version":1,"mode":"real","paused":true,"native_clients":[],"remote_clients":`), raw...)
	input = append(input, '}')
	var state applicationState
	if err := json.Unmarshal(input, &state); err != nil {
		t.Fatal(err)
	}
	if !state.RemoteInvalid || state.NativeInvalid || !state.Paused {
		t.Fatal("remote corruption broadened its recovery fence")
	}
	state.Paused = false
	encoded, err := encodeState(state)
	if err != nil || !bytes.Contains(encoded, raw) {
		t.Fatal("remote raw metadata replaced")
	}
	var restored applicationState
	if err := json.Unmarshal(encoded, &restored); err != nil || !restored.RemoteInvalid || restored.NativeInvalid || restored.Paused {
		t.Fatal("independent auth/core state lost")
	}
}

func TestRemoteJournalRejectsNativeIdentityAndCrossFamilyDuplicates(t *testing.T) {
	nonce := "0123456789abcdef0123456789abcdef"
	r := remoteClientRecord{Version: 1, Mode: "remote_bearer", IntentID: nonce, Account: "account", Principal: "wa-indexer/remote-client/v1/" + nonce, ConfigName: "remote-" + nonce + ".json", State: "ready", CredentialID: "remote-id"}
	if !validRemoteRecords([]remoteClientRecord{r}, nil) {
		t.Fatal("valid remote record refused")
	}
	for _, change := range []func(*remoteClientRecord){
		func(r *remoteClientRecord) { r.Principal = "wa-indexer/native-client/v1/" + nonce },
		func(r *remoteClientRecord) { r.Mode = "native" },
		func(r *remoteClientRecord) { r.ConfigName = "native-" + nonce + ".json" },
		func(r *remoteClientRecord) { r.CredentialID = "" },
	} {
		copy := r
		change(&copy)
		if validRemoteRecords([]remoteClientRecord{copy}, nil) {
			t.Fatal("invalid remote identity accepted")
		}
	}
	native := nativeClientRecord{IntentID: nonce, CredentialID: "native-id"}
	if validRemoteRecords([]remoteClientRecord{r}, []nativeClientRecord{native}) {
		t.Fatal("cross-family intent collision accepted")
	}
}
