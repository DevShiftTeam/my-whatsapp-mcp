package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func validDocumentEvent() *events.Message {
	e := liveMessage("document", "")
	hash := sha256.Sum256([]byte("source"))
	e.Message = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		FileName: proto.String("סיכום meeting.pdf"), Mimetype: proto.String("Application/PDF; charset=utf-8"),
		Caption: proto.String("The agenda for our meeting"), Title: proto.String("PRIVATE_TITLE"),
		DirectPath: proto.String("/authenticated/document"), URL: proto.String("https://untrusted.invalid/never-download"),
		MediaKey: make([]byte, 32), FileSHA256: hash[:], FileEncSHA256: make([]byte, 32), FileLength: proto.Uint64(6),
		ThumbnailDirectPath: proto.String("/PRIVATE_THUMBNAIL"), JPEGThumbnail: []byte("PRIVATE_JPEG"),
		AccessibilityLabel: proto.String("PRIVATE_ACCESSIBILITY"),
		ContextInfo:        &waE2E.ContextInfo{QuotedMessage: &waE2E.Message{Conversation: proto.String("PRIVATE_QUOTE")}},
	}}
	e.Message.GetDocumentMessage().ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	return e
}

func projectedFilename(ref archive.MediaReference) string {
	f := reflect.ValueOf(ref).FieldByName("Filename")
	if !f.IsValid() || f.Kind() != reflect.String {
		return ""
	}
	return f.String()
}

func TestDocumentProjectionPreservesDisplayMetadataAndReducedDescriptor(t *testing.T) {
	for _, mime := range []string{"Application/PDF; charset=utf-8", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "", "not a MIME type"} {
		t.Run(mime, func(t *testing.T) {
			s := fixtureStore()
			r := testRuntime(s)
			e := validDocumentEvent()
			e.Message.DocumentMessage.Mimetype = proto.String(mime)
			if err := r.message(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			got := s.events[0]
			if got.BodyKind != archive.MessageKind("document") || got.Body != "" || got.Caption != e.Message.DocumentMessage.GetCaption() || len(got.Media) != 1 {
				t.Fatal("document source lost kind or caption metadata")
			}
			ref := got.Media[0]
			if projectedFilename(ref) != e.Message.DocumentMessage.GetFileName() || ref.Kind != "document" || ref.DeclaredBytes != 6 || ref.Availability != "not_retained" {
				t.Fatal("document reference lost filename, size, or ordinary availability")
			}
			wantMIME := "application/octet-stream"
			if strings.HasPrefix(mime, "Application/PDF") {
				wantMIME = "application/pdf"
			} else if strings.HasPrefix(mime, "application/vnd.") {
				wantMIME = mime
			}
			if ref.MimeType != wantMIME {
				t.Fatal("document MIME did not become its bounded canonical base")
			}
			var descriptor waE2E.DocumentMessage
			if err := proto.Unmarshal(ref.ProtocolReference, &descriptor); err != nil {
				t.Fatal(err)
			}
			original := e.Message.DocumentMessage
			want := &waE2E.DocumentMessage{DirectPath: proto.String(original.GetDirectPath()), MediaKey: append([]byte(nil), original.GetMediaKey()...), FileSHA256: append([]byte(nil), original.GetFileSHA256()...), FileEncSHA256: append([]byte(nil), original.GetFileEncSHA256()...), FileLength: proto.Uint64(original.GetFileLength())}
			if !proto.Equal(&descriptor, want) {
				t.Fatal("document descriptor admitted display, quote, URL, thumbnail, or unknown fields")
			}
			var source archive.SourceEvent
			pending, err := r.projectBody(context.Background(), &source, e.Message)
			if err != nil || pending == nil {
				t.Fatal("valid document did not retain an isolated download descriptor")
			}
			defer wipePending(pending)
			original.MediaKey[0] = 7
			original.FileSHA256[0] ^= 1
			if !bytes.Equal(pending.attachment.GetFileSHA256(), want.GetFileSHA256()) || !bytes.Equal(pending.attachment.GetMediaKey(), want.GetMediaKey()) {
				t.Fatal("pending document descriptor borrowed mutable SDK hash or key bytes")
			}
		})
	}
}

func TestDocumentProjectionRejectsCompetingPrimaryPayload(t *testing.T) {
	for _, payload := range []string{"conversation", "image", "audio", "protocol"} {
		t.Run(payload, func(t *testing.T) {
			r := testRuntime(fixtureStore())
			e := validDocumentEvent()
			switch payload {
			case "conversation":
				e.Message.Conversation = proto.String("ambiguous body")
			case "image":
				e.Message.ImageMessage = validImageEvent().Message.ImageMessage
			case "audio":
				e.Message.AudioMessage = &waE2E.AudioMessage{}
			case "protocol":
				e.Message.ProtocolMessage = &waE2E.ProtocolMessage{}
			}
			var source archive.SourceEvent
			if _, err := r.projectBody(context.Background(), &source, e.Message); !errors.Is(err, archive.ErrInvalidEvent) || len(source.Media) != 0 {
				t.Fatal("competing document payload acquired source media authority")
			}
		})
	}
}

func TestDocumentCaptionWrapperUsesAuthenticatedSDKProjection(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	e := validDocumentEvent()
	e.RawMessage = &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: e.Message}}
	e.UnwrapRaw()
	if err := r.message(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	got := s.events[0]
	if got.BodyKind != archive.MessageKind("document") || got.Caption != "The agenda for our meeting" || len(got.Media) != 1 || len(got.Media[0].ProtocolReference) == 0 {
		t.Fatal("SDK document-caption wrapper lost its authenticated source projection")
	}
}

