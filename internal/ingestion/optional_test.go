package ingestion

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// These ports model only durable scheduling. Physical admission remains a
// separate callback, and completion must follow its synchronous Close.
type optionalStore struct {
	*recordingStore
	workMu                              sync.Mutex
	work                                archive.OriginalDownloadWork
	input                               archive.OriginalDownloadInput
	claimed                             bool
	claimCount, finishCount, retryCount int
	claimQueries                        int
	retained                            atomic.Bool
	selection                           archive.BackfillSelection
	anchors                             []archive.SourceAnchor
	selectorEntered, selectorRelease    chan struct{}
	selectorOnce                        sync.Once
	loadedCopy                          []byte
	lastRetry                           archive.OriginalDownloadRetry
}

func (s *optionalStore) ClaimOriginalDownload(_ context.Context, q archive.OriginalDownloadClaim) (archive.OriginalDownloadWork, error) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	s.claimQueries++
	if q.Account != s.work.Source.Account || s.claimed {
		return archive.OriginalDownloadWork{}, archive.ErrNoWork
	}
	s.claimed = true
	s.claimCount++
	return s.work, nil
}
func (s *optionalStore) LoadOriginalDownload(_ context.Context, lease archive.OriginalDownloadLease) (archive.OriginalDownloadInput, error) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if lease != s.work.Lease {
		return archive.OriginalDownloadInput{}, archive.ErrNoWork
	}
	x := s.input
	x.ProtocolReference = append([]byte(nil), x.ProtocolReference...)
	s.loadedCopy = x.ProtocolReference
	return x, nil
}
func (s *optionalStore) FinishOriginalDownload(_ context.Context, lease archive.OriginalDownloadLease) (bool, error) {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if lease != s.work.Lease {
		return false, archive.ErrNoWork
	}
	s.finishCount++
	return s.retained.Load(), nil
}
func (s *optionalStore) RetryOriginalDownload(_ context.Context, q archive.OriginalDownloadRetry) error {
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if q.Lease != s.work.Lease {
		return archive.ErrNoWork
	}
	s.retryCount++
	s.lastRetry = q
	return nil
}
func (s *optionalStore) SelectedBackfillAnchors(ctx context.Context, q archive.BackfillSelection) ([]archive.SourceAnchor, error) {
	s.workMu.Lock()
	s.selection = q
	anchors := append([]archive.SourceAnchor(nil), s.anchors...)
	s.workMu.Unlock()
	if s.selectorEntered != nil {
		s.selectorOnce.Do(func() { close(s.selectorEntered) })
		select {
		case <-s.selectorRelease:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return anchors, nil
}

func optionalFixture(t *testing.T) (*Runtime, *optionalStore) {
	t.Helper()
	base := fixtureStore()
	r := testRuntime(base)
	work, input := syntheticOriginalWork(t, r.SourceAccount())
	s := &optionalStore{recordingStore: base, work: work, input: input}
	r.store = s
	r.callbacks.MediaAdmitter = func(ctx context.Context, _ MediaDownloadRequest) (DownloadAdmission, error) {
		file, e := newMemoryFile(ctx, 1024)
		if e != nil {
			return nil, e
		}
		return &fakeDownloadAdmission{file: file, complete: func(_ context.Context, v VerifiedMedia) error {
			if v.Bytes != 6 || v.SHA256 != sha256.Sum256([]byte("source")) {
				return ErrMediaAuthentication
			}
			s.retained.Store(true)
			return nil
		}, close: file.Close}, nil
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, s
}

func syntheticOriginalWork(t *testing.T, account string) (archive.OriginalDownloadWork, archive.OriginalDownloadInput) {
	t.Helper()
	image := validImageEvent().Message.GetImageMessage()
	// Persist only the production reduced authenticated descriptor.
	descriptor := &waE2E.ImageMessage{DirectPath: proto.String(image.GetDirectPath()), MediaKey: append([]byte(nil), image.GetMediaKey()...), FileSHA256: append([]byte(nil), image.GetFileSHA256()...), FileEncSHA256: append([]byte(nil), image.GetFileEncSHA256()...), FileLength: proto.Uint64(image.GetFileLength())}
	data, err := proto.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	key := archive.SourceKey{Account: account, Chat: "200@s.whatsapp.net", Sender: "200@s.whatsapp.net", Message: "media"}
	work := archive.OriginalDownloadWork{Lease: archive.OriginalDownloadLease{MediaID: "transaction-owned-media", LeaseID: "synthetic-download-lease"}, Source: archive.SourceRef{Account: key.Account, ContentHash: strings.Repeat("a", 64), Version: archive.SourceVersion{MessageID: "opaque", Revision: 1, PolicyGeneration: 1}}, Kind: archive.MessageImage, DeclaredBytes: 6, Attempts: 1, LeaseUntil: time.Now().Add(2 * time.Minute)}
	return work, archive.OriginalDownloadInput{Work: work, Key: key, ProtocolReference: data}
}
func waitOptional(t *testing.T, check func() bool) {
	t.Helper()
	waitOptionalFor(t, time.Second, check)
}
func waitOptionalFor(t *testing.T, maximum time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(maximum)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("bounded optional operation did not reach expected state")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestOptionalFixedPollFindsDueWorkWithoutNewSourceWake(t *testing.T) {
	r, s := optionalFixture(t)
	s.claimed = true // Not yet due; the first source/start wake finds no work.
	r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, f whatsmeow.File) error {
		_, e := f.Write([]byte("source"))
		return e
	}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitOptional(t, func() bool {
		s.workMu.Lock()
		queried := s.claimQueries > 0
		s.workMu.Unlock()
		r.optionalMu.Lock()
		empty := len(r.optional.commands) == 0
		r.optionalMu.Unlock()
		r.mu.Lock()
		idle := r.admissions == 0
		r.mu.Unlock()
		return queried && empty && idle
	})
	s.workMu.Lock()
	s.claimed = false
	s.workMu.Unlock()
	// No wake is sent. The one fixed loop timer must notice durable due work.
	waitOptionalFor(t, optionalPoll+time.Second, func() bool { s.workMu.Lock(); defer s.workMu.Unlock(); return s.finishCount == 1 })
	if err := r.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if s.claimCount != 1 || s.retryCount != 0 {
		t.Fatal("fixed timer duplicated due original work")
	}
}

func TestOptionalTransferLeavesSourceAndControlsResponsive(t *testing.T) {
	r, s := optionalFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, f whatsmeow.File) error {
		close(started)
		<-release // Deliberately model synchronous cancellation/reaping delay.
		_, e := f.Write([]byte("source"))
		return e
	}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("optional transfer never started")
	}
	for i := 0; i < 100; i++ {
		r.WakeOriginalDownloads()
	}
	r.deliver(liveMessage("after-optional", "source remains live"))
	waitOptional(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.events) == 1 })
	m := liveMessage("after-optional", "")
	r.deliver(&events.DeleteForMe{ChatJID: m.Info.Chat, SenderJID: m.Info.Sender, MessageID: m.Info.ID, Timestamp: time.Now()})
	waitOptional(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.events) == 2 })
	s.mu.Lock()
	unsafe := len(s.unsafe)
	s.mu.Unlock()
	s.workMu.Lock()
	claims := s.claimCount
	s.workMu.Unlock()
	if unsafe != 0 || claims != 1 {
		t.Fatal("optional work blocked source authority or started duplicate transfer")
	}
	once.Do(func() { close(release) })
	if err := r.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOptionalJoinTimeoutPersistsAcrossStopAndResumeWithoutUnsafe(t *testing.T) {
	r, s := optionalFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, _ whatsmeow.File) error {
		close(started)
		<-release
		return context.Canceled
	}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("optional transfer never started")
	}
	for _, stop := range []func(context.Context) error{r.Pause, r.Pause, r.Resume} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := stop(ctx)
		cancel()
		if !errors.Is(err, ErrMediaCleanupIncomplete) || r.Quiesced() {
			t.Fatal("unjoined optional transfer erased persistent cleanup barrier")
		}
	}
	// Losing the session cannot turn an unresolved optional owner into a
	// successful no-session Resume or permit a replacement loop.
	r.client.Store.ID = nil
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := r.Resume(ctx)
	cancel()
	if !errors.Is(err, ErrMediaCleanupIncomplete) || r.Quiesced() {
		t.Fatal("no-session Resume bypassed optional join")
	}
	s.mu.Lock()
	unsafe := len(s.unsafe)
	s.mu.Unlock()
	if unsafe != 0 || r.Status().ReconciliationRequired {
		t.Fatal("optional-only join timeout quarantined source archive")
	}
	once.Do(func() { close(release) })
	if err := r.Pause(context.Background()); err != nil || !r.Quiesced() {
		t.Fatal("joined optional work did not permit clean stop")
	}
}

