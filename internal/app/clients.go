package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
)

var errNativeRecovery = errors.New("native_client_recovery_pending")

// Published bindings contain no tokens and never perform I/O during a resolve.
type publishedNativeRegistry struct {
	mu       sync.RWMutex
	bindings map[mcpserver.NativeClientBinding]bool
}

func (r *publishedNativeRegistry) PublishedNativeClient(binding mcpserver.NativeClientBinding) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bindings[binding]
}
func (r *publishedNativeRegistry) replace(bindings []mcpserver.NativeClientBinding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindings = make(map[mcpserver.NativeClientBinding]bool, len(bindings))
	for _, binding := range bindings {
		r.bindings[binding] = true
	}
}
func nativeBinding(r nativeClientRecord) mcpserver.NativeClientBinding {
	return mcpserver.NativeClientBinding{Account: r.Account, Principal: r.Principal, CredentialID: r.CredentialID}
}
func nonce128() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", errNativeRecovery
	}
	return hex.EncodeToString(nonce[:]), nil
}
func validNativeLabel(label string) bool {
	if label == "" || label != strings.TrimSpace(label) || len(label) > 128 || !utf8.ValidString(label) {
		return false
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// IssueNativeClientConfig is an internal owner action. The legacy name argument
// never selects a path: a fresh server-generated basename is journaled first.
// The endpoint must already belong to the application's native listener.
func (a *Application) IssueNativeClientConfig(ctx context.Context, request archive.CredentialCreate, _ string, endpoint string) (archive.CredentialID, string, error) {
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	defer a.data.Unlock()
	return a.issueNativeClientConfig(ctx, request, endpoint, "")
}

// Caller owns ops and data; identity and handoff authority are always generated here.
func (a *Application) issueNativeClientConfig(ctx context.Context, request archive.CredentialCreate, endpoint, client string) (archive.CredentialID, string, error) {
	if a.closed || a.nativeRecoveryPending || a.authState.NativeInvalid || a.authState.RemoteInvalid || a.nativeEndpoint.SocketPath == "" || endpoint != mcpserver.NativeAdapterEndpoint || len(a.authState.NativeClients)+len(a.authState.RemoteClients) >= 64 || !validNativeLabel(request.Label) || !request.ExpiresAt.After(time.Now()) {
		return "", "", errNativeRecovery
	}
	account, err := a.store.BoundAccount(ctx)
	if err != nil || account == "" {
		return "", "", errNativeRecovery
	}
	nonce, err := nonce128()
	if err != nil {
		return "", "", err
	}
	record := nativeClientRecord{Version: 1, IntentID: nonce, Account: account, Principal: archive.NativeClientPrincipalPrefix + "v1/" + nonce, ConfigName: "native-" + nonce + ".json", State: "pending"}
	record.Client, record.Label = client, request.Label
	a.authState.NativeClients = append(a.authState.NativeClients, record)
	a.nativeRecoveryPending = true
	if err = a.persistPaused(a.paused); err != nil {
		return "", "", errNativeRecovery
	}
	request.Principal = record.Principal
	request.Policy.Account = account
	var credential archive.Credential
	err = appdata.WithClients(a.paths, a.lease, func(root *os.Root) error {
		var issueErr error
		credential, issueErr = mcpserver.IssueCredentialFile(ctx, a.store, request, root, record.ConfigName, a.nativeEndpoint)
		return issueErr
	})
	index := len(a.authState.NativeClients) - 1
	a.authState.NativeClients[index].CredentialID = credential.ID
	if err != nil {
		// Keep the original pending intent, including every returned ID, even when
		// compensating cleanup or its acknowledgement failed. Never retry Create.
		_ = a.persistPaused(a.paused)
		return "", "", errNativeRecovery
	}
	grant, grantErr := a.store.ResolveCredentialID(ctx, credential.ID)
	if grantErr != nil || grant.Scope.Principal != record.Principal || !nativeGrantWithinPolicy(grant.Scope, request.Policy) {
		_ = a.persistPaused(a.paused)
		return "", "", errNativeRecovery
	}
	a.authState.NativeClients[index].State = "ready"
	if err = a.persistPaused(a.paused); err != nil {
		a.authState.NativeClients[index].State = "pending"
		return "", "", errNativeRecovery
	}
	if err = a.reconcileNativeClients(ctx); err != nil {
		return "", "", errNativeRecovery
	}
	return credential.ID, filepath.Join(a.paths.ClientDir, record.ConfigName), nil
}

func (a *Application) MCPHandler(cfg mcpserver.Config) (http.Handler, error) {
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	defer a.data.Unlock()
	if !a.closed && a.nativeRecoveryPending && !a.authState.NativeInvalid {
		if err := a.reconcileNativeClients(context.Background()); err != nil {
			return nil, errNativeRecovery
		}
	}
	if a.closed || a.nativeRecoveryPending || a.authState.NativeInvalid {
		return nil, errNativeRecovery
	}
	cfg.NativeClients = &a.nativeRegistry
	cfg.LocalCredentialID = ""
	return mcpserver.NewHandler(cfg, a.resources.retrieval, a.store)
}

func (a *Application) listNative(ctx context.Context, account, principal string) ([]archive.CredentialMetadata, error) {
	request := archive.CredentialListRequest{Account: account, Principal: principal, Limit: 32}
	if principal == "" {
		request.Namespace = archive.CredentialNamespaceNativeClients
	}
	result := []archive.CredentialMetadata{}
	for pages := 0; pages < 32; pages++ {
		page, err := a.store.ListCredentials(ctx, request)
		if err != nil || len(page.Items) > 32 || len(result)+len(page.Items) > 1024 {
			return nil, errNativeRecovery
		}
		for _, item := range page.Items {
			if item.Account != account || (principal != "" && item.Principal != principal) {
				return nil, errNativeRecovery
			}
		}
		result = append(result, page.Items...)
		if page.NextAfter == "" {
			return result, nil
		}
		if page.NextAfter == request.After || len(page.Items) == 0 {
			return nil, errNativeRecovery
		}
		request.After = page.NextAfter
	}
	return nil, errNativeRecovery
}
func (a *Application) revokePrincipal(ctx context.Context, account, principal string) error {
	// Each list is ordered behind accepted Create in the same archive FIFO. An
	// uncertain Create cannot appear after a successful empty recovery fence.
	for retry := 0; retry < 8; retry++ {
		records, err := a.listNative(ctx, account, principal)
		if err != nil {
			return err
		}
		active := false
		for _, record := range records {
			if record.Revoked {
				continue
			}
			active = true
			if err = a.store.RevokeCredential(ctx, record.ID, record.Revision); err != nil {
				// Reload current CAS revisions. Persistent failures remain pending.
				break
			}
		}
		if !active {
			return nil
		}
	}
	return errNativeRecovery
}
func (a *Application) revokeNativeID(ctx context.Context, account, principal string, id archive.CredentialID) error {
	for retry := 0; retry < 8; retry++ {
		records, err := a.listNative(ctx, account, principal)
		if err != nil {
			return err
		}
		active := false
		for _, record := range records {
			if record.ID != id || record.Revoked {
				continue
			}
			active = true
			if err = a.store.RevokeCredential(ctx, id, record.Revision); err == nil {
				break
			}
		}
		if !active {
			return nil
		}
	}
	return errNativeRecovery
}
func (a *Application) readNativeFile(name string) (mcpserver.AdapterConfig, archive.CredentialID, error) {
	var config mcpserver.AdapterConfig
	var id archive.CredentialID
	err := appdata.WithClients(a.paths, a.lease, func(root *os.Root) error {
		var err error
		config, id, err = mcpserver.ReadCredentialFileRoot(root, name)
		return err
	})
	return config, id, err
}
func (a *Application) removeNativeFile(record nativeClientRecord) error {
	return appdata.WithClients(a.paths, a.lease, func(root *os.Root) error {
		before, err := root.Lstat(record.ConfigName)
		if errors.Is(err, os.ErrNotExist) {
			return syncNativeDirectory(root)
		}
		if err != nil {
			return errNativeRecovery
		}
		_, id, err := mcpserver.ReadCredentialFileRoot(root, record.ConfigName)
		if err != nil || record.CredentialID == "" || id != record.CredentialID {
			return errNativeRecovery
		}
		after, err := root.Lstat(record.ConfigName)
		if err != nil || !os.SameFile(before, after) {
			return errNativeRecovery
		}
		if err = root.Remove(record.ConfigName); err != nil {
			return errNativeRecovery
		}
		return syncNativeDirectory(root)
	})
}

func syncNativeDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return errNativeRecovery
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return errNativeRecovery
	}
	return nil
}
func (a *Application) verifyNativeRecord(ctx context.Context, r nativeClientRecord, metadata []archive.CredentialMetadata) error {
	var matching *archive.CredentialMetadata
	for i := range metadata {
		m := &metadata[i]
		if m.ID == r.CredentialID && m.Account == r.Account && m.Principal == r.Principal && !m.Revoked {
			matching = m
			break
		}
	}
	if matching == nil {
		return errNativeRecovery
	}
	config, id, err := a.readNativeFile(r.ConfigName)
	if err != nil || id != r.CredentialID {
		return errNativeRecovery
	}
	grant, err := a.store.ResolveCredential(ctx, sha256.Sum256([]byte(config.Token)))
	if err != nil || grant.CredentialID != r.CredentialID || grant.Scope.Account != r.Account || grant.Scope.Principal != r.Principal {
		return errNativeRecovery
	}
	return a.store.CheckCredentialGrant(ctx, grant)
}

