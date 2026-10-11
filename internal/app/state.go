package app

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// This body-free state is separate from the archive and the WhatsApp session.
type applicationState struct {
	Version            int                  `json:"version"`
	Mode               string               `json:"mode"`
	Paused             bool                 `json:"paused"`
	DesiredConnected   bool                 `json:"desired_connected,omitempty"`
	NativeCredentialID archive.CredentialID `json:"native_credential_id,omitempty"`
	NativeClients      []nativeClientRecord `json:"-"`
	NativeRaw          json.RawMessage      `json:"-"`
	NativeInvalid      bool                 `json:"-"`
	RemoteClients      []remoteClientRecord `json:"-"`
	RemoteRaw          json.RawMessage      `json:"-"`
	RemoteInvalid      bool                 `json:"-"`
	LocalAI            localAIRecord        `json:"-"`
}

type nativeClientRecord struct {
	Version      int                  `json:"version"`
	IntentID     string               `json:"intent_id"`
	Account      string               `json:"account"`
	Principal    string               `json:"principal"`
	ConfigName   string               `json:"config_name"`
	State        string               `json:"state"`
	CredentialID archive.CredentialID `json:"credential_id,omitempty"`
	Client       string               `json:"client,omitempty"`
	Label        string               `json:"label,omitempty"`
}
type stateWire struct {
	Version            int                  `json:"version"`
	Mode               string               `json:"mode"`
	Paused             bool                 `json:"paused"`
	DesiredConnected   bool                 `json:"desired_connected,omitempty"`
	NativeCredentialID archive.CredentialID `json:"native_credential_id,omitempty"`
	NativeClients      json.RawMessage      `json:"native_clients,omitempty"`
	RemoteClients      json.RawMessage      `json:"remote_clients,omitempty"`
	LocalAI            json.RawMessage      `json:"local_ai,omitempty"`
}

func (s applicationState) MarshalJSON() ([]byte, error) {
	raw := s.NativeRaw
	if !s.NativeInvalid {
		var err error
		raw, err = json.Marshal(s.NativeClients)
		if err != nil {
			return nil, err
		}
	}
	remote := s.RemoteRaw
	if !s.RemoteInvalid {
		var err error
		remote, err = json.Marshal(s.RemoteClients)
		if err != nil {
			return nil, err
		}
	}
	ai, err := encodeLocalAIRecord(s.LocalAI)
	if err != nil {
		return nil, err
	}
	return json.Marshal(stateWire{Version: s.Version, Mode: s.Mode, Paused: s.Paused, DesiredConnected: s.DesiredConnected, NativeCredentialID: s.NativeCredentialID, NativeClients: raw, RemoteClients: remote, LocalAI: ai})
}
func (s *applicationState) UnmarshalJSON(data []byte) error {
	var wire stateWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	*s = applicationState{Version: wire.Version, Mode: wire.Mode, Paused: wire.Paused, DesiredConnected: wire.DesiredConnected, NativeCredentialID: wire.NativeCredentialID, NativeRaw: append(json.RawMessage(nil), wire.NativeClients...), RemoteRaw: append(json.RawMessage(nil), wire.RemoteClients...)}
	ai, err := decodeLocalAIRecord(wire.LocalAI)
	if err != nil {
		return err
	}
	s.LocalAI = ai
	if len(wire.NativeClients) != 0 && string(wire.NativeClients) != "null" {
		decoder = json.NewDecoder(bytes.NewReader(wire.NativeClients))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&s.NativeClients); err != nil || !validNativeRecords(s.NativeClients, wire.NativeCredentialID) {
			s.NativeInvalid = true
			s.NativeClients = nil
		}
	}
	if len(wire.RemoteClients) != 0 && string(wire.RemoteClients) != "null" {
		decoder = json.NewDecoder(bytes.NewReader(wire.RemoteClients))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&s.RemoteClients); err != nil || !validRemoteRecords(s.RemoteClients, s.NativeClients) {
			s.RemoteInvalid = true
			s.RemoteClients = nil
		}
	}
	return nil
}
func validNativeRecords(records []nativeClientRecord, legacy archive.CredentialID) bool {
	if len(records) > 64 {
		return false
	}
	seen := map[string]bool{}
	files := map[string]bool{}
	ids := map[archive.CredentialID]bool{}
	for _, r := range records {
		if (r.Client != "" && !validConnectionClient(r.Client)) || (r.Label != "" && !validNativeLabel(r.Label)) || (r.Client != "" && r.Label == "") {
			return false
		}
		data, err := json.Marshal(r)
		nonce, hexerr := hex.DecodeString(r.IntentID)
		if err != nil || len(data) > 1024 || r.Version != 1 || hexerr != nil || len(nonce) != 16 || hex.EncodeToString(nonce) != r.IntentID || r.Account == "" || len(r.Account) > 128 || !utf8.ValidString(r.Account) || seen[r.IntentID] || files[r.ConfigName] || (r.State != "pending" && r.State != "ready") || len(r.CredentialID) > 128 {
			return false
		}
		if r.Principal == archive.LegacyNativeClientPrincipal {
			if legacy == "" || r.CredentialID != legacy || !validLegacyName(r.ConfigName) {
				return false
			}
		} else if r.Principal != archive.NativeClientPrincipalPrefix+"v1/"+r.IntentID || r.ConfigName != "native-"+r.IntentID+".json" {
			return false
		}
		if r.State == "ready" && r.CredentialID == "" {
			return false
		}
		if r.CredentialID != "" && ids[r.CredentialID] {
			return false
		}
		seen[r.IntentID] = true
		files[r.ConfigName] = true
		if r.CredentialID != "" {
			ids[r.CredentialID] = true
		}
	}
	return true
}
func validLegacyName(name string) bool {
	return name != "" && len(name) <= 128 && !strings.ContainsAny(name, "/\\") && name != "." && name != ".." && !strings.HasPrefix(name, ".")
}

