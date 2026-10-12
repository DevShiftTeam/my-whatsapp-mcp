package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
)

func TestLiveRebuildDateRequiresExplicitYearAndTimezone(t *testing.T) {
	cfg := LiveRebuildBootstrapConfig{PolicyStartDate: "01-01", PolicyYear: 2026, PolicyTimezone: "Asia/Jerusalem"}
	got, err := liveRebuildPolicyDate(cfg)
	if err != nil || !got.Equal(time.Date(2025, 12, 31, 22, 0, 0, 0, time.UTC)) {
		t.Fatalf("local midnight = %v, %v", got, err)
	}
	for _, bad := range []LiveRebuildBootstrapConfig{{PolicyStartDate: "01-01"}, {PolicyStartDate: "01-01", PolicyYear: 2026}, {PolicyStartDate: "02-30", PolicyYear: 2026, PolicyTimezone: "UTC"}} {
		if _, err := liveRebuildPolicyDate(bad); err == nil {
			t.Fatal("ambiguous/invalid policy date accepted")
		}
	}
}

func TestLiveRebuildPendingDestinationNeverStarts(t *testing.T) {
	paths, err := appdata.Prepare(shortNativeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Root, liveRebuildPendingFile), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	if guard, err := AcquireLiveRebuildSourceGuard(paths.Root, nil); err == nil || guard != nil {
		t.Fatal("incomplete destination admitted")
	}
}

