package app

import (
	"context"
	"crypto/sha256"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"os"
	"testing"
	"time"
)

func TestNativeListenerRequiresPersistedIndependentGrant(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	_, err = a.MCPHandler(mcpserver.Config{Mode: mcpserver.LocalAdapter, Hosts: []string{"127.0.0.1:12345"}})
	if err == nil {
		t.Fatal("native listener acquired implicit owner authority")
	}
	if a.nativeCredentialID != archive.CredentialID("") {
		t.Fatal("startup created a native grant")
	}
}

func pairedNativeTestApp(t *testing.T) *Application {
	t.Helper()
	a, err := New(context.Background(), Config{DataDir: shortNativeRoot(t), Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	policy, err := a.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.seedDemo(context.Background(), policy.Generation); err != nil {
		t.Fatal(err)
	}
	a.nativeEndpoint = mcpserver.NativeEndpoint{SocketPath: a.paths.WorkingDir + "/native.sock"}
	if err = a.reconcileNativeClients(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a
}
func TestNativeIssuancePublishesOnlyDurableBoundIdentity(t *testing.T) {
	a := pairedNativeTestApp(t)
	id, path, err := a.IssueNativeClientConfig(context.Background(), archive.CredentialCreate{
		Label: "explicit client", Policy: archive.CredentialPolicy{Account: "caller-untrusted", AllChats: true}, ExpiresAt: time.Now().Add(time.Hour),
	}, "../../caller-path", mcpserver.NativeAdapterEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || path == "" || len(a.authState.NativeClients) != 1 {
		t.Fatal("missing durable identity")
	}
	record := a.authState.NativeClients[0]
	if record.Account != demoAccount || record.ConfigName == "../../caller-path" || record.State != "ready" || !a.nativeRegistry.PublishedNativeClient(nativeBinding(record)) {
		t.Fatal("authority was not server bound")
	}
	state, exists, err := readState(a.paths, a.lease)
	if err != nil || !exists || len(state.NativeClients) != 1 || state.NativeClients[0] != record {
		t.Fatal("published state not durable")
	}
	config, fileID, err := a.readNativeFile(record.ConfigName)
	if err != nil || fileID != id {
		t.Fatal("private config disagrees with durable ID")
	}
	grant, err := a.store.ResolveCredential(context.Background(), sha256.Sum256([]byte(config.Token)))
	if err != nil || grant.Scope.Account != demoAccount || grant.Scope.Principal != record.Principal {
		t.Fatal("caller account escaped binding")
	}
}
func TestPendingNativeRecoveryRevokesAndRemovesOnlyOwnedFile(t *testing.T) {
	a := pairedNativeTestApp(t)
	nonce, err := nonce128()
	if err != nil {
		t.Fatal(err)
	}
	record := nativeClientRecord{Version: 1, IntentID: nonce, Account: demoAccount, Principal: archive.NativeClientPrincipalPrefix + "v1/" + nonce, ConfigName: "native-" + nonce + ".json", State: "pending"}
	a.authState.NativeClients = []nativeClientRecord{record}
	if err = a.persistPaused(a.paused); err != nil {
		t.Fatal(err)
	}
	var credential archive.Credential
	err = appdata.WithClients(a.paths, a.lease, func(root *os.Root) error {
		var err error
		credential, err = mcpserver.IssueCredentialFile(context.Background(), a.store, archive.CredentialCreate{Label: "pending", Principal: record.Principal, Policy: archive.CredentialPolicy{Account: demoAccount, AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, root, record.ConfigName, a.nativeEndpoint)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate death before returned ID was journaled: ordered metadata recovers it.
	if err = a.reconcileNativeClients(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(a.authState.NativeClients) != 0 || a.nativeRegistry.PublishedNativeClient(mcpserver.NativeClientBinding{Account: demoAccount, Principal: record.Principal, CredentialID: credential.ID}) {
		t.Fatal("pending identity became published")
	}
	if _, err = a.store.ResolveCredentialID(context.Background(), credential.ID); err == nil {
		t.Fatal("pending grant survived recovery")
	}
	if _, _, err = a.readNativeFile(record.ConfigName); err == nil {
		t.Fatal("owned pending file survived recovery")
	}
}
func TestInvalidReadyConfigRetainsRecoveryFenceAndIdentity(t *testing.T) {
	a := pairedNativeTestApp(t)
	id, _, err := a.IssueNativeClientConfig(context.Background(), archive.CredentialCreate{Label: "ready", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, "", mcpserver.NativeAdapterEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	record := a.authState.NativeClients[0]
	err = appdata.WithClients(a.paths, a.lease, func(root *os.Root) error { return root.Remove(record.ConfigName) })
	if err != nil {
		t.Fatal(err)
	}
	if err = a.reconcileNativeClients(context.Background()); err == nil {
		t.Fatal("missing ready config reopened MCP")
	}
	if !a.nativeRecoveryPending || len(a.authState.NativeClients) != 1 || a.authState.NativeClients[0].CredentialID != id || a.nativeRegistry.PublishedNativeClient(nativeBinding(record)) {
		t.Fatal("failed recovery dropped ownership")
	}
}

func TestExpiredReadyRetirementDurablyPreventsClockRevival(t *testing.T) {
	a := pairedNativeTestApp(t)
	id, _, err := a.IssueNativeClientConfig(context.Background(), archive.CredentialCreate{
		Label: "expires", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: time.Now().Add(time.Hour),
	}, "", mcpserver.NativeAdapterEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	record := a.authState.NativeClients[0]
	config, _, err := a.readNativeFile(record.ConfigName)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the application's server clock moving forward, then returning:
	// durable CAS revoke must defeat the still-before-expiry archive clock.
	if err = a.reconcileNativeClientsAt(context.Background(), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.ResolveCredential(context.Background(), sha256.Sum256([]byte(config.Token))); err == nil {
		t.Fatal("expired grant revived when the clock returned")
	}
	if len(a.authState.NativeClients) != 0 || a.nativeRegistry.PublishedNativeClient(nativeBinding(record)) {
		t.Fatal("inactive client remained published")
	}
	if _, _, err = a.readNativeFile(record.ConfigName); err == nil {
		t.Fatal("retired owned token file remained")
	}
	state, exists, err := readState(a.paths, a.lease)
	if err != nil || !exists || len(state.NativeClients) != 0 {
		t.Fatal("retirement was not durably journaled")
	}
	if _, err = a.store.ResolveCredentialID(context.Background(), id); err == nil {
		t.Fatal("retired ID remained active")
	}
}
func TestInactiveReadyRetirementRequiresExactMetadataIdentity(t *testing.T) {
	record := nativeClientRecord{Account: "account", Principal: "principal", CredentialID: "id"}
	now := time.Now().UTC()
	for _, metadata := range []archive.CredentialMetadata{
		{ID: "different", Account: "account", Principal: "principal", Revoked: true},
		{ID: "id", Account: "different", Principal: "principal", Revoked: true},
		{ID: "id", Account: "account", Principal: "different", Revoked: true},
	} {
		if inactiveNativeRecord(record, []archive.CredentialMetadata{metadata}, now) {
			t.Fatal("foreign metadata retired a ready client")
		}
	}
}

func TestInterruptedInactiveCleanupResumesPendingJournal(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonce", true: "legacy"}[legacy], func(t *testing.T) {
			a := pairedNativeTestApp(t)
			root := a.paths.Root
			id, _, err := a.IssueNativeClientConfig(context.Background(), archive.CredentialCreate{Label: "retirement", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, "", mcpserver.NativeAdapterEndpoint)
			if err != nil {
				t.Fatal(err)
			}
			record := a.authState.NativeClients[0]
			if legacy {
				// Create the historical principal/file form with its exact persisted hint.
				if err = a.revokeNativeID(context.Background(), record.Account, record.Principal, id); err != nil {
					t.Fatal(err)
				}
				if err = a.removeNativeFile(record); err != nil {
					t.Fatal(err)
				}
				record.Principal = archive.LegacyNativeClientPrincipal
				record.ConfigName = "existing-client.json"
				var created archive.Credential
				err = appdata.WithClients(a.paths, a.lease, func(root *os.Root) error {
					var err error
					created, err = mcpserver.IssueCredentialFile(context.Background(), a.store, archive.CredentialCreate{Label: "legacy", Principal: record.Principal, Policy: archive.CredentialPolicy{Account: demoAccount, AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, root, record.ConfigName, a.nativeEndpoint)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				record.CredentialID = created.ID
				a.nativeCredentialID = created.ID
			}
			if err = a.revokeNativeID(context.Background(), record.Account, record.Principal, record.CredentialID); err != nil {
				t.Fatal(err)
			}
			record.State = "pending"
			a.authState.NativeClients = []nativeClientRecord{record}
			if err = a.persistPaused(a.paused); err != nil {
				t.Fatal(err)
			}
			if err = a.removeNativeFile(record); err != nil {
				t.Fatal(err)
			}
			// Simulate interruption after physical cleanup/directory sync, before
			// final journal removal. New owner must resume the same intent, not Create.
			if err = a.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			reopened, err := New(context.Background(), Config{DataDir: root, Demo: true, Offline: true})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(context.Background())
			if reopened.nativeRecoveryPending || len(reopened.authState.NativeClients) != 0 || reopened.nativeCredentialID != "" {
				t.Fatal("interrupted retirement remained permanently pending")
			}
		})
	}
}

func TestFailedPendingPersistenceRetainsReadyForSafeRetry(t *testing.T) {
	a := pairedNativeTestApp(t)
	_, _, err := a.IssueNativeClientConfig(context.Background(), archive.CredentialCreate{Label: "persist retry", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, "", mcpserver.NativeAdapterEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	record := a.authState.NativeClients[0]
	if err = a.revokeNativeID(context.Background(), record.Account, record.Principal, record.CredentialID); err != nil {
		t.Fatal(err)
	}
	// A private empty directory at the atomic-rename destination induces an
	// actual pending-write failure without changing root/clients privacy.
	err = appdata.WithWorking(a.paths, a.lease, func(root *os.Root) error {
		if err := root.Rename(stateFile, ".test-saved-state"); err != nil {
			return err
		}
		return root.Mkdir(stateFile, 0700)
	})
	if err != nil {
		t.Fatal(err)
	}
	restore := func() error {
		return appdata.WithWorking(a.paths, a.lease, func(root *os.Root) error {
			if err := root.Remove(stateFile); err != nil {
				return err
			}
			return root.Rename(".test-saved-state", stateFile)
		})
	}
	if err = a.reconcileNativeClients(context.Background()); err == nil {
		t.Fatal("pending-write failure was accepted")
	}
	if a.authState.NativeClients[0].State != "ready" {
		t.Fatal("failed pending write left unsafe in-memory pending")
	}
	if _, id, err := a.readNativeFile(record.ConfigName); err != nil || id != record.CredentialID {
		t.Fatal("failed pending write unlinked owned ready file")
	}
	if err = restore(); err != nil {
		t.Fatal(err)
	}
	if err = a.reconcileNativeClients(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(a.authState.NativeClients) != 0 {
		t.Fatal("safe retry did not retire inactive record")
	}
}
