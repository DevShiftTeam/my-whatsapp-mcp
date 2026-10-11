//go:build darwin || linux

package media

import (
	"bytes"
	"encoding/binary"
	"sort"
)

type mediaBox struct {
	kind       string
	start, end int64
}

func (v mediaVerification) box(offset, end int64) (mediaBox, bool) {
	h, err := v.read(offset, 8)
	if err != nil || end > v.size || end-offset < 8 {
		return mediaBox{}, false
	}
	n, header := int64(binary.BigEndian.Uint32(h[:4])), int64(8)
	if n == 1 {
		large, err := v.read(offset+8, 8)
		if err != nil {
			return mediaBox{}, false
		}
		wide := binary.BigEndian.Uint64(large)
		if wide > uint64(end-offset) {
			return mediaBox{}, false
		}
		n, header = int64(wide), 16
	} else if n == 0 {
		n = end - offset
	}
	if n < header || n > end-offset {
		return mediaBox{}, false
	}
	return mediaBox{kind: string(h[4:]), start: offset + header, end: offset + n}, true
}
func esDescriptor(p []byte) (byte, []byte, []byte, bool) {
	if len(p) < 2 {
		return 0, nil, nil, false
	}
	tag, n, index := p[0], 0, 1
	for count := 0; ; count++ {
		if index >= len(p) || count == 4 {
			return 0, nil, nil, false
		}
		b := p[index]
		index++
		n = n<<7 | int(b&127)
		if b&128 == 0 {
			break
		}
	}
	if n > len(p)-index {
		return 0, nil, nil, false
	}
	return tag, p[index : index+n], p[index+n:], true
}
func validAACConfig(p []byte) bool {
	if len(p) < 6 || !bytes.Equal(p[:4], []byte{0, 0, 0, 0}) {
		return false
	}
	tag, es, _, ok := esDescriptor(p[4:])
	if !ok || tag != 3 || len(es) < 3 {
		return false
	}
	index := 3
	flags := es[2]
	if flags&128 != 0 {
		index += 2
	}
	if flags&64 != 0 {
		if index >= len(es) {
			return false
		}
		index += 1 + int(es[index])
	}
	if flags&32 != 0 {
		index += 2
	}
	if index > len(es) {
		return false
	}
	tag, decoder, _, ok := esDescriptor(es[index:])
	if !ok || tag != 4 || len(decoder) < 13 || decoder[0] != 0x40 && decoder[0] != 0x66 && decoder[0] != 0x67 && decoder[0] != 0x68 || decoder[1]>>2 != 5 {
		return false
	}
	tag, config, _, ok := esDescriptor(decoder[13:])
	if !ok || tag != 5 || len(config) < 2 {
		return false
	}
	object, rate, channels := config[0]>>3, (config[0]&7)<<1|config[1]>>7, config[1]>>3&15
	return object == 2 && rate <= 12 && channels > 0 && channels <= 7 && config[1]&3 == 0
}
func (v mediaVerification) audioDescriptions(box mediaBox) bool {
	h, err := v.read(box.start, 8)
	if err != nil || !bytes.Equal(h[:4], []byte{0, 0, 0, 0}) {
		return false
	}
	count := binary.BigEndian.Uint32(h[4:])
	if count == 0 || count > 16 {
		return false
	}
	offset := box.start + 8
	for range count {
		entry, ok := v.box(offset, box.end)
		if !ok || entry.end-entry.start < 28 {
			return false
		}
		p, err := v.read(entry.start, 28)
		if err != nil || binary.BigEndian.Uint16(p[6:8]) == 0 || binary.BigEndian.Uint16(p[8:10]) != 0 || binary.BigEndian.Uint16(p[16:18]) == 0 || binary.BigEndian.Uint16(p[16:18]) > 8 || binary.BigEndian.Uint16(p[18:20]) == 0 || binary.BigEndian.Uint16(p[18:20]) > 32 || binary.BigEndian.Uint32(p[24:28])>>16 == 0 {
			return false
		}
		configured := false
		for childOffset := entry.start + 28; childOffset < entry.end; {
			child, ok := v.box(childOffset, entry.end)
			if !ok || child.end-child.start > 1<<20 {
				return false
			}
			config, err := v.read(child.start, child.end-child.start)
			if err != nil {
				return false
			}
			switch entry.kind {
			case "mp4a":
				configured = configured || child.kind == "esds" && validAACConfig(config)
			case "alac":
				configured = configured || child.kind == "alac" && len(config) >= 28
			case "Opus":
				configured = configured || child.kind == "dOps" && len(config) >= 11 && config[0] == 0 && config[1] > 0 && config[1] <= 8
			case "fLaC":
				configured = configured || child.kind == "dfLa" && len(config) >= 42
			}
			childOffset = child.end
		}
		if !configured {
			return false
		}
		offset = entry.end
	}
	return offset == box.end
}