func TestDocumentGroupRetryPermitsTransportMetadata(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	e := validDocumentEvent()
	e.Info.IsGroup = true
	e.Info.Chat = types.NewJID("300", types.GroupServer)
	e.Message.SenderKeyDistributionMessage = &waE2E.SenderKeyDistributionMessage{}
	e.Message.MessageContextInfo = &waE2E.MessageContextInfo{}
	if err := r.message(context.Background(), e); err != nil {
		t.Fatal("legitimate group retry transport metadata rejected its document", err)
	}
	got := s.events[0]
	if got.BodyKind != archive.MessageKind("document") || got.Key.Chat != e.Info.Chat.String() || len(got.Media) != 1 || len(got.Media[0].ProtocolReference) == 0 {
		t.Fatal("legitimate group document was not saved with its authentic source")
	}
}

func TestDocumentAmbiguityRejectedBeforeMutationPrivacyAndExcludedProjection(t *testing.T) {
	for _, mode := range []string{"revoke", "view_once", "excluded", "raw_wrapped", "source_web"} {
		t.Run(mode, func(t *testing.T) {
			s := fixtureStore()
			r := testRuntime(s)
			e := validDocumentEvent()
			e.Message.Conversation = proto.String("competing primary text")
			switch mode {
			case "revoke":
				e = mutation("target", waE2E.ProtocolMessage_REVOKE)
				e.Message.DocumentMessage = validDocumentEvent().Message.DocumentMessage
			case "view_once":
				e.IsViewOnce = true
			case "excluded":
				s.policy.ChatMode = archive.ChatsSelective
			case "raw_wrapped":
				e.RawMessage = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: e.Message}}
				e.Message = &waE2E.Message{Conversation: proto.String("ordinary unwrapped projection")}
			case "source_web":
				e.SourceWebMsg = &waWeb.WebMessageInfo{Message: e.Message}
				e.Message = &waE2E.Message{Conversation: proto.String("ordinary unwrapped projection")}
			}
			if err := r.message(context.Background(), e); !errors.Is(err, archive.ErrInvalidEvent) || len(s.events) != 0 || len(s.aliases) != 0 {
				t.Fatal("ambiguous document reached source or control mutation", err)
			}
		})
	}
}

