package app

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func shortNativeRoot(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(base, "wa-app-native-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return root
}
func unixNativeTestApp(t *testing.T) *Application {
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
	a.mcpEnabled = true
	if err = a.startMCP(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a
}
func writeLegacyNativeFixture(t *testing.T, a *Application, record nativeClientRecord) string {
	t.Helper()
	config, id, err := a.readNativeFile(record.ConfigName)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"version": 1, "credential_id": id, "endpoint": "http://127.0.0.1:12345/mcp", "token": config.Token})
	if err != nil {
		t.Fatal(err)
	}
	err = appdata.WithClients(a.paths, a.lease, func(root *os.Root) error {
		file, err := root.OpenFile(record.ConfigName, os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(payload)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
		return syncNativeDirectory(root)
	})
	if err != nil {
		t.Fatal(err)
	}
	return config.Token
}
func TestNativeV1MigrationPreservesConfiguredPathTokenAndGrant(t *testing.T) {
	a := unixNativeTestApp(t)
	id, path, err := a.IssueNativeClientConfig(context.Background(), archive.CredentialCreate{Label: "migrated", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, "", mcpserver.NativeAdapterEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	record := a.authState.NativeClients[0]
	token := writeLegacyNativeFixture(t, a, record)
	if err = a.reconcileNativeClients(context.Background()); err != nil {
		t.Fatal(err)
	}
	config, actualID, err := a.readNativeFile(record.ConfigName)
	if err != nil || actualID != id || config.Token != token || config.Endpoint != mcpserver.NativeAdapterEndpoint || config.NativeSocketPath != a.mcp.native.Path() {
		t.Fatal("migration changed identity or transport")
	}
	if filepath.Join(a.paths.ClientDir, a.authState.NativeClients[0].ConfigName) != path || a.authState.NativeClients[0].State != "ready" {
		t.Fatal("configured path moved")
	}
	metadata, err := a.listNative(context.Background(), demoAccount, record.Principal)
	if err != nil || len(metadata) != 1 || metadata[0].ID != id || metadata[0].Revoked {
		t.Fatal("migration created or revoked a different grant")
	}
}
func TestNativeMigrationFailedPendingPersistenceDoesNotRewriteOrPublish(t *testing.T) {
	a := unixNativeTestApp(t)
	_, _, err := a.IssueNativeClientConfig(context.Background(), archive.CredentialCreate{Label: "migration failure", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, "", mcpserver.NativeAdapterEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	record := a.authState.NativeClients[0]
	writeLegacyNativeFixture(t, a, record)
	err = appdata.WithWorking(a.paths, a.lease, func(root *os.Root) error {
		if err := root.Rename(stateFile, stateFile+".saved"); err != nil {
			return err
		}
		return root.Mkdir(stateFile, 0700)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.reconcileNativeClients(context.Background()); err == nil {
		t.Fatal("failed journal transition accepted")
	}
	config, _, err := a.readNativeFile(record.ConfigName)
	if err != nil || config.NativeSocketPath != "" || a.authState.NativeClients[0].State != "ready" || a.nativeRegistry.PublishedNativeClient(nativeBinding(record)) {
		t.Fatal("unacknowledged migration touched client authority")
	}
	err = appdata.WithWorking(a.paths, a.lease, func(root *os.Root) error {
		if err := root.Remove(stateFile); err != nil {
			return err
		}
		return root.Rename(stateFile+".saved", stateFile)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.reconcileNativeClients(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedNativeMigrationRevokesPendingRewrittenGrantOnReopen(t *testing.T) {
	a := unixNativeTestApp(t)
	id, _, err := a.IssueNativeClientConfig(context.Background(), archive.CredentialCreate{Label: "interrupted migration", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, "", mcpserver.NativeAdapterEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	record := a.authState.NativeClients[0]
	writeLegacyNativeFixture(t, a, record)
	expected, _, err := a.readNativeFile(record.ConfigName)
	if err != nil {
		t.Fatal(err)
	}
	a.authState.NativeClients[0].State = "pending"
	if err = a.persistPaused(a.paused); err != nil {
		t.Fatal(err)
	}
	if err = appdata.WithClients(a.paths, a.lease, func(root *os.Root) error {
		return mcpserver.RewriteCredentialFileRoot(root, record.ConfigName, id, expected, a.nativeEndpoint)
	}); err != nil {
		t.Fatal(err)
	}
	// This durable boundary is an interrupted migration: the transport file is
	// new, but ready publication has never been acknowledged.
	root := a.paths.Root
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(context.Background(), Config{DataDir: root, Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	if len(reopened.authState.NativeClients) != 0 || reopened.nativeRegistry.PublishedNativeClient(nativeBinding(record)) {
		t.Fatal("pending rewritten grant published")
	}
	metadata, err := reopened.listNative(context.Background(), demoAccount, record.Principal)
	if err != nil || len(metadata) != 1 || !metadata[0].Revoked {
		t.Fatal("interrupted migration grant survived recovery")
	}
	if _, _, err = reopened.readNativeFile(record.ConfigName); err == nil {
		t.Fatal("pending migration file survived acknowledged cleanup")
	}
}

func TestMalformedRemoteCapacityBlocksOnlyNewNativeIssuance(t *testing.T) {
	a := unixNativeTestApp(t)
	_, _, err := a.IssueNativeClientConfig(context.Background(), archive.CredentialCreate{Label: "existing native", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, "", mcpserver.NativeAdapterEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	record := a.authState.NativeClients[0]
	a.authState.RemoteInvalid = true
	a.authState.RemoteRaw = json.RawMessage(`{"untrusted":"unknown capacity"}`)
	if _, _, err = a.IssueNativeClientConfig(context.Background(), archive.CredentialCreate{Label: "new native", Policy: archive.CredentialPolicy{AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, "", mcpserver.NativeAdapterEndpoint); err == nil {
		t.Fatal("unknown combined capacity admitted")
	}
	if len(a.authState.NativeClients) != 1 || !a.nativeRegistry.PublishedNativeClient(nativeBinding(record)) {
		t.Fatal("remote corruption disabled existing native binding")
	}
	if err = a.reconcileNativeClients(context.Background()); err != nil || !a.nativeRegistry.PublishedNativeClient(nativeBinding(record)) {
		t.Fatal("remote corruption broadened native recovery fence")
	}
}
func TestConnectionsGETDoesNotBindOrRewrite(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: shortNativeRoot(t), Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	before, err := os.ReadFile(filepath.Join(a.paths.WorkingDir, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	a.mcpEnabled = true
	snapshot, err := a.Connections(context.Background())
	if err != nil || snapshot.LocalState == "ready" {
		t.Fatal("unbound GET claimed ready")
	}
	after, err := os.ReadFile(filepath.Join(a.paths.WorkingDir, stateFile))
	if err != nil || string(before) != string(after) {
		t.Fatal("GET rewrote auth state")
	}
	if a.mcp != nil {
		t.Fatal("GET bound transport")
	}
}
func TestDefaultNativeListenerStartsAfterTrustedAccountBinding(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: shortNativeRoot(t), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	policy, err := a.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.seedDemo(context.Background(), policy.Generation); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.data.RLock()
		ready := a.mcp != nil && a.mcp.localStarted
		a.data.RUnlock()
		if ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("default composition did not start native Unix listener")
}
func TestMCPShutdownTimeoutRetainsInstanceAndSocketGuardUntilHTTPDrain(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: shortNativeRoot(t), Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	socket, err := appdata.ListenNativeSocket(context.Background(), a.paths, a.lease)
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	a.mcp = &mcpListeners{native: socket}
	a.mcp.serve(context.Background(), socket.Listener(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; w.WriteHeader(204) }))
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return appdata.DialNativeSocket(ctx, socket.Path())
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	requested := make(chan struct{})
	go func() {
		defer close(requested)
		response, err := client.Get(mcpserver.NativeAdapterEndpoint)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("checked Unix request did not reach server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err = a.Close(ctx); err == nil {
		t.Fatal("live HTTP owner drained synthetically")
	}
	if lease, err := appdata.Acquire(a.paths); err == nil {
		lease.Close()
		t.Fatal("instance released before native HTTP drain")
	}
	close(release)
	<-requested
	if err = a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if lease, err := appdata.Acquire(a.paths); err != nil {
		t.Fatal("guard retained after full drain")
	} else {
		lease.Close()
	}
	if _, err = os.Lstat(socket.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned socket survived acknowledged cleanup")
	}
}
