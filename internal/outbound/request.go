// Package outbound validates agent-friendly outbound messages without owning a connection.
package outbound

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"mime"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
)

const MaxMediaBytes = 8 << 20
const MaxRequestBytes = ((MaxMediaBytes+2)/3)*4 + 32768

var ErrInvalid = errors.New("send_invalid_request")
var ErrUnavailable = errors.New("send_unavailable")

type Request struct {
	ChatID     archive.ChatID `json:"chat_id"`
	RequestID  string         `json:"request_id"`
	Kind       string         `json:"kind,omitempty"`
	Text       string         `json:"text,omitempty"`
	DataBase64 string         `json:"data_base64,omitempty"`
	MIMEType   string         `json:"mime_type,omitempty"`
	Filename   string         `json:"filename,omitempty"`
	Caption    string         `json:"caption,omitempty"`
	Seconds    uint32         `json:"seconds,omitempty"`
}
type Payload struct {
	Request       Request
	Data          []byte
	Width, Height uint32
	Digest        [32]byte
}
type Receipt struct {
	RequestID  string         `json:"request_id"`
	ChatID     archive.ChatID `json:"chat_id"`
	MessageID  string         `json:"message_id,omitempty"`
	State      string         `json:"state"`
	AcceptedAt string         `json:"accepted_at,omitempty"`
	Duplicate  bool           `json:"duplicate"`
}

func clean(v string, max int) bool {
	return len(v) <= max && utf8.ValidString(v) && !strings.ContainsRune(v, 0)
}
func ValidRequestID(v string) bool {
	if len(v) < 1 || len(v) > 128 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.:", c)) {
			return false
		}
	}
	return true
}
func Validate(r Request) (Payload, error) {
	fail := func() (Payload, error) { return Payload{}, ErrInvalid }
	if r.ChatID == "" || !clean(string(r.ChatID), 128) || !ValidRequestID(r.RequestID) || !clean(r.Text, 65536) || !clean(r.Caption, 4096) || !clean(r.Filename, 255) || !clean(r.MIMEType, 128) {
		return fail()
	}
	if r.Kind == "" {
		r.Kind = "text"
	}
	p := Payload{Request: r}
	if r.Kind == "text" {
		if strings.TrimSpace(r.Text) == "" || r.DataBase64 != "" || r.MIMEType != "" || r.Filename != "" || r.Caption != "" || r.Seconds != 0 {
			return fail()
		}
	} else {
		if r.Text != "" || r.DataBase64 == "" || len(r.DataBase64) > base64.StdEncoding.EncodedLen(MaxMediaBytes) {
			return fail()
		}
		// Reject whitespace, data URLs and noncanonical encodings, including nonzero pad bits.
		data, err := base64.StdEncoding.Strict().DecodeString(r.DataBase64)
		if err != nil || len(data) == 0 || len(data) > MaxMediaBytes || base64.StdEncoding.EncodeToString(data) != r.DataBase64 {
			return fail()
		}
		kind, params, err := mime.ParseMediaType(r.MIMEType)
		if err != nil {
			return fail()
		}
		if len(params) > 0 && !(r.Kind == "voice" && len(params) == 1 && params["codecs"] == "opus") {
			return fail()
		}
		r.MIMEType = kind
		p.Data = data
		if strings.ContainsAny(r.Filename, "/\\") || strings.TrimSpace(r.Filename) != r.Filename || r.Filename == "." || r.Filename == ".." || strings.IndexFunc(r.Filename, unicode.IsControl) >= 0 {
			return fail()
		}
		switch r.Kind {
		case "image":
			cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 16384 || cfg.Height > 16384 || int64(cfg.Width)*int64(cfg.Height) > 40000000 {
				return fail()
			}
			if format != "jpeg" && format != "png" || kind != "image/"+map[string]string{"jpeg": "jpeg", "png": "png"}[format] || r.Seconds != 0 {
				return fail()
			}
			p.Width, p.Height = uint32(cfg.Width), uint32(cfg.Height)
		case "video":
			if kind != "video/mp4" || len(data) < 12 || string(data[4:8]) != "ftyp" || r.Seconds > 3600 {
				return fail()
			}
		case "audio":
			if r.Caption != "" || r.Seconds > 3600 {
				return fail()
			}
			switch kind {
			case "audio/ogg":
				if !validOpus(data) {
					return fail()
				}
			case "audio/mpeg":
				if !(bytes.HasPrefix(data, []byte("ID3")) || len(data) > 2 && data[0] == 255 && data[1]&224 == 224) {
					return fail()
				}
			case "audio/mp4":
				if len(data) < 12 || string(data[4:8]) != "ftyp" {
					return fail()
				}
			default:
				return fail()
			}
		case "voice":
			if kind != "audio/ogg" || r.Seconds < 1 || r.Seconds > 3600 || r.Caption != "" || !validOpus(data) {
				return fail()
			}
			r.MIMEType = "audio/ogg; codecs=opus"
		case "document":
			if r.Filename == "" || r.Seconds != 0 || !(strings.HasPrefix(kind, "application/") || strings.HasPrefix(kind, "text/")) {
				return fail()
			}
		default:
			return fail()
		}
		p.Request = r
	}
	// Hash canonical metadata and decoded bytes, never retain base64 in the journal.
	canonical := p.Request
	canonical.DataBase64 = ""
	metadata, _ := json.Marshal(canonical)
	h := sha256.New()
	h.Write(metadata)
	h.Write(p.Data)
	copy(p.Digest[:], h.Sum(nil))
	return p, nil
}

