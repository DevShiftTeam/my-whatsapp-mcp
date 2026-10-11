package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type admissionStore struct {
	*recordingStore
	ids               []archive.MediaID
	eligibleCalls     int
	denyAfterDownload bool
	versions          []archive.SourceVersion
	eligibleErrAt     int
	eligibleErr       error
	updateErr         error
	failMessage       string
}

func (s *admissionStore) ApplySourceEvent(ctx context.Context, e archive.SourceEvent, g archive.Generation) (archive.ApplyResult, error) {
	if e.Key.Message == s.failMessage && s.failMessage != "" {
		return archive.ApplyResult{}, archive.ErrArchiveUnavailable
	}
	r, err := s.recordingStore.ApplySourceEvent(ctx, e, g)
	if err == nil {
		r.MediaIDs = append([]archive.MediaID(nil), s.ids...)
	}
	return r, err
}
func (s *admissionStore) EligibleMedia(_ context.Context, _ archive.SourceKey, version archive.SourceVersion) (bool, error) {
	s.eligibleCalls++
	s.versions = append(s.versions, version)
	if s.eligibleCalls == s.eligibleErrAt {
		return false, s.eligibleErr
	}
	return !s.denyAfterDownload || s.eligibleCalls == 1, nil
}

func (s *admissionStore) UpdateMedia(ctx context.Context, key archive.SourceKey, version archive.SourceVersion, status string) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	return s.recordingStore.UpdateMedia(ctx, key, version, status)
}

type fakeDownloadAdmission struct {
	file     whatsmeow.File
	complete func(context.Context, VerifiedMedia) error
	close    func() error
}

type nilStatMediaFile struct{ whatsmeow.File }

func (f nilStatMediaFile) Stat() (os.FileInfo, error) { return nil, nil }

func (a *fakeDownloadAdmission) File() whatsmeow.File { return a.file }
func (a *fakeDownloadAdmission) Complete(ctx context.Context, m VerifiedMedia) error {
	if a.complete != nil {
		return a.complete(ctx, m)
	}
	return nil
}
func (a *fakeDownloadAdmission) Close() error {
	if a.close != nil {
		return a.close()
	}
	return nil
}

func admissionFixture(t *testing.T) (*Runtime, *admissionStore) {
	t.Helper()
	base := fixtureStore()
	s := &admissionStore{recordingStore: base, ids: []archive.MediaID{"transaction-owned-media"}}
	r := testRuntime(base)
	r.store = s
	return r, s
}

// Pipeline unit tests explicitly invoke the context-only transfer after the
// source commit. Product projection only wakes the durable optional loop.
func commitAndTransferForTest(ctx context.Context, r *Runtime, s *admissionStore, event *events.Message) error {
	if err := r.message(ctx, event); err != nil {
		return err
	}
	s.mu.Lock()
	source := s.events[len(s.events)-1]
	version := s.metadata[source.Key].Version
	s.mu.Unlock()
	p, err := r.projectBody(ctx, &source, event.Message)
	if err != nil || p == nil {
		return err
	}
	defer wipePending(p)
	p.key, p.version, p.mediaID = source.Key, version, s.ids[0]
	return r.transferMedia(ctx, p, nil)
}

type scheduledAdmissionStore struct {
	*admissionStore
	scheduling *optionalStore
}

func (s *scheduledAdmissionStore) ClaimOriginalDownload(ctx context.Context, q archive.OriginalDownloadClaim) (archive.OriginalDownloadWork, error) {
	return s.scheduling.ClaimOriginalDownload(ctx, q)
}
func (s *scheduledAdmissionStore) LoadOriginalDownload(ctx context.Context, q archive.OriginalDownloadLease) (archive.OriginalDownloadInput, error) {
	return s.scheduling.LoadOriginalDownload(ctx, q)
}
func (s *scheduledAdmissionStore) FinishOriginalDownload(ctx context.Context, q archive.OriginalDownloadLease) (bool, error) {
	return s.scheduling.FinishOriginalDownload(ctx, q)
}
func (s *scheduledAdmissionStore) RetryOriginalDownload(ctx context.Context, q archive.OriginalDownloadRetry) error {
	return s.scheduling.RetryOriginalDownload(ctx, q)
}

