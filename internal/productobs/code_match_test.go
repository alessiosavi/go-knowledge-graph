package productobs

import (
	"slices"
	"testing"
	"unicode/utf8"
)

func TestCodeKey(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"ＡＢ１２３４５", "AB12345"},
		{"ﬁ1234", "FI1234"},
		{"Q\u03011234", "Q\u03011234"},
		{"a .-_/+#\t\n\r\v\u00a0\u2003\u0085b12345", "AB12345"},
		{"A\x1cB12345", "A\x1cB12345"},
	} {
		if got := CodeKey(tc.input); got != tc.want {
			t.Errorf("CodeKey(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestEligibleCodeKey(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"12345", false},
		{"1234567", true},
		{"１２３４５６７", true},
		{"ABCDE", false},
		{"AB12", false},
		{"ÀB１２", false},
		{"A1-2-3-4", true},
		{"Q\u0301123", true},
		{"1234567\u0301", false},
		{"", false},
		// Runes outside the token alphabet can never equal a window key.
		{"AB:12345", false},
		{"2010066 (ref on package)", false},
		{"PR20156123;", false},
	} {
		key := CodeKey(tc.value)
		if got := EligibleCodeKey(key); got != tc.want {
			t.Errorf("EligibleCodeKey(%q) = %t, want %t", key, got, tc.want)
		}
	}
}

func TestMatchCodesSkipsEmptyKey(t *testing.T) {
	if got := MatchCodes(" . + # ", map[string]struct{}{"": {}}); len(got) != 0 {
		t.Errorf("MatchCodes returned empty-key matches: %+v", got)
	}
}

func TestMatchCodes(t *testing.T) {
	keys := map[string]struct{}{"AB12345": {}, "Q\u03011234": {}, "12345CD": {}, "FI1234": {}}
	for _, tc := range []struct {
		name, text string
		want       []CodeMatch
	}{
		{"exact", "code: AB12345!", []CodeMatch{{"AB12345", [2]int{6, 13}, "AB12345", 1}}},
		{"trailing period", "AB12345.", []CodeMatch{{"AB12345", [2]int{0, 8}, "AB12345.", 1}}},
		{"hyphen", "AB-12345", []CodeMatch{{"AB12345", [2]int{0, 8}, "AB-12345", 1}}},
		{"suffix", "ZZ-AB12345", nil},
		{"prefix", "AB12345X", nil},
		{"space", "AB 12345", []CodeMatch{{"AB12345", [2]int{0, 8}, "AB 12345", 2}}},
		{"spaces and tabs", "AB \t 123 45", []CodeMatch{{"AB12345", [2]int{0, 11}, "AB \t 123 45", 3}}},
		{"punctuation token", "AB - 12345", []CodeMatch{{"AB12345", [2]int{0, 10}, "AB - 12345", 3}}},
		{"punctuation token fills window", "AB - 123 45", nil},
		{"four tokens", "A B 123 45", nil},
		{"line feed", "AB\n12345", nil},
		{"carriage return", "AB\r12345", nil},
		{"other whitespace", "AB\u00a012345", nil},
		{"line separator", "AB\u202812345", nil},
		{"next line", "AB\u008512345", nil},
		{"vertical tab", "AB\v12345", nil},
		{"form feed", "AB\f12345", nil},
		{"comma", "AB, 12345", nil},
		{"invalid UTF-8", "AB12345\xffAB\xfe12345\x80ﬁ1234", []CodeMatch{
			{"AB12345", [2]int{0, 7}, "AB12345", 1},
			{"FI1234", [2]int{17, 24}, "ﬁ1234", 1},
		}},
		{"combining mark", "🦷:Q\u03011234", []CodeMatch{{"Q\u03011234", [2]int{5, 12}, "Q\u03011234", 1}}},
		{"fullwidth", "（ＡＢ１２３４５）", []CodeMatch{{"AB12345", [2]int{3, 24}, "ＡＢ１２３４５", 1}}},
		{"ligature", "ﬁ1234", []CodeMatch{{"FI1234", [2]int{0, 7}, "ﬁ1234", 1}}},
		{"overlapping windows", "AB 12345 CD", []CodeMatch{
			{"AB12345", [2]int{0, 8}, "AB 12345", 2},
			{"12345CD", [2]int{3, 11}, "12345 CD", 2},
		}},
		{"repeated windows", "AB12345, AB12345", []CodeMatch{
			{"AB12345", [2]int{0, 7}, "AB12345", 1},
			{"AB12345", [2]int{9, 16}, "AB12345", 1},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := MatchCodes(tc.text, keys)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("MatchCodes(%q) = %+v, want %+v", tc.text, got, tc.want)
			}
			for _, match := range got {
				s, e := match.Span[0], match.Span[1]
				if s < 0 || s >= e || e > len(tc.text) || !utf8.ValidString(tc.text[s:e]) || tc.text[s:e] != match.Text {
					t.Errorf("invalid original byte span: %+v in %q", match, tc.text)
				}
			}
		})
	}
}