// recoverLegacy preserves the configured basename; no client path is renamed.
func (a *Application) recoverLegacy(ctx context.Context, account string, metadata []archive.CredentialMetadata) error {
	if a.nativeCredentialID == "" {
		return nil
	}
	for _, r := range a.authState.NativeClients {
		if r.CredentialID == a.nativeCredentialID {
			return nil
		}
	}
	var names []string
	err := appdata.WithClients(a.paths, a.lease, func(root *os.Root) error {
		directory, err := root.Open(".")
		if err != nil {
			return errNativeRecovery
		}
		entries, readErr := directory.ReadDir(65)
		closeErr := directory.Close()
		if (readErr != nil && !errors.Is(readErr, io.EOF)) || closeErr != nil || len(entries) > 64 {
			return errNativeRecovery
		}
		for _, entry := range entries {
			if validLegacyName(entry.Name()) {
				names = append(names, entry.Name())
			}
		}
		return nil
	})
	if err != nil {
		return errNativeRecovery
	}
	for _, name := range names {
		nonce, err := nonce128()
		if err != nil {
			return err
		}
		record := nativeClientRecord{Version: 1, IntentID: nonce, Account: account, Principal: archive.LegacyNativeClientPrincipal, ConfigName: name, State: "ready", CredentialID: a.nativeCredentialID}
		if a.verifyNativeRecord(ctx, record, metadata) == nil {
			if len(a.authState.NativeClients)+len(a.authState.RemoteClients) >= 64 {
				return errNativeRecovery
			}
			a.authState.NativeClients = append(a.authState.NativeClients, record)
			return a.persistPaused(a.paused)
		}
	}
	return errNativeRecovery
}
func (a *Application) reconcileNativeClients(ctx context.Context) error {
	return a.reconcileNativeClientsAt(ctx, time.Now().UTC())
}