func admissionSchedulingFixture(t *testing.T) (*Runtime, *admissionStore, *optionalStore) {
	t.Helper()
	r, s := admissionFixture(t)
	work, input := syntheticOriginalWork(t, r.SourceAccount())
	scheduling := &optionalStore{recordingStore: s.recordingStore, work: work, input: input}
	r.store = &scheduledAdmissionStore{admissionStore: s, scheduling: scheduling}
	t.Cleanup(func() { _ = r.Close() })
	return r, s, scheduling
}
func admissionFile(t *testing.T, ctx context.Context, maximum int64) (whatsmeow.File, func() error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "owned-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	capped, err := newCappedFile(ctx, f, maximum)
	if err != nil {
		t.Fatal(err)
	}
	return capped, func() error { return errors.Join(f.Close(), os.Remove(f.Name())) }
}

func TestMediaAdmissionBindsTransactionBeforeCiphertextAndCompletesBeforeClose(t *testing.T) {
	r, s := admissionFixture(t)
	var order []string
	var request MediaDownloadRequest
	var borrowed io.Reader
	r.callbacks.MediaReceiver = func(context.Context, VerifiedMedia) error { t.Fatal("legacy fallback used"); return nil }
	r.callbacks.MediaAdmitter = func(ctx context.Context, q MediaDownloadRequest) (DownloadAdmission, error) {
		order = append(order, "admit")
		if len(s.events) != 1 {
			t.Fatal("admitted before source commit")
		}
		request = q
		if q.MediaID != s.ids[0] || q.Version != s.metadata[q.Key].Version || q.DeclaredBytes != 6 || q.CiphertextMaximum != 26 || q.ProtocolSHA256 != sha256.Sum256([]byte("source")) {
			t.Fatalf("wrong exact source admission: %+v", q)
		}
		f, closeFile := admissionFile(t, ctx, q.CiphertextMaximum)
		return &fakeDownloadAdmission{file: f, complete: func(_ context.Context, m VerifiedMedia) error {
			order = append(order, "complete")
			borrowed = m.Reader
			got := make([]byte, 1)
			_, err := io.ReadFull(m.Reader, got)
			if err != nil || !bytes.Equal(got, []byte("s")) || m.Key != q.Key || m.Version != q.Version || m.Bytes != 6 || m.SHA256 != q.ProtocolSHA256 {
				t.Fatal("verified completion lost original authority")
			}
			return nil
		}, close: func() error { order = append(order, "close"); return closeFile() }}, nil
	}
	r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, f whatsmeow.File) error {
		order = append(order, "download")
		if request.MediaID == "" {
			t.Fatal("download started without admission")
		}
		if _, raw := f.(*os.File); raw {
			t.Fatal("raw descriptor exposed")
		}
		if _, err := f.Write(make([]byte, 26)); err != nil {
			return err
		}
		if _, err := f.Write([]byte{0}); !errors.Is(err, errLimitExceeded) {
			t.Fatal("ciphertext cap missing")
		}
		if err := f.Truncate(6); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		_, err := f.Write([]byte("source"))
		return err
	}
	if err := commitAndTransferForTest(context.Background(), r, s, validImageEvent()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"admit", "download", "complete", "close"}) || len(s.mediaUpdates) != 0 {
		t.Fatalf("wrong lifecycle/retention owner: %v %v", order, s.mediaUpdates)
	}
	if n, err := borrowed.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Fatal("borrowed reader remained live")
	}
}

func TestMediaAdmissionUnknownLengthReservesBoundedMaximum(t *testing.T) {
	r, s := admissionFixture(t)
	e := validImageEvent()
	e.Message.ImageMessage.FileLength = proto.Uint64(0)
	called := false
	r.callbacks.MediaAdmitter = func(ctx context.Context, q MediaDownloadRequest) (DownloadAdmission, error) {
		called = true
		if q.DeclaredBytes != 0 || q.CiphertextMaximum != (r.config.MediaMax/16+1)*16+10 {
			t.Fatal("unknown length did not reserve bounded maximum")
		}
		f, closeFile := admissionFile(t, ctx, q.CiphertextMaximum)
		return &fakeDownloadAdmission{file: f, close: closeFile}, nil
	}
	r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, f whatsmeow.File) error {
		_, err := f.Write([]byte("source"))
		return err
	}
	if err := commitAndTransferForTest(context.Background(), r, s, e); err != nil || !called {
		t.Fatalf("unknown media not admitted: %v", err)
	}
}

