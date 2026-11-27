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

func TestAnswerReview1RejectsLongestPrefixWithInvalidBoundary(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("graph1", "shop.example:1")
	seen.addProduct("graph2", "shop.example:1.foo")
	checkMarks(t, "Select [[shop.example:1.foobar", seen, nil, nil, 0, "Select shop.example:1.foobar")
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

func TestAnswerReview2_1UnterminatedPunctuationRequiresBoundary(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("graph1", "shop.example:1")
	for _, punct := range []string{".", ";", ":", "!", "?", ")"} {
		t.Run(punct, func(t *testing.T) {
			checkMarks(t, "[[shop.example:1"+punct+"variant", seen, nil, nil, 0, "shop.example:1"+punct+"variant")
			checkMarks(t, "Pick [[shop.example:1"+punct+"\u00a0Next.", seen, []string{"shop.example:1"}, nil, 0, "Pick"+punct+"\u00a0Next.")
		})
	}
}

func TestAnswerReviewBotUnterminatedBeforeNextGroup(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("graph1", "shop.example:1")
	seen.addProduct("graph2", "shop.example:2")
	// A malformed group closes only at its own "]]", never at a later group's.
	checkMarks(t, "A [[shop.example:1] is lighter than B [[shop.example:2]].", seen,
		[]string{"shop.example:1", "shop.example:2"}, nil, 0, "A is lighter than B.")
}

func TestResolveMarksUnterminatedBeforeNextRole(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("graph1", "shop.example:1")
	seen.addProduct("graph2", "shop.example:2")
	const prefix = "Compare "
	for _, tc := range []struct {
		name, firstMark, secondMark string
		products, alternatives      []string
	}{
		{"alternative then match", "[[!alt: graph1 ", "[[graph2]]", []string{"shop.example:2"}, []string{"shop.example:1"}},
		{"match then alternative", "[[graph1 ", "[[!alt: graph2]]", []string{"shop.example:1"}, []string{"shop.example:2"}},
		{"same product promoted to match", "[[!alt: graph1, ", "[[shop.example:1]]", []string{"shop.example:1"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := prefix + tc.firstMark + tc.secondMark + "."
			marks := resolveMarks(text, seen)
			if !slices.Equal(marks.Products, tc.products) || !slices.Equal(marks.Alternatives, tc.alternatives) || marks.NotReturned != 0 {
				t.Errorf("marks = %+v, want products %v and alternatives %v with no dropped tokens", marks, tc.products, tc.alternatives)
			}
			boundary := len(prefix) + len(tc.firstMark)
			wantSpans := [][2]int{{len(prefix), boundary}, {boundary, len(text) - 1}}
			if !slices.Equal(marks.Spans, wantSpans) {
				t.Errorf("spans = %v, want %v", marks.Spans, wantSpans)
			}
			if got := cleanSummary(text, marks.Spans); got != "Compare." {
				t.Errorf("summary = %q, want %q", got, "Compare.")
			}
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

func TestAnswerReview3CitationGroupsStopAtLineBreaks(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("graph1", "shop.example:1")
	seen.addProduct("graph2", "shop.example:2")
	for _, tc := range []struct{ name, newline string }{
		{"LF", "\n"}, {"CRLF", "\r\n"}, {"CR", "\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := "One [[shop.example:1." + tc.newline + "Two [[shop.example:2]]."
			checkMarks(t, text, seen, []string{"shop.example:1", "shop.example:2"}, nil, 0, "One."+tc.newline+"Two.")
		})
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

func TestAnswerReview5SummaryPreservesLeadingIndentation(t *testing.T) {
	for _, tc := range []struct{ name, text, summary string }{
		{"indented block", "Example:\n\n    [[unknown]].", "Example:\n\n    ."},
		{"first line", "    [[unknown]].", "    ."},
		{"after CR", "Example:\r\t  [[unknown]].", "Example:\r\t  ."},
		{"after CRLF", "Example:\r\n    [[unknown]].", "Example:\r\n    ."},
		{"after text", "    Example [[unknown]] .", "    Example."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkMarks(t, tc.text, nil, nil, nil, 1, tc.summary)
		})
	}
}

func TestAnswerReview2_2SummaryKeepsOnlyOriginalIndentation(t *testing.T) {
	for _, tc := range []struct{ name, text, summary string }{
		{"spaces", "  [[unknown]]  .", "  ."},
		{"tab", "\t[[unknown]]  .", "\t."},
		{"mixed after CRLF", "Example:\r\n\t  [[unknown]]  !", "Example:\r\n\t  !"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkMarks(t, tc.text, nil, nil, nil, 1, tc.summary)
		})
	}
}

func TestParseMarksRuledOutProductGetsNoCard(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("a", "shop.example:a")
	seen.addProduct("b", "shop.example:b")
	checkMarks(t, "Pick A [[a]], skip B.", seen, []string{"shop.example:a"}, nil, 0, "Pick A, skip B.")
}

func TestResolveAlternativeMarks(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("graph1", "shop.example:1")
	seen.addProduct("graph2", "alt:shop.example:1")
	seen.addExcerpt("chunk", Excerpt{Tool: "search", Text: "Passage"})
	for _, tc := range []struct {
		name, text, summary            string
		products, alternatives, chunks []string
		dropped                        int
	}{
		{"closed", "Try [[!alt: shop.example:1]].", "Try.", nil, []string{"shop.example:1"}, nil, 0},
		{"unterminated", "Try [[!alt: shop.example:1. More prose.", "Try. More prose.", nil, []string{"shop.example:1"}, nil, 0},
		{"unterminated unknown stays in prose", "[[!alt: unknown", "unknown", nil, nil, nil, 0},
		{"glued", "Try [[!alt:shop.example:1]].", "Try.", nil, []string{"shop.example:1"}, nil, 0},
		{"uppercase and leading whitespace", "Try [[\t !ALT: graph1]].", "Try.", nil, []string{"shop.example:1"}, nil, 0},
		{"mixed with unknown tokens", "Try [[!alt: shop.example:1, chunk, unknown, absent]].", "Try.", nil, []string{"shop.example:1"}, []string{"chunk"}, 2},
		{"prefix only", "[[!alt:]]", "", nil, nil, nil, 0},
		{"plain alt store", "Pick [[alt:shop.example:1]].", "Pick.", []string{"alt:shop.example:1"}, nil, nil, 0},
		{"literal prose", "The literal !alt: stays.", "The literal !alt: stays.", nil, nil, nil, 0},
		{"prefix must lead", "Pick [[, !alt: shop.example:1]].", "Pick.", []string{"shop.example:1"}, nil, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marks := resolveMarks(tc.text, seen)
			if !slices.Equal(marks.Products, tc.products) || !slices.Equal(marks.Alternatives, tc.alternatives) ||
				!slices.Equal(marks.Chunks, tc.chunks) || marks.NotReturned != tc.dropped {
				t.Errorf("marks = %+v, want products %v, alternatives %v, chunks %v, not returned %d",
					marks, tc.products, tc.alternatives, tc.chunks, tc.dropped)
			}
			if got := cleanSummary(tc.text, marks.Spans); got != tc.summary {
				t.Errorf("summary = %q, want %q", got, tc.summary)
			}
		})
	}
}

func TestResolveMatchTakesPrecedence(t *testing.T) {
	seen := newSeenIndex()
	seen.addProduct("graph1", "shop.example:1")
	seen.addProduct("graph2", "shop.example:2")
	for _, text := range []string{
		"[[!alt: graph1, graph2]] [[shop.example:1]]",
		"[[shop.example:1]] [[!alt: graph1, graph2]]",
	} {
		t.Run(text, func(t *testing.T) {
			marks := resolveMarks(text, seen)
			if !slices.Equal(marks.Products, []string{"shop.example:1"}) ||
				!slices.Equal(marks.Alternatives, []string{"shop.example:2"}) || marks.NotReturned != 0 {
				t.Fatalf("marks = %+v", marks)
			}
		})
	}
}
