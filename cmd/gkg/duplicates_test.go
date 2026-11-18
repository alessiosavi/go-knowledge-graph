package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/quality"
)

func TestQualityDuplicateCensusOutputCollision(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GKG_CONFIG", filepath.Join(dir, "missing.yaml"))
	path := filepath.Join(dir, "collision.jsonl")
	if err := os.Symlink(dir, "alias"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("collision.jsonl", "sample-link"); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []string{"collision.jsonl", "alias/collision.jsonl", "sample-link"} {
		t.Run(sample, func(t *testing.T) {
			var out, stderr bytes.Buffer
			args := []string{"quality", "--duplicates-out", dir + "/./collision.jsonl", "--out", sample, "--hygiene-sample", "1"}
			if code := run(args, &out, &stderr); code != 2 || !strings.Contains(stderr.String(), "require different files") {
				t.Errorf("collision: exit=%d stdout=%s stderr=%s", code, &out, &stderr)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Errorf("collision left an output file: %v", err)
			}
		})
	}
}

type failedQualityReportWriter struct{}

func (failedQualityReportWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestQualityDuplicateCensus(t *testing.T) {
	graphtest.MigrationStore(t)
	migrationQuery(t, `MATCH (m:GkgMeta {id:'extraction_schema'}) DELETE m`, nil)
	tag := fmt.Sprintf("gkg-census-%d", time.Now().UnixNano())
	key := func(suffix string) string { return graph.DomainKey("census-keys", tag+suffix) }
	long := "Kathmandu " + strings.Repeat("界", 100) + "."
	migrationQuery(t, `CREATE
 (d:Document {id:$tag, source:$tag, domain:'census-fixture', status:'done'}),
 (outside:Document {id:$tag+'-outside', source:'outside/'+$tag, domain:'census-outside', status:'done'}),
 (c1:Chunk {id:$tag+'-1', doc_id:$tag, text:'A false lead. Kathmandù is here.'}),
 (c2:Chunk {id:$tag+'-2', doc_id:$tag, text:$long}),
 (c3:Chunk {id:$tag+'-3', doc_id:$tag, text:'Kathmandu also appears here.'}),
 (c4:Chunk {id:$tag+'-0', doc_id:outside.id, text:'Kathmandu outside the selected domain.'}),
 (a:Entity {key:$keyPrefix+'-a', name:'Kathmandù', type:'location', mention_count:99}),
 (b:Entity {key:$keyPrefix+'-b', name:'Kathmandu', type:'location', mention_count:0}),
 (c:Entity {key:$keyPrefix+'-c', name:'Alpha+', type:'entity'}),
 (e:Entity {key:$keyPrefix+'-e', name:'Alpha', type:'entity'}),
 (f:Entity {key:'#'+$tag+':fixture-1', name:'Fixture', type:'value'}),
 (g:Entity {key:'@census-fixture:#'+$tag+':fixture-2', name:'Fixturè', type:'value'}),
 (h:Entity {key:$keyPrefix+'-h', name:'Kathmandú', type:'location'}),
 (d)-[:HAS_CHUNK]->(c1), (d)-[:HAS_CHUNK]->(c2), (d)-[:HAS_CHUNK]->(c3), (outside)-[:HAS_CHUNK]->(c4),
 (c1)-[:MENTIONS {start:14, end:24}]->(a),
 (c2)-[:MENTIONS {start:0, end:9}]->(b), (c3)-[:MENTIONS {start:0, end:9}]->(b),
 (c4)-[:MENTIONS {start:0, end:9}]->(b), (c4)-[:MENTIONS]->(h),
 (c1)-[:MENTIONS]->(c), (c1)-[:MENTIONS]->(e), (c1)-[:MENTIONS]->(f), (c1)-[:MENTIONS]->(g),
 (a)-[:knows {doc_id:$tag, chunk_id:c1.id, judge:'confirmed'}]->(b),
 (b)-[:uses {doc_id:$tag, chunk_id:c2.id, judge:'unclear'}]->(a),
 (a)-[:knows {doc_id:$tag, chunk_id:c1.id}]->(a),
 (h)-[:knows {doc_id:outside.id, chunk_id:c4.id}]->(a),
 (:FactCandidate {id:$tag+'-c1', head_key:a.key, tail_key:b.key, doc_id:$tag}),
 (:FactCandidate {id:$tag+'-c2', head_key:a.key, tail_key:a.key, doc_id:$tag}),
 (:FactCandidate {id:$tag+'-c3', head_key:'missing', tail_key:b.key, doc_id:outside.id}),
 (:RejectedFact {id:$tag+'-r1', head_key:a.key, tail_key:b.key, doc_id:$tag}),
 (:RejectedFact {id:$tag+'-r2', head_key:b.key, tail_key:b.key, doc_id:$tag})`, map[string]any{"tag": tag, "long": long, "keyPrefix": key("")})
	t.Cleanup(func() {
		migrationQuery(t, `MATCH (n) WHERE n.id STARTS WITH $tag OR n.key CONTAINS $tag DETACH DELETE n`, map[string]any{"tag": tag})
	})

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_CONFIG", cfg)
	t.Setenv("GKG_MEMGRAPH_URI", os.Getenv(graphtest.URIEnv))
	before := graphtest.Snapshot(t)

	seed := filepath.Join(dir, "seed.json")
	seedJSON := fmt.Sprintf(`{"schema":1,"rules":[{"match":%q,"domain":"census-selected"}]}`, tag)
	if err := os.WriteFile(seed, []byte(seedJSON), 0600); err != nil {
		t.Fatal(err)
	}
	baseArgs := []string{"quality", "--domain", "census-selected", "--seed", seed}
	path := filepath.Join(dir, "duplicates.jsonl")
	args := append(slices.Clone(baseArgs), "--duplicates-out", path)
	var out, stderr bytes.Buffer
	if code := run(args, &out, &stderr); code != 0 {
		t.Fatalf("census exit=%d stderr=%s", code, &stderr)
	}
	if !strings.Contains(out.String(), "duplicate census groups A=1 other=1, members A=2 other=2, group size max=17 p95=17\n") {
		t.Fatalf("missing or incorrect census summary:\n%s", &out)
	}
	if strings.Contains(out.String()+stderr.String(), "Kathmand") || strings.Contains(out.String()+stderr.String(), "Alpha") {
		t.Fatal("private census data leaked to command output")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("census file permissions: %v %v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) != 3 || len(lines[2]) != 0 {
		t.Fatalf("expected exactly two JSONL rows, got %q", data)
	}
	want := []string{
		fmt.Sprintf(`{"domain":"census-keys","fold":"alpha","tier":"other","reason":"symbol_mismatch","suggested_canonical":%q,"members":[
 {"key":%q,"name":"Alpha+","type":"entity","mention_count":1,"document_count":1,"typed_edges":{"confirmed":0,"other":0},"candidates":0,"rejected":0,"evidence_sentences":[]},
 {"key":%q,"name":"Alpha","type":"entity","mention_count":1,"document_count":1,"typed_edges":{"confirmed":0,"other":0},"candidates":0,"rejected":0,"evidence_sentences":[]}]}`, key("-c"), key("-c"), key("-e")),
		fmt.Sprintf(`{"domain":"census-keys","fold":"kathmandu","tier":"A","reason":"","suggested_canonical":%q,"members":[
 {"key":%q,"name":"Kathmandù","type":"location","mention_count":1,"document_count":1,"typed_edges":{"confirmed":1,"other":3},"candidates":2,"rejected":1,"evidence_sentences":["Kathmandù is here."]},
 {"key":%q,"name":"Kathmandu","type":"location","mention_count":3,"document_count":1,"typed_edges":{"confirmed":1,"other":1},"candidates":2,"rejected":2,"evidence_sentences":[%q,"Kathmandu also appears here."]}]}`,
			key("-b"), key("-a"), key("-b"), "Kathmandu "+strings.Repeat("界", 63)),
	}
	for i, expected := range want {
		var got, expectedRow map[string]any
		if err := json.Unmarshal(lines[i], &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(expected), &expectedRow); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, expectedRow) {
			t.Errorf("row %d = %s\nwant %s", i+1, lines[i], expected)
		}
	}

	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := run(args, &out, &stderr); code != 1 || !strings.Contains(stderr.String(), "exists") {
		t.Fatalf("existing file: exit=%d stdout=%s stderr=%s", code, &out, &stderr)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("existing file modified: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("existing file mode changed: %v %v", info, err)
	}

	out.Reset()
	stderr.Reset()
	reportArgs := append(slices.Clone(baseArgs), "--json", "--hygiene-sample", "1", "--out", filepath.Join(dir, "sample.json"))
	if code := run(reportArgs, &out, &stderr); code != 0 {
		t.Fatalf("report without census: exit=%d stderr=%s", code, &stderr)
	}
	var baseline quality.Report
	if err := json.Unmarshal(out.Bytes(), &baseline); err != nil || baseline.DuplicateGroups != 3 || baseline.DuplicateEntities != 6 {
		t.Fatalf("report without census = %s (%v)", &out, err)
	}
	out.Reset()
	stderr.Reset()
	args = append(slices.Clone(reportArgs), "--duplicates-out", filepath.Join(dir, "combined.jsonl"))
	if code := run(args, &out, &stderr); code != 0 {
		t.Fatalf("combined options: exit=%d stderr=%s", code, &stderr)
	}
	var report struct {
		quality.Report
		DuplicateCensus struct {
			MaxEdges int `json:"max_edges"`
		} `json:"duplicate_census"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.DuplicateCensus.MaxEdges != 17 {
		t.Fatalf("combined JSON report = %s (%v)", &out, err)
	}
	if report.DuplicateGroups != baseline.DuplicateGroups || report.DuplicateEntities != baseline.DuplicateEntities {
		t.Errorf("duplicate metrics changed: groups %d → %d, entities %d → %d", baseline.DuplicateGroups, report.DuplicateGroups, baseline.DuplicateEntities, report.DuplicateEntities)
	}

	t.Run("sample write failure", func(t *testing.T) {
		path := filepath.Join(dir, "failed-sample.jsonl")
		var out, stderr bytes.Buffer
		args := append(slices.Clone(baseArgs), "--duplicates-out", path, "--hygiene-sample", "1", "--out", dir)
		if code := run(args, &out, &stderr); code != 1 || !strings.Contains(stderr.String(), "write sample:") {
			t.Fatalf("sample write failure: exit=%d stderr=%s", code, &stderr)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("sample failure left a census file: %v", err)
		}
	})
	t.Run("report write failure", func(t *testing.T) {
		path := filepath.Join(dir, "failed-report.jsonl")
		var stderr bytes.Buffer
		args := append(slices.Clone(baseArgs), "--duplicates-out", path, "--json")
		if code := run(args, failedQualityReportWriter{}, &stderr); code != 1 || !strings.Contains(stderr.String(), "report:") {
			t.Fatalf("report write failure: exit=%d stderr=%s", code, &stderr)
		}
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("report failure left a census file: %v", err)
		}
	})
	before.AssertUnchanged(t, "quality census")
}
