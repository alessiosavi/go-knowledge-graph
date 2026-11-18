package graph_test

import (
	"testing"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

var foldKeyV2Fixtures = []struct {
	raw, wantRaw, wantDisplay string
}{
	{"'Bob's'", "bob", "bob"},
	{"Bob's", "bob", "bob"},
	{"BOB’S", "bob", "bob"},
	{"(Bob's)", "bob", "bob"},
	{"Company's.", "company", "company"},
	{"James'", "james", "james"},
	{"'s", "s", ""},
	{"A&trade;", "a", "a"},
	{"A™", "a", "a"},
	{"A", "a", "a"},
	{"&notit;", "&notit", "&notit"},
	{"a⃝́", "a", "a"},
	{"की", "की", "की"},
	{"كُ", "كُ", "كُ"},
	{"1  2", "1 2", "1 2"},
	{"1.10", "1.10", "1.10"},
	{"11.0", "11.0", "11.0"},
	{`"C#"`, "c#", "c#"},
	{`"100%"`, "100%", "100%"},
	{"C", "c", "c"},
	{"C#", "c#", "c#"},
	{"C++", "c++", "c++"},
	{"AT&T", "at&t", "at&t"},
	{"ATT", "att", "att"},
	{"TCP/IP", "tcp/ip", "tcp/ip"},
	{"TCPIP", "tcpip", "tcpip"},
	{"4.5", "4.5", "4.5"},
	{"45", "45", "45"},
	{"1,000", "1,000", "1,000"},
	{"1.000", "1.000", "1.000"},
	{"S22", "s22", "s22"},
	{"S22+", "s22+", "s22+"},
	{"100%", "100%", "100%"},
	{"100", "100", "100"},
	{"1 2", "1 2", "1 2"},
	{"12", "12", "12"},
	{"Kılıç", "kılıc", "kılıc"},
	{"Kilic", "kilic", "kilic"},
	{"İ", "i", "i"},
	{"I", "i", "i"},
	{"re-sign", "resign", "resign"},
	{"resign", "resign", "resign"},
	{"X 100", "x100", "x100"},
	{"X100", "x100", "x100"},
	{"中 文", "中文", "中文"},
	{"中文", "中文", "中文"},
	{"👩🏽‍🚀✈️", "", ""},
	{"{[“(‘Bob’s’)”]}", "bob", "bob"},
	{"'\u0301Bob's'\u0301", "bob", "bob"},
	{"Bob’\u20dds\u0903", "bob", "bob"},
	{"\u0301Café", "cafe", "cafe"},
	{"a\u0903\u20dd\u0301", "a", "a"},
	{"a-\u0301b", "ab", "ab"},
	{"a \u0301b", "ab", "ab"},
	{"a'ीb", "ab", "ab"},
	{"a&NotEqualTilde;b", "ab", "ab"},
	{"1 .\u0301 2", "1 2", "1 2"},
	{"1\u03012", "1\u03012", "1\u03012"},
	{"a+\u0301b", "a+\u0301b", "a+\u0301b"},
	{"Ｃ＋＋", "c++", "c++"},
	{"A®©℠&reg;&copy;&#x2120;", "a", "a"},
	{"AT&amp;T", "at&t", "at&t"},
	{"&#65;&#x42;&#X43;", "abc", "abc"},
	{"&amp;trade;", "&trade", "&trade"},
	{"&notin;", "", ""},
	{"AT&ampT", "at&ampt", "at&ampt"},
	{"&#x;", "&#x", "&#x"},
	{"&#65z;", "&#65z", "&#65z"},
	{"1&semi;2&#59;3&#x3b;4", "1 2 3 4", "1 2 3 4"},
	{"1\xff\t.-\u00a02", "1 2", "1 2"},
	{"a\x00b", "ab", "ab"},
	{"١\t— ٢", "١ ٢", "١ ٢"},
}

func TestFoldKeyV2Fixtures(t *testing.T) {
	for _, tc := range foldKeyV2Fixtures {
		t.Run(tc.raw, func(t *testing.T) {
			if got := graph.FoldKeyV2(tc.raw); got != tc.wantRaw {
				t.Errorf("FoldKeyV2(%q) = %q, want %q", tc.raw, got, tc.wantRaw)
			}
			display := graph.DisplayName(tc.raw)
			if got := graph.FoldKeyV2(display); got != tc.wantDisplay {
				t.Errorf("FoldKeyV2(DisplayName(%q) = %q) = %q, want %q", tc.raw, display, got, tc.wantDisplay)
			}
		})
	}
}

func TestFoldKeyV2Pairs(t *testing.T) {
	pairs := []struct {
		left, right string
		equal       bool
	}{
		{"C", "C#", false}, {"C", "C++", false}, {"C#", "C++", false},
		{"AT&T", "ATT", false}, {"TCP/IP", "TCPIP", false}, {"4.5", "45", false},
		{"1,000", "1.000", false}, {"S22", "S22+", false}, {"100%", "100", false},
		{"1 2", "12", false}, {"Kılıç", "Kilic", false}, {"1.10", "11.0", false},
		{"A™", "A", true}, {"A&trade;", "A™", true}, {"İ", "I", true},
		{"re-sign", "resign", true}, {"X 100", "X100", true}, {"中 文", "中文", true},
	}
	for _, pair := range pairs {
		for _, display := range []bool{false, true} {
			left, right := pair.left, pair.right
			if display {
				left, right = graph.DisplayName(left), graph.DisplayName(right)
			}
			a, b := graph.FoldKeyV2(left), graph.FoldKeyV2(right)
			if (a == b) != pair.equal {
				t.Errorf("folds of %q and %q (DisplayName=%t) are %q and %q, want equal=%t", pair.left, pair.right, display, a, b, pair.equal)
			}
		}
	}
}

func TestFoldKeyV2Idempotence(t *testing.T) {
	for _, tc := range foldKeyV2Fixtures {
		inputs := []string{tc.raw}
		if display := graph.DisplayName(tc.raw); display != tc.raw {
			inputs = append(inputs, display)
		}
		for _, input := range inputs {
			once := graph.FoldKeyV2(input)
			if twice := graph.FoldKeyV2(once); twice != once {
				t.Errorf("idempotence counterexample for %q: %q -> %q", input, once, twice)
			}
		}
	}
}

func TestFoldKeyV2IdempotenceSmallAlphabet(t *testing.T) {
	alphabet := []string{"a", "1", "-", " ", "\u0301", "ी", "."}
	var walk func(string, int)
	walk = func(input string, remaining int) {
		once := graph.FoldKeyV2(input)
		if twice := graph.FoldKeyV2(once); twice != once {
			t.Errorf("idempotence counterexample for %q: %q -> %q", input, once, twice)
		}
		if remaining == 0 {
			return
		}
		for _, symbol := range alphabet {
			walk(input+symbol, remaining-1)
		}
	}
	walk("", 4)
}

func FuzzFoldKeyV2(f *testing.F) {
	for _, tc := range foldKeyV2Fixtures {
		f.Add(tc.raw)
		if display := graph.DisplayName(tc.raw); display != tc.raw {
			f.Add(display)
		}
	}
	f.Fuzz(func(t *testing.T, input string) {
		got := graph.FoldKeyV2(input)
		if !utf8.ValidString(got) || !norm.NFC.IsNormalString(got) {
			t.Fatalf("FoldKeyV2(%q) = %q, want valid NFC UTF-8", input, got)
		}
		if again := graph.FoldKeyV2(input); got != again {
			t.Fatalf("FoldKeyV2(%q) gave %q and %q", input, got, again)
		}
		runes := []rune(got)
		for i, r := range runes {
			if unicode.IsSpace(r) && (r != ' ' || i == 0 || i == len(runes)-1 || !unicode.IsDigit(runes[i-1]) || !unicode.IsDigit(runes[i+1])) {
				t.Fatalf("FoldKeyV2(%q) = %q, want only single spaces between digits", input, got)
			}
		}
	})
}
