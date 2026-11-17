package reteval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func questionFile(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "questions.json")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadRejectsUnsupportedKinds(t *testing.T) {
	for _, kind := range []string{"entity", "bogus"} {
		t.Run(kind, func(t *testing.T) {
			set := QuestionSet{Schema: 1, Questions: []Question{{ID: "acme-question", Stratum: "single", Question: "Who owns Zorbo?", Gold: []Anchor{{Quote: "Acme owns Zorbo"}}, Kinds: []string{"chunk", kind}}}}
			_, err := Load(questionFile(t, set))
			if err == nil || !strings.Contains(err.Error(), "acme-question") || !strings.Contains(err.Error(), kind) {
				t.Fatalf("unsupported kind %q: err=%v", kind, err)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	base := Question{ID: "s", Stratum: "single", Question: "Who owns Zorbo?", Gold: []Anchor{{Quote: "Acme owns Zorbo"}}}
	for _, tc := range []struct {
		name   string
		mutate func(*QuestionSet)
	}{
		{"schema", func(s *QuestionSet) { s.Schema = 2 }},
		{"stratum", func(s *QuestionSet) { s.Questions[0].Stratum = "other" }},
		{"single-gold", func(s *QuestionSet) { s.Questions[0].Gold = nil }},
		{"multi-gold", func(s *QuestionSet) { s.Questions[0].Stratum = "multi"; s.Questions[0].Gold = nil }},
		{"global-items", func(s *QuestionSet) { s.Questions[0].Stratum = "global" }},
		{"null-absent", func(s *QuestionSet) { s.Questions[0].Stratum = "null" }},
		{"duplicate", func(s *QuestionSet) { s.Questions = append(s.Questions, base) }},
		{"domain", func(s *QuestionSet) { s.Questions[0].Domain = "quillon" }},
		{"kind", func(s *QuestionSet) { s.Questions[0].Kinds = []string{"bogus"} }},
		{"empty-quote", func(s *QuestionSet) { s.Questions[0].Gold = []Anchor{{Quote: ""}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := QuestionSet{Schema: 1, Questions: []Question{base}}
			tc.mutate(&s)
			if _, err := Load(questionFile(t, s)); err == nil {
				t.Fatal("want validation error")
			}
		})
	}
	s, err := Load(questionFile(t, QuestionSet{Schema: 1, Questions: []Question{base}}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Questions[0].Kinds, []string{"chunk", "auto"}) {
		t.Fatalf("kinds = %v", s.Questions[0].Kinds)
	}
}

func TestLoadStrictJSON(t *testing.T) {
	for _, text := range []string{`{"schema":1,"questons":[]}`, `{"schema":1,"questions":[]} {}`} {
		p := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(p, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatalf("accepted %s", text)
		}
	}
}
