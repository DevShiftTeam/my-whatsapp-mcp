package remoteaccess

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localauth"
)

const (
	maxIntentBytes     = 4096
	maxCredentialBytes = 32768
	maxSecretBytes     = 16384
)

type diskIntent struct {
	Version       int      `json:"version"`
	Provider      Provider `json:"provider"`
	Enabled       bool     `json:"enabled"`
	ExposeBrowser bool     `json:"expose_browser"`
	SavedOrigin   string   `json:"saved_origin"`
	NodeName      string   `json:"node_name"`
	CredentialRef string   `json:"credential_ref"`
}
type diskCredential struct {
	Version      int      `json:"version"`
	Reference    string   `json:"reference"`
	Provider     Provider `json:"provider"`
	IntentDigest string   `json:"intent_digest"`
	Secret       []byte   `json:"secret"`
}

// FileStore commits an intent manifest last. Its immutable credential object is
// bound to that exact manifest, preventing mixed identities after interruption.
// Only one current credential and bounded interrupted-save debris are retained.
type FileStore struct {
	mu         sync.Mutex
	paths      appdata.Paths
	lease      *appdata.InstanceLock
	checkpoint func(string) error
}

func NewStore(paths appdata.Paths, lease *appdata.InstanceLock) *FileStore {
	return &FileStore{paths: paths, lease: lease}
}
func (s *FileStore) Load(ctx context.Context) (Intent, Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var intent Intent
	var secret Secret
	err := appdata.WithRemoteAccess(s.paths, s.lease, func(root *os.Root) error {
		if ctx.Err() != nil {
			return ErrState
		}
		manifest, data, err := readManifest(root)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		credential, err := readCredential(root, manifest, data)
		if err != nil {
			return err
		}
		intent = manifest.intent()
		secret = credential
		return nil
	})
	if err != nil {
		return Intent{}, nil, ErrState
	}
	return intent, secret, nil
}
func (s *FileStore) Save(ctx context.Context, intent Intent, secret Secret) error {
	if validateIntent(intent) != nil || len(secret) > maxSecretBytes {
		return ErrInvalidIntent
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := appdata.WithRemoteAccess(s.paths, s.lease, func(root *os.Root) error {
		if ctx.Err() != nil {
			return ErrState
		}
		previous, previousData, err := readManifest(root)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return ErrState
		}
		if err == nil {
			if _, err := readCredential(root, previous, previousData); err != nil {
				return ErrState
			}
		}
		if err := cleanOrphans(root, previous.CredentialRef); err != nil {
			return err
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return ErrState
		}
		ref := hex.EncodeToString(nonce[:])
		manifest := diskIntent{intent.Version, intent.Provider, intent.Enabled, intent.ExposeBrowser, intent.SavedOrigin, intent.NodeName, ref}
		data, err := json.Marshal(manifest)
		if err != nil || len(data) > maxIntentBytes {
			return ErrState
		}
		cred, err := json.Marshal(diskCredential{1, ref, intent.Provider, digest(data), []byte(secret)})
		if err != nil || len(cred) > maxCredentialBytes {
			return ErrState
		}
		if err := writePrivate(root, "credential-"+ref+".json", cred); err != nil {
			return err
		}
		temporary := ".intent-" + ref
		if err := writePrivate(root, temporary, data); err != nil {
			return err
		}
		if err := syncRoot(root); err != nil {
			return err
		}
		if s.checkpoint != nil {
			if err := s.checkpoint("before_commit"); err != nil {
				return ErrState
			}
		}
		if ctx.Err() != nil {
			return ErrState
		}
		if err := root.Rename(temporary, "intent.json"); err != nil {
			return ErrState
		}
		if err := syncRoot(root); err != nil {
			return err
		}
		if s.checkpoint != nil {
			if err := s.checkpoint("after_commit"); err != nil {
				return ErrState
			}
		}
		if err := cleanOrphans(root, ref); err != nil {
			return err
		}
		return syncRoot(root)
	})
	if err != nil {
		return ErrState
	}
	return nil
}
func (s *FileStore) Remove(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := appdata.WithRemoteAccessRemoval(s.paths, s.lease, func(root *os.Root) error {
		if ctx.Err() != nil {
			return ErrState
		}
		// Remove the commit first: interruption disables ingress, never leaves an
		// enabled manifest referencing partially deleted credentials/node state.
		if err := root.Remove("intent.json"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ErrState
		}
		if err := syncRoot(root); err != nil {
			return err
		}
		names, err := directoryNames(root)
		if err != nil {
			return err
		}
		for _, name := range names {
			if name == localauth.RecordFilename {
				if err := localauth.ValidateRecordFile(root); err != nil {
					return ErrState
				}
				continue
			}
			if err := root.RemoveAll(name); err != nil {
				return ErrState
			}
		}
		return syncRoot(root)
	})
	if err != nil {
		return ErrState
	}
	return nil
}
func (d diskIntent) intent() Intent {
	return Intent{Version: d.Version, Provider: d.Provider, Enabled: d.Enabled, ExposeBrowser: d.ExposeBrowser, SavedOrigin: d.SavedOrigin, NodeName: d.NodeName}
}
func readManifest(root *os.Root) (diskIntent, []byte, error) {
	data, err := readPrivate(root, "intent.json", maxIntentBytes)
	if err != nil {
		return diskIntent{}, nil, err
	}
	var d diskIntent
	if strictDecode(data, &d, []string{"version", "provider", "enabled", "expose_browser", "saved_origin", "node_name", "credential_ref"}) != nil || validateIntent(d.intent()) != nil || !validRef(d.CredentialRef) {
		return diskIntent{}, nil, ErrState
	}
	return d, data, nil
}
func validRef(ref string) bool {
	if len(ref) != 32 {
		return false
	}
	_, err := hex.DecodeString(ref)
	return err == nil && strings.ToLower(ref) == ref
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func readPrivate(root *os.Root, name string, limit int) ([]byte, error) {
	f, err := appdata.OpenRemoteAccessFile(root, name, false)
	if err != nil {
		return nil, err
	}
	before, statErr := f.Stat()
	data, readErr := io.ReadAll(io.LimitReader(f, int64(limit+1)))
	after, afterErr := f.Stat()
	current, currentErr := root.Lstat(name)
	closeErr := f.Close()
	if statErr != nil || readErr != nil || afterErr != nil || currentErr != nil || closeErr != nil || len(data) > limit || !os.SameFile(before, after) || !os.SameFile(after, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, ErrState
	}
	return data, nil
}
func writePrivate(root *os.Root, name string, data []byte) error {
	f, err := appdata.OpenRemoteAccessFile(root, name, true)
	if err != nil {
		return ErrState
	}
	before, statErr := f.Stat()
	n, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if statErr != nil || n != len(data) || writeErr != nil || syncErr != nil || closeErr != nil {
		return ErrState
	}
	check, err := appdata.OpenRemoteAccessFile(root, name, false)
	if err != nil {
		return ErrState
	}
	current, currentErr := check.Stat()
	closeErr = check.Close()
	if currentErr != nil || closeErr != nil || !os.SameFile(before, current) || current.Size() != int64(len(data)) {
		return ErrState
	}
	return nil
}
func syncRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return ErrState
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil || closeErr != nil {
		return ErrState
	}
	return nil
}
func directoryNames(root *os.Root) ([]string, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, ErrState
	}
	defer f.Close()
	names, err := f.Readdirnames(4097)
	if err != nil && err != io.EOF || len(names) > 4096 {
		return nil, ErrState
	}
	return names, nil
}
func cleanOrphans(root *os.Root, keep string) error {
	names, err := directoryNames(root)
	if err != nil {
		return err
	}
	for _, name := range names {
		credential := strings.HasPrefix(name, "credential-") && strings.HasSuffix(name, ".json") && validRef(strings.TrimSuffix(strings.TrimPrefix(name, "credential-"), ".json"))
		temporary := strings.HasPrefix(name, ".intent-") && validRef(strings.TrimPrefix(name, ".intent-"))
		if temporary || credential && name != "credential-"+keep+".json" {
			if err := root.Remove(name); err != nil {
				return ErrState
			}
		}
	}
	return nil
}

