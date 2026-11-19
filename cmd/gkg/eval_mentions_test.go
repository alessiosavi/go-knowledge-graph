package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

func TestEvalMentionsArtifact(t *testing.T) {
	setupEvalMentions(t)
	const text = "Élovan Ltd uses Velrix and Desquon."
	// The sample preserves file order. Replay requests and predictions use sequence order.
	writeMentionsSample(t, true, []mentionSampleChunk{
		{id: "entity-only", text: text, seq: 1},
		{id: "relations", text: text, seq: 0},
		{id: "shared-span", text: text, seq: 2},
	})
	var checkOutputs atomic.Bool
	checkOutputs.Store(true)
	newMentionsSidecar(t, func() {
		if checkOutputs.Load() {
			for _, path := range []string{"first-predictions.jsonl", "first-mentions.jsonl"} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Errorf("before sidecar health: %s mode or stat error: %v, %v", path, info, err)
				}
			}
		}
	}, func(chunks []extract.RequestChunk) any {
		results := make([]map[string]any, 0, len(chunks))
		for _, c := range chunks {
			head := wireEvalMention(text, "Élovan Ltd", "Synthetic & item")
			tail := wireEvalMention(text, "Velrix", "value")
			dropped := wireEvalMention(text, "Desquon", "Synthetic & item")
			result := map[string]any{"chunk_id": c.ID}
			switch c.ID {
			case "entity-only":
				result["entities"] = []any{head, dropped}
			case "relations":
				result["entities"] = []any{head, tail, dropped}
				result["relations"] = []any{
					map[string]any{"head": 0, "tail": 1, "label": "employs", "score": .4},
					map[string]any{"head": 0, "tail": 1, "label": "employs", "score": .1},
					map[string]any{"head": 2, "tail": 1, "label": "employs", "score": .8},
				}
			case "shared-span":
				invalid := wireEvalMention(text, "Velrix", "unrequested entity")
				otherHead := wireEvalMention(text, "Élovan Ltd", "value")
				result["entities"] = []any{invalid, head, otherHead, tail}
				result["diagnostics"] = []any{map[string]any{"type": "ambiguous_relation",
					"label": "employs", "score": .7, "head_candidates": []int{0, 1, 2}, "tail_candidates": []int{3}}}
			default:
				t.Errorf("unexpected chunk %q", c.ID)
			}
			results = append(results, result)
		}
		return results
	})

	const summary = "chunks=3 ok=1 empty=2 errors=0 occurrences=3 stored_winners=1 duplicate_roots_outside_sample=0 duplicate_root_cycles=0\n"
	runPair := func(prefix string) ([]map[string]any, []map[string]any, []byte, []byte) {
		p, m := prefix+"-predictions.jsonl", prefix+"-mentions.jsonl"
		assertEvalCLI(t, mentionReplayArgs(p, m), 0, summary, "")
		predictions, pBytes := readEvalArtifact(t, p)
		mentions, mBytes := readEvalArtifact(t, m)
		return predictions, mentions, pBytes, mBytes
	}
	p, m, p1, m1 := runPair("first")
	checkOutputs.Store(false)
	pAgain, mAgain, p2, m2 := runPair("second")
	id := assertEvalAttempt(t, p[0], m[0])
	idAgain := assertEvalAttempt(t, pAgain[0], mAgain[0])
	if id == idAgain {
		t.Fatal("two replays used the same attempt ID")
	}
	for _, pair := range [][2][]byte{{p1, p2}, {m1, m2}} {
		if !bytes.Equal(normalizeEvalAttempt(t, pair[0], id), normalizeEvalAttempt(t, pair[1], idAgain)) {
			t.Error("artifact bytes differ after normalizing the attempt ID and checksum")
		}
	}
	assertEvalCLI(t, []string{"eval", "-replay", "sample.jsonl", "-predictions", "legacy.jsonl", "-labels", "synthetic"}, 0, summary, "")
	_, legacy := readEvalArtifact(t, "legacy.jsonl")
	if !bytes.Equal(legacy, rewriteEvalAttempt(t, p1, id, "")) {
		t.Error("predictions with mentions differ from legacy predictions beyond the attempt ID and checksum")
	}

	header := m[0]
	if header["format"] != "gkg-mentions/v1" || header["offsets"] != "utf8_bytes_chunk" || header["no_borrow"] != false {
		t.Errorf("mentions header = %+v", header)
	}
	for _, field := range []string{"input_sha256", "preset", "preset_sha256", "hygiene", "write_mode", "store_floor", "assert_floor",
		"sidecar_model", "sidecar_image", "sidecar_image_digest", "extractor_version", "gkg_version"} {
		if value, ok := header[field]; !ok || !reflect.DeepEqual(value, p[0][field]) {
			t.Errorf("mentions header %s = %+v, want %+v", field, value, p[0][field])
		}
	}
	const labels = `{"entities":[{"name":"item","text":"Synthetic \u0026 item","threshold":0.4,"scope":""},{"name":"value","text":"value","threshold":0.5,"scope":"document"}],"relations":[{"name":"uses","text":"employs","threshold":0.05}]}`
	var decodedLabels any
	if err := json.Unmarshal([]byte(labels), &decodedLabels); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(header["labels"], decodedLabels) || header["labels_sha256"] != fmt.Sprintf("%x", sha256.Sum256([]byte(labels))) {
		t.Errorf("labels or hash = %+v, %v", header["labels"], header["labels_sha256"])
	}
	var rawHeader struct{ Labels json.RawMessage }
	firstLine, _, _ := bytes.Cut(m1, []byte("\n"))
	if err := json.Unmarshal(firstLine, &rawHeader); err != nil || string(rawHeader.Labels) != labels {
		t.Errorf("encoded labels = %s, %v", rawHeader.Labels, err)
	}

	var order []string
	for _, row := range m[1 : len(m)-1] {
		order = append(order, fmt.Sprintf("%v/%v", row["chunk_id"], row["type"]))
	}
	wantOrder := []string{
		"entity-only/chunk", "entity-only/entity", "entity-only/entity",
		"relations/chunk", "relations/entity", "relations/entity", "relations/entity", "relations/relation", "relations/relation", "relations/relation",
		"shared-span/chunk", "shared-span/entity", "shared-span/entity", "shared-span/entity", "shared-span/ambiguous_relation",
	}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("mentions row order = %v, want %v", order, wantOrder)
	}
	if p[5]["chunk_id"] != "entity-only" || p[5]["status"] != "empty" ||
		p[6]["chunk_id"] != "shared-span" || p[6]["status"] != "empty" {
		t.Errorf("predictions for chunks without relations = %+v", p[5:7])
	}
	assertMentionsChunk(t, m[1], "entity-only", "ok", 2, 0, 0)
	if m[1]["doc_id"] != "doc-synthetic" || m[1]["product_id"] != float64(42) ||
		!reflect.DeepEqual(m[1]["kind"], map[string]any{"name": "record"}) {
		t.Errorf("mentions chunk metadata = %+v", m[1])
	}
	assertMentionsEntity(t, m[2], 0, "item", "Élovan Ltd", 0, map[string]any{
		"outcome": "kept", "name": "Élovan", "aliases": []any{"Élovan Ltd"}, "key": graph.DomainKey("synthra", graph.EntityKey("Élovan")),
	})
	assertMentionsEntity(t, m[3], 1, "item", "Desquon", strings.Index(text, "Desquon"), map[string]any{
		"outcome": "dropped", "rule": "stoplist",
	})

	assertMentionsChunk(t, m[4], "relations", "ok", 3, 3, 0)
	assertMentionsEntity(t, m[6], 1, "value", "Velrix", strings.Index(text, "Velrix"), map[string]any{
		"outcome": "kept", "name": "Velrix", "aliases": []any{}, "key": graph.ScopedEntityKey("doc-synthetic", "Velrix"),
	})
	for i, want := range []string{"kept", "floor:store_floor", "hygiene:stoplist"} {
		rel, occurrence := m[8+i], p[2+i]
		if rel["index"] != float64(i) || rel["label"] != occurrence["relation"] || rel["score"] != occurrence["score"] ||
			rel["disposition"] != want || rel["disposition"] != occurrence["disposition"] ||
			rel["low_confidence"] != (i == 0) || rel["low_confidence"] != occurrence["low_confidence"] ||
			rel["stored_winner"] != (i == 0) || rel["stored_winner"] != occurrence["stored_winner"] {
			t.Errorf("relation %d = %+v, occurrence = %+v", i, rel, occurrence)
		}
		headIndex := 0
		if i == 2 {
			headIndex = 2
		}
		for name, index := range map[string]int{"head": headIndex, "tail": 1} {
			endpoint := occurrence[name].(map[string]any)
			wantEndpoint := map[string]any{"index": float64(index), "start": endpoint["start"], "end": endpoint["end"], "label": endpoint["type"]}
			if !reflect.DeepEqual(rel[name], wantEndpoint) {
				t.Errorf("relation %d %s = %+v, want %+v", i, name, rel[name], wantEndpoint)
			}
		}
	}

	assertMentionsChunk(t, m[11], "shared-span", "ok", 3, 0, 1)
	ambiguous := m[15]
	velrix := strings.Index(text, "Velrix")
	wantHead := []any{
		mentionEndpoint(0, 0, len("Élovan Ltd"), "item"),
		mentionEndpoint(1, 0, len("Élovan Ltd"), "value"),
	}
	wantTail := []any{mentionEndpoint(2, velrix, velrix+len("Velrix"), "value")}
	if ambiguous["index"] != float64(0) || ambiguous["label"] != "uses" || ambiguous["score"] != .7 ||
		!reflect.DeepEqual(ambiguous["head_candidates"], wantHead) || !reflect.DeepEqual(ambiguous["tail_candidates"], wantTail) {
		t.Errorf("remapped ambiguous relation = %+v", ambiguous)
	}
	assertMentionsCounts(t, m, 3, 3, 0, 0, 0, 8, 3, 1, true)
}