// Validate Ogg framing, CRC, ordered packets and nonempty Opus audio.
// This does not decode audio or qualify perceptual quality.
func validOpus(data []byte) bool {
	offset := 0
	ended := false
	var serial, sequence uint32
	packets := 0
	packet := []byte{}
	audio := false
	for offset < len(data) {
		if len(data)-offset < 27 {
			return false
		}
		page := data[offset:]
		if string(page[:4]) != "OggS" || page[4] != 0 || ended || page[5]&^byte(7) != 0 {
			return false
		}
		n := int(page[26])
		if n == 0 || len(page) < 27+n {
			return false
		}
		size := 0
		for _, v := range page[27 : 27+n] {
			size += int(v)
		}
		end := 27 + n + size
		if len(page) < end {
			return false
		}
		page = page[:end]
		var crc uint32
		for i, v := range page {
			if i >= 22 && i < 26 {
				v = 0
			}
			crc ^= uint32(v) << 24
			for bit := 0; bit < 8; bit++ {
				if crc&0x80000000 != 0 {
					crc = (crc << 1) ^ 0x04c11db7
				} else {
					crc <<= 1
				}
			}
		}
		if crc != binary.LittleEndian.Uint32(page[22:26]) {
			return false
		}
		if offset == 0 {
			if page[5] != 2 || binary.LittleEndian.Uint32(page[18:22]) != 0 {
				return false
			}
			serial = binary.LittleEndian.Uint32(page[14:18])
		} else {
			sequence++
			resumedBOS := audio && page[5] == 2 && binary.LittleEndian.Uint64(page[6:14]) == 0 && n == 1 && page[27] != 255
			if (page[5]&2 != 0 && !resumedBOS) || binary.LittleEndian.Uint32(page[14:18]) != serial || binary.LittleEndian.Uint32(page[18:22]) != sequence {
				return false
			}
		}
		if (len(packet) > 0) != (page[5]&1 != 0) {
			return false
		}
		at := 27 + n
		for laceIndex, segment := range page[27 : 27+n] {
			length := int(segment)
			if len(packet)+length > 65536 {
				return false
			}
			packet = append(packet, page[at:at+length]...)
			at += length
			if segment == 255 {
				continue
			}
			switch packets {
			case 0:
				if offset != 0 || n != 1 || binary.LittleEndian.Uint64(page[6:14]) != 0 || !validOpusHead(packet) {
					return false
				}
			case 1:
				if laceIndex != n-1 || binary.LittleEndian.Uint64(page[6:14]) != 0 || len(packet) < 16 || string(packet[:8]) != "OpusTags" || !validOpusComment(packet[8:]) {
					return false
				}
			default:
				if !validOpusPacket(packet) {
					return false
				}
				audio = true
			}
			packets++
			packet = packet[:0]
		}
		ended = page[5]&4 != 0
		if ended && (len(packet) != 0 || binary.LittleEndian.Uint64(page[6:14]) == 0) {
			return false
		}
		offset += end
	}
	// WhatsApp Opus recordings may end without an EOS marker. Accept EOF
	// only after complete, CRC-checked pages and a completed audio packet.
	return audio && packets >= 3 && len(packet) == 0
}

