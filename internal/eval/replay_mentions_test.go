package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func TestReplayMentionsWriteHeader(t *testing.T) {
	var output bytes.Buffer
	m := replayMentions{enc: json.NewEncoder(&output)}
	preset := ingest.CheckedPreset{
		Entities: []extract.Label{
			{Name: "item", Text: "Élan & item", Threshold: .00001},
			{Name: "value", Threshold: .5, Scope: "document"},
		},
		Relations: []extract.Label{{Name: "uses", Text: "employs", Threshold: .05}},
	}
	if err := m.writeHeader(replayHeader{}, preset); err != nil {
		t.Fatal(err)
	}
	fields := replayMentionHeaderFields(t, output.Bytes())
	const wantLabels = `{"entities":[{"name":"item","text":"Élan \u0026 item","threshold":0.00001,"scope":""},{"name":"value","text":"value","threshold":0.5,"scope":"document"}],"relations":[{"name":"uses","text":"employs","threshold":0.05}]}`
	if labels := fields["labels"]; string(labels) != wantLabels {
		t.Errorf("labels bytes = %s, want %s", labels, wantLabels)
	}
	wantHash := fmt.Sprintf(`"%x"`, sha256.Sum256(fields["labels"]))
	if got := string(fields["labels_sha256"]); got != wantHash {
		t.Errorf("labels_sha256 = %s, want %s", got, wantHash)
	}
	if _, ok := fields["sidecar_model"]; ok {
		t.Error("empty sidecar model was emitted")
	}
}

func TestWriteReplayHeadersAttemptID(t *testing.T) {
	original := newReplayAttemptID
	t.Cleanup(func() { newReplayAttemptID = original })
	const nonce = "00112233445566778899aabbccddeeff"
	calls := 0
	newReplayAttemptID = func() (string, error) {
		calls++
		return nonce, nil
	}
	var predictions, mentions bytes.Buffer
	enc := json.NewEncoder(&predictions)
	m := replayMentions{enc: json.NewEncoder(&mentions)}
	header := replayHeader{Type: "header", SidecarModel: "test-model"}
	if err := writeReplayHeaders(enc, &m, header, ingest.CheckedPreset{}); err != nil {
		t.Fatal(err)
	}
	for _, output := range []*bytes.Buffer{&predictions, &mentions} {
		fields := replayMentionHeaderFields(t, output.Bytes())
		if got := string(fields["attempt_id"]); got != `"`+nonce+`"` {
			t.Errorf("attempt_id = %s, want %q", got, nonce)
		}
		if got := string(fields["sidecar_model"]); got != `"test-model"` {
			t.Errorf("sidecar_model = %s, want %q", got, header.SidecarModel)
		}
	}
	if calls != 1 {
		t.Errorf("attempt ID generator called %d times, want 1", calls)
	}

	predictions.Reset()
	mentions.Reset()
	failure := errors.New("random source unavailable")
	newReplayAttemptID = func() (string, error) { return "", failure }
	if err := writeReplayHeaders(enc, &m, header, ingest.CheckedPreset{}); !errors.Is(err, failure) {
		t.Errorf("writeReplayHeaders error = %v, want %v", err, failure)
	}
	if predictions.Len() != 0 || mentions.Len() != 0 {
		t.Error("attempt ID failure wrote header bytes")
	}
}

