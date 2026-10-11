//go:build sqlite_fts5

package ingestion

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type historyDrainConnection struct {
	*fakeConnection
	removed chan struct{}
	once    sync.Once
}

func (c *historyDrainConnection) RemoveEventHandler(id uint32) bool {
	removed := c.fakeConnection.RemoveEventHandler(id)
	c.once.Do(func() { close(c.removed) })
	return removed
}

type historySocketDrainConnection struct {
	*historyDrainConnection
	disconnected      atomic.Bool
	disconnectStarted chan struct{}
	disconnectOnce    sync.Once
	disconnectCalls   atomic.Int32
}

func (c *historySocketDrainConnection) Disconnect() {
	c.disconnectCalls.Add(1)
	c.disconnected.Store(true)
	if c.disconnectStarted != nil {
		c.disconnectOnce.Do(func() { close(c.disconnectStarted) })
	}
	c.fakeConnection.Disconnect()
}

type historyBlockedSocketConnection struct {
	*historyDrainConnection
	disconnectStarted, disconnectRelease chan struct{}
	disconnectOnce                       sync.Once
	disconnectCalls                      atomic.Int32
}

func (c *historyBlockedSocketConnection) Disconnect() {
	c.disconnectCalls.Add(1)
	c.disconnectOnce.Do(func() { close(c.disconnectStarted) })
	<-c.disconnectRelease
	c.fakeConnection.Disconnect()
}

type historyBlockedRemovalConnection struct {
	*historySocketDrainConnection
	removeRelease chan struct{}
}

func (c *historyBlockedRemovalConnection) RemoveEventHandler(id uint32) bool {
	removed := c.historyDrainConnection.RemoveEventHandler(id)
	<-c.removeRelease
	return removed
}

func historyDrainEnvelope(r *Runtime, bytes int) *events.Message {
	event := liveMessage("accepted-history-envelope", "")
	event.Info.IsFromMe = true
	event.Info.Sender = *r.client.Store.ID
	event.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:                    waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(),
		HistorySyncNotification: &waE2E.HistorySyncNotification{FileLength: proto.Uint64(uint64(bytes))},
	}}
	return event
}

func awaitHistoryDrain(t *testing.T, ready <-chan struct{}, step string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatalf("history drain did not reach %s", step)
	}
}