func TestDocumentProjectionKeepsOverLimitReceiptAndDropsInvalidFilename(t *testing.T) {
	for _, scenario := range []string{"zero_length", "at_limit", "over_limit", "configured_limit", "invalid_utf8", "oversized_filename", "nul_filename"} {
		t.Run(scenario, func(t *testing.T) {
			r := testRuntime(fixtureStore())
			r.config.MediaMax = 25 << 20
			e := validDocumentEvent()
			wantValid, wantName := true, e.Message.DocumentMessage.GetFileName()
			switch scenario {
			case "zero_length":
				e.Message.DocumentMessage.FileLength = proto.Uint64(0)
			case "at_limit":
				e.Message.DocumentMessage.FileLength = proto.Uint64(8 << 20)
			case "over_limit":
				e.Message.DocumentMessage.FileLength = proto.Uint64((8 << 20) + 1)
				wantValid = false
			case "configured_limit":
				r.config.MediaMax = 5
				wantValid = false
			case "invalid_utf8":
				e.Message.DocumentMessage.FileName = proto.String(string([]byte{0xff}))
				wantName = ""
			case "oversized_filename":
				e.Message.DocumentMessage.FileName = proto.String(strings.Repeat("ש", 128))
				wantName = ""
			case "nul_filename":
				e.Message.DocumentMessage.FileName = proto.String("report\x00.pdf")
				wantName = ""
			}
			var source archive.SourceEvent
			pending, err := r.projectBody(context.Background(), &source, e.Message)
			if err != nil || source.BodyKind != archive.MessageKind("document") || len(source.Media) != 1 || projectedFilename(source.Media[0]) != wantName {
				t.Fatal("invalid optional filename or attachment bounds discarded source metadata")
			}
			defer wipePending(pending)
			if (pending != nil) != wantValid || (len(source.Media[0].ProtocolReference) != 0) != wantValid {
				t.Fatal("document download authority ignored the document or configured byte limit")
			}
		})
	}
}

func TestDocumentViewOnceAndExcludedPolicyDoNotPersistContents(t *testing.T) {
	for _, excluded := range []bool{false, true} {
		t.Run(map[bool]string{false: "view_once", true: "excluded"}[excluded], func(t *testing.T) {
			s := fixtureStore()
			r := testRuntime(s)
			e := validDocumentEvent()
			e.IsViewOnce = !excluded
			if excluded {
				s.policy.ChatMode = archive.ChatsSelective
				s.policy.IncludedSourceChats = []string{"900@s.whatsapp.net"}
			}
			if err := r.message(context.Background(), e); err != nil {
				t.Fatal(err)
			}
			got := s.events[0]
			if got.Body != "" || got.Caption != "" {
				t.Fatal("view-once or excluded document persisted contents")
			}
			if excluded {
				if len(got.Media) != 0 {
					t.Fatal("excluded document persisted attachment metadata")
				}
			} else if got.BodyKind != archive.MessageKind("document") || len(got.Media) != 1 || !got.Media[0].ViewOnce || got.Media[0].Availability != "view_once" || projectedFilename(got.Media[0]) != "" || len(got.Media[0].ProtocolReference) != 0 {
				t.Fatal("view-once document lost privacy or source kind")
			}
		})
	}
}

func TestDocumentOriginalDecoderUsesDocumentKeysAndReducedDescriptor(t *testing.T) {
	r := testRuntime(fixtureStore())
	work, input := syntheticOriginalWork(t, r.SourceAccount())
	work.Kind = archive.MessageKind("document")
	input.Work = work
	document := validDocumentEvent().Message.DocumentMessage
	var err error
	input.ProtocolReference, err = proto.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if !validOriginalWork(work, r.SourceAccount()) {
		t.Fatal("durable scheduler rejected ordinary document work")
	}
	pending, err := decodeOriginal(input, 25<<20)
	if err != nil || pending == nil {
		t.Fatal("durable document descriptor could not be decoded", err)
	}
	defer wipePending(pending)
	decoded, ok := pending.attachment.(*waE2E.DocumentMessage)
	if !ok || pending.kind != archive.MessageKind("document") || pending.mediaID != work.Lease.MediaID || pending.version != work.Source.Version {
		t.Fatal("document decode changed source, media lease, or WhatsApp key type")
	}
	want := &waE2E.DocumentMessage{DirectPath: proto.String(document.GetDirectPath()), MediaKey: append([]byte(nil), document.GetMediaKey()...), FileSHA256: append([]byte(nil), document.GetFileSHA256()...), FileEncSHA256: append([]byte(nil), document.GetFileEncSHA256()...), FileLength: proto.Uint64(document.GetFileLength())}
	if !proto.Equal(decoded, want) {
		t.Fatal("durable decoder retained untrusted document display or network fields")
	}
	for _, length := range []uint64{7, (8 << 20) + 1} {
		document.FileLength = proto.Uint64(length)
		input.Work.DeclaredBytes = int64(length)
		if length == 7 {
			input.Work.DeclaredBytes = 6
		}
		input.ProtocolReference, err = proto.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = decodeOriginal(input, 25<<20); !errors.Is(err, ErrMediaInvalidAdmission) {
			t.Fatal("document decoder accepted mismatched or over-limit declared bytes")
		}
	}
}

