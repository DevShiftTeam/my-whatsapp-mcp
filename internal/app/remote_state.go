package app

import (
	"encoding/hex"
	"encoding/json"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"unicode/utf8"
)

type remoteClientRecord struct {
	Version      int                  `json:"version"`
	Mode         string               `json:"mode"`
	IntentID     string               `json:"intent_id"`
	Account      string               `json:"account"`
	Principal    string               `json:"principal"`
	ConfigName   string               `json:"config_name"`
	State        string               `json:"state"`
	CredentialID archive.CredentialID `json:"credential_id,omitempty"`
	Client       string               `json:"client,omitempty"`
	Label        string               `json:"label,omitempty"`
}

func validRemoteRecords(records []remoteClientRecord, native []nativeClientRecord) bool {
	if len(records)+len(native) > 64 {
		return false
	}
	seen, files, ids := map[string]bool{}, map[string]bool{}, map[archive.CredentialID]bool{}
	for _, r := range native {
		seen[r.IntentID] = true
		files[r.ConfigName] = true
		if r.CredentialID != "" {
			ids[r.CredentialID] = true
		}
	}
	for _, r := range records {
		encoded, err := json.Marshal(r)
		nonce, nonceErr := hex.DecodeString(r.IntentID)
		if err != nil || len(encoded) > 1024 || r.Version != 1 || r.Mode != "remote_bearer" || nonceErr != nil || len(nonce) != 16 || hex.EncodeToString(nonce) != r.IntentID || r.Account == "" || len(r.Account) > 128 || !utf8.ValidString(r.Account) || r.Principal != archive.RemoteClientPrincipalPrefix+"v1/"+r.IntentID || r.ConfigName != "remote-"+r.IntentID+".json" || (r.State != "pending" && r.State != "ready") || len(r.CredentialID) > 128 || (r.State == "ready" && r.CredentialID == "") || seen[r.IntentID] || files[r.ConfigName] || (r.CredentialID != "" && ids[r.CredentialID]) || (r.Client != "" && (!validConnectionClient(r.Client) || r.Label == "")) || (r.Label != "" && !validNativeLabel(r.Label)) {
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