func TestStopDrainsAcceptedHistoryBeforeSourceCancellation(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	c := &historyDrainConnection{fakeConnection: &fakeConnection{}, removed: make(chan struct{})}
	r.connection = c
	compressedHistory := compressed(historyWire(historyRecord("history-body", "accepted history body")))
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var sourceCtx context.Context
	r.download = func(ctx context.Context, _ whatsmeow.DownloadableMessage, output whatsmeow.File) error {
		sourceCtx = ctx
		close(entered)
		select {
		case <-release:
			_, err := output.Write(compressedHistory)
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	r.deliver(historyDrainEnvelope(r, len(compressedHistory)))
	awaitHistoryDrain(t, entered, "source download")
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	done := make(chan error, 1)
	go func() { done <- r.Pause(stopCtx) }()
	awaitHistoryDrain(t, c.removed, "handler removal")
	if err := sourceCtx.Err(); err != nil {
		t.Fatalf("planned stop canceled accepted source history before drain: %v", err)
	}
	select {
	case <-done:
		t.Fatal("stop acknowledged before accepted source history finished")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatalf("complete accepted history did not drain cleanly: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) != 1 || s.events[0].Key.Message != "history-body" || len(s.unsafe) != 0 {
		t.Fatal("planned stop lost the source body or fenced the archive")
	}
	if len(s.order) == 0 || s.order[len(s.order)-1] != "end" || !r.Quiesced() || r.Status().ReconciliationRequired {
		t.Fatal("observation ended without clean source drain")
	}
}

func TestStopDeadlineDuringAcceptedHistoryStillFailsClosed(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	c := &historyDrainConnection{fakeConnection: &fakeConnection{}, removed: make(chan struct{})}
	r.connection = c
	entered, canceled := make(chan struct{}), make(chan struct{})
	r.download = func(ctx context.Context, _ whatsmeow.DownloadableMessage, _ whatsmeow.File) error {
		close(entered)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	r.deliver(historyDrainEnvelope(r, 1))
	awaitHistoryDrain(t, entered, "source download")
	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()
	done := make(chan error, 1)
	go func() { done <- r.Pause(stopCtx) }()
	awaitHistoryDrain(t, c.removed, "handler removal")
	stopCancel()
	if err := <-done; err == nil {
		t.Fatal("canceled stop reported clean drainage")
	}
	awaitHistoryDrain(t, canceled, "source cancellation after stop deadline")
	if !r.Status().ReconciliationRequired {
		t.Fatal("incomplete accepted history was not fenced")
	}
	if err := r.Disconnect(context.Background()); !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatalf("retry cleared an incomplete source observation: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) != 0 || len(s.unsafe) == 0 {
		t.Fatal("canceled history admitted source contents or omitted the durable fence")
	}
	for _, step := range s.order {
		if step == "end" {
			t.Fatal("failed drain cleared its source observation")
		}
	}
}

func TestStopKeepsSocketAvailableForAcceptedHistoryMediaMetadata(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	c := &historySocketDrainConnection{historyDrainConnection: &historyDrainConnection{
		fakeConnection: &fakeConnection{}, removed: make(chan struct{}),
	}}
	r.connection = c
	compressedHistory := compressed(historyWire(historyRecord("socket-history-body", "accepted history requires media metadata")))
	entered, refresh := make(chan struct{}), make(chan struct{})
	var refreshOnce sync.Once
	defer refreshOnce.Do(func() { close(refresh) })
	r.download = func(ctx context.Context, _ whatsmeow.DownloadableMessage, output whatsmeow.File) error {
		close(entered)
		select {
		case <-refresh:
		case <-ctx.Done():
			return ctx.Err()
		}
		// DownloadToFile refreshes an expired media-host cache over the owned
		// WhatsApp socket. Model that real transport requirement after admission
		// has stopped, rather than letting a fake download ignore the socket.
		if c.disconnected.Load() {
			return archive.ErrSourceUnavailable
		}
		_, err := output.Write(compressedHistory)
		return err
	}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	r.deliver(historyDrainEnvelope(r, len(compressedHistory)))
	awaitHistoryDrain(t, entered, "socket-dependent source download")
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	done := make(chan error, 1)
	go func() { done <- r.Disconnect(stopCtx) }()
	awaitHistoryDrain(t, c.removed, "socket-dependent handler removal")
	// Handler removal publishes this owned barrier before the source join.
	// Waiting on it makes the metadata request follow the completed admission
	// cutoff, including any eager socket teardown in the old implementation.
	awaitHistoryDrain(t, r.deliveryStopDone, "completed handler-removal owner")
	refreshOnce.Do(func() { close(refresh) })
	if err := <-done; err != nil {
		t.Fatalf("planned stop closed the socket required by accepted history: %v", err)
	}
	if !c.disconnected.Load() || !r.Quiesced() || r.Status().ReconciliationRequired {
		t.Fatal("source drain did not finish with an owned socket shutdown")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) != 1 || s.events[0].Key.Message != "socket-history-body" || len(s.unsafe) != 0 {
		t.Fatal("planned stop lost socket-dependent accepted history")
	}
}

func TestCanceledStartupDrainsAcceptedHistoryBeforeClosingSocket(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	t.Cleanup(func() { _ = r.Close() })
	socket := &historySocketDrainConnection{historyDrainConnection: &historyDrainConnection{
		fakeConnection: &fakeConnection{}, removed: make(chan struct{}),
	}}
	request, cancel := context.WithCancel(context.Background())
	defer cancel()
	compressedHistory := compressed(historyWire(historyRecord("startup-history-body", "accepted before startup cancellation")))
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var sourceCtx context.Context
	r.download = func(ctx context.Context, _ whatsmeow.DownloadableMessage, output whatsmeow.File) error {
		sourceCtx = ctx
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		if socket.disconnected.Load() {
			return archive.ErrSourceUnavailable
		}
		_, err := output.Write(compressedHistory)
		return err
	}
	r.connection = &historyStartupConnection{historySocketDrainConnection: socket, connect: func(context.Context) error {
		// An SDK handler may start as the connect call returns. Hold that late
		// return across request cancellation after source admission succeeds.
		r.deliver(historyDrainEnvelope(r, len(compressedHistory)))
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			return errors.New("test source download did not start")
		}
		<-request.Done()
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- r.Resume(request) }()
	awaitHistoryDrain(t, entered, "accepted startup history")
	cancel()
	awaitHistoryDrain(t, socket.removed, "canceled startup handler removal")
	if err := sourceCtx.Err(); err != nil {
		releaseOnce.Do(func() { close(release) })
		<-done
		t.Fatalf("request cancellation interrupted accepted startup history before owned cleanup: %v", err)
	}
	select {
	case <-done:
		t.Fatal("canceled startup acknowledged before accepted history drained")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-done; !errors.Is(err, archive.ErrSourceUnavailable) {
		t.Fatalf("canceled startup result: %v", err)
	}
	if !r.Quiesced() || r.Status().ReconciliationRequired || !socket.disconnected.Load() || r.observation != "" {
		t.Fatal("canceled startup did not complete clean owned source/socket cleanup")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) != 1 || s.events[0].Key.Message != "startup-history-body" || len(s.unsafe) != 0 {
		t.Fatal("canceled startup lost accepted source history or fenced the archive")
	}
}

type historyStartupConnection struct {
	*historySocketDrainConnection
	connect func(context.Context) error
}

func (c *historyStartupConnection) ConnectContext(ctx context.Context) error {
	return c.connect(ctx)
}

func TestCanceledResumeJoinsPriorSourceObservationWithoutRedial(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	t.Cleanup(func() { _ = r.Close() })
	c := &historyDrainConnection{fakeConnection: &fakeConnection{}, removed: make(chan struct{})}
	r.connection = c
	compressedHistory := compressed(historyWire(historyRecord("prior-resume-body", "accepted before repeated resume")))
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var sourceCtx context.Context
	r.download = func(ctx context.Context, _ whatsmeow.DownloadableMessage, output whatsmeow.File) error {
		sourceCtx = ctx
		close(entered)
		select {
		case <-release:
			_, err := output.Write(compressedHistory)
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(historyDrainEnvelope(r, len(compressedHistory)))
	awaitHistoryDrain(t, entered, "prior observation source download")
	request, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Resume(request) }()
	awaitHistoryDrain(t, c.removed, "repeat-resume prior handler removal")
	cancel()
	// The caller withdrawing a new dial does not revoke ownership of already
	// accepted source work. The prior observation must finish its own join.
	select {
	case err := <-done:
		releaseOnce.Do(func() { close(release) })
		_ = r.Disconnect(context.Background())
		t.Fatalf("caller cancellation abandoned the owned prior observation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := sourceCtx.Err(); err != nil {
		releaseOnce.Do(func() { close(release) })
		<-done
		t.Fatalf("repeat-resume cancellation interrupted accepted source: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("repeat-resume cancellation result: %v", err)
	}
	if !r.Quiesced() || r.Status().ReconciliationRequired || r.observation != "" || c.connectCalls != 1 {
		t.Fatal("canceled repeat-resume fenced the drained archive or started a new dial")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) != 1 || s.events[0].Key.Message != "prior-resume-body" || len(s.unsafe) != 0 {
		t.Fatal("canceled repeat-resume lost accepted history or marked it unsafe")
	}
}

func TestSocketCleanupWaitsForLateSuccessfulDial(t *testing.T) {
	r := testRuntime(fixtureStore())
	c := &historySocketDrainConnection{historyDrainConnection: &historyDrainConnection{
		fakeConnection: &fakeConnection{}, removed: make(chan struct{}),
	}, disconnectStarted: make(chan struct{})}
	r.connection = c
	r.connectDone = make(chan struct{})
	dialDone := r.connectDone
	// A deadline may schedule socket cleanup while its dial is still owned.
	// The one cleanup owner must retire a socket created by a late success.
	r.op.Lock()
	cleanup := r.startSocketStop()
	r.op.Unlock()
	select {
	case <-cleanup:
		close(dialDone)
		t.Fatal("socket teardown completed before the pending dial returned")
	case <-time.After(20 * time.Millisecond):
	}
	c.disconnected.Store(false) // The pending dial completes with a live socket.
	close(dialDone)
	awaitHistoryDrain(t, cleanup, "socket cleanup after late dial success")
	if !c.disconnected.Load() {
		t.Fatal("late successful dial survived its retained socket cleanup owner")
	}
	r.op.Lock()
	retry := r.startSocketStop()
	r.op.Unlock()
	if retry != cleanup {
		t.Fatal("socket cleanup retry replaced its retained single owner")
	}
}

func TestStopDeadlineRetainsPendingDialUntilFinalSocketCleanup(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	c := &historySocketDrainConnection{historyDrainConnection: &historyDrainConnection{
		fakeConnection: &fakeConnection{}, removed: make(chan struct{}),
	}, disconnectStarted: make(chan struct{})}
	r.connection = c
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	// Model an owned dial that ignores cancellation and completes late. Its
	// completion channel, rather than its caller, owns the physical return.
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	r.op.Lock()
	r.connectDone = make(chan struct{})
	dialDone := r.connectDone
	r.op.Unlock()
	go func() {
		<-release
		c.disconnected.Store(false)
		close(dialDone)
	}()
	stopCtx, cancelStop := context.WithCancel(context.Background())
	defer cancelStop()
	r.op.Lock()
	runCancel := r.cancel
	r.cancel = func() {
		runCancel()
		cancelStop() // Expire only after the accepted worker has joined.
	}
	r.op.Unlock()
	if err := r.Disconnect(stopCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pending-dial stop deadline: %v", err)
	}
	if r.Quiesced() || !r.Status().ReconciliationRequired || r.observation == "" {
		t.Fatal("unjoined dial cleared its unsafe observation or reported quiescence")
	}
	select {
	case <-c.disconnectStarted:
		releaseOnce.Do(func() { close(release) })
		_ = r.Disconnect(context.Background())
		t.Fatal("deadline cleanup disconnected before the owned late dial returned")
	case <-time.After(20 * time.Millisecond):
	}
	retry := make(chan error, 1)
	go func() { retry <- r.Disconnect(context.Background()) }()
	releaseOnce.Do(func() { close(release) })
	if err := <-retry; !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatalf("pending-dial cleanup retry: %v", err)
	}
	if !r.Quiesced() || !r.Status().ReconciliationRequired || !c.disconnected.Load() || c.disconnectCalls.Load() != 1 || c.connectCalls != 1 {
		t.Fatal("retry lost its dial/socket owner, cleared the fence, or redialed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.unsafe) == 0 {
		t.Fatal("genuine dial join failure omitted its durable unsafe fence")
	}
	for _, step := range s.order {
		if step == "end" {
			t.Fatal("failed pending-dial cleanup ended its unsafe observation")
		}
	}
}

func captureRuntimeFailure(t *testing.T, call func()) map[string]any {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = previous; _ = writer.Close(); _ = reader.Close() }()
	call()
	_ = writer.Close()
	os.Stderr = previous
	encoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic map[string]any
	if err = json.Unmarshal(encoded, &diagnostic); err != nil {
		t.Fatalf("missing fixed runtime failure diagnostic: %v", err)
	}
	return diagnostic
}

func TestRuntimeFailureDiagnosticReportsSafetyGateWithoutPrivateData(t *testing.T) {
	r := testRuntime(fixtureStore())
	diagnostic := captureRuntimeFailure(t, func() {
		_ = r.connectionFailure("observation", archive.ErrReconciliationRequired)
	})
	if len(diagnostic) != 4 || diagnostic["event"] != "runtime_failure" || diagnostic["stage"] != "observation" || diagnostic["code"] != "connection_failed" || diagnostic["class"] != "reconciliation_required" {
		t.Fatalf("source safety gate lost its fixed failure classification: %+v", diagnostic)
	}
	private := "SYNTHETIC_PRIVATE_URL_PATH_ACCOUNT_TOKEN_BODY"
	diagnostic = captureRuntimeFailure(t, func() {
		_ = r.connectionFailure(private, opaqueConnectionError{})
	})
	encoded, err := json.Marshal(diagnostic)
	if err != nil || strings.Contains(string(encoded), private) || len(diagnostic) != 4 || diagnostic["stage"] != "unknown" || diagnostic["class"] != "unknown" {
		t.Fatal("runtime failure diagnostic retained an untrusted stage or formatted an opaque error")
	}
}

func TestFailClosedDiagnosticReportsEntryCode(t *testing.T) {
	r := testRuntime(fixtureStore())
	diagnostic := captureRuntimeFailure(t, func() { r.failClosed("reconciliation_required") })
	if len(diagnostic) != 4 || diagnostic["event"] != "runtime_failure" || diagnostic["stage"] != "unknown" || diagnostic["code"] != "reconciliation_required" || diagnostic["class"] != "none" {
		t.Fatalf("failClosed entry lost its fixed classification: %+v", diagnostic)
	}
}

func TestConnectionClassifiesArchiveCommandFailures(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{archive.ErrReconciliationRequired, "reconciliation_required"},
		{archive.ErrQueueLimitExceeded, "queue_limit_exceeded"},
		{archive.ErrArchiveUnavailable, "archive_unavailable"},
		{archive.ErrSourceRevisionConflict, "source_revision_conflict"},
		{archive.ErrIdentityAliasConflict, "identity_alias_conflict"},
		{archive.ErrMutationMetadataMissing, "mutation_metadata_missing"},
		{archive.ErrPolicyChanged, "policy_changed"},
		{archive.ErrInvalidPolicy, "invalid_policy"},
		{archive.ErrInvalidEvent, "invalid_event"},
	} {
		class, reason := connectionErrorClass(test.err)
		if class != test.want || reason != 0 {
			t.Fatalf("archive command classification: got %s/%d, want %s", class, reason, test.want)
		}
	}
}

func TestStopFailureDiagnosticReportsHandlerJoinStage(t *testing.T) {
	r := testRuntime(fixtureStore())
	c := &historyBlockedRemovalConnection{historySocketDrainConnection: &historySocketDrainConnection{
		historyDrainConnection: &historyDrainConnection{fakeConnection: &fakeConnection{}, removed: make(chan struct{})},
	}, removeRelease: make(chan struct{})}
	r.connection = c
	t.Cleanup(func() { _ = r.Close() })
	defer close(c.removeRelease)
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	diagnostic := captureRuntimeFailure(t, func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- r.Pause(ctx) }()
		awaitHistoryDrain(t, c.removed, "diagnostic handler removal")
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("handler join deadline: %v", err)
		}
	})
	if len(diagnostic) != 4 || diagnostic["stage"] != "stop_handler" || diagnostic["code"] != "drain_failed" || diagnostic["class"] != "canceled" {
		t.Fatalf("handler join failure lost its fixed stage: %+v", diagnostic)
	}
}

func TestRuntimeFailureDiagnosticClosesUntrustedValues(t *testing.T) {
	private := "SYNTHETIC_PRIVATE_STAGE_CODE_ERROR_URL_PATH_TOKEN"
	encoded := runtimeFailureDiagnostic(private, private, opaqueConnectionError{})
	if string(encoded) != "{\"event\":\"runtime_failure\",\"stage\":\"unknown\",\"code\":\"unknown\",\"class\":\"unknown\"}\n" || strings.Contains(string(encoded), private) {
		t.Fatal("runtime diagnostic retained untrusted values or formatted an opaque error")
	}
	for _, stage := range []string{"observation", "stop_handler", "stop_deliveries", "stop_source", "stop_connect", "stop_socket", "stop_qr", "stop_sync", "stop_store", "stop_observation", "connected_account", "connected_sync", "backfill_request", "control_invalidation", "source_message", "queue_admission"} {
		var diagnostic map[string]any
		if err := json.Unmarshal(runtimeFailureDiagnostic(stage, "reconciliation_required", context.DeadlineExceeded), &diagnostic); err != nil || len(diagnostic) != 4 || diagnostic["stage"] != stage || diagnostic["class"] != "deadline" || diagnostic["code"] != "reconciliation_required" {
			t.Fatal("runtime diagnostic lost a closed source/cleanup boundary")
		}
	}
}

type sourceOperationDeadlineStore struct {
	*recordingStore
	stage string
}

func (s *sourceOperationDeadlineStore) SourcePolicy(ctx context.Context, account string) (archive.IngestionPolicy, error) {
	if s.stage == "source_policy" {
		return archive.IngestionPolicy{}, context.DeadlineExceeded
	}
	return s.recordingStore.SourcePolicy(ctx, account)
}

func (s *sourceOperationDeadlineStore) Canonical(ctx context.Context, account, identity string) (string, error) {
	if s.stage == "source_canonical" {
		return "", context.DeadlineExceeded
	}
	return s.recordingStore.Canonical(ctx, account, identity)
}

func (s *sourceOperationDeadlineStore) ApplyAlias(ctx context.Context, alias archive.SourceAlias) error {
	if strings.Contains(s.stage, "alias") {
		return context.DeadlineExceeded
	}
	return s.recordingStore.ApplyAlias(ctx, alias)
}

func (s *sourceOperationDeadlineStore) SetSourceNames(ctx context.Context, names archive.SourceNames) error {
	if s.stage == "source_names" {
		return context.DeadlineExceeded
	}
	return s.recordingStore.SetSourceNames(ctx, names)
}

func (s *sourceOperationDeadlineStore) ApplySourceEvent(ctx context.Context, event archive.SourceEvent, generation archive.Generation) (archive.ApplyResult, error) {
	if s.stage == "source_apply" {
		return archive.ApplyResult{}, context.DeadlineExceeded
	}
	return s.recordingStore.ApplySourceEvent(ctx, event, generation)
}

func (s *sourceOperationDeadlineStore) SetSyncStatus(ctx context.Context, update archive.SyncUpdate) error {
	if s.stage == "source_sync" {
		return context.DeadlineExceeded
	}
	return s.recordingStore.SetSyncStatus(ctx, update)
}

func TestMessageDeadlineDiagnosticIdentifiesExactStoreOperation(t *testing.T) {
	for _, stage := range []string{"source_policy", "source_canonical", "source_session_alias", "source_sender_alias", "source_recipient_alias", "source_names", "source_apply", "source_sync"} {
		t.Run(stage, func(t *testing.T) {
			r := testRuntime(fixtureStore())
			r.store = &sourceOperationDeadlineStore{recordingStore: fixtureStore(), stage: stage}
			event := liveMessage("synthetic-operation-deadline", "private synthetic source text")
			event.Info.PushName = "private synthetic source name"
			switch stage {
			case "source_session_alias":
				r.client.Store.LID = types.NewJID("100", types.HiddenUserServer)
			case "source_sender_alias":
				event.Info.SenderAlt = types.NewJID("200", types.HiddenUserServer)
			case "source_recipient_alias":
				event.Info.RecipientAlt = types.NewJID("200", types.HiddenUserServer)
			}
			diagnostic := captureRuntimeFailure(t, func() {
				if err := r.message(context.Background(), event); !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("message source deadline changed its cause: %v", err)
				}
			})
			if len(diagnostic) != 4 || diagnostic["stage"] != stage || diagnostic["code"] != "source_operation_failed" || diagnostic["class"] != "deadline" || r.Status().ReconciliationRequired {
				t.Fatalf("source operation deadline lost its stage or changed its fence semantics: %+v", diagnostic)
			}
		})
	}
}

func TestSourceDeadlineDiagnosticPreservesParentContextState(t *testing.T) {
	for _, test := range []struct {
		name, code        string
		canceled, expired bool
	}{
		{name: "live", code: "source_operation_failed"},
		{name: "canceled", code: "source_parent_canceled", canceled: true},
		{name: "expired", code: "source_parent_deadline", expired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testRuntime(fixtureStore())
			r.store = &sourceOperationDeadlineStore{recordingStore: fixtureStore(), stage: "source_policy"}
			ctx := context.Background()
			if test.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else if test.expired {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			}
			diagnostic := captureRuntimeFailure(t, func() {
				if err := r.message(ctx, liveMessage("synthetic-parent-state", "private synthetic body")); !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("source parent state changed the returned cause: %v", err)
				}
			})
			if diagnostic["stage"] != "source_policy" || diagnostic["code"] != test.code || diagnostic["class"] != "deadline" || len(diagnostic) != 4 {
				t.Fatalf("source failure lost its fixed parent-context classification: %+v", diagnostic)
			}
		})
	}
}

