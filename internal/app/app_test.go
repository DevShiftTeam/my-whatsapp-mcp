package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/appdata"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"github.com/Bodokh/my-whatsapp-mcp/internal/retrieval"
)

func privateTestRoot(t *testing.T) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, "application")
}

func TestPolicyBeforePairingAndNoAutomaticPair(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx, Config{DataDir: privateTestRoot(t), Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	setup, err := a.Setup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if setup.Configured {
		t.Fatal("new application configured before owner choice")
	}
	entries, err := os.ReadDir(a.paths.SessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("demo startup created a real WhatsApp session")
	}
	state, err := a.PairingState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.QRAvailable || state.State == "pairing" {
		t.Fatal("startup paired automatically")
	}
	if err := a.StartPairing(ctx); !errors.Is(err, archive.ErrInvalidPolicy) {
		t.Fatalf("pair without policy: %v", err)
	}
	status, err := a.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Messages != 0 || status.Connection != "setup_required" {
		t.Fatalf("startup status: %+v", status)
	}
}

func TestDemoSourceSearchReplyEditDeleteAndReopen(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	a, err := New(ctx, Config{DataDir: root, Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := a.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.StartPairing(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := a.Search(ctx, archive.SearchRequest{Query: "פגישה", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 1 {
		t.Fatalf("Hebrew result count: %d", len(page.Hits))
	}
	sourceID := page.Hits[0].MessageID
	contextPage, err := a.Context(ctx, archive.ContextRequest{MessageID: sourceID, Before: 20, After: 20})
	if err != nil {
		t.Fatal(err)
	}
	var reply, edit, image, voice bool
	for _, m := range contextPage.Messages {
		if strings.Contains(m.Body, "Confirmed") {
			reply = m.ReplyTo == sourceID && strings.Contains(m.ReplyQuote, "פגישה")
		}
		if strings.Contains(m.Body, "updated") {
			edit = m.Edited
		}
		image = image || m.Kind == archive.MessageImage
		voice = voice || m.Kind == archive.MessageVoice
		if strings.Contains(m.Body, "DELETED_CANARY") || strings.Contains(m.Body, "OLD_EDIT_CANARY") {
			t.Fatal("stale source text returned")
		}
	}
	if !reply || !edit || !image || !voice {
		t.Fatalf("source details: reply=%t edit=%t image=%t voice=%t", reply, edit, image, voice)
	}
	status, err := a.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Connection != "disconnected" || status.Semantic.State != "not_configured" || status.Media.State != "not_configured" {
		t.Fatalf("truthful demo status: %+v", status)
	}
	count := status.Messages
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	a, err = New(ctx, Config{DataDir: root, Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	if err = a.StartPairing(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = a.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Messages != count {
		t.Fatalf("replay duplicated rows: %d -> %d", count, status.Messages)
	}
	page, err = a.Search(ctx, archive.SearchRequest{Query: "פגישה"})
	if err != nil || len(page.Hits) != 1 || page.Hits[0].MessageID != sourceID {
		t.Fatalf("reopened source lookup: %+v %v", page, err)
	}
	// Owner filters only narrow the server account/policy grant.
	page, err = a.Search(ctx, archive.SearchRequest{Query: "פגישה", From: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("date scope: %+v %v", page, err)
	}
	discovery, err := a.Chats(ctx, archive.ChatListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovery.Chats) < 2 {
		t.Fatal("demo discovery missing second chat")
	}
	narrowed, err := a.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsSelective}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if narrowed.Generation <= policy.Generation {
		t.Fatal("policy generation unchanged")
	}
	page, err = a.Search(ctx, archive.SearchRequest{Query: "פגישה"})
	if err != nil || len(page.Hits) != 0 {
		t.Fatalf("policy denial: %+v %v", page, err)
	}
	if _, err = a.Context(ctx, archive.ContextRequest{MessageID: sourceID}); !errors.Is(err, archive.ErrSourceUnavailable) {
		t.Fatalf("excluded context: %v", err)
	}
}

func TestPausePersistenceAndDeleteOnlyOwnedData(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	a, err := New(ctx, Config{DataDir: root, Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0); err != nil {
		t.Fatal(err)
	}
	if err = a.StartPairing(ctx); err != nil {
		t.Fatal(err)
	}
	if err = a.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	a, err = New(ctx, Config{DataDir: root, Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	status, err := a.Status(ctx)
	if err != nil || !status.Paused {
		t.Fatalf("reopened pause: %+v %v", status, err)
	}
	if err = a.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if err = a.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = a.Status(ctx)
	if err != nil || status.Paused || status.Messages == 0 {
		t.Fatalf("disconnect lost archive: %+v %v", status, err)
	}
	unrelated := filepath.Join(root, "preserve.txt")
	if err = os.WriteFile(unrelated, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.DeleteLocalData(ctx, false); err == nil {
		t.Fatal("delete lacked explicit confirmation")
	}
	if err = a.DeleteLocalData(ctx, true); err != nil {
		t.Fatal(err)
	}
	setup, err := a.Setup(ctx)
	if err != nil || setup.Configured {
		t.Fatalf("reset setup: %+v %v", setup, err)
	}
	if data, err := os.ReadFile(unrelated); err != nil || string(data) != "preserve" {
		t.Fatal("delete touched unrelated root entry")
	}
	if _, err = appdata.Acquire(a.paths); !errors.Is(err, appdata.ErrInstanceAlreadyRunning) {
		t.Fatalf("reset dropped instance lease: %v", err)
	}
}

func TestDemoRefusesRealApplicationRoot(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	a, err := New(ctx, Config{DataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if a, err = New(ctx, Config{DataDir: root, Demo: true}); err == nil {
		a.Close(ctx)
		t.Fatal("demo opened real application data")
	}
}

func TestDemoRootCannotSelectDefaultRealData(t *testing.T) {
	normal, err := appdata.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resolveDataRoot(Config{DataDir: normal, Demo: true}); !errors.Is(err, appdata.ErrUnsafeDataPath) {
		t.Fatalf("demo default-real selection: %v", err)
	}
	demo, err := resolveDataRoot(Config{Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	if demo == normal || filepath.Base(demo) != "WA Indexer Demo" {
		t.Fatal("default demo did not use isolated sibling")
	}
}

// The connector test double still uses the real store observation and source
// lifecycle. Only the unavailable WhatsApp network edge is replaced.
type syntheticConnector struct {
	store              archive.ConnectorStore
	callbacks          ingestion.Callbacks
	account            string
	observation        archive.ObservationID
	snapshot           ingestion.Status
	pairCalls          int
	resumeCalls        int
	disconnectError    error
	quiescedAfterError bool
	resumeError        error
}

func (c *syntheticConnector) SourceAccount() string    { return c.account }
func (c *syntheticConnector) Quiesced() bool           { return c.disconnectError == nil || c.quiescedAfterError }
func (c *syntheticConnector) Status() ingestion.Status { return c.snapshot }
func (c *syntheticConnector) Pair(ctx context.Context) error {
	c.pairCalls++
	observation, err := c.store.BeginObservation(ctx, "")
	if err != nil {
		return err
	}
	c.observation = observation
	c.snapshot.Connection = "pairing"
	c.callbacks.StatusChanged(c.snapshot)
	c.callbacks.QRChanged(ingestion.QRNotification{Event: "code", Code: "SYNTHETIC_PRIVATE_QR", Timeout: time.Minute})
	return nil
}
func (c *syntheticConnector) Connect(ctx context.Context) error { return c.Pair(ctx) }
func (c *syntheticConnector) Pause(ctx context.Context) error {
	if err := c.Disconnect(ctx); err != nil {
		return err
	}
	c.snapshot.Paused = true
	c.snapshot.Connection = "paused"
	c.callbacks.StatusChanged(c.snapshot)
	return nil
}
func (c *syntheticConnector) Resume(ctx context.Context) error {
	c.resumeCalls++
	if c.resumeError != nil {
		return c.resumeError
	}
	c.snapshot.Paused = false
	c.snapshot.Connection = "disconnected"
	c.callbacks.StatusChanged(c.snapshot)
	return nil
}
func (c *syntheticConnector) Disconnect(ctx context.Context) error {
	if c.disconnectError != nil {
		return c.disconnectError
	}
	c.snapshot.Connection = "disconnected"
	c.callbacks.StatusChanged(c.snapshot)
	c.callbacks.QRChanged(ingestion.QRNotification{Event: "disconnected"})
	if c.observation != "" {
		if err := c.store.EndObservation(ctx, c.observation); err != nil {
			return err
		}
		c.observation = ""
	}
	return nil
}

func TestResumeWriteFailureKeepsRuntimePaused(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	var runtime *syntheticConnector
	factory := func(_ ingestion.Config, s archive.ConnectorStore, cb ingestion.Callbacks) (connector, error) {
		runtime = &syntheticConnector{store: s, callbacks: cb}
		return runtime, nil
	}
	a, err := newApplication(ctx, Config{DataDir: root}, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	if err = a.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	// A read-only working directory is meaningful only when running without root.
	// Replace it with a symlink instead: the held-root API must reject it safely.
	renamed := filepath.Join(root, "working-original")
	if err = os.Rename(a.paths.WorkingDir, renamed); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(renamed, a.paths.WorkingDir); err != nil {
		t.Fatal(err)
	}
	if err = a.Resume(ctx); err == nil {
		t.Fatal("resume succeeded despite failed private state write")
	}
	if runtime.resumeCalls != 0 {
		t.Fatal("runtime resumed before pause persistence succeeded")
	}
	status, err := a.Status(ctx)
	if err != nil || !status.Paused {
		t.Fatalf("truthful failed resume: %+v %v", status, err)
	}
	if err = os.Remove(a.paths.WorkingDir); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(renamed, a.paths.WorkingDir); err != nil {
		t.Fatal(err)
	}
}

func TestPauseRetryPersistsAfterTransientStateFailure(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	a, err := New(ctx, Config{DataDir: root, Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(root, "working-original")
	if err = os.Rename(a.paths.WorkingDir, original); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(original, a.paths.WorkingDir); err != nil {
		t.Fatal(err)
	}
	if err = a.Pause(ctx); err == nil {
		t.Fatal("pause persistence unexpectedly succeeded")
	}
	if err = os.Remove(a.paths.WorkingDir); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(original, a.paths.WorkingDir); err != nil {
		t.Fatal(err)
	}
	if err = a.Pause(ctx); err != nil {
		t.Fatalf("durable pause retry: %v", err)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	a, err = New(ctx, Config{DataDir: root, Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	status, err := a.Status(ctx)
	if err != nil || !status.Paused {
		t.Fatalf("pause retry not persisted: %+v %v", status, err)
	}
}

func TestFailedRuntimeResumeRestoresDurablePause(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	var runtime *syntheticConnector
	factory := func(_ ingestion.Config, s archive.ConnectorStore, cb ingestion.Callbacks) (connector, error) {
		runtime = &syntheticConnector{store: s, callbacks: cb}
		return runtime, nil
	}
	a, err := newApplication(ctx, Config{DataDir: root}, factory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0); err != nil {
		t.Fatal(err)
	}
	if err = a.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	runtime.resumeError = archive.ErrSourceUnavailable
	if err = a.Resume(ctx); !errors.Is(err, archive.ErrSourceUnavailable) {
		t.Fatalf("failed resume: %v", err)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	a, err = newApplication(ctx, Config{DataDir: root}, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	status, err := a.Status(ctx)
	if err != nil || !status.Paused || status.Connection != "paused" {
		t.Fatalf("failed resume lost durable pause: %+v %v", status, err)
	}
}

func TestShutdownFailureRetainsOwnershipUntilQuiesced(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	var runtime *syntheticConnector
	factory := func(_ ingestion.Config, s archive.ConnectorStore, cb ingestion.Callbacks) (connector, error) {
		runtime = &syntheticConnector{store: s, callbacks: cb}
		return runtime, nil
	}
	a, err := newApplication(ctx, Config{DataDir: root}, factory)
	if err != nil {
		t.Fatal(err)
	}
	runtime.disconnectError = context.DeadlineExceeded
	if err = a.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown failure: %v", err)
	}
	if _, err = appdata.Acquire(a.paths); !errors.Is(err, appdata.ErrInstanceAlreadyRunning) {
		t.Fatalf("live worker lost ownership: %v", err)
	}
	if _, err = a.Status(ctx); err != nil {
		t.Fatalf("shutdown failure hid diagnostics: %v", err)
	}
	runtime.disconnectError = nil
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := appdata.Acquire(a.paths)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
}

func TestExplicitDeleteCanResetQuiescedUnsafeArchive(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	var runtime *syntheticConnector
	factory := func(_ ingestion.Config, s archive.ConnectorStore, cb ingestion.Callbacks) (connector, error) {
		runtime = &syntheticConnector{store: s, callbacks: cb}
		return runtime, nil
	}
	a, err := newApplication(ctx, Config{DataDir: root}, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	if _, err = a.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0); err != nil {
		t.Fatal(err)
	}
	if err = a.store.MarkUnsafe(ctx, "fixture-account", "drain_failed"); err != nil {
		t.Fatal(err)
	}
	runtime.disconnectError = archive.ErrReconciliationRequired
	runtime.quiescedAfterError = true
	if err = a.DeleteLocalData(ctx, true); err != nil {
		t.Fatalf("explicit unsafe reset: %v", err)
	}
	status, err := a.Status(ctx)
	if err != nil || status.ReconciliationRequired || status.Connection != "setup_required" {
		t.Fatalf("unsafe state survived explicit reset: %+v %v", status, err)
	}
}

func TestFailedDeleteRestoresDiagnosticQueries(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	var runtime *syntheticConnector
	factory := func(_ ingestion.Config, s archive.ConnectorStore, cb ingestion.Callbacks) (connector, error) {
		runtime = &syntheticConnector{store: s, callbacks: cb}
		return runtime, nil
	}
	a, err := newApplication(ctx, Config{DataDir: root}, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	runtime.disconnectError = context.DeadlineExceeded
	if err = a.DeleteLocalData(ctx, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop failure: %v", err)
	}
	if _, err = a.Status(ctx); err != nil {
		t.Fatalf("failed delete disabled diagnostics: %v", err)
	}
	runtime.disconnectError = nil
	if err = a.DeleteLocalData(ctx, true); err != nil {
		t.Fatalf("delete retry: %v", err)
	}
}

func TestInterruptedStateStagingDoesNotBlockPause(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	a, err := New(ctx, Config{DataDir: root, Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	if err = os.WriteFile(filepath.Join(a.paths.WorkingDir, ".application-state-new"), []byte("interrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Pause(ctx); err != nil {
		t.Fatalf("stale staging blocked new state: %v", err)
	}
}

func TestAcceptedDemoReplaySurvivesCallerCancellation(t *testing.T) {
	root := privateTestRoot(t)
	a, err := New(context.Background(), Config{DataDir: root, Demo: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	policy, err := a.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.Close(); err != nil {
		t.Fatal(err)
	}
	request, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	var armed atomic.Bool
	// The real writer invokes its clock during the first permitted source apply,
	// after BeginObservation has succeeded. Cancel the original browser request
	// exactly there, without delaying or mocking the SQLite transaction.
	a.store, err = archive.Open(context.Background(), a.paths.ArchiveDir, archive.Options{Now: func() time.Time {
		if armed.Load() {
			once.Do(cancel)
		}
		return time.Now()
	}})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := retrieval.NewService(retrieval.Config{Reader: a.store, Candidates: a.store, Index: a.store.ActiveIndex})
	if err != nil {
		t.Fatal(err)
	}
	a.resources.retrieval.set(reader)
	armed.Store(true)
	if err = a.seedDemo(request, policy.Generation); err != nil {
		t.Fatalf("accepted synthetic replay used canceled request: %v", err)
	}
	if request.Err() == nil {
		t.Fatal("fixture did not cancel original request")
	}
	page, err := a.Search(context.Background(), archive.SearchRequest{Query: "updated"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("accepted edit was incomplete: %+v %v", page, err)
	}
	status, err := a.Status(context.Background())
	if err != nil || status.ReconciliationRequired {
		t.Fatalf("safe finite replay left denial: %+v %v", status, err)
	}
	if _, err = os.Stat(filepath.Join(root, "archive", "observation.marker")); !os.IsNotExist(err) {
		t.Fatalf("safe replay left marker: %v", err)
	}
}
func (c *syntheticConnector) Close() error { return nil }

func TestInertRuntimeQRMemoryAndCleanShutdown(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	var runtime *syntheticConnector
	factory := func(_ ingestion.Config, s archive.ConnectorStore, cb ingestion.Callbacks) (connector, error) {
		runtime = &syntheticConnector{store: s, callbacks: cb}
		return runtime, nil
	}
	a, err := newApplication(ctx, Config{DataDir: root}, factory)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.pairCalls != 0 {
		t.Fatal("constructor initiated pairing")
	}
	if err = a.StartPairing(ctx); !errors.Is(err, archive.ErrInvalidPolicy) {
		t.Fatal(err)
	}
	if _, err = a.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0); err != nil {
		t.Fatal(err)
	}
	if err = a.StartPairing(ctx); err != nil {
		t.Fatal(err)
	}
	png, err := a.QRPNG(ctx)
	if err != nil || len(png) < 8 || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("QR image: %v", err)
	}
	// Raw QR text and private source identity must not enter public status.
	status, err := a.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Connection != "pairing" {
		t.Fatalf("pairing state: %+v", status)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, "archive", "observation.marker")); !os.IsNotExist(err) {
		t.Fatalf("clean shutdown retained observation: %v", err)
	}
	if err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "SYNTHETIC_PRIVATE_QR") {
			t.Fatalf("QR text reached disk")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReopenedAccountMismatchCannotReadOrMerge(t *testing.T) {
	ctx := context.Background()
	root := privateTestRoot(t)
	factory := func(_ ingestion.Config, s archive.ConnectorStore, cb ingestion.Callbacks) (connector, error) {
		return &syntheticConnector{store: s, callbacks: cb}, nil
	}
	a, err := newApplication(ctx, Config{DataDir: root}, factory)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := a.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.store.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: "fixture-account-a", Chat: "fixture-chat", Sender: "fixture-person", Message: "source-1"}, Kind: archive.EventOriginal, OriginalDateKnown: true, OriginalSentAt: time.Now(), Body: "private fixture", BodyKind: archive.MessageText}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	mismatched := func(_ ingestion.Config, s archive.ConnectorStore, cb ingestion.Callbacks) (connector, error) {
		return &syntheticConnector{store: s, callbacks: cb, account: "fixture-account-b"}, nil
	}
	if a, err = newApplication(ctx, Config{DataDir: root}, mismatched); !errors.Is(err, archive.ErrUnauthorized) {
		if a != nil {
			a.Close(ctx)
		}
		t.Fatalf("account mismatch: %v", err)
	}
	a, err = newApplication(ctx, Config{DataDir: root}, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	page, err := a.Search(ctx, archive.SearchRequest{Query: "private"})
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("disconnected account restore: %+v %v", page, err)
	}
}
