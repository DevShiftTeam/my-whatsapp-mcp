package app

import (
	"bytes"
	"encoding/binary"
	"io"
)

// Dependency audit reads only the bounded Mach-O header/load-command region.
// Symbol/string-table counts cannot induce unbounded allocations in a general
// object-file parser. Fat binaries and ambient rpath authority are unsupported.
func boundedMachOLibraries(file io.ReaderAt, size int64, executable bool) ([]string, error) {
	var header [32]byte
	if size < 32 {
		return nil, errBundledAI
	}
	if n, err := file.ReadAt(header[:], 0); err != nil || n != len(header) {
		return nil, errBundledAI
	}
	order := binary.LittleEndian
	typeValue := uint32(6)
	if executable {
		typeValue = 2
	}
	commands, commandBytes := order.Uint32(header[16:20]), order.Uint32(header[20:24])
	if order.Uint32(header[:4]) != 0xfeedfacf || order.Uint32(header[4:8]) != 0x0100000c || order.Uint32(header[12:16]) != typeValue || commands > 4096 || commandBytes > 1<<20 || int64(commandBytes) > size-32 {
		return nil, errBundledAI
	}
	data := make([]byte, int(commandBytes))
	if n, err := file.ReadAt(data, 32); err != nil || n != len(data) {
		return nil, errBundledAI
	}
	result := []string{}
	position := 0
	dyldCount := 0
	for index := uint32(0); index < commands; index++ {
		if len(data)-position < 8 {
			return nil, errBundledAI
		}
		command, length := order.Uint32(data[position:position+4]), order.Uint32(data[position+4:position+8])
		if length < 8 || length%8 != 0 || uint64(length) > uint64(len(data)-position) {
			return nil, errBundledAI
		}
		block := data[position : position+int(length)]
		if command == 0x8000001c {
			return nil, errBundledAI
		} // LC_RPATH
		switch command {
		case 0xe:
			if !executable || len(block) < 16 || dyldCount != 0 {
				return nil, errBundledAI
			}
			offset := order.Uint32(block[8:12])
			if offset < 12 || uint64(offset) >= uint64(len(block)) {
				return nil, errBundledAI
			}
			name := block[int(offset):]
			end := bytes.IndexByte(name, 0)
			if end < 0 || string(name[:end]) != "/usr/lib/dyld" {
				return nil, errBundledAI
			}
			dyldCount++
		case 0xd:
			if executable || len(block) < 24 {
				return nil, errBundledAI
			}
			offset := order.Uint32(block[8:12])
			if offset < 24 || uint64(offset) >= uint64(len(block)) {
				return nil, errBundledAI
			}
			end := bytes.IndexByte(block[int(offset):], 0)
			if end <= 0 || end > 512 {
				return nil, errBundledAI
			}
		case 0x0c, 0x80000018, 0x8000001f, 0x20, 0x80000023:
			if len(block) < 24 {
				return nil, errBundledAI
			}
			offset := order.Uint32(block[8:12])
			if offset < 24 || uint64(offset) >= uint64(len(block)) {
				return nil, errBundledAI
			}
			name := block[int(offset):]
			end := bytes.IndexByte(name, 0)
			if end <= 0 || end > 512 || len(result) >= 128 {
				return nil, errBundledAI
			}
			result = append(result, string(name[:end]))
		case 0x19:
			if len(block) < 72 || uint64(len(block)) != 72+80*uint64(order.Uint32(block[64:])) {
				return nil, errBundledAI
			}
		case 0x2, 0x1b, 0x80000028:
			if len(block) != 24 {
				return nil, errBundledAI
			}
		case 0xb:
			if len(block) != 80 {
				return nil, errBundledAI
			}
		case 0x5:
			if len(block) < 16 {
				return nil, errBundledAI
			}
		case 0x22, 0x80000022:
			if len(block) != 48 {
				return nil, errBundledAI
			}
		case 0x24, 0x2a, 0x1d, 0x1e, 0x26, 0x29, 0x2b, 0x80000033, 0x80000034:
			if len(block) != 16 {
				return nil, errBundledAI
			}
		case 0x32:
			if len(block) < 24 || uint64(len(block)) != 24+8*uint64(order.Uint32(block[20:])) {
				return nil, errBundledAI
			}
		default:
			return nil, errBundledAI
		}
		position += int(length)
	}
	if position != len(data) || executable && dyldCount != 1 {
		return nil, errBundledAI
	}
	return result, nil
}
