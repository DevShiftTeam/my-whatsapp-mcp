package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
)

func rebuildHistoryTestConfig(t *testing.T, root, policy string) Config {
	t.Helper()
	cfg := Config{DataDir: root, Offline: true}
	field := reflect.ValueOf(&cfg).Elem().FieldByName("RebuildHistoryPolicy")
	if !field.IsValid() || !field.CanSet() || field.Kind() != reflect.String {
		t.Fatal("trusted rebuild history policy is unavailable")
	}
	field.SetString(policy)
	return cfg
}

func TestRebuildHistoryPolicyConfigCopyRetainsOmittedAndExplicitSelection(t *testing.T) {
	for _, policy := range []string{"", "selected-original", "delivered-history"} {
		cfg := rebuildHistoryTestConfig(t, "/synthetic/profile", policy)
		copied := copyApplicationConfig(cfg)
		if reflect.ValueOf(copied).FieldByName("RebuildHistoryPolicy").String() != policy {
			t.Fatal("trusted config copy lost history selection", policy)
		}
		reflect.ValueOf(&cfg).Elem().FieldByName("RebuildHistoryPolicy").SetString("caller-changed")
		if reflect.ValueOf(copied).FieldByName("RebuildHistoryPolicy").String() != policy {
			t.Fatal("caller mutation changed retained history selection")
		}
	}
}

func TestRebuildHistoryPolicyInvalidRejectsBeforeProfileAndSDK(t *testing.T) {
	root := filepath.Join(privateTestRoot(t), "absent-profile")
	cfg := rebuildHistoryTestConfig(t, root, "unreviewed-mode")
	calls := 0
	factory := func(ingestion.Config, archive.ConnectorStore, ingestion.Callbacks) (connector, error) {
		calls++
		return nil, archive.ErrSourceUnavailable
	}
	a, err := newApplication(context.Background(), cfg, factory)
	if a != nil {
		a.Close(context.Background())
	}
	if err == nil || calls != 0 {
		t.Fatal("invalid history policy reached runtime startup", err, calls)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("invalid history policy changed the filesystem", err)
	}
}

