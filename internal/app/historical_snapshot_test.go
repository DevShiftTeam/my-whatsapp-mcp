package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"github.com/Bodokh/my-whatsapp-mcp/internal/mcpserver"
	"github.com/Bodokh/my-whatsapp-mcp/internal/outbound"
	"github.com/Bodokh/my-whatsapp-mcp/internal/remoteaccess"
	"github.com/Bodokh/my-whatsapp-mcp/internal/transcription"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func historicalFixture(t *testing.T, configure ...func(appdata.Paths, *appdata.InstanceLock, *archive.Store)) (string, time.Time) {
	t.Helper()
	ctx := context.Background()
	paths, err := appdata.Prepare(privateTestRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(paths)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeState(paths, lease, applicationState{Version: 1, Mode: "real"}); err != nil {
		t.Fatal(err)
	}
	s, err := archive.Open(ctx, paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: "snapshot-account", Chat: "snapshot-chat", Sender: "snapshot-sender", Message: "one"}, Kind: archive.EventOriginal, Body: "historical canary", BodyKind: archive.MessageText, OriginalSentAt: time.Now().Add(-time.Hour), OriginalDateKnown: true, SourceRevision: 1}, p.Generation)
	if err != nil {
		t.Fatal(err)
	}
	for _, setup := range configure {
		setup(paths, lease, s)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	captured := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	marker, _ := json.Marshal(map[string]any{"version": 1, "captured_at": captured, "authorization_verified_at": time.Now().UTC(), "current_authorization_verified": true})
	if err = os.WriteFile(filepath.Join(paths.ArchiveDir, archive.HistoricalSnapshotMarker), marker, 0600); err != nil {
		t.Fatal(err)
	}
	return paths.Root, captured
}

