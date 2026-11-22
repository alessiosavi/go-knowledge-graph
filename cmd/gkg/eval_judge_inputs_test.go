package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

const (
	judgeFixtureDomain = "ta2-replay"
	judgeFixtureModel  = "fixture-gliner"
	judgeFixtureHead   = "Zorvax Ltd"
	judgeFixtureTail   = "Nimbrix"
)

func TestEvalReplayJudgeInputsMatchStoredWinners(t *testing.T) {
	st, pins, _ := judgeExportFixture(t)
	ctx := t.Context()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	pipeline, _, err := buildPipeline(ctx, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	var root, duplicate string
	docPresets := map[string]string{}
	for i, preset := range []string{"alpha", "beta", "alpha"} {
		doc, err := st.UpsertDocument(ctx, graph.DocumentInput{
			Kind: "text", Domain: judgeFixtureDomain, Labels: preset,
			Title: fmt.Sprintf("Repaired title %d", i), Source: fmt.Sprint(i), Hash: fmt.Sprint(i),
		})
		if err != nil {
			t.Fatal(err)
		}
		docPresets[doc.ID] = preset
		chunk := graph.Chunk{Text: "Prior note. Zorvax Ltd works with Nimbrix. Zorvax works with Nimbrix."}
		if i == 2 {
			chunk.DuplicateOf = root
			duplicate = doc.ID
		}
		if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{chunk}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			root = doc.ID + "-c0"
		}
		if err := st.SetStatus(ctx, doc.ID, graph.StatusChunked, ""); err != nil {
			t.Fatal(err)
		}
		if err := pipeline.Process(ctx, doc.ID); err != nil {
			t.Fatal(err)
		}
	}
	assertEvalCLI(t, []string{"eval-sample", "-domain", judgeFixtureDomain, "-out", "all.jsonl"}, 0, "documents=3 chunks=3\n", "")
	rows, _ := readEvalArtifact(t, "all.jsonl")
	edges, err := st.PendingStoredJudgeEdges(ctx, judgeFixtureDomain, nil, 10)
	if err != nil || len(edges) != 2 {
		t.Fatalf("pending edges = %+v, err = %v; want two winners", edges, err)
	}
	writeJudgePins(t, pins)
	for _, preset := range []string{"alpha", "beta"} {
		t.Run(preset, func(t *testing.T) {
			var selected []map[string]any
			var docs, chunks int
			for _, row := range rows[1 : len(rows)-1] {
				docID := row["doc_id"].(string)
				if docPresets[docID] != preset {
					continue
				}
				selected = append(selected, row)
				if row["type"] == "document" {
					docs++
					if row["labels"] != preset {
						t.Fatalf("sample labels %v", row)
					}
				} else {
					chunks++
					if docID == duplicate && row["duplicate_of"] != root {
						t.Fatalf("sample lost duplicate root: %v", row)
					}
				}
			}
			sample := preset + ".jsonl"
			writeJudgeSample(t, sample, selected, docs, chunks)
			predictions, judgeInputs := preset+"-predictions.jsonl", preset+"-inputs.jsonl"
			var stdout, stderr bytes.Buffer
			if code := run([]string{"eval", "-replay", sample, "-predictions", predictions, "-judge-inputs", judgeInputs,
				"-pins", "pins.json", "-labels", preset}, &stdout, &stderr); code != 0 {
				t.Fatalf("replay exit %d, stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			predictionRows, _ := readEvalArtifact(t, predictions)
			if header := predictionRows[0]; header["backup_tar_sha256"] != pins["backup_tar_sha256"] ||
				header["sidecar_image_digest"] != pins["sidecar"].(map[string]any)["digest"] {
				t.Fatalf("predictions provenance: %+v", header)
			}
			got := readSingleJudgeInput(t, judgeInputs)
			if got.DocID == duplicate {
				t.Fatal("duplicate document emitted a judge input")
			}
			for _, edge := range edges {
				if docPresets[edge.DocID] == preset {
					assertJudgeInputMatchesEdge(t, got, edge)
				}
			}
			input, err := ingest.DecodeJudgeInput(got.Payload)
			if err != nil || input.Head != judgeFixtureHead || input.Head == got.HeadKey ||
				got.HeadKey != graph.DomainKey(judgeFixtureDomain, graph.EntityKey("Zorvax")) ||
				input.Tail != judgeFixtureTail || input.Confidence != .85 || got.Confidence != input.Confidence ||
				input.Evidence != "Zorvax Ltd works with Nimbrix." || input.DocContext == "" ||
				got.InputHash != fmt.Sprintf("%x", sha256.Sum256(got.Payload)) {
				t.Fatalf("export did not exercise normalized identity and original evidence: %+v, %v", input, err)
			}
		})
	}
}

func TestEvalReplayJudgeInputsUsePinnedWriteModeAndFloors(t *testing.T) {
	st, pins, _ := judgeExportFixture(t)
	ctx := t.Context()
	// Use the legacy write mode in the production pipeline, then restore the
	// current marker that the read-only replay command requires.
	setJudgeFixtureSchema(t, graph.SchemaVersionV4-1)
	t.Setenv("GKG_INGEST_STORE_FLOOR", "0.7")
	t.Setenv("GKG_INGEST_ASSERT_FLOOR", "0.8")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	pipeline, _, err := buildPipeline(ctx, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if got := pipeline.WriteMode(); got != (ingest.WriteMode{}) {
		t.Fatalf("expected legacy ingest write mode, got %+v", got)
	}
	doc, err := st.UpsertDocument(ctx, graph.DocumentInput{
		Kind: "text", Domain: judgeFixtureDomain, Labels: "alpha", Title: "Legacy fixture", Source: "legacy", Hash: "legacy",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutChunks(ctx, doc.ID, []graph.Chunk{{Text: "Zorvax Ltd works with Nimbrix. Zorvax works with Nimbrix."}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus(ctx, doc.ID, graph.StatusChunked, ""); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.Process(ctx, doc.ID); err != nil {
		t.Fatal(err)
	}
	edges, err := st.PendingStoredJudgeEdges(ctx, judgeFixtureDomain, nil, 10)
	if err != nil || len(edges) != 1 {
		t.Fatalf("want one stored winner, got %+v, err=%v", edges, err)
	}
	setJudgeFixtureSchema(t, graph.ExtractionSchemaVersion)
	assertEvalCLI(t, []string{"eval-sample", "-domain", judgeFixtureDomain, "-out", "sample.jsonl"}, 0, "documents=1 chunks=1\n", "")

	t.Setenv("GKG_JUDGE_STORE_INPUTS", "false")
	t.Setenv("GKG_JUDGE_MODE", "all")
	t.Setenv("GKG_FACTS_REQUIRE_CONFIRMATION", "true")
	t.Setenv("GKG_INGEST_STORE_FLOOR", "0.99")
	t.Setenv("GKG_INGEST_ASSERT_FLOOR", "0.99")
	pins["write_mode"] = map[string]any{"hygiene": false, "domains": false, "facts_as_candidates": false}
	pins["store_floor"], pins["assert_floor"] = .7, .8
	writeJudgePins(t, pins)
	const summary = "chunks=1 ok=1 empty=0 errors=0 occurrences=2 stored_winners=1 duplicate_roots_outside_sample=0 duplicate_root_cycles=0\n"
	assertEvalCLI(t, append(judgeReplayArgs(), "-mentions", "mentions.jsonl"), 0, summary, "")
	assertJudgeInputMatchesEdge(t, readSingleJudgeInput(t, "judge-inputs.jsonl"), edges[0])
	rows, _ := readEvalArtifact(t, "predictions.jsonl")
	header := rows[0]
	mentions, _ := readEvalArtifact(t, "mentions.jsonl")
	assertEvalAttempt(t, header, mentions[0])
	if mentions[0]["format"] != "gkg-mentions/v1" || mentions[len(mentions)-1]["counts"].(map[string]any)["complete"] != true {
		t.Fatalf("incomplete pinned mentions artifact: %v", mentions)
	}
	if !reflect.DeepEqual(header["write_mode"], pins["write_mode"]) ||
		header["store_floor"] != .7 || header["assert_floor"] != .8 {
		t.Fatalf("predictions ignored pins: %v", header)
	}
	if rows[2]["low_confidence"] != false || rows[2]["stored_winner"] != true ||
		rows[2]["head"].(map[string]any)["key"] != graph.EntityKey(judgeFixtureHead) ||
		rows[3]["disposition"] != "floor:store_floor" {
		t.Fatalf("predictions used config instead of pinned hygiene, domains or floors: %v", rows[2:4])
	}
	t.Setenv("GKG_EXTRACTOR_SIDECAR_IMAGE", "fixture:tag")
	pinsOnlyArgs := []string{"eval", "-replay", "sample.jsonl", "-predictions", "predictions-only.jsonl", "-pins", "pins.json", "-labels", "alpha"}
	stoplistDigest := pins["stoplist_sha256"]
	pins["stoplist_sha256"] = strings.Repeat("9", 64)
	writeJudgePins(t, pins)
	assertEvalCLI(t, pinsOnlyArgs, 2, "", "stoplist_sha256 does not match")
	assertMissingEvalFile(t, "predictions-only.jsonl")
	pins["stoplist_sha256"] = stoplistDigest
	pins["sidecar"].(map[string]any)["digest"] = ""
	writeJudgePins(t, pins)
	assertEvalCLI(t, pinsOnlyArgs, 0, summary, "sidecar image digest is unknown")
	pinsOnly, _ := readEvalArtifact(t, "predictions-only.jsonl")
	onlyHeader := pinsOnly[0]
	if onlyHeader["sidecar_image_digest"] != "" || onlyHeader["backup_tar_sha256"] != pins["backup_tar_sha256"] ||
		!reflect.DeepEqual(onlyHeader["write_mode"], pins["write_mode"]) ||
		onlyHeader["store_floor"] != .7 || onlyHeader["assert_floor"] != .8 ||
		!reflect.DeepEqual(pinsOnly[2:4], rows[2:4]) {
		t.Fatalf("predictions-only replay ignored pins: %v", pinsOnly)
	}
}

func TestEvalReplayJudgeInputsRefuseNewerSchemaWithMatchingPins(t *testing.T) {
	_, pins, _ := judgeExportFixture(t)
	newer := graph.ExtractionSchemaVersion + 1
	setJudgeFixtureSchema(t, newer)
	pins["schema_version"] = newer
	writeSingleJudgeSample(t, "alpha")
	writeJudgePins(t, pins)
	assertEvalCLI(t, judgeReplayArgs(), 1, "", fmt.Sprintf("graph extraction schema v%d is newer than this binary (v%d); deploy a newer gkg or restore the pre-migration snapshot", newer, graph.ExtractionSchemaVersion))
	assertNoJudgeArtifacts(t)
}

func setJudgeFixtureSchema(t *testing.T, schema int) {
	t.Helper()
	migrationQuery(t, `MATCH (m:GkgMeta {id:'extraction_schema'}) SET m.value = $schema`, map[string]any{"schema": schema})
}

func TestEvalReplayJudgeInputsRefusePinsAndLabelMismatch(t *testing.T) {
	_, initialPins, _ := judgeExportFixture(t)
	for _, tc := range []struct {
		name   string
		suffix []any
		key    string
		value  any
		want   string
	}{
		{"schema", nil, "schema_version", 3, "schema_version"},
		{"stoplist", nil, "stoplist_sha256", strings.Repeat("9", 64), "stoplist_sha256"},
		{"preset-hash", []any{"presets", 0}, "sha256", strings.Repeat("9", 64), "preset"},
		{"preset-absent", []any{"presets", 0}, "name", "other", "preset"},
		{"sidecar-model", []any{"sidecar"}, "model", "other", "sidecar.model"},
		{"sidecar-digest", []any{"sidecar"}, "digest", "sha256:" + strings.Repeat("9", 64), "sidecar.digest"},
		{"source-missing", nil, "source", nil, `missing key "source"`},
		{"backup-hash-missing", nil, "backup_tar_sha256", nil, `missing key "backup_tar_sha256"`},
		{"schema-missing", nil, "schema_version", nil, `missing key "schema_version"`},
		{"write-mode-missing", nil, "write_mode", nil, `missing key "write_mode"`},
		{"store-floor-missing", nil, "store_floor", nil, `missing key "store_floor"`},
		{"assert-floor-missing", nil, "assert_floor", nil, `missing key "assert_floor"`},
		{"stoplist-missing", nil, "stoplist_sha256", nil, `missing key "stoplist_sha256"`},
		{"sidecar-missing", nil, "sidecar", nil, `missing key "sidecar"`},
		{"presets-missing", nil, "presets", nil, `missing key "presets"`},
		{"hygiene-missing", []any{"write_mode"}, "hygiene", nil, `missing key "hygiene"`},
		{"domains-missing", []any{"write_mode"}, "domains", nil, `missing key "domains"`},
		{"candidates-missing", []any{"write_mode"}, "facts_as_candidates", nil, `missing key "facts_as_candidates"`},
		{"model-missing", []any{"sidecar"}, "model", nil, `missing key "model"`},
		{"digest-missing", []any{"sidecar"}, "digest", nil, `missing key "digest"`},
		{"preset-name-missing", []any{"presets", 0}, "name", nil, `missing key "name"`},
		{"preset-hash-missing", []any{"presets", 0}, "sha256", nil, `missing key "sha256"`},
		{"top-unknown", nil, "extra", true, "extra"},
		{"write-mode-unknown", []any{"write_mode"}, "extra", true, "extra"},
		{"sidecar-unknown", []any{"sidecar"}, "extra", true, "extra"},
		{"preset-unknown", []any{"presets", 0}, "extra", true, "extra"},
		{"empty-source", nil, "source", "", "source must be non-empty"},
		{"empty-model", []any{"sidecar"}, "model", "", "sidecar.model must be non-empty"},
		{"empty-backup-hash", nil, "backup_tar_sha256", "", "backup_tar_sha256 must be 64 lowercase hexadecimal characters"},
		{"short-backup-hash", nil, "backup_tar_sha256", strings.Repeat("a", 63), "backup_tar_sha256 must be 64 lowercase hexadecimal characters"},
		{"uppercase-backup-hash", nil, "backup_tar_sha256", strings.Repeat("A", 64), "backup_tar_sha256 must be 64 lowercase hexadecimal characters"},
		{"nonhex-stoplist-hash", nil, "stoplist_sha256", strings.Repeat("z", 64), "stoplist_sha256 must be 64 lowercase hexadecimal characters"},
		{"uppercase-preset-hash", []any{"presets", 0}, "sha256", strings.Repeat("A", 64), "presets[0].sha256 must be 64 lowercase hexadecimal characters"},
		{"invalid-unselected-preset-hash", []any{"presets", 1}, "sha256", "", "presets[1].sha256 must be 64 lowercase hexadecimal characters"},
		{"digest-without-prefix", []any{"sidecar"}, "digest", strings.Repeat("a", 64), "sidecar.digest must be empty or sha256:"},
		{"uppercase-digest", []any{"sidecar"}, "digest", "sha256:" + strings.Repeat("A", 64), "sidecar.digest must be empty or sha256:"},
		{"nonhex-digest", []any{"sidecar"}, "digest", "sha256:" + strings.Repeat("g", 64), "sidecar.digest must be empty or sha256:"},
		{"short-digest", []any{"sidecar"}, "digest", "sha256:1234", "sidecar.digest must be empty or sha256:"},
		{"empty-presets", nil, "presets", []any{}, "presets must be non-empty"},
		{"negative-store-floor", nil, "store_floor", -.01, "must satisfy 0 <= store_floor <= assert_floor <= 1"},
		{"negative-assert-floor", nil, "assert_floor", -.01, "must satisfy 0 <= store_floor <= assert_floor <= 1"},
		{"high-store-floor", nil, "store_floor", 1.01, "must satisfy 0 <= store_floor <= assert_floor <= 1"},
		{"high-assert-floor", nil, "assert_floor", 1.01, "must satisfy 0 <= store_floor <= assert_floor <= 1"},
		{"inverted-floors", nil, "store_floor", .6, "must satisfy 0 <= store_floor <= assert_floor <= 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeSingleJudgeSample(t, "alpha")
			var pins map[string]any
			data, err := json.Marshal(initialPins)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &pins); err != nil {
				t.Fatal(err)
			}
			var current any = pins
			for _, field := range tc.suffix {
				switch field := field.(type) {
				case string:
					current = current.(map[string]any)[field]
				case int:
					current = current.([]any)[field]
				}
			}
			fields := current.(map[string]any)
			if tc.value == nil {
				delete(fields, tc.key)
			} else {
				fields[tc.key] = tc.value
			}
			writeJudgePins(t, pins)
			assertEvalCLI(t, judgeReplayArgs(), 2, "", tc.want)
			assertNoJudgeArtifacts(t)
		})
	}
	t.Run("labels", func(t *testing.T) {
		writeSingleJudgeSample(t, "beta")
		writeJudgePins(t, initialPins)
		assertEvalCLI(t, judgeReplayArgs(), 2, "", "labels")
		assertNoJudgeArtifacts(t)
	})
	t.Run("missing-pins-flag", func(t *testing.T) {
		assertEvalCLI(t, judgeReplayArgs()[:7], 2, "", "requires -pins")
		assertNoJudgeArtifacts(t)
	})
}

func TestEvalReplayJudgeInputsLeaveNoArtifactsOnExtractionError(t *testing.T) {
	_, pins, failChunk := judgeExportFixture(t)
	writeJudgeSample(t, "sample.jsonl", []map[string]any{
		{"type": "document", "doc_id": "synthetic", "labels": "alpha"},
		{"type": "chunk", "doc_id": "synthetic", "chunk_id": "ok", "seq": 0, "text": "Zorvax Ltd works with Nimbrix. Zorvax works with Nimbrix."},
		{"type": "chunk", "doc_id": "synthetic", "chunk_id": "fail", "seq": 1, "text": "Zorvax Ltd works with Nimbrix. Zorvax works with Nimbrix."},
	}, 1, 2)
	writeJudgePins(t, pins)
	failChunk.Store(true)
	assertEvalCLI(t, judgeReplayArgs(), 1, "", "synthetic extraction error")
	assertNoJudgeArtifacts(t)
}

func judgeReplayArgs() []string {
	return []string{"eval", "-replay", "sample.jsonl", "-predictions", "predictions.jsonl", "-judge-inputs", "judge-inputs.jsonl", "-pins", "pins.json", "-labels", "alpha"}
}

type exportedJudgeInput struct {
	DocID      string  `json:"doc_id"`
	ChunkID    string  `json:"chunk_id"`
	HeadKey    string  `json:"head_key"`
	Rel        string  `json:"rel"`
	TailKey    string  `json:"tail_key"`
	Confidence float64 `json:"confidence"`
	Payload    []byte  `json:"judge_input_b64"`
	InputHash  string  `json:"input_hash"`
}

func readSingleJudgeInput(t *testing.T, path string) exportedJudgeInput {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("judge inputs permissions: %v, %v", info, err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("want one judge input in %s: %s", path, data)
	}
	var values map[string]any
	if err := json.Unmarshal(lines[0], &values); err != nil {
		t.Fatal(err)
	}
	assertSettingsKeys(t, values, "doc_id", "chunk_id", "head_key", "rel", "tail_key", "confidence", "judge_input_b64", "input_hash")
	var got exportedJudgeInput
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func assertJudgeInputMatchesEdge(t *testing.T, got exportedJudgeInput, edge graph.StoredJudgeEdge) {
	t.Helper()
	if got.DocID != edge.DocID || got.ChunkID != edge.ChunkID ||
		got.HeadKey != edge.HeadKey || got.Rel != edge.Rel || got.TailKey != edge.TailKey ||
		got.InputHash != edge.InputHash || string(got.Payload) != edge.JudgeInput {
		t.Fatalf("export %+v payload=%s; pending edge %+v", got, got.Payload, edge)
	}
	input, err := ingest.DecodeJudgeInput(got.Payload)
	if err != nil || got.Confidence != input.Confidence {
		t.Fatalf("exported confidence %v, stored input %+v, err=%v", got.Confidence, input, err)
	}
}

func assertNoJudgeArtifacts(t *testing.T) {
	t.Helper()
	for _, path := range []string{"predictions.jsonl", "judge-inputs.jsonl"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s exists or could not be checked: %v", path, err)
		}
	}
	if paths, err := filepath.Glob(".gkg-eval-*"); err != nil || len(paths) != 0 {
		t.Errorf("temporary replay files remain: %v, %v", paths, err)
	}
}

func writeJudgePins(t *testing.T, pins map[string]any) {
	t.Helper()
	data, err := json.Marshal(pins)
	if err != nil {
		t.Fatal(err)
	}
	writeEvalFile(t, "pins.json", data)
}

func writeSingleJudgeSample(t *testing.T, labels string) {
	t.Helper()
	writeJudgeSample(t, "sample.jsonl", []map[string]any{
		{"type": "document", "doc_id": "synthetic", "labels": labels},
		{"type": "chunk", "doc_id": "synthetic", "chunk_id": "ok", "seq": 0, "text": "Zorvax Ltd works with Nimbrix. Zorvax works with Nimbrix."},
	}, 1, 1)
}

func writeJudgeSample(t *testing.T, path string, rows []map[string]any, docs, chunks int) {
	t.Helper()
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	counts := map[string]int{"documents": docs, "chunks": chunks}
	if err := enc.Encode(map[string]any{"type": "header", "domain": judgeFixtureDomain, "counts": counts}); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Encode(map[string]any{"type": "trailer", "counts": counts, "sha256": fmt.Sprintf("%x", sha256.Sum256(out.Bytes()))}); err != nil {
		t.Fatal(err)
	}
	writeEvalFile(t, path, out.Bytes())
}

func judgeExportFixture(t *testing.T) (*graph.Store, map[string]any, *atomic.Bool) {
	t.Helper()
	st := preflightCommandStore(t)
	migrationQuery(t, `CREATE (:GkgMeta {id:'extraction_schema', value:$schema})`, map[string]any{"schema": graph.ExtractionSchemaVersion})
	var failChunk atomic.Bool
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /healthz":
			fmt.Fprintf(w, `{"status":"ok","model":%q}`, judgeFixtureModel)
		case "POST /v1/extract":
			var req struct {
				Chunks    []extract.RequestChunk
				Relations []struct{ Label string }
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Relations) != 1 {
				t.Errorf("invalid extract request: %+v, %v", req, err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			rel := req.Relations[0].Label
			var results []map[string]any
			for _, c := range req.Chunks {
				if failChunk.Load() && c.ID == "fail" {
					http.Error(w, "synthetic extraction error", http.StatusServiceUnavailable)
					return
				}
				names := []string{judgeFixtureHead, judgeFixtureTail, "Zorvax"}
				mentions := make([]map[string]any, 0, len(names))
				for i, name := range names {
					start := strings.Index(c.Text, name)
					if i == 2 {
						start = strings.LastIndex(c.Text, name)
					}
					mentions = append(mentions, map[string]any{"text": name, "label": "organization", "start": start, "end": start + len(name), "score": .9})
				}
				results = append(results, map[string]any{
					"chunk_id": c.ID, "entities": mentions,
					"relations": []map[string]any{
						{"head": 0, "tail": 1, "label": rel, "score": .85},
						{"head": 2, "tail": 1, "label": rel, "score": .65},
					},
				})
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"results": results}); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected sidecar call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(sidecar.Close)
	presetDir := t.TempDir()
	presets := make([]any, 0, 2)
	for _, name := range []string{"alpha", "beta"} {
		preset := fmt.Appendf(nil, "entities:\n  - name: organization\n    threshold: 0.4\nrelations:\n  - name: %s-relation\n    threshold: 0.4\n    text: works with %s\n", name, name)
		writeEvalFile(t, filepath.Join(presetDir, name+".yaml"), preset)
		presets = append(presets, map[string]any{"name": name, "sha256": fmt.Sprintf("%x", sha256.Sum256(preset))})
	}
	stoplist := []byte("# a synthetic stoplist\nUnrelated name\n")
	stoplistPath := filepath.Join(t.TempDir(), "stoplist.txt")
	writeEvalFile(t, stoplistPath, stoplist)
	digest := "sha256:" + strings.Repeat("3f", 32)
	for key, value := range map[string]string{
		"GKG_EXTRACTOR_MODE": "sidecar", "GKG_EXTRACTOR_SIDECAR_URL": sidecar.URL,
		"GKG_EXTRACTOR_SIDECAR_MODEL": judgeFixtureModel, "GKG_EXTRACTOR_SIDECAR_IMAGE": "fixture@" + digest,
		"GKG_LABELS_PRESETS_DIR": presetDir, "GKG_INGEST_STOPLIST_FILE": stoplistPath,
		"GKG_JUDGE_STORE_INPUTS": "true", "GKG_FACTS_REQUIRE_CONFIRMATION": "false",
		"GKG_INGEST_STORE_FLOOR": "0.15", "GKG_INGEST_ASSERT_FLOOR": "0.5",
	} {
		t.Setenv(key, value)
	}
	return st, map[string]any{
		"source": "synthetic", "backup_tar_sha256": strings.Repeat("a", 64), "schema_version": graph.ExtractionSchemaVersion,
		"write_mode":  map[string]any{"hygiene": true, "domains": true, "facts_as_candidates": false},
		"store_floor": .15, "assert_floor": .5,
		"stoplist_sha256": fmt.Sprintf("%x", sha256.Sum256(stoplist)), "sidecar": map[string]any{"model": judgeFixtureModel, "digest": digest},
		"presets": presets,
	}, &failChunk
}