func TestMediaAdmissionDenialHasNoDownloadOrLegacyFallback(t *testing.T) {
	r, s := admissionFixture(t)
	r.callbacks.MediaAdmitter = func(context.Context, MediaDownloadRequest) (DownloadAdmission, error) {
		return nil, errors.New("private quota diagnostic")
	}
	r.callbacks.MediaReceiver = func(context.Context, VerifiedMedia) error { t.Fatal("post-verified fallback used"); return nil }
	r.download = func(context.Context, whatsmeow.DownloadableMessage, whatsmeow.File) error {
		t.Fatal("unadmitted writer")
		return nil
	}
	if err := commitAndTransferForTest(context.Background(), r, s, validImageEvent()); !errors.Is(err, ErrMediaUnavailable) {
		t.Fatal("optional media denial did not return fixed media outcome", err)
	}
	if len(s.events) != 1 || len(s.mediaUpdates) != 0 {
		t.Fatal("admission denial altered committed source or discarded retry metadata")
	}
}

func TestMediaAdmissionReturnedWithErrorStillClosesWithoutWriting(t *testing.T) {
	r, s := admissionFixture(t)
	closed := 0
	r.callbacks.MediaAdmitter = func(context.Context, MediaDownloadRequest) (DownloadAdmission, error) {
		return &fakeDownloadAdmission{close: func() error { closed++; return nil }}, errors.New("private admission diagnostic")
	}
	r.download = func(context.Context, whatsmeow.DownloadableMessage, whatsmeow.File) error {
		t.Fatal("failed admission wrote bytes")
		return nil
	}
	if err := commitAndTransferForTest(context.Background(), r, s, validImageEvent()); !errors.Is(err, ErrMediaUnavailable) || closed != 1 {
		t.Fatalf("failed admission cleanup lost: %d %v", closed, err)
	}
}

func TestMediaTypedNilAdmissionNeverStartsWriter(t *testing.T) {
	r, s := admissionFixture(t)
	r.callbacks.MediaAdmitter = func(context.Context, MediaDownloadRequest) (DownloadAdmission, error) {
		var a *fakeDownloadAdmission
		return a, nil
	}
	r.download = func(context.Context, whatsmeow.DownloadableMessage, whatsmeow.File) error {
		t.Fatal("nil admission wrote bytes")
		return nil
	}
	if err := commitAndTransferForTest(context.Background(), r, s, validImageEvent()); !errors.Is(err, ErrMediaInvalidAdmission) {
		t.Fatal(err)
	}
}

func TestMediaAdmissionRequiresExactlyOneTransactionAssociation(t *testing.T) {
	for _, ids := range [][]archive.MediaID{nil, {"a", "b"}, {""}} {
		r, s := admissionFixture(t)
		s.ids = ids
		r.callbacks.MediaAdmitter = func(context.Context, MediaDownloadRequest) (DownloadAdmission, error) {
			t.Fatal("guessed association admitted")
			return nil, nil
		}
		if err := r.message(context.Background(), validImageEvent()); !errors.Is(err, ErrMediaInvalidAdmission) {
			t.Fatalf("ambiguous association not rejected: %v", err)
		}
	}
}

func TestMediaAdmissionStaleSourceOrInvalidReceiptClosesWithoutCompletion(t *testing.T) {
	for _, mode := range []string{"stale", "hash", "length", "nil_file", "typed_nil_file", "nil_stat", "raw_file", "complete_failed", "close_failed"} {
		t.Run(mode, func(t *testing.T) {
			r, s := admissionFixture(t)
			s.denyAfterDownload = mode == "stale"
			closed, completed := 0, 0
			r.callbacks.MediaAdmitter = func(ctx context.Context, q MediaDownloadRequest) (DownloadAdmission, error) {
				f, closeFile := admissionFile(t, ctx, q.CiphertextMaximum)
				if mode == "nil_file" {
					f = nil
				}
				if mode == "typed_nil_file" {
					var nilFile *cappedFile
					f = nilFile
				}
				if mode == "nil_stat" {
					f = nilStatMediaFile{f}
				}
				if mode == "raw_file" {
					raw, err := os.CreateTemp(t.TempDir(), "raw-*")
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = raw.Close() })
					f = raw
				}
				return &fakeDownloadAdmission{file: f, complete: func(context.Context, VerifiedMedia) error {
					completed++
					if mode == "complete_failed" {
						return errors.New("private uncertain completion diagnostic")
					}
					return nil
				}, close: func() error {
					closed++
					err := closeFile()
					if mode == "close_failed" {
						return errors.New("private cleanup diagnostic")
					}
					return err
				}}, nil
			}
			r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, f whatsmeow.File) error {
				if mode == "nil_file" || mode == "typed_nil_file" || mode == "nil_stat" || mode == "raw_file" {
					t.Fatal("invalid supplied file reached downloader")
				}
				body := "source"
				if mode == "hash" {
					body = "forged"
				}
				if mode == "length" {
					body = "source!"
				}
				_, err := f.Write([]byte(body))
				return err
			}
			err := commitAndTransferForTest(context.Background(), r, s, validImageEvent())
			want := ErrMediaInvalidAdmission
			switch mode {
			case "stale":
				want = ErrMediaSourceChanged
			case "hash", "length":
				want = ErrMediaAuthentication
			case "complete_failed":
				want = ErrMediaUnavailable
			case "close_failed":
				want = ErrMediaCleanupIncomplete
			}
			if !errors.Is(err, want) {
				t.Fatalf("wrong fixed media outcome %v, want %v", err, want)
			}
			if closed != 1 || completed != map[bool]int{true: 1, false: 0}[mode == "close_failed" || mode == "complete_failed"] {
				t.Fatalf("wrong rejected lifecycle completed=%d closed=%d", completed, closed)
			}
			if mode == "complete_failed" && len(s.mediaUpdates) != 0 {
				t.Fatal("uncertain publication overwritten by transport status")
			}
			for _, version := range s.versions {
				if version.Revision != 1 {
					t.Fatal("expected source version upgraded")
				}
			}
		})
	}
}

