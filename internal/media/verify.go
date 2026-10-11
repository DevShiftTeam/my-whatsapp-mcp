//go:build darwin || linux

package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

// verifyMediaFile never changes the descriptor's offset. The caller must stop
// writes first and hold its artifact lease until all users of this FD stop.
// These checks validate headers and container framing, not codec decoding.
func verifyMediaFile(ctx context.Context, f *os.File, kind archive.MessageKind, maximum int64) (string, int64, string, error) {
	if maximum <= 0 {
		return "", 0, "", ErrLimit
	}
	if ctx == nil || f == nil {
		return "", 0, "", ErrUnsafeFile
	}
	if err := ctx.Err(); err != nil {
		return "", 0, "", err
	}
	if kind != "" && kind != archive.MessageImage && kind != archive.MessageVoice {
		return "", 0, "", ErrFormat
	}
	if err := checkPrivateFD(f, false); err != nil {
		return "", 0, "", err
	}
	before, err := f.Stat()
	if err != nil {
		return "", 0, "", err
	}
	if before.Size() > maximum {
		return "", 0, "", ErrLimit
	}
	if before.Size() <= 0 {
		return "", 0, "", ErrFormat
	}
	v := mediaVerification{ctx: ctx, file: f, size: before.Size()}
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	for offset := int64(0); offset < v.size; {
		if err := ctx.Err(); err != nil {
			return "", 0, "", err
		}
		n := min(int64(len(buffer)), v.size-offset)
		read, err := f.ReadAt(buffer[:n], offset)
		if err != nil || int64(read) != n {
			return "", 0, "", errors.Join(ErrUnsafeFile, err)
		}
		_, _ = h.Write(buffer[:read])
		offset += int64(read)
	}
	format, err := v.format(kind)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return "", 0, "", err
	}
	if err := ctx.Err(); err != nil {
		return "", 0, "", err
	}
	if err := checkPrivateFD(f, false); err != nil {
		return "", 0, "", err
	}
	after, err := f.Stat()
	if err != nil {
		return "", 0, "", err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return "", 0, "", ErrUnsafeFile
	}
	return hex.EncodeToString(h.Sum(nil)), v.size, format, nil
}

type mediaVerification struct {
	ctx  context.Context
	file *os.File
	size int64
}

type verificationReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r verificationReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (v mediaVerification) read(offset, length int64) ([]byte, error) {
	if err := v.ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || length > 1<<20 || offset > v.size || length > v.size-offset {
		return nil, ErrFormat
	}
	p := make([]byte, int(length))
	n, err := v.file.ReadAt(p, offset)
	if int64(n) != length || err != nil {
		return nil, errors.Join(ErrFormat, err)
	}
	return p, nil
}

func (v mediaVerification) format(kind archive.MessageKind) (string, error) {
	p, err := v.read(0, min(v.size, 12))
	if err != nil {
		return "", err
	}
	imageAllowed, voiceAllowed := kind == "" || kind == archive.MessageImage, kind == "" || kind == archive.MessageVoice
	var format string
	var valid bool
	switch {
	case imageAllowed && bytes.HasPrefix(p, []byte("\x89PNG\r\n\x1a\n")):
		format, valid = "png", v.png()
	case imageAllowed && bytes.HasPrefix(p, []byte{0xff, 0xd8}):
		format, valid = "jpeg", v.jpeg()
	case imageAllowed && len(p) == 12 && string(p[:4]) == "RIFF" && string(p[8:]) == "WEBP":
		format, valid = "webp", v.webp()
	case voiceAllowed && len(p) == 12 && string(p[:4]) == "RIFF" && string(p[8:]) == "WAVE":
		format, valid = "wav", v.wav()
	case voiceAllowed && bytes.HasPrefix(p, []byte("OggS")):
		format, valid = v.ogg()
	case voiceAllowed && bytes.HasPrefix(p, []byte("fLaC")):
		format, valid = "flac", v.flac()
	case voiceAllowed && (bytes.HasPrefix(p, []byte("ID3")) || len(p) >= 2 && p[0] == 0xff && p[1]&0xe0 == 0xe0):
		format, valid = "mp3", v.mp3()
	case voiceAllowed && len(p) >= 8 && (string(p[4:8]) == "ftyp" || string(p[4:8]) == "free"):
		format, valid = "m4a", v.m4a()
	}
	if !valid {
		return "", ErrFormat
	}
	return format, nil
}
