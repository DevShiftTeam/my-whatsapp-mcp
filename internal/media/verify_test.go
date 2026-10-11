package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unicode/utf8"

	"github.com/Bodokh/wa-indexer/internal/archive"
)

func verificationFile(t *testing.T, data []byte) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "media")
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func verificationImages(t *testing.T) (pngData, jpegData []byte) {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 2, 2))
	var p, j bytes.Buffer
	if err := png.Encode(&p, im); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&j, im, nil); err != nil {
		t.Fatal(err)
	}
	return p.Bytes(), j.Bytes()
}

func riffFixture(form string, chunks ...[]byte) []byte {
	p := append([]byte("RIFF\x00\x00\x00\x00"), []byte(form)...)
	for _, c := range chunks {
		p = append(p, c...)
	}
	binary.LittleEndian.PutUint32(p[4:8], uint32(len(p)-8))
	return p
}

func pngChunkFixture(kind string, payload []byte) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p[:4], uint32(len(payload)))
	copy(p[4:], kind)
	p = append(p, payload...)
	crc := crc32.ChecksumIEEE(p[4:])
	return append(p, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}
func riffChunkFixture(kind string, payload []byte) []byte {
	p := append([]byte(kind), make([]byte, 4)...)
	binary.LittleEndian.PutUint32(p[4:8], uint32(len(payload)))
	p = append(p, payload...)
	if len(payload)%2 != 0 {
		p = append(p, 0)
	}
	return p
}
func waveFixture() []byte {
	fmt := make([]byte, 16)
	binary.LittleEndian.PutUint16(fmt[0:2], 1)
	binary.LittleEndian.PutUint16(fmt[2:4], 1)
	binary.LittleEndian.PutUint32(fmt[4:8], 16000)
	binary.LittleEndian.PutUint32(fmt[8:12], 32000)
	binary.LittleEndian.PutUint16(fmt[12:14], 2)
	binary.LittleEndian.PutUint16(fmt[14:16], 16)
	return riffFixture("WAVE", riffChunkFixture("fmt ", fmt), riffChunkFixture("data", []byte{0, 0, 0, 0}))
}
func oggPageFixture(sequence uint32, flags byte, payload []byte) []byte {
	p := make([]byte, 28)
	copy(p, "OggS")
	p[5], p[26], p[27] = flags, 1, byte(len(payload))
	binary.LittleEndian.PutUint32(p[14:18], 1)
	binary.LittleEndian.PutUint32(p[18:22], sequence)
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
func opusFixture() []byte {
	head := append([]byte("OpusHead"), []byte{1, 1, 0, 0, 0x80, 0x3e, 0, 0, 0, 0, 0}...)
	tags := append([]byte("OpusTags"), make([]byte, 8)...)
	p := oggPageFixture(0, 2, head)
	p = append(p, oggPageFixture(1, 0, tags)...)
	return append(p, oggPageFixture(2, 4, []byte{0xf8, 0xff, 0xfe})...)
}

func opusAudioFixture(packet []byte) []byte {
	p := opusFixture()
	return append(bytes.Clone(p[:len(p)-31]), oggPageFixture(2, 4, packet)...)
}

func TestVerifyOpusPacketFraming(t *testing.T) {
	for _, packet := range [][]byte{{0xf8}, {0xf8, 0xff, 0xfe}, {0xf9, 0, 0}, {0xfa, 0}, {0xfb, 1}} {
		p := opusAudioFixture(packet)
		_, _, format, err := verifyMediaFile(context.Background(), verificationFile(t, p), archive.MessageVoice, int64(len(p)))
		if err != nil || format != "opus" {
			t.Fatalf("valid packet %x rejected: %v", packet, err)
		}
	}
	for _, packet := range [][]byte{{}, {0xf9, 0xff}, {0xfa, 0xff}, {0xfb, 0}, {0xfb, 3, 1, 2}, {0xfb, 0x41, 0xff}} {
		p := opusAudioFixture(packet)
		if _, _, _, err := verifyMediaFile(context.Background(), verificationFile(t, p), archive.MessageVoice, int64(len(p))); !errors.Is(err, ErrFormat) {
			t.Fatalf("malformed packet %x accepted: %v", packet, err)
		}
	}
}

func rewriteOggFixtureCRC(p []byte) {
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

func TestVerifyRejectsOpusIdentificationPageConstraints(t *testing.T) {
	granule := opusFixture()
	granule[6] = 1
	rewriteOggFixtureCRC(granule[:47])
	head := append([]byte("OpusHead"), []byte{1, 1, 0, 0, 0x80, 0x3e, 0, 0, 0, 0, 254, 1, 0, 0}...)
	family := append(oggPageFixture(0, 2, head), opusFixture()[47:]...)
	for _, p := range [][]byte{granule, family} {
		if _, _, _, err := verifyMediaFile(context.Background(), verificationFile(t, p), archive.MessageVoice, int64(len(p))); !errors.Is(err, ErrFormat) {
			t.Fatalf("invalid identification accepted: %v", err)
		}
	}
}

func repeatedFLACFixture(frames int) []byte {
	p := bytes.Clone(flacFixture()[:42])
	packed := uint64(44100)<<44 | uint64(15)<<36 | uint64(frames*16)
	binary.BigEndian.PutUint64(p[18:26], packed)
	for number := 0; number < frames; number++ {
		h := []byte{0xff, 0xf8, 0x69, 0x08}
		h = utf8.AppendRune(h, rune(number))
		h = append(h, 15)
		var crc byte
		for _, b := range h {
			crc ^= b
			for range 8 {
				if crc&0x80 != 0 {
					crc = crc<<1 ^ 7
				} else {
					crc <<= 1
				}
			}
		}
		h = append(h, crc, 0, 0, 0)
		var frameCRC uint16
		for _, b := range h {
			frameCRC ^= uint16(b) << 8
			for range 8 {
				if frameCRC&0x8000 != 0 {
					frameCRC = frameCRC<<1 ^ 0x8005
				} else {
					frameCRC <<= 1
				}
			}
		}
		h = append(h, byte(frameCRC>>8), byte(frameCRC))
		p = append(p, h...)
	}
	return p
}

func TestVerifyManySmallFLACFramesUseBoundedAllocation(t *testing.T) {
	p := repeatedFLACFixture(4096)
	f := verificationFile(t, p)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, format, err := verifyMediaFile(context.Background(), f, archive.MessageVoice, int64(len(p)))
	runtime.ReadMemStats(&after)
	if err != nil || format != "flac" {
		t.Fatalf("valid framed stream rejected: %v", err)
	}
	// A new 64-KiB lookahead buffer per tiny frame allocates over256MiB.
	if after.TotalAlloc-before.TotalAlloc > 16<<20 {
		t.Fatalf("framing allocated %d bytes for %d input bytes", after.TotalAlloc-before.TotalAlloc, len(p))
	}
}

func TestVerifyAcceptsNominalGrayscaleJPEGSampling(t *testing.T) {
	var data bytes.Buffer
	if err := jpeg.Encode(&data, image.NewGray(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	p := data.Bytes()
	sof := bytes.Index(p, []byte{0xff, 0xc0})
	p[sof+11] = 0x44
	_, _, format, err := verifyMediaFile(context.Background(), verificationFile(t, p), archive.MessageImage, int64(len(p)))
	if err != nil || format != "jpeg" {
		t.Fatalf("nominal grayscale sampling rejected: %v", err)
	}
}

func TestVerifyRejectsMP4SampleExtentAndCounts(t *testing.T) {
	oversized := m4aFixture()
	stsz := bytes.Index(oversized, []byte("stsz"))
	binary.BigEndian.PutUint32(oversized[stsz+8:stsz+12], uint32(len(oversized)))
	badCount := m4aFixture()
	stsc := bytes.Index(badCount, []byte("stsc"))
	binary.BigEndian.PutUint32(badCount[stsc+16:stsc+20], 2)
	lastByte := m4aFixture()
	stco := bytes.Index(lastByte, []byte("stco"))
	binary.BigEndian.PutUint32(lastByte[stco+12:stco+16], uint32(len(lastByte)-1))
	for _, p := range [][]byte{oversized, badCount, lastByte} {
		if _, _, _, err := verifyMediaFile(context.Background(), verificationFile(t, p), archive.MessageVoice, int64(len(p))); !errors.Is(err, ErrFormat) {
			t.Fatalf("invalid sample map accepted: %v", err)
		}
	}
}

func TestVerifyRejectsUnsupportedAACConfigurationAndMissingHierarchy(t *testing.T) {
	sbr := m4aFixture()
	config := bytes.Index(sbr, []byte{5, 2, 0x14, 8})
	sbr[config+2], sbr[config+3] = 0x2a, 0x10
	flat := m4aFixture()
	mdia := bytes.Index(flat, []byte("mdia"))
	copy(flat[mdia:mdia+4], "free")
	for _, p := range [][]byte{sbr, flat} {
		if _, _, _, err := verifyMediaFile(context.Background(), verificationFile(t, p), archive.MessageVoice, int64(len(p))); !errors.Is(err, ErrFormat) {
			t.Fatalf("unsupported audio metadata accepted: %v", err)
		}
	}
}

func TestVerifyRejectsWebPAlphaFraming(t *testing.T) {
	canvas := riffChunkFixture("VP8X", []byte{0x10, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	image := riffChunkFixture("VP8 ", []byte{0x30, 1, 0, 0x9d, 1, 0x2a, 1, 0, 1, 0, 0x0e, 0xc0, 0xfe, 0x25, 0xa4, 0, 3, 0x70, 0, 0, 0, 0})
	for _, alpha := range [][]byte{nil, {0x80, 0}, {2, 0}} {
		p := riffFixture("WEBP", canvas, riffChunkFixture("ALPH", alpha), image)
		if _, _, _, err := verifyMediaFile(context.Background(), verificationFile(t, p), archive.MessageImage, int64(len(p))); !errors.Is(err, ErrFormat) {
			t.Fatalf("invalid alpha metadata accepted: %v", err)
		}
	}
}

func TestVerifyRejectsVorbisCommentWithoutFramingByte(t *testing.T) {
	head := make([]byte, 30)
	copy(head, "\x01vorbis")
	head[11] = 1
	binary.LittleEndian.PutUint32(head[12:16], 48000)
	head[28], head[29] = 0x66, 1
	comment := append([]byte("\x03vorbis"), []byte{0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1}...)
	p := oggPageFixture(0, 2, head)
	p = append(p, oggPageFixture(1, 0, comment)...)
	p = append(p, oggPageFixture(2, 0, []byte("\x05vorbis\x01"))...)
	p = append(p, oggPageFixture(3, 4, []byte{0})...)
	if _, _, _, err := verifyMediaFile(context.Background(), verificationFile(t, p), archive.MessageVoice, int64(len(p))); !errors.Is(err, ErrFormat) {
		t.Fatalf("missing comment framing accepted: %v", err)
	}
}
func flacFixture() []byte {
	p := append([]byte("fLaC\x80\x00\x00\x22"), make([]byte, 34)...)
	binary.BigEndian.PutUint16(p[8:10], 16)
	binary.BigEndian.PutUint16(p[10:12], 16)
	info := uint64(44100)<<44 | uint64(15)<<36 | 16
	binary.BigEndian.PutUint64(p[18:26], info)
	h := []byte{0xff, 0xf8, 0x69, 0x08, 0, 15}
	var crc byte
	for _, b := range h {
		crc ^= b
		for range 8 {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 7
			} else {
				crc <<= 1
			}
		}
	}
	h = append(h, crc, 0, 0, 0) // Constant mono subframe, signed 16-bit zero.
	var frameCRC uint16
	for _, b := range h {
		frameCRC ^= uint16(b) << 8
		for range 8 {
			if frameCRC&0x8000 != 0 {
				frameCRC = frameCRC<<1 ^ 0x8005
			} else {
				frameCRC <<= 1
			}
		}
	}
	h = append(h, byte(frameCRC>>8), byte(frameCRC))
	return append(p, h...)
}
func mp3Fixture() []byte {
	// MPEG-1 Layer III, 128 kb/s, 44.1 kHz; 417-byte frame.
	p := make([]byte, 417)
	copy(p, []byte{0xff, 0xfb, 0x90, 0})
	return p
}
func mp4BoxFixture(kind string, payload []byte) []byte {
	p := make([]byte, 8)
	binary.BigEndian.PutUint32(p[:4], uint32(len(payload)+8))
	copy(p[4:], kind)
	return append(p, payload...)
}
func m4aFixture() []byte {
	entry := make([]byte, 28)
	binary.BigEndian.PutUint16(entry[6:8], 1)
	binary.BigEndian.PutUint16(entry[16:18], 1)
	binary.BigEndian.PutUint16(entry[18:20], 16)
	binary.BigEndian.PutUint32(entry[24:28], 16000<<16)
	entry = append(entry, mp4BoxFixture("esds", []byte{0, 0, 0, 0, 3, 25, 0, 1, 0, 4, 17, 0x40, 0x15, 0, 0, 0, 0, 0, 0xfa, 0, 0, 0, 0xfa, 0, 5, 2, 0x14, 8, 6, 1, 2})...)
	stsd := append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, mp4BoxFixture("mp4a", entry)...)
	stblData := mp4BoxFixture("stsd", stsd)
	stblData = append(stblData, mp4BoxFixture("stts", []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 4, 0})...)
	stblData = append(stblData, mp4BoxFixture("stsc", []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1})...)
	stblData = append(stblData, mp4BoxFixture("stsz", []byte{0, 0, 0, 0, 0, 0, 0, 6, 0, 0, 0, 1})...)
	stblData = append(stblData, mp4BoxFixture("stco", []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0})...)
	handler := make([]byte, 24)
	copy(handler[8:12], "soun")
	mdhd := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhd[12:16], 16000)
	binary.BigEndian.PutUint32(mdhd[16:20], 1024)
	mdia := mp4BoxFixture("mdia", append(mp4BoxFixture("mdhd", mdhd), append(mp4BoxFixture("hdlr", handler), mp4BoxFixture("minf", mp4BoxFixture("stbl", stblData))...)...))
	tkhd := make([]byte, 84)
	binary.BigEndian.PutUint32(tkhd[12:16], 1)
	trak := mp4BoxFixture("trak", append(mp4BoxFixture("tkhd", tkhd), mdia...))
	mvhd := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhd[12:16], 16000)
	binary.BigEndian.PutUint32(mvhd[16:20], 1024)
	moov := mp4BoxFixture("moov", append(mp4BoxFixture("mvhd", mvhd), trak...))
	p := mp4BoxFixture("ftyp", []byte("M4A \x00\x00\x00\x00M4A isom"))
	p = append(p, moov...)
	chunk := bytes.Index(p, []byte("stco"))
	binary.BigEndian.PutUint32(p[chunk+12:chunk+16], uint32(len(p)+8))
	return append(p, mp4BoxFixture("mdat", []byte{0x21, 0x10, 4, 0x60, 0x8c, 0x1c})...)
}