func TestSourceOperationDiagnosticPreservesExactErrorIdentity(t *testing.T) {
	for _, original := range []error{archive.ErrSourceRevisionConflict, archive.ErrPolicyChanged, opaqueConnectionError{}} {
		diagnostic := captureRuntimeFailure(t, func() {
			if got := sourceOperationFailure(context.Background(), "source_apply", original); got != original {
				t.Error("source diagnostic replaced the exact archive error identity")
			}
		})
		class, _ := connectionErrorClass(original)
		if len(diagnostic) != 4 || diagnostic["stage"] != "source_apply" || diagnostic["code"] != "source_operation_failed" || diagnostic["class"] != class {
			t.Fatal("source diagnostic changed its closed classification")
		}
	}
}

func TestStopSocketCloseDeadlineRetainsCleanupOwner(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	c := &historyBlockedSocketConnection{
		historyDrainConnection: &historyDrainConnection{fakeConnection: &fakeConnection{}, removed: make(chan struct{})},
		disconnectStarted:      make(chan struct{}), disconnectRelease: make(chan struct{}),
	}
	r.connection = c
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(c.disconnectRelease) })
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()
	done := make(chan error, 1)
	go func() { done <- r.Disconnect(stopCtx) }()
	awaitHistoryDrain(t, c.disconnectStarted, "bounded socket close")
	stopCancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("socket close did not preserve the caller deadline: %v", err)
	}
	if r.Quiesced() || !r.Status().ReconciliationRequired {
		t.Fatal("socket close timeout released ownership or reported clean drain")
	}
	retryCtx, retryCancel := context.WithCancel(context.Background())
	retryCancel()
	if err := r.Disconnect(retryCtx); !errors.Is(err, context.Canceled) || r.Quiesced() || c.disconnectCalls.Load() != 1 {
		t.Fatalf("held socket cleanup was acknowledged or duplicated by retry: %v", err)
	}
	retry := make(chan error, 1)
	go func() { retry <- r.Disconnect(context.Background()) }()
	releaseOnce.Do(func() { close(c.disconnectRelease) })
	if err := <-retry; !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatalf("socket close retry removed its unsafe fence: %v", err)
	}
	if !r.Quiesced() || c.disconnectCalls.Load() != 1 {
		t.Fatal("socket close retry duplicated cleanup or failed to join it")
	}
}