// Only the server clock supplies now; the separate internal entry supports
// the clock-revival regression without sleeping or caller expiry authority.
func (a *Application) reconcileNativeClientsAt(ctx context.Context, now time.Time) error {
	a.nativeRegistry.replace(nil)
	a.nativeRecoveryPending = true
	if a.authState.NativeInvalid {
		return errNativeRecovery
	}
	account, err := a.store.BoundAccount(ctx)
	if err != nil || account == "" {
		return errNativeRecovery
	}
	metadata, err := a.listNative(ctx, account, "")
	if err != nil {
		return err
	}
	if err = a.recoverLegacy(ctx, account, metadata); err != nil {
		return err
	}
	bindings := []mcpserver.NativeClientBinding{}
	retained := []nativeClientRecord{}
	dropLegacyHint := false
	for index, record := range a.authState.NativeClients {
		if record.Account != account {
			return errNativeRecovery
		}
		if record.State == "ready" && inactiveNativeRecord(record, metadata, now) {
			_, id, fileErr := a.readNativeFile(record.ConfigName)
			if fileErr != nil || id != record.CredentialID {
				return errNativeRecovery
			}
			// Expiry alone is not durable: CAS revoke prevents clock revival.
			if err = a.revokeNativeID(ctx, record.Account, record.Principal, record.CredentialID); err != nil {
				return err
			}
			// The already-revoked retirement remains recoverable if the
			// process stops after unlink but before the final journal drop.
			previous := a.authState.NativeClients[index]
			record.State = "pending"
			a.authState.NativeClients[index] = record
			if err = a.persistPaused(a.paused); err != nil {
				a.authState.NativeClients[index] = previous
				return err
			}
			if err = a.removeNativeFile(record); err != nil {
				return err
			}
			if record.CredentialID == a.nativeCredentialID && record.Principal == archive.LegacyNativeClientPrincipal {
				dropLegacyHint = true
			}
			continue
		}
		if record.State == "ready" {
			if a.verifyNativeRecord(ctx, record, metadata) != nil {
				_ = a.revokeNativeID(ctx, record.Account, record.Principal, record.CredentialID)
				return errNativeRecovery
			}
			if a.nativeEndpoint.SocketPath != "" {
				if err := a.migrateNativeTransport(ctx, index, record); err != nil {
					return err
				}
				record = a.authState.NativeClients[index]
			}
			retained = append(retained, record)
			if a.nativeEndpoint.SocketPath != "" {
				bindings = append(bindings, nativeBinding(record))
			}
			continue
		}
		if record.CredentialID == "" {
			for _, m := range metadata {
				if m.Account == record.Account && m.Principal == record.Principal {
					if record.CredentialID != "" && record.CredentialID != m.ID {
						return errNativeRecovery
					}
					record.CredentialID = m.ID
				}
			}
		}
		// Every pending physical cleanup requires a successful durable
		// pending acknowledgement in this attempt, including retry after an
		// uncertain prior write. Memory alone never authorizes unlink.
		a.authState.NativeClients[index] = record
		if err = a.persistPaused(a.paused); err != nil {
			return err
		}
		if err = a.revokePrincipal(ctx, record.Account, record.Principal); err != nil {
			return err
		}
		if err = a.removeNativeFile(record); err != nil {
			return err
		}
		if record.CredentialID == a.nativeCredentialID && record.Principal == archive.LegacyNativeClientPrincipal {
			dropLegacyHint = true
		}
	}
	// Every unbound reserved grant, including orphan legacy rows, is remotely
	// excluded by MCP and revoked before any listener can become ready.
	for _, m := range metadata {
		bound := false
		for _, r := range retained {
			if m.ID == r.CredentialID && m.Principal == r.Principal && m.Account == r.Account {
				bound = true
				break
			}
		}
		if !bound && !m.Revoked {
			if err = a.revokeNativeID(ctx, m.Account, m.Principal, m.ID); err != nil {
				return err
			}
		}
	}
	pendingState := a.authState
	oldHint := a.nativeCredentialID
	a.authState.NativeClients = retained
	if dropLegacyHint {
		a.nativeCredentialID = ""
	}
	if err = a.persistPaused(a.paused); err != nil {
		a.authState = pendingState
		a.nativeCredentialID = oldHint
		return err
	}
	a.nativeRegistry.replace(bindings)
	a.nativeRecoveryPending = false
	return nil
}

func inactiveNativeRecord(record nativeClientRecord, metadata []archive.CredentialMetadata, now time.Time) bool {
	for _, item := range metadata {
		if item.ID == record.CredentialID && item.Account == record.Account && item.Principal == record.Principal {
			return item.Revoked || (!item.ExpiresAt.IsZero() && !item.ExpiresAt.After(now))
		}
	}
	return false
}
