package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/Bodokh/wa-indexer/internal/archive"
	"github.com/Bodokh/wa-indexer/internal/diskspace"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

type pendingMedia struct {
	key        archive.SourceKey
	version    archive.SourceVersion
	mediaID    archive.MediaID
	kind       archive.MessageKind
	attachment whatsmeow.DownloadableMessage
	hash       []byte
}

func (r *Runtime) projectViewOnce(e *archive.SourceEvent, m *waE2E.Message) {
	e.Body = ""
	e.Caption = ""
	e.BodyKind = archive.MessageUnsupported
	mime := ""
	if image := m.GetImageMessage(); image != nil {
		e.BodyKind = archive.MessageImage
		mime = image.GetMimetype()
	} else if audio := m.GetAudioMessage(); audio != nil {
		e.BodyKind = archive.MessageVoice
		mime = audio.GetMimetype()
	}
	if len(mime) > 256 {
		mime = ""
	}
	e.Media = []archive.MediaReference{{Kind: string(e.BodyKind), MimeType: mime, ViewOnce: true, Availability: "view_once"}}
}
func messageContext(m *waE2E.Message) *waE2E.ContextInfo {
	if m == nil {
		return nil
	}
	if v := m.GetExtendedTextMessage(); v != nil {
		return v.GetContextInfo()
	}
	if v := m.GetImageMessage(); v != nil {
		return v.GetContextInfo()
	}
	if v := m.GetAudioMessage(); v != nil {
		return v.GetContextInfo()
	}
	if v := m.GetVideoMessage(); v != nil {
		return v.GetContextInfo()
	}
	if v := m.GetDocumentMessage(); v != nil {
		return v.GetContextInfo()
	}
	if v := m.GetStickerMessage(); v != nil {
		return v.GetContextInfo()
	}
	return nil
}
func (r *Runtime) projectBody(ctx context.Context, e *archive.SourceEvent, m *waE2E.Message) (*pendingMedia, error) {
	if m == nil {
		return nil, nil
	}
	var pending *pendingMedia
	e.BodyKind = archive.MessageUnsupported
	if m.Conversation != nil {
		e.BodyKind = archive.MessageText
		e.Body = m.GetConversation()
	} else if text := m.GetExtendedTextMessage(); text != nil {
		e.BodyKind = archive.MessageText
		e.Body = text.GetText()
	}
	if image := m.GetImageMessage(); image != nil {
		e.BodyKind = archive.MessageImage
		e.Caption = image.GetCaption()
		descriptor := &waE2E.ImageMessage{DirectPath: proto.String(image.GetDirectPath()), MediaKey: append([]byte(nil), image.GetMediaKey()...), FileSHA256: append([]byte(nil), image.GetFileSHA256()...), FileEncSHA256: append([]byte(nil), image.GetFileEncSHA256()...), FileLength: proto.Uint64(image.GetFileLength())}
		pending = r.mediaReference(e, descriptor, archive.MessageImage, image.GetMimetype(), image.GetViewOnce())
	} else if audio := m.GetAudioMessage(); audio != nil {
		e.BodyKind = archive.MessageVoice
		descriptor := &waE2E.AudioMessage{DirectPath: proto.String(audio.GetDirectPath()), MediaKey: append([]byte(nil), audio.GetMediaKey()...), FileSHA256: append([]byte(nil), audio.GetFileSHA256()...), FileEncSHA256: append([]byte(nil), audio.GetFileEncSHA256()...), FileLength: proto.Uint64(audio.GetFileLength())}
		pending = r.mediaReference(e, descriptor, archive.MessageVoice, audio.GetMimetype(), audio.GetViewOnce())
	}
	if c := messageContext(m); c != nil {
		if id := c.GetStanzaID(); id != "" && len(id) <= 256 {
			participant := c.GetParticipant()
			if participant == "" {
				participant = e.Key.Chat
			}
			sender, err := r.canonical(ctx, participant)
			if err == nil {
				e.ReplyTo = &archive.SourceKey{Account: e.Key.Account, Chat: e.Key.Chat, Sender: sender, Message: id, FromMe: sender == e.Key.Account}
			}
		}
	}
	return pending, nil
}
func (r *Runtime) mediaReference(e *archive.SourceEvent, descriptor whatsmeow.DownloadableMessage, kind archive.MessageKind, mime string, viewOnce bool) *pendingMedia {
	if len(mime) > 256 {
		mime = ""
	}
	length := descriptor.(interface{ GetFileLength() uint64 }).GetFileLength()
	availability := "not_retained"
	valid := validMediaDescriptor(descriptor, r.config.MediaMax)
	if !valid {
		availability = "unavailable"
	}
	if viewOnce {
		availability = "view_once"
		valid = false
		e.Body = ""
		e.Caption = ""
	}
	ref := archive.MediaReference{Kind: string(kind), MimeType: mime, DeclaredBytes: int64(min(length, uint64(1<<63-1))), ViewOnce: viewOnce, Availability: availability}
	if valid {
		data, err := proto.Marshal(descriptor.(proto.Message))
		if err == nil && len(data) <= 8192 {
			ref.ProtocolReference = data
		} else {
			valid = false
			ref.Availability = "unavailable"
		}
	}
	e.Media = []archive.MediaReference{ref}
	if !valid {
		return nil
	}
	return &pendingMedia{kind: kind, attachment: descriptor, hash: append([]byte(nil), descriptor.GetFileSHA256()...)}
}
func validMediaDescriptor(descriptor whatsmeow.DownloadableMessage, maximum int64) bool {
	length := descriptor.(interface{ GetFileLength() uint64 }).GetFileLength()
	return maximum > 0 && strings.HasPrefix(descriptor.GetDirectPath(), "/") && len(descriptor.GetDirectPath()) <= 4096 && len(descriptor.GetMediaKey()) == 32 && len(descriptor.GetFileSHA256()) == 32 && len(descriptor.GetFileEncSHA256()) == 32 && length <= uint64(maximum)
}
func nilMediaPort(port any) bool {
	if port == nil {
		return true
	}
	v := reflect.ValueOf(port)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	}
	return false
}

