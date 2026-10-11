//go:build darwin || linux

package media

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image/jpeg"
	"image/png"
	"io"
)

func (v mediaVerification) png() bool {
	config, err := png.DecodeConfig(verificationReader{v.ctx, io.NewSectionReader(v.file, 0, v.size)})
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return false
	}
	seenIDAT, seenData, endedData, palette := false, false, false, false
	var colour byte
	for offset, index := int64(8), 0; offset < v.size; index++ {
		h, err := v.read(offset, 8)
		if err != nil {
			return false
		}
		n, kind := int64(binary.BigEndian.Uint32(h[:4])), string(h[4:])
		if n > v.size-offset-12 || index == 0 && (kind != "IHDR" || n != 13) || index != 0 && kind == "IHDR" {
			return false
		}
		for _, c := range h[4:] {
			if c < 'A' || c > 'Z' && c < 'a' || c > 'z' {
				return false
			}
		}
		if h[6]&32 != 0 {
			return false
		}
		crc := crc32.NewIEEE()
		_, _ = crc.Write(h[4:])
		for read := int64(0); read < n; {
			p, err := v.read(offset+8+read, min(n-read, 64<<10))
			if err != nil {
				return false
			}
			_, _ = crc.Write(p)
			read += int64(len(p))
		}
		checksum, err := v.read(offset+8+n, 4)
		if err != nil || crc.Sum32() != binary.BigEndian.Uint32(checksum) {
			return false
		}
		switch kind {
		case "IHDR":
			p, err := v.read(offset+8, n)
			if err != nil {
				return false
			}
			colour = p[9]
		case "PLTE":
			if palette || seenIDAT || colour == 0 || colour == 4 || n == 0 || n > 768 || n%3 != 0 {
				return false
			}
			palette = true
		case "IDAT":
			if endedData || colour == 3 && !palette {
				return false
			}
			seenIDAT = true
			seenData = seenData || n > 0
		case "IEND":
			return n == 0 && seenData && offset+12 == v.size
		default:
			if h[4]&32 == 0 {
				return false
			}
			if seenIDAT {
				endedData = true
			}
		}
		offset += n + 12
	}
	return false
}