// Require exact keys once each; disallow null and case-insensitive aliases.
func strictDecode(data []byte, out any, keys []string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return ErrState
	}
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
	}
	seen := map[string]bool{}
	for dec.More() {
		token, err := dec.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] || seen[key] {
			return ErrState
		}
		seen[key] = true
		var value json.RawMessage
		if dec.Decode(&value) != nil || bytes.Equal(value, []byte("null")) && key != "secret" {
			return ErrState
		}
	}
	token, err = dec.Token()
	if err != nil || token != json.Delim('}') || len(seen) != len(keys) || dec.Decode(new(any)) != io.EOF {
		return ErrState
	}
	if json.Unmarshal(data, out) != nil {
		return ErrState
	}
	return nil
}

func readCredential(root *os.Root, manifest diskIntent, data []byte) (Secret, error) {
	credentialData, err := readPrivate(root, "credential-"+manifest.CredentialRef+".json", maxCredentialBytes)
	if err != nil {
		return nil, ErrState
	}
	var c diskCredential
	if strictDecode(credentialData, &c, []string{"version", "reference", "provider", "intent_digest", "secret"}) != nil || c.Version != 1 || c.Reference != manifest.CredentialRef || c.Provider != manifest.Provider || c.IntentDigest != digest(data) || len(c.Secret) > maxSecretBytes {
		return nil, ErrState
	}
	return Secret(c.Secret), nil
}
func (d diskCredential) String() string   { return "[redacted provider credential]" }
func (d diskCredential) GoString() string { return d.String() }
