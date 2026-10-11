//go:build darwin || linux

package media

import "bytes"

// pdf only recognizes bounded header and EOF framing. It does not parse,
// decompress, render, sanitize, or execute PDF content. Other authenticated
// document bytes remain opaque documents, including malformed PDF-shaped data.
func (v mediaVerification) pdf() bool {
	if v.size < 16 {
		return false
	}
	header, err := v.read(0, 9)
	if err != nil || !bytes.HasPrefix(header, []byte("%PDF-")) ||
		(header[5] != '1' && header[5] != '2') || header[6] != '.' ||
		header[7] < '0' || header[7] > '9' || (header[8] != '\r' && header[8] != '\n') {
		return false
	}
	start := max(int64(0), v.size-1024)
	tail, err := v.read(start, v.size-start)
	if err != nil {
		return false
	}
	end := len(tail)
	for end > 0 && pdfWhitespace(tail[end-1]) {
		end--
	}
	if end < 5 || !bytes.Equal(tail[end-5:end], []byte("%%EOF")) {
		return false
	}
	marker := end - 5
	if marker > 0 {
		return tail[marker-1] == '\r' || tail[marker-1] == '\n'
	}
	if start == 0 {
		return false
	}
	previous, err := v.read(start-1, 1)
	return err == nil && (previous[0] == '\r' || previous[0] == '\n')
}

func pdfWhitespace(b byte) bool {
	return b == 0 || b == '\t' || b == '\n' || b == '\f' || b == '\r' || b == ' '
}
