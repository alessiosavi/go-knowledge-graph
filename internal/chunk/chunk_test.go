package chunk_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunk"
)

const (
	enText = "Dr. Smith went home. It rained all night. We stayed inside. The value is 3.14 exactly. Then we left."
	itText = "Città perché così. Però però però. ÀÈÌÒÙ àèìòù. La fine. Domani partiamo presto."
)

func assertOffsetsSliceOriginal(t *testing.T, text string, spans []chunk.Span) {
	t.Helper()
	if len(spans) == 0 {
		t.Fatal("no spans produced")
	}
	prevStart := -1
	prevEnd := -1
	for i, s := range spans {
		if s.Seq != i {
			t.Errorf("span %d Seq = %d", i, s.Seq)
		}
		if s.Start >= s.End {
			t.Errorf("span %d has Start %d >= End %d", i, s.Start, s.End)
		}
		if s.Start <= prevStart {
			t.Errorf("span %d Start %d not after previous %d", i, s.Start, prevStart)
		}
		if s.End <= prevEnd {
			t.Errorf("span %d ends at %d, not beyond the previous span's end %d (pure overlap)", i, s.End, prevEnd)
		}
		prevStart = s.Start
		prevEnd = s.End
		if got := text[s.Start:s.End]; got != s.Text {
			t.Errorf("span %d: text[Start:End] = %q, Text = %q", i, got, s.Text)
		}
	}
}

func assertEndsAtSentenceBoundary(t *testing.T, spans []chunk.Span) {
	t.Helper()
	for i, s := range spans {
		trimmed := strings.TrimSpace(s.Text)
		if trimmed == "" {
			t.Errorf("span %d is blank", i)
			continue
		}
		r, _ := utf8.DecodeLastRuneInString(trimmed)
		if !strings.ContainsRune(".!?…", r) {
			t.Errorf("span %d does not end at a sentence boundary: %q", i, trimmed)
		}
	}
}

func TestSplitRespectsSentenceBoundaries(t *testing.T) {
	spans := chunk.Split(enText, chunk.Options{TargetWords: 3, OverlapSentences: 0})
	if len(spans) < 2 {
		t.Fatalf("got %d spans, want multiple with a small target", len(spans))
	}
	assertEndsAtSentenceBoundary(t, spans)

	// The abbreviation guard must keep "Dr. Smith went home." in one piece.
	found := false
	for _, s := range spans {
		if strings.Contains(s.Text, "Dr. Smith went home.") {
			found = true
		}
	}
	if !found {
		t.Errorf("abbreviation split offending sentence across chunks: %+v", spans)
	}
	// 3.14 must not split either.
	found = false
	for _, s := range spans {
		if strings.Contains(s.Text, "value is 3.14 exactly.") {
			found = true
		}
	}
	if !found {
		t.Errorf("decimal number split a sentence across chunks: %+v", spans)
	}

	// A digit with a period ends its sentence ("It costs 5.").
	spans = chunk.Split("It costs 5. That is cheap.", chunk.Options{TargetWords: 2})
	if len(spans) == 0 || spans[0].Text != "It costs 5." {
		t.Errorf("digit-terminated sentence: spans = %+v, want first span exactly %q", spans, "It costs 5.")
	}
}

func TestSplitOffsetsSliceOriginal(t *testing.T) {
	for name, text := range map[string]string{"en": enText, "it": itText} {
		t.Run(name, func(t *testing.T) {
			spans := chunk.Split(text, chunk.Options{TargetWords: 4, OverlapSentences: 1})
			assertOffsetsSliceOriginal(t, text, spans)
		})
	}
	t.Run("empty", func(t *testing.T) {
		if spans := chunk.Split("   \n\n  ", chunk.Options{TargetWords: 10}); len(spans) != 0 {
			t.Errorf("whitespace-only text produced %+v", spans)
		}
	})
}

func TestOverlapOneSentence(t *testing.T) {
	text := "Alpha beta. Gamma delta. Epsilon zeta. Eta theta."
	spans := chunk.Split(text, chunk.Options{TargetWords: 6, OverlapSentences: 1})
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	if !strings.HasPrefix(spans[1].Text, "Epsilon zeta.") {
		t.Errorf("span 1 = %q, want it to start with the previous span's last sentence", spans[1].Text)
	}
	if spans[1].Start != strings.Index(text, "Epsilon zeta.") {
		t.Errorf("overlap span Start = %d, want %d (original offset)", spans[1].Start, strings.Index(text, "Epsilon zeta."))
	}
	assertOffsetsSliceOriginal(t, text, spans)

	// An overlap larger than the chunk's sentence count is clamped and never
	// stalls.
	clamped := chunk.Split(text, chunk.Options{TargetWords: 6, OverlapSentences: 5})
	if len(clamped) != 2 {
		t.Fatalf("clamped: got %d spans, want 2", len(clamped))
	}
	if !strings.HasPrefix(clamped[1].Text, "Gamma delta.") {
		t.Errorf("clamped span 1 = %q, want it to start after the first sentence", clamped[1].Text)
	}
	assertOffsetsSliceOriginal(t, text, clamped)
}