const stateFile = "application-state.json"

func readState(paths appdata.Paths, lease *appdata.InstanceLock) (state applicationState, exists bool, result error) {
	result = appdata.WithWorking(paths, lease, func(root *os.Root) error {
		info, err := root.Lstat(stateFile)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || !privateStateFile(info) || info.Size() > 65536 {
			return appdata.ErrUnsafeDataPath
		}
		file, err := root.OpenFile(stateFile, os.O_RDONLY|stateNoFollow(), 0)
		if err != nil {
			return appdata.ErrDataUnavailable
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil || !os.SameFile(info, opened) || !privateStateFile(opened) || !stateNoExtendedACL(file) {
			return appdata.ErrUnsafeDataPath
		}
		decoder := json.NewDecoder(io.LimitReader(file, 65537))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&state); err != nil {
			return appdata.ErrDataUnavailable
		}
		if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return appdata.ErrDataUnavailable
		}
		if state.Version != 1 || (state.Mode != "real" && state.Mode != "demo") {
			return appdata.ErrDataUnavailable
		}
		exists = true
		return nil
	})
	return
}
func encodeState(state applicationState) ([]byte, error) {
	if !state.NativeInvalid && !state.RemoteInvalid {
		return json.Marshal(state)
	}
	ai, err := encodeLocalAIRecord(state.LocalAI)
	if err != nil {
		return nil, err
	}
	core, err := json.Marshal(stateWire{Version: state.Version, Mode: state.Mode, Paused: state.Paused, DesiredConnected: state.DesiredConnected, NativeCredentialID: state.NativeCredentialID, LocalAI: ai})
	if err != nil {
		return nil, appdata.ErrDataUnavailable
	}
	native, remote := state.NativeRaw, state.RemoteRaw
	if !state.NativeInvalid {
		native, err = json.Marshal(state.NativeClients)
	}
	if err != nil {
		return nil, appdata.ErrDataUnavailable
	}
	if !state.RemoteInvalid {
		remote, err = json.Marshal(state.RemoteClients)
	}
	if err != nil || !json.Valid(native) || !json.Valid(remote) {
		return nil, appdata.ErrDataUnavailable
	}
	result := append(core[:len(core)-1], []byte(",\"native_clients\":")...)
	result = append(result, native...)
	result = append(result, []byte(",\"remote_clients\":")...)
	result = append(result, remote...)
	result = append(result, '}')
	return result, nil
}
func writeState(paths appdata.Paths, lease *appdata.InstanceLock, state applicationState) error {
	return appdata.WithWorking(paths, lease, func(root *os.Root) error {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return appdata.ErrDataUnavailable
		}
		// A crash may leave a private staging file. A unique new name permits future
		// writes without interpreting or deleting an interrupted earlier transition.
		temporary := ".application-state-" + hex.EncodeToString(nonce[:])
		file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|stateNoFollow(), 0600)
		if err != nil {
			return appdata.ErrDataUnavailable
		}
		defer root.Remove(temporary)
		if err = file.Chmod(0600); err != nil {
			file.Close()
			return appdata.ErrDataUnavailable
		}
		encoded, encodeErr := encodeState(state)
		if encodeErr == nil && len(encoded) > 65536 {
			encodeErr = appdata.ErrDataUnavailable
		}
		if encodeErr == nil {
			_, encodeErr = file.Write(encoded)
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if encodeErr != nil || syncErr != nil || closeErr != nil {
			return appdata.ErrDataUnavailable
		}
		if err = root.Rename(temporary, stateFile); err != nil {
			return appdata.ErrDataUnavailable
		}
		directory, err := root.Open(".")
		if err != nil {
			return appdata.ErrDataUnavailable
		}
		syncErr = directory.Sync()
		closeErr = directory.Close()
		if syncErr != nil || closeErr != nil {
			return appdata.ErrDataUnavailable
		}
		return nil
	})
}
func (a *Application) persistPaused(paused bool) error {
	mode := "real"
	if a.demo {
		mode = "demo"
	}
	state := a.authState
	state.Version = 1
	state.Mode = mode
	state.Paused = paused
	state.NativeCredentialID = a.nativeCredentialID
	if err := writeState(a.paths, a.lease, state); err != nil {
		return err
	}
	a.authState = state
	return nil
}

// Connection intent is body-free and uses the same private, atomic state write
// as pause and credential journals. An old state without this bit stays inert.
func (a *Application) persistConnectionIntent(desired bool) error {
	previous := a.authState.DesiredConnected
	a.authState.DesiredConnected = desired
	if err := a.persistPaused(a.paused); err != nil {
		a.authState.DesiredConnected = previous
		return err
	}
	return nil
}