func TestVerifyHashesExactFileWithoutChangingOffset(t *testing.T) {
	p, j := verificationImages(t)
	for _, tt := range []struct {
		name   string
		data   []byte
		kind   archive.MessageKind
		format string
	}{
		{"png", p, archive.MessageImage, "png"}, {"jpeg", j, archive.MessageImage, "jpeg"},
		{"webp", riffFixture("WEBP", riffChunkFixture("VP8 ", []byte{0x30, 0x01, 0, 0x9d, 0x01, 0x2a, 1, 0, 1, 0, 0x0e, 0xc0, 0xfe, 0x25, 0xa4, 0, 3, 0x70, 0, 0, 0, 0})), archive.MessageImage, "webp"},
		{"wav", waveFixture(), archive.MessageVoice, "wav"}, {"opus", opusFixture(), archive.MessageVoice, "opus"},
		{"flac", flacFixture(), archive.MessageVoice, "flac"}, {"mp3", mp3Fixture(), archive.MessageVoice, "mp3"}, {"m4a", m4aFixture(), archive.MessageVoice, "m4a"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := verificationFile(t, tt.data)
			if _, err := f.Seek(3, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			digest, size, format, err := verifyMediaFile(context.Background(), f, tt.kind, int64(len(tt.data)))
			want := sha256.Sum256(tt.data)
			if err != nil || digest != hex.EncodeToString(want[:]) || size != int64(len(tt.data)) || format != tt.format {
				t.Fatalf("verify = %q %d %q %v", digest, size, format, err)
			}
			if offset, _ := f.Seek(0, io.SeekCurrent); offset != 3 {
				t.Fatalf("offset changed: %d", offset)
			}
		})
	}
}

