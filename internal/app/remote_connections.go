package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/clientsetup"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"time"
)

var _ localui.RemoteCredentialApplication = (*Application)(nil)

func (a *Application) remoteRouteReady() bool {
	if a.remoteShutdownPending() || a.mcp == nil || a.mcpConfig.Remote == nil || a.mcpConfig.Remote.Server.Mode != mcpserver.Bearer || a.remoteRecoveryPending || a.authState.RemoteInvalid {
		return false
	}
	_, state := a.mcp.status()
	return state == "ready"
}

func (a *Application) appendRemoteConnections(ctx context.Context, result *localui.ConnectionSnapshot, account string) error {
	metadata, err := a.listRemote(ctx, account, "")
	if err != nil {
		return localui.ErrConnectionUnavailable
	}
	for _, record := range a.authState.RemoteClients {
		item := localui.Connection{ID: record.IntentID, Client: record.Client, Label: record.Label, Mode: "remote", State: "recovery_pending", SendPermission: archive.SendPermission{Mode: "none", Chats: []archive.ChatID{}}}
		for _, row := range metadata {
			if row.ID != record.CredentialID || row.Account != record.Account || row.Principal != record.Principal {
				continue
			}
			item.Revision = row.Revision
			expiry := row.ExpiresAt
			item.ExpiresAt = &expiry
			if row.Revoked {
				item.State = "revoked"
			} else if !row.ExpiresAt.After(time.Now()) {
				item.State = "expired"
			} else if record.State == "ready" {
				grant, err := a.store.ResolveCredentialID(ctx, record.CredentialID)
				if err != nil || grant.Scope.Account != record.Account || grant.Scope.Principal != record.Principal {
					return localui.ErrConnectionUnavailable
				}
				if err = a.store.CheckCredentialGrant(ctx, grant); err != nil {
					return localui.ErrConnectionUnavailable
				}
				item.Scope = connectionDates(grant.Scope)
				item.SendPermission, err = a.store.GetSendPermission(ctx, grant)
				if err != nil {
					return localui.ErrConnectionUnavailable
				}
				item.State = "ready"
			}
			break
		}
		result.Items = append(result.Items, item)
	}
	return nil
}

func (a *Application) remoteConnectionGuide(ctx context.Context, request localui.ConnectionGuideRequest) (clientsetup.Guide, error) {
	if !a.remoteRouteReady() {
		return clientsetup.Guide{}, localui.ErrConnectionUnavailable
	}
	for _, record := range a.authState.RemoteClients {
		if record.IntentID != request.ID {
			continue
		}
		if record.State != "ready" || record.Client != request.Client || !a.remoteRegistry.PublishedRemoteClient(remoteBinding(record)) {
			return clientsetup.Guide{}, localui.ErrConnectionRecovery
		}
		metadata, err := a.listRemote(ctx, record.Account, record.Principal)
		if err != nil || a.verifyRemoteRecord(ctx, record, metadata) != nil {
			return clientsetup.Guide{}, localui.ErrConnectionRecovery
		}
		config, _, err := a.readRemoteFile(record.ConfigName)
		if err != nil {
			return clientsetup.Guide{}, localui.ErrConnectionRecovery
		}
		remote := a.mcpConfig.Remote
		guide, err := clientsetup.Render(clientsetup.Client(request.Client), clientsetup.Remote, clientsetup.SetupConfig{RemoteURL: remote.PublicURL, Auth: clientsetup.Bearer, BearerTokenEnv: "WA_INDEXER_CLIENT_TOKEN", CredentialID: string(record.CredentialID), Token: config.Token, HeaderAuthAvailable: remote.HeaderAuthAvailable})
		if errors.Is(err, clientsetup.ErrUnsupported) {
			return guide, localui.ErrConnectionUnsupported
		}
		if err != nil {
			return clientsetup.Guide{}, localui.ErrConnectionUnavailable
		}
		return guide, nil
	}
	return clientsetup.Guide{}, localui.ErrConnectionNotFound
}