func TestMediaAdmissionCloseWaitsForSynchronousWriterAfterCancellation(t *testing.T) {
	r, s := admissionFixture(t)
	entered, stopped, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	r.callbacks.MediaAdmitter = func(ctx context.Context, q MediaDownloadRequest) (DownloadAdmission, error) {
		f, closeFile := admissionFile(t, ctx, q.CiphertextMaximum)
		return &fakeDownloadAdmission{file: f, complete: func(context.Context, VerifiedMedia) error { t.Error("cancelled completion"); return nil }, close: func() error { close(closed); return closeFile() }}, nil
	}
	r.download = func(ctx context.Context, _ whatsmeow.DownloadableMessage, _ whatsmeow.File) error {
		close(entered)
		<-ctx.Done()
		<-stopped
		return ctx.Err()
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- commitAndTransferForTest(ctx, r, s, validImageEvent()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("admitted writer did not start")
	}
	cancel()
	select {
	case <-closed:
		t.Fatal("cleanup ran before synchronous writer stopped")
	case <-time.After(20 * time.Millisecond):
	}
	close(stopped)
	select {
	case err := <-done:
		if !errors.Is(err, ErrMediaCanceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer cleanup did not finish")
	}
	select {
	case <-closed:
	default:
		t.Fatal("admission not closed")
	}
}

type failedMediaRead struct{ whatsmeow.File }

func (f failedMediaRead) Read([]byte) (int, error) { return 0, errors.New("private read diagnostic") }

func TestConsumedOptionalMediaFailuresNeverMarkSourceUnsafe(t *testing.T) {
	for _, mode := range []string{"eligible_before", "eligible_after", "deadline", "read", "update", "admission", "budget", "complete", "close"} {
		t.Run(mode, func(t *testing.T) {
			r, s, scheduling := admissionSchedulingFixture(t)
			want := ErrMediaUnavailable
			switch mode {
			case "eligible_before":
				s.eligibleErrAt = 1
				s.eligibleErr = errors.New("private eligibility diagnostic")
			case "eligible_after":
				s.eligibleErrAt = 2
				s.eligibleErr = errors.New("private eligibility diagnostic")
			case "deadline":
				s.eligibleErrAt = 2
				s.eligibleErr = context.DeadlineExceeded
				want = ErrMediaDeadline
			case "update":
				s.updateErr = errors.New("private update diagnostic")
			case "budget":
				want = ErrMediaWaitingForDiskBudget
			case "close":
				want = ErrMediaCleanupIncomplete
			}
			var closed atomic.Int32
			r.callbacks.MediaAdmitter = func(ctx context.Context, q MediaDownloadRequest) (DownloadAdmission, error) {
				if mode == "admission" {
					return nil, errors.New("private admission diagnostic")
				}
				if mode == "budget" {
					return nil, archive.ErrQuotaExceeded
				}
				f, closeFile := admissionFile(t, ctx, q.CiphertextMaximum)
				if mode == "read" {
					f = failedMediaRead{f}
				}
				return &fakeDownloadAdmission{file: f, complete: func(context.Context, VerifiedMedia) error {
					if mode == "complete" {
						return errors.New("private completion diagnostic")
					}
					return nil
				}, close: func() error {
					n := closed.Add(1)
					if n > 1 {
						return nil
					}
					err := closeFile()
					if mode == "close" {
						return errors.New("private cleanup diagnostic")
					}
					return err
				}}, nil
			}
			r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, f whatsmeow.File) error {
				body := "source"
				if mode == "update" {
					body = "forged"
				}
				_, err := f.Write([]byte(body))
				return err
			}
			if err := r.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			r.deliver(validImageEvent())
			r.deliver(&events.Connected{})
			waitOptional(t, func() bool {
				scheduling.workMu.Lock()
				defer scheduling.workMu.Unlock()
				return scheduling.retryCount == 1
			})
			if err := r.Pause(context.Background()); err != nil {
				t.Fatal(err)
			}
			status := r.Status()
			if status.ReconciliationRequired || status.Connection == "reconciliation_required" || len(s.unsafe) != 0 {
				t.Fatalf("media failure poisoned source archive: %+v unsafe=%v", status, s.unsafe)
			}
			if status.MediaCode != want.Error() || len(s.events) != 1 || status.Applied != 1 {
				t.Fatalf("wrong media-only outcome/valid source receipt: %+v", status)
			}
			if mode != "eligible_before" && mode != "admission" && mode != "budget" && closed.Load() < 1 {
				t.Fatal("admission cleanup skipped")
			}
		})
	}
}

func TestMediaErrorCodesAreFixedAndFinite(t *testing.T) {
	for _, e := range []MediaError{ErrMediaUnavailable, ErrMediaSourceChanged, ErrMediaCanceled, ErrMediaDeadline, ErrMediaWaitingForDiskBudget, ErrMediaCleanupIncomplete, ErrMediaInvalidAdmission, ErrMediaAuthentication, ErrMediaLimit} {
		if !e.valid() || e.Error() == "" {
			t.Fatal("missing media boundary code")
		}
	}
	if MediaError(255).valid() || MediaError(255).Error() != ErrMediaUnavailable.Error() {
		t.Fatal("invalid media status was not masked")
	}
}

func TestConsumedSourceCommitFailureStillMarksArchiveUnsafe(t *testing.T) {
	r, s := admissionFixture(t)
	s.failApply = archive.ErrArchiveUnavailable
	r.callbacks.MediaAdmitter = func(context.Context, MediaDownloadRequest) (DownloadAdmission, error) {
		t.Fatal("admitted failed source commit")
		return nil, nil
	}
	r.runCtx = context.Background()
	r.queueBytes = 64
	r.admissions = 1
	r.consume(queuedEvent{event: validImageEvent(), bytes: 64})
	if !r.Status().ReconciliationRequired || len(s.unsafe) != 1 || len(s.events) != 0 {
		t.Fatal("media containment weakened source commit safety")
	}
}

func TestAuthenticatedHistoryOptionalMediaFailureContinuesSourcesAndControls(t *testing.T) {
	for _, mode := range []string{"admission", "close", "edit_close"} {
		t.Run(mode, func(t *testing.T) {
			r, s, scheduling := admissionSchedulingFixture(t)
			if err := r.message(context.Background(), liveMessage("target", "original")); err != nil {
				t.Fatal(err)
			}
			image := historyRecord("media", "")
			image.Message.Message = validImageEvent().Message
			if mode == "edit_close" {
				if err := r.message(context.Background(), liveMessage("media-target", "original image target")); err != nil {
					t.Fatal(err)
				}
				scheduling.input.Key.Message = "media-target"
				image.Message.Message = r.client.BuildEdit(types.NewJID("200", types.DefaultUserServer), "media-target", image.Message.Message)
				image.Message.Message.EditedMessage.Message.ProtocolMessage.TimestampMS = proto.Int64(2000123)
			}
			later := historyRecord("later", "later source retained")
			control := historyRecord("control", "")
			control.Message.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("target"), RemoteJID: proto.String("200@s.whatsapp.net")}}}
			var closed atomic.Int32
			r.callbacks.MediaAdmitter = func(ctx context.Context, q MediaDownloadRequest) (DownloadAdmission, error) {
				if mode == "admission" {
					return nil, archive.ErrQuotaExceeded
				}
				f, closeFile := admissionFile(t, ctx, q.CiphertextMaximum)
				return &fakeDownloadAdmission{file: f, close: func() error {
					if closed.Add(1) == 1 {
						_ = closeFile()
						return errors.New("private cleanup diagnostic")
					}
					return nil
				}}, nil
			}
			r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, f whatsmeow.File) error {
				_, err := f.Write([]byte("source"))
				return err
			}
			e := liveMessage("history", "")
			e.Info.IsFromMe = true
			e.Info.Sender = *r.client.Store.ID
			e.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(), HistorySyncNotification: &waE2E.HistorySyncNotification{InitialHistBootstrapInlinePayload: compressed(historyWire(image, later, control))}}}
			if err := r.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			r.deliver(e)
			r.deliver(&events.Connected{})
			waitOptional(t, func() bool {
				scheduling.workMu.Lock()
				defer scheduling.workMu.Unlock()
				return scheduling.retryCount == 1
			})
			if err := r.Pause(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := r.Status()
			if got.HistoryChunks != 1 || got.ReconciliationRequired || len(s.unsafe) != 0 {
				t.Fatalf("optional media aborted authenticated history: %+v %v", got, s.unsafe)
			}
			foundLater, foundDelete, foundImage := false, false, false
			for _, event := range s.events {
				foundLater = foundLater || event.Key.Message == "later" && event.Body == "later source retained"
				foundDelete = foundDelete || event.Key.Message == "target" && event.Kind == archive.EventDelete
				foundImage = foundImage || event.BodyKind == archive.MessageImage && (event.Kind == archive.EventOriginal || event.Kind == archive.EventEditContent)
			}
			if !foundLater || !foundDelete || !foundImage {
				t.Fatal("optional media discarded later source/control observations")
			}
			want := ErrMediaCleanupIncomplete
			if mode == "admission" {
				want = ErrMediaWaitingForDiskBudget
			}
			if got.MediaCode != want.Error() {
				t.Fatalf("fixed media outcome lost through history: %q", got.MediaCode)
			}
		})
	}
}

