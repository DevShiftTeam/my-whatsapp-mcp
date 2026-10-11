//go:build darwin || linux

package media

import (
	"bytes"
	"encoding/binary"
)

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
	// Multistream Opus needs a separate self-delimiting packet layout. This
	// format gate supports a single elementary stream with explicit mapping.
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
func validComment(p []byte) (int, bool) {
	if len(p) < 8 {
		return 0, false
	}
	vendor := int64(binary.LittleEndian.Uint32(p[:4]))
	if vendor > int64(len(p))-8 {
		return 0, false
	}
	offset := 4 + int(vendor)
	count := binary.LittleEndian.Uint32(p[offset : offset+4])
	offset += 4
	if count > 4096 {
		return 0, false
	}
	for range count {
		if len(p)-offset < 4 {
			return 0, false
		}
		n := int64(binary.LittleEndian.Uint32(p[offset : offset+4]))
		offset += 4
		if n > int64(len(p)-offset) {
			return 0, false
		}
		offset += int(n)
	}
	return offset, true
}

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

func validOpusComment(p []byte) bool { _, ok := validComment(p); return ok }
func validVorbisComment(p []byte) bool {
	offset, ok := validComment(p)
	return ok && offset < len(p) && p[offset] == 1 && offset+1 == len(p)
}

func oggChecksum(p []byte) uint32 {
	var crc uint32
	for i, b := range p {
		if i >= 22 && i < 26 {
			b = 0
		}
		crc ^= uint32(b) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
func (v mediaVerification) ogg() (string, bool) {
	var serial, sequence uint32
	var packet []byte
	codec, packets, audio, eos := "", 0, false, false
	for offset := int64(0); offset < v.size; {
		h, err := v.read(offset, 27)
		if err != nil || string(h[:4]) != "OggS" || h[4] != 0 || h[5]&0xf8 != 0 || eos {
			return "", false
		}
		if offset == 0 {
			if h[5] != 2 || binary.LittleEndian.Uint32(h[18:22]) != 0 {
				return "", false
			}
			serial = binary.LittleEndian.Uint32(h[14:18])
		} else if h[5]&2 != 0 {
			return "", false
		}
		if binary.LittleEndian.Uint32(h[14:18]) != serial || binary.LittleEndian.Uint32(h[18:22]) != sequence || (h[5]&1 != 0) != (len(packet) != 0) || h[26] == 0 {
			return "", false
		}
		sequence++
		laces, err := v.read(offset+27, int64(h[26]))
		if err != nil {
			return "", false
		}
		bodySize := int64(0)
		for _, n := range laces {
			bodySize += int64(n)
		}
		page, err := v.read(offset, 27+int64(h[26])+bodySize)
		if err != nil || oggChecksum(page) != binary.LittleEndian.Uint32(h[22:26]) {
			return "", false
		}
		body := page[27+int(h[26]):]
		for laceIndex, n := range laces {
			if len(packet)+int(n) > 1<<20 {
				return "", false
			}
			packet = append(packet, body[:int(n)]...)
			body = body[int(n):]
			if n == 255 {
				continue
			}
			if packets == 0 {
				if offset != 0 || len(laces) != 1 || binary.LittleEndian.Uint64(h[6:14]) != 0 {
					return "", false
				}
				switch {
				case validOpusHead(packet):
					codec = "opus"
				case len(packet) == 30 && bytes.Equal(packet[:7], []byte("\x01vorbis")) && binary.LittleEndian.Uint32(packet[7:11]) == 0 && packet[11] > 0 && packet[11] <= 8 && binary.LittleEndian.Uint32(packet[12:16]) > 0 && packet[28]&15 >= 6 && packet[28]>>4 <= 13 && packet[28]&15 <= packet[28]>>4 && packet[29] == 1:
					codec = "ogg"
				default:
					return "", false
				}
			} else if codec == "opus" {
				if packets == 1 {
					if laceIndex != len(laces)-1 || binary.LittleEndian.Uint64(h[6:14]) != 0 {
						return "", false
					}
					if len(packet) < 16 || string(packet[:8]) != "OpusTags" || !validOpusComment(packet[8:]) {
						return "", false
					}
				} else {
					if !validOpusPacket(packet) {
						return "", false
					}
					audio = true
				}
			} else if packets == 1 {
				if len(packet) < 16 || !bytes.Equal(packet[:7], []byte("\x03vorbis")) || !validVorbisComment(packet[7:]) {
					return "", false
				}
			} else if packets == 2 {
				if len(packet) < 8 || !bytes.Equal(packet[:7], []byte("\x05vorbis")) || packet[len(packet)-1]&1 == 0 {
					return "", false
				}
			} else {
				if len(packet) == 0 || packet[0]&1 != 0 {
					return "", false
				}
				audio = true
			}
			packets++
			packet = packet[:0]
		}
		eos = h[5]&4 != 0
		offset += int64(len(page))
	}
	return codec, audio && eos && len(packet) == 0
}