func TestOptionalQueueSaturationAndCanceledRequestNeverMarkUnsafe(t *testing.T) {
	r, s := optionalFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	r.config.BackfillRequests = 32
	r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, _ whatsmeow.File) error {
		close(started)
		<-release
		return context.Canceled
	}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("optional transfer never started")
	}
	q := BackfillRequest{PolicyGeneration: 1, AllSelected: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.RequestBackfill(ctx, q); !errors.Is(err, ErrMediaCanceled) {
		t.Fatal("canceled optional request admitted")
	}
	// An outstanding coalesced wake may be replaced by explicit metadata.
	if err := r.RequestBackfill(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if err := r.RequestBackfill(context.Background(), q); err == nil {
		t.Fatal("optional request queue exceeded capacity1")
	}
	s.mu.Lock()
	unsafe := len(s.unsafe)
	s.mu.Unlock()
	if unsafe != 0 || r.Status().ReconciliationRequired {
		t.Fatal("optional queue saturation became lost source delivery")
	}
	once.Do(func() { close(release) })
	if err := r.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type optionalBlockingSourceStore struct {
	*optionalStore
	entered, release chan struct{}
}

func (s *optionalBlockingSourceStore) ApplySourceEvent(ctx context.Context, e archive.SourceEvent, g archive.Generation) (archive.ApplyResult, error) {
	if e.Key.Message == "blocked-source" {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return archive.ApplyResult{}, ctx.Err()
		}
	}
	return s.recordingStore.ApplySourceEvent(ctx, e, g)
}

func TestOptionalBackfillRechecksSourcePriorityAfterMetadataBeforeSend(t *testing.T) {
	r, s := optionalFixture(t)
	r.callbacks.MediaAdmitter = nil
	r.config.BackfillRequests = 1
	s.anchors = []archive.SourceAnchor{backfillAnchor("200", "anchor", time.Unix(1000, 0))}
	s.selectorEntered, s.selectorRelease = make(chan struct{}), make(chan struct{})
	blocked := &optionalBlockingSourceStore{optionalStore: s, entered: make(chan struct{}), release: make(chan struct{})}
	r.store = blocked
	var selectorOnce, sourceOnce sync.Once
	defer selectorOnce.Do(func() { close(s.selectorRelease) })
	defer sourceOnce.Do(func() { close(blocked.release) })
	sent := make(chan struct{}, 1)
	r.sendHistoryRequest = func(context.Context, *waE2E.Message) error { sent <- struct{}{}; return nil }
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	select {
	case <-s.selectorEntered:
	case <-time.After(time.Second):
		t.Fatal("selection did not enter metadata wait")
	}
	r.deliver(liveMessage("blocked-source", "accepted ordinary source"))
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("source body was not admitted during selector wait")
	}
	selectorOnce.Do(func() { close(s.selectorRelease) })
	select {
	case <-sent:
		t.Fatal("backfill sent while accepted source work was pending")
	case <-time.After(20 * time.Millisecond):
	}
	r.optionalMu.Lock()
	used := r.backfillRequests
	r.optionalMu.Unlock()
	if used != 0 {
		t.Fatal("yield consumed anchor allowance")
	}
	sourceOnce.Do(func() { close(blocked.release) })
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("yielded exact anchor did not resume after source commit")
	}
	if err := r.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) != 1 || len(s.unsafe) != 0 {
		t.Fatal("optional yield changed source safety or receipt")
	}
}