func (v mediaVerification) jpeg() bool {
	config, err := jpeg.DecodeConfig(verificationReader{v.ctx, io.NewSectionReader(v.file, 0, v.size)})
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return false
	}
	r := bufio.NewReaderSize(verificationReader{v.ctx, io.NewSectionReader(v.file, 2, v.size-2)}, 64<<10)
	remaining, seenScan, entropy, inScan := v.size-2, false, false, false
	var frameMarker byte
	components := make(map[byte]int)
	read := func() (byte, error) {
		b, err := r.ReadByte()
		if err == nil {
			remaining--
		}
		return b, err
	}
	for remaining > 0 {
		b, err := read()
		if err != nil {
			return false
		}
		if b != 0xff {
			if !inScan {
				return false
			}
			entropy = true
			continue
		}
		for {
			b, err = read()
			if err != nil {
				return false
			}
			if b != 0xff {
				break
			}
		}
		marker := b
		if inScan && (marker == 0 || marker >= 0xd0 && marker <= 0xd7) {
			entropy = true
			continue
		}
		if inScan && !entropy {
			return false
		}
		inScan = false
		if marker == 0xd9 {
			return seenScan && entropy && remaining == 0
		}
		supported := marker == 0xc0 || marker == 0xc1 || marker == 0xc2 || marker == 0xc4 || marker == 0xdb || marker == 0xdd || marker == 0xda || marker >= 0xe0 && marker <= 0xef || marker == 0xfe
		if !supported {
			return false
		}
		hi, err := read()
		if err != nil {
			return false
		}
		lo, err := read()
		if err != nil {
			return false
		}
		n := int64(hi)<<8 | int64(lo)
		if n < 2 || n-2 > remaining {
			return false
		}
		payload := make([]byte, int(n-2))
		if _, err := io.ReadFull(r, payload); err != nil {
			return false
		}
		remaining -= n - 2
		switch marker {
		case 0xc0, 0xc1, 0xc2:
			if frameMarker != 0 || len(payload) < 6 || payload[0] != 8 || payload[5] == 0 || payload[5] > 4 || len(payload) != 6+3*int(payload[5]) || binary.BigEndian.Uint16(payload[1:3]) == 0 || binary.BigEndian.Uint16(payload[3:5]) == 0 {
				return false
			}
			for i := 0; i < int(payload[5]); i++ {
				id, sampling, table := payload[6+3*i], payload[7+3*i], payload[8+3*i]
				horizontal, vertical := int(sampling>>4), int(sampling&15)
				if _, exists := components[id]; exists || horizontal == 0 || horizontal > 4 || vertical == 0 || vertical > 4 || table > 3 {
					return false
				}
				components[id] = horizontal * vertical
			}
			frameMarker = marker
		case 0xda:
			if frameMarker == 0 || len(payload) < 4 || payload[0] == 0 || payload[0] > 4 || len(payload) != 4+2*int(payload[0]) {
				return false
			}
			seen := make(map[byte]bool)
			total := 0
			for i := 0; i < int(payload[0]); i++ {
				id, selectors := payload[1+2*i], payload[2+2*i]
				sampling, exists := components[id]
				bound := byte(3)
				if frameMarker == 0xc0 {
					bound = 1
				}
				if !exists || seen[id] || selectors>>4 > bound || selectors&15 > bound {
					return false
				}
				seen[id] = true
				total += sampling
			}
			if payload[0] > 1 && total > 10 {
				return false
			}
			tail := payload[len(payload)-3:]
			ss, se, ah, al := tail[0], tail[1], tail[2]>>4, tail[2]&15
			if frameMarker != 0xc2 {
				if ss != 0 || se != 63 || ah != 0 || al != 0 {
					return false
				}
			} else if ss > se || se >= 64 || ss == 0 && se != 0 || ss != 0 && payload[0] != 1 || ah > 13 || al > 13 || ah != 0 && ah != al+1 {
				return false
			}
			seenScan, inScan, entropy = true, true, false
		case 0xdd:
			if len(payload) != 2 {
				return false
			}
		case 0xdb:
			for len(payload) > 0 {
				info := payload[0]
				precision := info >> 4
				if precision > 1 || info&15 > 3 {
					return false
				}
				count := 64 * (1 + int(precision))
				if len(payload) < count+1 {
					return false
				}
				payload = payload[count+1:]
			}
		case 0xc4:
			for len(payload) > 0 {
				if len(payload) < 17 || payload[0]>>4 > 1 || payload[0]&15 > 3 {
					return false
				}
				count, slots := 0, 1
				for _, n := range payload[1:17] {
					count += int(n)
					slots = slots*2 - int(n)
					if slots < 0 {
						return false
					}
				}
				if count == 0 || count > 256 || len(payload) < 17+count {
					return false
				}
				payload = payload[17+count:]
			}
		}
	}
	return false
}

func (v mediaVerification) riff(form string) bool {
	h, err := v.read(0, 12)
	return err == nil && string(h[:4]) == "RIFF" && string(h[8:]) == form && int64(binary.LittleEndian.Uint32(h[4:8])) == v.size-8
}

func (v mediaVerification) webpImage(offset, n int64, kind string) bool {
	if n < 6 {
		return false
	}
	p, err := v.read(offset, min(n, 10))
	if err != nil {
		return false
	}
	if kind == "VP8L" {
		return p[0] == 0x2f && p[4]&0xe0 == 0
	}
	if kind != "VP8 " || n < 11 {
		return false
	}
	frame := uint32(p[0]) | uint32(p[1])<<8 | uint32(p[2])<<16
	return frame&1 == 0 && frame>>1&7 <= 3 && frame&16 != 0 && frame>>5 > 0 && int64(frame>>5) <= n-10 && bytes.Equal(p[3:6], []byte{0x9d, 1, 0x2a}) && binary.LittleEndian.Uint16(p[6:8])&0x3fff != 0 && binary.LittleEndian.Uint16(p[8:10])&0x3fff != 0
}

