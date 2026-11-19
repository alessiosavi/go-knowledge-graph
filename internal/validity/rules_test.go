package validity

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func readTestLexicon(t *testing.T, path string) Lexicon {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var preset struct {
		Validity yaml.Node `yaml:"validity"`
	}
	if err := yaml.Unmarshal(data, &preset); err != nil {
		t.Fatal(err)
	}
	lexicon, err := DecodeLexiconYAML(&preset.Validity)
	if err != nil {
		t.Fatal(err)
	}
	if lexicon == nil {
		t.Fatal("fixture lacks validity block")
	}
	return *lexicon
}

func compileTestRules(t *testing.T, lexicon Lexicon) *Rules {
	t.Helper()
	rules, err := NewRules(lexicon)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestDecide(t *testing.T) {
	rules := compileTestRules(t, readTestLexicon(t, "testdata/invented.yaml"))
	for _, tc := range []struct {
		name string
		in   Input
		want Decision
	}{
		{"title case", Input{Head: "Widget In Wool", Title: "Zorb"}, Decision{Keep, "head-not-lowerish"}},
		{"capitalized phrase", Input{Head: "Widget in wool", Title: "Widget in wool"}, Decision{Reattach, "ra-title-product"}},
		{"leading space before uppercase", Input{Head: " Widget in wool", Title: "Zorb"}, Decision{Keep, "head-not-lowerish"}},
		{"uppercase property", Input{Head: "widget Ⅳ", Title: "Zorb"}, Decision{Keep, "head-not-lowerish"}},
		{"python whitespace between words", Input{Head: "Widget\x1fwool", Title: "Widget Wool"}, Decision{Reattach, "ra-title-product"}},
		{"lowerish head", Input{Head: "widget", Title: "Widget in wool"}, Decision{Reattach, "ra-title-product"}},
		{"unrelated title", Input{Head: "widget", Title: "Zorb"}, Decision{Drop, "r1-containment"}},
		{"empty title", Input{Head: "widget", Title: "\u00a0\x1f\t"}, Decision{Keep, "empty-title"}},
		{"non-lowerish before empty title", Input{Head: "Widget"}, Decision{Keep, "head-not-lowerish"}},
		{"punctuation title", Input{Head: "widget", Title: "–"}, Decision{Drop, "r1-containment"}},
		{"head without tokens", Input{Head: "•", Title: "Zorb"}, Decision{Drop, "r1-containment"}},
		{"at threshold", Input{Head: "widget wool", Title: "Widget"}, Decision{Reattach, "ra-title-product"}},
		{"containment counts repeated tokens", Input{Head: "widget wool wool", Title: "Widget"}, Decision{Drop, "r1-containment"}},
		{"containment before is_a", Input{Head: "widget", Relation: "is_a", Title: "Zorb"}, Decision{Drop, "r1-containment"}},
		{"is_a before title product", Input{Head: "widget", Relation: "is_a", Title: "Widget"}, Decision{Keep, "is-a-keep"}},
		{"fold accents and apostrophes", Input{Head: "wídget’s wool", Title: "Widget's wool"}, Decision{Reattach, "ra-title-product"}},
		{"mapped product", Input{Head: "zorbs", Title: "Zorb"}, Decision{Reattach, "ra-title-product"}},
		{"product stop after mapping", Input{Head: "widget", Title: "Zorb preludes widget"}, Decision{Keep, "default-keep"}},
		{"made_of title fallback", Input{Head: "widget", Relation: "made_of", Title: "Zorb stoppon widget"}, Decision{Reattach, "made-of-reattach"}},
		{"made_of requires all head tokens", Input{Head: "widget wool", Relation: "made_of", Title: "Zorb stoppon widget"}, Decision{Keep, "default-keep"}},
		{"title product before made_of", Input{Head: "widget", Relation: "made_of", Title: "Widget"}, Decision{Reattach, "ra-title-product"}},
		{"dashed bullet before tail and casing", Input{Head: "Wídget Wool", Relation: "has_feature", Tail: "gleamy", Evidence: "  - widget wool"}, Decision{Drop, "bullet-line"}},
		{"star bullet", Input{Head: "widget wool", Title: "Widget Wool", Evidence: "  * widget wool"}, Decision{Drop, "bullet-line"}},
		{"numbered bullet", Input{Head: "widget wool", Title: "Widget Wool", Evidence: "  14. widget wool"}, Decision{Drop, "bullet-line"}},
		{"bullet symbol", Input{Head: "zorbs wool", Evidence: "\u00a0•\u202fZorb wool"}, Decision{Drop, "bullet-line"}},
		{"bullet must match all tokens", Input{Head: "widget wool", Title: "Widget Wool", Evidence: "  - widget wool zorb"}, Decision{Reattach, "ra-title-product"}},
		{"bullet needs whitespace", Input{Head: "widget wool", Title: "Widget Wool", Evidence: "-widget wool"}, Decision{Reattach, "ra-title-product"}},
		{"one word bullet", Input{Head: "widget", Title: "Widget", Evidence: "  - widget"}, Decision{Reattach, "ra-title-product"}},
		{"tail adjective before quality and casing", Input{Head: "Widget Wool", Relation: "has_feature", Tail: "vexel"}, Decision{Drop, "tail-adj"}},
		{"tail before empty title", Input{Head: "widget", Relation: "has_feature", Tail: "gleamy"}, Decision{Drop, "tail-adj"}},
		{"tail keeps uppercase", Input{Head: "widget", Relation: "has_feature", Tail: "Vexel", Title: "Widget"}, Decision{Reattach, "ra-title-product"}},
		{"tail keeps registered mark", Input{Head: "widget", Relation: "has_feature", Tail: "vexel®", Title: "Widget"}, Decision{Reattach, "ra-title-product"}},
		{"tail keeps trademark", Input{Head: "widget", Relation: "has_feature", Tail: "vexel™", Title: "Widget"}, Decision{Reattach, "ra-title-product"}},
		{"tail keeps ASCII digit", Input{Head: "widget", Relation: "has_feature", Tail: "vexel 3", Title: "Widget"}, Decision{Reattach, "ra-title-product"}},
		{"tail with non-ASCII digit", Input{Head: "widget", Relation: "has_feature", Tail: "vexel ٩", Title: "Widget"}, Decision{Drop, "tail-adj"}},
		{"tail ignores surface relation name", Input{Head: "widget", Relation: "Has Feature", Tail: "vexel", Title: "Widget"}, Decision{Reattach, "ra-title-product"}},
		{"tail uses unmapped tokens", Input{Head: "widget", Relation: "has_feature", Tail: "vexels", Title: "Widget"}, Decision{Reattach, "ra-title-product"}},
		{"tail quality with function words and adverbs", Input{Head: "widget", Relation: "is_a", Tail: "plim wib brilliqissimo zolmente zubly"}, Decision{Drop, "tail-quality"}},
		{"tail class", Input{Head: "widget", Relation: "made_of", Tail: "drindle zestivissima"}, Decision{Drop, "tail-class"}},
		{"tail class is_a", Input{Head: "widget", Relation: "is_a", Tail: "drindle zestiv"}, Decision{Drop, "tail-class"}},
		{"tail class requires evaluative", Input{Head: "widget", Relation: "is_a", Tail: "drindle gleamy", Title: "Widget"}, Decision{Keep, "is-a-keep"}},
		{"tail class only for is_a or made_of", Input{Head: "widget", Relation: "has_feature", Tail: "drindle zestivissima", Title: "Widget"}, Decision{Reattach, "ra-title-product"}},
		{"tail excludes short adverbs from stripping", Input{Head: "widget", Relation: "has_feature", Tail: "vexel zmente ably", Title: "Widget"}, Decision{Reattach, "ra-title-product"}},
		{"tail empty core", Input{Head: "widget", Relation: "has_feature", Tail: "wib zolmente zubly", Title: "Widget"}, Decision{Reattach, "ra-title-product"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rules.Decide(tc.in); got != tc.want {
				t.Errorf("Decide = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBulletPythonLineBreaks(t *testing.T) {
	rules := compileTestRules(t, readTestLexicon(t, "testdata/invented.yaml"))
	for _, sep := range []string{"\n", "\r", "\r\n", "\v", "\f", "\x1c", "\x1d", "\x1e", "\u0085", "\u2028", "\u2029"} {
		in := Input{Head: "widget wool", Evidence: "prefix" + sep + "\x1f٠٢)\u00a0widget wool" + sep + "suffix"}
		if got := rules.Decide(in); got != (Decision{Drop, "bullet-line"}) {
			t.Errorf("splitlines separator %q: Decide = %+v, want bullet-line drop", sep, got)
		}
	}
}

func TestTokensPythonNormalization(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{"İ-ﬁ-Ü-Ｚ-K", []string{"i", "fi", "u", "z", "k"}},
		{"ᴬˢ", []string{"s"}},                // Lowercasing occurs before compatibility decomposition.
		{"zo\u034frb", []string{"zo", "rb"}}, // A combining mark of class zero separates words.
		{"zo" + strings.Repeat("\u0301", 64) + "rb", []string{"zorb"}},
	} {
		if got := tokens(tc.input); !slices.Equal(got, tc.want) {
			t.Errorf("tokens(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestCatalogV3Parity(t *testing.T) {
	dir := os.Getenv("GKG_VALIDITY_PARITY_DIR")
	if dir == "" {
		t.Skip("set GKG_VALIDITY_PARITY_DIR to the private parity directory")
	}
	rules := compileTestRules(t, readTestLexicon(t, filepath.Join(dir, "catalog-validity.yaml")))
	file, err := os.Open(filepath.Join(dir, "parity-800.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var rows, matches int
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		rows++
		var row struct {
			Input
			Decision
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Errorf("line %d: decode parity row: %v", rows, err)
			continue
		}
		got := rules.Decide(row.Input)
		if got != row.Decision {
			t.Errorf("line %d: got %+v, want %+v", rows, got, row.Decision)
		} else {
			matches++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if rows != 800 {
		t.Errorf("read %d parity rows, want 800", rows)
	}
	t.Logf("matched action and rule on %d/%d rows", matches, rows)
}