func TestStopHandlerRemovalDeadlineStillStopsSocketAndRetainsOwner(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	c := &historyBlockedRemovalConnection{
		historySocketDrainConnection: &historySocketDrainConnection{
			historyDrainConnection: &historyDrainConnection{fakeConnection: &fakeConnection{}, removed: make(chan struct{})},
			disconnectStarted:      make(chan struct{}),
		},
		removeRelease: make(chan struct{}),
	}
	r.connection = c
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(c.removeRelease) })
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()
	done := make(chan error, 1)
	go func() { done <- r.Disconnect(stopCtx) }()
	awaitHistoryDrain(t, c.removed, "blocked SDK handler removal")
	stopCancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("handler removal did not preserve the caller deadline: %v", err)
	}
	awaitHistoryDrain(t, c.disconnectStarted, "socket shutdown after handler deadline")
	if r.Quiesced() || !r.Status().ReconciliationRequired {
		t.Fatal("handler timeout released ownership or reported clean drain")
	}
	retryCtx, retryCancel := context.WithCancel(context.Background())
	retryCancel()
	if err := r.Disconnect(retryCtx); !errors.Is(err, context.Canceled) || r.Quiesced() {
		t.Fatalf("held handler cleanup was acknowledged by retry: %v", err)
	}
	retry := make(chan error, 1)
	go func() { retry <- r.Disconnect(context.Background()) }()
	releaseOnce.Do(func() { close(c.removeRelease) })
	if err := <-retry; !errors.Is(err, archive.ErrReconciliationRequired) {
		t.Fatalf("handler-removal retry cleared its unsafe fence: %v", err)
	}
	if !r.Quiesced() {
		t.Fatal("handler-removal retry failed to join cleanup")
	}
}