func TestOptionalFailedCloseRetainsHandleAndBlocksSchedulingAcknowledgement(t *testing.T) {
	r, s := optionalFixture(t)
	var allow atomic.Bool
	var closes atomic.Int32
	r.callbacks.MediaAdmitter = func(ctx context.Context, _ MediaDownloadRequest) (DownloadAdmission, error) {
		file, err := newMemoryFile(ctx, 1024)
		if err != nil {
			return nil, err
		}
		var once sync.Once
		return &fakeDownloadAdmission{file: file, complete: func(context.Context, VerifiedMedia) error { s.retained.Store(true); return nil }, close: func() error {
			closes.Add(1)
			once.Do(func() { _ = file.Close() })
			if !allow.Load() {
				return errors.New("private stopped-cleanup diagnostic")
			}
			return nil
		}}, nil
	}
	r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, f whatsmeow.File) error {
		_, e := f.Write([]byte("source"))
		return e
	}
	defer allow.Store(true)
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitOptional(t, func() bool { return closes.Load() >= 2 && r.Status().MediaCode == ErrMediaCleanupIncomplete.Error() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := r.Pause(ctx)
	cancel()
	if !errors.Is(err, ErrMediaCleanupIncomplete) || r.Quiesced() {
		t.Fatal("failed physical cleanup acknowledged as joined")
	}
	s.workMu.Lock()
	finish, retry := s.finishCount, s.retryCount
	s.workMu.Unlock()
	if finish != 0 || retry != 0 {
		t.Fatal("scheduling acknowledged before stopped handle cleanup")
	}
	s.mu.Lock()
	unsafe := len(s.unsafe)
	s.mu.Unlock()
	if unsafe != 0 || r.Status().ReconciliationRequired {
		t.Fatal("cleanup barrier quarantined valid source")
	}
	allow.Store(true)
	ctx, cancel = context.WithTimeout(context.Background(), optionalPoll+time.Second)
	defer cancel()
	if err := r.Pause(ctx); err != nil || !r.Quiesced() {
		t.Fatal("same stopped owner did not settle bounded cleanup tail", err)
	}
	s.workMu.Lock()
	finish, retry = s.finishCount, s.retryCount
	s.workMu.Unlock()
	if finish != 1 || retry != 0 || closes.Load() < 3 {
		t.Fatal("retained original lost authoritative Finish or reused completed row")
	}
}

func TestOptionalCompletionReplyLossUsesAuthoritativeFinishWithoutRetry(t *testing.T) {
	r, s := optionalFixture(t)
	r.callbacks.MediaAdmitter = func(ctx context.Context, _ MediaDownloadRequest) (DownloadAdmission, error) {
		file, err := newMemoryFile(ctx, 1024)
		if err != nil {
			return nil, err
		}
		return &fakeDownloadAdmission{file: file, complete: func(context.Context, VerifiedMedia) error {
			s.retained.Store(true)
			return errors.New("private completion reply lost")
		}, close: file.Close}, nil
	}
	r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, f whatsmeow.File) error {
		_, e := f.Write([]byte("source"))
		return e
	}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitOptional(t, func() bool { s.workMu.Lock(); defer s.workMu.Unlock(); return s.finishCount == 1 })
	if err := r.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.workMu.Lock()
	defer s.workMu.Unlock()
	if s.retryCount != 0 || s.claimCount != 1 || !s.retained.Load() {
		t.Fatal("uncertain completion caused duplicate original retry")
	}
}