type chunkRun struct{ first, samples, description uint32 }
type audioTrack struct {
	header, mediaHeader, handler, description, timing, chunkMap, sizes, offsets bool
	samples                                                                     uint32
	timedSamples                                                                uint64
	descriptions, lastChunk, maximumDescription, chunks, defaultSize            uint32
	sampleSizes                                                                 []uint32
	chunkOffsets                                                                []uint64
	runs                                                                        []chunkRun
}

func mediaRangeAt(data []mediaBox, offset uint64) (mediaBox, bool) {
	index := sort.Search(len(data), func(i int) bool { return uint64(data[i].end) > offset })
	if index == len(data) || offset < uint64(data[index].start) {
		return mediaBox{}, false
	}
	return data[index], true
}
func (v mediaVerification) sampleTable(box mediaBox, track *audioTrack, data []mediaBox) bool {
	n := box.end - box.start
	if n < 8 || n > 1<<20 {
		return false
	}
	p, err := v.read(box.start, n)
	if err != nil || !bytes.Equal(p[:4], []byte{0, 0, 0, 0}) {
		return false
	}
	count := binary.BigEndian.Uint32(p[4:8])
	switch box.kind {
	case "stts":
		if track.timing || count == 0 || uint64(count)*8+8 != uint64(len(p)) {
			return false
		}
		for offset := 8; offset < len(p); offset += 8 {
			if offset&8191 == 8 && v.ctx.Err() != nil {
				return false
			}
			samples, delta := binary.BigEndian.Uint32(p[offset:offset+4]), binary.BigEndian.Uint32(p[offset+4:offset+8])
			if samples == 0 || delta == 0 {
				return false
			}
			track.timedSamples += uint64(samples)
			if track.timedSamples > 1<<32-1 {
				return false
			}
		}
		track.timing = true
	case "stsc":
		if track.chunkMap || count == 0 || uint64(count)*12+8 != uint64(len(p)) {
			return false
		}
		track.runs = make([]chunkRun, 0, count)
		for offset := 8; offset < len(p); offset += 12 {
			if offset%12288 == 8 && v.ctx.Err() != nil {
				return false
			}
			first, samples, description := binary.BigEndian.Uint32(p[offset:offset+4]), binary.BigEndian.Uint32(p[offset+4:offset+8]), binary.BigEndian.Uint32(p[offset+8:offset+12])
			if samples == 0 || description == 0 || first <= track.lastChunk || offset == 8 && first != 1 {
				return false
			}
			track.lastChunk = first
			track.maximumDescription = max(track.maximumDescription, description)
			track.runs = append(track.runs, chunkRun{first, samples, description})
		}
		track.chunkMap = true
	case "stsz":
		if track.sizes || len(p) < 12 {
			return false
		}
		track.samples = binary.BigEndian.Uint32(p[8:12])
		if track.samples == 0 {
			return false
		}
		track.defaultSize = count
		if count == 0 {
			if uint64(track.samples)*4+12 != uint64(len(p)) {
				return false
			}
			track.sampleSizes = make([]uint32, 0, track.samples)
			for offset := 12; offset < len(p); offset += 4 {
				if offset&4095 == 12 && v.ctx.Err() != nil {
					return false
				}
				size := binary.BigEndian.Uint32(p[offset : offset+4])
				if size == 0 || int64(size) > v.size {
					return false
				}
				track.sampleSizes = append(track.sampleSizes, size)
			}
		} else if len(p) != 12 || int64(count) > v.size {
			return false
		}
		track.sizes = true
	case "stco", "co64":
		width := 8
		if box.kind == "stco" {
			width = 4
		}
		if track.offsets || count == 0 || uint64(count)*uint64(width)+8 != uint64(len(p)) {
			return false
		}
		track.chunkOffsets = make([]uint64, 0, count)
		for offset := 8; offset < len(p); offset += width {
			if (offset-8)/width%1024 == 0 && v.ctx.Err() != nil {
				return false
			}
			value := uint64(binary.BigEndian.Uint32(p[offset : offset+4]))
			if width == 8 {
				value = binary.BigEndian.Uint64(p[offset : offset+8])
			}
			if _, ok := mediaRangeAt(data, value); !ok {
				return false
			}
			track.chunkOffsets = append(track.chunkOffsets, value)
		}
		track.chunks = count
		track.offsets = true
	default:
		return false
	}
	return true
}
func (v mediaVerification) sampleExtents(track *audioTrack, data []mediaBox) bool {
	if len(track.runs) == 0 || len(track.chunkOffsets) == 0 {
		return false
	}
	sample, run := uint64(0), 0
	for index, offset := range track.chunkOffsets {
		if index%256 == 0 && v.ctx.Err() != nil {
			return false
		}
		for run+1 < len(track.runs) && track.runs[run+1].first <= uint32(index+1) {
			run++
		}
		count := uint64(track.runs[run].samples)
		if count > uint64(track.samples)-sample {
			return false
		}
		size := uint64(0)
		if track.defaultSize != 0 {
			size = count * uint64(track.defaultSize)
		} else {
			for i := uint64(0); i < count; i++ {
				if i%1024 == 0 && v.ctx.Err() != nil {
					return false
				}
				size += uint64(track.sampleSizes[sample+i])
				if size > uint64(v.size) {
					return false
				}
			}
		}
		part, ok := mediaRangeAt(data, offset)
		if !ok || size == 0 || size > uint64(part.end)-offset {
			return false
		}
		sample += count
	}
	return sample == uint64(track.samples)
}