func TestDurableHistoryStopPreservesTailDeletionAndClientGrant(t *testing.T) {
	s, dir := durableFixture(t)
	r := durableRuntime(t, s)
	c := &historyDrainConnection{fakeConnection: &fakeConnection{}, removed: make(chan struct{})}
	r.connection = c
	ctx := context.Background()
	if err := r.message(ctx, liveMessage("history-delete-target", "deletedhistorycanary")); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("synthetic-history-drain-client"))
	credential, err := s.CreateCredential(ctx, archive.CredentialCreate{
		Label: "Synthetic history drain client", Principal: "client:history-drain", Digest: digest,
		Policy: archive.CredentialPolicy{Account: durableAccount, AllChats: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	tail := historyRecord("tail-delete-control", "")
	tail.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String("history-delete-target"), RemoteJID: proto.String("200@s.whatsapp.net")},
	}}
	compressedHistory := compressed(historyWire(historyRecord("new-history-body", "retainedhistorycanary"), tail))
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	r.download = func(ctx context.Context, _ whatsmeow.DownloadableMessage, output whatsmeow.File) error {
		close(entered)
		select {
		case <-release:
			_, err := output.Write(compressedHistory)
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err = r.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	r.deliver(historyDrainEnvelope(r, len(compressedHistory)))
	awaitHistoryDrain(t, entered, "durable source download")
	stopCtx, stopCancel := context.WithTimeout(ctx, 2*time.Second)
	defer stopCancel()
	done := make(chan error, 1)
	go func() { done <- r.Disconnect(stopCtx) }()
	awaitHistoryDrain(t, c.removed, "durable handler removal")
	releaseOnce.Do(func() { close(release) })
	if err = <-done; err != nil {
		t.Fatalf("planned stop failed to preserve the accepted source history: %v", err)
	}
	if r.client.Store.ID == nil || r.SourceAccount() != durableAccount {
		t.Fatal("planned stop removed the saved device identity")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := durableStore(t, dir)
	grant, err := reopened.ResolveCredential(ctx, digest)
	if err != nil || grant.CredentialID != credential.ID || grant.CredentialRevision != credential.Revision {
		t.Fatalf("planned stop or restart changed the client grant: %v", err)
	}
	for _, check := range []struct {
		query string
		want  int
	}{{"retainedhistorycanary", 1}, {"deletedhistorycanary", 0}} {
		page, err := reopened.Search(ctx, grant.Scope, archive.SearchRequest{Query: check.query})
		if err != nil || len(page.Hits) != check.want {
			t.Fatalf("history tail controls or retained source did not survive restart: query %s, hits %d, error %v", check.query, len(page.Hits), err)
		}
	}
	for _, marker := range []string{"observation.marker", "unsafe.marker"} {
		if _, err := os.Lstat(filepath.Join(dir, marker)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("successful source drain retained %s: %v", marker, err)
		}
	}
}
