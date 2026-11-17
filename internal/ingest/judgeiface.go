package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
)

// FactInput is one fact submitted to the judge for verification.
type FactInput struct {
	Head       string
	Rel        string
	Tail       string
	Evidence   string
	DocContext string
	Confidence float64
	// DocID names the document that asked. It is not part of the judge
	// request or the cache key; it only ties a cached verdict to the
	// document whose deletion removes it.
	DocID string
}

// judgeInputPayload fixes the key order of the user message shared by ingest,
// the verdict cache and backfill. Keep every field, including zero values.
// Fields are in sorted key order, so the bytes equal what the judge received
// before this type existed (json.Marshal of a map sorts its keys).
type judgeInputPayload struct {
	DocContext string `json:"doc_context"`
	Evidence   string `json:"evidence"`
	Fact       struct {
		Confidence float64 `json:"confidence"`
		Head       string  `json:"head"`
		Rel        string  `json:"rel"`
		Tail       string  `json:"tail"`
	} `json:"fact"`
}

// CanonicalJudgeInput returns the exact JSON user message sent to the judge.
// A non-finite confidence cannot be encoded in JSON and returns nil.
func CanonicalJudgeInput(in FactInput) []byte {
	var payload judgeInputPayload
	payload.Fact.Head, payload.Fact.Rel, payload.Fact.Tail = in.Head, in.Rel, in.Tail
	payload.Fact.Confidence = in.Confidence
	payload.Evidence, payload.DocContext = in.Evidence, in.DocContext
	b, _ := json.Marshal(payload)
	return b
}

// DecodeJudgeInput reconstructs a fact from its persisted judge user message.
func DecodeJudgeInput(b []byte) (FactInput, error) {
	var payload judgeInputPayload
	if err := json.Unmarshal(b, &payload); err != nil {
		return FactInput{}, fmt.Errorf("ingest: decode judge input: %w", err)
	}
	return FactInput{
		Head: payload.Fact.Head, Rel: payload.Fact.Rel, Tail: payload.Fact.Tail,
		Confidence: payload.Fact.Confidence, Evidence: payload.Evidence, DocContext: payload.DocContext,
	}, nil
}

// Verdict is the judge's answer for one fact.
type Verdict struct {
	Status     string // confirmed | refuted | unclear
	Confidence float64
	Reason     string
}

// FactJudge verifies facts in online mode. Errors are failure-isolated: the
// document still completes, the fact just stays unverified.
type FactJudge interface {
	VerifyFact(ctx context.Context, in FactInput) (Verdict, error)
}

// ShouldJudge reports whether a fact is selected for verification by the
// configured mode (off | low-confidence | sample | all). Sample selection
// is deterministic per fact identity. Unknown modes select nothing.
func ShouldJudge(mode string, confidence, verifyFloor, samplePct float64, factKey string) bool {
	switch mode {
	case "low-confidence":
		return confidence < verifyFloor
	case "all":
		return true
	case "sample":
		if samplePct <= 0 {
			return false
		}
		if samplePct >= 100 {
			return true
		}
		sum := sha256.Sum256([]byte(factKey))
		bucket := binary.BigEndian.Uint16(sum[:2])
		return float64(bucket) < samplePct/100*65536
	default: // off, empty, unknown
		return false
	}
}

// EvidenceSentence returns the sentence of text containing the byte span
// [start, end): the slice between the nearest sentence boundaries around
// it, trimmed and capped at 500 bytes.
func EvidenceSentence(text string, start, end int) string {
	if text == "" {
		return ""
	}
	if start < 0 {
		start = 0
	}
	if start > len(text) {
		start = len(text)
	}
	if end < start {
		end = start
	}
	if end > len(text) {
		end = len(text)
	}
	for start > 0 && start < len(text) && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}

	lo := start
	for lo > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:lo])
		if isSentenceBoundary(r) {
			break
		}
		lo -= size
	}
	hi := end
	for hi < len(text) {
		r, size := utf8.DecodeRuneInString(text[hi:])
		hi += size
		if isSentenceBoundary(r) {
			break
		}
	}
	ev := strings.TrimSpace(text[lo:hi])
	if len(ev) > 500 {
		cut := 500
		for cut > 0 && cut < len(ev) && !utf8.RuneStart(ev[cut]) {
			cut--
		}
		ev = strings.TrimSpace(ev[:cut])
	}
	return ev
}

func isSentenceBoundary(r rune) bool {
	return r == '.' || r == '!' || r == '?' || r == '…' || r == '\n'
}

// judgeModeActive reports whether a mode selects facts for judging.
func judgeModeActive(mode string) bool {
	switch mode {
	case "low-confidence", "sample", "all":
		return true
	}
	return false
}

// DefaultLabels returns copies of the built-in label sets (the same ones
// the pipeline uses when no preset is requested).
func DefaultLabels() ([]extract.Label, []extract.Label) {
	entities := append([]extract.Label(nil), defaultEntityLabels...)
	relations := append([]extract.Label(nil), defaultRelationLabels...)
	return entities, relations
}
