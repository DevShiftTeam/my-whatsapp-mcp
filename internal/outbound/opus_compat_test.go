package outbound

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"testing"
)

func TestOpusCompleteRecordingWithoutEOS(t *testing.T) {
	data := opusRecordingWithPages(opusTestPage(2, 0, []byte{3}, []byte{0xf8, 0xff, 0xfe}))
	for _, kind := range []string{"audio", "voice"} {
		t.Run(kind, func(t *testing.T) {
			r := Request{ChatID: "chat", RequestID: "no-eos", Kind: kind, MIMEType: "audio/ogg", DataBase64: base64.StdEncoding.EncodeToString(data)}
			if kind == "voice" {
				r.Seconds = 1
			}
			if _, err := Validate(r); err != nil {
				t.Fatalf("complete Ogg/Opus recording without EOS rejected: %v", err)
			}
		})
	}
}

func TestOpusCompleteContinuedPacketWithoutEOS(t *testing.T) {
	packet := append([]byte{0xf8}, bytes.Repeat([]byte{0}, 255)...)
	data := opusRecordingWithPages(
		opusTestPage(2, 0, []byte{255}, packet[:255]),
		opusTestPage(3, 1, []byte{1}, packet[255:]),
	)
	if !validOpus(data) {
		t.Fatal("complete continued Opus packet without EOS rejected")
	}
}

func TestOpusWithoutEOSStillRejectsBrokenFraming(t *testing.T) {
	validPage := opusTestPage(2, 0, []byte{3}, []byte{0xf8, 0xff, 0xfe})
	badCRC := append([]byte(nil), validPage...)
	badCRC[len(badCRC)-1] ^= 1
	badSequence := opusTestPage(3, 0, []byte{3}, []byte{0xf8, 0xff, 0xfe})
	badSerial := append([]byte(nil), validPage...)
	binary.LittleEndian.PutUint32(badSerial[14:18], 2)
	opusTestChecksum(badSerial)
	for name, pages := range map[string][][]byte{
		"truncated page header":      {validPage[:20]},
		"truncated lacing table":     {validPage[:27]},
		"truncated audio":            {validPage[:len(validPage)-1]},
		"bad checksum":               {badCRC},
		"wrong sequence":             {badSequence},
		"different stream serial":    {badSerial},
		"unexpected continuation":    {opusTestPage(2, 1, []byte{3}, []byte{0xf8, 0xff, 0xfe})},
		"repeated beginning flag":    {opusTestPage(2, 2, []byte{3}, []byte{0xf8, 0xff, 0xfe})},
		"reserved flag":              {opusTestPage(2, 8, []byte{3}, []byte{0xf8, 0xff, 0xfe})},
		"unfinished packet":          {opusTestPage(2, 0, []byte{255}, append([]byte{0xf8}, bytes.Repeat([]byte{0}, 254)...))},
		"truncated variable framing": {opusTestPage(2, 0, []byte{2}, []byte{0xfa, 252})},
		"missing frame count":        {opusTestPage(2, 0, []byte{1}, []byte{0xfb})},
		"zero frame count":           {opusTestPage(2, 0, []byte{2}, []byte{0xfb, 0})},
		"unequal constant frames":    {opusTestPage(2, 0, []byte{2}, []byte{0xf9, 0xff})},
		"data after end of stream": {
			opusTestPage(2, 4, []byte{3}, []byte{0xf8, 0xff, 0xfe}),
			opusTestPage(3, 0, []byte{3}, []byte{0xf8, 0xff, 0xfe}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			data := opusRecordingWithPages(pages...)
			r := Request{ChatID: "chat", RequestID: "bad-opus", Kind: "voice", MIMEType: "audio/ogg", Seconds: 1, DataBase64: base64.StdEncoding.EncodeToString(data)}
			if _, err := Validate(r); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid Ogg/Opus recording accepted: %v", err)
			}
		})
	}
}

func opusRecordingWithPages(pages ...[]byte) []byte {
	fixture := syntheticOpusFixture()
	// Keep the existing synthetic identification and comment pages.
	offset := 0
	for range 2 {
		segments := int(fixture[offset+26])
		pageSize := 27 + segments
		for _, size := range fixture[offset+27 : offset+27+segments] {
			pageSize += int(size)
		}
		offset += pageSize
	}
	data := append([]byte(nil), fixture[:offset]...)
	for _, page := range pages {
		data = append(data, page...)
	}
	return data
}

func opusTestPage(sequence uint32, flags byte, laces, payload []byte) []byte {
	p := make([]byte, 27)
	copy(p, "OggS")
	p[5], p[26] = flags, byte(len(laces))
	binary.LittleEndian.PutUint64(p[6:14], 960)
	binary.LittleEndian.PutUint32(p[14:18], 1)
	binary.LittleEndian.PutUint32(p[18:22], sequence)
	p = append(p, laces...)
	p = append(p, payload...)
	opusTestChecksum(p)
	return p
}

func opusTestChecksum(p []byte) {
	for i := 22; i < 26; i++ {
		p[i] = 0
	}
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
}
