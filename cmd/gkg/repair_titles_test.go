package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestRepairTitlesFlags(t *testing.T) {
	for _, tt := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"-h"}, 0, "-apply"},
		{[]string{"-bogus"}, 2, "flag provided but not defined"},
		{[]string{"-out"}, 2, "flag needs an argument"},
		{[]string{"-domain", "Bad"}, 2, "invalid domain slug"},
		{[]string{"trailing"}, 2, "unexpected argument"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"repair-titles"}, tt.args...), &stdout, &stderr); code != tt.code ||
			stdout.Len() != 0 || !strings.Contains(stderr.String(), tt.want) {
			t.Errorf("repair-titles %v = %d, stdout %q, stderr %q", tt.args, code, &stdout, &stderr)
		}
	}
}

func useRepairTitlesDatabase(t *testing.T) {
	t.Helper()
	uri := os.Getenv(graphtest.URIEnv)
	if uri == "" {
		t.Skipf("set %s to run integration tests", graphtest.URIEnv)
	}
	t.Chdir(t.TempDir())
	t.Setenv("GKG_CONFIG", "")
	t.Setenv("GKG_MEMGRAPH_URI", uri)
	t.Setenv("GKG_MEMGRAPH_USER", "")
	t.Setenv("GKG_MEMGRAPH_PASSWORD", "")
	t.Setenv("GKG_INGEST_STOPLIST_FILE", "")
	t.Cleanup(func() {
		migrationQuery(t, `MATCH (d:Document)
WHERE d.domain IN ['repair-titles-fixture', 'repair-titles-other', 'repair-titles-batch']
OPTIONAL MATCH (d)-[:HAS_CHUNK]->(c:Chunk)
DETACH DELETE c, d`, nil)
	})
}

func TestRepairTitlesCLI(t *testing.T) {
	useRepairTitlesDatabase(t)
	const scope = "repair-titles-fixture"
	migrationQuery(t, `
CREATE (f:Document {id:'fixture-title-a', domain:$scope, source:'index.md', status:'done', updated_at:'fixture'}),
       (f)-[:HAS_CHUNK]->(:Chunk {seq:1, start:11, text:'Fixture Front\n---\n\n# Later'}),
       (f)-[:HAS_CHUNK]->(:Chunk {seq:0, start:0, text:'---\ntitle: Fixture Front'}),
       (h:Document {id:'fixture-title-b', domain:$scope, source:'index.md', title:''}),
       (h)-[:HAS_CHUNK]->(:Chunk {seq:1, start:16, text:'===\n\nFixture body.'}),
       (h)-[:HAS_CHUNK]->(:Chunk {seq:0, start:0, text:'Fixture Heading'}),
       (:Document {id:'fixture-title-c', domain:$scope, source:'fixture-slug.md#part', title:''}),
       (k:Document {id:'fixture-title-d', domain:$scope, source:'replace-me.md', title:'Fixture Kept', title_source:'caller'}),
       (k)-[:HAS_CHUNK]->(:Chunk {seq:0, start:0, text:'# Fixture replacement'}),
       (l:Document {id:'fixture-title-e', domain:$scope, source:'index.md'}),
       (l)-[:HAS_CHUNK]->(:Chunk {seq:0, start:0, text:$long}),
       (l)-[:HAS_CHUNK]->(:Chunk {seq:1, start:8192, text:'# Fixture beyond the limit'}),
       (:Document {id:'fixture-title-f', domain:$scope, source:'ab12cd34'}),
       (:Document {id:'fixture-title-outside', domain:'repair-titles-other', source:'outside-fixture.md', title:''})
`, map[string]any{"scope": scope, "long": strings.Repeat("è", 4095) + "."})

	// A persisted judge request and verdict must survive a title-only repair.
	t.Cleanup(func() {
		migrationQuery(t, `MATCH (n) WHERE n.doc_id = 'fixture-title-a' DETACH DELETE n`, nil)
	})
	migrationQuery(t, `
CREATE (:FactCandidate {id:'fixture-title-candidate', doc_id:'fixture-title-a', judge_input:'fixture request', input_hash:'fixture hash'}),
       (:JudgeVerdict {key:'fixture-title-verdict', doc_id:'fixture-title-a', status:'confirmed'})`, nil)
	judgeState := func() []map[string]any {
		t.Helper()
		rows := migrationQuery(t, `MATCH (n) WHERE n.doc_id = 'fixture-title-a' RETURN properties(n) AS state ORDER BY id(n)`, nil)
		var got []map[string]any
		for _, row := range rows {
			got = append(got, row.AsMap())
		}
		return got
	}
	before, judgeBefore := graphtest.Snapshot(t), judgeState()
	runCLI := func(want string, extra ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		args := append([]string{"repair-titles", "-domain", scope}, extra...)
		if code := run(args, &stdout, &stderr); code != 0 || stdout.String() != want+"\n" || stderr.Len() != 0 {
			t.Fatalf("%v = %d, stdout %q, stderr %q, want %q", args, code, &stdout, &stderr, want)
		}
	}
	const changes = "total=6 frontmatter=1 h1=1 slug=1 unchanged=3"
	report := filepath.Join(t.TempDir(), "titles.jsonl")
	runCLI(changes, "-out", report)
	before.AssertUnchanged(t, "repair-titles dry run")
	info, err := os.Stat(report)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("report mode = %o, want 600", info.Mode().Perm())
	}
	expect := []graph.DocumentTitleUpdate{
		{ID: "fixture-title-a", Title: "Fixture Front", TitleSource: "frontmatter"},
		{ID: "fixture-title-b", Title: "Fixture Heading", TitleSource: "h1"},
		{ID: "fixture-title-c", Title: "fixture slug", TitleSource: "slug"},
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var got []graph.DocumentTitleUpdate
	for line := range strings.Lines(string(data)) {
		var row graph.DocumentTitleUpdate
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("invalid JSONL row: %v", err)
		}
		got = append(got, row)
	}
	if !reflect.DeepEqual(got, expect) {
		t.Fatalf("report = %+v, want %+v", got, expect)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"repair-titles", "-apply", "-domain", scope, "-out", report}, &stdout, &stderr); code != 1 || stdout.Len() != 0 {
		t.Fatalf("existing report = %d, stdout %q, stderr %q", code, &stdout, &stderr)
	}
	if current, err := os.ReadFile(report); err != nil || !bytes.Equal(current, data) {
		t.Fatalf("existing report changed: %v", err)
	}
	before.AssertUnchanged(t, "repair-titles with existing report")

	runCLI(changes, "-apply", "-out", filepath.Join(t.TempDir(), "applied.jsonl"))
	stored := migrationQuery(t, `MATCH (d:Document) WHERE d.domain IN [$scope, 'repair-titles-other']
RETURN d.id AS id, coalesce(d.title, '') AS title, coalesce(d.title_source, '') AS origin,
       coalesce(d.status, '') AS status, coalesce(d.updated_at, '') AS updated_at ORDER BY id`, map[string]any{"scope": scope})
	expect = append(expect,
		graph.DocumentTitleUpdate{ID: "fixture-title-d", Title: "Fixture Kept", TitleSource: "caller"},
		graph.DocumentTitleUpdate{ID: "fixture-title-e"},
		graph.DocumentTitleUpdate{ID: "fixture-title-f"},
		graph.DocumentTitleUpdate{ID: "fixture-title-outside"},
	)
	if len(stored) != len(expect) {
		t.Fatalf("stored %d documents, want %d", len(stored), len(expect))
	}
	for i, row := range stored {
		state := row.AsMap()
		if want := expect[i]; state["id"] != want.ID || state["title"] != want.Title || state["origin"] != want.TitleSource {
			t.Fatalf("stored title = %v, want %+v", state, want)
		}
	}
	if state := stored[0].AsMap(); state["status"] != "done" || state["updated_at"] != "fixture" {
		t.Fatalf("repair altered pipeline state: %v", state)
	}
	if after := judgeState(); !reflect.DeepEqual(judgeBefore, after) {
		t.Fatalf("repair altered judge state: %v -> %v", judgeBefore, after)
	}
	repaired := graphtest.Snapshot(t)
	runCLI("total=6 frontmatter=0 h1=0 slug=0 unchanged=6", "-apply")
	repaired.AssertUnchanged(t, "repair-titles second apply")
}