func TestOriginalDescriptorDecodeBoundsAndLoadedVersionAreNotUpgraded(t *testing.T) {
	for _, mode := range []string{"oversized", "malformed", "different_account", "different_version", "declared", "key", "direct_path"} {
		t.Run(mode, func(t *testing.T) {
			r, s := optionalFixture(t)
			switch mode {
			case "oversized":
				s.input.ProtocolReference = make([]byte, 8193)
			case "malformed":
				s.input.ProtocolReference = []byte{0xff}
			case "different_account":
				s.input.Key.Account = "other@s.whatsapp.net"
			case "different_version":
				s.input.Work.Source.Version.Revision++
			case "declared":
				s.input.Work.DeclaredBytes++
			case "key", "direct_path":
				var m waE2E.ImageMessage
				if err := proto.Unmarshal(s.input.ProtocolReference, &m); err != nil {
					t.Fatal(err)
				}
				if mode == "key" {
					m.MediaKey = []byte{1}
				} else {
					m.DirectPath = proto.String("https://untrusted.invalid/arbitrary")
				}
				var err error
				s.input.ProtocolReference, err = proto.Marshal(&m)
				if err != nil {
					t.Fatal(err)
				}
			}
			r.callbacks.MediaAdmitter = func(context.Context, MediaDownloadRequest) (DownloadAdmission, error) {
				t.Error("malformed persisted authority reached admission")
				return nil, ErrMediaInvalidAdmission
			}
			r.download = func(context.Context, whatsmeow.DownloadableMessage, whatsmeow.File) error {
				t.Error("malformed persisted authority reached network")
				return nil
			}
			if err := r.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			r.deliver(&events.Connected{})
			waitOptional(t, func() bool { s.workMu.Lock(); defer s.workMu.Unlock(); return s.retryCount == 1 })
			if err := r.Pause(context.Background()); err != nil {
				t.Fatal(err)
			}
			if r.Status().MediaCode != ErrMediaInvalidAdmission.Error() || r.Status().ReconciliationRequired {
				t.Fatal("malformed retry was not bounded media-only denial")
			}
			s.workMu.Lock()
			defer s.workMu.Unlock()
			if s.lastRetry.Lease != s.work.Lease || s.lastRetry.Code != ErrMediaInvalidAdmission.Error() || !s.lastRetry.Terminal {
				t.Fatal("malformed retry lost exact lease or finite terminal code")
			}
			for _, b := range s.loadedCopy {
				if b != 0 {
					t.Fatal("private loaded protocol bytes were not cleared")
				}
			}
		})
	}
}