func TestReplayMentionsWriteChunk(t *testing.T) {
	var output bytes.Buffer
	m := replayMentions{enc: json.NewEncoder(&output), counts: mentionsCounts{ChunksRequested: 3}}
	doc := sampleDocument{DocID: "doc"}
	assessment := replayAssessment{
		raw: extract.ChunkResult{
			Mentions: []extract.Mention{
				{Label: "item", Text: "Élan Ltd", Start: 0, End: 9, Score: .9},
				{Label: "value", Text: "Velrix", Start: 15, End: 21, Score: .8},
				{Label: "item", Text: "Desquon", Start: 26, End: 33, Score: .7},
			},
			Candidates: []extract.Candidate{
				{Head: 0, Tail: 1, Label: "uses", Score: .4},
				{Head: 0, Tail: 1, Label: "uses", Score: .1},
				{Head: 2, Tail: 1, Label: "uses", Score: .9},
			},
			Diagnostics: []extract.Diagnostic{
				{Type: "other"},
				{Type: "ambiguous_relation", Label: "uses", Score: .7, HeadCandidates: []int{0, 2}, TailCandidates: []int{1}},
			},
		},
		ReplayAssessment: ingest.ReplayAssessment{
			Mentions: []ingest.MentionDecision{
				{Name: "Élan", Key: "élan", Aliases: []string{"Élan Ltd"}},
				{Name: "Velrix", Key: "velrix"},
				{DropRule: "stoplist"},
			},
			Occurrences: []ingest.OccurrenceDecision{
				{Disposition: "kept", LowConfidence: true, StoredWinner: true},
				{Disposition: "floor:store_floor"},
				{Disposition: "hygiene:stoplist"},
			},
		},
	}
	if err := m.writeChunk(doc, sampleChunk{ChunkID: "root"}, assessment, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.writeChunk(doc, sampleChunk{ChunkID: "empty"}, replayAssessment{}, ""); err != nil {
		t.Fatal(err)
	}
	if m.trailerCounts().Complete {
		t.Error("trailer complete before all requested chunks have rows")
	}
	if err := m.writeChunk(doc, sampleChunk{ChunkID: "duplicate", DuplicateOf: "root"}, assessment, "root"); err != nil {
		t.Fatal(err)
	}
	assertReplayMentionsRows(t, output.Bytes(),
		`{"type":"chunk","chunk_id":"root","doc_id":"doc","status":"ok","error":"","entities":3,"relations":3,"ambiguous_relations":1}`,
		`{"type":"entity","chunk_id":"root","index":0,"start":0,"end":9,"label":"item","text":"Élan Ltd","score":0.9,"hygiene":{"outcome":"kept","name":"Élan","aliases":["Élan Ltd"],"key":"élan"}}`,
		`{"type":"entity","chunk_id":"root","index":1,"start":15,"end":21,"label":"value","text":"Velrix","score":0.8,"hygiene":{"outcome":"kept","name":"Velrix","aliases":[],"key":"velrix"}}`,
		`{"type":"entity","chunk_id":"root","index":2,"start":26,"end":33,"label":"item","text":"Desquon","score":0.7,"hygiene":{"outcome":"dropped","rule":"stoplist"}}`,
		`{"type":"relation","chunk_id":"root","index":0,"label":"uses","score":0.4,"head":{"index":0,"start":0,"end":9,"label":"item"},"tail":{"index":1,"start":15,"end":21,"label":"value"},"disposition":"kept","low_confidence":true,"stored_winner":true}`,
		`{"type":"relation","chunk_id":"root","index":1,"label":"uses","score":0.1,"head":{"index":0,"start":0,"end":9,"label":"item"},"tail":{"index":1,"start":15,"end":21,"label":"value"},"disposition":"floor:store_floor","low_confidence":false,"stored_winner":false}`,
		`{"type":"relation","chunk_id":"root","index":2,"label":"uses","score":0.9,"head":{"index":2,"start":26,"end":33,"label":"item"},"tail":{"index":1,"start":15,"end":21,"label":"value"},"disposition":"hygiene:stoplist","low_confidence":false,"stored_winner":false}`,
		`{"type":"ambiguous_relation","chunk_id":"root","index":1,"label":"uses","score":0.7,"head_candidates":[{"index":0,"start":0,"end":9,"label":"item"},{"index":2,"start":26,"end":33,"label":"item"}],"tail_candidates":[{"index":1,"start":15,"end":21,"label":"value"}]}`,
		`{"type":"chunk","chunk_id":"empty","doc_id":"doc","status":"empty","error":"","entities":0,"relations":0,"ambiguous_relations":0}`,
		`{"type":"chunk","chunk_id":"duplicate","doc_id":"doc","status":"borrowed","source_chunk_id":"root","error":"","entities":0,"relations":0,"ambiguous_relations":0}`,
	)
	want := mentionsCounts{ChunksRequested: 3, ChunksOK: 1, ChunksEmpty: 1, ChunksBorrowed: 1, Entities: 3, Relations: 3, AmbiguousRelations: 1, Complete: true}
	if got := m.trailerCounts(); got != want {
		t.Errorf("trailer counts = %+v, want %+v", got, want)
	}
}

func TestReplayMentionsFailedBorrow(t *testing.T) {
	var output bytes.Buffer
	m := replayMentions{enc: json.NewEncoder(&output), counts: mentionsCounts{ChunksRequested: 1}}
	assessment := replayAssessment{err: errors.New("root failed")}
	if err := m.writeChunk(sampleDocument{DocID: "doc"}, sampleChunk{ChunkID: "duplicate", DuplicateOf: "root"}, assessment, "root"); err != nil {
		t.Fatal(err)
	}
	assertReplayMentionsRows(t, output.Bytes(),
		`{"type":"chunk","chunk_id":"duplicate","doc_id":"doc","status":"error","error":"root failed","entities":0,"relations":0,"ambiguous_relations":0}`,
	)
	if got, want := m.trailerCounts(), (mentionsCounts{ChunksRequested: 1, ChunksError: 1}); got != want {
		t.Errorf("trailer counts = %+v, want %+v", got, want)
	}
}

func replayMentionHeaderFields(t *testing.T, header []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(header, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func assertReplayMentionsRows(t *testing.T, output []byte, want ...string) {
	t.Helper()
	rows := bytes.Split(bytes.TrimSuffix(output, []byte("\n")), []byte("\n"))
	if len(rows) != len(want) {
		t.Fatalf("wrote %d rows, want %d:\n%s", len(rows), len(want), output)
	}
	for i, row := range rows {
		var got, expected map[string]any
		if err := json.Unmarshal(row, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(want[i]), &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("row %d = %s, want %s", i, row, want[i])
		}
	}
}
