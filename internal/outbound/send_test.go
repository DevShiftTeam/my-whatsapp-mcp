//go:build sqlite_fts5

package outbound

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"path/filepath"
	"testing"
	"time"
)

type provider struct {
	sent     int
	uploaded int
	fail     bool
	onUpload func()
	message  *waE2E.Message
}

func (p *provider) IsConnected() bool                  { return true }
func (p *provider) IsLoggedIn() bool                   { return true }
func (p *provider) GenerateMessageID() types.MessageID { return "TEST-ID" }
func (p *provider) Upload(_ context.Context, b []byte, _ whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	p.uploaded++
	if p.onUpload != nil {
		p.onUpload()
	}
	return whatsmeow.UploadResponse{URL: "https://example.invalid/upload", FileLength: uint64(len(b))}, nil
}
func (p *provider) SendMessage(_ context.Context, _ types.JID, m *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	p.sent++
	p.message = m
	if p.fail {
		return whatsmeow.SendResponse{}, errors.New("private backend failure")
	}
	return whatsmeow.SendResponse{ID: "TEST-ID", Timestamp: time.Now()}, nil
}
func sendFixture(t *testing.T) (*archive.Store, archive.CredentialGrant, archive.ChatID) {
	t.Helper()
	ctx := context.Background()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	s, err := archive.Open(ctx, dir, archive.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	policy, err := s.SetPolicy(ctx, archive.ArchivePolicy{DateMode: archive.DateAllAvailable, ChatMode: archive.ChatsAll}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplySourceEvent(ctx, archive.SourceEvent{Key: archive.SourceKey{Account: "account", Chat: "123@s.whatsapp.net", Sender: "123@s.whatsapp.net", Message: "source"}, Kind: archive.EventOriginal, OriginalSentAt: time.Now(), OriginalDateKnown: true, SourceRevision: 1, Body: "hello", BodyKind: archive.MessageText}, policy.Generation)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.ListChats(ctx, archive.ReadScope{Account: "account", Principal: "owner", PolicyGeneration: policy.Generation, AllChats: true}, archive.ChatListRequest{})
	if err != nil || len(page.Chats) != 1 {
		t.Fatal(err)
	}
	chat := page.Chats[0].ID
	c, err := s.CreateCredential(ctx, archive.CredentialCreate{Label: "Test", Principal: "client", Digest: sha256.Sum256([]byte("test")), Policy: archive.CredentialPolicy{Account: "account", Chats: []archive.ChatID{chat}}})
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.ResolveCredentialID(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetSendPermission(ctx, g, []archive.ChatID{chat}); err != nil {
		t.Fatal(err)
	}
	return s, g, chat
}
func TestSendReplayDoesNotDuplicateAndFailureIsUncertain(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "unknown"}[fail], func(t *testing.T) {
			s, g, chat := sendFixture(t)
			p := &provider{fail: fail}
			r := Request{ChatID: chat, RequestID: "one", Text: "שלום"}
			first, err := Send(context.Background(), s, p, g, r)
			if err != nil {
				t.Fatal(err)
			}
			again, err := Send(context.Background(), s, p, g, r)
			if err != nil || p.sent != 1 || !again.Duplicate || again.State != first.State {
				t.Fatalf("duplicate send: %#v %v %d", again, err, p.sent)
			}
			if fail && first.State != "outcome_unknown" {
				t.Fatal("provider error overclaimed failure")
			}
			r.Text = "changed"
			if _, err = Send(context.Background(), s, p, g, r); !errors.Is(err, archive.ErrSendConflict) || p.sent != 1 {
				t.Fatal("changed replay sent")
			}
		})
	}
}
func TestSendRechecksPermissionAfterUpload(t *testing.T) {
	s, g, chat := sendFixture(t)
	p := &provider{onUpload: func() {
		if err := s.SetSendPermission(context.Background(), g, nil); err != nil {
			t.Fatal(err)
		}
	}}
	r := Request{ChatID: chat, RequestID: "media", Kind: "document", MIMEType: "application/pdf", Filename: "report.pdf", DataBase64: base64.StdEncoding.EncodeToString([]byte("%PDF-1.7"))}
	if _, err := Send(context.Background(), s, p, g, r); !errors.Is(err, archive.ErrSendForbidden) || p.sent != 0 {
		t.Fatal("revoked media transmitted")
	}
}

func TestVoiceMessageCarriesOpusPTTAndUploadedBytes(t *testing.T) {
	s, g, chat := sendFixture(t)
	bytes := syntheticOpusFixture()
	p := &provider{}
	r := Request{ChatID: chat, RequestID: "voice", Kind: "voice", MIMEType: "audio/ogg; codecs=opus", Seconds: 60, DataBase64: base64.StdEncoding.EncodeToString(bytes)}
	receipt, err := Send(context.Background(), s, p, g, r)
	if err != nil || receipt.State != "accepted" || p.uploaded != 1 {
		t.Fatalf("voice send failed: %#v %v", receipt, err)
	}
	audio := p.message.GetAudioMessage()
	if !audio.GetPTT() || audio.GetMimetype() != "audio/ogg; codecs=opus" || audio.GetSeconds() != 60 || audio.GetFileLength() != uint64(len(bytes)) {
		t.Fatal("voice payload lost PTT, Opus metadata or bytes")
	}
}
