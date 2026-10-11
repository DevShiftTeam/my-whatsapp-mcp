package textnorm

import "testing"

func TestNormalizeLexicalPreservesTextWhileRemovingHebrewMarks(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"niqqud", "שָׁלוֹם", "שלום"},
		{"single quotes", "ג׳ון ‘John’", "ג'ון 'john'"},
		{"double quotes", "צה״ל “ABC”", "צה\"ל \"abc\""},
		{"logical order and digits", "ABC שלום 052-1234567 https://example.com/A", "abc שלום 052-1234567 https://example.com/a"},
		{"final letters and prefixes", "בבית מלך מלכ דני דן", "בבית מלך מלכ דני דן"},
		{"non-Hebrew marks and emoji", "CAFÉ e\u0301 😀 عَرَبِيّ", "café e\u0301 😀 عَرَبِيّ"},
		{"Hebrew punctuation", "א־ב׀ג׃ד׆ה", "א־ב׀ג׃ד׆ה"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeLexical(tc.input); got != tc.want {
				t.Errorf("NormalizeLexical(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