func TestDocumentUnknownLengthTransferReservesOnlyDocumentMaximum(t *testing.T) {
	r, s := admissionFixture(t)
	r.config.MediaMax = 25 << 20
	e := validDocumentEvent()
	e.Message.DocumentMessage.FileLength = proto.Uint64(0)
	completed, closed := false, false
	r.callbacks.MediaAdmitter = func(ctx context.Context, q MediaDownloadRequest) (DownloadAdmission, error) {
		maximum, err := mediaCiphertextMaximum(8 << 20)
		if err != nil || q.Kind != archive.MessageKind("document") || q.DeclaredBytes != 0 || q.CiphertextMaximum != maximum {
			t.Fatal("unknown-length document reserved the global media maximum")
		}
		file, err := newMemoryFile(ctx, 1024)
		if err != nil {
			t.Fatal(err)
		}
		return &fakeDownloadAdmission{file: file, complete: func(_ context.Context, v VerifiedMedia) error {
			if closed || v.Kind != archive.MessageKind("document") || v.Bytes != 6 || v.SHA256 != sha256.Sum256([]byte("source")) {
				t.Fatal("document completion lost kind, hash, or lifetime")
			}
			completed = true
			return nil
		}, close: func() error { closed = true; return file.Close() }}, nil
	}
	r.download = func(_ context.Context, attachment whatsmeow.DownloadableMessage, file whatsmeow.File) error {
		if _, ok := attachment.(*waE2E.DocumentMessage); !ok {
			t.Fatal("document download used another media key type")
		}
		_, err := file.Write([]byte("source"))
		return err
	}
	if err := commitAndTransferForTest(context.Background(), r, s, e); err != nil || !completed || !closed {
		t.Fatal("document could not complete verified transfer before close", err)
	}
}

type documentRedeliveryRuntimePort interface {
	RequestDocumentRedelivery(context.Context, archive.ReadScope, []archive.MessageID) (int, error)
}

func TestDocumentRuntimeHasBoundedOwnerRedelivery(t *testing.T) {
	r := testRuntime(fixtureStore())
	if _, ok := any(r).(documentRedeliveryRuntimePort); !ok {
		t.Fatal("runtime lacks targeted owner document redelivery")
	}
}

func TestStandaloneSenderKeyDistributionDoesNotConflictWithCompanionPayload(t *testing.T) {
	s, _ := durableFixture(t)
	r := durableRuntime(t, s)
	ctx := context.Background()
	if err := r.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	content := liveMessage("group-stanza-1", "groupstanzacompanionreceipt")
	content.Info.IsGroup = true
	content.Info.Chat = types.NewJID("300", types.GroupServer)
	content.Info.Sender = types.NewJID("200", types.DefaultUserServer)

	skdm := *content
	skdm.Message = &waE2E.Message{
		SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{
			GroupID:                             proto.String("300@g.us"),
			AxolotlSenderKeyDistributionMessage: []byte{1, 2, 3, 4},
		},
		MessageContextInfo: &waE2E.MessageContextInfo{},
	}
	skdm.RawMessage = skdm.Message

	if err := r.messageWithMediaOutcome(ctx, &skdm); err != nil {
		t.Fatalf("standalone sender key distribution failed: %v", err)
	}
	if err := r.messageWithMediaOutcome(ctx, content); err != nil {
		t.Fatalf("companion group message failed after sender key distribution: %v", err)
	}
	if err := r.messageWithMediaOutcome(ctx, &skdm); err != nil {
		t.Fatalf("trailing standalone sender key distribution failed: %v", err)
	}
	page, err := durableSearch(t, s, "groupstanzacompanionreceipt")
	if err != nil || len(page.Hits) != 1 {
		t.Fatalf("companion group message was not searchable: hits=%d err=%v", len(page.Hits), err)
	}
}
