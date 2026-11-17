// Package reteval evaluates retrieval and optionally scores live or replayed
// agent answers. Question sets and reports may contain private corpus data.
package reteval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type QuestionSet struct {
	Schema    int        `json:"schema"`
	Questions []Question `json:"questions"`
}

type Question struct {
	ID          string       `json:"id"`
	Stratum     string       `json:"stratum"`
	Question    string       `json:"question"`
	Keywords    string       `json:"keywords,omitempty"`
	Domain      string       `json:"domain,omitempty"`
	Kinds       []string     `json:"kinds,omitempty"`
	Gold        []Anchor     `json:"gold,omitempty"`
	Answer      []AnswerPart `json:"answer,omitempty"`
	AnswerItems []AnswerItem `json:"answer_items,omitempty"`
	Absent      []string     `json:"absent,omitempty"`
}

type Anchor struct {
	Source string `json:"source"`
	Quote  string `json:"quote"`
}

type AnswerPart struct {
	Names []string `json:"names"`
}
type AnswerItem struct {
	Names    []string `json:"names"`
	Evidence []Anchor `json:"evidence"`
}

// Load reads and validates a schema-v1 question set.
func Load(path string) (QuestionSet, error) {
	f, err := os.Open(path)
	if err != nil {
		return QuestionSet{}, fmt.Errorf("reteval: questions: %w", err)
	}
	defer f.Close()
	r := bufio.NewReader(f)
	if prefix, _ := r.Peek(3); string(prefix) == "\xef\xbb\xbf" {
		_, _ = r.Discard(3)
	}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var set QuestionSet
	if err := dec.Decode(&set); err != nil {
		return QuestionSet{}, fmt.Errorf("reteval: questions: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return QuestionSet{}, fmt.Errorf("reteval: questions: expected one JSON object")
	}
	return prepare(set)
}

func prepare(set QuestionSet) (QuestionSet, error) {
	if set.Schema != 1 {
		return QuestionSet{}, fmt.Errorf("reteval: schema must be 1")
	}
	// Copy before applying defaults; Run must not mutate its caller's set.
	set.Questions = append([]Question(nil), set.Questions...)
	seen := map[string]bool{}
	for i := range set.Questions {
		q := &set.Questions[i]
		fail := func(reason string) (QuestionSet, error) {
			return QuestionSet{}, fmt.Errorf("reteval: question %q: %s", q.ID, reason)
		}
		if strings.TrimSpace(q.ID) == "" || strings.TrimSpace(q.Question) == "" {
			return fail("id and question are required")
		}
		if seen[q.ID] {
			return fail("duplicate id")
		}
		seen[q.ID] = true
		if q.Domain != "" {
			return fail("domain scope not supported yet")
		}
		if len(q.Kinds) == 0 {
			q.Kinds = []string{"chunk", "auto"}
		}
		for _, k := range q.Kinds {
			if k != "chunk" && k != "auto" {
				return fail(fmt.Sprintf("unsupported kind %q; kind must be chunk or auto", k))
			}
		}
		switch q.Stratum {
		case "single", "multi":
			if len(q.Gold) == 0 {
				return fail("gold is required")
			}
		case "global":
			if len(q.AnswerItems) == 0 {
				return fail("answer_items is required")
			}
		case "null":
			if len(q.Absent) == 0 {
				return fail("absent is required")
			}
		default:
			return fail("unknown stratum")
		}
		anchors := append([]Anchor(nil), q.Gold...)
		for _, item := range q.AnswerItems {
			if !validNames(item.Names) || len(item.Evidence) == 0 {
				return fail("answer items require names and evidence")
			}
			anchors = append(anchors, item.Evidence...)
		}
		for _, a := range anchors {
			if strings.TrimSpace(a.Quote) == "" {
				return fail("anchor quote is required")
			}
		}
		for _, a := range q.Answer {
			if !validNames(a.Names) {
				return fail("answer part names are required")
			}
		}
		for _, term := range q.Absent {
			if strings.TrimSpace(term) == "" {
				return fail("absent terms must not be empty")
			}
		}
	}
	return set, nil
}

func validNames(names []string) bool {
	if len(names) == 0 {
		return false
	}
	for _, n := range names {
		if Fold(n) == "" {
			return false
		}
	}
	return true
}