func mediaFailure(err error, fallback MediaError) MediaError {
	var mediaErr MediaError
	if errors.As(err, &mediaErr) && mediaErr.valid() {
		return mediaErr
	}
	switch {
	case errors.Is(err, context.Canceled):
		return ErrMediaCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return ErrMediaDeadline
	case errors.Is(err, archive.ErrQuotaExceeded), errors.Is(err, diskspace.ErrInsufficientSpace):
		return ErrMediaWaitingForDiskBudget
	case errors.Is(err, archive.ErrSourceUnavailable), errors.Is(err, archive.ErrPolicyChanged):
		return ErrMediaSourceChanged
	case errors.Is(err, errLimitExceeded), errors.Is(err, archive.ErrLimitExceeded):
		return ErrMediaLimit
	}
	if fallback.valid() {
		return fallback
	}
	return ErrMediaUnavailable
}

// downloadMedia never creates a production media file. The admission owns its
// reservation, capped inode, durable completion, and cleanup acknowledgement.
func (r *Runtime) downloadMedia(ctx context.Context, p *pendingMedia) (resultErr error) {
	return r.transferMedia(ctx, p, nil)
}

type mediaTransferCleanup struct{ admission DownloadAdmission }

// transferMedia is synchronous and context-only. Production cancellation belongs
// to the optional loop; it cannot replace the concurrent history cancellation.
func (r *Runtime) transferMedia(ctx context.Context, p *pendingMedia, cleanup *mediaTransferCleanup) (resultErr error) {
	if r.callbacks.MediaAdmitter == nil {
		if r.legacyMediaReceiver && r.callbacks.MediaReceiver != nil {
			return r.downloadLegacyMedia(ctx, p)
		}
		return nil
	}
	defer func() {
		var outcome MediaError
		r.mu.Lock()
		defer r.mu.Unlock()
		if errors.As(resultErr, &outcome) && outcome.valid() {
			r.status.MediaCode = outcome.Error()
		} else if resultErr == nil {
			r.status.MediaCode = ""
		}
	}()
	if p == nil || p.mediaID == "" || len(p.hash) != sha256.Size {
		return ErrMediaInvalidAdmission
	}
	eligible, err := r.store.EligibleMedia(ctx, p.key, p.version)
	if err != nil {
		return mediaFailure(err, ErrMediaUnavailable)
	}
	if !eligible {
		return ErrMediaSourceChanged
	}
	declared := p.attachment.(interface{ GetFileLength() uint64 }).GetFileLength()
	if declared > uint64(r.config.MediaMax) {
		return ErrMediaLimit
	}
	plainMaximum := int64(declared)
	if plainMaximum == 0 {
		plainMaximum = r.config.MediaMax
	}
	wireMaximum, err := mediaCiphertextMaximum(plainMaximum)
	if err != nil {
		return mediaFailure(err, ErrMediaInvalidAdmission)
	}
	var digest [32]byte
	copy(digest[:], p.hash)
	downloadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	admission, admitErr := r.callbacks.MediaAdmitter(downloadCtx, MediaDownloadRequest{Key: p.key, Version: p.version, MediaID: p.mediaID, Kind: p.kind, DeclaredBytes: int64(declared), CiphertextMaximum: wireMaximum, ProtocolSHA256: digest})
	if !nilMediaPort(admission) {
		// DownloadAttachment is synchronous. This defer cannot release accounting
		// while its writer is still running, even when cancellation arrives.
		defer func() {
			if admission.Close() != nil {
				if cleanup != nil {
					cleanup.admission = admission
				}
				// Stopped admission handles and their uncertain charged cleanup
				// remain owned by the media store for reconciliation/retry.
				resultErr = ErrMediaCleanupIncomplete
			}
		}()
	}
	if admitErr != nil {
		// Optional retention cannot erase a committed source or manufacture an
		// observation gap. The durable association remains available for retry.
		return mediaFailure(admitErr, ErrMediaUnavailable)
	}
	if nilMediaPort(admission) {
		return ErrMediaInvalidAdmission
	}
	file := admission.File()
	if nilMediaPort(file) {
		return ErrMediaInvalidAdmission
	}
	if _, raw := file.(*os.File); raw {
		return ErrMediaInvalidAdmission // admission must not expose raw FD authority
	}
	capped, err := newCappedFile(downloadCtx, file, wireMaximum)
	if err != nil {
		return mediaFailure(err, ErrMediaInvalidAdmission)
	}
	if err = r.downloadAttachment(downloadCtx, p.attachment, capped); err != nil || downloadCtx.Err() != nil {
		if downloadCtx.Err() != nil {
			return mediaFailure(downloadCtx.Err(), ErrMediaUnavailable)
		}
		return r.mediaUnavailable(ctx, p, mediaFailure(err, ErrMediaUnavailable))
	}
	stat, err := capped.Stat()
	if err != nil || nilMediaPort(stat) {
		return r.mediaUnavailable(ctx, p, mediaFailure(err, ErrMediaUnavailable))
	}
	if stat.Size() <= 0 || stat.Size() > plainMaximum || declared != 0 && stat.Size() != int64(declared) {
		return r.mediaUnavailable(ctx, p, ErrMediaAuthentication)
	}
	if _, err = capped.Seek(0, io.SeekStart); err != nil {
		return r.mediaUnavailable(ctx, p, mediaFailure(err, ErrMediaUnavailable))
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(capped, plainMaximum+1))
	if err != nil {
		return r.mediaUnavailable(ctx, p, mediaFailure(err, ErrMediaUnavailable))
	}
	if count != stat.Size() || count > plainMaximum || !bytes.Equal(hash.Sum(nil), p.hash) {
		return r.mediaUnavailable(ctx, p, ErrMediaAuthentication)
	}
	eligible, err = r.store.EligibleMedia(downloadCtx, p.key, p.version)
	if err != nil {
		return mediaFailure(err, ErrMediaUnavailable)
	}
	if !eligible {
		return ErrMediaSourceChanged
	}
	if _, err = capped.Seek(0, io.SeekStart); err != nil {
		return r.mediaUnavailable(ctx, p, mediaFailure(err, ErrMediaUnavailable))
	}
	if err = downloadCtx.Err(); err != nil {
		return mediaFailure(err, ErrMediaUnavailable)
	}
	// Completion owns format verification, filesystem publication and the final
	// archive source/association transaction. Never overwrite its retained state
	// with a legacy receipt status, including an uncertain completion response.
	if err = admission.Complete(downloadCtx, VerifiedMedia{Key: p.key, Version: p.version, Kind: p.kind, SHA256: digest, Bytes: count, Reader: io.LimitReader(capped, count)}); err != nil {
		return mediaFailure(err, ErrMediaUnavailable)
	}
	return nil
}

