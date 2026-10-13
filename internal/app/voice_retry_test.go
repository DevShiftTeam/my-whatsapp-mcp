//go:build sqlite_fts5

package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/ingestion"
	"github.com/Bodokh/my-whatsapp-mcp/internal/localui"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestVoiceRetryApplicationDerivesAccountAndWakesOnlyQueuedWork(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	runtime := &downloadWakeConnector{}
	a.runtime = runtime
	defer func() { a.runtime = nil }()
	port, ok := any(a).(localui.VoiceRetryApplication)
	if !ok {
		t.Fatal("application does not expose owner voice retry")
	}
	if _, err = port.RetryVoiceDownloads(context.Background()); !errors.Is(err, localui.ErrVoiceRetryUnavailable) || runtime.wakes != 0 {
		t.Fatal("unconfigured archive invented retry account")
	}
	policy, err := a.store.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := proto.Marshal(&waE2E.AudioMessage{DirectPath: proto.String("/synthetic/voice-retry"), MediaKey: bytes.Repeat([]byte{0x11}, 32), FileSHA256: bytes.Repeat([]byte{0xaa}, 32), FileEncSHA256: bytes.Repeat([]byte{0x22}, 32), FileLength: proto.Uint64(64)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.store.ApplySourceEvent(context.Background(), archive.SourceEvent{Key: archive.SourceKey{Account: "account", Chat: "chat", Sender: "sender", Message: "voice"}, Kind: archive.EventOriginal, OriginalSentAt: time.Now().Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1, BodyKind: archive.MessageVoice, Media: []archive.MediaReference{{Kind: "voice", MimeType: "audio/ogg", DeclaredBytes: 64, ProtocolReference: reference, Availability: "not_retained"}}}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	work, err := a.store.ClaimOriginalDownload(context.Background(), archive.OriginalDownloadClaim{Account: "account", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.RetryOriginalDownload(context.Background(), archive.OriginalDownloadRetry{Lease: work.Lease, Code: "media_unavailable", Terminal: true}); err != nil {
		t.Fatal(err)
	}
	result, err := port.RetryVoiceDownloads(context.Background())
	if err != nil || result.Queued != 1 || runtime.wakes != 1 {
		t.Fatal("application did not queue and wake one bound-account voice")
	}
	result, err = port.RetryVoiceDownloads(context.Background())
	if err != nil || result.Queued != 0 || runtime.wakes != 1 {
		t.Fatal("no-op application retry woke downloader again")
	}
	a.closed = true
	if _, err = port.RetryVoiceDownloads(context.Background()); !errors.Is(err, localui.ErrVoiceRetryUnavailable) || runtime.wakes != 1 {
		t.Fatal("closed application accepted retry")
	}
	a.closed = false
}

type ownerRedeliveryConnector struct {
	*downloadWakeConnector
	requested int
	scope     archive.ReadScope
	entered   chan struct{}
	release   chan struct{}
}

func (r *ownerRedeliveryConnector) Status() ingestion.Status {
	return ingestion.Status{Connection: "connected", HasSession: true}
}

func (r *ownerRedeliveryConnector) RequestVoiceRedelivery(ctx context.Context, scope archive.ReadScope) (int, error) {
	r.scope = scope
	if r.entered != nil {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return r.requested, nil
}

func TestVoiceRetryUsesOptionalRuntimeRedeliveryWithBoundOwnerScope(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	runtime := &ownerRedeliveryConnector{downloadWakeConnector: &downloadWakeConnector{}, requested: 2}
	a.runtime = runtime
	defer func() { a.runtime = nil }()
	policy, err := a.store.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.store.ApplySourceEvent(context.Background(), archive.SourceEvent{Key: archive.SourceKey{Account: "account", Chat: "chat", Sender: "sender", Message: "owner-source"}, Kind: archive.EventOriginal, OriginalSentAt: time.Now().Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1, BodyKind: archive.MessageText, Body: "fixture"}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.RetryVoiceDownloads(context.Background())
	if err != nil || result.Queued != 0 || result.Requested != 2 {
		t.Fatal("acknowledged primary phone requests were conflated with downloaded voices")
	}
	if runtime.scope.Account != "account" || runtime.scope.Principal != "owner" || runtime.scope.PolicyGeneration != policy.Generation || !runtime.scope.AllChats {
		t.Fatal("voice recovery escaped server-owned current owner scope")
	}
	if runtime.wakes != 0 {
		t.Fatal("phone acknowledgement invented durable download work")
	}
}

func TestVoiceRetryKeepsStatusReadableWhilePrimaryPhoneRequestWaits(t *testing.T) {
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	runtime := &ownerRedeliveryConnector{downloadWakeConnector: &downloadWakeConnector{}, requested: 1, entered: make(chan struct{}), release: make(chan struct{})}
	a.runtime = runtime
	defer func() { a.runtime = nil }()
	policy, err := a.store.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.store.ApplySourceEvent(context.Background(), archive.SourceEvent{Key: archive.SourceKey{Account: "account", Chat: "chat", Sender: "sender", Message: "readable-during-phone-request"}, Kind: archive.EventOriginal, OriginalSentAt: time.Now().Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1, BodyKind: archive.MessageText, Body: "fixture"}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	retryDone := make(chan error, 1)
	go func() { _, err := a.RetryVoiceDownloads(context.Background()); retryDone <- err }()
	select {
	case <-runtime.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("owner retry never reached bounded phone operation")
	}
	readDone := make(chan error, 1)
	go func() { _, err := a.Status(context.Background()); readDone <- err }()
	select {
	case err := <-readDone:
		close(runtime.release)
		if retryErr := <-retryDone; retryErr != nil {
			t.Fatal(retryErr)
		}
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(runtime.release)
		<-retryDone
		<-readDone
		t.Fatal("primary phone I/O blocked an ordinary archive status read")
	}
}

func TestOwnerVoiceChatRetryPreservesPolicyAndRejectsUnknown(t *testing.T) {
	ctx := context.Background()
	a, err := New(ctx, Config{DataDir: privateTestRoot(t), Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)
	runtime := &ownerRedeliveryConnector{downloadWakeConnector: &downloadWakeConnector{}, requested: 1}
	a.runtime = runtime
	defer func() { a.runtime = nil }()
	start := time.Now().Add(-24 * time.Hour).Truncate(time.Millisecond)
	policy, err := a.store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateSince, ImportStart: start, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.store.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: "account", Chat: "chat", Sender: "sender", Message: "scoped-voice-parent"}, Kind: archive.EventOriginal, OriginalSentAt: time.Now().Add(-time.Minute), OriginalDateKnown: true, SourceRevision: 1, BodyKind: archive.MessageText, Body: "synthetic"}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := a.scope(ctx)
	page, err := a.store.ListChats(ctx, scope, archive.ChatListRequest{})
	if err != nil || len(page.Chats) != 1 {
		t.Fatal("chat fixture unavailable", err)
	}
	if _, err = a.RetryVoiceDownloadsForChat(ctx, page.Chats[0].ID); err != nil {
		t.Fatal(err)
	}
	if runtime.scope.AllChats || len(runtime.scope.Chats) != 1 || runtime.scope.Chats[0] != page.Chats[0].ID || runtime.scope.PolicyGeneration != policy.Generation {
		t.Fatal("retry did not narrow owner scope")
	}
	before := runtime.scope
	if _, err = a.RetryVoiceDownloadsForChat(ctx, "unknown-chat"); err == nil {
		t.Fatal("unknown chat accepted")
	}
	if runtime.scope.Chats[0] != before.Chats[0] {
		t.Fatal("unknown chat dispatched requests")
	}
	current, err := a.store.Policy(ctx)
	if err != nil || current.Generation != policy.Generation || current.ChatMode != policy.ChatMode || !current.ImportStart.Equal(policy.ImportStart) {
		t.Fatal("chat retry changed archive policy")
	}
}

// A wake makes the synthetic connector busy immediately, reproducing the
// admission boundary without starting a downloader or contacting a phone.
type orderedVoiceRetryConnector struct {
	*downloadWakeConnector
	busy       bool
	calls      []string
	requestErr error
}

func (r *orderedVoiceRetryConnector) WakeOriginalDownloads() {
	r.calls = append(r.calls, "wake")
	r.busy = true
	r.wakes++
}

func (r *orderedVoiceRetryConnector) RequestOwnerVoiceRedownloads(context.Context, archive.ReadScope) (int, error) {
	r.calls = append(r.calls, "request")
	if r.busy {
		return 0, ingestion.ErrMediaCanceled
	}
	if r.requestErr != nil {
		return 0, r.requestErr
	}
	return 1, nil
}

func voiceRetryOrderApplication(t *testing.T) (*Application, archive.ArchivePolicy) {
	t.Helper()
	a, err := New(context.Background(), Config{DataDir: privateTestRoot(t), Demo: true, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.runtime = nil; a.Close(context.Background()) })
	policy, err := a.store.SetPolicy(context.Background(), archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return a, policy
}

func voiceRetryOrderSource(t *testing.T, a *Application, policy archive.ArchivePolicy, message string) archive.OriginalDownloadWork {
	t.Helper()
	ctx := context.Background()
	reference, err := proto.Marshal(&waE2E.AudioMessage{DirectPath: proto.String("/synthetic/voice-order"), MediaKey: bytes.Repeat([]byte{0x11}, 32), FileSHA256: bytes.Repeat([]byte{0xaa}, 32), FileEncSHA256: bytes.Repeat([]byte{0x22}, 32), FileLength: proto.Uint64(64)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.store.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: "account", Chat: "chat", Sender: "sender", Message: message}, Kind: archive.EventOriginal, OriginalSentAt: time.Now().Add(-time.Minute).Truncate(time.Millisecond), OriginalDateKnown: true, SourceRevision: 1, BodyKind: archive.MessageVoice, Media: []archive.MediaReference{{Kind: "voice", MimeType: "audio/ogg", DeclaredBytes: 64, ProtocolReference: reference, Availability: "not_retained"}}}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	work, err := a.store.ClaimOriginalDownload(ctx, archive.OriginalDownloadClaim{Account: "account", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return work
}

func TestVoiceRetryRequestsBeforeWakingQueuedDownloads(t *testing.T) {
	for _, failRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "request_failure"}[failRequest], func(t *testing.T) {
			a, policy := voiceRetryOrderApplication(t)
			work := voiceRetryOrderSource(t, a, policy, "failed-original")
			if err := a.store.RetryOriginalDownload(context.Background(), archive.OriginalDownloadRetry{Lease: work.Lease, Code: "media_unavailable", Terminal: true}); err != nil {
				t.Fatal(err)
			}
			runtime := &orderedVoiceRetryConnector{downloadWakeConnector: &downloadWakeConnector{}}
			if failRequest {
				runtime.requestErr = ingestion.ErrMediaUnavailable
			}
			a.runtime = runtime
			result, err := a.RetryVoiceDownloads(context.Background())
			if err != nil || result.Queued != 1 || runtime.wakes != 1 {
				t.Fatalf("queued work was not preserved and woken: %+v, %v, wakes=%d", result, err, runtime.wakes)
			}
			if strings.Join(runtime.calls, ",") != "request,wake" {
				t.Fatalf("queued download wake preceded owner phone request: %v", runtime.calls)
			}
			if failRequest {
				if result.Requested != 0 || result.Recovery.Code != "voice_retry_unavailable" {
					t.Fatalf("phone failure conflated with queued work: %+v", result)
				}
			} else if result.Requested != 1 || result.Recovery.Code != "" {
				t.Fatalf("wake made the owner request unavailable: %+v", result)
			}
		})
	}
}

func TestVoiceRetryLatestRequestFailureOverridesExpiredRecoveryCode(t *testing.T) {
	ctx := context.Background()
	a, policy := voiceRetryOrderApplication(t)
	work := voiceRetryOrderSource(t, a, policy, "previously-completed")
	reservation, err := a.store.AdmitMedia(ctx, archive.MediaAdmissionRequest{Source: work.Source, MediaID: work.Lease.MediaID, Budget: archive.BudgetOriginals, MaximumBytes: 128, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := a.store.PublishOriginal(ctx, archive.OriginalPublication{Reservation: reservation, SHA256: strings.Repeat("a", 64), Bytes: 64, ActualFormat: "ogg"}); err != nil || !ok {
		t.Fatalf("synthetic original publication: %v %v", ok, err)
	}
	policy, err = a.store.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := a.scope(ctx)
	if err != nil {
		t.Fatal(err)
	}
	intents, err := a.store.PrepareOwnerVoiceRedownloads(ctx, scope, 1)
	if err != nil || len(intents) != 1 {
		t.Fatalf("synthetic recovery intent: %d %v", len(intents), err)
	}
	intent := intents[0]
	if err := a.store.RegisterOwnerVoiceRequest(ctx, archive.OwnerVoiceRequestRegistration{IntentID: intent.ID, RequestID: "synthetic-expired-request", Source: intent.Source}); err != nil {
		t.Fatal(err)
	}
	// Advance only this synthetic request's persisted deadline; no sleeping and
	// no changes to the source, policy, or historical completion evidence.
	address := url.URL{Scheme: "file", Path: filepath.Join(a.paths.ArchiveDir, "archive.sqlite")}
	address.RawQuery = "mode=rw"
	db, err := sql.Open("sqlite3", address.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "UPDATE owner_voice_redownload_intents SET request_expires_at=1 WHERE id=?", intent.ID); err != nil {
		t.Fatal(err)
	}
	before, err := a.store.OwnerVoiceRecoveryStatus(ctx, scope)
	if err != nil || before.Failed != 1 || before.Code != "media_deadline_exceeded" {
		t.Fatalf("expired recovery fixture missing: %+v %v", before, err)
	}
	runtime := &orderedVoiceRetryConnector{downloadWakeConnector: &downloadWakeConnector{}, requestErr: ingestion.ErrMediaCanceled}
	a.runtime = runtime
	result, err := a.RetryVoiceDownloads(ctx)
	if err != nil || result.Requested != 0 || result.Queued != 0 || result.Recovery.Failed != before.Failed {
		t.Fatalf("latest request changed historical counts: %+v %v", result, err)
	}
	if result.Recovery.Code != "voice_retry_unavailable" {
		t.Fatalf("expired recovery code masked latest phone request failure: %+v", result)
	}
	after, err := a.store.OwnerVoiceRecoveryStatus(ctx, scope)
	if err != nil || after != before {
		t.Fatalf("response feedback changed durable recovery audit: before=%+v after=%+v err=%v", before, after, err)
	}
}
