package app

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"os"
)

var errRemoteRecovery = errors.New("remote_client_recovery_pending")

func (a *Application) readRemoteFile(name string) (mcpserver.RemoteCredentialConfig, archive.CredentialID, error) {
	var config mcpserver.RemoteCredentialConfig
	var id archive.CredentialID
	err := appdata.WithClients(a.paths, a.lease, func(root *os.Root) error {
		var err error
		config, id, err = mcpserver.ReadRemoteCredentialFileRoot(root, name)
		return err
	})
	return config, id, err
}

func (a *Application) removeRemoteFile(record remoteClientRecord) error {
	return appdata.WithClients(a.paths, a.lease, func(root *os.Root) error {
		before, err := root.Lstat(record.ConfigName)
		if errors.Is(err, os.ErrNotExist) {
			return syncNativeDirectory(root)
		}
		if err != nil {
			return errRemoteRecovery
		}
		_, id, err := mcpserver.ReadRemoteCredentialFileRoot(root, record.ConfigName)
		if err != nil || record.CredentialID == "" || id != record.CredentialID {
			return errRemoteRecovery
		}
		after, err := root.Lstat(record.ConfigName)
		if err != nil || !os.SameFile(before, after) {
			return errRemoteRecovery
		}
		if err = root.Remove(record.ConfigName); err != nil {
			return errRemoteRecovery
		}
		return syncNativeDirectory(root)
	})
}

func (a *Application) listRemote(ctx context.Context, account, principal string) ([]archive.CredentialMetadata, error) {
	if principal != "" {
		return a.listNative(ctx, account, principal)
	}
	request := archive.CredentialListRequest{Account: account, Namespace: archive.CredentialNamespaceRemoteClients, Limit: 32}
	result := []archive.CredentialMetadata{}
	seen := map[archive.CredentialID]bool{}
	for pages := 0; pages < 32; pages++ {
		page, err := a.store.ListCredentials(ctx, request)
		if err != nil || len(page.Items) > 32 || len(result)+len(page.Items) > 1024 {
			return nil, errRemoteRecovery
		}
		for _, item := range page.Items {
			if item.Account != account || item.ID == "" || seen[item.ID] {
				return nil, errRemoteRecovery
			}
			seen[item.ID] = true
		}
		result = append(result, page.Items...)
		if page.NextAfter == "" {
			return result, nil
		}
		if page.NextAfter == request.After || len(page.Items) == 0 {
			return nil, errRemoteRecovery
		}
		request.After = page.NextAfter
	}
	return nil, errRemoteRecovery
}
