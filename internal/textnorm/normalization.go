// Package textnorm provides the versioned normalization shared by lexical
// indexing and search. Original logical-order text remains with callers.
package textnorm

import "strings"

const LexicalNormalizationVersion = "hebrew_marks_quotes_v1"

// NormalizeLexical creates a separate search field. Prefixes, final letters,
// names, and digits are preserved.
func NormalizeLexical(text string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x591 && r <= 0x5BD || r == 0x5BF || r >= 0x5C1 && r <= 0x5C2 || r >= 0x5C4 && r <= 0x5C5 || r == 0x5C7 {
			return -1
		}
		switch r {
		case 0x5F3, 0x2018, 0x2019:
			return '\''
		case 0x5F4, 0x201C, 0x201D:
			return '"'
		}
		return r
	}, strings.ToLower(text))
}
