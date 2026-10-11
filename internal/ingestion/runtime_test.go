package ingestion

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type recordingStore struct {
	archive.ConnectorStore
	mu           sync.Mutex
	policy       archive.IngestionPolicy
	events       []archive.SourceEvent
	syncs        []archive.SyncUpdate
	aliases      []archive.SourceAlias
	names        []archive.SourceNames
	mediaUpdates []string
	unsafe       []archive.UnsafeReason
	metadata     map[archive.SourceKey]archive.SourceMetadata
	order        []string
	failApply    error
}

func fixtureStore() *recordingStore {
	return &recordingStore{policy: archive.IngestionPolicy{Generation: 1, DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, metadata: map[archive.SourceKey]archive.SourceMetadata{}}
}
func (s *recordingStore) Policy(context.Context) (archive.ArchivePolicy, error) {
	return archive.ArchivePolicy{Generation: s.policy.Generation, DateMode: s.policy.DateMode, ChatMode: s.policy.ChatMode}, nil
}
func (s *recordingStore) SourcePolicy(context.Context, string) (archive.IngestionPolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy, nil
}
func (s *recordingStore) Canonical(_ context.Context, _ string, id string) (string, error) {
	return id, nil
}
func (s *recordingStore) ApplyAlias(_ context.Context, a archive.SourceAlias) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aliases = append(s.aliases, a)
	return nil
}
func (s *recordingStore) SetSourceNames(_ context.Context, n archive.SourceNames) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names = append(s.names, n)
	return nil
}
func (s *recordingStore) ApplySourceEvent(_ context.Context, e archive.SourceEvent, g archive.Generation) (archive.ApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failApply != nil {
		return archive.ApplyResult{}, s.failApply
	}
	if g != s.policy.Generation {
		return archive.ApplyResult{}, archive.ErrPolicyChanged
	}
	s.events = append(s.events, e)
	s.order = append(s.order, "apply")
	s.metadata[e.Key] = archive.SourceMetadata{Key: e.Key, OriginalSentAt: e.OriginalSentAt, OriginalDateKnown: e.OriginalDateKnown, ProtocolRevision: e.SourceRevision, Version: archive.SourceVersion{MessageID: "opaque", Revision: 1, PolicyGeneration: g}}
	return archive.ApplyResult{Outcome: archive.OutcomeApplied, PolicyGeneration: g, Version: s.metadata[e.Key].Version}, nil
}
func (s *recordingStore) SourceMetadata(_ context.Context, key archive.SourceKey) (archive.SourceMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.metadata[key]
	if !ok {
		return v, archive.ErrSourceUnavailable
	}
	return v, nil
}
func (s *recordingStore) MutationTargets(_ context.Context, account, chat, id string) ([]archive.SourceMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []archive.SourceMetadata
	for k, m := range s.metadata {
		if k.Account == account && k.Chat == chat && k.Message == id {
			out = append(out, m)
		}
	}
	return out, nil
}

// This recording-only mock cannot truthfully persist a durable senderless
// guard. Tests requiring successful withholding use the actual archive store.
func (s *recordingStore) RecordUnresolvedDeletion(context.Context, string, string, string) error {
	return archive.ErrArchiveUnavailable
}
func (s *recordingStore) BackfillAnchors(context.Context, string, int) ([]archive.SourceAnchor, error) {
	return nil, nil
}
func (s *recordingStore) ClearChat(_ context.Context, _ string, _ string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "clear")
	return nil
}
func (s *recordingStore) MarkUnsafe(_ context.Context, _ string, reason archive.UnsafeReason) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsafe = append(s.unsafe, reason)
	s.order = append(s.order, "unsafe")
	return nil
}
func (s *recordingStore) SetSyncStatus(_ context.Context, u archive.SyncUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncs = append(s.syncs, u)
	return nil
}
func (s *recordingStore) BeginObservation(context.Context, string) (archive.ObservationID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "begin")
	return "observation", nil
}
func (s *recordingStore) EndObservation(context.Context, archive.ObservationID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "end")
	return nil
}
func (s *recordingStore) EligibleMedia(context.Context, archive.SourceKey, archive.SourceVersion) (bool, error) {
	return true, nil
}
func (s *recordingStore) UpdateMedia(_ context.Context, _ archive.SourceKey, _ archive.SourceVersion, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mediaUpdates = append(s.mediaUpdates, status)
	return nil
}

type fakeConnection struct {
	mu           sync.Mutex
	handler      whatsmeow.EventHandler
	order        *[]string
	qr           chan whatsmeow.QRChannelItem
	connectCalls int
}

func (f *fakeConnection) ConnectContext(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connectCalls++
	if f.order != nil {
		*f.order = append(*f.order, "connect")
	}
	return nil
}
func (f *fakeConnection) Disconnect() {
	if f.order != nil {
		*f.order = append(*f.order, "disconnect")
	}
}
func (f *fakeConnection) AddEventHandler(h whatsmeow.EventHandler) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
	return 1
}
func (f *fakeConnection) RemoveEventHandler(uint32) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = nil
	return true
}
func (f *fakeConnection) GetQRChannel(context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	return f.qr, nil
}