func TestHeadingInterruptsUnterminatedLine(t *testing.T) {
	text := "Intro line\n# Heading\nBody sentence."
	spans := chunk.Split(text, chunk.Options{TargetWords: 100, OverlapSentences: 1})
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2 (heading must interrupt even without a terminator)", len(spans))
	}
	if got := strings.TrimSpace(spans[0].Text); got != "Intro line" {
		t.Errorf("span 0 = %q, want the unterminated intro line alone", got)
	}
	if !strings.HasPrefix(spans[1].Text, "# Heading") {
		t.Errorf("span 1 = %q, want it to start with the heading", spans[1].Text)
	}
	assertOffsetsSliceOriginal(t, text, spans)

	text2 := "some text\nCARE GUIDE\nWash daily. Rinse well."
	spans2 := chunk.Split(text2, chunk.Options{TargetWords: 100, OverlapSentences: 1})
	if len(spans2) != 2 {
		t.Fatalf("all-caps: got %d spans, want 2", len(spans2))
	}
	if !strings.HasPrefix(spans2[1].Text, "CARE GUIDE") {
		t.Errorf("all-caps span 1 = %q, want it to start with the heading", spans2[1].Text)
	}
	assertOffsetsSliceOriginal(t, text2, spans2)
}

func TestCommonWordAbbreviations(t *testing.T) {
	// "No." ends its sentence when not followed by a number…
	spans := chunk.Split("No. Andiamo via domani.", chunk.Options{TargetWords: 1})
	if len(spans) < 2 || strings.TrimSpace(spans[0].Text) != "No." {
		t.Errorf("No. must end its sentence when not followed by a number: %+v", spans)
	}
	// …but before a number it is an abbreviation.
	spans = chunk.Split("Vedi no. 5.", chunk.Options{TargetWords: 1})
	if len(spans) == 0 || strings.TrimSpace(spans[0].Text) != "Vedi no. 5." {
		t.Errorf("no. before a number must not split: %+v", spans)
	}
}

func TestOverlapNeverProducesPureOverlapChunk(t *testing.T) {
	// Reviewer trace: the overlap sentence plus the next one exceed the
	// budget, so the naive scan would emit a chunk that is a strict suffix
	// of its predecessor. Every chunk must carry new text.
	text := "Alpha beta. Gamma delta. Epsilon zeta eta theta iota kappa lambda mu nu xi. Nu xi."
	spans := chunk.Split(text, chunk.Options{TargetWords: 6, OverlapSentences: 1})
	assertOffsetsSliceOriginal(t, text, spans)
	if len(spans) < 2 {
		t.Fatalf("got %d spans, want multiple", len(spans))
	}
	if !strings.Contains(spans[1].Text, "Epsilon") {
		t.Errorf("span 1 = %q, want it to include the sentence beyond the overlap", spans[1].Text)
	}
}

func TestHeadingStartsNewChunk(t *testing.T) {
	text := "Intro line one. Intro line two.\n# Big Heading\nBody sentence one. Body sentence two."
	spans := chunk.Split(text, chunk.Options{TargetWords: 100, OverlapSentences: 1})
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2 (heading forces a boundary)", len(spans))
	}
	if strings.Contains(spans[0].Text, "Big Heading") {
		t.Errorf("heading leaked into the previous chunk: %q", spans[0].Text)
	}
	if !strings.HasPrefix(spans[1].Text, "# Big Heading") {
		t.Errorf("span 1 = %q, want it to start with the heading (no overlap across a heading)", spans[1].Text)
	}
	assertOffsetsSliceOriginal(t, text, spans)
}

func TestAllCapsLineStartsNewChunk(t *testing.T) {
	text := "CARE GUIDE\nWash daily. Rinse well. Dry slowly."
	spans := chunk.Split(text, chunk.Options{TargetWords: 2, OverlapSentences: 0})
	if len(spans) < 2 {
		t.Fatalf("got %d spans, want a boundary at the all-caps line", len(spans))
	}
	if got := strings.TrimSpace(spans[0].Text); got != "CARE GUIDE" {
		t.Errorf("span 0 = %q, want the all-caps heading", got)
	}
	if !strings.Contains(spans[1].Text, "Wash daily.") {
		t.Errorf("span 1 = %q, want the body", spans[1].Text)
	}
	assertOffsetsSliceOriginal(t, text, spans)
}

func TestItalianAccents(t *testing.T) {
	spans := chunk.Split(itText, chunk.Options{TargetWords: 3, OverlapSentences: 0})
	assertOffsetsSliceOriginal(t, itText, spans)
	joined := ""
	for _, s := range spans {
		joined += s.Text + " "
	}
	for _, want := range []string{"Città", "perché", "ÀÈÌÒÙ"} {
		if !strings.Contains(joined, want) {
			t.Errorf("accents lost: %q missing from %q", want, joined)
		}
	}
	assertEndsAtSentenceBoundary(t, spans)
}