func TestEvalMentionsStrictResults(t *testing.T) {
	setupEvalMentions(t)
	writeMentionsSample(t, false, []mentionSampleChunk{
		{id: "recovers", text: "Élovan.", seq: 0}, {id: "breaks", text: "Velrix.", seq: 1},
	})

	for _, tc := range []struct {
		name, message string
		batch, single []string
	}{
		{"missing", `missing result for chunk "breaks"`, []string{"recovers"}, nil},
		{"duplicate", `duplicate result for chunk "breaks"`, []string{"recovers", "breaks", "breaks"}, []string{"breaks", "breaks"}},
		{"unrequested", `unrequested chunk "intruder"`, []string{"recovers", "breaks", "intruder"}, []string{"breaks", "intruder"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var calls [][]string
			newMentionsSidecar(t, nil, func(chunks []extract.RequestChunk) any {
				ids := make([]string, 0, len(chunks))
				for _, c := range chunks {
					ids = append(ids, c.ID)
				}
				mu.Lock()
				calls = append(calls, ids)
				mu.Unlock()
				switch {
				case len(ids) == 2:
					return emptyEvalResults(tc.batch...)
				case reflect.DeepEqual(ids, []string{"breaks"}):
					return emptyEvalResults(tc.single...)
				default:
					return emptyEvalResults(ids...)
				}
			})
			p, m := tc.name+"-predictions.jsonl", tc.name+"-mentions.jsonl"
			summary := "chunks=2 ok=0 empty=1 errors=1 occurrences=0 stored_winners=0 duplicate_roots_outside_sample=0 duplicate_root_cycles=0\n"
			assertEvalCLI(t, mentionReplayArgs(p, m), 1, summary, "")
			predictions, _ := readEvalArtifact(t, p)
			rows, _ := readEvalArtifact(t, m)
			assertEvalAttempt(t, predictions[0], rows[0])
			assertMentionsChunk(t, rows[1], "recovers", "empty", 0, 0, 0)
			assertMentionsChunk(t, rows[2], "breaks", "error", 0, 0, 0)
			if !strings.Contains(fmt.Sprint(rows[2]["error"]), tc.message) || rows[2]["error"] != predictions[2]["error"] {
				t.Errorf("mentions error = %q, predictions = %q, want %q", rows[2]["error"], predictions[2]["error"], tc.message)
			}
			assertMentionsCounts(t, rows, 2, 0, 1, 0, 1, 0, 0, 0, false)
			mu.Lock()
			defer mu.Unlock()
			if want := [][]string{{"recovers", "breaks"}, {"recovers"}, {"breaks"}}; !reflect.DeepEqual(calls, want) {
				t.Errorf("sidecar calls = %v, want %v", calls, want)
			}
		})
	}
}

