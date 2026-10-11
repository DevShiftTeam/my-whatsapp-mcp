//go:build darwin || linux

package media

import (
	"bytes"
	"encoding/binary"
)

func (v mediaVerification) wav() bool {
	if !v.riff("WAVE") {
		return false
	}
	var block uint16
	seenData := false
	for offset := int64(12); offset < v.size; {
		h, err := v.read(offset, 8)
		if err != nil {
			return false
		}
		n := int64(binary.LittleEndian.Uint32(h[4:]))
		if n+n%2 > v.size-offset-8 {
			return false
		}
		switch string(h[:4]) {
		case "fmt ":
			if block != 0 || n < 16 || n > 65536 {
				return false
			}
			p, err := v.read(offset+8, n)
			if err != nil {
				return false
			}
			codec, channels, rate, bytesPerSecond, align, bits := binary.LittleEndian.Uint16(p[:2]), binary.LittleEndian.Uint16(p[2:4]), binary.LittleEndian.Uint32(p[4:8]), binary.LittleEndian.Uint32(p[8:12]), binary.LittleEndian.Uint16(p[12:14]), binary.LittleEndian.Uint16(p[14:16])
			if codec == 0xfffe {
				if n < 40 || binary.LittleEndian.Uint16(p[16:18]) < 22 || int64(binary.LittleEndian.Uint16(p[16:18])) > n-18 || binary.LittleEndian.Uint16(p[18:20]) == 0 || binary.LittleEndian.Uint16(p[18:20]) > bits || !bytes.Equal(p[26:40], []byte{0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}) {
					return false
				}
				codec = binary.LittleEndian.Uint16(p[24:26])
			}
			if codec != 1 && codec != 3 || channels == 0 || channels > 8 || rate == 0 || rate > 384000 || bits == 0 || bits%8 != 0 || codec == 1 && bits > 32 || codec == 3 && bits != 32 && bits != 64 || uint32(align) != uint32(channels)*uint32(bits)/8 || bytesPerSecond != rate*uint32(align) {
				return false
			}
			block = align
		case "data":
			if block == 0 || n == 0 || n%int64(block) != 0 {
				return false
			}
			seenData = true
		}
		offset += 8 + n + n%2
	}
	return block != 0 && seenData
}
