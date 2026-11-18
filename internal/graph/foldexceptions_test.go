package graph_test

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func TestFoldExceptionsKeysAndDigest(t *testing.T) {
	input := "\ufeff@shop-2:a:b\r\n \r\n \t // comment\r\n  plain name \t\r\n #doc-1:entity  \nplain // literal"
	exceptions, err := graph.ParseFoldExceptions(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"#doc-1:entity", "@shop-2:a:b", "plain // literal", "plain name"}
	keys := exceptions.Keys()
	if !slices.Equal(keys, want) {
		t.Fatalf("Keys() = %q, want %q", keys, want)
	}
	keys[0] = "mutated caller slice"
	for _, key := range want {
		if !exceptions.Contains(key) {
			t.Errorf("Contains(%q) = false", key)
		}
	}
	for _, key := range []string{"entity", "@other:a:b", "@shop-2:A:B", " plain name ", "// comment", ""} {
		if exceptions.Contains(key) {
			t.Errorf("Contains(%q) = true", key)
		}
	}
	const wantDigest = "c6635101af2900b9cc72a2ff9310aa02b1746190d49fda80b8eee3d4b3054210"
	if got := exceptions.Digest(); got != wantDigest {
		t.Errorf("Digest() = %q, want %q", got, wantDigest)
	}
	sorted, err := graph.ParseFoldExceptions(strings.NewReader(strings.Join(want, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if got := sorted.Digest(); got != wantDigest {
		t.Errorf("Digest() with sorted input = %q, want %q", got, wantDigest)
	}
}

func TestFoldExceptionsEmpty(t *testing.T) {
	empty, err := graph.ParseFoldExceptions(strings.NewReader("\ufeff  // comment only\r\n  "))
	if err != nil {
		t.Fatal(err)
	}
	for _, exceptions := range []graph.FoldExceptions{empty, {}} {
		if len(exceptions.Keys()) != 0 || exceptions.Contains("") {
			t.Errorf("empty exceptions returned keys %q", exceptions.Keys())
		}
		const emptyDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		if got := exceptions.Digest(); got != emptyDigest {
			t.Errorf("empty Digest() = %q, want %q", got, emptyDigest)
		}
	}
}

func TestFoldExceptionsRejectsInvalidLines(t *testing.T) {
	for _, tc := range []struct {
		line, reason string
	}{
		{"@shop: \t", "empty surface"},
		{"#doc: \t", "empty surface"},
		{"@shop: entity", "surface has surrounding whitespace"},
		{"#doc:\tentity", "surface has surrounding whitespace"},
		{"@shop:\u00a0entity", "surface has surrounding whitespace"},
		{"@Shop:entity", "invalid domain"},
		{"@:entity", "invalid domain"},
		{"@shop_name:entity", "invalid domain"},
		{"@" + strings.Repeat("a", 41) + ":entity", "invalid domain"},
		{"@shop", "missing ':'"},
		{"#doc", "missing ':'"},
		{"#:entity", "empty document ID"},
		{"# \t:entity", "empty document ID"},
		{" first \t", "duplicate key"},
		{"@shop:second", "duplicate key"},
		{"#doc:third", "duplicate key"},
	} {
		t.Run(tc.line, func(t *testing.T) {
			input := "first\n@shop:second\n#doc:third\n\n // skipped\n" + tc.line
			exceptions, err := graph.ParseFoldExceptions(strings.NewReader(input))
			if err == nil || !strings.Contains(err.Error(), "line 6:") || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("ParseFoldExceptions error = %v, want line 6 and %q", err, tc.reason)
			}
			if len(exceptions.Keys()) != 0 {
				t.Errorf("partial keys returned on error: %q", exceptions.Keys())
			}
		})
	}
}

func TestFoldExceptionsReadError(t *testing.T) {
	failure := errors.New("read failed")
	reader := io.MultiReader(strings.NewReader("valid key\npartial line"), iotest.ErrReader(failure))
	exceptions, err := graph.ParseFoldExceptions(reader)
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "line 2:") {
		t.Fatalf("ParseFoldExceptions error = %v, want line 2 and %v", err, failure)
	}
	if len(exceptions.Keys()) != 0 {
		t.Errorf("partial keys returned on error: %q", exceptions.Keys())
	}
}

func TestFoldExceptionsLongLine(t *testing.T) {
	key := strings.Repeat("x", 70_000)
	exceptions, err := graph.ParseFoldExceptions(strings.NewReader(key))
	if err != nil {
		t.Fatal(err)
	}
	if !exceptions.Contains(key) {
		t.Error("long key missing")
	}
}