func TestEvalMentionsNoBorrow(t *testing.T) {
	setupEvalMentions(t)
	const rootText, duplicateText = "Élovan uses Velrix.", "Desvyr uses Velrix."
	writeMentionsSample(t, false, []mentionSampleChunk{
		{id: "duplicate", text: duplicateText, duplicateOf: "root", seq: 0},
		{id: "root", text: rootText, seq: 1},
		{id: "outside", text: duplicateText, duplicateOf: "unsampled", seq: 2},
	})
	var mu sync.Mutex
	var batches [][]string
	var failRoot atomic.Bool
	newMentionsSidecar(t, nil, func(chunks []extract.RequestChunk) any {
		ids := make([]string, 0, len(chunks))
		results := make([]map[string]any, 0, len(chunks))
		for _, c := range chunks {
			ids = append(ids, c.ID)
			if c.ID == "root" && failRoot.Load() {
				continue // A missing result fails single-chunk retry as well.
			}
			head, _, _ := strings.Cut(c.Text, " ")
			results = append(results, map[string]any{"chunk_id": c.ID,
				"entities":  []any{wireEvalMention(c.Text, head, "item"), wireEvalMention(c.Text, "Velrix", "value")},
				"relations": []any{map[string]any{"head": 0, "tail": 1, "label": "employs", "score": .9}},
			})
		}
		mu.Lock()
		batches = append(batches, ids)
		mu.Unlock()
		return results
	})
	const localSummary = "chunks=3 ok=3 empty=0 errors=0 occurrences=3 stored_winners=3 duplicate_roots_outside_sample=1 duplicate_root_cycles=0\n"
	args := append(mentionReplayArgs("local.jsonl", "local-mentions.jsonl"), "-no-borrow")
	assertEvalCLI(t, args, 0, localSummary, "")
	p, paired := readEvalArtifact(t, "local.jsonl")
	m, _ := readEvalArtifact(t, "local-mentions.jsonl")
	if p[0]["no_borrow"] != true || m[0]["no_borrow"] != true {
		t.Errorf("no_borrow missing from headers: %+v, %+v", p[0], m[0])
	}
	for _, index := range []int{2, 6} {
		row := p[index]
		if _, ok := row["source_chunk_id"]; ok || row["stored_winner"] != true ||
			row["head"].(map[string]any)["text"] != "Desvyr" {
			t.Errorf("expected local prediction, got %+v", row)
		}
	}
	if p[5]["duplicate_root"] != "outside_sample" || m[2]["text"] != "Desvyr" || m[4]["stored_winner"] != true {
		t.Errorf("expected informational root and local evidence, got %+v, %+v, %+v", p[5], m[2], m[4])
	}
	for _, field := range []string{"product_id", "kind", "source_chunk_id"} {
		if _, ok := m[1][field]; ok {
			t.Errorf("graph sample added %s: %+v", field, m[1])
		}
	}
	assertMentionsCounts(t, m, 3, 3, 0, 0, 0, 6, 3, 0, true)
	args = []string{"eval", "-replay", "sample.jsonl", "-predictions", "local-unpaired.jsonl", "-labels", "synthetic", "-no-borrow"}
	assertEvalCLI(t, args, 0, localSummary, "")
	_, unpaired := readEvalArtifact(t, "local-unpaired.jsonl")
	if id := assertEvalAttempt(t, p[0], m[0]); !bytes.Equal(unpaired, rewriteEvalAttempt(t, paired, id, "")) {
		t.Error("unpaired -no-borrow predictions differ beyond the attempt ID and checksum")
	}

	assertEvalCLI(t, mentionReplayArgs("borrowed.jsonl", "borrowed-mentions.jsonl"), 0,
		"chunks=3 ok=2 empty=1 errors=0 occurrences=2 stored_winners=1 duplicate_roots_outside_sample=1 duplicate_root_cycles=0\n", "")
	borrowed, _ := readEvalArtifact(t, "borrowed-mentions.jsonl")
	assertMentionsChunk(t, borrowed[1], "duplicate", "borrowed", 0, 0, 0)
	for i, row := range borrowed {
		source, ok := row["source_chunk_id"]
		if (i == 1 && source != "root") || (i != 1 && ok) {
			t.Errorf("unexpected source_chunk_id on mentions row %d: %+v", i, row)
		}
	}
	assertMentionsChunk(t, borrowed[2], "root", "ok", 2, 1, 0)
	assertMentionsChunk(t, borrowed[6], "outside", "empty", 0, 0, 0)
	assertMentionsCounts(t, borrowed, 3, 1, 1, 1, 0, 2, 1, 0, true)

	failRoot.Store(true)
	assertEvalCLI(t, mentionReplayArgs("failed.jsonl", "failed-mentions.jsonl"), 1,
		"chunks=3 ok=0 empty=1 errors=2 occurrences=0 stored_winners=0 duplicate_roots_outside_sample=1 duplicate_root_cycles=0\n", "")
	failed, _ := readEvalArtifact(t, "failed-mentions.jsonl")
	assertMentionsChunk(t, failed[1], "duplicate", "error", 0, 0, 0)
	assertMentionsChunk(t, failed[2], "root", "error", 0, 0, 0)
	if _, ok := failed[1]["source_chunk_id"]; ok || failed[1]["error"] == "" || failed[1]["error"] != failed[2]["error"] {
		t.Errorf("duplicate did not report the root error: %+v, root: %+v", failed[1], failed[2])
	}
	assertMentionsCounts(t, failed, 3, 0, 1, 0, 2, 0, 0, 0, false)
	mu.Lock()
	defer mu.Unlock()
	if want := [][]string{{"duplicate", "root", "outside"}, {"duplicate", "root", "outside"}, {"root"}, {"root"}, {"root"}}; !reflect.DeepEqual(batches, want) {
		t.Errorf("sidecar batches = %v, want %v", batches, want)
	}
}