func TestRepairTitlesBatches(t *testing.T) {
	useRepairTitlesDatabase(t)
	const scope = "repair-titles-batch"
	ids := make([]string, titleRepairBatch+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("fixture-title-batch-%03d", i)
	}
	migrationQuery(t, `UNWIND $ids AS id CREATE (:Document {id:id, domain:$scope, source:'fixture-batch.md'})`,
		map[string]any{"ids": ids, "scope": scope})
	var stdout, stderr bytes.Buffer
	want := fmt.Sprintf("total=%d frontmatter=0 h1=0 slug=%d unchanged=0\n", len(ids), len(ids))
	reportPath := filepath.Join(t.TempDir(), "applied.jsonl")
	if code := run([]string{"repair-titles", "-apply", "-domain", scope, "-out", reportPath}, &stdout, &stderr); code != 0 ||
		stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("batch apply = %d, stdout %q, stderr %q, want %q", code, &stdout, &stderr, want)
	}
	report, err := os.Open(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	defer report.Close()
	decoder := json.NewDecoder(report)
	for _, id := range ids {
		var row graph.DocumentTitleUpdate
		want := graph.DocumentTitleUpdate{ID: id, Title: "fixture batch", TitleSource: "slug"}
		if err := decoder.Decode(&row); err != nil || row != want {
			t.Fatalf("report row = %+v, %v, want %+v", row, err, want)
		}
	}
	var extra graph.DocumentTitleUpdate
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("report after final row = %+v, %v, want EOF", extra, err)
	}
	rows := migrationQuery(t, `MATCH (d:Document {domain:$scope, title:'fixture batch', title_source:'slug'}) RETURN count(d) AS n`,
		map[string]any{"scope": scope})
	if n, _ := rows[0].Get("n"); n != int64(len(ids)) {
		t.Errorf("repaired %v documents, want %d", n, len(ids))
	}
}