func TestRebuildHistoryPolicyActivationPreservesDeviceAuthorityAndExactRequests(t *testing.T) {
	ctx := context.Background()
	bootstrap, original := syntheticLiveRebuild(t)
	ready, err := BootstrapLiveRebuild(ctx, bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := appdata.ExistingPaths(bootstrap.DestinationProfileDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := archive.Open(ctx, paths.ArchiveDir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	progress, err := store.RebuildProgress(ctx)
	if err != nil || progress == nil {
		t.Fatal("missing original recovery operation", err)
	}
	candidates, err := store.NextRebuildCandidates(ctx, 1)
	if err != nil || len(candidates) != 1 {
		t.Fatal("missing exact-request fixture candidate", err)
	}
	if err := store.PendingRebuildRequest(ctx, archive.RebuildRequest{ID: "preserved-selected-original-request", Generation: progress.Generation, OperationID: progress.OperationID, OperationRevision: progress.Revision, Key: candidates[0].Key, PolicyGeneration: progress.PolicyGeneration, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordUnknownHistoryGap(ctx, bootstrap.ExpectedAccountID, 229); err != nil {
		t.Fatal(err)
	}
	unknownBefore, err := store.UnknownHistoryGapDiagnostics(ctx, bootstrap.ExpectedAccountID)
	if err != nil || unknownBefore == nil || unknownBefore.TotalObservations != 1 {
		t.Fatal("missing existing partial coverage fixture", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db := classifiedIntegrationDB(t, paths)
	var credential, chat string
	if err := db.QueryRow("SELECT id FROM credentials ORDER BY id LIMIT 1").Scan(&credential); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT id FROM chats ORDER BY id LIMIT 1").Scan(&chat); err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{"accepted", "failed", "outcome_unknown"} {
		if _, err := db.Exec("INSERT INTO send_requests(credential_id,request_id,account,chat_id,digest,protocol_id,state,accepted_at) VALUES(?,?,?,?,?,?,?,0)", credential, "history-policy-"+outcome, bootstrap.ExpectedAccountID, chat, make([]byte, 32), "synthetic-"+outcome, outcome); err != nil {
			t.Fatal(err)
		}
	}
	tables := []string{"archive_policy", "policy_chats", "credentials", "credential_chats", "send_permissions", "send_permission_modes", "send_requests", "extension_original_budget", "rebuild_provenance", "rebuild_guards", "rebuild_requests", "rebuild_operation", "rebuild_commands", "rebuild_discovery_attempts", "snapshot_recovery_audit", "unknown_history_coverage", "unknown_history_codes"}
	before := map[string]string{}
	for _, table := range tables {
		before[table] = classifiedNoticesRows(t, db, "SELECT * FROM "+table+" ORDER BY rowid")
	}
	var spent, pending int
	if err := db.QueryRow("SELECT copy_spent FROM rebuild_operation WHERE id=1").Scan(&spent); err != nil || spent != 1 {
		t.Fatal("fixture did not spend an exact request", err)
	}
	if err := db.QueryRow("SELECT count(*) FROM rebuild_requests WHERE state='pending'").Scan(&pending); err != nil || pending != 1 {
		t.Fatal("fixture lost pending exact request", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for name, raw := range original {
		files[filepath.Join(bootstrap.SourceProfileDir, name)] = raw
		files[filepath.Join(bootstrap.CurrentBackupDir, name)] = raw
		if filepath.Dir(name) != "archive" {
			path := filepath.Join(paths.Root, name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			files[path] = data
		}
	}
	readyPath := filepath.Join(paths.Root, liveRebuildReceiptFile)
	files[readyPath], err = os.ReadFile(readyPath)
	if err != nil {
		t.Fatal(err)
	}
	noSDK := func(ingestion.Config, archive.ConnectorStore, ingestion.Callbacks) (connector, error) {
		t.Error("offline history activation attempted SDK construction")
		return nil, archive.ErrSourceUnavailable
	}
	cfg := rebuildHistoryTestConfig(t, paths.Root, "delivered-history")
	var selected archive.RebuildHistorySelection
	var activatedCoverageEpoch uint64
	for attempt := 0; attempt < 2; attempt++ {
		a, err := newApplication(ctx, cfg, noSDK)
		if err != nil {
			t.Fatal("trusted delivered activation/restart failed", attempt, err)
		}
		t.Cleanup(func() { a.Close(ctx) })
		selection, err := a.store.RebuildHistorySelection(ctx)
		if err != nil || selection == nil || selection.Policy != archive.RebuildHistoryDelivered || selection.StrategyEpoch == 0 || selection.ObservationGeneration != ready.Archive.Generation || selection.Account != bootstrap.ExpectedAccountID {
			t.Fatal("startup did not select the delivered-history strategy", selection, err)
		}
		if attempt == 0 {
			selected = *selection
		} else if *selection != selected {
			t.Fatal("same-selection restart changed durable strategy identity")
		}
		var runtimePolicy archive.RebuildHistoryPolicy
		fake := &connectionRecoveryFixture{}
		a.factory = func(cfg ingestion.Config, _ archive.ConnectorStore, _ ingestion.Callbacks) (connector, error) {
			runtimePolicy = cfg.RebuildHistoryPolicy
			return fake, nil
		}
		recreated, err := a.makeRuntime()
		if err != nil || recreated != fake || runtimePolicy != archive.RebuildHistoryDelivered {
			t.Fatal("runtime recreation lost the normalized delivered selection", runtimePolicy, err)
		}
		if err := recreated.Close(); err != nil || fake.connectCalls != 0 {
			t.Fatal("runtime configuration test unexpectedly connected", err)
		}
		unknown, err := a.store.UnknownHistoryGapDiagnostics(ctx, bootstrap.ExpectedAccountID)
		if err != nil || unknown == nil || unknown.TotalObservations != unknownBefore.TotalObservations || !reflect.DeepEqual(unknown.Codes, unknownBefore.Codes) {
			t.Fatal("activation fabricated or reset unknown diagnostic evidence", err)
		}
		if attempt == 0 {
			if unknown.CoverageEpoch <= unknownBefore.CoverageEpoch {
				t.Fatal("activation did not advance the combined coverage witness")
			}
			activatedCoverageEpoch = unknown.CoverageEpoch
		} else if unknown.CoverageEpoch != activatedCoverageEpoch {
			t.Fatal("same-selection restart changed the combined coverage witness")
		}
		if err := a.Close(ctx); err != nil {
			t.Fatal(err)
		}
		auditDB := classifiedIntegrationDB(t, paths)
		var audits int
		if err := auditDB.QueryRow("SELECT count(*) FROM rebuild_history_selection_audit").Scan(&audits); err != nil || audits != 1 {
			t.Fatal("activation or restart duplicated its durable selection audit", audits, err)
		}
		if err := auditDB.Close(); err != nil {
			t.Fatal(err)
		}
		classifiedFollowupAssertExistingFiles(t, files)
	}
	omitted, err := newApplication(ctx, Config{DataDir: paths.Root, Offline: true}, noSDK)
	if omitted != nil {
		omitted.Close(ctx)
	}
	if err == nil {
		t.Fatal("omitted default silently changed the durable delivered selection")
	}
	back, err := newApplication(ctx, rebuildHistoryTestConfig(t, paths.Root, "selected-original"), noSDK)
	if err != nil {
		t.Fatal("explicit selected-original switch failed", err)
	}
	t.Cleanup(func() { back.Close(ctx) })
	selection, err := back.store.RebuildHistorySelection(ctx)
	if err != nil || selection == nil || selection.Policy != archive.RebuildHistorySelectedOriginal || selection.StrategyEpoch <= selected.StrategyEpoch {
		t.Fatal("explicit switch did not record a new strategy epoch", selection, err)
	}
	policy, err := back.store.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, err := back.store.Status(ctx, archive.ReadScope{Account: bootstrap.ExpectedAccountID, Principal: "owner", AllChats: true, PolicyGeneration: policy.Generation})
	if err != nil || !status.Snapshot.DeliveredHistorySnapshot || !status.Snapshot.UnknownHistoryGap || status.Coverage != "partial" {
		t.Fatal("switching back lost sticky delivered or unknown coverage", err)
	}
	if err := back.Close(ctx); err != nil {
		t.Fatal(err)
	}
	db = classifiedIntegrationDB(t, paths)
	var audits int
	if err := db.QueryRow("SELECT count(*) FROM rebuild_history_selection_audit").Scan(&audits); err != nil || audits != 2 {
		t.Fatal("explicit switchback did not append exactly one selection audit", audits, err)
	}
	for _, table := range tables {
		if got := classifiedNoticesRows(t, db, "SELECT * FROM "+table+" ORDER BY rowid"); got != before[table] {
			t.Fatal("activation/restart changed preserved authority or exact-request evidence", table)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	classifiedFollowupAssertExistingFiles(t, files)
}
