package domain

import (
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var testParams = Params{Tau: 0.05, Delta: 0.02, TauOverride: 0.15}

func TestFold(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"Caffè-Ünïon 3", "caffeunion3"},
		{"Acme-Max / Quillon Ltd", "acmemaxquillonltd"},
		{"ﬃ Ａ① K İ", "ffia1ki"},
		{"Straße Łódź Ω ı", "straeodz"},
		{"cafe\u0301", "cafe"},
		{"!!! 中文", ""},
		{"", ""},
	} {
		t.Run(tt.in, func(t *testing.T) {
			if got := Fold(tt.in); got != tt.want {
				t.Fatalf("Fold(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildProfileCountsDistinctChunksAfterFolding(t *testing.T) {
	got := BuildProfile(map[string][]string{
		"chunk-a": {"Caffè", "CAFFE", "Caffè", "Acme-Max", "!!!"},
		"chunk-b": {"caffe", "Acme Max", "Quillon Ltd"},
		"chunk-c": nil,
	})
	want := Profile{"caffe": 2, "acmemax": 2, "quillonltd": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildProfile = %v, want %v", got, want)
	}
	if got := BuildProfile(nil); len(got) != 0 {
		t.Fatalf("empty chunks = %v", got)
	}
}

func TestScoreSmoothedIDFAndLogTF(t *testing.T) {
	// N=3: idf(common)=1, idf(rare)=1+ln(2). The target's rare
	// weight is (1+ln(2))²; a's rare weight is 1+ln(2).
	scores := Score(Profile{"common": 1, "rare": 2}, []DocProfile{
		{Domain: "a", Profile: Profile{"common": 1, "rare": 1}},
		{Domain: "b", Profile: Profile{"common": 1}},
		{Domain: "c", Profile: Profile{"common": 1}},
	})
	rareIDF := 1 + math.Log(2)
	targetNorm := math.Sqrt(1 + math.Pow(rareIDF, 4))
	wantA := (1 + math.Pow(rareIDF, 3)) / (targetNorm * math.Sqrt(1+rareIDF*rareIDF))
	assertScores(t, scores, []DomainScore{{"a", wantA}, {"b", 1 / targetNorm}, {"c", 1 / targetNorm}})
}

func TestScoreIdenticalDocumentsAndSmoothing(t *testing.T) {
	// Every term occurs in every corpus document: smoothing must keep
	// idf=1, rather than erasing the entire vector with idf=0.
	profile := Profile{"acmemax": 2, "quillon": 1}
	assertScores(t, Score(profile, []DocProfile{
		{Domain: "zorbo", Profile: profile},
		{Domain: "acme", Profile: profile},
	}), []DomainScore{{"acme", 1}, {"zorbo", 1}})
}

func TestScoreMeanOfTopThree(t *testing.T) {
	assertScores(t, Score(Profile{"quillon": 1}, []DocProfile{
		{Domain: "a", Profile: Profile{"quillon": 1}},
		{Domain: "a", Profile: Profile{"quillon": 1}},
		{Domain: "a", Profile: Profile{}},
		{Domain: "a", Profile: Profile{}},
		{Domain: "b", Profile: Profile{"quillon": 1}},
	}), []DomainScore{{"b", 1}, {"a", 2.0 / 3}})
}

func TestScoreEmptyVectors(t *testing.T) {
	assertScores(t, Score(nil, []DocProfile{
		{Domain: "b", Profile: Profile{"quillon": 1}},
		{Domain: "a", Profile: Profile{}},
	}), []DomainScore{{"a", 0}, {"b", 0}})
	assertScores(t, Score(Profile{"quillon": 1}, []DocProfile{
		{Domain: "a", Profile: Profile{}},
	}), []DomainScore{{"a", 0}})
	if got := Score(Profile{"quillon": 1}, nil); len(got) != 0 {
		t.Fatalf("empty corpus = %v", got)
	}
}

func TestDecisionRule(t *testing.T) {
	target := DocProfile{DocID: "0123456789abcdef0123456789abcdef"}
	for _, tt := range []struct {
		name, action, domain string
		scores               []DomainScore
		params               Params
	}{
		{"attach", "attach", "a", []DomainScore{{"a", .30}, {"b", .05}}, testParams},
		{"margin fail", "new", "d-01234567", []DomainScore{{"a", .30}, {"b", .29}}, testParams},
		{"below tau", "new", "d-01234567", []DomainScore{{"a", .04}}, testParams},
		{"single domain", "attach", "a", []DomainScore{{"a", .30}}, testParams},
		{"empty corpus", "new", "d-01234567", nil, testParams},
		{"inclusive thresholds", "attach", "a", []DomainScore{{"a", .25}, {"b", .125}}, Params{Tau: .25, Delta: .125}},
		{"tie lowest id", "attach", "a", []DomainScore{{"a", .30}, {"b", .30}}, Params{Tau: .05, Delta: 0}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := decideScores(target, tt.scores, tt.params)
			if got.Action != tt.action || got.Domain != tt.domain || got.Params != tt.params {
				t.Fatalf("decision = %+v, want %s %s with %+v", got, tt.action, tt.domain, tt.params)
			}
			assertScores(t, got.Top, tt.scores)
		})
	}
}

func TestDecideExcludesTargetAndKeepsTopThree(t *testing.T) {
	target := DocProfile{DocID: "0123456789abcdef0123456789abcdef", Domain: "self", Profile: Profile{"quillon": 1}}
	corpus := []DocProfile{target}
	for _, id := range []string{"d", "c", "b", "a"} {
		corpus = append(corpus, DocProfile{DocID: id, Domain: id, Profile: target.Profile})
	}
	got := Decide(target, corpus, Params{Tau: .05, Delta: 0})
	if got.Action != "attach" || got.Domain != "a" {
		t.Fatalf("decision = %+v, want attach a", got)
	}
	assertScores(t, got.Top, []DomainScore{{"a", 1}, {"b", 1}, {"c", 1}})
	got = Decide(target, []DocProfile{target}, testParams)
	if got.Action != "new" || got.Domain != "d-01234567" {
		t.Fatalf("self-only corpus = %+v", got)
	}
}

func TestPrefixPrior(t *testing.T) {
	target := DocProfile{DocID: "0123456789abcdef0123456789abcdef", Source: "https://Docs.Acme.example/new", Profile: Profile{"zorbo": 1}}
	prior := []DocProfile{
		{DocID: "p1", Domain: "acme", Source: "https://docs.acme.example/one", Profile: Profile{"quillon": 1}},
		{DocID: "p2", Domain: "acme", Source: "https://docs.acme.example/two", Profile: Profile{"quillon": 1}},
	}
	t.Run("unanimous prior", func(t *testing.T) {
		got := Decide(target, prior, testParams)
		if got.Action != "prefix" || got.Domain != "acme" || got.Params != testParams {
			t.Fatalf("decision = %+v, want prefix acme", got)
		}
	})
	t.Run("override ignores attach margin and names both domains", func(t *testing.T) {
		corpus := append(append([]DocProfile{}, prior...),
			DocProfile{DocID: "z1", Domain: "zorbo", Source: "zorbo.txt", Profile: target.Profile},
			DocProfile{DocID: "z2", Domain: "zz", Source: "zz.txt", Profile: target.Profile})
		got := Decide(target, corpus, testParams)
		if got.Action != "attach" || got.Domain != "zorbo" || !strings.Contains(got.Note, "acme") || !strings.Contains(got.Note, "zorbo") {
			t.Fatalf("override = %+v", got)
		}
	})
	t.Run("override below threshold keeps prior", func(t *testing.T) {
		corpus := append(append([]DocProfile{}, prior...), DocProfile{DocID: "z1", Domain: "zorbo", Profile: target.Profile})
		got := Decide(target, corpus, Params{Tau: .05, Delta: .02, TauOverride: 1.01})
		if got.Action != "prefix" || got.Domain != "acme" {
			t.Fatalf("decision = %+v", got)
		}
	})
	t.Run("one document cannot supply a prior", func(t *testing.T) {
		if got := Decide(target, prior[:1], testParams); got.Action != "new" {
			t.Fatalf("decision = %+v", got)
		}
	})
	t.Run("mixed domains cannot supply a prior", func(t *testing.T) {
		mixed := append([]DocProfile{}, prior...)
		mixed[1].Domain = "quillon"
		if got := Decide(target, mixed, testParams); got.Action != "new" {
			t.Fatalf("decision = %+v", got)
		}
	})
	t.Run("detection agreeing with the prior stays prefix", func(t *testing.T) {
		agree := append([]DocProfile{}, prior...)
		agree[0].Profile = target.Profile
		got := Decide(target, agree, testParams)
		if got.Action != "prefix" || got.Domain != "acme" || got.Top[0].Score < testParams.TauOverride {
			t.Fatalf("decision = %+v, want prefix acme above the override threshold", got)
		}
	})
	t.Run("the target never counts toward its own prior", func(t *testing.T) {
		self := target
		self.Domain = "acme"
		if got := Decide(self, []DocProfile{self, prior[0]}, testParams); got.Action != "new" {
			t.Fatalf("decision = %+v, want new: one other document cannot supply a prior", got)
		}
	})
	t.Run("bare filenames have no prior", func(t *testing.T) {
		bare := append([]DocProfile{}, prior...)
		bare[0].Source, bare[1].Source = "a.txt", "b.txt"
		target := target
		target.Source = "c.txt"
		if got := Decide(target, bare, testParams); got.Action != "new" {
			t.Fatalf("decision = %+v", got)
		}
	})
}

func TestCanonicalPrefix(t *testing.T) {
	for _, tt := range []struct{ source, want string }{
		{"https://Docs.Acme.example/a/b", "docs.acme.example"},
		{"https://Docs.Acme.example:8443/a", "docs.acme.example"},
		{"https://user:pw@docs.acme.example/a", "docs.acme.example"},
		{"https://docs.acme.example/a?b=c/d", "docs.acme.example"},
		{"https://[::1]:8443/a", "::1"},
		{"https://docs.acme.example/a%zz", ""},
		{"ftp:///zorbo/x", ""},
		{"file:///data/zorbo/x.pdf", "/data/zorbo"},
		{"data/zorbo/", "data/zorbo"},
		{"./x", ""},
		{"/data/zorbo/x.pdf", "/data/zorbo"},
		{`C:\data\zorbo\x.pdf`, "C:/data/zorbo"},
		{"data/zorbo/../quillon/x.pdf", "data/quillon"},
		{"/x.pdf", "/"},
		{"x.pdf", ""},
		{"", ""},
	} {
		if got := CanonicalPrefix(tt.source); got != tt.want {
			t.Errorf("CanonicalPrefix(%q) = %q, want %q", tt.source, got, tt.want)
		}
	}
}

func TestNewDomainID(t *testing.T) {
	if got := NewDomainID("0123456789abcdef0123456789abcdef"); got != "d-01234567" {
		t.Fatalf("NewDomainID = %q", got)
	}
	// A malformed stored id must not crash detection: it still yields a
	// deterministic id in the d-<8 hex> space.
	idShape := regexp.MustCompile(`^d-[0-9a-f]{8}$`)
	seen := map[string]string{}
	for _, bad := range []string{"", "abc", strings.Repeat("Z", 32), "0123456789ABCDEF0123456789ABCDEF", strings.Repeat("0", 33)} {
		got := NewDomainID(bad)
		if !idShape.MatchString(got) || got != NewDomainID(bad) {
			t.Errorf("NewDomainID(%q) = %q, want a stable d-<8 hex> id", bad, got)
		}
		if prev, ok := seen[got]; ok {
			t.Errorf("NewDomainID(%q) and NewDomainID(%q) collide on %q", prev, bad, got)
		}
		seen[got] = bad
	}
}

func TestDomainName(t *testing.T) {
	target := DocProfile{DocID: "0123456789abcdef0123456789abcdef", Domain: "self", Profile: Profile{"common": 20, "acmemax": 1, "quillon": 1, "zorbo": 1}}
	corpus := []DocProfile{
		{DocID: "a", Domain: "a", Profile: Profile{"common": 1, "quillon": 1}},
		{DocID: "b", Domain: "b", Profile: Profile{"common": 1, "zorbo": 1}},
		{DocID: "c", Domain: "c", Profile: Profile{"common": 1}},
		target,
	}
	originals := map[string]string{"common": "Common", "acmemax": "Acme-Max", "quillon": "Quillon Ltd", "zorbo": "Zorbo"}
	if got := DomainName(target, originals, corpus); got != "Common / Acme-Max / Quillon Ltd" {
		t.Fatalf("DomainName = %q", got)
	}
	if got := DomainName(DocProfile{}, nil, corpus); got != "" {
		t.Fatalf("empty domain name = %q", got)
	}
}

func TestSeedLongestSourcePrefix(t *testing.T) {
	seed, err := LoadSeed(strings.NewReader(`{"schema":1,"names":{"a":"Acme","b":"Zorbo"},"rules":[{"match":"focus/x/","domain":"b"},{"match":"focus/","domain":"a"},{"match":"https://Docs.Acme.example/","domain":"a"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if seed.Schema != 1 || seed.Names["b"] != "Zorbo" {
		t.Fatalf("seed = %+v", seed)
	}
	for _, tt := range []struct {
		source, domain string
		found          bool
	}{
		{"focus/x/y.pdf", "b", true},
		{"focus/z.pdf", "a", true},
		{"focus/", "a", true},
		{"other.pdf", "", false},
		{"Focus/x/y.pdf", "", false},
		{"https://Docs.Acme.example/a.pdf", "a", true},
		{"https://docs.acme.example/a.pdf", "", false},
	} {
		if got, ok := seed.DomainFor(tt.source); got != tt.domain || ok != tt.found {
			t.Errorf("DomainFor(%q) = %q, %v; want %q, %v", tt.source, got, ok, tt.domain, tt.found)
		}
	}
	seed.Rules[0], seed.Rules[1] = seed.Rules[1], seed.Rules[0]
	if got, ok := seed.DomainFor("focus/x/y.pdf"); got != "b" || !ok {
		t.Fatalf("rule order changed result: %q %v", got, ok)
	}
}

func TestLoadSeedRejectsInvalid(t *testing.T) {
	for _, input := range []string{
		`{"schema":1,"rules":[{"match":"focus/","domain":"a"},{"match":"focus/","domain":"b"}]}`,
		`{"schema":2}`,
		`{}`,
		`{"schema":1,"rules":[{"match":"","domain":"a"}]}`,
		`{"schema":1,"rules":[{"match":"focus/","domain":"Upper"}]}`,
		`{"schema":1,"rules":[{"match":"focus/","domain":""}]}`,
		`{"schema":1,"names":{"bad_slug":"Acme"}}`,
		fmt.Sprintf(`{"schema":1,"names":{"%s":"Acme"}}`, strings.Repeat("a", 41)),
		`{"schema":1} {"schema":1}`,
		`{"schema":`,
		`{"schema":1,"rule":[{"match":"focus/","domain":"a"}]}`,
		`{"schema":1,"rules":[{"match":"focus/","domain":"a","dommain":"b"}]}`,
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := LoadSeed(strings.NewReader(input)); err == nil {
				t.Fatal("LoadSeed accepted invalid seed")
			}
		})
	}
	if _, err := LoadSeed(strings.NewReader(fmt.Sprintf(`{"schema":1,"names":{"%s":"Acme"}}`, strings.Repeat("a", 40)))); err != nil {
		t.Fatalf("40-character slug rejected: %v", err)
	}
}

// TestLoadSeedErrorIsDeterministic pins the first invalid name in sorted
// order, so preflight's message does not vary with map iteration.
func TestLoadSeedErrorIsDeterministic(t *testing.T) {
	for range 20 {
		_, err := LoadSeed(strings.NewReader(`{"schema":1,"names":{"zz_bad":"A","mm_bad":"B","aa_bad":"C"}}`))
		if err == nil || !strings.Contains(err.Error(), `"aa_bad"`) {
			t.Fatalf("err = %v, want the first invalid slug in sorted order", err)
		}
	}
}

func TestValidDomain(t *testing.T) {
	for in, want := range map[string]bool{
		"acme-1": true, strings.Repeat("a", 40): true,
		"": false, "Upper": false, "bad_slug": false, strings.Repeat("a", 41): false, "a b": false,
	} {
		if got := ValidDomain(in); got != want {
			t.Errorf("ValidDomain(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestZeroParamsUseDefaults keeps the zero Params from meaning "attach
// everything": an all-zero value is the documented defaults.
func TestZeroParamsUseDefaults(t *testing.T) {
	if DefaultParams() != (Params{Tau: 0.05, Delta: 0.02, TauOverride: 0.15}) {
		t.Fatalf("DefaultParams = %+v", DefaultParams())
	}
	target := DocProfile{DocID: "0123456789abcdef0123456789abcdef"}
	corpus := []DocProfile{{DocID: "a1", Domain: "acme"}, {DocID: "b1", Domain: "zorbo"}}
	got := Decide(target, corpus, Params{})
	if got.Action != "new" || got.Params != DefaultParams() {
		t.Fatalf("zero params decision = %+v, want new under the defaults", got)
	}
	// Under zero thresholds the empty-profile b1 would attach at score 0.
	profiles := []DocProfile{
		{DocID: "a1", Domain: "a", Profile: Profile{"zorbo": 1}},
		{DocID: "a2", Domain: "a", Profile: Profile{"zorbo": 1}},
		{DocID: "b1", Domain: "b"},
	}
	if got, want := Evaluate(profiles, Params{}), Evaluate(profiles, DefaultParams()); got != want {
		t.Fatalf("Evaluate zero params = %+v, want the defaults' %+v", got, want)
	}
}

func TestDomainToken(t *testing.T) {
	if got := DomainToken("b"); got != "dk3e23e8160039594a" {
		t.Fatalf("DomainToken(b) = %q", got)
	}
	if DomainToken("b") == DomainToken("a-b") {
		t.Fatal("different slugs have the same token")
	}
	for _, slug := range []string{"b", "a-b", strings.Repeat("a", 40)} {
		if got := DomainToken(slug); !regexp.MustCompile(`^[a-z0-9]{18}$`).MatchString(got) {
			t.Errorf("DomainToken(%q) = %q", slug, got)
		}
	}
}

func TestEvaluateThreeDomains(t *testing.T) {
	profiles := []DocProfile{
		{DocID: "00000000000000000000000000000001", Domain: "a", Profile: Profile{"quillon": 1}},
		{DocID: "00000000000000000000000000000002", Domain: "a", Profile: Profile{"quillon": 1}},
		{DocID: "00000000000000000000000000000003", Domain: "b", Profile: Profile{"zorbo": 1}},
		{DocID: "00000000000000000000000000000004", Domain: "b", Profile: Profile{"zorbo": 1}},
		{DocID: "00000000000000000000000000000005", Domain: "c", Profile: Profile{"quillon": 1}},
	}
	// LOO: a wins both a/c ties but the margin rejects the two a docs;
	// b's two docs attach correctly, and c attaches to a. Unseen: the
	// two a docs attach to c, c attaches to a, and both b docs stay new.
	got := Evaluate(profiles, testParams)
	want := EvalResult{Docs: 5, TopChoiceAccuracy: .8, AutoAttachShare: .6, UnseenFalseAttachShare: .6}
	if got != want {
		t.Fatalf("Evaluate = %+v, want %+v", got, want)
	}
	t.Logf("docs=%d top_choice_accuracy=%.2f auto_attach_share=%.2f unseen_false_attach_share=%.2f", got.Docs, got.TopChoiceAccuracy, got.AutoAttachShare, got.UnseenFalseAttachShare)
}

func TestEvaluateUsesCompletePrefixRule(t *testing.T) {
	profiles := []DocProfile{
		{DocID: "00000000000000000000000000000001", Domain: "a", Source: "/acme/one.pdf", Profile: Profile{}},
		{DocID: "00000000000000000000000000000002", Domain: "a", Source: "/acme/two.pdf", Profile: Profile{}},
		{DocID: "00000000000000000000000000000003", Domain: "a", Source: "/acme/three.pdf", Profile: Profile{}},
	}
	want := EvalResult{Docs: 3, TopChoiceAccuracy: 1, AutoAttachShare: 1, UnseenFalseAttachShare: 0}
	if got := Evaluate(profiles, testParams); got != want {
		t.Fatalf("Evaluate with prefix = %+v, want %+v", got, want)
	}
	if got := Evaluate(nil, testParams); got != (EvalResult{}) {
		t.Fatalf("empty Evaluate = %+v", got)
	}
}

func assertScores(t *testing.T, got, want []DomainScore) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("scores = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].Domain != want[i].Domain || math.IsNaN(got[i].Score) || math.Abs(got[i].Score-want[i].Score) > 1e-12 {
			t.Fatalf("scores[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestParamsFromConfigPerField: each zero field takes its default on its
// own, so ingest, evaluate and the migration resolve the same thresholds.
func TestParamsFromConfigPerField(t *testing.T) {
	d := DefaultParams()
	for _, tc := range []struct{ in, want Params }{
		{Params{}, d},
		{Params{Tau: 0.07}, Params{Tau: 0.07, Delta: d.Delta, TauOverride: d.TauOverride}},
		{Params{Delta: 0.01, TauOverride: 0.2}, Params{Tau: d.Tau, Delta: 0.01, TauOverride: 0.2}},
		{Params{Tau: 0.1, Delta: 0.03, TauOverride: 0.3}, Params{Tau: 0.1, Delta: 0.03, TauOverride: 0.3}},
	} {
		if got := ParamsFromConfig(tc.in.Tau, tc.in.Delta, tc.in.TauOverride); got != tc.want {
			t.Errorf("ParamsFromConfig(%+v) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}