func (a *Application) RevealRemoteCredential(ctx context.Context, request localui.ConnectionRevealRequest) (localui.RemoteCredentialSecret, error) {
	if a.snapshot != nil {
		return localui.RemoteCredentialSecret{}, archive.ErrHistoricalSnapshotReadOnly
	}
	if request.ID == "" || request.Revision == 0 {
		return localui.RemoteCredentialSecret{}, localui.ErrConnectionInvalid
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed || !a.mcpEnabled || !a.remoteRouteReady() {
		return localui.RemoteCredentialSecret{}, localui.ErrConnectionUnavailable
	}
	for _, record := range a.authState.RemoteClients {
		if record.IntentID != request.ID {
			continue
		}
		if record.State != "ready" || !a.remoteRegistry.PublishedRemoteClient(remoteBinding(record)) {
			return localui.RemoteCredentialSecret{}, localui.ErrConnectionRecovery
		}
		metadata, err := a.listRemote(ctx, record.Account, record.Principal)
		if err != nil {
			return localui.RemoteCredentialSecret{}, localui.ErrConnectionUnavailable
		}
		for _, row := range metadata {
			if row.ID != record.CredentialID || row.Account != record.Account || row.Principal != record.Principal {
				continue
			}
			if row.Revision != request.Revision {
				return localui.RemoteCredentialSecret{}, localui.ErrConnectionConflict
			}
			if row.Revoked || !row.ExpiresAt.After(time.Now()) {
				return localui.RemoteCredentialSecret{}, localui.ErrConnectionUnavailable
			}
			config, id, err := a.readRemoteFile(record.ConfigName)
			if err != nil || id != record.CredentialID {
				return localui.RemoteCredentialSecret{}, localui.ErrConnectionRecovery
			}
			grant, err := a.store.ResolveCredential(ctx, sha256.Sum256([]byte(config.Token)))
			if err == nil && grant.CredentialRevision != request.Revision {
				return localui.RemoteCredentialSecret{}, localui.ErrConnectionConflict
			}
			if err != nil || grant.CredentialID != record.CredentialID || grant.Scope.Account != record.Account || grant.Scope.Principal != record.Principal || a.store.CheckCredentialGrant(ctx, grant) != nil {
				return localui.RemoteCredentialSecret{}, localui.ErrConnectionUnavailable
			}
			return localui.RemoteCredentialSecret{ID: record.IntentID, Revision: row.Revision, PublicURL: a.mcpConfig.Remote.PublicURL, Token: config.Token, ExpiresAt: row.ExpiresAt}, nil
		}
		return localui.RemoteCredentialSecret{}, localui.ErrConnectionUnavailable
	}
	return localui.RemoteCredentialSecret{}, localui.ErrConnectionNotFound
}

func (a *Application) revokeRemoteConnection(ctx context.Context, request localui.ConnectionRevokeRequest) error {
	for _, record := range a.authState.RemoteClients {
		if record.IntentID != request.ID {
			continue
		}
		metadata, err := a.listRemote(ctx, record.Account, record.Principal)
		if err != nil {
			return localui.ErrConnectionUnavailable
		}
		for _, row := range metadata {
			if row.ID != record.CredentialID || row.Account != record.Account || row.Principal != record.Principal {
				continue
			}
			if row.Revision != request.Revision {
				return localui.ErrConnectionConflict
			}
			if err = a.store.RevokeCredential(ctx, record.CredentialID, request.Revision); err != nil {
				if errors.Is(err, archive.ErrPolicyChanged) {
					return localui.ErrConnectionConflict
				}
				return localui.ErrConnectionUnavailable
			}
			a.remoteRegistry.replace(nil)
			if err = a.reconcileRemoteClients(ctx); err != nil {
				return localui.ErrConnectionRecovery
			}
			return nil
		}
		return localui.ErrConnectionNotFound
	}
	return localui.ErrConnectionNotFound
}