func TestVerifyRejectsMalformedAndWrongKind(t *testing.T) {
	p, j := verificationImages(t)
	badPNG := bytes.Clone(p)
	badPNG[29] ^= 1
	badRIFF := waveFixture()
	badRIFF[4]++
	badOgg := opusFixture()
	badOgg[len(badOgg)-1] ^= 1
	badFLAC := flacFixture()
	badFLAC[42+6] ^= 1
	badMP4 := m4aFixture()
	binary.BigEndian.PutUint32(badMP4[:4], 7)
	idat := bytes.Index(p, []byte("IDAT")) - 4
	interruptedPNG := append(bytes.Clone(p[:idat]), pngChunkFixture("IDAT", nil)...)
	interruptedPNG = append(interruptedPNG, pngChunkFixture("tEXt", []byte("name\x00value"))...)
	interruptedPNG = append(interruptedPNG, p[idat:]...)
	paletteAfterIDAT := append(bytes.Clone(p[:idat]), pngChunkFixture("IDAT", nil)...)
	paletteAfterIDAT = append(paletteAfterIDAT, pngChunkFixture("PLTE", []byte{0, 0, 0})...)
	paletteAfterIDAT = append(paletteAfterIDAT, p[idat:]...)
	sos := bytes.Index(j, []byte{0xff, 0xda})
	unknownJPEGComponent := bytes.Clone(j)
	unknownJPEGComponent[sos+5] = 99
	badJPEGSelector := bytes.Clone(j)
	badJPEGSelector[sos+6] = 0xff
	badJPEGScan := bytes.Clone(j)
	badJPEGScan[sos+5+2*int(j[sos+4])] = 1
	reservedJPEGMarker := append(bytes.Clone(j[:2]), []byte{0xff, 2, 0, 2}...)
	reservedJPEGMarker = append(reservedJPEGMarker, j[2:]...)
	badWAVEExtension := waveFixture()
	badWAVEExtension = riffFixture("WAVE", riffChunkFixture("fmt ", append([]byte{0xfe, 0xff, 1, 0, 0x80, 0x3e, 0, 0, 0, 0x7d, 0, 0, 2, 0, 16, 0, 0xff, 0xff, 16, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}, nil...)), riffChunkFixture("data", []byte{0, 0}))
	zeroSuffixFLAC := append(flacFixture(), 0, 0)
	for _, tt := range []struct {
		name string
		data []byte
		kind archive.MessageKind
	}{
		{"png crc", badPNG, archive.MessageImage}, {"png truncated", p[:len(p)-1], archive.MessageImage},
		{"jpeg truncated", j[:len(j)-2], archive.MessageImage}, {"image as voice", p, archive.MessageVoice},
		{"empty IDAT interrupted", interruptedPNG, archive.MessageImage}, {"PLTE after empty IDAT", paletteAfterIDAT, archive.MessageImage},
		{"jpeg unknown component", unknownJPEGComponent, archive.MessageImage}, {"jpeg selector", badJPEGSelector, archive.MessageImage}, {"jpeg sequential scan", badJPEGScan, archive.MessageImage}, {"jpeg reserved marker", reservedJPEGMarker, archive.MessageImage},
		{"voice as image", waveFixture(), archive.MessageImage}, {"riff length", badRIFF, archive.MessageVoice},
		{"ogg checksum", badOgg, archive.MessageVoice}, {"flac header crc", badFLAC, archive.MessageVoice},
		{"wav extension exceeds chunk", badWAVEExtension, archive.MessageVoice}, {"flac zero suffix", zeroSuffixFLAC, archive.MessageVoice},
		{"mp3 truncated frame", mp3Fixture()[:100], archive.MessageVoice}, {"mp4 box length", badMP4, archive.MessageVoice},
		{"bare magic", []byte("OpusHead"), archive.MessageVoice}, {"unknown", []byte("arbitrary media bytes"), ""},
		{"unsupported kind", p, archive.MessageText},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := verifyMediaFile(context.Background(), verificationFile(t, tt.data), tt.kind, int64(len(tt.data)))
			if !errors.Is(err, ErrFormat) {
				t.Fatalf("wanted format rejection, got %v", err)
			}
		})
	}
}

