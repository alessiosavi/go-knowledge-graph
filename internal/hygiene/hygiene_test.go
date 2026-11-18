package hygiene

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func testPreset(t *testing.T, name string) Preset {
	t.Helper()
	p, err := LoadPreset("../../labels", name, "")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValueLabels(t *testing.T) {
	for _, preset := range []string{"mail", "meeting"} {
		p := testPreset(t, preset)
		for _, label := range []string{"date", "amount", "organization"} {
			value := label == "date" || (label == "amount" && preset == "mail")
			for _, name := range []string{"2032-04-18", "18/04/2032", "1.200", "a", "--flag", "snake_case", "src/value.go"} {
				t.Run(preset+"/"+label+"/"+name, func(t *testing.T) {
					raw := extract.Mention{Text: name, Label: label}
					got := Apply(extract.ChunkResult{Mentions: []extract.Mention{raw}}, "", p)
					normalized, _, rule := NameOnly(name, label, p)
					if (rule == "") != value || (value && normalized != name) {
						t.Errorf("NameOnly value=%t: name=%q rule=%q", value, normalized, rule)
					}
					if (len(got.Mentions) == 1) != value {
						t.Errorf("value=%t: %+v", value, got)
					}
				})
			}
		}
		for _, tt := range []struct {
			name string
			rule Rule
		}{
			{"20\n32", "newline"}, {strings.Repeat("1", 61), "too_long"},
			{"[18](http://x)", "markdown_link"}, {"## Date", "heading_line"}, {"Descrizione", "stoplist"},
		} {
			got := Apply(extract.ChunkResult{Mentions: []extract.Mention{{Text: tt.name, Label: "date"}}}, tt.name, p)
			if len(got.Mentions) != 0 || got.Dropped[tt.rule] != 1 {
				t.Errorf("value %q must still drop under %s: %+v", tt.name, tt.rule, got)
			}
		}
		got := Apply(extract.ChunkResult{Mentions: []extract.Mention{{Text: "  l’Acme Ltd  ", Label: "date"}}}, "", p)
		if len(got.Mentions) != 1 || got.Mentions[0].Name != "Acme" {
			t.Errorf("value normalization: %+v", got)
		}
	}
}

func TestDropRules(t *testing.T) {
	p := testPreset(t, "general")
	p.Allow["QX"] = true
	overlay := filepath.Join(t.TempDir(), "stop.txt")
	if err := os.WriteFile(overlay, []byte("# private overlay\nZorbo boilerplate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	withOverlay, err := LoadPreset("../../labels", "general", overlay)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		rule  Rule
		names []string
	}{
		{"newline", []string{"Acme\nMax", "Acme\rMax"}},
		{"too_long", []string{strings.Repeat("é", 61)}},
		{"no_letters", []string{"12-34", "..."}},
		{"too_short", []string{"ab", "ai"}},
		{"cli_flag", []string{"-v", "--force", "-flag"}},
		{"markdown_link", []string{"[Acme](http://x)", "(#intro)"}},
		{"stoplist", []string{"Descrizione", "INDICE", "Sezione 3", "Section 12", "Déscrizione"}},
		{"path_like", []string{"src/acme.go", `C:\x`}},
		{"code_like", []string{"`x`", "f()", "{a}", "a=b", "a;b", "snake_case"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.rule), func(t *testing.T) {
			for _, name := range tt.names {
				t.Run(name, func(t *testing.T) {
					_, _, rule := NameOnly(name, "organization", p)
					if rule != tt.rule {
						t.Errorf("NameOnly(%q) dropped=%q, want %q", name, rule, tt.rule)
					}
					raw := extract.Mention{Label: "organization", Text: name, Start: 2, End: 2 + len(name), Score: .8}
					r := Apply(extract.ChunkResult{ChunkID: "chunk", Mentions: []extract.Mention{raw}}, "A sentence about "+name, p)
					if len(r.Mentions) != 0 || r.Dropped[tt.rule] != 1 {
						t.Errorf("Apply(%q) = %+v", name, r)
					}
				})
			}
		})
	}
	for _, name := range []string{strings.Repeat("é", 60), "AI", "EU", "UK", "US", "UE", "IT", "QX"} {
		if _, _, rule := NameOnly(name, "organization", p); rule != "" {
			t.Errorf("NameOnly(%q) dropped=%q, want kept", name, rule)
		}
	}
	if _, _, rule := NameOnly("Zorbo boilerplate", "organization", withOverlay); rule != "stoplist" {
		t.Errorf("overlay rule=%q", rule)
	}
	code := testPreset(t, "code")
	// Short and no-letter rules still apply to the code preset.
	for _, name := range []string{"src/acme.go", `C:\x`, "`x`", "f()", "{a}", "a=b", "a;b", "snake_case"} {
		if _, _, rule := NameOnly(name, "organization", code); rule != "" {
			t.Errorf("code NameOnly(%q) dropped=%q", name, rule)
		}
	}
}

func TestHeadingLine(t *testing.T) {
	p := testPreset(t, "general")
	for _, name := range []string{"Zorbo Pumps", "## Zorbo Pumps"} {
		text := "Before\n## Zorbo Pumps\nAfter"
		start := strings.Index(text, name)
		r := extract.ChunkResult{Mentions: []extract.Mention{{Text: name, Label: "product", Start: start, End: start + len(name)}}}
		got := Apply(r, text, p)
		if len(got.Mentions) != 0 || got.Dropped["heading_line"] != 1 {
			t.Errorf("heading %q: %+v", name, got)
		}
		got = Apply(r, "We discuss "+name+" in this sentence.", p)
		if len(got.Mentions) != 1 {
			t.Errorf("sentence %q: %+v", name, got)
		}
		if _, _, rule := NameOnly(name, "organization", p); rule != "" {
			t.Errorf("NameOnly(%q) dropped=%q", name, rule)
		}
	}
}

func TestNormalize(t *testing.T) {
	p := testPreset(t, "general")
	tests := []struct {
		raw, want string
		aliases   []string
	}{
		{"Zorbo’s", "Zorbo's", nil}, {"“Zorbo”", `"Zorbo"`, nil},
		{"l'Acme", "Acme", nil}, {"l’Acme", "Acme", nil}, {"l'Ax", "l'Ax", nil},
		{"  Acme\t Max  ", "Acme Max", nil},
		// A possessive company form keys like its plain form.
		{"Quillon Ltd's", "Quillon", []string{"Quillon Ltd's"}},
		{"Quillon Ltd’s", "Quillon", []string{"Quillon Ltd's"}},
		{"`Quillon Ltd`", "Quillon", []string{"`Quillon Ltd`"}},
		{"**Quillon Ltd**", "Quillon", []string{"**Quillon Ltd**"}},
		{"Quillon Ltd,", "Quillon", []string{"Quillon Ltd,"}},
	}
	for _, prefix := range []string{"un'", "d'", "dell'", "dall'", "all'", "nell'", "sull'", "dagl'", "degl'", "negl'", "sugl'", "quell'"} {
		tests = append(tests, struct {
			raw, want string
			aliases   []string
		}{prefix + "Acme", "Acme", nil})
	}
	for _, suffix := range []string{"S.r.l.", "Srl", "Srl.", "s.R.L.", "sRL", "S.p.A", "S.p.A.", "S.P.A", "S.P.A.", "SpA", "SpA.", "SPA", "SPA.", "Ltd", "Ltd.", "lTD", "Inc", "Inc.", "iNC", "GmbH", "GmbH.", "gMBh", "LLC", "LLC.", "llc"} {
		raw := "Quillon " + suffix
		tests = append(tests, struct {
			raw, want string
			aliases   []string
		}{raw, "Quillon", []string{raw}})
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			// Raw code-like mentions remain disallowed outside the code preset.
			p := p
			if strings.Contains(tt.raw, "`") {
				p = testPreset(t, "code")
			}
			name, aliases, rule := NameOnly(tt.raw, "organization", p)
			if name != tt.want || !reflect.DeepEqual(aliases, tt.aliases) || rule != "" {
				t.Errorf("NameOnly=%q %v %q; want %q %v kept", name, aliases, rule, tt.want, tt.aliases)
			}
			if strings.Contains(tt.raw, "Quillon") && graph.EntityKey(name) != "quillon" {
				t.Errorf("normalized key=%q, want quillon", graph.EntityKey(name))
			}
			raw := extract.Mention{Text: tt.raw, Label: "organization", Start: 4, End: 4 + len(tt.raw), Score: .9}
			got := Apply(extract.ChunkResult{ChunkID: "c", Mentions: []extract.Mention{raw}}, "about "+tt.raw, p)
			if len(got.Mentions) != 1 {
				t.Fatalf("Apply=%+v", got)
			}
			m := got.Mentions[0]
			if m.Mention != raw || m.Name != name || !reflect.DeepEqual(m.Aliases, aliases) || got.ChunkID != "c" {
				t.Errorf("raw spans or normalized name changed: %+v", got)
			}
			wantRenamed := 0
			if name != tt.raw {
				wantRenamed = 1
			}
			if got.Renamed != wantRenamed {
				t.Errorf("Renamed=%d, want %d", got.Renamed, wantRenamed)
			}
		})
	}
}