func TestLiveRebuildOfflineNativeRebindPreservesMultipleReadyRecords(t *testing.T) {
	paths, err := appdata.Prepare(shortNativeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	oldSocket := "/private/synthetic-old/working/native.sock"
	newSocket := filepath.Join(paths.WorkingDir, "native.sock")
	records := []nativeClientRecord{{CredentialID: "id-one", ConfigName: "one.json", State: "ready"}, {CredentialID: "id-two", ConfigName: "two.json", State: "ready"}}
	before := append([]nativeClientRecord(nil), records...)
	for _, r := range records {
		data, _ := json.Marshal(map[string]any{"version": 2, "credential_id": r.CredentialID, "endpoint": mcpserver.NativeAdapterEndpoint, "native_socket_path": oldSocket, "token": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"})
		if err := os.WriteFile(filepath.Join(paths.ClientDir, r.ConfigName), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := rebindLiveRebuildNative(context.Background(), paths, lease, records, oldSocket, newSocket); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(records, before) {
		t.Fatal("ready records changed")
	}
	for _, r := range records {
		var cfg mcpserver.AdapterConfig
		var id archive.CredentialID
		err := appdata.WithClients(paths, lease, func(root *os.Root) error {
			var e error
			cfg, id, e = mcpserver.ReadCredentialFileRoot(root, r.ConfigName)
			return e
		})
		if err != nil || id != r.CredentialID || cfg.NativeSocketPath != newSocket || cfg.Token != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" {
			t.Fatalf("rebind changed identity: %v", err)
		}
	}
	// A journal is a one-shot offline operation, not an implicit startup repair.
	if err := rebindLiveRebuildNative(context.Background(), paths, lease, records, oldSocket, newSocket); err == nil {
		t.Fatal("replayed native migration")
	}
}

func TestLiveRebuildRejectsPendingNativeBeforeRewrite(t *testing.T) {
	paths, err := appdata.Prepare(shortNativeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := rebindLiveRebuildNative(context.Background(), paths, lease, []nativeClientRecord{{State: "pending"}}, "/old/native.sock", "/new/native.sock"); err == nil {
		t.Fatal("pending record accepted")
	}
}

func syntheticLiveRebuild(t *testing.T) (LiveRebuildBootstrapConfig, map[string][]byte) {
	t.Helper()
	ctx := context.Background()
	source, err := appdata.Prepare(shortNativeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(source)
	if err != nil {
		t.Fatal(err)
	}
	store, err := archive.Open(ctx, source.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	account := "15550000001@s.whatsapp.net"
	policy, err := store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: account, Chat: "15550000002@s.whatsapp.net", Sender: "15550000002@s.whatsapp.net", Message: "old-body"}, Kind: archive.EventOriginal, Body: "MUST-NOT-BECOME-LIVE", BodyKind: archive.MessageText, OriginalSentAt: time.Now().Add(-time.Hour), OriginalDateKnown: true, SourceRevision: 1}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	state := applicationState{Version: 1, Mode: "real", Paused: true, DesiredConnected: true, LocalAI: defaultLocalAIRecord()}
	for i := 1; i <= 4; i++ {
		intent := fmt.Sprintf("%032x", i)
		request := archive.CredentialCreate{Label: "Synthetic client", Policy: archive.CredentialPolicy{Account: account, AllChats: true}, ExpiresAt: time.Now().Add(24 * time.Hour)}
		if i <= 2 {
			r := nativeClientRecord{Version: 1, IntentID: intent, Account: account, Principal: archive.NativeClientPrincipalPrefix + "v1/" + intent, ConfigName: "native-" + intent + ".json", State: "ready"}
			request.Principal = r.Principal
			err = appdata.WithClients(source, lease, func(root *os.Root) error {
				c, e := mcpserver.IssueCredentialFile(ctx, store, request, root, r.ConfigName, mcpserver.NativeEndpoint{SocketPath: filepath.Join(source.WorkingDir, "native.sock")})
				r.CredentialID = c.ID
				return e
			})
			if err != nil {
				t.Fatal(err)
			}
			state.NativeClients = append(state.NativeClients, r)
		} else {
			r := remoteClientRecord{Version: 1, Mode: "remote_bearer", IntentID: intent, Account: account, Principal: archive.RemoteClientPrincipalPrefix + "v1/" + intent, ConfigName: "remote-" + intent + ".json", State: "ready"}
			request.Principal = r.Principal
			err = appdata.WithClients(source, lease, func(root *os.Root) error {
				c, e := mcpserver.IssueRemoteCredentialFile(ctx, store, request, root, r.ConfigName, mcpserver.RemoteEndpoint{PublicURL: "https://synthetic.example.com/mcp"})
				r.CredentialID = c.ID
				return e
			})
			if err != nil {
				t.Fatal(err)
			}
			state.RemoteClients = append(state.RemoteClients, r)
		}
	}
	if err := writeState(source, lease, state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.WorkingDir, "admin.env"), []byte("SYNTHETIC_CONFIG=preserved\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.MediaDir, "old-original.bin"), []byte("OLD-MEDIA-MUST-NOT-COPY"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUnsafe(ctx, account, "drain_failed"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(source.SessionDir, "session.sqlite")
	if err := os.WriteFile(sessionPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", sessionPath+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	container := sqlstore.NewWithDB(db, "sqlite3", waLog.Noop)
	if err := container.Upgrade(ctx); err != nil {
		t.Fatal(err)
	}
	device := container.NewDevice()
	jid := types.NewADJID("15550000001", 0, 1)
	device.ID = &jid
	device.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := device.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if err := container.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := appdata.Prepare(shortNativeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	bl, err := appdata.Acquire(backup)
	if err != nil {
		t.Fatal(err)
	}
	bl.Close()
	files := map[string][]byte{}
	names := []string{"archive/archive.sqlite", "archive/credentials-denied.marker", "session/session.sqlite", "working/" + stateFile, "working/admin.env"}
	for _, r := range state.NativeClients {
		names = append(names, "clients/"+r.ConfigName)
	}
	for _, r := range state.RemoteClients {
		names = append(names, "clients/"+r.ConfigName)
	}
	manifest := LiveRebuildBackupManifest{Version: 1, SourceProfileDir: source.Root, Account: account, DeviceID: jid.String(), CapturedAt: time.Now().Add(-time.Second), Files: map[string]string{}}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(source.Root, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = data
		manifest.Files[name] = liveRebuildDigest(data)
		if err := os.WriteFile(filepath.Join(backup.Root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(backup.Root, LiveRebuildBackupManifestFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := LiveRebuildBootstrapConfig{SourceProfileDir: source.Root, CurrentBackupDir: backup.Root, DestinationProfileDir: filepath.Join(shortNativeRoot(t), "destination"), ExpectedBackupSHA256: liveRebuildDigest(data), ExpectedAccountID: account, ExpectedDeviceID: jid.String(), Boundary: time.Now()}
	guard := LiveRebuildSourceGuard{Version: 1, SourceProfileDir: source.Root, DestinationProfileDir: cfg.DestinationProfileDir, DeviceID: jid.String(), BackupSHA256: cfg.ExpectedBackupSHA256, SourceStopped: true}
	data, _ = json.Marshal(guard)
	cfg.SourceStoppedGuardPath = appdata.LiveRebuildSourceGuardPath(source.Root)
	cfg.SourceStoppedGuardSHA256 = liveRebuildDigest(data)
	if err := os.WriteFile(cfg.SourceStoppedGuardPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(cfg.SourceStoppedGuardPath) })
	return cfg, files
}

func TestLiveRebuildBootstrapPreservesCurrentPrivateStateAndIsCreateOnly(t *testing.T) {
	cfg, before := syntheticLiveRebuild(t)
	receipt, err := BootstrapLiveRebuild(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Archive.Account != cfg.ExpectedAccountID || receipt.Archive.DeviceID != cfg.ExpectedDeviceID {
		t.Fatal("device/account identity changed")
	}
	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(cfg.SourceProfileDir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("source bytes changed")
		}
	}
	for _, name := range []string{"working/" + stateFile, "working/admin.env", "session/session.sqlite"} {
		got, err := os.ReadFile(filepath.Join(cfg.DestinationProfileDir, name))
		if err != nil || !bytes.Equal(got, before[name]) {
			t.Fatalf("private metadata changed: %s", name)
		}
	}
	entries, err := os.ReadDir(filepath.Join(cfg.DestinationProfileDir, "media"))
	if err != nil || len(entries) != 0 {
		t.Fatal("old media copied")
	}
	db, err := sql.Open("sqlite3", filepath.Join(cfg.DestinationProfileDir, "archive/archive.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM messages").Scan(&n); err != nil || n != 0 {
		t.Fatal("old message bodies copied")
	}
	replayed, err := BootstrapLiveRebuild(context.Background(), cfg)
	if err != nil || !reflect.DeepEqual(receipt, replayed) {
		t.Fatalf("idempotent receipt changed: %v", err)
	}
	changed := cfg
	changed.PolicyStartDate = "01-01"
	changed.PolicyYear = 2026
	changed.PolicyTimezone = "UTC"
	if _, err := BootstrapLiveRebuild(context.Background(), changed); err == nil {
		t.Fatal("different request reused published destination")
	}
	guard, err := AcquireLiveRebuildSourceGuard(cfg.DestinationProfileDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	paths, err := appdata.ExistingPaths(cfg.SourceProfileDir)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := appdata.AcquireExisting(paths); err == nil {
		l.Close()
		t.Fatal("original device owner could restart")
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	live, err := New(context.Background(), Config{DataDir: cfg.DestinationProfileDir, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close(context.Background())
	if live.rebuild == nil || live.rebuildSourceLease == nil || live.liveRebuildMetadata().Coverage != "partial" {
		t.Fatal("normal application did not compose rebuild provenance")
	}
	if l, err := appdata.AcquireExisting(paths); err == nil {
		l.Close()
		t.Fatal("application did not retain source ownership")
	}
	p, err := live.store.Policy(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.DateMode = archive.DateSince
	p.ImportStart = time.Now().Add(-2 * time.Hour)
	if _, err := live.SetPolicy(context.Background(), p, p.Generation); err != nil {
		t.Fatal("rebuilt policy not editable")
	}
	if err := live.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	released, err := appdata.AcquireExisting(paths)
	if err != nil {
		t.Fatal("closed application retained source lease")
	}
	released.Close()
}

func TestLiveRebuildBootstrapRejectsChangedBackupAndLiveSource(t *testing.T) {
	for _, kind := range []string{"changed_source", "changed_backup", "live_source", "uncheckpointed_session"} {
		t.Run(kind, func(t *testing.T) {
			cfg, _ := syntheticLiveRebuild(t)
			switch kind {
			case "changed_source", "changed_backup":
				root := cfg.SourceProfileDir
				if kind == "changed_backup" {
					root = cfg.CurrentBackupDir
				}
				path := filepath.Join(root, "working/admin.env")
				if err := os.WriteFile(path, []byte("changed\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "live_source":
				paths, err := appdata.ExistingPaths(cfg.SourceProfileDir)
				if err != nil {
					t.Fatal(err)
				}
				lease, err := appdata.AcquireExisting(paths)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Close()
			case "uncheckpointed_session":
				if err := os.WriteFile(filepath.Join(cfg.CurrentBackupDir, "session/session.sqlite-wal"), []byte("uncheckpointed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := BootstrapLiveRebuild(context.Background(), cfg); err == nil {
				t.Fatal("unsafe current backup/source accepted")
			}
			if _, err := os.Lstat(cfg.DestinationProfileDir); !os.IsNotExist(err) {
				t.Fatal("failed validation created destination")
			}
		})
	}
}

func TestLiveRebuildBootstrapRejectsPendingCredentialDenial(t *testing.T) {
	for _, location := range []string{"source", "backup"} {
		for _, kind := range []string{"empty", "credential", "directory", "symlink"} {
			t.Run(location+"/"+kind, func(t *testing.T) {
				cfg, before := syntheticLiveRebuild(t)
				root := cfg.SourceProfileDir
				if location == "backup" {
					root = cfg.CurrentBackupDir
				}
				pending := filepath.Join(root, "archive/credentials-denied.marker.pending")
				switch kind {
				case "empty", "credential":
					var data []byte
					if kind == "credential" {
						var state applicationState
						if err := json.Unmarshal(before["working/"+stateFile], &state); err != nil {
							t.Fatal(err)
						}
						data = []byte("v1\n" + string(state.NativeClients[0].CredentialID) + "\n")
					}
					if err := os.WriteFile(pending, data, 0600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(pending, 0700); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink("missing-denial", pending); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := BootstrapLiveRebuild(context.Background(), cfg); err == nil {
					t.Fatal("pending credential denial omitted from authority")
				}
				if _, err := os.Lstat(cfg.DestinationProfileDir); !os.IsNotExist(err) {
					t.Fatal("pending credential denial created destination")
				}
				for name, want := range before {
					got, err := os.ReadFile(filepath.Join(cfg.SourceProfileDir, name))
					if err != nil || !bytes.Equal(got, want) {
						t.Fatalf("source changed: %s", name)
					}
				}
			})
		}
	}
}

func TestLiveRebuildInterruptedNativeRebindRemainsUnpublished(t *testing.T) {
	paths, err := appdata.Prepare(shortNativeRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := os.WriteFile(filepath.Join(paths.Root, liveRebuildPendingFile), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	oldSocket := "/private/synthetic/working/native.sock"
	newSocket := filepath.Join(paths.WorkingDir, "native.sock")
	records := []nativeClientRecord{{CredentialID: "one", ConfigName: "a.json", State: "ready"}, {CredentialID: "two", ConfigName: "b.json", State: "ready"}}
	for i, r := range records {
		socket := oldSocket
		if i == 1 {
			socket = "/private/wrong/working/native.sock"
		}
		data, _ := json.Marshal(map[string]any{"version": 2, "credential_id": r.CredentialID, "endpoint": mcpserver.NativeAdapterEndpoint, "native_socket_path": socket, "token": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"})
		if err := os.WriteFile(filepath.Join(paths.ClientDir, r.ConfigName), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := rebindLiveRebuildNative(context.Background(), paths, lease, records, oldSocket, newSocket); err == nil {
		t.Fatal("mismatched second client accepted")
	}
	first, err := mcpserver.ReadCredentialFile(filepath.Join(paths.ClientDir, "a.json"))
	if err != nil || first.NativeSocketPath != newSocket {
		t.Fatal("fixture did not reach interrupted rewrite")
	}
	if _, err := os.Stat(filepath.Join(paths.WorkingDir, "live-rebuild-native-pending.json")); err != nil {
		t.Fatal("interruption lost journal")
	}
	if _, err := os.Stat(filepath.Join(paths.WorkingDir, "live-rebuild-native-ready.json")); !os.IsNotExist(err) {
		t.Fatal("interrupted journal marked ready")
	}
	if guard, err := AcquireLiveRebuildSourceGuard(paths.Root, nil); err == nil || guard != nil {
		t.Fatal("partially rebound destination started")
	}
}

func TestLiveRebuildExternalPrivateConfigAndInheritedGuard(t *testing.T) {
	cfg, before := syntheticLiveRebuild(t)
	external := filepath.Join(shortNativeRoot(t), "admin.env")
	if err := os.WriteFile(external, before["working/admin.env"], 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cfg.SourceProfileDir, "working/admin.env")); err != nil {
		t.Fatal(err)
	}
	cfg.PrivateConfigFiles = []LiveRebuildPrivateConfig{{SourcePath: external, DestinationPath: "working/admin.env"}}
	paths, err := appdata.ExistingPaths(cfg.SourceProfileDir)
	if err != nil {
		t.Fatal(err)
	}
	original, err := appdata.AcquireExisting(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	shared, err := original.WorkerGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	fd := int(shared.Fd())
	cfg.SourceLockFD = &fd
	if _, err := BootstrapLiveRebuild(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := shared.Stat(); err != nil {
		t.Fatal("bootstrap closed operator source authority")
	}
	guard, err := AcquireLiveRebuildSourceGuard(cfg.DestinationProfileDir, &fd)
	if err != nil {
		t.Fatal(err)
	}
	guard.Close()
	cfg.SourceLockFD = nil // receipt identity does not depend on an ephemeral fd.
	if _, err := BootstrapLiveRebuild(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(cfg.DestinationProfileDir, "working/admin.env"))
	if err != nil || !bytes.Equal(got, before["working/admin.env"]) {
		t.Fatal("external admin config not preserved")
	}
}
