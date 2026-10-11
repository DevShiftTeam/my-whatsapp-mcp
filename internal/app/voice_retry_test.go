//go:build sqlite_fts5

package app

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
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