func TestCompanySuffixNormalize(t *testing.T) {
	p := testPreset(t, "general")
	for _, tt := range []struct {
		raw, want string
	}{
		{"Zorbo Resort & Spa", "Zorbo Resort & Spa"},
		{"Zorbo Hotel & SPA", "Zorbo Hotel & SPA"},
		{"Zorbo Hotel & SpA", "Zorbo Hotel & SpA"},
		{"Zorbo Hotel & S.p.A.", "Zorbo Hotel & S.p.A."},
		{"Zorbo Hotel & Ltd", "Zorbo Hotel & Ltd"},
		{"Acme Spa", "Acme Spa"},
		{"Acme spa", "Acme spa"},
		{"Acme SPa", "Acme SPa"},
		{"Acme Spa.", "Acme Spa."},
		{"Acme spa.", "Acme spa."},
		{"Acme S.p.a.", "Acme"},
		{"Acme s.p.a.", "Acme"},
		{"Acme sPa", "Acme sPa"},
		{"Quillon Packaging,Inc", "Quillon Packaging"},
		{"Acme,SpA", "Acme"},
		{"Quillon Packaging Co., Ltd", "Quillon Packaging"},
		{"Quillon Co Ltd", "Quillon Co"},
		{"Quillon Co. Ltd", "Quillon"},
		{"Quillon cO., lTd.", "Quillon"},
		{"Quillon Co., Inc.", "Quillon"},
		{"Quillon Co., LLC", "Quillon"},
		{"Quillon Co., GmbH", "Quillon"},
		{"Quillon, Co., Ltd", "Quillon"},
		{"Quillon Packaging, Ltd", "Quillon Packaging"},
		{"Quillon Alco Ltd", "Quillon Alco"},
		{"Quillon co-op Ltd", "Quillon co-op"},
		{"Quillon Co., Ltd's", "Quillon"},
		{"**Quillon Co., Ltd**", "Quillon"},
		{"Smith & Co. Ltd", "Smith & Co."},
		{"Foo & Co., Ltd", "Foo & Co."},
		{"Acme & Co. GmbH", "Acme & Co."},
		{"Acme & Co", "Acme & Co"},
		{"Acme Co.", "Acme Co."},
		{"Acme Co. Srl", "Acme Co."},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			name, aliases, rule := NameOnly(tt.raw, "organization", p)
			var wantAliases []string
			if tt.want != tt.raw {
				wantAliases = []string{tt.raw}
			}
			if name != tt.want || !reflect.DeepEqual(aliases, wantAliases) || rule != "" {
				t.Errorf("NameOnly=%q %v %q; want %q %v kept", name, aliases, rule, tt.want, wantAliases)
			}
		})
	}
}

