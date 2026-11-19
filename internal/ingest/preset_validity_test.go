package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/hygiene"
	"github.com/alessiosavi/go-knowledge-graph/internal/validity"
)

func TestValidityPresetLoaders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invented.yaml")
	data, err := os.ReadFile("../validity/testdata/invented.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const labels = "entities: [{name: zorb, threshold: 0.5}]\nrelations: [{name: has_feature, threshold: 0.5}]\n"
	write := func(t *testing.T, block string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(labels+block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loaders := []struct {
		name string
		load func() (*validity.Lexicon, error)
	}{
		{"hygiene", func() (*validity.Lexicon, error) {
			p, err := hygiene.LoadPreset(dir, "invented", "")
			return p.Validity, err
		}},
		{"checked", func() (*validity.Lexicon, error) {
			p, err := LoadCheckedPreset(dir, "invented", "")
			return p.Hygiene.Validity, err
		}},
	}
	for _, block := range []string{"", string(data)} {
		write(t, block)
		for _, loader := range loaders {
			lexicon, err := loader.load()
			if err != nil || (lexicon == nil) != (block == "") {
				t.Fatalf("%s: validity = %+v, err = %v", loader.name, lexicon, err)
			}
			if lexicon != nil && (lexicon.Rules != validity.RulesID || lexicon.ProductTypes["zorbs"] != "zorb") {
				t.Fatalf("%s: unexpected lexicon %+v", loader.name, lexicon)
			}
		}
		if _, _, err := LoadPreset(dir, "invented"); err != nil {
			t.Fatal(err)
		}
	}

	fixture := string(data)
	invalid := []struct{ name, block, want string }{
		{"unknown field", fixture + "  typoz: []\n", "typoz"},
		{"unknown rules", strings.Replace(fixture, validity.RulesID, "zorb-v1", 1), "validity.rules"},
		{"missing rules", strings.Replace(fixture, "  rules: catalog-v3\n", "", 1), "validity.rules"},
		{"null block", "validity: null\n", "validity: null"},
		{"scalar block", "validity: zorb\n", "validity:"},
		{"empty product types", strings.Replace(fixture, "{zorbs: zorb, vexels: vexel, preludes: stoppon}", "{}", 1), "validity.product_types"},
		{"invalid product key", strings.Replace(fixture, "zorbs:", "Zorbs:", 1), "validity.product_types"},
		{"invalid product value", strings.Replace(fixture, "zorbs: zorb", "zorbs: 'zorb box'", 1), "validity.product_types"},
		{"invalid word", strings.Replace(fixture, "[gleamy]", "[gleamy-zorb]", 1), "validity.sensory_words"},
		{"invalid evaluative stem", strings.Replace(fixture, "[zestiv]", "[zestiv2]", 1), "validity.evaluative_stems"},
		{"invalid sensory stem", strings.Replace(fixture, "[brilliq]", "[Brilliq]", 1), "validity.sensory_stems"},
		{"null list element", strings.Replace(fixture, "[gleamy]", "[gleamy, null]", 1), "validity: null"},
	}
	for _, field := range []string{
		"product_types", "stop_tokens", "quality_nouns", "class_nouns", "evaluative_stems",
		"evaluative_words", "sensory_stems", "sensory_words", "function_words",
	} {
		prefix := "  " + field + ":"
		_, after, _ := strings.Cut(fixture, prefix)
		value, _, _ := strings.Cut(after, "\n")
		line := prefix + value + "\n"
		invalid = append(invalid, struct{ name, block, want string }{
			"missing " + field, strings.Replace(fixture, line, "", 1), "validity." + field,
		})
		if field != "product_types" {
			invalid = append(invalid, struct{ name, block, want string }{
				"empty " + field, strings.Replace(fixture, line, prefix+" []\n", 1), "validity." + field,
			})
		}
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			write(t, tc.block)
			for _, loader := range loaders {
				_, err := loader.load()
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("%s: error = %v, want %q", loader.name, err, tc.want)
				}
			}
			if _, _, err := LoadPreset(dir, "invented"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("labels: error = %v, want %q", err, tc.want)
			}
		})
	}
}
