package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// Only private synthetic profile metadata is needed to prove lease ownership.
func syntheticForwardChain(t *testing.T) (appdata.Paths, appdata.Paths, appdata.Paths) {
	t.Helper()
	roots := make([]appdata.Paths, 3)
	for i := range roots {
		p, err := appdata.Prepare(shortNativeRoot(t))
		if err != nil {
			t.Fatal(err)
		}
		l, err := appdata.Acquire(p)
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
		roots[i] = p
	}
	now := time.Now().UTC().Add(-time.Minute)
	parent := archive.RebuildProvenance{Generation: "parent", Account: "account", DeviceID: "device", SourceSHA256: liveRebuildDigest([]byte("original")), Boundary: now}
	next := parent
	next.Generation = "next"
	next.Boundary = now.Add(time.Second)
	next.SourceSHA256 = liveRebuildDigest([]byte("latest"))
	var parentBytes []byte
	for i := 0; i < 2; i++ {
		guard := LiveRebuildSourceGuard{Version: 1, SourceProfileDir: roots[i].Root, DestinationProfileDir: roots[i+1].Root, DeviceID: "device", BackupSHA256: liveRebuildDigest([]byte{byte(i)}), SourceStopped: true}
		raw, _ := json.Marshal(guard)
		if err := os.WriteFile(appdata.LiveRebuildSourceGuardPath(roots[i].Root), raw, 0600); err != nil {
			t.Fatal(err)
		}
		provenance := parent
		if i == 1 {
			provenance = next
		}
		receipt := LiveRebuildBootstrapReceipt{Version: 1, SourceProfileDir: roots[i].Root, DestinationProfileDir: roots[i+1].Root, GuardSHA256: liveRebuildDigest(raw), BackupSHA256: guard.BackupSHA256, Archive: archive.RebuildReceipt{RebuildProvenance: provenance}}
		raw, _ = json.Marshal(receipt)
		if i == 1 {
			var fields map[string]any
			json.Unmarshal(raw, &fields)
			fields["source_receipt_sha256"] = liveRebuildDigest(parentBytes)
			fields["files"] = map[string]string{liveRebuildReceiptFile: liveRebuildDigest(parentBytes)}
			fields["archive"].(map[string]any)["ancestors"] = []archive.RebuildProvenance{parent}
			raw, _ = json.Marshal(fields)
		}
		if err := os.WriteFile(filepath.Join(roots[i+1].Root, liveRebuildReceiptFile), raw, 0600); err != nil {
			t.Fatal(err)
		}
		parentBytes = raw
	}
	return roots[0], roots[1], roots[2]
}

