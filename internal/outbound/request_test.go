package outbound

import (
	"encoding/base64"
	"errors"
	"os"
	"testing"
)

func TestRequestRejectsMalformedAndMismatchedMedia(t *testing.T) {
	good := Request{ChatID: "chat", RequestID: "send-1", Kind: "document", MIMEType: "application/pdf", Filename: "report.pdf", DataBase64: base64.StdEncoding.EncodeToString([]byte("%PDF-1.7\n"))}
	if _, err := Validate(good); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Request){
		func(r *Request) { r.DataBase64 = "not base64" },
		func(r *Request) { r.Kind = "image"; r.MIMEType = "image/png" },
		func(r *Request) { r.Filename = "../report.pdf" },
		func(r *Request) { r.RequestID = "" },
		func(r *Request) { r.Text = "unexpected" },
	} {
		r := good
		change(&r)
		if _, err := Validate(r); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid input accepted: %v", err)
		}
	}
}
func TestVoiceRequiresOpusContainerAndMetadata(t *testing.T) {
	r := Request{ChatID: "chat", RequestID: "voice-1", Kind: "voice", MIMEType: "audio/ogg; codecs=opus", Seconds: 2, DataBase64: base64.StdEncoding.EncodeToString([]byte("fake OpusHead"))}
	if _, err := Validate(r); !errors.Is(err, ErrInvalid) {
		t.Fatal("fake voice accepted")
	}
}

func TestVoiceAcceptsActualOpusFixtureAndRejectsCorruptedContainer(t *testing.T) {
	data, err := os.ReadFile("../feasibility/testdata/media/long-silence.ogg")
	if err != nil {
		t.Fatal(err)
	}
	r := Request{ChatID: "chat", RequestID: "voice", Kind: "voice", MIMEType: "audio/ogg; codecs=opus", Seconds: 60, DataBase64: base64.StdEncoding.EncodeToString(data)}
	if _, err = Validate(r); err != nil {
		t.Fatalf("real Opus rejected: %v", err)
	}
	data[len(data)-1] ^= 1
	r.DataBase64 = base64.StdEncoding.EncodeToString(data)
	if _, err = Validate(r); err == nil {
		t.Fatal("CRC-corrupted recording accepted")
	}
}