func (r *Runtime) mediaUnavailable(ctx context.Context, p *pendingMedia, cause MediaError) error {
	err := r.store.UpdateMedia(ctx, p.key, p.version, "unavailable")
	if err != nil {
		return mediaFailure(err, ErrMediaUnavailable)
	}
	return cause
}

// The legacy receiver exists only behind an unexported isolated-test hook.
// Public New never enables it and production retention requires MediaAdmitter.
func (r *Runtime) downloadLegacyMedia(ctx context.Context, p *pendingMedia) error {
	eligible, err := r.store.EligibleMedia(ctx, p.key, p.version)
	if err != nil || !eligible {
		return err
	}
	if r.callbacks.MediaReceiver == nil {
		return nil
	}
	if err = privateDirectory(r.config.TemporaryDir, false); err != nil {
		return err
	}
	raw, err := os.CreateTemp(r.config.TemporaryDir, ".media-*")
	if err != nil {
		return archive.ErrSourceUnavailable
	}
	defer func() { _ = raw.Close(); _ = os.Remove(raw.Name()) }()
	if err = raw.Chmod(0600); err != nil {
		return archive.ErrUnsafeDataPath
	}
	downloadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r.activeMu.Lock()
	r.activeCancel = cancel
	r.activeMu.Unlock()
	defer func() { r.activeMu.Lock(); r.activeCancel = nil; r.activeMu.Unlock() }()
	wireMaximum, err := mediaCiphertextMaximum(r.config.MediaMax)
	if err != nil {
		return err
	}
	capped, err := newCappedFile(downloadCtx, raw, wireMaximum)
	if err != nil {
		return err
	}
	err = r.downloadAttachment(downloadCtx, p.attachment, capped)
	contextErr := downloadCtx.Err()
	if err == nil {
		err = contextErr
	}
	if err != nil { // Transport failure cannot erase the independently committed message.
		return r.store.UpdateMedia(ctx, p.key, p.version, "unavailable")
	}
	stat, err := raw.Stat()
	if err != nil {
		return archive.ErrSourceUnavailable
	}
	if stat.Size() > r.config.MediaMax {
		return r.store.UpdateMedia(ctx, p.key, p.version, "unavailable")
	}
	if _, err = raw.Seek(0, io.SeekStart); err != nil {
		return archive.ErrSourceUnavailable
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(raw, r.config.MediaMax+1))
	if err != nil || count > r.config.MediaMax || !bytes.Equal(hash.Sum(nil), p.hash) {
		return r.store.UpdateMedia(ctx, p.key, p.version, "unavailable")
	}
	eligible, err = r.store.EligibleMedia(ctx, p.key, p.version)
	if err != nil || !eligible {
		return err
	}
	if _, err = raw.Seek(0, io.SeekStart); err != nil {
		return archive.ErrSourceUnavailable
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	// The sink executes before cleanup, receives no path/key material, and must
	// independently commit its reservation plus association at this source version.
	err = r.callbacks.MediaReceiver(downloadCtx, VerifiedMedia{Key: p.key, Version: p.version, Kind: p.kind, SHA256: digest, Bytes: count, Reader: io.LimitReader(raw, count)})
	if err != nil {
		return r.store.UpdateMedia(ctx, p.key, p.version, "unavailable")
	}
	// Retained availability belongs to the durable media sink's association
	// transaction. This first-slice bridge only reports verified transport receipt.
	return r.store.UpdateMedia(ctx, p.key, p.version, "downloaded_not_retained")
}