func little24(p []byte) int64 { return int64(p[0]) | int64(p[1])<<8 | int64(p[2])<<16 }
func (v mediaVerification) webpAlpha(offset, n, width, height int64) bool {
	if n < 2 {
		return false
	}
	p, err := v.read(offset, 1)
	if err != nil || p[0]&0xc0 != 0 || p[0]>>4&3 > 1 || p[0]&3 > 1 {
		return false
	}
	return p[0]&3 == 1 || n-1 == width*height
}
func (v mediaVerification) webp() bool {
	if !v.riff("WEBP") {
		return false
	}
	extended, animated, alphaAllowed, animationHeader, seenImage := false, false, false, false, false
	canvasWidth, canvasHeight := int64(0), int64(0)
	alphaOffset, alphaSize := int64(0), int64(0)
	for offset, index := int64(12), 0; offset < v.size; index++ {
		h, err := v.read(offset, 8)
		if err != nil {
			return false
		}
		kind, n := string(h[:4]), int64(binary.LittleEndian.Uint32(h[4:]))
		if n+n%2 > v.size-offset-8 {
			return false
		}
		switch kind {
		case "VP8X":
			if index != 0 || n != 10 {
				return false
			}
			p, err := v.read(offset+8, n)
			if err != nil || p[0]&0xc1 != 0 || p[1] != 0 || p[2] != 0 || p[3] != 0 {
				return false
			}
			extended, animated, alphaAllowed = true, p[0]&2 != 0, p[0]&16 != 0
			canvasWidth, canvasHeight = little24(p[4:7])+1, little24(p[7:10])+1
		case "ALPH":
			if !extended || animated || !alphaAllowed || seenImage || alphaOffset != 0 || n < 2 {
				return false
			}
			alphaOffset, alphaSize = offset+8, n
		case "VP8 ", "VP8L":
			if animated || seenImage || index != 0 && !extended || !v.webpImage(offset+8, n, kind) || kind == "VP8L" && alphaOffset != 0 {
				return false
			}
			if alphaOffset != 0 {
				p, err := v.read(offset+8, 10)
				if err != nil || !v.webpAlpha(alphaOffset, alphaSize, int64(binary.LittleEndian.Uint16(p[6:8])&0x3fff), int64(binary.LittleEndian.Uint16(p[8:10])&0x3fff)) {
					return false
				}
			}
			seenImage = true
		case "ANIM":
			if !animated || animationHeader || n != 6 {
				return false
			}
			animationHeader = true
		case "ANMF":
			if !animated || !animationHeader || n < 24 {
				return false
			}
			frame, err := v.read(offset+8, 16)
			if err != nil || frame[15]&0xfc != 0 {
				return false
			}
			width, height := little24(frame[6:9])+1, little24(frame[9:12])+1
			if little24(frame[:3])*2+width > canvasWidth || little24(frame[3:6])*2+height > canvasHeight {
				return false
			}
			frameImage, frameAlpha := false, false
			frameAlphaOffset, frameAlphaSize := int64(0), int64(0)
			for sub := offset + 24; sub < offset+8+n; {
				child, err := v.read(sub, 8)
				if err != nil {
					return false
				}
				size := int64(binary.LittleEndian.Uint32(child[4:]))
				if size+size%2 > offset+n-sub {
					return false
				}
				childKind := string(child[:4])
				if childKind == "ALPH" {
					if frameImage || frameAlpha || !alphaAllowed || !v.webpAlpha(sub+8, size, width, height) {
						return false
					}
					frameAlpha = true
					frameAlphaOffset, frameAlphaSize = sub+8, size
				} else {
					if frameImage || !v.webpImage(sub+8, size, childKind) || frameAlpha && childKind != "VP8 " {
						return false
					}
					frameImage = true
					if frameAlpha && !v.webpAlpha(frameAlphaOffset, frameAlphaSize, width, height) {
						return false
					}
				}
				sub += 8 + size + size%2
			}
			if !frameImage {
				return false
			}
			seenImage = true
		case "ICCP", "EXIF", "XMP ":
			if !extended {
				return false
			}
		default:
			return false
		}
		offset += 8 + n + n%2
	}
	return seenImage
}
