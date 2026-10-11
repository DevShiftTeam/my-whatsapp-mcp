//go:build darwin || linux

package media

import (
	"bytes"
)

func (v mediaVerification) mp3() bool {
	offset := int64(0)
	h, err := v.read(0, min(v.size, 10))
	if err != nil {
		return false
	}
	if len(h) >= 3 && string(h[:3]) == "ID3" {
		if len(h) != 10 || h[3] < 2 || h[3] > 4 || h[4] == 255 || h[3] == 2 && h[5]&63 != 0 || h[3] == 3 && h[5]&31 != 0 || h[3] == 4 && h[5]&15 != 0 {
			return false
		}
		for _, b := range h[6:10] {
			if b&128 != 0 {
				return false
			}
		}
		n := int64(h[6])<<21 | int64(h[7])<<14 | int64(h[8])<<7 | int64(h[9])
		offset = 10 + n
		if h[3] == 4 && h[5]&16 != 0 {
			footer, err := v.read(offset, 10)
			if err != nil || string(footer[:3]) != "3DI" || !bytes.Equal(footer[3:], h[3:]) {
				return false
			}
			offset += 10
		}
		if offset >= v.size {
			return false
		}
	}
	frames := 0
	for offset < v.size {
		if v.size-offset == 128 {
			tag, err := v.read(offset, 3)
			if err == nil && string(tag) == "TAG" {
				offset += 128
				break
			}
		}
		h, err := v.read(offset, 4)
		if err != nil || h[0] != 0xff || h[1]&0xe0 != 0xe0 || h[1]>>3&3 == 1 || h[1]>>1&3 != 1 || h[2]>>4 == 0 || h[2]>>4 == 15 || h[2]>>2&3 == 3 || h[3]&3 == 2 {
			return false
		}
		version := h[1] >> 3 & 3
		bitrate := [...]int64{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}[h[2]>>4]
		if version != 3 {
			bitrate = [...]int64{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}[h[2]>>4]
		}
		rate := [...]int64{44100, 48000, 32000}[h[2]>>2&3]
		coefficient := int64(144)
		if version == 2 {
			rate /= 2
			coefficient = 72
		} else if version == 0 {
			rate /= 4
			coefficient = 72
		}
		n := coefficient*bitrate*1000/rate + int64(h[2]>>1&1)
		if n < 24 || n > v.size-offset {
			return false
		}
		offset += n
		frames++
	}
	return frames > 0 && offset == v.size
}