func TestEvalMentionsFlags(t *testing.T) {
	t.Chdir(t.TempDir())
	assertEvalCLI(t, []string{"eval", "-mentions", "mentions.jsonl"}, 2, "", "-mentions requires -replay")
	assertEvalCLI(t, []string{"eval", "-no-borrow=false"}, 2, "", "-no-borrow requires -replay")
	assertEvalCLI(t, mentionReplayArgs("predictions.jsonl", "./predictions.jsonl"), 2, "", "different")
	assertEvalCLI(t, mentionReplayArgs("predictions.jsonl", "./sample.jsonl"), 2, "", "different")
	if entries, err := os.ReadDir("."); err != nil || len(entries) != 0 {
		t.Errorf("invalid flags wrote files: %v, %v", entries, err)
	}
}

func TestEvalMentionsRollback(t *testing.T) {
	setupEvalMentions(t)
	writeMentionsSample(t, false, []mentionSampleChunk{{id: "only", text: "Élovan."}})
	var calls atomic.Int32
	newMentionsSidecar(t, func() { calls.Add(1) }, func(chunks []extract.RequestChunk) any {
		calls.Add(1)
		return emptyEvalResults(chunks[0].ID)
	})
	for _, tc := range []struct{ existing, absent string }{
		{"predictions.jsonl", "mentions.jsonl"},
		{"mentions.jsonl", "predictions.jsonl"},
	} {
		t.Run(tc.existing, func(t *testing.T) {
			writeEvalFile(t, tc.existing, []byte("do not overwrite"))
			t.Cleanup(func() { _ = os.Remove(tc.existing) })
			assertEvalCLI(t, mentionReplayArgs("predictions.jsonl", "mentions.jsonl"), 2, "", "file exists")
			assertMissingEvalFile(t, tc.absent)
			if data, err := os.ReadFile(tc.existing); err != nil || string(data) != "do not overwrite" {
				t.Errorf("existing output changed: %q, %v", data, err)
			}
		})
	}
	writeEvalFile(t, "sample.jsonl", []byte("invalid sample"))
	assertEvalCLI(t, mentionReplayArgs("predictions.jsonl", "mentions.jsonl"), 1, "", "read sample")
	assertMissingEvalFile(t, "predictions.jsonl")
	assertMissingEvalFile(t, "mentions.jsonl")
	if calls.Load() != 0 {
		t.Errorf("invalid replay made %d sidecar calls", calls.Load())
	}
}

