package outbound

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
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

func TestVoiceAcceptsSyntheticOpusAndRejectsCorruptedContainer(t *testing.T) {
	data := syntheticOpusFixture()
	r := Request{ChatID: "chat", RequestID: "voice", Kind: "voice", MIMEType: "audio/ogg; codecs=opus", Seconds: 60, DataBase64: base64.StdEncoding.EncodeToString(data)}
	if _, err := Validate(r); err != nil {
		t.Fatalf("synthetic Opus rejected: %v", err)
	}
	data[len(data)-1] ^= 1
	r.DataBase64 = base64.StdEncoding.EncodeToString(data)
	if _, err := Validate(r); err == nil {
		t.Fatal("CRC-corrupted recording accepted")
	}
}

// The fixture contains the standard Opus silence packet in a complete,
// CRC-correct container; it has no recording or external file dependency.
func syntheticOpusFixture() []byte {
	page := func(sequence uint32, flags byte, payload []byte) []byte {
		p := make([]byte, 28)
		copy(p, "OggS")
		p[5], p[26], p[27] = flags, 1, byte(len(payload))
		binary.LittleEndian.PutUint32(p[14:18], 1)
		binary.LittleEndian.PutUint32(p[18:22], sequence)
		if flags&4 != 0 {
			binary.LittleEndian.PutUint64(p[6:14], 960)
		}
		p = append(p, payload...)
		var crc uint32
		for _, b := range p {
			crc ^= uint32(b) << 24
			for range 8 {
				if crc&0x80000000 != 0 {
					crc = crc<<1 ^ 0x04c11db7
				} else {
					crc <<= 1
				}
			}
		}
		binary.LittleEndian.PutUint32(p[22:26], crc)
		return p
	}
	head := append([]byte("OpusHead"), []byte{1, 1, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0}...)
	tags := append([]byte("OpusTags"), make([]byte, 8)...)
	data := page(0, 2, head)
	data = append(data, page(1, 0, tags)...)
	return append(data, page(2, 4, []byte{0xf8, 0xff, 0xfe})...)
}