func TestVerifyEnforcesBoundsPrivateFDAndCancellation(t *testing.T) {
	p, _ := verificationImages(t)
	for _, limit := range []int64{0, -1, int64(len(p) - 1)} {
		_, _, _, err := verifyMediaFile(context.Background(), verificationFile(t, p), archive.MessageImage, limit)
		if !errors.Is(err, ErrLimit) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
	f := verificationFile(t, p)
	if err := f.Chmod(0644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := verifyMediaFile(context.Background(), f, "", int64(len(p))); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("public file accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := verifyMediaFile(ctx, verificationFile(t, p), "", int64(len(p))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if _, _, _, err := verifyMediaFile(context.Background(), verificationFile(t, nil), "", 1); !errors.Is(err, ErrFormat) {
		t.Fatalf("empty file accepted: %v", err)
	}
}

type verificationMutationContext struct {
	context.Context
	calls  int
	at     int
	mutate func()
}

func (c *verificationMutationContext) Err() error {
	c.calls++
	at := c.at
	if at == 0 {
		at = 2
	}
	if c.calls == at {
		c.mutate()
	}
	return c.Context.Err()
}

func TestVerifyRejectsExtentOrPermissionsChangedAfterInitialStat(t *testing.T) {
	p, _ := verificationImages(t)
	for _, kind := range []string{"extent", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			f := verificationFile(t, p)
			ctx := &verificationMutationContext{Context: context.Background(), mutate: func() {
				var err error
				if kind == "extent" {
					err = f.Truncate(int64(len(p) - 1))
				} else {
					err = f.Chmod(0644)
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			if _, _, _, err := verifyMediaFile(ctx, f, archive.MessageImage, int64(len(p))); !errors.Is(err, ErrUnsafeFile) {
				t.Fatalf("file changed after initial stat accepted: %v", err)
			}
		})
	}
}

func TestVerifyObservesCancellationBetweenHashChunks(t *testing.T) {
	p := riffFixture("WAVE", waveFixture()[12:36], riffChunkFixture("data", make([]byte, 128<<10)))
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &verificationMutationContext{Context: base, at: 3, mutate: cancel}
	if _, _, _, err := verifyMediaFile(ctx, verificationFile(t, p), archive.MessageVoice, int64(len(p))); !errors.Is(err, context.Canceled) {
		t.Fatalf("in-progress cancellation ignored: %v", err)
	}
}
