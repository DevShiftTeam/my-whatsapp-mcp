package app

import (
	"context"
	"github.com/Bodokh/wa-indexer/internal/appdata"
	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/clientsetup"
	"github.com/Bodokh/wa-indexer/internal/mcpserver"
	"os"
	"time"
)

// The caller owns the lifecycle/data mutation lane. No request supplies route,
// principal, account, config basename or a token value.
func (a *Application) issueRemoteClient(ctx context.Context, request archive.CredentialCreate, client string) (archive.CredentialID, error) {
	remote := a.mcpConfig.Remote
	if a.closed || a.mcp == nil || a.authState.NativeInvalid || a.authState.RemoteInvalid || a.remoteRecoveryPending || remote == nil || remote.Server.Mode != mcpserver.Bearer || len(a.authState.NativeClients)+len(a.authState.RemoteClients) >= 64 || !validConnectionClient(client) || !validNativeLabel(request.Label) || !request.ExpiresAt.After(time.Now()) {
		return "", errRemoteRecovery
	}
	_, state := a.mcp.status()
	if state != "ready" {
		return "", errRemoteRecovery
	}
	canonical, err := clientsetup.NormalizeRemoteURL(remote.PublicURL)
	if err != nil || canonical != remote.PublicURL {
		return "", errRemoteRecovery
	}
	account, err := a.store.BoundAccount(ctx)
	if err != nil || account == "" {
		return "", errRemoteRecovery
	}
	nonce, err := nonce128()
	if err != nil {
		return "", errRemoteRecovery
	}
	record := remoteClientRecord{Version: 1, Mode: "remote_bearer", IntentID: nonce, Account: account, Principal: archive.RemoteClientPrincipalPrefix + "v1/" + nonce, ConfigName: "remote-" + nonce + ".json", State: "pending", Client: client, Label: request.Label}
	a.authState.RemoteClients = append(a.authState.RemoteClients, record)
	a.remoteRecoveryPending = true
	if err = a.persistPaused(a.paused); err != nil {
		return "", errRemoteRecovery
	}
	request.Principal = record.Principal
	request.Policy.Account = account
	var credential archive.Credential
	err = appdata.WithClients(a.paths, a.lease, func(root *os.Root) error {
		var issueErr error
		credential, issueErr = mcpserver.IssueRemoteCredentialFile(ctx, a.store, request, root, record.ConfigName, mcpserver.RemoteEndpoint{PublicURL: canonical})
		return issueErr
	})
	index := len(a.authState.RemoteClients) - 1
	a.authState.RemoteClients[index].CredentialID = credential.ID
	if err != nil {
		_ = a.persistPaused(a.paused)
		return "", errRemoteRecovery
	}
	if credential.ID == "" || credential.Principal != record.Principal || !credential.ExpiresAt.After(time.Now()) || credential.ExpiresAt.After(request.ExpiresAt) {
		_ = a.persistPaused(a.paused)
		return "", errRemoteRecovery
	}
	grant, err := a.store.ResolveCredentialID(ctx, credential.ID)
	if err != nil || grant.Scope.Principal != record.Principal || !nativeGrantWithinPolicy(grant.Scope, request.Policy) {
		_ = a.persistPaused(a.paused)
		return "", errRemoteRecovery
	}
	if err = a.store.CheckCredentialGrant(ctx, grant); err != nil {
		_ = a.persistPaused(a.paused)
		return "", errRemoteRecovery
	}
	a.authState.RemoteClients[index].State = "ready"
	if err = a.persistPaused(a.paused); err != nil {
		a.authState.RemoteClients[index].State = "pending"
		return "", errRemoteRecovery
	}
	if err = a.reconcileRemoteClients(ctx); err != nil {
		return "", errRemoteRecovery
	}
	return credential.ID, nil
}
