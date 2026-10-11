package app

import (
	"context"
	"os"

	"github.com/Bodokh/wa-indexer/internal/appdata"
	"github.com/Bodokh/wa-indexer/internal/mcpserver"
)

// Caller has verified the grant and owns lifecycle serialization. A v1 client
// keeps its configured basename and token; pending durability precedes rewrite.
func (a *Application) migrateNativeTransport(ctx context.Context, index int, record nativeClientRecord) error {
	if err := ctx.Err(); err != nil {
		return errNativeRecovery
	}
	config, id, err := a.readNativeFile(record.ConfigName)
	if err != nil || id != record.CredentialID || a.nativeEndpoint.SocketPath == "" {
		return errNativeRecovery
	}
	if config.NativeSocketPath != "" {
		if config.NativeSocketPath != a.nativeEndpoint.SocketPath || config.Endpoint != mcpserver.NativeAdapterEndpoint {
			return errNativeRecovery
		}
		return nil
	}
	previous := a.authState.NativeClients[index]
	a.authState.NativeClients[index].State = "pending"
	if err = a.persistPaused(a.paused); err != nil {
		a.authState.NativeClients[index] = previous
		return errNativeRecovery
	}
	if err = appdata.WithClients(a.paths, a.lease, func(root *os.Root) error {
		return mcpserver.RewriteCredentialFileRoot(root, record.ConfigName, record.CredentialID, config, a.nativeEndpoint)
	}); err != nil {
		return errNativeRecovery
	}
	updated, updatedID, err := a.readNativeFile(record.ConfigName)
	if err != nil || updatedID != record.CredentialID || updated.Token != config.Token || updated.NativeSocketPath != a.nativeEndpoint.SocketPath || updated.Endpoint != mcpserver.NativeAdapterEndpoint {
		return errNativeRecovery
	}
	a.authState.NativeClients[index].State = "ready"
	if err = a.persistPaused(a.paused); err != nil {
		a.authState.NativeClients[index].State = "pending"
		return errNativeRecovery
	}
	return nil
}
