package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

func TestEvalSampleReadOnly(t *testing.T) {
	_ = preflightCommandStore(t)
	migrationQuery(t, `
CREATE (a:Document {id:'doc-a', domain:'zorbax', status:'done', title:'Zorbax Notes', title_source:'synthetic', source:'source-a', labels:'synthetic'}),
       (b:Document {id:'doc-b', domain:'zorbax', status:'done', source:'source-b'}),
       (f:Document {id:'doc-f', domain:'zorbax', status:'failed'}),
       (x:Document {id:'doc-x', domain:'nimbrix', status:'done'}),
       (a)-[:HAS_CHUNK]->(:Chunk {id:'chunk-1', doc_id:'doc-a', seq:1, start:5, end:10, text:'Ézrk', duplicate_of:'chunk-x'}),
       (a)-[:HAS_CHUNK]->(:Chunk {id:'chunk-0', doc_id:'doc-a', seq:0, start:0, end:5, text:'Zorva'}),
       (b)-[:HAS_CHUNK]->(:Chunk {id:'chunk-2', doc_id:'doc-b', seq:0, start:0, end:5, text:'Nimbr'}),
       (f)-[:HAS_CHUNK]->(:Chunk {id:'chunk-f', doc_id:'doc-f', seq:0, text:'Failedonly'}),
       (x)-[:HAS_CHUNK]->(:Chunk {id:'chunk-x', doc_id:'doc-x', seq:0, text:'Outsideonly'})`, nil)
	before := graphtest.Snapshot(t)
	defer before.AssertUnchanged(t, "eval-sample")

	dir := t.TempDir()
	path := filepath.Join(dir, "sample.jsonl")
	args := []string{"eval-sample", "-domain", "zorbax", "-seed", "74", "-out", path}
	assertEvalCLI(t, args, 0, "documents=2 chunks=3\n", "")
	rows, data := readEvalArtifact(t, path)
	if len(rows) != 7 ||
		rows[0]["domain"] != "zorbax" || rows[0]["seed"] != float64(74) ||
		rows[1]["source"] != "source-a" || rows[1]["title"] != "Zorbax Notes" || rows[1]["title_source"] != "synthetic" || rows[1]["labels"] != "synthetic" ||
		rows[2]["chunk_id"] != "chunk-0" || rows[3]["doc_id"] != "doc-a" || rows[3]["chunk_id"] != "chunk-1" ||
		rows[3]["seq"] != float64(1) || rows[3]["start"] != float64(5) || rows[3]["end"] != float64(10) || rows[3]["text"] != "Ézrk" ||
		rows[3]["duplicate_of"] != "" || rows[3]["dup_doc_id"] != "" ||
		rows[4]["doc_id"] != "doc-b" || rows[4]["title"] != "" || rows[4]["title_source"] != "" || rows[4]["labels"] != "" ||
		rows[5]["chunk_id"] != "chunk-2" || rows[5]["duplicate_of"] != "" || rows[5]["dup_doc_id"] != "" ||
		strings.Contains(string(data), "Outsideonly") || strings.Contains(string(data), "Failedonly") {
		t.Fatalf("sample rows = %+v", rows)
	}
	wantCounts := map[string]any{"documents": float64(2), "chunks": float64(3)}
	if !reflect.DeepEqual(rows[0]["counts"], wantCounts) || !reflect.DeepEqual(rows[6]["counts"], wantCounts) {
		t.Errorf("sample counts = %v / %v", rows[0]["counts"], rows[6]["counts"])
	}
	assertEvalCLI(t, args, 1, "", "file exists")
	if _, again := readEvalArtifact(t, path); !bytes.Equal(again, data) {
		t.Fatal("existing sample was changed")
	}

	var first []byte
	for _, name := range []string{"sample-a.jsonl", "sample-b.jsonl"} {
		path := filepath.Join(dir, name)
		var out, errOut bytes.Buffer
		code := run([]string{"eval-sample", "-domain", "zorbax", "-docs", "1", "-seed", "74", "-out", path}, &out, &errOut)
		if code != 0 || errOut.Len() != 0 || !strings.HasPrefix(out.String(), "documents=1 chunks=") {
			t.Fatalf("limited sample exit=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
		}
		_, data := readEvalArtifact(t, path)
		if first != nil && !bytes.Equal(first, data) {
			t.Fatal("same seed produced different samples")
		}
		first = data
	}
}

func TestEvalReplayDuplicateRoots(t *testing.T) {
	_ = preflightCommandStore(t)
	t.Setenv("GKG_INGEST_STOPLIST_FILE", "")
	t.Setenv("GKG_EXTRACTOR_SIDECAR_IMAGE", "sha256:"+strings.Repeat("3f", 32))
	for _, tc := range []struct {
		name, domain, status, rootTarget, issue string
	}{
		{name: "foreign-domain", domain: "nimbrix", status: "done"},
		{name: "not-done", domain: "zorbax", status: "chunked"},
		{name: "outside-sample", domain: "zorbax", status: "done", issue: "outside_sample"},
		{name: "cycle", domain: "zorbax", status: "done", rootTarget: "child-chunk", issue: "cycle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { wipeCommandGraph(t) })
			migrationQuery(t, `
CREATE (:GkgMeta {id:'extraction_schema', value:$schema}),
       (a:Document {id:'doc-a', domain:'zorbax', status:'done'}),
       (z:Document {id:'doc-z', domain:$domain, status:$status}),
       (a)-[:HAS_CHUNK]->(:Chunk {id:'child-chunk', doc_id:'doc-a', seq:0, text:'Zorvax uses Nimbrix.', duplicate_of:'bridge-chunk'}),
       (z)-[:HAS_CHUNK]->(:Chunk {id:'root-chunk', doc_id:'doc-z', seq:0, text:'Zorvax uses Nimbrix.', duplicate_of:$rootTarget}),
       (z)-[:HAS_CHUNK]->(:Chunk {id:'bridge-chunk', doc_id:'doc-z', seq:1, text:'Zorvax uses Nimbrix.', duplicate_of:'root-chunk'})`,
				map[string]any{"schema": graph.ExtractionSchemaVersion, "domain": tc.domain, "status": tc.status, "rootTarget": tc.rootTarget})
			before := graphtest.Snapshot(t)
			defer before.AssertUnchanged(t, "sample and replay")

			requests := make(chan extract.RequestChunk, 1)
			sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /healthz":
					fmt.Fprint(w, `{"status":"ok"}`)
				case "POST /v1/extract":
					var req struct {
						Chunks []extract.RequestChunk `json:"chunks"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Chunks) != 1 {
						t.Errorf("unexpected extraction request: %+v, %v", req, err)
						http.Error(w, "unexpected request", http.StatusBadRequest)
						return
					}
					select {
					case requests <- req.Chunks[0]:
					default:
						t.Error("extra extraction request")
					}
					fmt.Fprint(w, `{"results":[]}`)
				default:
					t.Errorf("unexpected sidecar request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer sidecar.Close()
			t.Setenv("GKG_EXTRACTOR_SIDECAR_URL", sidecar.URL)

			dir := t.TempDir()
			input, output := filepath.Join(dir, "sample.jsonl"), filepath.Join(dir, "predictions.jsonl")
			assertEvalCLI(t, []string{"eval-sample", "-domain", "zorbax", "-docs", "1", "-seed", "0", "-out", input},
				0, "documents=1 chunks=1\n", "")
			sampleRows, _ := readEvalArtifact(t, input)
			wantTarget, wantOwner := "", ""
			if tc.issue == "outside_sample" {
				wantTarget, wantOwner = "root-chunk", "doc-z"
			}
			if chunk := sampleRows[2]; chunk["chunk_id"] != "child-chunk" || chunk["duplicate_of"] != wantTarget || chunk["dup_doc_id"] != wantOwner {
				t.Fatalf("sampled chunk = %+v", chunk)
			}

			assertEvalCLI(t, []string{"eval", "-replay", input, "-predictions", output},
				0, "chunks=1 ok=0 empty=1 errors=0 occurrences=0 stored_winners=0\n", "")
			rows, _ := readEvalArtifact(t, output)
			issue, _ := rows[1]["duplicate_root"].(string)
			if len(rows) != 3 || rows[1]["chunk_id"] != "child-chunk" || issue != tc.issue {
				t.Fatalf("replay rows = %+v, want duplicate root issue %q", rows, tc.issue)
			}
			counts := rows[2]["counts"].(map[string]any)
			for field, kind := range map[string]string{"duplicate_roots_outside_sample": "outside_sample", "duplicate_root_cycles": "cycle"} {
				want := float64(0)
				if tc.issue == kind {
					want = 1
				}
				if counts[field] != want {
					t.Errorf("trailer %s = %v, want %v", field, counts[field], want)
				}
			}
			select {
			case got := <-requests:
				if tc.issue == "outside_sample" || got.ID != "child-chunk" || got.Text != "Zorvax uses Nimbrix." {
					t.Errorf("unexpected extraction: %+v", got)
				}
			default:
				if tc.issue != "outside_sample" {
					t.Error("sampled chunk was not extracted")
				}
			}
		})
	}
}

func TestEvalReplaySidecarOnly(t *testing.T) {
	_ = preflightCommandStore(t)
	migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema', value:$schema})`, map[string]any{"schema": graph.ExtractionSchemaVersion})
	before := graphtest.Snapshot(t)
	defer before.AssertUnchanged(t, "eval replay")

	const text = "Zorvax Ltd uses Nimbrix. Zorvax uses Nimbrix. Desquon uses Nimbrix. Zorvax links Orbex."
	imageDigest := "sha256:" + strings.Repeat("3f", 32)
	presetDir := t.TempDir()
	var otherCalls, healthCalls, extractCalls atomic.Int32
	var failHealth atomic.Bool
	var batchMu sync.Mutex
	var batches [][]string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherCalls.Add(1)
		http.Error(w, "unexpected service call", http.StatusBadRequest)
	}))
	defer other.Close()
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /healthz":
			healthCalls.Add(1)
			if failHealth.Load() {
				http.Error(w, "synthetic health failure", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{"status":"ok","model":"reported-synthetic-model"}`)
		case "POST /v1/extract":
			extractCalls.Add(1)
			var req struct {
				Chunks    []extract.RequestChunk   `json:"chunks"`
				Relations []struct{ Label string } `json:"relations"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Chunks) == 0 ||
				len(req.Relations) != 1 || req.Relations[0].Label != "utilizes" {
				t.Errorf("unexpected request: %+v, %v", req, err)
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}
			var ids []string
			failed := false
			for _, c := range req.Chunks {
				ids = append(ids, c.ID)
				failed = failed || c.ID == "chunk-error"
			}
			batchMu.Lock()
			batches = append(batches, ids)
			batchMu.Unlock()
			if failed {
				http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
				return
			}
			var results []map[string]any
			for _, c := range req.Chunks {
				if c.ID != "chunk-good" {
					continue // exercise omitted results
				}
				if c.Text != text {
					t.Errorf("sidecar text = %q, want stored text", c.Text)
				}
				mentions := []map[string]any{}
				for _, name := range []string{"Zorvax Ltd", "Zorvax", "Nimbrix", "Desquon", "Orbex"} {
					start := strings.Index(text, name)
					if name == "Zorvax" {
						start = strings.Index(text, "Zorvax uses")
					}
					label := "synthetic kind"
					if name == "Nimbrix" {
						label = "value"
					}
					mentions = append(mentions, map[string]any{"text": name, "label": label, "start": start, "end": start + len(name), "score": .9})
				}
				var facts []map[string]any
				for _, f := range []struct {
					head, tail int
					score      float64
				}{{0, 2, .7}, {1, 2, .9}, {0, 2, .9}, {3, 2, .8}, {0, 4, .1}, {1, 4, .3}} {
					facts = append(facts, map[string]any{"head": f.head, "tail": f.tail, "label": "utilizes", "score": f.score})
				}
				results = append(results, map[string]any{"chunk_id": c.ID, "entities": mentions, "relations": facts})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
		default:
			t.Errorf("unexpected sidecar call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer sidecar.Close()
	preset := "entities:\n  - name: kind\n    text: synthetic kind\n    threshold: 0.4\n" +
		"  - name: value\n    threshold: 0.4\n    scope: document\n" +
		"relations:\n  - name: uses\n    text: utilizes\n    threshold: 0.25\n" +
		"allowlist: [Zorvax]\nstoplist:\n  patterns: ['^Zalken$']\n"
	presetPath := filepath.Join(presetDir, "synthetic.yaml")
	writeEvalFile(t, presetPath, []byte(preset))
	writeEvalFile(t, "overlay.txt", []byte("Desquon\n"))
	for key, value := range map[string]string{
		"GKG_CONFIG": "", "GKG_LABELS_PRESETS_DIR": presetDir, "GKG_INGEST_STOPLIST_FILE": "overlay.txt",
		"GKG_INGEST_STORE_FLOOR": "0.20", "GKG_INGEST_ASSERT_FLOOR": "0.55",
		"GKG_EXTRACTOR_MODE": "openai", "GKG_EXTRACTOR_SIDECAR_URL": sidecar.URL, "GKG_EXTRACTOR_SIDECAR_MODEL": "configured-model",
		"GKG_EXTRACTOR_SIDECAR_IMAGE":   "synthetic-sidecar:tag@" + imageDigest,
		"GKG_EXTRACTOR_OPENAI_BASE_URL": other.URL, "GKG_JUDGE_MODE": "all", "GKG_JUDGE_BASE_URL": other.URL,
		"GKG_FACTS_REQUIRE_CONFIRMATION": "false", "GKG_EMBEDDER_ENABLED": "true", "GKG_EMBEDDER_BASE_URL": other.URL,
		"GKG_AGENT_ENABLED": "true", "GKG_AGENT_BASE_URL": other.URL, "GKG_CONVERTER_MODE": "docling", "GKG_CONVERTER_URL": other.URL,
	} {
		t.Setenv(key, value)
	}
	input := evalReplaySample(t, text)
	args := []string{"eval", "-replay", "sample.jsonl", "-predictions", "predictions.jsonl", "-labels", "synthetic"}
	const summary = "chunks=14 ok=3 empty=10 errors=1 occurrences=18 stored_winners=2\n"
	assertEvalCLI(t, args, 1, summary, "")
	rows, data := readEvalArtifact(t, "predictions.jsonl")
	if rows[0]["input_sha256"] != fmt.Sprintf("%x", sha256.Sum256(input)) ||
		rows[0]["preset"] != "synthetic" || rows[0]["preset_sha256"] != fmt.Sprintf("%x", sha256.Sum256([]byte(preset))) ||
		rows[0]["store_floor"] != .20 || rows[0]["assert_floor"] != .55 || rows[0]["sidecar_model"] != "reported-synthetic-model" ||
		rows[0]["sidecar_image"] != "synthetic-sidecar:tag@"+imageDigest || rows[0]["sidecar_image_digest"] != imageDigest ||
		rows[0]["extractor_version"] != "sidecar:configured-model" || rows[0]["gkg_version"] != version {
		t.Errorf("replay header = %+v", rows[0])
	}
	wantMode := map[string]any{"hygiene": true, "domains": true, "facts_as_candidates": false}
	wantMismatches := []any{map[string]any{"doc_id": "doc-b", "labels": ""}}
	if !reflect.DeepEqual(rows[0]["write_mode"], wantMode) || !reflect.DeepEqual(rows[0]["label_mismatches"], wantMismatches) {
		t.Errorf("replay header = %+v", rows[0])
	}
	policy := rows[0]["hygiene"].(map[string]any)
	if !reflect.DeepEqual(policy["stoplist_exact"], map[string]any{"desquon": true}) ||
		!reflect.DeepEqual(policy["stoplist_patterns"], []any{"(?i)^Zalken$"}) ||
		!reflect.DeepEqual(policy["document_scoped_labels"], map[string]any{"value": true}) ||
		!reflect.DeepEqual(policy["allowlist"], map[string]any{"Zorvax": true}) || policy["code_tokens"] != false {
		t.Errorf("effective hygiene = %+v", policy)
	}
	resultRows := map[string]map[string]any{}
	var occurrences []map[string]any
	duplicates := map[string][]map[string]any{}
	for _, row := range rows[1 : len(rows)-1] {
		switch row["type"] {
		case "result":
			id := row["chunk_id"].(string)
			wantDoc, wantLabels, mismatch := "doc-a", any("synthetic"), false
			switch id {
			case "other-doc-chunk", "chunk-dup-cross":
				wantDoc, wantLabels, mismatch = "doc-b", "", true
			case "legacy-doc-chunk":
				wantDoc, wantLabels = "doc-legacy", nil
			}
			if resultRows[id] != nil || row["doc_id"] != wantDoc || row["labels"] != wantLabels || row["label_mismatch"] != mismatch {
				t.Errorf("invalid result row %+v", row)
			}
			resultRows[id] = row
		case "occurrence":
			id := row["chunk_id"].(string)
			if id == "chunk-dup" || id == "chunk-dup-cross" {
				duplicates[id] = append(duplicates[id], row)
			} else {
				occurrences = append(occurrences, row)
			}
		default:
			t.Errorf("unexpected replay row %+v", row)
		}
	}
	if len(resultRows) != 14 || resultRows["chunk-good"]["status"] != "ok" || resultRows["chunk-dup"]["status"] != "ok" ||
		resultRows["chunk-dup-cross"]["status"] != "ok" || resultRows["chunk-dup-missing"]["status"] != "empty" ||
		resultRows["chunk-dup-missing"]["duplicate_root"] != "outside_sample" ||
		resultRows["chunk-empty"]["status"] != "empty" || resultRows["chunk-empty"]["error"] != "" ||
		resultRows["chunk-error"]["status"] != "error" || !strings.Contains(fmt.Sprint(resultRows["chunk-error"]["error"]), "HTTP 307") {
		t.Errorf("result rows = %+v", resultRows)
	}
	if len(occurrences) != 6 {
		t.Fatalf("nonduplicate occurrences = %+v", occurrences)
	}
	for _, id := range []string{"chunk-dup", "chunk-dup-cross"} {
		borrowed := duplicates[id]
		if len(borrowed) != len(occurrences) {
			t.Fatalf("%s borrowed %d occurrences, want %d", id, len(borrowed), len(occurrences))
		}
		for i, root := range occurrences {
			want := maps.Clone(root)
			want["chunk_id"], want["doc_id"], want["document_title"] = id, "doc-a", "Zorbax Record"
			if id == "chunk-dup-cross" {
				want["doc_id"], want["document_title"] = "doc-b", ""
			}
			want["source_chunk_id"], want["stored_winner"] = "chunk-good", false
			if !reflect.DeepEqual(borrowed[i], want) {
				t.Errorf("%s occurrence %d = %+v, want %+v", id, i, borrowed[i], want)
			}
		}
	}
	r := occurrences[0]
	wantHead := map[string]any{"text": "Zorvax Ltd", "start": float64(0), "end": float64(10),
		"name": "Zorvax", "key": graph.DomainKey("zorbax", graph.EntityKey("Zorvax")), "type": "kind"}
	start := strings.Index(text, "Nimbrix")
	wantTail := map[string]any{"text": "Nimbrix", "start": float64(start), "end": float64(start + len("Nimbrix")),
		"name": "Nimbrix", "key": graph.ScopedEntityKey("doc-a", "Nimbrix"), "type": "value"}
	if !reflect.DeepEqual(r["head"], wantHead) || !reflect.DeepEqual(r["tail"], wantTail) || r["score"] != .7 ||
		r["relation"] != "uses" || r["relation_label_text"] != "utilizes" || r["evidence_sentence"] != "Zorvax Ltd uses Nimbrix." ||
		r["document_title"] != "Zorbax Record" || r["doc_id"] != "doc-a" || r["chunk_id"] != "chunk-good" {
		t.Errorf("unexpected occurrence = %+v", r)
	}
	for i, want := range []struct {
		disposition string
		winner, low bool
	}{{"kept", false, false}, {"kept", true, false}, {"kept", false, false}, {"hygiene:stoplist", false, false},
		{"floor:store_floor", false, false}, {"kept", true, true}} {
		row := occurrences[i]
		if row["disposition"] != want.disposition || row["stored_winner"] != want.winner || row["low_confidence"] != want.low {
			t.Errorf("occurrence %d = %+v, want %+v", i, row, want)
		}
	}
	wantCounts := map[string]any{"chunks": float64(14), "ok": float64(3), "empty": float64(10), "errors": float64(1),
		"occurrences": float64(18), "stored_winners": float64(2), "duplicate_roots_outside_sample": float64(1), "duplicate_root_cycles": float64(0)}
	if !reflect.DeepEqual(rows[len(rows)-1]["counts"], wantCounts) {
		t.Errorf("trailer counts = %+v", rows[len(rows)-1])
	}

	assertEvalCLI(t, args, 1, "", "file exists")
	if _, again := readEvalArtifact(t, "predictions.jsonl"); !bytes.Equal(again, data) {
		t.Fatal("existing predictions were changed")
	}
	args[4] = "new.jsonl"
	writeEvalFile(t, presetPath, []byte(preset+"unknown_test_key: true\n"))
	assertEvalCLI(t, args, 1, "", "unknown_test_key")
	writeEvalFile(t, presetPath, []byte(preset))
	writeEvalFile(t, "sample.jsonl", bytes.Replace(input, []byte("Zorbax Record"), []byte("Changed Record"), 1))
	assertEvalCLI(t, args, 1, "", "SHA-256")
	if _, err := os.Stat("new.jsonl"); !os.IsNotExist(err) {
		t.Errorf("invalid replay created an artifact: %v", err)
	}
	wantBatches := [][]string{
		{"other-doc-chunk"},
		{"chunk-good", "chunk-empty", "chunk-2", "chunk-3", "chunk-4", "chunk-5", "chunk-6", "chunk-7"},
		{"chunk-error"}, {"chunk-error"}, {"legacy-doc-chunk"},
	}
	batchMu.Lock()
	if !reflect.DeepEqual(batches, wantBatches) {
		t.Errorf("batches = %v, want %v", batches, wantBatches)
	}
	batchMu.Unlock()
	if h, e, other := healthCalls.Load(), extractCalls.Load(), otherCalls.Load(); h != 1 || e != 5 || other != 0 {
		t.Errorf("HTTP calls: health=%d extraction=%d other=%d", h, e, other)
	}

	writeEvalFile(t, "sample.jsonl", input)
	args[4] = "unhealthy.jsonl"
	failHealth.Store(true)
	assertEvalCLI(t, args, 1, "chunks=14 ok=0 empty=0 errors=14 occurrences=0 stored_winners=0\n", "")
	unhealthy, _ := readEvalArtifact(t, args[4])
	for _, row := range unhealthy[1 : len(unhealthy)-1] {
		if row["type"] != "result" || row["status"] != "error" || !strings.Contains(fmt.Sprint(row["error"]), "synthetic health failure") {
			t.Errorf("unhealthy replay result = %+v", row)
		}
	}
	if counts := unhealthy[len(unhealthy)-1]["counts"].(map[string]any); counts["duplicate_roots_outside_sample"] != float64(1) {
		t.Errorf("unhealthy replay trailer counts = %+v", counts)
	}
	if h, e := healthCalls.Load(), extractCalls.Load(); h != 2 || e != 5 {
		t.Errorf("HTTP calls after failed health: health=%d extraction=%d", h, e)
	}

	t.Run("candidate-winners", func(t *testing.T) {
		failHealth.Store(false)
		t.Setenv("GKG_FACTS_REQUIRE_CONFIRMATION", "true")
		args[4] = "candidates.jsonl"
		assertEvalCLI(t, args, 1, summary, "")
		candidates, _ := readEvalArtifact(t, args[4])
		wantMode := map[string]any{"hygiene": true, "domains": true, "facts_as_candidates": true}
		if !reflect.DeepEqual(candidates[0]["write_mode"], wantMode) ||
			!reflect.DeepEqual(candidates[1:len(candidates)-1], rows[1:len(rows)-1]) ||
			!reflect.DeepEqual(candidates[len(candidates)-1]["counts"], wantCounts) {
			t.Error("candidate replay differs from direct replay in mode, results, occurrences or counts")
		}
		if other := otherCalls.Load(); other != 0 {
			t.Errorf("candidate replay made %d non-sidecar calls", other)
		}
	})
}

func TestEvalReplayMatchesIngestBatches(t *testing.T) {
	st := preflightCommandStore(t)
	migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema', value:$schema})`, map[string]any{"schema": graph.ExtractionSchemaVersion})
	var mu sync.Mutex
	var batches [][]extract.RequestChunk
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /healthz":
			fmt.Fprint(w, `{"status":"ok"}`)
		case "POST /v1/extract":
			var req struct {
				Chunks []extract.RequestChunk `json:"chunks"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode request: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			mu.Lock()
			batches = append(batches, req.Chunks)
			mu.Unlock()
			fmt.Fprint(w, `{"results":[]}`)
		default:
			t.Errorf("unexpected sidecar request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer sidecar.Close()
	t.Setenv("GKG_EXTRACTOR_MODE", "sidecar")
	t.Setenv("GKG_EXTRACTOR_SIDECAR_URL", sidecar.URL)
	t.Setenv("GKG_EXTRACTOR_SIDECAR_IMAGE", "sha256:"+strings.Repeat("3f", 32))
	t.Setenv("GKG_INGEST_STOPLIST_FILE", "")
	t.Setenv("GKG_FACTS_REQUIRE_CONFIRMATION", "false")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	pipeline, _, err := buildPipeline(ctx, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Domain: "zorbax", Source: "synthetic", Hash: "stored"})
	if err != nil {
		t.Fatal(err)
	}
	var chunks []graph.Chunk
	var want []extract.RequestChunk
	for seq := range 11 {
		chunk := graph.Chunk{Seq: seq, Text: fmt.Sprintf("Zorvax record %d.", seq)}
		if seq == 1 || seq == 8 {
			chunk.DuplicateOf = doc.ID + "-c0"
		} else {
			want = append(want, extract.RequestChunk{ID: fmt.Sprintf("%s-c%d", doc.ID, seq), Text: chunk.Text})
		}
		chunks = append(chunks, chunk)
	}
	if err := st.PutChunks(ctx, doc.ID, chunks); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus(ctx, doc.ID, graph.StatusChunked, ""); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Process(ctx, doc.ID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	ingestBatches := batches
	batches = nil
	mu.Unlock()
	if !reflect.DeepEqual(ingestBatches, [][]extract.RequestChunk{want[:8], want[8:]}) {
		t.Fatalf("ingest batches = %v, expected nonduplicate chunks split at 8", ingestBatches)
	}

	assertEvalCLI(t, []string{"eval-sample", "-domain", "zorbax", "-out", "sample.jsonl"}, 0, "documents=1 chunks=11\n", "")
	assertEvalCLI(t, []string{"eval", "-replay", "sample.jsonl", "-predictions", "predictions.jsonl"},
		0, "chunks=11 ok=0 empty=11 errors=0 occurrences=0 stored_winners=0\n", "")
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(batches, ingestBatches) {
		t.Errorf("replay batches = %v, want the ingest batches %v", batches, ingestBatches)
	}
}

func TestEvalReplayUsage(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GKG_CONFIG", "")
	base := []string{"eval", "-replay", "sample.jsonl", "-predictions", "predictions.jsonl"}
	for _, flag := range []string{"-golden=", "-sweep=", "-out=", "-provider=mock", "-provider=openai", "-provider=", "unexpected"} {
		t.Run(flag, func(t *testing.T) {
			assertEvalCLI(t, append(base, flag), 2, "", "gkg eval:")
		})
	}
	assertEvalCLI(t, []string{"eval", "-replay", "sample.jsonl"}, 2, "", "requires")
	assertEvalCLI(t, []string{"eval", "-predictions", "predictions.jsonl"}, 2, "", "requires")
	assertEvalCLI(t, []string{"eval", "-replay=", "-predictions", "predictions.jsonl"}, 2, "", "requires")
	assertEvalCLI(t, []string{"eval", "-replay", "missing.jsonl", "-predictions", "predictions.jsonl", "-provider=sidecar"}, 1, "", "read sample")
	assertEvalCLI(t, []string{"eval-sample", "-domain", "INVALID", "-out", "sample.jsonl"}, 2, "", "usage:")
}

func TestEvalReplayWriteMode(t *testing.T) {
	_ = preflightCommandStore(t)
	evalReplaySample(t, "Zorvax uses Nimbrix.")
	t.Setenv("GKG_INGEST_STOPLIST_FILE", "")
	var calls atomic.Int32
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/healthz":
			fmt.Fprint(w, `{"status":"ok"}`)
		case "/v1/extract":
			fmt.Fprint(w, `{"results":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer sidecar.Close()
	t.Setenv("GKG_EXTRACTOR_SIDECAR_URL", sidecar.URL)
	t.Setenv("GKG_JUDGE_MODE", "all")
	t.Setenv("GKG_JUDGE_BASE_URL", sidecar.URL)
	const oldSchema = "gkg eval: replay requires extraction schema v%d (graph is v%d); migrate the graph first\n"
	for _, tc := range []struct {
		name, confirmation, want string
		schema                   int
	}{
		{"missing-marker", "false", fmt.Sprintf(oldSchema, graph.ExtractionSchemaVersion, 1), 0},
		{"legacy-v2", "false", fmt.Sprintf(oldSchema, graph.ExtractionSchemaVersion, 2), 2},
		{"legacy-v3", "true", fmt.Sprintf(oldSchema, graph.ExtractionSchemaVersion, 3), 3},
		{"newer", "false", fmt.Sprintf("graph extraction schema v%d is newer than this binary (v%d)", graph.ExtractionSchemaVersion+1, graph.ExtractionSchemaVersion), graph.ExtractionSchemaVersion + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.schema != 0 {
				migrationQuery(t, `MERGE (m:GkgMeta {id:'extraction_schema'}) SET m.value = $schema`, map[string]any{"schema": tc.schema})
			}
			t.Setenv("GKG_FACTS_REQUIRE_CONFIRMATION", tc.confirmation)
			before := graphtest.Snapshot(t)
			defer before.AssertUnchanged(t, "unsupported eval replay")
			const output = "unsupported.jsonl"
			args := []string{"eval", "-replay", "sample.jsonl", "-predictions", output}
			assertEvalCLI(t, args, 1, "", tc.want)
			if _, err := os.Stat(output); !os.IsNotExist(err) {
				t.Errorf("unsupported replay created an artifact: %v", err)
			}
		})
	}
	if count := calls.Load(); count != 0 {
		t.Errorf("unsupported modes called sidecar %d times", count)
	}
	t.Run("direct-facts", func(t *testing.T) {
		migrationQuery(t, `MERGE (m:GkgMeta {id:'extraction_schema'}) SET m.value = $schema`, map[string]any{"schema": graph.ExtractionSchemaVersion})
		t.Setenv("GKG_FACTS_REQUIRE_CONFIRMATION", "false")
		const image = "synthetic-sidecar:tag"
		t.Setenv("GKG_EXTRACTOR_SIDECAR_IMAGE", image)
		before := graphtest.Snapshot(t)
		defer before.AssertUnchanged(t, "supported eval replay")
		const output = "supported.jsonl"
		args := []string{"eval", "-replay", "sample.jsonl", "-predictions", output}
		assertEvalCLI(t, args, 0, "chunks=14 ok=0 empty=14 errors=0 occurrences=0 stored_winners=0\n",
			"gkg eval: warning: sidecar image digest is unknown")
		rows, _ := readEvalArtifact(t, output)
		want := []any{map[string]any{"doc_id": "doc-a", "labels": "synthetic"}}
		if rows[0]["preset"] != "" || rows[0]["sidecar_image"] != image || rows[0]["sidecar_image_digest"] != "unknown" ||
			!reflect.DeepEqual(rows[0]["label_mismatches"], want) {
			t.Errorf("built-in replay header = %+v", rows[0])
		}
	})
}

func TestPresetCheck(t *testing.T) {
	files, err := filepath.Glob("../../labels/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("shipped presets = %v (%v)", files, err)
	}
	for _, path := range files {
		assertEvalCLI(t, []string{"preset", "check", path}, 0, "preset ok\n", "")
	}
	makeLabels := func(kind string, n int) string {
		data := kind + ":\n"
		for i := range n {
			data += fmt.Sprintf("  - name: synthetic_%d\n    threshold: 1\n", i)
		}
		return data
	}
	for _, tc := range []struct{ name, yaml, want string }{
		{"unknown", "unlisted_key: true\n", "unlisted_key"},
		{"nested-unknown", "stoplist:\n  unmatched: []\n", "unmatched"},
		{"label-unknown", "entities: [{name: kind, threshold: 1, typo: true}]\n", "typo"},
		{"sanitized", "relations: [{name: part-of, threshold: 1}, {name: part_of, threshold: 1}]\n", "sanitize"},
		{"aliases", "entities: [{name: kind, threshold: 1, text: Group}, {name: group, threshold: 1}]\n", "ambiguous"},
		{"negative", "entities: [{name: kind, threshold: -0.2}]\n", "threshold"},
		{"zero", "entities: [{name: kind, threshold: 0}]\n", "threshold"},
		{"high", "entities: [{name: kind, threshold: 1.01}]\n", "threshold"},
		{"nan", "entities: [{name: kind, threshold: .nan}]\n", "threshold"},
		{"infinite", "entities: [{name: kind, threshold: .inf}]\n", "threshold"},
		{"many-entities", makeLabels("entities", 31), "at most 30"},
		{"many-relations", makeLabels("relations", 16), "at most 15"},
		{"long-text", fmt.Sprintf("entities: [{name: kind, threshold: 1, text: %q}]\n", strings.Repeat("é", 61)), "60 characters"},
		{"invalid-hygiene", "stoplist: {patterns: ['[']}\n", "pattern"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "test-preset.yaml")
			writeEvalFile(t, file, []byte(tc.yaml))
			assertEvalCLI(t, []string{"preset", "check", file}, 1, "", tc.want)
		})
	}
	if _, err := ingest.LoadCheckedPreset(t.TempDir(), "", ""); err != nil {
		t.Errorf("built-in preset: %v", err)
	}
	assertEvalCLI(t, []string{"preset", "check"}, 2, "", "usage:")
}

func TestPresetCheckStoplist(t *testing.T) {
	dir := t.TempDir()
	preset := filepath.Join(dir, "synthetic.yaml")
	overlay := filepath.Join(dir, "overlay.txt")
	writeEvalFile(t, preset, []byte("entities: [{name: kind, threshold: 1}]\n"))
	args := []string{"preset", "check", preset, "-stoplist", overlay}
	assertEvalCLI(t, args, 1, "", "hygiene overlay")
	writeEvalFile(t, overlay, []byte("# synthetic stop words\nDesquon\n"))
	assertEvalCLI(t, args, 0, "preset ok\n", "")
	writeEvalFile(t, overlay, []byte(strings.Repeat("x", 70<<10)))
	assertEvalCLI(t, args, 1, "", "token too long")
	assertEvalCLI(t, args[:len(args)-1], 2, "", "needs an argument")

	writeEvalFile(t, preset, []byte("entities: [{name: kind, threshold: 0}]\n"))
	assertEvalCLI(t, args, 1, "", `label preset "synthetic": label "kind" threshold`)
}

func evalReplaySample(t *testing.T, text string) []byte {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	counts := map[string]int{"documents": 3, "chunks": 14}
	rows := []any{
		map[string]any{"type": "header", "domain": "zorbax", "counts": counts, "seed": 0},
		map[string]any{"type": "document", "doc_id": "doc-b", "labels": ""},
		map[string]any{"type": "chunk", "doc_id": "doc-b", "chunk_id": "other-doc-chunk", "seq": 0, "text": text},
		map[string]any{"type": "chunk", "doc_id": "doc-b", "chunk_id": "chunk-dup-cross", "seq": 1, "text": "Zorvax record.",
			"duplicate_of": "chunk-dup", "dup_doc_id": "doc-a"},
		map[string]any{"type": "document", "doc_id": "doc-a", "title": "Zorbax Record", "title_source": "", "source": "synthetic", "labels": "synthetic"},
	}
	ordered := []string{"chunk-dup", "chunk-good", "chunk-empty", "chunk-2", "chunk-3", "chunk-4", "chunk-5", "chunk-6", "chunk-7", "chunk-error", "chunk-dup-missing"}
	// Put some rows out of order to verify requests use seq rather than file order.
	for _, i := range []int{2, 0, 1, 3, 4, 5, 6, 7, 9, 8, 10} {
		id := ordered[i]
		duplicate, owner := "", ""
		chunkText := text
		switch id {
		case "chunk-dup":
			duplicate, owner = "chunk-good", "doc-a"
			chunkText = "Zorvax note."
		case "chunk-dup-missing":
			duplicate, owner = "outside-chunk", "outside-doc"
		}
		rows = append(rows, map[string]any{"type": "chunk", "doc_id": "doc-a", "chunk_id": id, "text": chunkText,
			"seq": i, "start": 0, "end": len(chunkText), "duplicate_of": duplicate, "dup_doc_id": owner})
	}
	rows = append(rows,
		map[string]any{"type": "document", "doc_id": "doc-legacy"},
		map[string]any{"type": "chunk", "doc_id": "doc-legacy", "chunk_id": "legacy-doc-chunk", "seq": 0, "text": text},
	)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	trailer := map[string]any{"type": "trailer", "counts": counts, "sha256": fmt.Sprintf("%x", sha256.Sum256(b.Bytes()))}
	if err := enc.Encode(trailer); err != nil {
		t.Fatal(err)
	}
	writeEvalFile(t, "sample.jsonl", b.Bytes())
	return b.Bytes()
}

func readEvalArtifact(t *testing.T, path string) ([]map[string]any, []byte) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("%s mode = %o, want 0600", path, mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	offset, trailerOffset := 0, 0
	for line := range bytes.Lines(data) {
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
		trailerOffset = offset
		offset += len(line)
	}
	if len(rows) < 2 || rows[0]["type"] != "header" || rows[len(rows)-1]["type"] != "trailer" ||
		rows[len(rows)-1]["sha256"] != fmt.Sprintf("%x", sha256.Sum256(data[:trailerOffset])) {
		t.Fatalf("invalid header or trailer for %s: %+v", path, rows)
	}
	return rows, data
}

func assertEvalCLI(t *testing.T, args []string, wantCode int, wantOut, wantErr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	if code != wantCode || out.String() != wantOut ||
		(wantErr == "" && errOut.Len() != 0) || (wantErr != "" && !strings.Contains(errOut.String(), wantErr)) {
		t.Fatalf("%v exit=%d stdout=%q stderr=%q, want %d %q and stderr matching %q",
			args, code, out.String(), errOut.String(), wantCode, wantOut, wantErr)
	}
}

func writeEvalFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
