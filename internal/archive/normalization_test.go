package archive

import (
	"errors"
	"strings"
	"testing"
)

func TestLiteralQueryNormalizesAndQuotesEveryLexicalTerm(t *testing.T) {
	for _, tc := range []struct {
		name, query, want string
	}{
		{"niqqud", "שָׁלוֹם ABC", `"שלום" AND "abc"`},
		{"Hebrew quote variants", "ג׳ון צה״ל", `"ג" AND "ון" AND "צה" AND "ל"`},
		{"English quote variants", "‘John’ “ABC”", `"john" AND "abc"`},
		{"Boolean syntax", "alpha OR beta NOT gamma AND delta", `"alpha" AND "or" AND "beta" AND "not" AND "gamma" AND "and" AND "delta"`},
		{"column prefix phrase syntax", `body:alpha* +"beta gamma" NEAR(delta, 2)`, `"body" AND "alpha" AND "beta" AND "gamma" AND "near" AND "delta" AND "2"`},
		{"SQL punctuation", `alpha'; DROP TABLE messages; --`, `"alpha" AND "drop" AND "table" AND "messages"`},
		{"emoji and punctuation separators", "alpha😀beta💬שלום--world", `"alpha" AND "beta" AND "שלום" AND "world"`},
		{"URL and number separators", "https://example.com/A 052-1234567", `"https" AND "example" AND "com" AND "a" AND "052" AND "1234567"`},
		{"combining marks inside term", "\u0301e\u0301 x\u0301y", "\"e\u0301\" AND \"x\u0301y\""},
		{"all number categories", "123 Ⅷ ²", `"123" AND "ⅷ" AND "²"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := literalQuery(tc.query)
			if err != nil {
				t.Fatalf("literalQuery(%q): %v", tc.query, err)
			}
			if got != tc.want {
				t.Errorf("literalQuery(%q) = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

func TestLiteralQueryRejectsNonLexicalAndUnboundedInput(t *testing.T) {
	for _, tc := range []struct {
		name, query string
	}{
		{"empty", ""},
		{"whitespace", " \t\n"},
		{"emoji only", "😀💬"},
		{"marks only", "\u0301\u064E\u05B8"},
		{"punctuation only", `*:() +"' ;--`},
		{"symbols only", "©€₹"},
		{"private use only", "\uE000"},
		{"invalid UTF-8", string([]byte{'a', 0xff})},
		{"oversize byte input", "a" + strings.Repeat(" ", 1024)},
		{"oversize multibyte input", "a" + strings.Repeat("😀", 256)},
		{"too many lexical terms", strings.Repeat("a ", 33)},
		{"too many punctuation-delimited terms", strings.Repeat("a-", 33)},
		{"too long English term", strings.Repeat("a", 129)},
		{"too long Hebrew term", strings.Repeat("א", 129)},
		{"too many combining runes in term", "a" + strings.Repeat("\u0301", 128)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := literalQuery(tc.query)
			if !errors.Is(err, ErrInvalidQuery) {
				t.Errorf("literalQuery returned %q, %v; want ErrInvalidQuery", got, err)
			}
			if got != "" {
				t.Errorf("invalid query produced FTS expression %q", got)
			}
		})
	}
}

func TestLiteralQueryAcceptsBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, query, want string
	}{
		{"exact byte limit", "a" + strings.Repeat(" ", 1023), `"a"`},
		{"exact term count", strings.Repeat("a ", 32), strings.TrimSuffix(strings.Repeat(`"a" AND `, 32), " AND ")},
		{"exact English rune limit", strings.Repeat("a", 128), `"` + strings.Repeat("a", 128) + `"`},
		{"exact Hebrew rune limit", strings.Repeat("א", 128), `"` + strings.Repeat("א", 128) + `"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := literalQuery(tc.query)
			if err != nil || got != tc.want {
				t.Errorf("literalQuery boundary: got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