func TestApplyCompanySuffix(t *testing.T) {
	text := "Zorbo Resort & Spa uses Quillon Packaging Co., Ltd."
	resort := extract.Mention{Text: "Zorbo Resort & Spa", Label: "organization", Start: 0, End: len("Zorbo Resort & Spa"), Score: .9}
	company := extract.Mention{Text: "Quillon Packaging Co., Ltd", Label: "organization", Start: strings.Index(text, "Quillon"), End: len(text) - 1, Score: .8}
	candidate := extract.Candidate{Head: 0, Tail: 1, Label: "uses", Score: .8}
	raw := extract.ChunkResult{ChunkID: "c", Mentions: []extract.Mention{resort, company}, Candidates: []extract.Candidate{candidate}}
	got := Apply(raw, text, testPreset(t, "general"))
	want := []Mention{
		{Mention: resort, Name: "Zorbo Resort & Spa"},
		{Mention: company, Name: "Quillon Packaging", Aliases: []string{"Quillon Packaging Co., Ltd"}},
	}
	if !reflect.DeepEqual(got.Mentions, want) || !reflect.DeepEqual(got.Candidates, []extract.Candidate{candidate}) || got.Renamed != 1 || len(got.Dropped) != 0 || got.ChunkID != "c" {
		t.Errorf("Apply=%+v; want mentions=%+v, unchanged candidate, one rename and no drops", got, want)
	}
	if !reflect.DeepEqual(raw.Mentions, []extract.Mention{resort, company}) {
		t.Error("Apply mutated provider mentions")
	}
}