func setupEvalMentions(t *testing.T) {
	t.Helper()
	_ = preflightCommandStore(t)
	migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema', value:$schema})`, map[string]any{"schema": graph.ExtractionSchemaVersion})
	t.Setenv("GKG_LABELS_PRESETS_DIR", ".")
	t.Setenv("GKG_INGEST_STOPLIST_FILE", "")
	t.Setenv("GKG_INGEST_STORE_FLOOR", "0.20")
	t.Setenv("GKG_INGEST_ASSERT_FLOOR", "0.55")
	t.Setenv("GKG_EXTRACTOR_SIDECAR_IMAGE", "sha256:"+strings.Repeat("3f", 32))
	writeEvalFile(t, "synthetic.yaml", []byte(`entities:
  - name: item
    text: Synthetic & item
    threshold: 0.4
  - name: value
    threshold: 0.5
    scope: document
relations:
  - name: uses
    text: employs
    threshold: 0.05
stoplist:
  exact: [Desquon]
`))
}

type mentionSampleChunk struct {
	id, text, duplicateOf string
	seq                   int
}

func writeMentionsSample(t *testing.T, metadata bool, chunks []mentionSampleChunk) {
	t.Helper()
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	encode := func(row map[string]any) {
		t.Helper()
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	counts := map[string]int{"documents": 1, "chunks": len(chunks)}
	header := map[string]any{"type": "header", "domain": "synthra", "counts": counts}
	doc := map[string]any{"type": "document", "doc_id": "doc-synthetic", "labels": "synthetic"}
	if metadata {
		header["origin"] = "from-file"
		doc["product_id"], doc["kind"] = 42, map[string]string{"name": "record"}
	}
	encode(header)
	encode(doc)
	for _, chunk := range chunks {
		encode(map[string]any{"type": "chunk", "doc_id": "doc-synthetic", "chunk_id": chunk.id, "seq": chunk.seq, "text": chunk.text,
			"start": 0, "end": len(chunk.text), "duplicate_of": chunk.duplicateOf})
	}
	encode(map[string]any{"type": "trailer", "counts": counts, "sha256": fmt.Sprintf("%x", sha256.Sum256(data.Bytes()))})
	writeEvalFile(t, "sample.jsonl", data.Bytes())
}

func newMentionsSidecar(t *testing.T, health func(), results func([]extract.RequestChunk) any) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /healthz":
			if health != nil {
				health()
			}
			fmt.Fprint(w, `{"status":"ok","model":"synthetic-sidecar"}`)
		case "POST /v1/extract":
			var req struct {
				Chunks []extract.RequestChunk `json:"chunks"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Chunks) == 0 {
				t.Errorf("unexpected request: %+v, %v", req, err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"results": results(req.Chunks)}); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected sidecar request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("GKG_EXTRACTOR_SIDECAR_URL", server.URL)
}

func emptyEvalResults(ids ...string) []map[string]string {
	results := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		results = append(results, map[string]string{"chunk_id": id})
	}
	return results
}

func wireEvalMention(chunk, text, label string) map[string]any {
	start := strings.Index(chunk, text)
	return map[string]any{"label": label, "text": text, "start": start, "end": start + len(text), "score": .9}
}

func mentionEndpoint(index, start, end int, label string) map[string]any {
	return map[string]any{"index": float64(index), "start": float64(start), "end": float64(end), "label": label}
}

func mentionReplayArgs(predictions, mentions string) []string {
	return []string{"eval", "-replay", "sample.jsonl", "-labels", "synthetic", "-predictions", predictions, "-mentions", mentions}
}

func assertEvalAttempt(t *testing.T, predictions, mentions map[string]any) string {
	t.Helper()
	id, _ := mentions["attempt_id"].(string)
	decoded, err := hex.DecodeString(id)
	if len(decoded) != 16 || err != nil || id != predictions["attempt_id"] {
		t.Fatalf("invalid or unpaired attempt IDs %q and %q", id, predictions["attempt_id"])
	}
	return id
}

func normalizeEvalAttempt(t *testing.T, data []byte, id string) []byte {
	t.Helper()
	return rewriteEvalAttempt(t, data, id, strings.Repeat("0", 32))
}

func rewriteEvalAttempt(t *testing.T, data []byte, oldID, newID string) []byte {
	t.Helper()
	header, rest, _ := bytes.Cut(data, []byte("\n"))
	attempt := []byte(fmt.Sprintf(`"attempt_id":"%s"`, oldID))
	updated := []byte(fmt.Sprintf(`"attempt_id":"%s"`, newID))
	if newID == "" {
		// Allow either the first or the last field position to carry the attempt.
		updated = nil
		if bytes.Contains(header, append(bytes.Clone(attempt), ',')) {
			attempt = append(attempt, ',')
		} else {
			attempt = append([]byte{','}, attempt...)
		}
	}
	if !bytes.Contains(header, attempt) {
		t.Fatal("attempt ID missing from header bytes")
	}
	header = bytes.Replace(header, attempt, updated, 1)
	separator := bytes.LastIndexByte(bytes.TrimSuffix(rest, []byte("\n")), '\n') + 1
	prefix := append(append(header, '\n'), rest[:separator]...)
	digest := []byte(fmt.Sprintf(`"sha256":"%x"`, sha256.Sum256(prefix)))
	trailer := regexp.MustCompile(`"sha256":"[a-f0-9]{64}"`).ReplaceAllLiteral(rest[separator:], digest)
	return append(prefix, trailer...)
}

func assertMentionsChunk(t *testing.T, row map[string]any, id, status string, entities, relations, ambiguous int) {
	t.Helper()
	if row["type"] != "chunk" || row["chunk_id"] != id || row["status"] != status ||
		row["entities"] != float64(entities) || row["relations"] != float64(relations) || row["ambiguous_relations"] != float64(ambiguous) ||
		(status != "error" && row["error"] != "") {
		t.Errorf("mentions chunk %q = %+v, want %s with counts %d, %d, %d", id, row, status, entities, relations, ambiguous)
	}
}

func assertMentionsEntity(t *testing.T, row map[string]any, index int, label, text string, start int, hygiene map[string]any) {
	t.Helper()
	if row["index"] != float64(index) || row["label"] != label || row["text"] != text || row["score"] != .9 ||
		row["start"] != float64(start) || row["end"] != float64(start+len(text)) || !reflect.DeepEqual(row["hygiene"], hygiene) {
		t.Errorf("entity %d = %+v, want %q with hygiene %+v", index, row, text, hygiene)
	}
}

func assertMentionsCounts(t *testing.T, rows []map[string]any, requested, ok, empty, borrowed, failed, entities, relations, ambiguous int, complete bool) {
	t.Helper()
	want := map[string]any{"chunks_requested": float64(requested), "chunks_ok": float64(ok), "chunks_empty": float64(empty),
		"chunks_borrowed": float64(borrowed), "chunks_error": float64(failed), "entities": float64(entities), "relations": float64(relations),
		"ambiguous_relations": float64(ambiguous), "complete": complete}
	if got := rows[len(rows)-1]["counts"]; !reflect.DeepEqual(got, want) {
		t.Errorf("mentions counts = %+v, want %+v", got, want)
	}
}

func assertMissingEvalFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %q to be absent, stat error = %v", path, err)
	}
}
