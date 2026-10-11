//go:build darwin || linux

package media

import (
	"bufio"
	"encoding/binary"
	"io"
	"math/bits"
)

func flacCRC8(p []byte) byte {
	var crc byte
	for _, b := range p {
		crc ^= b
		for range 8 {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

var flacCRC16Table = func() [256]uint16 {
	var table [256]uint16
	for i := range table {
		crc := uint16(i) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
		table[i] = crc
	}
	return table
}()

type flacStream struct {
	maxBlock, rate, channels, depth int
	samples                         uint64
}
type flacFrame struct {
	header, block, assignment int
	variable                  bool
	number                    uint64
}

func (v mediaVerification) flacHeader(offset int64, stream flacStream) (flacFrame, bool) {
	h, err := v.read(offset, min(int64(20), v.size-offset))
	if err != nil || len(h) < 8 || h[0] != 0xff || h[1]&0xfe != 0xf8 || h[2]>>4 == 0 || h[2]&15 == 15 || h[3]>>4 > 10 || h[3]&1 != 0 {
		return flacFrame{}, false
	}
	channel := int(h[3] >> 4)
	count := channel + 1
	if channel >= 8 {
		count = 2
	}
	if count != stream.channels {
		return flacFrame{}, false
	}
	depths := [...]int{stream.depth, 8, 12, 0, 16, 20, 24, 0}
	if depths[h[3]>>1&7] != stream.depth {
		return flacFrame{}, false
	}
	frame := flacFrame{assignment: channel, variable: h[1]&1 != 0}
	index := 5
	first := h[4]
	if first < 128 {
		frame.number = uint64(first)
	} else {
		count := bits.LeadingZeros8(^first)
		if count < 2 || count > 7 || len(h) < 4+count {
			return flacFrame{}, false
		}
		frame.number = uint64(first & (0x7f >> count))
		for i := 1; i < count; i++ {
			if h[4+i]&0xc0 != 0x80 {
				return flacFrame{}, false
			}
			frame.number = frame.number<<6 | uint64(h[4+i]&63)
		}
		minimum := [...]uint64{0, 0, 0x80, 0x800, 0x10000, 0x200000, 0x4000000, 0x80000000}
		if frame.number < minimum[count] {
			return flacFrame{}, false
		}
		index = 4 + count
	}
	if frame.number >= 1<<36 || !frame.variable && frame.number >= 1<<31 {
		return flacFrame{}, false
	}
	blockCode := h[2] >> 4
	switch {
	case blockCode == 1:
		frame.block = 192
	case blockCode >= 2 && blockCode <= 5:
		frame.block = 576 << uint(blockCode-2)
	case blockCode == 6:
		if index >= len(h) {
			return flacFrame{}, false
		}
		frame.block = int(h[index]) + 1
		index++
	case blockCode == 7:
		if index+2 > len(h) {
			return flacFrame{}, false
		}
		frame.block = int(binary.BigEndian.Uint16(h[index:index+2])) + 1
		index += 2
	default:
		frame.block = 256 << uint(blockCode-8)
	}
	rateCode := h[2] & 15
	rates := [...]int{stream.rate, 88200, 176400, 192000, 8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000}
	rate := 0
	if rateCode < 12 {
		rate = rates[rateCode]
	} else if rateCode == 12 {
		if index >= len(h) {
			return flacFrame{}, false
		}
		rate = int(h[index]) * 1000
		index++
	} else {
		if index+2 > len(h) {
			return flacFrame{}, false
		}
		rate = int(binary.BigEndian.Uint16(h[index : index+2]))
		index += 2
		if rateCode == 14 {
			rate *= 10
		}
	}
	if rate != stream.rate || frame.block > stream.maxBlock || index >= len(h)-2 || flacCRC8(h[:index]) != h[index] {
		return flacFrame{}, false
	}
	frame.header = index + 1
	return frame, true
}

type flacBits struct {
	reader    *bufio.Reader
	current   byte
	available uint
	consumed  int64
}

func (b *flacBits) take(n uint) (uint64, bool) {
	if n > 64 {
		return 0, false
	}
	var value uint64
	for n > 0 {
		if b.available == 0 {
			next, err := b.reader.ReadByte()
			if err != nil {
				return 0, false
			}
			b.current = next
			b.available = 8
			b.consumed++
		}
		count := min(n, b.available)
		shift := b.available - count
		mask := byte((uint16(1) << count) - 1)
		value = value<<count | uint64(b.current>>shift&mask)
		b.available -= count
		n -= count
	}
	return value, true
}
func (b *flacBits) skip(n uint64) bool {
	if n <= uint64(b.available) {
		_, ok := b.take(uint(n))
		return ok
	}
	n -= uint64(b.available)
	b.available = 0
	whole := n / 8
	if whole > uint64(^uint(0)>>1) {
		return false
	}
	count, err := b.reader.Discard(int(whole))
	b.consumed += int64(count)
	if err != nil {
		return false
	}
	_, ok := b.take(uint(n % 8))
	return ok
}
func (b *flacBits) unary(maximum uint64) (uint64, bool) {
	var count uint64
	for {
		if b.available == 0 {
			if _, ok := b.take(0); !ok {
				return 0, false
			}
			next, err := b.reader.ReadByte()
			if err != nil {
				return 0, false
			}
			b.current = next
			b.available = 8
			b.consumed++
		}
		zeros := min(uint(bits.LeadingZeros8(b.current<<uint(8-b.available))), b.available)
		if uint64(zeros) > maximum-count {
			return 0, false
		}
		count += uint64(zeros)
		b.available -= zeros
		if b.available != 0 {
			b.available--
			return count, true
		}
	}
}
func flacResidual(b *flacBits, block, order int) bool {
	method, ok := b.take(2)
	if !ok || method > 1 {
		return false
	}
	partitionOrder, ok := b.take(4)
	if !ok {
		return false
	}
	partitions := 1 << partitionOrder
	if block%partitions != 0 || block/partitions < order {
		return false
	}
	width := uint(4 + method)
	for partition := 0; partition < partitions; partition++ {
		count := block / partitions
		if partition == 0 {
			count -= order
		}
		parameter, ok := b.take(width)
		if !ok {
			return false
		}
		if parameter == (1<<width)-1 {
			raw, ok := b.take(5)
			if !ok || !b.skip(uint64(count)*raw) {
				return false
			}
			continue
		}
		for range count {
			if _, ok := b.unary(1 << 32); !ok || !b.skip(parameter) {
				return false
			}
		}
	}
	return true
}
func flacSubframe(b *flacBits, block, depth int) bool {
	padding, ok := b.take(1)
	if !ok || padding != 0 {
		return false
	}
	kind, ok := b.take(6)
	if !ok {
		return false
	}
	wasted, ok := b.take(1)
	if !ok {
		return false
	}
	if wasted != 0 {
		count, ok := b.unary(uint64(depth - 1))
		if !ok || int(count)+1 >= depth {
			return false
		}
		depth -= int(count) + 1
	}
	switch {
	case kind == 0:
		return b.skip(uint64(depth))
	case kind == 1:
		return b.skip(uint64(depth) * uint64(block))
	case kind >= 8 && kind <= 12:
		order := int(kind) - 8
		return order <= block && b.skip(uint64(order*depth)) && flacResidual(b, block, order)
	case kind >= 32:
		order := int(kind) - 31
		if order > block || !b.skip(uint64(order*depth)) {
			return false
		}
		precision, ok := b.take(4)
		if !ok || precision == 15 || !b.skip(5) || !b.skip(uint64(order)*(precision+1)) {
			return false
		}
		return flacResidual(b, block, order)
	}
	return false
}
func (v mediaVerification) flac() bool {
	offset := int64(4)
	var stream flacStream
	for index := 0; ; index++ {
		h, err := v.read(offset, 4)
		if err != nil || h[0]&127 > 6 {
			return false
		}
		n := int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3])
		if n > v.size-offset-4 || index == 0 && (h[0]&127 != 0 || n != 34) || index != 0 && h[0]&127 == 0 {
			return false
		}
		if index == 0 {
			info, err := v.read(offset+4, n)
			if err != nil || binary.BigEndian.Uint16(info[:2]) < 16 || binary.BigEndian.Uint16(info[:2]) > binary.BigEndian.Uint16(info[2:4]) {
				return false
			}
			packed := binary.BigEndian.Uint64(info[10:18])
			stream = flacStream{maxBlock: int(binary.BigEndian.Uint16(info[2:4])), rate: int(packed >> 44), channels: int(packed>>41&7) + 1, depth: int(packed>>36&31) + 1, samples: packed & ((1 << 36) - 1)}
			if stream.rate == 0 || stream.rate > 655350 || stream.depth < 4 || stream.depth > 32 {
				return false
			}
		}
		offset += 4 + n
		if h[0]&128 != 0 {
			break
		}
	}
	var samples, frames uint64
	reader := bufio.NewReaderSize(verificationReader{v.ctx, io.NewSectionReader(v.file, offset, v.size-offset)}, 64<<10)
	for offset < v.size {
		if err := v.ctx.Err(); err != nil {
			return false
		}
		frame, ok := v.flacHeader(offset, stream)
		if !ok || frame.variable && frame.number != samples || !frame.variable && frame.number != frames {
			return false
		}
		headerEnd := offset + int64(frame.header)
		if _, err := reader.Discard(frame.header); err != nil {
			return false
		}
		bits := flacBits{reader: reader}
		for channel := 0; channel < stream.channels; channel++ {
			depth := stream.depth
			if frame.assignment == 8 && channel == 1 || frame.assignment == 9 && channel == 0 || frame.assignment == 10 && channel == 1 {
				depth++
			}
			if !flacSubframe(&bits, frame.block, depth) {
				return false
			}
		}
		if padding, ok := bits.take(bits.available); !ok || padding != 0 {
			return false
		}
		if _, ok := bits.take(16); !ok {
			return false
		}
		end := headerEnd + bits.consumed
		if end <= offset || end > v.size {
			return false
		}
		var crc uint16
		for read := offset; read < end; {
			p, err := v.read(read, min(end-read, 64<<10))
			if err != nil {
				return false
			}
			for _, b := range p {
				crc = crc<<8 ^ flacCRC16Table[byte(crc>>8)^b]
			}
			read += int64(len(p))
		}
		if crc != 0 {
			return false
		}
		offset = end
		samples += uint64(frame.block)
		frames++
		if samples >= 1<<36 {
			return false
		}
	}
	return frames > 0 && (stream.samples == 0 || stream.samples == samples)
}
