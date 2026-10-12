package app

import (
	"context"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/clientsetup"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

var _ localui.ConnectionApplication = (*Application)(nil)

func validConnectionClient(value string) bool {
	if len(value) > 32 {
		return false
	}
	switch clientsetup.Client(value) {
	case clientsetup.CodexCLI, clientsetup.CodexDesktop, clientsetup.ClaudeCode, clientsetup.ClaudeConnectors, clientsetup.Composio:
		return true
	}
	return false
}
func validateConnectionRequest(request localui.ConnectionRequest, now time.Time) error {
	if !validConnectionClient(request.Client) || !validNativeLabel(request.Label) || (request.Mode != "local" && request.Mode != "remote") || request.ExpiresAt == nil || !request.ExpiresAt.After(now) {
		return localui.ErrConnectionInvalid
	}
	if request.AllChats {
		if len(request.Chats) != 0 {
			return localui.ErrConnectionInvalid
		}
	} else if len(request.Chats) < 1 || len(request.Chats) > 256 {
		return localui.ErrConnectionInvalid
	}
	seen := map[archive.ChatID]bool{}
	for _, chat := range request.Chats {
		if chat == "" || len(chat) > 128 || !utf8.ValidString(string(chat)) || strings.ContainsRune(string(chat), 0) || seen[chat] {
			return localui.ErrConnectionInvalid
		}
		seen[chat] = true
	}
	if request.StartDate != nil && request.StartDate.IsZero() || request.EndDate != nil && request.EndDate.IsZero() {
		return localui.ErrConnectionInvalid
	}
	for _, bound := range []*time.Time{request.StartDate, request.EndDate} {
		if bound != nil && (bound.Year() < 1 || bound.Year() > 9999) {
			return localui.ErrConnectionInvalid
		}
	}
	if request.StartDate != nil {
		value := request.StartDate.UTC().Truncate(time.Millisecond)
		if value.Before(*request.StartDate) {
			value = value.Add(time.Millisecond)
		}
		if value.UnixMilli() == 0 || value.Year() > 9999 {
			return localui.ErrConnectionInvalid
		}
	}
	if request.EndDate != nil {
		value := request.EndDate.UTC().Truncate(time.Millisecond)
		if value.UnixMilli() == 0 {
			return localui.ErrConnectionInvalid
		}
	}
	if request.StartDate != nil && request.EndDate != nil && !request.StartDate.Before(*request.EndDate) {
		return localui.ErrConnectionInvalid
	}
	return nil
}
func connectionDates(scope archive.ReadScope) localui.ConnectionScopeSummary {
	result := localui.ConnectionScopeSummary{AllChats: scope.AllChats, ChatCount: len(scope.Chats), Chats: append([]archive.ChatID{}, scope.Chats...)}
	if !scope.From.IsZero() {
		value := scope.From
		result.StartDate = &value
	}
	if !scope.Until.IsZero() {
		value := scope.Until
		result.EndDate = &value
	}
	return result
}
func (a *Application) connectionSnapshot(ctx context.Context) (localui.ConnectionSnapshot, error) {
	result := localui.ConnectionSnapshot{Items: []localui.Connection{}, LocalState: "unavailable", RemoteState: "not_configured", RecoveryState: "ready", LocalRecoveryState: "ready", RemoteRecoveryState: "ready"}
	if a.authState.NativeInvalid {
		result.LocalRecoveryState = "invalid"
	} else if a.nativeRecoveryPending {
		result.LocalRecoveryState = "recovery_pending"
	}
	if a.authState.RemoteInvalid {
		result.RemoteRecoveryState = "invalid"
	} else if a.remoteRecoveryPending {
		result.RemoteRecoveryState = "recovery_pending"
	}
	// The compatibility field keeps its historical local-family meaning. New
	// consumers select the exact family's readiness rather than widening fences.
	result.RecoveryState = result.LocalRecoveryState
	if a.mcp != nil {
		result.LocalState, result.RemoteState = a.mcp.status()
	}
	account, err := a.store.BoundAccount(ctx)
	if err != nil {
		return result, localui.ErrConnectionUnavailable
	}
	if account == "" {
		return result, nil
	}
	metadata, err := a.listNative(ctx, account, "")
	if err != nil {
		return result, localui.ErrConnectionUnavailable
	}
	for _, record := range a.authState.NativeClients {
		item := localui.Connection{ID: record.IntentID, Client: record.Client, Label: record.Label, Mode: "local", State: "recovery_pending", SendPermission: archive.SendPermission{Mode: "none", Chats: []archive.ChatID{}}}
		if item.Client == "" {
			item.Client = "unknown"
		}
		if item.Label == "" {
			item.Label = "Native client"
		}
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
					return result, localui.ErrConnectionUnavailable
				}
				if err = a.store.CheckCredentialGrant(ctx, grant); err != nil {
					return result, localui.ErrConnectionUnavailable
				}
				item.Scope = connectionDates(grant.Scope)
				item.SendPermission, err = a.store.GetSendPermission(ctx, grant)
				if err != nil {
					return result, localui.ErrConnectionUnavailable
				}
				item.State = "ready"
			}
			break
		}
		result.Items = append(result.Items, item)
	}
	if err := a.appendRemoteConnections(ctx, &result, account); err != nil {
		return result, err
	}
	return result, nil
}
func (a *Application) Connections(ctx context.Context) (localui.ConnectionSnapshot, error) {
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed {
		return localui.ConnectionSnapshot{}, localui.ErrConnectionUnavailable
	}
	return a.connectionSnapshot(ctx)
}
func (a *Application) CreateConnection(ctx context.Context, request localui.ConnectionRequest) (localui.Connection, error) {
	if err := validateConnectionRequest(request, time.Now()); err != nil {
		return localui.Connection{}, err
	}
	// These clients have no documented local stdio configuration.
	if request.Mode == "local" && (request.Client == string(clientsetup.ClaudeConnectors) || request.Client == string(clientsetup.Composio)) {
		return localui.Connection{}, localui.ErrConnectionUnsupported
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed || !a.mcpEnabled {
		return localui.Connection{}, localui.ErrConnectionUnavailable
	}
	if err := a.startMCP(ctx); err != nil {
		if request.Mode == "local" && a.nativeRecoveryPending || request.Mode == "remote" && a.remoteRecoveryPending {
			return localui.Connection{}, localui.ErrConnectionRecovery
		}
		return localui.Connection{}, localui.ErrConnectionUnavailable
	}
	local, remote := a.mcp.status()
	if request.Mode == "local" && local != "ready" || request.Mode == "remote" && remote != "ready" {
		return localui.Connection{}, localui.ErrConnectionUnavailable
	}
	if request.Mode == "remote" && (a.mcpConfig.Remote == nil || a.mcpConfig.Remote.Server.Mode != mcpserver.Bearer) {
		return localui.Connection{}, localui.ErrConnectionUnavailable
	}
	if request.Mode == "remote" && request.Client == string(clientsetup.ClaudeConnectors) && !a.mcpConfig.Remote.HeaderAuthAvailable {
		return localui.Connection{}, localui.ErrConnectionUnsupported
	}
	if request.Mode == "local" && request.Client == string(clientsetup.ClaudeCode) && (a.dockerContainerName == "" && strings.Contains(a.executablePath, "${") || strings.Contains(a.paths.ClientDir, "${")) {
		return localui.Connection{}, localui.ErrConnectionUnsupported
	}
	policy := archive.CredentialPolicy{AllChats: request.AllChats, Chats: append([]archive.ChatID(nil), request.Chats...)}
	if request.StartDate != nil {
		policy.From = request.StartDate.UTC().Truncate(time.Millisecond)
		if policy.From.Before(*request.StartDate) {
			policy.From = policy.From.Add(time.Millisecond)
		}
	}
	if request.EndDate != nil {
		policy.Until = request.EndDate.UTC().Truncate(time.Millisecond)
	}
	if !policy.From.IsZero() && !policy.Until.IsZero() && !policy.From.Before(policy.Until) {
		return localui.Connection{}, localui.ErrConnectionInvalid
	}
	var id archive.CredentialID
	var err error
	create := archive.CredentialCreate{Label: request.Label, Policy: policy, ExpiresAt: *request.ExpiresAt}
	if request.Mode == "local" {
		id, _, err = a.issueNativeClientConfig(ctx, create, mcpserver.NativeAdapterEndpoint, request.Client)
	} else {
		id, err = a.issueRemoteClient(ctx, create, request.Client)
	}
	if err != nil {
		return localui.Connection{}, localui.ErrConnectionRecovery
	}
	snapshot, err := a.connectionSnapshot(ctx)
	if err != nil {
		return localui.Connection{}, err
	}
	for _, record := range a.authState.NativeClients {
		if record.CredentialID == id {
			for _, item := range snapshot.Items {
				if item.ID == record.IntentID {
					return item, nil
				}
			}
		}
	}
	for _, record := range a.authState.RemoteClients {
		if record.CredentialID == id {
			for _, item := range snapshot.Items {
				if item.ID == record.IntentID {
					return item, nil
				}
			}
		}
	}
	return localui.Connection{}, localui.ErrConnectionRecovery
}
func (a *Application) ConnectionGuide(ctx context.Context, request localui.ConnectionGuideRequest) (clientsetup.Guide, error) {
	if !validConnectionClient(request.Client) || request.ID == "" {
		return clientsetup.Guide{}, localui.ErrConnectionInvalid
	}
	if request.Mode != "local" && request.Mode != "remote" {
		return clientsetup.Guide{}, localui.ErrConnectionUnavailable
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed || !a.mcpEnabled {
		return clientsetup.Guide{}, localui.ErrConnectionUnavailable
	}
	if err := a.startMCP(ctx); err != nil {
		return clientsetup.Guide{}, localui.ErrConnectionRecovery
	}
	if request.Mode == "remote" {
		return a.remoteConnectionGuide(ctx, request)
	}
	local, _ := a.mcp.status()
	if local != "ready" || a.nativeRecoveryPending || a.authState.NativeInvalid {
		return clientsetup.Guide{}, localui.ErrConnectionRecovery
	}
	for _, record := range a.authState.NativeClients {
		if record.IntentID != request.ID {
			continue
		}
		if record.State != "ready" || record.Client != "" && record.Client != request.Client {
			return clientsetup.Guide{}, localui.ErrConnectionUnsupported
		}
		if !a.nativeRegistry.PublishedNativeClient(nativeBinding(record)) {
			return clientsetup.Guide{}, localui.ErrConnectionRecovery
		}
		metadata, err := a.listNative(ctx, record.Account, record.Principal)
		if err != nil || a.verifyNativeRecord(ctx, record, metadata) != nil {
			return clientsetup.Guide{}, localui.ErrConnectionRecovery
		}
		config, id, err := a.readNativeFile(record.ConfigName)
		if err != nil || id != record.CredentialID || config.NativeSocketPath != a.nativeEndpoint.SocketPath || config.Endpoint != mcpserver.NativeAdapterEndpoint {
			return clientsetup.Guide{}, localui.ErrConnectionRecovery
		}
		setup := clientsetup.SetupConfig{ExecutablePath: a.executablePath, LocalConfigPath: filepath.Join(a.paths.ClientDir, record.ConfigName), LocalCredentialID: string(record.CredentialID), Token: config.Token}
		if a.dockerContainerName != "" {
			setup.ExecutablePath = ""
			setup.Docker = &clientsetup.DockerRunner{Container: a.dockerContainerName}
		}
		guide, err := clientsetup.Render(clientsetup.Client(request.Client), clientsetup.Local, setup)
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
func (a *Application) RevokeConnection(ctx context.Context, request localui.ConnectionRevokeRequest) error {
	if request.ID == "" || request.Revision == 0 {
		return localui.ErrConnectionInvalid
	}
	a.ops.Lock()
	defer a.ops.Unlock()
	a.data.Lock()
	defer a.data.Unlock()
	if a.closed {
		return localui.ErrConnectionUnavailable
	}
	for _, record := range a.authState.NativeClients {
		if record.IntentID != request.ID {
			continue
		}
		metadata, err := a.listNative(ctx, record.Account, record.Principal)
		if err != nil {
			return localui.ErrConnectionUnavailable
		}
		revision := uint64(0)
		for _, row := range metadata {
			if row.ID == record.CredentialID && row.Account == record.Account && row.Principal == record.Principal {
				revision = row.Revision
				break
			}
		}
		if revision == 0 {
			return localui.ErrConnectionRecovery
		}
		if revision != request.Revision {
			return localui.ErrConnectionConflict
		}
		if err = a.store.RevokeCredential(ctx, record.CredentialID, request.Revision); err != nil {
			if errors.Is(err, archive.ErrPolicyChanged) {
				return localui.ErrConnectionConflict
			}
			return localui.ErrConnectionUnavailable
		}
		a.nativeRegistry.replace(nil)
		if err = a.reconcileNativeClients(ctx); err != nil {
			return localui.ErrConnectionRecovery
		}
		return nil
	}
	return a.revokeRemoteConnection(ctx, request)
}
