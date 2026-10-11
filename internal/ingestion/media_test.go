package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func validImageEvent() *events.Message {
	e := liveMessage("media", "")
	hash := sha256.Sum256([]byte("source"))
	e.Message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("CAPTION_MUST_NOT_ENTER_REFERENCE"), DirectPath: proto.String("/authenticated/source"), URL: proto.String("https://arbitrary.invalid/never-download"), MediaKey: make([]byte, 32), FileSHA256: hash[:], FileEncSHA256: make([]byte, 32), FileLength: proto.Uint64(6), ContextInfo: &waE2E.ContextInfo{QuotedMessage: &waE2E.Message{Conversation: proto.String("QUOTE_MUST_NOT_ENTER_REFERENCE")}}}}
	return e
}
func TestMediaReferenceWhitelistExcludesCaptionQuoteAndURL(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	if err := r.message(context.Background(), validImageEvent()); err != nil {
		t.Fatal(err)
	}
	reference := s.events[0].Media[0].ProtocolReference
	for _, private := range []string{"CAPTION_MUST_NOT_ENTER_REFERENCE", "QUOTE_MUST_NOT_ENTER_REFERENCE", "arbitrary.invalid"} {
		if bytes.Contains(reference, []byte(private)) {
			t.Fatal("non-whitelisted field entered protocol reference")
		}
	}
	var descriptor waE2E.ImageMessage
	if err := proto.Unmarshal(reference, &descriptor); err != nil {
		t.Fatal(err)
	}
	if descriptor.GetDirectPath() != "/authenticated/source" || descriptor.Caption != nil || descriptor.ContextInfo != nil || descriptor.URL != nil {
		t.Fatal("protocol descriptor whitelist changed")
	}
}
func TestViewOnceReceiptHasKindAndNoPlaintextReferenceOrDownload(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	e := validImageEvent()
	e.IsViewOnce = true
	r.callbacks.MediaReceiver = func(context.Context, VerifiedMedia) error { t.Fatal("view-once handoff"); return nil }
	r.download = func(context.Context, whatsmeow.DownloadableMessage, whatsmeow.File) error {
		t.Fatal("view-once download")
		return nil
	}
	if err := r.message(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	got := s.events[0]
	if got.BodyKind != archive.MessageImage || got.Body != "" || got.Caption != "" || len(got.Media) != 1 || !got.Media[0].ViewOnce || got.Media[0].Availability != "view_once" || len(got.Media[0].ProtocolReference) != 0 {
		t.Fatalf("view-once receipt lost privacy/kind: %+v", got)
	}
}
func TestVerifiedMediaHandoffPrecedesCleanupAndDoesNotClaimRetention(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "receiver_failure"}[fail], func(t *testing.T) {
			s := fixtureStore()
			r := testRuntime(s)
			r.legacyMediaReceiver = true
			dir := canonicalSessionTestDir(t)
			var err error
			r.config.TemporaryDir = dir
			var borrowed io.Reader
			called := false
			r.callbacks.MediaReceiver = func(_ context.Context, m VerifiedMedia) error {
				called = true
				borrowed = m.Reader
				if m.Key.Message != "media" || m.Version.Revision != 1 || m.Bytes != 6 || m.Kind != archive.MessageImage {
					t.Fatal("handoff did not bind current source/version")
				}
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 1 {
					t.Fatal("temp removed before handoff")
				}
				var one [1]byte
				if _, err = m.Reader.Read(one[:]); err != nil || one[0] != 's' {
					t.Fatal("verified descriptor unavailable during handoff")
				}
				if fail {
					return errors.New("synthetic_receiver_failure")
				}
				return nil
			}
			r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, f whatsmeow.File) error {
				_, err := f.Write([]byte("source"))
				return err
			}
			if err = r.message(context.Background(), validImageEvent()); err != nil {
				t.Fatal(err)
			}
			if !called || len(s.events) != 1 {
				t.Fatal("media transport failure lost message receipt")
			}
			wantStatus := "downloaded_not_retained"
			if fail {
				wantStatus = "unavailable"
			}
			if len(s.mediaUpdates) != 1 || s.mediaUpdates[0] != wantStatus {
				t.Fatalf("transport receipt implied retention: %v", s.mediaUpdates)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatal("temporary media leaked after handoff")
			}
			if _, err = borrowed.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
				t.Fatal("borrowed descriptor remained live after callback")
			}
		})
	}
}

func TestMediaControlCancellationReachesVerifiedReceiver(t *testing.T) {
	s := fixtureStore()
	r := testRuntime(s)
	r.legacyMediaReceiver = true
	r.config.TemporaryDir = canonicalSessionTestDir(t)
	r.download = func(_ context.Context, _ whatsmeow.DownloadableMessage, f whatsmeow.File) error {
		_, err := f.Write([]byte("source"))
		return err
	}
	r.callbacks.MediaReceiver = func(ctx context.Context, _ VerifiedMedia) error {
		r.cancelActive()
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			t.Fatal("control cancellation did not reach active verified receiver")
			return nil
		}
	}
	if err := r.message(context.Background(), validImageEvent()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(r.config.TemporaryDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("cancelled media callback leaked temporary source")
	}
}