func TestLiveRebuildChainRetainsBothSourceLocks(t *testing.T) {
	original, predecessor, next := syntheticForwardChain(t)
	held, err := AcquireLiveRebuildSourceGuard(next.Root, nil)
	if err != nil {
		t.Fatalf("valid two-hop chain rejected: %v", err)
	}
	defer held.Close()
	for _, p := range []appdata.Paths{original, predecessor} {
		other, err := appdata.AcquireExisting(p)
		if err == nil {
			other.Close()
			t.Fatal("ancestor device owner not excluded")
		}
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []appdata.Paths{original, predecessor} {
		other, err := appdata.AcquireExisting(p)
		if err != nil {
			t.Fatal("ancestor lease leaked")
		}
		other.Close()
	}
}

func TestLiveRebuildChainRejectsTamperedReceipt(t *testing.T) {
	_, predecessor, next := syntheticForwardChain(t)
	path := filepath.Join(predecessor.Root, liveRebuildReceiptFile)
	raw, _ := os.ReadFile(path)
	raw = append(raw, ' ')
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	held, err := AcquireLiveRebuildSourceGuard(next.Root, nil)
	if err == nil {
		held.Close()
		t.Fatal("mutated predecessor receipt admitted")
	}
	// Partial acquisition must not strand the immediate predecessor.
	lock, err := appdata.AcquireExisting(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}

func TestLiveRebuildChainInheritedGuardsSurviveGuardianClose(t *testing.T) {
	original, predecessor, next := syntheticForwardChain(t)
	old, err := appdata.AcquireExisting(original)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := appdata.AcquireExisting(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	originalFD, err := old.WorkerGuard()
	if err != nil {
		t.Fatal(err)
	}
	latestFD, err := latest.WorkerGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	defer latest.Close()
	defer originalFD.Close()
	defer latestFD.Close()
	fd := int(latestFD.Fd())
	held, err := AcquireLiveRebuildSourceGuard(next.Root, &fd, int(originalFD.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	old.Close()
	latest.Close()
	originalFD.Close()
	latestFD.Close()
	for _, p := range []appdata.Paths{original, predecessor} {
		other, err := appdata.AcquireExisting(p)
		if err == nil {
			other.Close()
			t.Fatal("guardian shutdown released destination authority")
		}
	}
}

func TestLiveRebuildChainRejectsInvalidAncestryAndCleansPartialLocks(t *testing.T) {
	for _, kind := range []string{"missing", "device", "cycle", "depth", "guard", "pending", "held_original"} {
		t.Run(kind, func(t *testing.T) {
			original, predecessor, next := syntheticForwardChain(t)
			var external *appdata.InstanceLock
			switch kind {
			case "missing":
				os.Remove(filepath.Join(predecessor.Root, liveRebuildReceiptFile))
			case "device", "cycle", "depth":
				path := filepath.Join(predecessor.Root, liveRebuildReceiptFile)
				raw, _ := os.ReadFile(path)
				var r LiveRebuildBootstrapReceipt
				if err := json.Unmarshal(raw, &r); err != nil {
					t.Fatal(err)
				}
				if kind == "device" {
					r.Archive.DeviceID = "different"
				}
				if kind == "cycle" {
					r.SourceProfileDir = next.Root
				}
				if kind == "depth" {
					r.Archive.Ancestors = []archive.RebuildProvenance{r.Archive.RebuildProvenance}
				}
				raw, _ = json.Marshal(r)
				os.WriteFile(path, raw, 0600)
				path = filepath.Join(next.Root, liveRebuildReceiptFile)
				childRaw, _ := os.ReadFile(path)
				var child LiveRebuildBootstrapReceipt
				json.Unmarshal(childRaw, &child)
				child.SourceReceiptSHA256 = liveRebuildDigest(raw)
				child.Files[liveRebuildReceiptFile] = child.SourceReceiptSHA256
				child.Archive.Ancestors = append([]archive.RebuildProvenance{r.Archive.RebuildProvenance}, r.Archive.Ancestors...)
				raw, _ = json.Marshal(child)
				os.WriteFile(path, raw, 0600)
			case "guard":
				os.WriteFile(appdata.LiveRebuildSourceGuardPath(original.Root), []byte("changed"), 0600)
			case "pending":
				os.WriteFile(filepath.Join(original.Root, liveRebuildPendingFile), []byte("pending"), 0600)
			case "held_original":
				var err error
				external, err = appdata.AcquireExisting(original)
				if err != nil {
					t.Fatal(err)
				}
				defer external.Close()
			}
			held, err := AcquireLiveRebuildSourceGuard(next.Root, nil)
			if err == nil {
				held.Close()
				t.Fatal("invalid chain admitted")
			}
			independent, err := appdata.AcquireExisting(predecessor)
			if err != nil {
				t.Fatal("partial acquisition leaked immediate lease")
			}
			independent.Close()
		})
	}
}

func TestLiveRebuildChainRejectsUnmatchedAndDuplicateDescriptors(t *testing.T) {
	original, predecessor, next := syntheticForwardChain(t)
	old, err := appdata.AcquireExisting(original)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	latest, err := appdata.AcquireExisting(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Close()
	origFD, _ := old.WorkerGuard()
	defer origFD.Close()
	latestFD, _ := latest.WorkerGuard()
	defer latestFD.Close()
	fd := int(latestFD.Fd())
	wrong := int(origFD.Fd())
	for _, c := range []struct {
		immediate *int
		others    []int
	}{{&fd, []int{fd}}, {&wrong, []int{fd}}, {&fd, []int{999999}}, {&fd, []int{wrong, wrong}}} {
		held, err := AcquireLiveRebuildSourceGuard(next.Root, c.immediate, c.others...)
		if err == nil {
			held.Close()
			t.Fatal("unmatched or duplicate descriptor admitted")
		}
	}
}

func TestLiveRebuildChainBootstrapCopiesLatestEvolvedSession(t *testing.T) {
	ctx := context.Background()
	first, originalBytes := syntheticLiveRebuild(t)
	parent, err := BootstrapLiveRebuild(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(first.DestinationProfileDir, "session/session.sqlite")
	db, err := sql.Open("sqlite3", sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE synthetic_forward_session(epoch INTEGER); INSERT INTO synthetic_forward_session VALUES(3)"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	latest, err := archive.Open(ctx, filepath.Join(first.DestinationProfileDir, "archive"), archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	progress, err := latest.RebuildProgress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := latest.NextRebuildCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 {
		t.Fatal("synthetic candidate missing", err)
	}
	if err = latest.PendingRebuildRequest(ctx, archive.RebuildRequest{ID: "forward-private-pending", Generation: progress.Generation, OperationID: progress.OperationID, OperationRevision: progress.Revision, Key: candidates[0].Key, PolicyGeneration: progress.PolicyGeneration, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	progress, err = latest.RebuildProgress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = latest.ControlRebuild(ctx, archive.RebuildControl{ExpectedRevision: progress.Revision, CommandID: "paused-before-forward", Action: "pause"}); err != nil {
		t.Fatal(err)
	}
	if err = latest.MarkUnsafe(ctx, first.ExpectedAccountID, "mutation_metadata_missing"); err != nil {
		t.Fatal(err)
	}
	if err = latest.Close(); err != nil {
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
	manifest := LiveRebuildBackupManifest{Version: 1, SourceProfileDir: first.DestinationProfileDir, Account: first.ExpectedAccountID, DeviceID: first.ExpectedDeviceID, CapturedAt: time.Now(), Files: map[string]string{}}
	names := []string{liveRebuildReceiptFile}
	for name := range parent.Files {
		names = append(names, name)
	}
	before := map[string][]byte{}
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(first.DestinationProfileDir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = raw
		manifest.Files[name] = liveRebuildDigest(raw)
		if err = os.WriteFile(filepath.Join(backup.Root, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if bytes.Equal(before["session/session.sqlite"], originalBytes["session/session.sqlite"]) {
		t.Fatal("session fixture did not evolve")
	}
	raw, _ := json.Marshal(manifest)
	if err = os.WriteFile(filepath.Join(backup.Root, LiveRebuildBackupManifestFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := LiveRebuildBootstrapConfig{SourceProfileDir: first.DestinationProfileDir, CurrentBackupDir: backup.Root, DestinationProfileDir: filepath.Join(shortNativeRoot(t), "forward"), ExpectedBackupSHA256: liveRebuildDigest(raw), ExpectedAccountID: first.ExpectedAccountID, ExpectedDeviceID: first.ExpectedDeviceID, Boundary: time.Now()}
	guard := LiveRebuildSourceGuard{Version: 1, SourceProfileDir: cfg.SourceProfileDir, DestinationProfileDir: cfg.DestinationProfileDir, DeviceID: cfg.ExpectedDeviceID, BackupSHA256: cfg.ExpectedBackupSHA256, SourceStopped: true}
	raw, _ = json.Marshal(guard)
	cfg.SourceStoppedGuardSHA256 = liveRebuildDigest(raw)
	cfg.SourceStoppedGuardPath = appdata.LiveRebuildSourceGuardPath(cfg.SourceProfileDir)
	if err = os.WriteFile(cfg.SourceStoppedGuardPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(cfg.SourceStoppedGuardPath) })
	override := cfg
	override.PolicyStartDate = "01-01"
	override.PolicyYear = 2020
	override.PolicyTimezone = "UTC"
	if _, err := BootstrapLiveRebuild(ctx, override); err == nil {
		t.Fatal("forward policy override could resume paused operation")
	}
	if _, err := os.Lstat(cfg.DestinationProfileDir); !os.IsNotExist(err) {
		t.Fatal("denied override created destination")
	}
	nestedCfg := cfg
	nestedCfg.DestinationProfileDir = filepath.Join(first.SourceProfileDir, "must-not-create")
	guard.DestinationProfileDir = nestedCfg.DestinationProfileDir
	raw, _ = json.Marshal(guard)
	nestedCfg.SourceStoppedGuardSHA256 = liveRebuildDigest(raw)
	if err = os.WriteFile(cfg.SourceStoppedGuardPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BootstrapLiveRebuild(ctx, nestedCfg); err == nil {
		t.Fatal("forward bootstrap entered original namespace")
	}
	if _, err := os.Lstat(nestedCfg.DestinationProfileDir); !os.IsNotExist(err) {
		t.Fatal("nested destination created before refusal")
	}
	guard.DestinationProfileDir = cfg.DestinationProfileDir
	raw, _ = json.Marshal(guard)
	if err = os.WriteFile(cfg.SourceStoppedGuardPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	receipt, err := BootstrapLiveRebuild(ctx, cfg)
	if err != nil {
		t.Fatal("forward profile bootstrap", err)
	}
	if len(receipt.Archive.Ancestors) != 1 || receipt.Archive.Ancestors[0] != parent.Archive.RebuildProvenance || receipt.SourceReceiptSHA256 != manifest.Files[liveRebuildReceiptFile] {
		t.Fatal("unsealed lineage")
	}
	for _, root := range []string{cfg.SourceProfileDir, cfg.CurrentBackupDir} {
		for name, want := range before {
			got, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatal("latest source/backup modified", name)
			}
		}
	}
	for name, want := range originalBytes {
		got, err := os.ReadFile(filepath.Join(first.SourceProfileDir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("ultimate original modified", name)
		}
	}
	noNetwork := func(ingestion.Config, archive.ConnectorStore, ingestion.Callbacks) (connector, error) {
		t.Error("offline test tried connector")
		return nil, archive.ErrSourceUnavailable
	}
	for i := 0; i < 2; i++ {
		live, err := newApplication(ctx, Config{DataDir: cfg.DestinationProfileDir, Offline: true}, noNetwork)
		if err != nil {
			t.Fatal(err)
		}
		p, err := live.RebuildProgress(ctx)
		if err != nil || p.CopySpent != 1 || p.Pending != 0 || p.Accepted != 0 || p.State != "paused" {
			t.Fatal("ancestor ledger lost or false restored rows", err)
		}
		if i == 0 {
			fake := &syntheticConnector{store: live.store, disconnectError: context.DeadlineExceeded, callbacks: ingestion.Callbacks{StatusChanged: func(ingestion.Status) {}, QRChanged: func(ingestion.QRNotification) {}}}
			live.runtime = fake
			if err := live.Close(ctx); err == nil {
				t.Fatal("failed worker join acknowledged")
			}
			for _, root := range []string{first.SourceProfileDir, cfg.SourceProfileDir, cfg.DestinationProfileDir} {
				paths, err := appdata.ExistingPaths(root)
				if err != nil {
					t.Fatal(err)
				}
				lease, err := appdata.AcquireExisting(paths)
				if err == nil {
					lease.Close()
					t.Fatal("failed join released ancestor/device ownership")
				}
			}
			fake.disconnectError = nil
		}
		if err = live.Close(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(cfg.DestinationProfileDir, "session/session.sqlite"))
		if err != nil || !bytes.Equal(got, before["session/session.sqlite"]) {
			t.Fatal("evolved canonical session rewound")
		}
	}
}

func TestLiveRebuildChainRejectsNestedAncestorNamespace(t *testing.T) {
	original, predecessor, next := syntheticForwardChain(t)
	nested, err := appdata.Prepare(filepath.Join(original.Root, "nested-destination"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(nested)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	raw, err := os.ReadFile(filepath.Join(next.Root, liveRebuildReceiptFile))
	if err != nil {
		t.Fatal(err)
	}
	var r LiveRebuildBootstrapReceipt
	if err = json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	r.DestinationProfileDir = nested.Root
	guard := LiveRebuildSourceGuard{Version: 1, SourceProfileDir: predecessor.Root, DestinationProfileDir: nested.Root, DeviceID: r.Archive.DeviceID, BackupSHA256: r.BackupSHA256, SourceStopped: true}
	raw, _ = json.Marshal(guard)
	r.GuardSHA256 = liveRebuildDigest(raw)
	if err = os.WriteFile(appdata.LiveRebuildSourceGuardPath(predecessor.Root), raw, 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(r)
	if err = os.WriteFile(filepath.Join(nested.Root, liveRebuildReceiptFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	held, err := AcquireLiveRebuildSourceGuard(nested.Root, nil)
	if err == nil {
		held.Close()
		t.Fatal("nested destination overlaps preserved ancestor namespace")
	}
}