func testRuntime(s *recordingStore) *Runtime {
	jid := types.NewJID("100", types.DefaultUserServer)
	c := whatsmeow.NewClient(&wmstore.Device{ID: &jid}, waLog.Noop)
	return newRuntime(Config{BodyQueueCount: 8, BodyQueueBytes: 8 << 20, MediaMax: 4 << 20}, s, Callbacks{}, c, &fakeConnection{})
}

type contextConnection struct {
	*fakeConnection
	contexts  []context.Context
	qrContext context.Context
	connect   func(context.Context) error
}

func (c *contextConnection) GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	c.qrContext = ctx
	return c.fakeConnection.GetQRChannel(ctx)
}

func (c *contextConnection) ConnectContext(ctx context.Context) error {
	c.contexts = append(c.contexts, ctx)
	if c.connect != nil {
		return c.connect(ctx)
	}
	return c.fakeConnection.ConnectContext(ctx)
}

func TestConnectionUsesOwnedLifetime(t *testing.T) {
	for _, pair := range []bool{true, false} {
		name := "Connect"
		if pair {
			name = "Pair"
		}
		t.Run(name, func(t *testing.T) {
			r := testRuntime(fixtureStore())
			if pair {
				r.client.Store.ID = nil
			}
			c := &contextConnection{fakeConnection: &fakeConnection{}}
			if pair {
				c.qr = make(chan whatsmeow.QRChannelItem)
			}
			r.connection = c
			request, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := r.Connect
			if pair {
				start = r.Pair
			}
			if err := start(request); err != nil {
				t.Fatal(err)
			}
			defer r.Disconnect(context.Background())
			cancel() // The successful HTTP operation has returned.
			if len(c.contexts) != 1 || c.contexts[0].Err() != nil {
				t.Fatal("successful operation cancellation terminated the owned socket context")
			}
			if pair && c.qrContext.Err() != nil {
				t.Fatal("successful operation cancellation terminated QR delivery")
			}
			if err := r.Disconnect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(c.contexts[0].Err(), context.Canceled) || !r.Quiesced() {
				t.Fatal("explicit disconnect did not cancel and drain the owned connection")
			}
			select {
			case <-r.qrDone:
			default:
				t.Fatal("explicit disconnect did not join the QR worker")
			}
		})
	}
	t.Run("PostPairOperation", func(t *testing.T) {
		r := testRuntime(fixtureStore())
		r.client.Store.ID = nil
		c := &contextConnection{fakeConnection: &fakeConnection{}}
		r.connection = c
		if err := r.Pair(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer r.Disconnect(context.Background())
		jid := types.NewJID("100", types.DefaultUserServer)
		r.client.Store.ID = &jid
		r.completePairing(r.observation)
		if len(c.contexts) != 2 || c.contexts[1].Err() != nil {
			t.Fatal("post-pair operation timeout context terminated the new owned socket")
		}
		if !errors.Is(c.contexts[0].Err(), context.Canceled) {
			t.Fatal("post-pair reconnect did not stop the old pairing connection")
		}
	})
	t.Run("StartupCancellation", func(t *testing.T) {
		r := testRuntime(fixtureStore())
		r.client.Store.ID = nil
		entered := make(chan struct{})
		c := &contextConnection{fakeConnection: &fakeConnection{}, connect: func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}}
		r.connection = c
		request, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- r.Pair(request) }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("connection startup was not entered")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, archive.ErrSourceUnavailable) {
				t.Fatalf("cancelled startup: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("request cancellation did not interrupt socket startup")
		}
		if len(c.contexts) != 1 || !errors.Is(c.contexts[0].Err(), context.Canceled) {
			t.Fatal("cancelled startup retained an active socket context")
		}
		if r.observation != "" || !r.Quiesced() {
			t.Fatal("cancelled startup returned before draining its observation and workers")
		}
		for _, done := range []<-chan struct{}{r.workerDone, r.qrDone} {
			select {
			case <-done:
			default:
				t.Fatal("cancelled startup retained an owned worker")
			}
		}
		if err := r.Disconnect(context.Background()); err != nil || !r.Quiesced() {
			t.Fatalf("cancelled startup cleanup: %v", err)
		}
	})
	t.Run("StartupCancellationRacesSuccess", func(t *testing.T) {
		r := testRuntime(fixtureStore())
		request, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := &contextConnection{fakeConnection: &fakeConnection{}, connect: func(context.Context) error {
			cancel()
			return nil
		}}
		r.connection = c
		defer r.Disconnect(context.Background())
		if err := r.Connect(request); !errors.Is(err, archive.ErrSourceUnavailable) {
			t.Fatalf("cancelled successful startup: %v", err)
		}
		if c.contexts[0].Err() == nil || r.observation != "" || !r.Quiesced() {
			t.Fatal("request cancellation racing connect success left a live run")
		}
	})
}