// Check Opus packet framing without decoding the audio. Keep this aligned
// with the retained-recording verifier in internal/media/verify_ogg.go.
func validOpusPacket(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	config := p[0] >> 3
	duration := 480
	if config < 12 {
		duration = [...]int{480, 960, 1920, 2880}[config&3]
	} else if config < 16 {
		duration = [...]int{480, 960}[config&1]
	} else {
		duration = [...]int{120, 240, 480, 960}[config&3]
	}
	code, body := p[0]&3, p[1:]
	switch code {
	case 0:
		return len(body) <= 1275
	case 1:
		return 2*duration <= 5760 && len(body)%2 == 0 && len(body)/2 <= 1275
	case 2:
		first, rest, ok := opusFrameLength(body)
		if !ok || 2*duration > 5760 || first > len(rest) {
			return false
		}
		return len(rest)-first <= 1275
	case 3:
		if len(body) == 0 {
			return false
		}
		control := body[0]
		count := int(control & 63)
		body = body[1:]
		if count == 0 || count > 48 || count*duration > 5760 {
			return false
		}
		if control&64 != 0 {
			padding := 0
			for {
				if len(body) == 0 {
					return false
				}
				n := body[0]
				body = body[1:]
				if n == 255 {
					padding += 254
				} else {
					padding += int(n)
				}
				if padding > len(body) {
					return false
				}
				if n != 255 {
					break
				}
			}
			body = body[:len(body)-padding]
		}
		if control&128 == 0 {
			return len(body)%count == 0 && len(body)/count <= 1275
		}
		total := 0
		for i := 0; i < count-1; i++ {
			n, rest, ok := opusFrameLength(body)
			if !ok {
				return false
			}
			total += n
			body = rest
			if total > len(body) {
				return false
			}
		}
		return len(body)-total <= 1275
	}
	return false
}

func opusFrameLength(p []byte) (int, []byte, bool) {
	if len(p) == 0 {
		return 0, nil, false
	}
	n := int(p[0])
	p = p[1:]
	if n >= 252 {
		if len(p) == 0 {
			return 0, nil, false
		}
		n += 4 * int(p[0])
		p = p[1:]
	}
	return n, p, n <= 1275
}

func validOpusHead(p []byte) bool {
	if len(p) < 19 || string(p[:8]) != "OpusHead" || p[8] == 0 || p[8] > 15 || p[9] == 0 || p[9] > 8 {
		return false
	}
	if p[18] != 0 && p[18] != 1 && p[18] != 255 {
		return false
	}
	if p[18] == 0 {
		return p[9] <= 2 && len(p) == 19
	}
	if len(p) != 21+int(p[9]) || p[19] != 1 || p[20] > p[19] {
		return false
	}
	for _, channel := range p[21:] {
		if channel != 255 && int(channel) >= int(p[19])+int(p[20]) {
			return false
		}
	}
	return true
}

func validOpusComment(p []byte) bool {
	if len(p) < 8 {
		return false
	}
	vendor := int64(binary.LittleEndian.Uint32(p[:4]))
	if vendor > int64(len(p))-8 {
		return false
	}
	offset := 4 + int(vendor)
	count := binary.LittleEndian.Uint32(p[offset : offset+4])
	offset += 4
	if count > 4096 {
		return false
	}
	for range count {
		if len(p)-offset < 4 {
			return false
		}
		n := int64(binary.LittleEndian.Uint32(p[offset : offset+4]))
		offset += 4
		if n > int64(len(p)-offset) {
			return false
		}
		offset += int(n)
	}
	return true
}

