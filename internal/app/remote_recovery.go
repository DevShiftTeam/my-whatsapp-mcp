package app

import (
	"context"
	"crypto/sha256"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"time"
)

func (a *Application) verifyRemoteRecord(ctx context.Context, record remoteClientRecord, metadata []archive.CredentialMetadata) error {
	found := false
	for _, row := range metadata {
		if row.ID == record.CredentialID && row.Account == record.Account && row.Principal == record.Principal && !row.Revoked && row.ExpiresAt.After(time.Now()) {
			found = true
			break
		}
	}
	if !found {
		return errRemoteRecovery
	}
	config, id, err := a.readRemoteFile(record.ConfigName)
	if err != nil || id != record.CredentialID {
		return errRemoteRecovery
	}
	grant, err := a.store.ResolveCredential(ctx, sha256.Sum256([]byte(config.Token)))
	if err != nil || grant.CredentialID != record.CredentialID || grant.Scope.Account != record.Account || grant.Scope.Principal != record.Principal {
		return errRemoteRecovery
	}
	return a.store.CheckCredentialGrant(ctx, grant)
}

func inactiveRemoteRecord(record remoteClientRecord, metadata []archive.CredentialMetadata) bool {
	for _, row := range metadata {
		if row.ID == record.CredentialID && row.Account == record.Account && row.Principal == record.Principal {
			return row.Revoked || !row.ExpiresAt.After(time.Now())
		}
	}
	return false
}

// Recovery is called only by the app-owned mutation/startup lane. It uses the
// archive's ordered metadata capability and never accepts a request principal.
func (a *Application) reconcileRemoteClients(ctx context.Context) error {
	a.remoteRegistry.replace(nil)
	a.remoteRecoveryPending = true
	if a.authState.RemoteInvalid {
		return errRemoteRecovery
	}
	account, err := a.store.BoundAccount(ctx)
	if err != nil {
		return errRemoteRecovery
	}
	if account == "" {
		if len(a.authState.RemoteClients) != 0 {
			return errRemoteRecovery
		}
		a.remoteRecoveryPending = false
		return nil
	}
	metadata, err := a.listRemote(ctx, account, "")
	if err != nil {
		return errRemoteRecovery
	}
	retained := []remoteClientRecord{}
	bindings := []mcpserver.RemoteClientBinding{}
	for index, record := range a.authState.RemoteClients {
		if record.Account != account {
			return errRemoteRecovery
		}
		if record.State == "ready" && !inactiveRemoteRecord(record, metadata) {
			if err = a.verifyRemoteRecord(ctx, record, metadata); err != nil {
				return errRemoteRecovery
			}
			retained = append(retained, record)
			bindings = append(bindings, remoteBinding(record))
			continue
		}
		if record.State == "ready" {
			// Inactive retirement first proves the owned matching file, then
			// durably revokes against clock revival before acknowledging pending.
			_, id, fileErr := a.readRemoteFile(record.ConfigName)
			if fileErr != nil || id != record.CredentialID {
				return errRemoteRecovery
			}
			if err = a.revokeNativeID(ctx, record.Account, record.Principal, record.CredentialID); err != nil {
				return errRemoteRecovery
			}
			prior := a.authState.RemoteClients[index]
			record.State = "pending"
			a.authState.RemoteClients[index] = record
			if err = a.persistPaused(a.paused); err != nil {
				a.authState.RemoteClients[index] = prior
				return errRemoteRecovery
			}
		}
		if record.CredentialID == "" {
			for _, row := range metadata {
				if row.Account == record.Account && row.Principal == record.Principal {
					if record.CredentialID != "" && record.CredentialID != row.ID {
						return errRemoteRecovery
					}
					record.CredentialID = row.ID
				}
			}
		}
		a.authState.RemoteClients[index] = record
		if err = a.persistPaused(a.paused); err != nil {
			return errRemoteRecovery
		}
		if err = a.revokePrincipal(ctx, record.Account, record.Principal); err != nil {
			return errRemoteRecovery
		}
		if err = a.removeRemoteFile(record); err != nil {
			return errRemoteRecovery
		}
	}
	for _, row := range metadata {
		bound := false
		for _, record := range retained {
			if row.ID == record.CredentialID && row.Account == record.Account && row.Principal == record.Principal {
				bound = true
				break
			}
		}
		if !bound && !row.Revoked {
			if err = a.revokeNativeID(ctx, row.Account, row.Principal, row.ID); err != nil {
				return errRemoteRecovery
			}
		}
	}
	prior := a.authState
	a.authState.RemoteClients = retained
	if err = a.persistPaused(a.paused); err != nil {
		a.authState = prior
		return errRemoteRecovery
	}
	a.remoteRegistry.replace(bindings)
	a.remoteRecoveryPending = false
	return nil
}