func TestSuffixOnlyNames(t *testing.T) {
	p := testPreset(t, "general")
	for _, name := range []string{"Srl", "Srl.", "S.R.L.", "SpA", "SpA.", "S.p.A.", "Ltd", "Ltd.", "Inc", "Inc.", "GmbH", "GmbH.", "LLC", "LLC."} {
		got, aliases, rule := NameOnly(name, "organization", p)
		if got != name || len(aliases) != 0 || rule != "" {
			t.Errorf("suffix-only %q: name=%q aliases=%v rule=%q", name, got, aliases, rule)
		}
	}
}

func TestRuleOrderAndFactReindex(t *testing.T) {
	p := testPreset(t, "general")
	for _, tt := range []struct {
		raw  string
		rule Rule
	}{
		{"--force\nAcme", "newline"}, {"--force", "cli_flag"}, {"[Acme](http://x)", "markdown_link"},
		{"l'Descrizione", "stoplist"}, {"ab Ltd", "too_short"}, {strings.Repeat("A", 60) + " Ltd", ""},
	} {
		name, _, rule := NameOnly(tt.raw, "organization", p)
		if rule != tt.rule {
			t.Errorf("%q dropped=%q, want %q", tt.raw, rule, tt.rule)
		}
		if rule == "" && graph.EntityKey(name) == "" {
			t.Errorf("empty entity key for %q", name)
		}
	}
	r := extract.ChunkResult{ChunkID: "c", Mentions: []extract.Mention{
		{Text: "Descrizione", Label: "document"}, {Text: "l'Acme", Label: "organization", Start: 10, End: 16}, {Text: "Quillon Ltd", Label: "organization", Start: 20, End: 31},
	}, Candidates: []extract.Candidate{
		{Head: 0, Tail: 1, Label: "uses", Score: .9}, {Head: 1, Tail: 0, Label: "uses", Score: .9}, {Head: 1, Tail: 2, Label: "uses", Score: .8},
		{Head: -1, Tail: 2, Label: "uses"}, {Head: 1, Tail: 3, Label: "uses"},
	}}
	before := append([]extract.Mention(nil), r.Mentions...)
	got := Apply(r, "about names", p)
	if len(got.Mentions) != 2 || len(got.Candidates) != 1 || got.Candidates[0] != (extract.Candidate{Head: 0, Tail: 1, Label: "uses", Score: .8}) || got.Dropped["stoplist"] != 1 || got.Renamed != 2 {
		t.Fatalf("Apply=%+v", got)
	}
	if got.Mentions[got.Candidates[0].Head].Text != "l'Acme" || got.Mentions[got.Candidates[0].Tail].Text != "Quillon Ltd" {
		t.Error("reindexed endpoints no longer point at raw mentions")
	}
	if !reflect.DeepEqual(before, r.Mentions) {
		t.Error("Apply mutated provider result")
	}
	empty := Apply(extract.ChunkResult{Mentions: []extract.Mention{{Text: "'s", Label: "product"}}}, "", Preset{Allow: map[string]bool{"'s": true}})
	if len(empty.Mentions) != 0 {
		t.Errorf("empty EntityKey was retained: %+v", empty)
	}
}

