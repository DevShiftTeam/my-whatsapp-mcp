package outbound

import (
	"context"
	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
	"time"
)

type Store interface {
	SendTarget(context.Context, archive.CredentialGrant, archive.ChatID) (string, error)
	ClaimSend(context.Context, archive.CredentialGrant, archive.ChatID, string, [32]byte, string) (archive.SendRecord, bool, error)
	FinishSend(context.Context, archive.CredentialGrant, string, string, time.Time) error
	SendStatus(context.Context, archive.CredentialGrant, string) (archive.SendRecord, error)
}
type Client interface {
	IsConnected() bool
	IsLoggedIn() bool
	GenerateMessageID() types.MessageID
	Upload(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	SendMessage(context.Context, types.JID, *waE2E.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
}

// Send runs under the owner lifecycle lock and calls SendMessage once per request.
// The connector may retry the same encrypted frame with the same protocol ID.
func Send(ctx context.Context, store Store, client Client, grant archive.CredentialGrant, request Request) (Receipt, error) {
	p, err := Validate(request)
	if err != nil {
		return Receipt{}, err
	}
	target, err := store.SendTarget(ctx, grant, p.Request.ChatID)
	if err != nil {
		return Receipt{}, err
	}
	jid, err := types.ParseJID(target)
	if err != nil || jid.ToNonAD().String() != target || jid.User == "" || jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer && jid.Server != types.GroupServer {
		return Receipt{}, ErrInvalid
	}
	if client == nil || !client.IsConnected() || !client.IsLoggedIn() {
		return Receipt{}, ErrUnavailable
	}
	record, fresh, err := store.ClaimSend(ctx, grant, p.Request.ChatID, p.Request.RequestID, p.Digest, string(client.GenerateMessageID()))
	if err != nil {
		return Receipt{}, err
	}
	if !fresh {
		return ReceiptFrom(record, true), nil
	}
	// Commit uncertainty before any provider work. A crash never permits a resend.
	fail := func() (Receipt, error) {
		persist(ctx, store, grant, record.RequestID, "failed", time.Time{})
		record.State = "failed"
		return ReceiptFrom(record, false), nil
	}
	var uploaded whatsmeow.UploadResponse
	if p.Request.Kind != "text" {
		uploaded, err = client.Upload(ctx, p.Data, mediaType(p.Request.Kind))
		if err != nil {
			return fail()
		}
	}
	// Uploading can block: authority and cancellation must be fresh before send.
	current, err := store.SendTarget(ctx, grant, p.Request.ChatID)
	if err != nil {
		return Receipt{}, err
	}
	if current != target {
		return Receipt{}, archive.ErrSendForbidden
	}
	if ctx.Err() != nil {
		return fail()
	}
	sent, err := client.SendMessage(ctx, jid, buildMessage(p, uploaded), whatsmeow.SendRequestExtra{ID: types.MessageID(record.ProtocolID), Timeout: 30 * time.Second})
	if err != nil {
		return ReceiptFrom(record, false), nil
	}
	if sent.ID != types.MessageID(record.ProtocolID) || sent.Timestamp.IsZero() {
		return ReceiptFrom(record, false), nil
	}
	if persist(ctx, store, grant, record.RequestID, "accepted", sent.Timestamp) != nil {
		return ReceiptFrom(record, false), nil
	}
	record.State = "accepted"
	record.AcceptedAt = sent.Timestamp
	return ReceiptFrom(record, false), nil
}
func persist(ctx context.Context, s Store, g archive.CredentialGrant, id, state string, at time.Time) error {
	// Server acknowledgement must survive request cancellation; this contains no provider work.
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return s.FinishSend(bounded, g, id, state, at)
}
func ReceiptFrom(r archive.SendRecord, duplicate bool) Receipt {
	out := Receipt{RequestID: r.RequestID, ChatID: r.ChatID, MessageID: r.ProtocolID, State: r.State, Duplicate: duplicate}
	if !r.AcceptedAt.IsZero() {
		out.AcceptedAt = r.AcceptedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}
func mediaType(kind string) whatsmeow.MediaType {
	switch kind {
	case "image":
		return whatsmeow.MediaImage
	case "video":
		return whatsmeow.MediaVideo
	case "audio", "voice":
		return whatsmeow.MediaAudio
	default:
		return whatsmeow.MediaDocument
	}
}
func buildMessage(p Payload, u whatsmeow.UploadResponse) *waE2E.Message {
	r := p.Request
	switch r.Kind {
	case "text":
		return &waE2E.Message{Conversation: proto.String(r.Text)}
	case "image":
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{URL: &u.URL, DirectPath: &u.DirectPath, MediaKey: u.MediaKey, FileSHA256: u.FileSHA256, FileEncSHA256: u.FileEncSHA256, FileLength: &u.FileLength, Mimetype: proto.String(r.MIMEType), Caption: proto.String(r.Caption), Width: &p.Width, Height: &p.Height}}
	case "video":
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{URL: &u.URL, DirectPath: &u.DirectPath, MediaKey: u.MediaKey, FileSHA256: u.FileSHA256, FileEncSHA256: u.FileEncSHA256, FileLength: &u.FileLength, Mimetype: proto.String(r.MIMEType), Caption: proto.String(r.Caption), Seconds: &r.Seconds}}
	case "audio", "voice":
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{URL: &u.URL, DirectPath: &u.DirectPath, MediaKey: u.MediaKey, FileSHA256: u.FileSHA256, FileEncSHA256: u.FileEncSHA256, FileLength: &u.FileLength, Mimetype: proto.String(r.MIMEType), PTT: proto.Bool(r.Kind == "voice"), Seconds: &r.Seconds}}
	default:
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{URL: &u.URL, DirectPath: &u.DirectPath, MediaKey: u.MediaKey, FileSHA256: u.FileSHA256, FileEncSHA256: u.FileEncSHA256, FileLength: &u.FileLength, Mimetype: proto.String(r.MIMEType), FileName: proto.String(r.Filename), Caption: proto.String(r.Caption)}}
	}
}