func (v mediaVerification) trackBoxes(container mediaBox, track *audioTrack, data []mediaBox) bool {
	seenContainer := false
	for offset := container.start; offset < container.end; {
		box, ok := v.box(offset, container.end)
		if !ok {
			return false
		}
		n := box.end - box.start
		switch box.kind {
		case "tkhd", "mdhd":
			if box.kind == "tkhd" && (container.kind != "trak" || track.header) || box.kind == "mdhd" && (container.kind != "mdia" || track.mediaHeader) {
				return false
			}
			p, err := v.read(box.start, min(n, 32))
			if err != nil || len(p) < 24 || p[0] > 1 {
				return false
			}
			minimum := int64(24)
			if box.kind == "tkhd" {
				minimum = 84
			}
			if p[0] == 1 {
				minimum += 12
			}
			if n < minimum {
				return false
			}
			position := 12
			if p[0] == 1 {
				position = 20
			}
			if len(p) < position+4 || binary.BigEndian.Uint32(p[position:position+4]) == 0 {
				return false
			}
			if box.kind == "tkhd" {
				track.header = true
			} else {
				track.mediaHeader = true
			}
		case "hdlr":
			if container.kind != "mdia" || track.handler {
				return false
			}
			p, err := v.read(box.start, min(n, 24))
			if err != nil || len(p) != 24 || p[0] != 0 || string(p[8:12]) != "soun" {
				return false
			}
			track.handler = true
		case "stsd":
			if container.kind != "stbl" || track.description || !v.audioDescriptions(box) {
				return false
			}
			p, err := v.read(box.start+4, 4)
			if err != nil {
				return false
			}
			track.descriptions = binary.BigEndian.Uint32(p)
			track.description = true
		case "stts", "stsc", "stsz", "stco", "co64":
			if container.kind != "stbl" || !v.sampleTable(box, track, data) {
				return false
			}
		case "mdia", "minf", "stbl":
			expected := "mdia"
			if container.kind == "mdia" {
				expected = "minf"
			} else if container.kind == "minf" {
				expected = "stbl"
			}
			if container.kind == "stbl" || box.kind != expected || seenContainer || !v.trackBoxes(box, track, data) {
				return false
			}
			seenContainer = true
		}
		offset = box.end
	}
	return container.kind == "stbl" || seenContainer
}

func (v mediaVerification) m4a() bool {
	ftyp := false
	var moov *mediaBox
	var data []mediaBox
	for offset := int64(0); offset < v.size; {
		box, ok := v.box(offset, v.size)
		if !ok {
			return false
		}
		n := box.end - box.start
		switch box.kind {
		case "ftyp":
			if ftyp || n < 8 || n > 4096 || n%4 != 0 {
				return false
			}
			p, err := v.read(box.start, n)
			if err != nil {
				return false
			}
			supported := false
			for index := 0; index < len(p); index += 4 {
				if index == 4 {
					continue
				}
				switch string(p[index : index+4]) {
				case "M4A ", "M4B ", "isom", "iso2", "mp41", "mp42":
					supported = true
				}
			}
			if !supported {
				return false
			}
			ftyp = true
		case "mdat":
			if n == 0 || len(data) == 256 {
				return false
			}
			data = append(data, box)
		case "moov":
			if !ftyp || moov != nil {
				return false
			}
			copy := box
			moov = &copy
		}
		offset = box.end
	}
	if !ftyp || moov == nil || len(data) == 0 {
		return false
	}
	header, tracks := false, 0
	for offset := moov.start; offset < moov.end; {
		box, ok := v.box(offset, moov.end)
		if !ok {
			return false
		}
		switch box.kind {
		case "mvhd":
			n := box.end - box.start
			p, err := v.read(box.start, min(n, 24))
			if err != nil || len(p) < 16 || p[0] > 1 || header || n < 100+int64(p[0])*12 {
				return false
			}
			position := 12
			if p[0] == 1 {
				position = 20
			}
			if len(p) < position+4 || binary.BigEndian.Uint32(p[position:position+4]) == 0 {
				return false
			}
			header = true
		case "trak":
			track := audioTrack{}
			if !v.trackBoxes(box, &track, data) || !track.header || !track.mediaHeader || !track.handler || !track.description || !track.timing || !track.chunkMap || !track.sizes || !track.offsets || track.timedSamples != uint64(track.samples) || track.maximumDescription > track.descriptions || track.lastChunk > track.chunks || !v.sampleExtents(&track, data) {
				return false
			}
			tracks++
			if tracks > 16 {
				return false
			}
		}
		offset = box.end
	}
	return header && tracks > 0
}