func TestLoadPreset(t *testing.T) {
	for _, name := range []string{"", "general", "code", "documentation", "demo", "mail", "meeting", "news"} {
		p := testPreset(t, name)
		if p.Code != (name == "code" || name == "documentation") {
			t.Errorf("%q code=%t", name, p.Code)
		}
		if _, _, rule := NameOnly("INDICE", "organization", p); rule != "stoplist" {
			t.Errorf("%q stoplist=%q", name, rule)
		}
		if _, _, rule := NameOnly("AI", "organization", p); rule != "" {
			t.Errorf("%q AI=%q", name, rule)
		}
	}
	dir := t.TempDir()
	data := "allowlist: [QX]\nstoplist:\n  exact: [Zorbo boilerplate]\n  patterns: ['^Chapter [0-9]+$']\n"
	if err := os.WriteFile(filepath.Join(dir, "custom.yaml"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPreset(dir, "custom", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Zórbo boilerplate", "CHAPTER 42"} {
		if _, _, rule := NameOnly(name, "organization", p); rule != "stoplist" {
			t.Errorf("custom %q=%q", name, rule)
		}
	}
	if _, _, rule := NameOnly("QX", "organization", p); rule != "" {
		t.Errorf("allow QX=%q", rule)
	}
	for _, name := range []string{"../general", "missing"} {
		if _, err := LoadPreset(dir, name, ""); err == nil {
			t.Errorf("LoadPreset(%q) succeeded", name)
		}
	}
	if _, err := LoadPreset(dir, "custom", filepath.Join(dir, "absent")); err == nil {
		t.Error("missing overlay succeeded")
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("stoplist:\n  patterns: ['[']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPreset(dir, "bad", ""); err == nil {
		t.Error("invalid pattern succeeded")
	}
}

func TestCodeTokensSetting(t *testing.T) {
	dir := t.TempDir()
	for _, tt := range []struct {
		name, setting string
		want          bool
	}{
		{"custom", "code_tokens: true\n", true},
		{"code", "", false},
		{"documentation", "code_tokens: false\n", false},
	} {
		if err := os.WriteFile(filepath.Join(dir, tt.name+".yaml"), []byte(tt.setting), 0600); err != nil {
			t.Fatal(err)
		}
		p, err := LoadPreset(dir, tt.name, "")
		if err != nil {
			t.Fatal(err)
		}
		if p.Code != tt.want {
			t.Errorf("%s Code=%t, want %t", tt.name, p.Code, tt.want)
		}
		_, _, rule := NameOnly("buffer_batch_limit", "config_setting", p)
		if (rule == "") != tt.want {
			t.Errorf("%s code token dropped=%q", tt.name, rule)
		}
	}
	for _, name := range []string{"code", "documentation"} {
		p := testPreset(t, name)
		for _, token := range []string{"buffer_batch_limit", "src/value.go"} {
			if _, _, rule := NameOnly(token, "component", p); rule != "" {
				t.Errorf("%s %q dropped=%q", name, token, rule)
			}
		}
	}
}

func TestBuiltinPresetWithoutFiles(t *testing.T) {
	dir := t.TempDir()
	// Even an invalid named preset must be irrelevant to built-in defaults.
	if err := os.WriteFile(filepath.Join(dir, "general.yaml"), []byte("[invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPreset(dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Code {
		t.Error("built-in preset allows code tokens")
	}
	for _, name := range []string{"AI", "EU", "UK", "US", "UE", "IT"} {
		if _, _, rule := NameOnly(name, "organization", p); rule != "" {
			t.Errorf("%q dropped=%q", name, rule)
		}
	}
	// The built-in preset matches labels/general.yaml's generic words.
	for _, name := range []string{"Descrizione", "Informazioni", "Indice", "Introduzione", "Description", "Information", "Index", "Introduction", "Sezione 3", "Section 12"} {
		if _, _, rule := NameOnly(name, "document", p); rule != "stoplist" {
			t.Errorf("%q dropped=%q", name, rule)
		}
	}
	overlay := filepath.Join(dir, "stop.txt")
	if err := os.WriteFile(overlay, []byte("# private entries\n\nZorbo boilerplate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err = LoadPreset(filepath.Join(dir, "absent"), "", overlay)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, rule := NameOnly("Zórbo boilerplate", "document", p); rule != "stoplist" {
		t.Errorf("overlay dropped=%q", rule)
	}
	if _, err := LoadPreset(dir, "", filepath.Join(dir, "absent")); err == nil {
		t.Error("missing built-in overlay succeeded")
	}
	if _, err := LoadPreset(filepath.Join(dir, "absent"), "general", ""); err == nil {
		t.Error("named general without file succeeded")
	}
}

func TestHeadingLineKeepsSentenceOccurrence(t *testing.T) {
	p := testPreset(t, "general")
	text := "## Zorbo Pumps\nWe sell Zorbo Pumps today."
	title := extract.Mention{Text: "Zorbo Pumps", Label: "product", Start: 3, End: 14}
	sentence := extract.Mention{Text: "Zorbo Pumps", Label: "product", Start: 23, End: 34}
	got := Apply(extract.ChunkResult{Mentions: []extract.Mention{title, sentence}}, text, p)
	if len(got.Mentions) != 1 || got.Mentions[0].Mention != sentence || got.Dropped["heading_line"] != 1 {
		t.Fatalf("heading/sentence occurrences=%+v, want only the sentence span", got)
	}
}

func TestHeadingLineWithoutUsableSpan(t *testing.T) {
	p := testPreset(t, "general")
	text := "## Zorbo Pumps\nWe sell Zorbo Pumps today."
	for _, span := range [][2]int{{0, 0}, {-1, 10}, {0, 1000}, {0, 11}} {
		for _, name := range []string{"Zorbo Pumps", "## Zorbo Pumps"} {
			raw := extract.Mention{Text: name, Label: "product", Start: span[0], End: span[1]}
			got := Apply(extract.ChunkResult{Mentions: []extract.Mention{raw}}, text, p)
			if strings.HasPrefix(name, "#") {
				if len(got.Mentions) != 0 || got.Dropped["heading_line"] != 1 {
					t.Errorf("explicit heading: %+v", got)
				}
			} else if len(got.Mentions) != 1 || got.Mentions[0].Mention != raw || len(got.Dropped) != 0 {
				t.Errorf("unlocated prose mention: %+v", got)
			}
		}
	}
}

func TestEmptyKey(t *testing.T) {
	p := Preset{Allow: map[string]bool{"'s": true}, Values: map[string]bool{"date": true}}
	for _, raw := range []extract.Mention{{Text: "'s", Label: "organization"}, {Text: "...", Label: "date"}} {
		got := Apply(extract.ChunkResult{Mentions: []extract.Mention{raw}}, "", p)
		if len(got.Mentions) != 0 || got.Dropped["empty_key"] != 1 {
			t.Errorf("empty-key mention not accounted for: %+v", got)
		}
		if _, _, rule := NameOnly(raw.Text, raw.Label, p); rule != "empty_key" {
			t.Errorf("NameOnly(%q) rule=%q", raw.Text, rule)
		}
	}
}