func TestHistoricalSnapshotNeverConstructsOwnersAndDeniesMutations(t *testing.T) {
	ctx := context.Background()
	root, captured := historicalFixture(t)
	core := filepath.Join(root, "archive", "archive.sqlite")
	before, err := os.ReadFile(core)
	if err != nil {
		t.Fatal(err)
	}
	factory := func(ingestion.Config, archive.ConnectorStore, ingestion.Callbacks) (connector, error) {
		t.Fatal("connector factory invoked")
		return nil, nil
	}
	resourcesFactory := func(context.Context, appdata.Paths, *archive.Store, Config, bool) (*resources, error) {
		t.Fatal("ordinary resource factory invoked")
		return nil, nil
	}
	a, err := newApplicationWithResources(ctx, Config{DataDir: root, Snapshot: &SnapshotConfig{CapturedAt: captured}}, factory, resourcesFactory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	if a.runtime != nil || a.localAI != nil || a.resources.voice != nil || a.resources.media != nil || a.resources.models != nil || a.resources.runDone != nil || a.connectionOwner.done != nil {
		t.Fatal("snapshot constructed runtime owner")
	}
	entries, err := os.ReadDir(filepath.Join(root, "session"))
	if err != nil || len(entries) != 0 {
		t.Fatal("session was opened")
	}
	for name, run := range map[string]func() error{
		"budget":          func() error { _, e := a.SetOriginalBudget(ctx, localui.OriginalBudgetRequest{}); return e },
		"model-operation": func() error { _, e := a.StartModelOperation(ctx, localui.ModelOperationRequest{}); return e },
		"model-cancel":    func() error { return a.CancelModelOperation(ctx, "any") },
		"revoke":          func() error { return a.RevokeConnection(ctx, localui.ConnectionRevokeRequest{}) },
		"guide":           func() error { _, e := a.ConnectionGuide(ctx, localui.ConnectionGuideRequest{}); return e },
		"reveal":          func() error { _, e := a.RevealRemoteCredential(ctx, localui.ConnectionRevealRequest{}); return e },
		"issue":           func() error { _, _, e := a.IssueNativeClientConfig(ctx, archive.CredentialCreate{}, "", ""); return e },
		"send-permission": func() error { return a.SetSendPermission(ctx, "", 0, nil) },
		"send-mode":       func() error { return a.SetSendPermissionMode(ctx, "", 0, archive.SendPermission{}, 0) },
		"qr":              func() error { _, e := a.QRPNG(ctx); return e },
		"pair":            func() error { return a.StartPairing(ctx) }, "pause": func() error { return a.Pause(ctx) }, "resume": func() error { return a.Resume(ctx) }, "disconnect": func() error { return a.Disconnect(ctx) }, "reset": func() error { return a.DeleteLocalData(ctx, true) }, "install": func() error { return a.InstallModel(ctx, "any") }, "verify": func() error { return a.VerifyModel(ctx, "any") }, "remove": func() error { return a.RemoveModel(ctx, "any") }, "policy": func() error { _, e := a.SetPolicy(ctx, archive.ArchivePolicy{}, 0); return e }, "send": func() error { _, e := a.Send(ctx, archive.CredentialGrant{}, outbound.Request{}); return e }, "voice": func() error { _, e := a.RetryVoiceDownloads(ctx); return e }, "documents": func() error { _, e := a.RetryDocumentDownloads(ctx, nil); return e }, "grants": func() error { _, e := a.CreateConnection(ctx, localui.ConnectionRequest{}); return e }, "local-ai": func() error { _, e := a.SetLocalAI(ctx, localui.LocalAIUpdate{}); return e },
	} {
		t.Run(name, func(t *testing.T) {
			if e := run(); !errors.Is(e, archive.ErrHistoricalSnapshotReadOnly) {
				t.Fatalf("mutation did not deny explicitly: %v", e)
			}
		})
	}
	if _, e := a.OriginalBudget(ctx); e != nil {
		t.Fatalf("budget read: %v", e)
	}
	if _, e := a.Connections(ctx); e != nil {
		t.Fatalf("connection read: %v", e)
	}
	if _, e := a.ModelState(ctx); e != nil {
		t.Fatalf("model state read: %v", e)
	}
	if _, e := a.Stats(ctx, archive.StatsRequest{}); e != nil {
		t.Fatalf("stats read: %v", e)
	}
	page, err := a.Search(ctx, archive.SearchRequest{Query: "canary"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("read failed: %v", err)
	}
	status, err := a.Status(ctx)
	if err != nil || status.Connection != "historical_snapshot" || status.Coverage != "partial" {
		t.Fatalf("misleading status: %v %+v", err, status)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(core)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("snapshot archive changed")
	}
	if _, err = New(ctx, Config{DataDir: root, Offline: true}); err == nil {
		t.Fatal("ordinary startup accepted sealed snapshot")
	}
}

func TestHistoricalSnapshotVoiceReadinessIsMetadataOnly(t *testing.T) {
	root, captured := historicalFixture(t)
	service := newVoiceHTTPFixture(t, true)
	a, err := New(context.Background(), Config{DataDir: root, Snapshot: &SnapshotConfig{CapturedAt: captured}, Transcription: &transcription.Options{URL: service.server.URL, Token: voiceFixtureToken, Language: "he"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	status, err := a.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Media.Code != "service_ready_processing_disabled" || a.resources.voice != nil || service.calls.Load() != 0 {
		t.Fatal("metadata readiness started voice processing")
	}
}

func TestHistoricalSnapshotRequiresCurrentAuthorizationAttestation(t *testing.T) {
	for _, kind := range []string{"missing", "unverified", "different-capture", "future-verification"} {
		t.Run(kind, func(t *testing.T) {
			root, captured := historicalFixture(t)
			path := filepath.Join(root, "archive", archive.HistoricalSnapshotMarker)
			if kind == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				marker := snapshotMarker{Version: 1, CapturedAt: captured, AuthorizationVerifiedAt: time.Now().UTC(), CurrentAuthorizationVerified: true}
				if kind == "unverified" {
					marker.CurrentAuthorizationVerified = false
				}
				if kind == "different-capture" {
					marker.CapturedAt = marker.CapturedAt.Add(-time.Hour)
				}
				if kind == "future-verification" {
					marker.AuthorizationVerifiedAt = time.Now().Add(time.Hour)
				}
				data, _ := json.Marshal(marker)
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			a, err := New(context.Background(), Config{DataDir: root, Snapshot: &SnapshotConfig{CapturedAt: captured}})
			if a != nil {
				a.Close(context.Background())
			}
			if !errors.Is(err, archive.ErrHistoricalSnapshotReadOnly) {
				t.Fatalf("unproven historical authority accepted: %v", err)
			}
		})
	}
}

func TestHistoricalSnapshotAttachmentNeverQueuesOrOpensOriginal(t *testing.T) {
	service, fixture, originals, _, _ := attachmentServiceFixture(t)
	service.historicalSnapshot = true
	service.queue = func(context.Context, archive.CredentialGrant, archive.MessageID) (bool, error) {
		t.Fatal("snapshot queued download")
		return true, nil
	}
	for _, fetch := range []bool{false, true} {
		got, _, err := service.Prepare(context.Background(), fixture.grant, fixture.view.AttachmentID, fetch, true)
		if err != nil || got.Status != "unavailable" || got.Code != "historical_snapshot_read_only" || got.DownloadURL != "" || got.InlineBase64 != "" || len(originals.opened) != 0 {
			t.Fatalf("original delivery admitted: %v", err)
		}
	}
	fixture.denied = true
	if _, _, err := service.Prepare(context.Background(), fixture.grant, fixture.view.AttachmentID, true, true); err == nil {
		t.Fatal("snapshot bypassed credential checks")
	}
}

func TestHistoricalSnapshotPublishesOnlyVerifiedReadyRemoteRecords(t *testing.T) {
	for _, kind := range []string{"ready", "pending", "orphan", "revoked", "narrowed"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			var record remoteClientRecord
			var token string
			var oldGrant archive.CredentialGrant
			root, captured := historicalFixture(t, func(paths appdata.Paths, lease *appdata.InstanceLock, s *archive.Store) {
				id := "0123456789abcdef0123456789abcdef"
				record = remoteClientRecord{Version: 1, Mode: "remote_bearer", IntentID: id, Account: "snapshot-account", Principal: archive.RemoteClientPrincipalPrefix + "v1/" + id, ConfigName: "remote-" + id + ".json", State: "ready", Client: "codex_cli", Label: "Synthetic snapshot client"}
				err := appdata.WithClients(paths, lease, func(dir *os.Root) error {
					credential, e := mcpserver.IssueRemoteCredentialFile(ctx, s, archive.CredentialCreate{Label: record.Label, Principal: record.Principal, Policy: archive.CredentialPolicy{Account: record.Account, AllChats: true}, ExpiresAt: time.Now().Add(time.Hour)}, dir, record.ConfigName, mcpserver.RemoteEndpoint{PublicURL: "https://snapshot.example/mcp"})
					if e != nil {
						return e
					}
					record.CredentialID = credential.ID
					config, _, e := mcpserver.ReadRemoteCredentialFileRoot(dir, record.ConfigName)
					token = config.Token
					return e
				})
				if err != nil {
					t.Fatal(err)
				}
				oldGrant, err = s.ResolveCredential(ctx, sha256.Sum256([]byte(token)))
				if err != nil {
					t.Fatal(err)
				}
				if kind == "ready" {
					if err = s.SetSendPermissionMode(ctx, oldGrant, archive.SendPermission{Mode: "all"}, 0); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "revoked" {
					if err = s.RevokeCredential(ctx, record.CredentialID, oldGrant.CredentialRevision); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "narrowed" {
					if _, err = s.SetCredentialPolicy(ctx, record.CredentialID, archive.CredentialPolicy{Account: record.Account, AllChats: true, From: time.Now()}, oldGrant.CredentialRevision); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "pending" {
					record.State = "pending"
				}
				state := applicationState{Version: 1, Mode: "real", RemoteClients: []remoteClientRecord{record}}
				if kind == "orphan" {
					state.RemoteClients = nil
				}
				if err = writeState(paths, lease, state); err != nil {
					t.Fatal(err)
				}
			})
			a, err := New(ctx, Config{DataDir: root, Snapshot: &SnapshotConfig{CapturedAt: captured}, MCP: &MCPConfig{Remote: &RemoteMCPConfig{Listen: "127.0.0.1:0", PublicURL: "https://snapshot.example/mcp", Server: mcpserver.Config{Mode: mcpserver.Bearer, Hosts: []string{"snapshot.example"}, Origins: []string{"https://snapshot.example"}}}}})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close(ctx)
			before, err := os.ReadFile(filepath.Join(root, "working", stateFile))
			if err != nil {
				t.Fatal(err)
			}
			err = a.publishHistoricalClients(ctx, false)
			published := a.remoteRegistry.PublishedRemoteClient(remoteBinding(record))
			if kind == "ready" || kind == "narrowed" {
				if err != nil || !published {
					t.Fatalf("ready connection not published: %v", err)
				}
				grant, e := a.store.ResolveCredential(ctx, sha256.Sum256([]byte(token)))
				if e != nil {
					t.Fatal(e)
				}
				page, e := a.store.Search(ctx, grant.Scope, archive.SearchRequest{Query: "canary"})
				if e != nil {
					t.Fatal(e)
				}
				if kind == "ready" {
					if len(page.Hits) != 1 {
						t.Fatal("ready grant lost historical read")
					}
					request := httptest.NewRequest("POST", "https://snapshot.example/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
					request.Header.Set("Authorization", "Bearer "+token)
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Accept", "application/json, text/event-stream")
					response := httptest.NewRecorder()
					a.RemoteMCPHandler().ServeHTTP(response, request)
					var discovery struct {
						Result struct {
							Tools []struct {
								Name string `json:"name"`
							} `json:"tools"`
						} `json:"result"`
					}
					if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &discovery) != nil || len(discovery.Result.Tools) != 12 {
						t.Fatalf("snapshot lost twelve-tool contract: status=%d count=%d", response.Code, len(discovery.Result.Tools))
					}

					permission, e := a.store.GetSendPermission(ctx, grant)
					if e != nil || permission.Mode != "all" {
						t.Fatalf("permissive saved fixture missing: %v", e)
					}
					if _, e = a.Send(ctx, grant, outbound.Request{Kind: "text", ChatID: page.Hits[0].ChatID, RequestID: "snapshot-send", Text: "synthetic"}); !errors.Is(e, archive.ErrHistoricalSnapshotReadOnly) {
						t.Fatal("permissive historical send admitted")
					}
					if _, e = a.store.SendStatus(ctx, grant, "snapshot-send"); !errors.Is(e, archive.ErrSendNotFound) {
						t.Fatalf("snapshot send wrote journal: %v", e)
					}
				}
				if kind == "narrowed" && (len(page.Hits) != 0 || a.store.CheckCredentialGrant(ctx, oldGrant) == nil) {
					t.Fatal("historical mode resurrected wider grant")
				}
			} else if err == nil || published {
				t.Fatal("unverified connection published")
			}
			after, err := os.ReadFile(filepath.Join(root, "working", stateFile))
			if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatal("snapshot repaired client journal")
			}
		})
	}
}

// Optional remote state must stay unopened even through trusted composition
// methods: a copied provider intent is never permission to restore its owner.
func TestHistoricalSnapshotDeniesManagedOwnersBeforeStateAccess(t *testing.T) {
	ctx := context.Background()
	root, captured := historicalFixture(t)
	a, err := New(ctx, Config{DataDir: root, Snapshot: &SnapshotConfig{CapturedAt: captured}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	remoteBefore, err := os.ReadDir(a.paths.RemoteDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for name, run := range map[string]func() error{
		"default-manager": func() error { _, e := a.NewDefaultRemoteAccessManager(nil); return e },
		"custom-manager": func() error {
			_, e := a.NewRemoteAccessManager(map[remoteaccess.Provider]remoteaccess.Adapter{}, nil)
			return e
		},
		"internal-construction": func() error {
			_, _, e := a.constructRemoteAccess(func(appdata.Paths, *appdata.InstanceLock) map[remoteaccess.Provider]remoteaccess.Adapter {
				t.Fatal("provider factory called")
				return nil
			}, nil)
			return e
		},
		"managed-mcp": func() error {
			return a.ConfigureManagedMCP(ctx, &ManagedMCPConfig{PublicURL: "https://snapshot.invalid/mcp", Generation: 1})
		},
		"configure":     func() error { return a.ConfigureRemoteAccess(ctx, localui.RemoteAccessSubmission{}) },
		"disable":       func() error { return a.DisableRemoteAccess(ctx) },
		"remove":        func() error { return a.RemoveRemoteAccess(ctx) },
		"signin":        func() error { _, e := a.RemoteAccessSignIn(ctx); return e },
		"status":        func() error { _, e := a.RemoteAccessStatus(ctx); return e },
		"authenticator": func() error { _, e := a.RemoteOwnerAuthenticator(); return e },
		"password":      func() error { return a.SetRemoteOwnerPassword(ctx, []byte("synthetic snapshot passphrase")) },
	} {
		t.Run(name, func(t *testing.T) {
			if e := run(); !errors.Is(e, archive.ErrHistoricalSnapshotReadOnly) {
				t.Fatalf("remote operation: %v", e)
			}
		})
	}
	if a.remoteLifecycle.manager != nil || a.remoteLifecycle.adapters != nil || a.ownerAuth.auth != nil || a.managedMCP.active || a.managedMCP.generation != 0 {
		t.Fatal("snapshot allocated remote owner")
	}
	remoteAfter, err := os.ReadDir(a.paths.RemoteDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(remoteBefore) != len(remoteAfter) {
		t.Fatal("snapshot changed optional remote files")
	}
	if a.ManagedMCPReady() {
		t.Fatal("snapshot advertised managed readiness")
	}
}

func TestHistoricalSnapshotRejectsTrustedManagedTransport(t *testing.T) {
	root, captured := historicalFixture(t)
	_, err := New(context.Background(), Config{DataDir: root, Snapshot: &SnapshotConfig{CapturedAt: captured}, MCP: &MCPConfig{Remote: &RemoteMCPConfig{Managed: true, PublicURL: "https://snapshot.invalid/mcp"}}})
	if !errors.Is(err, archive.ErrHistoricalSnapshotReadOnly) {
		t.Fatalf("managed snapshot transport: %v", err)
	}
}
