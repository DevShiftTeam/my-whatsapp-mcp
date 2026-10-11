package archive

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Bodokh/wa-indexer/internal/textnorm"
)

const LexicalNormalizationVersion = textnorm.LexicalNormalizationVersion

// NormalizeLexical creates a separate search field without changing the
// original body. Indexing and queries share the same versioned normalization.
func NormalizeLexical(text string) string {
	return textnorm.NormalizeLexical(text)
}

const (
	literalQueryMaxBytes     = 1024
	literalQueryMaxTerms     = 32
	literalQueryMaxTermRunes = 128
)

// literalQuery compiles bounded human text into a literal FTS5 conjunction.
// Punctuation and symbols separate terms; marks cannot start a term. No user
// input becomes an FTS operator, column selector, prefix, or SQL expression.
func literalQuery(query string) (string, error) {
	if len(query) > literalQueryMaxBytes || !utf8.ValidString(query) {
		return "", ErrInvalidQuery
	}

	var terms []string
	var term strings.Builder
	termRunes := 0
	flush := func() bool {
		if termRunes == 0 {
			return true
		}
		if len(terms) == literalQueryMaxTerms {
			return false
		}
		terms = append(terms, `"`+strings.ReplaceAll(term.String(), `"`, `""`)+`"`)
		term.Reset()
		termRunes = 0
		return true
	}
	for _, r := range NormalizeLexical(query) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) && termRunes > 0 {
			termRunes++
			if termRunes > literalQueryMaxTermRunes {
				return "", ErrInvalidQuery
			}
			term.WriteRune(r)
		} else if !flush() {
			return "", ErrInvalidQuery
		}
	}
	if !flush() || len(terms) == 0 {
		return "", ErrInvalidQuery
	}
	return strings.Join(terms, " AND "), nil
}