func TestLifecycleObservationPrecedesConnectAndEndsAfterDrain(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	if err := r.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.order) < 2 || s.order[0] != "begin" || s.order[len(s.order)-1] != "end" {
		t.Fatalf("observation ordering: %v", s.order)
	}
	if r.Status().Connection != "disconnected" {
		t.Fatal(r.Status())
	}
}
func TestFailedControlMarksUnsafeAndNeverClearsObservation(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.failApply = errors.New("test_write_failure")
	r.deliver(&events.DeleteForMe{ChatJID: types.NewJID("200", types.DefaultUserServer), SenderJID: types.NewJID("200", types.DefaultUserServer), MessageID: "target", Timestamp: time.Now()})
	_ = r.Disconnect(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.unsafe) == 0 {
		t.Fatal("control failure did not deny archive")
	}
	for _, step := range s.order {
		if step == "end" {
			t.Fatal("unsafe observation cleared")
		}
	}
	if !r.Status().ReconciliationRequired {
		t.Fatal("unsafe status missing")
	}
}
func TestRuntimeStatusDoesNotExposeSourceAccount(t *testing.T) {
	r := testRuntime(fixtureStore())
	if r.SourceAccount() != "100@s.whatsapp.net" {
		t.Fatal("protected source account unavailable")
	}
	if r.Status().HasSession != true {
		t.Fatal("stored session state missing")
	}
}
func TestUnsafeDisconnectAndCloseAreIdempotent(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.failApply = errors.New("test_write_failure")
	r.deliver(&events.DeleteForMe{ChatJID: types.NewJID("200", types.DefaultUserServer), SenderJID: types.NewJID("200", types.DefaultUserServer), MessageID: "target", Timestamp: time.Now()})
	_ = r.Disconnect(context.Background())
	_ = r.Close()
	_ = r.Close()
}
func TestAdmissionBoundsRetainedRawProtocolGraph(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	r.config.BodyQueueBytes = 1024
	r.queue = make(chan queuedEvent, 8)
	r.accepting = true
	e := liveMessage("small", "small")
	e.RawMessage = &waE2E.Message{Conversation: proto.String(strings.Repeat("x", 2048))}
	r.deliver(e)
	if len(r.queue) != 0 || !r.Status().ReconciliationRequired {
		t.Fatal("retained raw graph bypassed byte admission bound")
	}
}
func TestAppStateGapAllowsCleanObservationShutdown(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.AppStateSyncError{})
	if err := r.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Status().ReconciliationRequired || len(s.unsafe) != 0 {
		t.Fatal("app-state coverage gap blocked clean shutdown")
	}
	ended := false
	for _, step := range s.order {
		if step == "end" {
			ended = true
		}
	}
	if !ended {
		t.Fatal("readable-message observation was not cleanly ended")
	}
}

type blockingApplyStore struct {
	*recordingStore
	started, unblock chan struct{}
}

func (s *blockingApplyStore) ApplySourceEvent(ctx context.Context, e archive.SourceEvent, g archive.Generation) (archive.ApplyResult, error) {
	if e.Key.Message == "first" && e.Kind == archive.EventOriginal {
		close(s.started)
		select {
		case <-s.unblock:
		case <-ctx.Done():
			return archive.ApplyResult{}, ctx.Err()
		}
	}
	return s.recordingStore.ApplySourceEvent(ctx, e, g)
}
func TestAdmissionIncludesInFlightBodyCountAndBytes(t *testing.T) {
	for _, byCount := range []bool{true, false} {
		t.Run(map[bool]string{true: "count", false: "bytes"}[byCount], func(t *testing.T) {
			base := fixtureStore()
			s := &blockingApplyStore{recordingStore: base, started: make(chan struct{}), unblock: make(chan struct{})}
			r := testRuntime(base)
			r.store = s
			r.admissionTimeout = 5 * time.Millisecond
			r.config.BodyQueueCount = 2
			r.config.BodyQueueBytes = 600
			if byCount {
				r.config.BodyQueueCount = 1
				r.config.BodyQueueBytes = 8 << 20
			}
			if err := r.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			r.deliver(liveMessage("first", strings.Repeat("a", 400)))
			select {
			case <-s.started:
			case <-time.After(time.Second):
				t.Fatal("body did not become in-flight")
			}
			r.deliver(liveMessage("second", strings.Repeat("b", 400)))
			denied := r.Status().ReconciliationRequired
			close(s.unblock)
			_ = r.Close()
			if !denied {
				t.Fatal("in-flight body released admission capacity before commit")
			}
		})
	}
}

type failingCloser struct{ err error }

func (c *failingCloser) Close() error { return c.err }
func TestSessionCloseFailureCannotAuthorizeReset(t *testing.T) {
	r := testRuntime(fixtureStore())
	c := &failingCloser{err: errors.New("synthetic_close_failure")}
	r.session = c
	if err := r.Close(); !errors.Is(err, archive.ErrSourceUnavailable) {
		t.Fatalf("session failure did not override lifecycle result: %v", err)
	}
	if r.Quiesced() {
		t.Fatal("failed session close authorized reset")
	}
	_ = r.Disconnect(context.Background())
	if r.Quiesced() {
		t.Fatal("disconnect erased an outstanding session-close failure")
	}
	c.err = nil
	if err := r.Close(); err != nil || !r.Quiesced() {
		t.Fatalf("safe retry failed: %v", err)
	}
}
