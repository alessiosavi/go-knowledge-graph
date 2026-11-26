package agent

import (
	"slices"
	"testing"
)

func checkMarks(t *testing.T, text string, seen *seenIndex, products, chunks []string, dropped int, summary string) {
	t.Helper()
	got := resolveMarks(text, seen)
	if !slices.Equal(got.Products, products) || !slices.Equal(got.Chunks, chunks) || got.NotReturned != dropped {
		t.Fatalf("marks = %+v, want products %v, chunks %v, not returned %d", got, products, chunks, dropped)
	}
	if cleaned := cleanSummary(text, got.Spans); cleaned != summary {
		t.Errorf("summary = %q, want %q", cleaned, summary)
	}
}

func TestResolveMarksPrefixNotCitation(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("graph1", "shop.example:1")
	checkMarks(t, "[[shop.example:12]]", seen, nil, nil, 1, "")
	checkMarks(t, "[[shop.example:12 is unavailable", seen, nil, nil, 0, "shop.example:12 is unavailable")
}

func TestResolveMarksUnterminatedKeepsProse(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("graph1", "shop.example:1")
	checkMarks(t, "Pick it [[shop.example:1. Next sentence.", seen, []string{"shop.example:1"}, nil, 0, "Pick it. Next sentence.")
}

func TestResolveMarksLongestSeenWins(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("graph1", "shop.example:1")
	seen.addProduct("graph2", "shop.example:1.")
	checkMarks(t, "Pick it [[shop.example:1. Next sentence.", seen, []string{"shop.example:1."}, nil, 0, "Pick it Next sentence.")
}

func TestResolveMarksSeparatorIDsNotCandidates(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("graph1", "shop.example:1")
	seen.addProduct("graph2", "shop.example:1],")
	for _, text := range []string{"Pick [[shop.example:1], .", "Pick [[shop.example:1],]]."} {
		checkMarks(t, text, seen, []string{"shop.example:1"}, nil, 0, "Pick.")
	}
}

func TestResolveMarksTrailingPunctuation(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("graph1", "shop.example:1")
	for _, punct := range []string{".", ";", ":", "!", "?", ")"} {
		t.Run(punct, func(t *testing.T) {
			checkMarks(t, "Pick [[shop.example:1"+punct, seen, []string{"shop.example:1"}, nil, 0, "Pick"+punct)
		})
	}
}

func TestResolveMarksTabIDByGraphID(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("graph1", "shop.example:1\t2")
	checkMarks(t, "Pick [[graph1]].", seen, []string{"shop.example:1\t2"}, nil, 0, "Pick.")
	checkMarks(t, "Pick [[shop.example:1\t2]].", seen, nil, nil, 2, "Pick.")
}

func TestResolveMarksSpansMatchCleaning(t *testing.T) {
	text := "A [[a]].\nB [[b], .\nC"
	seen := newSeenIndex()
	seen.addProduct("a", "shop.example:a")
	seen.addProduct("b", "shop.example:b")
	marks := resolveMarks(text, seen)
	if want := [][2]int{{2, 7}, {11, 17}}; !slices.Equal(marks.Spans, want) {
		t.Fatalf("spans = %v, want %v", marks.Spans, want)
	}
	if got := cleanSummary(text, marks.Spans); got != "A.\nB.\nC" {
		t.Errorf("summary = %q", got)
	}
}

func TestParseMarksForms(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("a", "shop.example:a")
	seen.addProduct("b", "shop.example:b")
	seen.addProduct("c", "shop.example:comma,id")
	seen.addProduct("d", "shop.example:space id")
	seen.addExcerpt("chunk", Excerpt{Tool: "search", Text: "Text"})
	for _, tc := range []struct {
		mark     string
		products []string
		chunks   []string
		dropped  int
	}{
		{"[[a]]", []string{"shop.example:a"}, nil, 0},
		{"[[a, b]]", []string{"shop.example:a", "shop.example:b"}, nil, 0},
		{"[[a], [b]]", []string{"shop.example:a", "shop.example:b"}, nil, 0},
		{"[[a], ", []string{"shop.example:a"}, nil, 0},
		{"[[b, shop.example:b, a, chunk]]", []string{"shop.example:b", "shop.example:a"}, []string{"chunk"}, 0},
		{"[[document, unseen, shop.example:context]]", nil, nil, 3},
		{"[[shop.example:comma,id, shop.example:space id]]", nil, nil, 4},
		{"[[c, d]]", []string{"shop.example:comma,id", "shop.example:space id"}, nil, 0},
	} {
		t.Run(tc.mark, func(t *testing.T) {
			checkMarks(t, "Pick "+tc.mark+".", seen, tc.products, tc.chunks, tc.dropped, "Pick.")
		})
	}
}

func TestCleanSummary(t *testing.T) {
	const hash = "0123456789abcdef0123456789ABCDEF"
	for _, tc := range []struct{ text, want string }{
		{"Rapporto 1:2.", "Rapporto 1:2."},
		{"**Lumina** [[x]].", "**Lumina**."},
		{"Cite " + hash + "-c12.", "Cite."},
		{"**Keep**\n| a | b |\n| x" + hash + " | y |", "**Keep**\n| a | b |\n| x" + hash + " | y |"},
		{"A !", "A !"},
		{"  [[x]]", "  "},
	} {
		t.Run(tc.text, func(t *testing.T) {
			marks := resolveMarks(tc.text, newSeenIndex())
			if got := cleanSummary(tc.text, marks.Spans); got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseMarksRuledOutProductGetsNoCard(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("a", "shop.example:a")
	seen.addProduct("b", "shop.example:b")
	checkMarks(t, "Pick A [[a]], skip B.", seen, []string{"shop.example:a"}, nil, 0, "Pick A, skip B.")
}
