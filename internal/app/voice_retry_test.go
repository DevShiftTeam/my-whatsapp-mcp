//go:build sqlite_fts5

package app

import (
	"bytes"
	"context"
	"errors"
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
