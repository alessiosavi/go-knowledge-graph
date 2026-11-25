package productobs

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProductTextsSkipTable(t *testing.T) {
	file, err := os.Open("testdata/product_texts_skip.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	line := 0
	for scanner.Scan() {
		line++
		value, label, ok := strings.Cut(scanner.Text(), "\t")
		if !ok || (label != "skip" && label != "keep") {
			t.Fatalf("invalid fixture row %d: %q", line, scanner.Text())
		}
		got, stats := ProductTexts(Record{Attributes: map[string]string{"test_key": value}})
		var want []ProductText
		var wantStats ProductTextStats
		if label == "keep" {
			want = []ProductText{{Key: "test_key", Text: "test key: " + value}}
		} else {
			wantStats.SkippedValues = 1
		}
		if !slices.Equal(got, want) {
			t.Errorf("row %d, value %q (%s): got %+v, want %+v", line, value, label, got, want)
		}
		if stats != wantStats {
			t.Errorf("row %d, value %q (%s): stats = %+v, want %+v", line, value, label, stats, wantStats)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if line != 42 {
		t.Fatalf("read %d fixtures, want 42", line)
	}
}

func TestProductTextsOrderAndDuplicates(t *testing.T) {
	record := Record{
		Title: "Product title",
		Attributes: map[string]string{
			"z_color":  "white",
			"a finish": "white",
			"a_finish": "white",
			"raw__key": "  sterìle pack  ",
		},
	}
	want := []ProductText{
		{Key: "a finish", Text: "a finish: white"},
		{Key: "a_finish", Text: "a finish: white"},
		{Key: "raw__key", Text: "raw  key:   sterìle pack  "},
		{Key: "z_color", Text: "z color: white"},
	}
	if got, _ := ProductTexts(record); !slices.Equal(got, want) {
		t.Errorf("ProductTexts = %+v, want %+v", got, want)
	}
	if got, stats := ProductTexts(Record{Title: record.Title}); len(got) != 0 || stats != (ProductTextStats{}) {
		t.Errorf("title alone produced texts %+v and stats %+v", got, stats)
	}
}

func TestProductTextsByteLimits(t *testing.T) {
	key := strings.Repeat("é", 128)
	beforeRune := strings.Repeat("a", 2047-len("cut: "))
	exact := strings.Repeat("a", 2048-len("exact: ")-len("é")) + "é"
	got, stats := ProductTexts(Record{Attributes: map[string]string{
		"cut":     beforeRune + "🦷 text",
		"empty":   "",
		"exact":   exact,
		key:       "cotton",
		key + "!": "long key",
	}})
	if want := (ProductTextStats{SkippedValues: 1, SkippedKeys: 1, Truncated: 1}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	want := []ProductText{
		{Key: "cut", Text: "cut: " + beforeRune, Truncated: true},
		{Key: "exact", Text: "exact: " + exact},
		{Key: key, Text: key + ": cotton"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d texts, want %d", len(got), len(want))
	}
	for i, text := range got {
		if text != want[i] || !utf8.ValidString(text.Text) {
			t.Errorf("text %d: key bytes=%d text bytes=%d truncated=%t valid UTF-8=%t, want key bytes=%d text bytes=%d truncated=%t",
				i, len(text.Key), len(text.Text), text.Truncated, utf8.ValidString(text.Text), len(want[i].Key), len(want[i].Text), want[i].Truncated)
		}
	}
}

func TestProductTextsCap(t *testing.T) {
	attributes := make(map[string]string, 514)
	attributes["000"] = "350ml"
	for i := 1; i <= 513; i++ {
		attributes[fmt.Sprintf("%03d", i)] = "cotton"
	}
	got, stats := ProductTexts(Record{Attributes: attributes})
	if want := (ProductTextStats{SkippedValues: 1}); stats != want {
		t.Errorf("stats = %+v, want %+v", stats, want)
	}
	if len(got) != 512 {
		t.Fatalf("got %d texts, want 512", len(got))
	}
	if got[0].Key != "001" || got[len(got)-1].Key != "512" {
		t.Errorf("first and last keys = %q, %q, want 001 and 512", got[0].Key, got[len(got)-1].Key)
	}
}

func TestProductTextIDs(t *testing.T) {
	const text = "material: cotton"
	for _, tc := range []struct{ store, want string }{
		{"t1a-store", "12244c0030768859a6408bd3c6f03c6e"},
		{"t1a-other", "1872dc50ee3a987a49191e1cbdc3e0a9"},
	} {
		if got := ProductTextID(tc.store, text); got != tc.want {
			t.Errorf("ProductTextID(%q, %q) = %q, want %q", tc.store, text, got, tc.want)
		}
	}
	if got, want := ProductTextVecID(text), "1986dbc2f01429813512288ebb54c761"; got != want {
		t.Errorf("ProductTextVecID(%q) = %q, want %q", text, got, want)
	}
}
