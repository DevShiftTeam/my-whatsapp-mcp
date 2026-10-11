//go:build darwin || linux

package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

func opusCompatibilityPage(sequence uint32, flags byte, granule uint64, laces, body []byte) []byte {
	p := make([]byte, 27)
	copy(p, "OggS")
	p[5], p[26] = flags, byte(len(laces))
	binary.LittleEndian.PutUint64(p[6:14], granule)
	binary.LittleEndian.PutUint32(p[14:18], 1)
	binary.LittleEndian.PutUint32(p[18:22], sequence)
	p = append(p, laces...)
	p = append(p, body...)
	rewriteOggFixtureCRC(p)
	return p
}

func opusCompatibilityHeaders() []byte {
	head := append([]byte("OpusHead"), []byte{1, 1, 0x38, 1, 0x80, 0xbb, 0, 0, 0, 0, 0}...)
	tags := append([]byte("OpusTags\x08\x00\x00\x00fixture!"), make([]byte, 4)...)
	p := opusCompatibilityPage(0, 2, 0, []byte{19}, head)
	return append(p, opusCompatibilityPage(1, 0, 0, []byte{24}, tags)...)
}

func opusCompatibilityFixture(finalFlags byte) ([]byte, int) {
	p := opusCompatibilityHeaders()
	p = append(p, opusCompatibilityPage(2, 0, 1920, []byte{3, 3}, []byte{0xf8, 0xff, 0xfe, 0xf8, 0xff, 0xfe})...)
	last := len(p)
	p = append(p, opusCompatibilityPage(3, finalFlags, 2880, []byte{3}, []byte{0xf8, 0xff, 0xfe})...)
	return p, last
}

func TestVerifyWhatsAppOpusWithoutEOS(t *testing.T) {
	for _, flags := range []byte{0, 4} {
		p, _ := opusCompatibilityFixture(flags)
		_, size, format, err := verifyMediaFile(context.Background(), verificationFile(t, p), archive.MessageVoice, int64(len(p)))
		if err != nil || format != "opus" || size != int64(len(p)) {
			t.Fatalf("complete Opus pages with final flags %d: format=%q size=%d err=%v", flags, format, size, err)
		}
	}
	p, last := opusCompatibilityFixture(0)
	continued := append(bytes.Clone(p[:last]), opusCompatibilityPage(3, 0, ^uint64(0), []byte{255}, append([]byte{0xf8}, make([]byte, 254)...))...)
	continued = append(continued, opusCompatibilityPage(4, 1, 2880, []byte{0}, nil)...)
	_, _, format, err := verifyMediaFile(context.Background(), verificationFile(t, continued), archive.MessageVoice, int64(len(continued)))
	if err != nil || format != "opus" {
		t.Fatalf("complete continued Opus packet without EOS: format=%q err=%v", format, err)
	}
}

func TestVerifyWhatsAppOpusWithoutEOSKeepsFramingChecks(t *testing.T) {
	p, last := opusCompatibilityFixture(0)
	changeLast := func(change func([]byte)) []byte {
		out := bytes.Clone(p)
		change(out[last:])
		rewriteOggFixtureCRC(out[last:])
		return out
	}
	badCRC := bytes.Clone(p)
	badCRC[len(badCRC)-1] ^= 1
	unfinished := append(bytes.Clone(p[:last]), opusCompatibilityPage(3, 0, ^uint64(0), []byte{255}, append([]byte{0xf8}, make([]byte, 254)...))...)
	eos, _ := opusCompatibilityFixture(4)
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"checksum", badCRC},
		{"missing page", changeLast(func(page []byte) { binary.LittleEndian.PutUint32(page[18:22], 4) })},
		{"different stream", changeLast(func(page []byte) { binary.LittleEndian.PutUint32(page[14:18], 2) })},
		{"unexpected continuation", changeLast(func(page []byte) { page[5] = 1 })},
		{"incomplete packet", unfinished},
		{"truncated page header", p[:last+26]},
		{"truncated page body", p[:len(p)-1]},
		{"trailing byte", append(bytes.Clone(p), 0)},
		{"page after EOS", append(eos, opusCompatibilityPage(4, 0, 3840, []byte{3}, []byte{0xf8, 0xff, 0xfe})...)},
		{"headers without audio", opusCompatibilityHeaders()},
		{"invalid audio packet", changeLast(func(page []byte) { page[28], page[29] = 0xfb, 0xff })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, _, err := verifyMediaFile(context.Background(), verificationFile(t, tc.data), archive.MessageVoice, int64(len(tc.data))); !errors.Is(err, ErrFormat) {
				t.Fatalf("malformed Opus accepted: %v", err)
			}
		})
	}
}

func TestVerifyVorbisStillRequiresEOS(t *testing.T) {
	head := make([]byte, 30)
	copy(head, "\x01vorbis")
	head[11] = 1
	binary.LittleEndian.PutUint32(head[12:16], 48000)
	head[28], head[29] = 0x66, 1
	comment := append([]byte("\x03vorbis"), make([]byte, 8)...)
	comment = append(comment, 1)
	prefix := oggPageFixture(0, 2, head)
	prefix = append(prefix, oggPageFixture(1, 0, comment)...)
	prefix = append(prefix, oggPageFixture(2, 0, []byte("\x05vorbis\x01"))...)
	for _, flags := range []byte{0, 4} {
		p := append(bytes.Clone(prefix), oggPageFixture(3, flags, []byte{0})...)
		_, _, format, err := verifyMediaFile(context.Background(), verificationFile(t, p), archive.MessageVoice, int64(len(p)))
		if flags == 0 {
			if !errors.Is(err, ErrFormat) {
				t.Fatalf("Vorbis without EOS accepted: %v", err)
			}
		} else if err != nil || format != "ogg" {
			t.Fatalf("Vorbis with EOS: format=%q err=%v", format, err)
		}
	}
}
