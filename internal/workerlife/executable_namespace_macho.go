package workerlife

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

// auditExecutableMachO permits a relocated thin ARM64 executable only when
// every loader dependency names an absolute system library. It audits raw load
// commands because debug/macho.ImportedLibraries omits weak/reexport variants.
// This checks the loader closure, not the behavior of trusted executable code.
func auditExecutableMachO(ctx context.Context, reader io.ReaderAt, fileBytes int64) error {
	if ctx == nil || namespaceNil(reader) {
		return ErrProtocol
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if fileBytes < 32 || fileBytes > 256<<20 {
		return ErrLimit
	}
	var header [32]byte
	if _, err := reader.ReadAt(header[:], 0); err != nil {
		return ErrUnavailable
	}
	order := binary.LittleEndian
	if order.Uint32(header[:]) != 0xfeedfacf || order.Uint32(header[4:]) != 0x0100000c || order.Uint32(header[12:]) != 2 {
		return ErrUnavailable
	}
	count, extent := order.Uint32(header[16:]), order.Uint32(header[20:])
	if count == 0 || count > 4096 || uint64(extent) < uint64(count)*8 || extent > 1<<20 || int64(extent) > fileBytes-32 {
		return ErrLimit
	}
	commands := make([]byte, int(extent))
	if _, err := reader.ReadAt(commands, 32); err != nil {
		return ErrUnavailable
	}
	offset, dyldCount := 0, 0
	for index := uint32(0); index < count; index++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(commands)-offset < 8 {
			return ErrUnavailable
		}
		command, size := order.Uint32(commands[offset:]), order.Uint32(commands[offset+4:])
		if size < 8 || size%8 != 0 || uint64(size) > uint64(len(commands)-offset) {
			return ErrUnavailable
		}
		data := commands[offset : offset+int(size)]
		switch command {
		case 0xc, 0x80000018, 0x8000001f, 0x20, 0x80000023:
			name, err := namespaceMachOString(data, 24)
			if err != nil || !namespaceSystemLibrary(name) {
				return ErrUnavailable
			}
		case 0xe: // LC_LOAD_DYLINKER, the only accepted interpreter.
			name, err := namespaceMachOString(data, 12)
			if err != nil || name != "/usr/lib/dyld" || dyldCount != 0 {
				return ErrUnavailable
			}
			dyldCount++
		case 0x19: // LC_SEGMENT_64: header + fixed-size section descriptors.
			if len(data) < 72 || uint64(len(data)) != 72+80*uint64(order.Uint32(data[64:])) {
				return ErrUnavailable
			}
		case 0x2: // LC_SYMTAB
			if len(data) != 24 {
				return ErrUnavailable
			}
		case 0xb: // LC_DYSYMTAB
			if len(data) != 80 {
				return ErrUnavailable
			}
		case 0x5: // LC_UNIXTHREAD; pinned Go uses this on older targets.
			if len(data) < 16 {
				return ErrUnavailable
			}
		case 0x1b: // LC_UUID
			if len(data) != 24 {
				return ErrUnavailable
			}
		case 0x22, 0x80000022: // LC_DYLD_INFO / LC_DYLD_INFO_ONLY
			if len(data) != 48 {
				return ErrUnavailable
			}
		case 0x80000028: // LC_MAIN
			if len(data) != 24 {
				return ErrUnavailable
			}
		case 0x24: // LC_VERSION_MIN_MACOSX
			if len(data) != 16 {
				return ErrUnavailable
			}
		case 0x32: // LC_BUILD_VERSION + build-tool records.
			if len(data) < 24 || uint64(len(data)) != 24+8*uint64(order.Uint32(data[20:])) {
				return ErrUnavailable
			}
		case 0x2a: // LC_SOURCE_VERSION
			if len(data) != 16 {
				return ErrUnavailable
			}
		case 0x1d, 0x1e, 0x26, 0x29, 0x2b, 0x80000033, 0x80000034:
			// Known linkedit-data commands contain offsets, not load paths:
			// signature, segment splits, function starts, data-in-code, code
			// sign DRs, exports trie and chained fixups.
			if len(data) != 16 {
				return ErrUnavailable
			}
		default:
			// This includes RPATH, DYLD_ENVIRONMENT, ID/prebound/subframework
			// commands, obsolete file inclusions, and unknown required loaders.
			return ErrUnavailable
		}
		offset += int(size)
	}
	if offset != len(commands) || dyldCount != 1 {
		return ErrUnavailable
	}
	return nil
}

func namespaceMachOString(command []byte, minimum int) (string, error) {
	if len(command) < minimum {
		return "", ErrUnavailable
	}
	offset := binary.LittleEndian.Uint32(command[8:])
	if uint64(offset) < uint64(minimum) || uint64(offset) >= uint64(len(command)) {
		return "", ErrUnavailable
	}
	body := command[int(offset):]
	end := bytes.IndexByte(body, 0)
	if end <= 0 || end > 4096 {
		return "", ErrUnavailable
	}
	for _, value := range body[end:] {
		if value != 0 {
			return "", ErrUnavailable
		}
	}
	name := string(body[:end])
	if !utf8.ValidString(name) || strings.IndexFunc(name, func(value rune) bool { return value < 0x20 || value == 0x7f }) >= 0 {
		return "", ErrUnavailable
	}
	return name, nil
}

func namespaceSystemLibrary(name string) bool {
	if path.Clean(name) != name {
		return false
	}
	return strings.HasPrefix(name, "/usr/lib/") || strings.HasPrefix(name, "/System/Library/Frameworks/")
}