func TestCommittedOriginalOnlyWakesAndNeverRunsNetworkOnSourceActor(t *testing.T) {
	r, s := admissionFixture(t)
	r.callbacks.MediaAdmitter = func(context.Context, MediaDownloadRequest) (DownloadAdmission, error) {
		t.Fatal("projection network admission ran inline")
		return nil, nil
	}
	r.download = func(context.Context, whatsmeow.DownloadableMessage, whatsmeow.File) error {
		t.Fatal("projection network ran inline")
		return nil
	}
	if err := r.message(context.Background(), validImageEvent()); err != nil {
		t.Fatal(err)
	}
	if len(s.events) != 1 || s.eligibleCalls != 0 {
		t.Fatal("source commit was withheld for optional transfer")
	}
}

func TestOptionalStopCancelsNetworkWhileAcceptedOrdinarySourceDrains(t *testing.T) {
	r, s := optionalFixture(t)
	blocked := &optionalBlockingSourceStore{optionalStore: s, entered: make(chan struct{}), release: make(chan struct{})}
	r.store = blocked
	networkEntered, networkCanceled, networkStopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var sourceOnce, networkOnce sync.Once
	defer sourceOnce.Do(func() { close(blocked.release) })
	defer networkOnce.Do(func() { close(networkStopped) })
	r.download = func(ctx context.Context, _ whatsmeow.DownloadableMessage, _ whatsmeow.File) error {
		close(networkEntered)
		<-ctx.Done()
		close(networkCanceled)
		<-networkStopped
		return ctx.Err()
	}
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	select {
	case <-networkEntered:
	case <-time.After(time.Second):
		t.Fatal("optional transfer did not start")
	}
	r.deliver(liveMessage("blocked-source", "accepted body must drain"))
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("ordinary source was not admitted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Pause(ctx) }()
	select {
	case <-networkCanceled:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel optional network promptly")
	}
	if r.runCtx.Err() != nil {
		t.Fatal("stop canceled accepted ordinary source before drain")
	}
	sourceOnce.Do(func() { close(blocked.release) })
	waitOptional(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, step := range s.order {
			if step == "end" {
				return true
			}
		}
		return false
	})
	select {
	case <-done:
		t.Fatal("stop acknowledged before synchronous network owner returned")
	default:
	}
	networkOnce.Do(func() { close(networkStopped) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop did not join cleanup after source drain")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) != 1 || len(s.unsafe) != 0 || !r.Quiesced() {
		t.Fatal("accepted source drainage lost receipt or poisoned archive")
	}
}

func TestBackfillRequestValidatesAndCopiesOnlyBoundedSelectionMetadata(t *testing.T) {
	for _, q := range []BackfillRequest{
		{AllSelected: true}, {PolicyGeneration: 1}, {PolicyGeneration: 1, AllSelected: true, Chats: []archive.ChatID{"chat"}},
		{PolicyGeneration: 1, Chats: []archive.ChatID{""}}, {PolicyGeneration: 1, Chats: []archive.ChatID{"chat", "chat"}},
		{PolicyGeneration: 1, Chats: []archive.ChatID{archive.ChatID(strings.Repeat("a", 129))}},
		{PolicyGeneration: 1, Chats: []archive.ChatID{"a\x00b"}}, {PolicyGeneration: 1, AllSelected: true, Limit: 33},
	} {
		if !errors.Is(validateBackfillRequest(q), ErrMediaInvalidAdmission) {
			t.Fatal("invalid selection admitted")
		}
	}
	r, _ := optionalFixture(t)
	r.config.BackfillRequests = 32
	n := &optionalNetwork{commands: make(chan optionalCommand, 1), accepting: true}
	r.optionalMu.Lock()
	r.optional = n
	r.optionalMu.Unlock()
	// This test owns no goroutine or lifecycle, and restores the empty slot
	// before fixture cleanup. Only the bounded metadata channel is exercised.
	defer func() { r.optionalMu.Lock(); r.optional = nil; r.optionalMu.Unlock() }()
	q := BackfillRequest{PolicyGeneration: 1, Chats: []archive.ChatID{"opaque-chat"}}
	if err := r.RequestBackfill(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	q.Chats[0] = "caller-mutated"
	cmd := <-n.commands
	if cmd.request.Chats[0] != "opaque-chat" || cmd.request.Limit != 32 || cmd.request.AllSelected || cmd.automatic {
		t.Fatal("queued request borrowed caller metadata or authority")
	}
}

func TestExplicitBackfillCooldownSurvivesPauseAndResume(t *testing.T) {
	r, _ := optionalFixture(t)
	r.config.BackfillRequests = 32
	r.callbacks.MediaAdmitter = nil
	if err := r.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitOptional(t, func() bool {
		r.optionalMu.Lock()
		pending := r.optional.backfillPending
		r.optionalMu.Unlock()
		return r.Status().Connection == "connected" && !pending
	})
	q := BackfillRequest{PolicyGeneration: 1, AllSelected: true}
	if err := r.RequestBackfill(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	waitOptional(t, func() bool { r.optionalMu.Lock(); defer r.optionalMu.Unlock(); return !r.optional.backfillPending })
	if err := r.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.deliver(&events.Connected{})
	waitOptional(t, func() bool {
		r.optionalMu.Lock()
		pending := r.optional.backfillPending
		r.optionalMu.Unlock()
		return r.Status().Connection == "connected" && !pending
	})
	if err := r.RequestBackfill(context.Background(), q); !errors.Is(err, ErrMediaLimit) {
		t.Fatal("new optional loop reset explicit request cooldown", err)
	}
	if err := r.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
}