func TestAuthenticatedHistoryLaterSourceFailureStillQuarantines(t *testing.T) {
	r, s := admissionFixture(t)
	s.failMessage = "later"
	image := historyRecord("media", "")
	image.Message.Message = validImageEvent().Message
	r.callbacks.MediaAdmitter = func(context.Context, MediaDownloadRequest) (DownloadAdmission, error) {
		return nil, archive.ErrQuotaExceeded
	}
	e := liveMessage("history", "")
	e.Info.IsFromMe = true
	e.Info.Sender = *r.client.Store.ID
	e.Message = &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_HISTORY_SYNC_NOTIFICATION.Enum(), HistorySyncNotification: &waE2E.HistorySyncNotification{InitialHistBootstrapInlinePayload: compressed(historyWire(image, historyRecord("later", "source failure")))}}}
	if err := r.historyFrom(context.Background(), e); err == nil {
		t.Fatal("genuine source failure contained as optional media")
	}
	if !r.Status().ReconciliationRequired || r.Status().HistoryChunks != 0 || len(s.unsafe) == 0 || len(s.events) != 1 {
		t.Fatal("later source failure did not preserve fail-closed history authority")
	}
}

func TestProductionMediaReceiverAloneCannotRetainOrDownload(t *testing.T) {
	r, s := admissionFixture(t)
	r.callbacks.MediaReceiver = func(context.Context, VerifiedMedia) error { t.Fatal("production receiver fallback"); return nil }
	r.download = func(context.Context, whatsmeow.DownloadableMessage, whatsmeow.File) error {
		t.Fatal("unreserved production download")
		return nil
	}
	if err := r.message(context.Background(), validImageEvent()); err != nil || len(s.events) != 1 {
		t.Fatalf("source receipt failed: %v", err)
	}
}

func TestCiphertextMaximumCoversPaddingAndMACWithoutOverflow(t *testing.T) {
	for _, plain := range []int64{1, 6, 15, 16, 17, 64 << 20} {
		got, err := mediaCiphertextMaximum(plain)
		if err != nil || got != (plain/16+1)*16+10 || got > plain+26 {
			t.Fatalf("wrong wire bound %d => %d/%v", plain, got, err)
		}
	}
	for _, plain := range []int64{0, -1, 65 << 20, 1 << 62} {
		if _, err := mediaCiphertextMaximum(plain); !errors.Is(err, errInvalidLimit) {
			t.Fatal("unbounded maximum accepted")
		}
	}
}
