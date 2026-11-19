package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestRepairLabelsFlags(t *testing.T) {
	for _, tt := range []struct {
		args []string
		code int
		text string
	}{
		{[]string{"-h"}, 0, "-dry-run"},
		{[]string{"-bogus"}, 2, "flag provided but not defined"},
		{[]string{"-domain"}, 2, "flag needs an argument"},
		{[]string{"-domain", "Bad"}, 2, "invalid domain slug"},
		{[]string{"trailing"}, 2, "unexpected argument"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"repair-labels"}, tt.args...), &stdout, &stderr); code != tt.code ||
			!strings.Contains(stderr.String(), tt.text) || stdout.Len() != 0 {
			t.Errorf("repair-labels %v = %d, stdout %q, stderr %q", tt.args, code, &stdout, &stderr)
		}
	}
}

func TestRepairLabelsReportFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.jsonl")
	f, err := openLabelRepairReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("original\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("report file mode = %o, want 600", mode)
	}
	if f, err := openLabelRepairReport(path); !errors.Is(err, os.ErrExist) {
		if f != nil {
			f.Close()
		}
		t.Fatalf("opening existing report = %v, want ErrExist", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "original\n" {
		t.Fatalf("report contents = %q, %v", data, err)
	}
}

func TestRepairLabelsCLI(t *testing.T) {
	graphtest.Store(t)
	t.Chdir(t.TempDir())
	t.Setenv("GKG_CONFIG", "")
	t.Setenv("GKG_MEMGRAPH_URI", os.Getenv(graphtest.URIEnv))
	t.Setenv("GKG_MEMGRAPH_USER", "")
	t.Setenv("GKG_MEMGRAPH_PASSWORD", "")
	t.Setenv("GKG_INGEST_STOPLIST_FILE", "")
	const labelLine = "## Part 3: " // 11 bytes, so a 2-byte rune straddles byte 200.
	text := labelLine + strings.Repeat("è", 100) + "\n## Approved: details\n- **Remove**: details\n## Kelmora (14)"
	const docID = "label-doc"
	const chunkID = "label-chunk"
	const scope = "repair-labels-cli"
	t.Cleanup(func() {
		migrationQuery(t, `MATCH (d:Document) WHERE d.domain = $scope OR d.id = 'label-other-doc'
OPTIONAL MATCH (d)-[:HAS_CHUNK]->(c:Chunk)
DETACH DELETE c, d`, map[string]any{"scope": scope})
		migrationQuery(t, `MATCH (e:Entity)
WHERE e.key IN ['label-cli-part', 'label-cli-approved', 'label-cli-remove', 'label-cli-skipped', 'label-cli-kelmora']
DETACH DELETE e`, nil)
	})
	migrationQuery(t, `CREATE (d:Document {id: $doc, domain: $scope, labels: 'custom', status: 'done'}),
(c:Chunk {id: $chunk, text: $text}), (d)-[:HAS_CHUNK]->(c),
(out:Document {id: 'label-other-doc', domain: 'other', status: 'done'})-[:HAS_CHUNK]->(b:Chunk {id: 'label-other-chunk', text: 'Part 3 is in prose.'}),
(e:Entity {id: 'label-cli-entity', key: 'label-cli-part', name: 'Part 3', mention_count: 999}),
(a:Entity {id: 'allowed-cli-entity', key: 'label-cli-approved', name: 'Approved'}),
(r:Entity {id: 'remove-cli-entity', key: 'label-cli-remove', name: 'Remove'}),
(l:Entity {id: 'location-cli-entity', key: 'label-cli-kelmora', name: 'Kelmora'}),
(c)-[:MENTIONS {start: 3, end: 9, type: 'topic'}]->(e),
(c)-[:MENTIONS {start: $allowed, end: $allowed + 8, type: 'topic'}]->(a),
(c)-[:MENTIONS {start: $remove, end: $remove + 6, type: 'topic'}]->(r),
(c)-[:MENTIONS {start: $location, end: $location + 7, type: 'location'}]->(l),
(b)-[:MENTIONS {start: 0, end: 6, type: 'topic'}]->(e)`, map[string]any{
		"doc": docID, "chunk": chunkID, "scope": scope,
		"text": text, "allowed": strings.Index(text, "Approved"), "remove": strings.Index(text, "Remove"),
		"location": strings.Index(text, "Kelmora"),
	})
	migrationQuery(t, `MATCH (d:Document {id: $doc})
UNWIND range(1, 201) AS n
CREATE (d)-[:HAS_CHUNK]->(:Chunk {id: 'empty-' + toString(n), text: ''})`, map[string]any{"doc": docID})
	migrationQuery(t, `MATCH (d:Document {id: $doc}), (r:Entity {key: 'label-cli-remove'})
CREATE (d)-[:HAS_CHUNK]->(:Chunk {id: 'third-chunk', text: '## Remove — details'})
       -[:MENTIONS {start: 3, end: 9, type: 'topic'}]->(r)`, map[string]any{"doc": docID})
	migrationQuery(t, `CREATE (busy:Document {id: 'label-busy-doc', domain: $scope, status: 'chunked'}),
(missing:Document {id: 'label-missing-doc', domain: $scope, status: 'done', labels: 'missing-preset'}),
(e:Entity {key: 'label-cli-skipped', name: 'Skipped'})
WITH busy, missing, e
UNWIND [busy, missing] AS d
UNWIND [1, 2] AS n
CREATE (d)-[:HAS_CHUNK]->(:Chunk {id: d.id + '-c' + toString(n), text: '## Skipped: details'})
       -[:MENTIONS {start: 3, end: 10, type: 'topic'}]->(e)`, map[string]any{"scope": scope})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "custom.yaml"), []byte("allowlist: [Approved]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_LABELS_PRESETS_DIR", dir)
	path := filepath.Join(dir, "report.jsonl")
	check := func(want string, flags ...string) {
		t.Helper()
		var out, errb bytes.Buffer
		warn := "warning: skipping document label-missing-doc: hygiene preset \"missing-preset\""
		if code := run(append([]string{"repair-labels", "-domain", scope}, flags...), &out, &errb); code != 0 ||
			out.String() != want || !strings.Contains(errb.String(), warn) || strings.Count(errb.String(), "\n") != 1 {
			t.Fatalf("repair-labels %v = %d, stdout %q, stderr %q, want %q", flags, code, &out, &errb, want)
		}
	}
	const matched = "scanned_chunks=207 spans=3 heading_lead=2 lead_in=1 facts_confirmed=0 facts_other=0 candidates=0 rejected=0 entities_removed=1 skipped_docs=2\n"
	check(matched, "-dry-run", "-report", path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 4 || lines[3] != `{"skipped_docs":2}` {
		t.Fatalf("report lines = %q", lines)
	}
	for i, want := range []struct {
		chunk, key, name, rule, line string
		removed                      bool
	}{
		{chunkID, "label-cli-part", "Part 3", "heading_lead", labelLine + strings.Repeat("è", 94), false},
		{chunkID, "label-cli-remove", "Remove", "lead_in", "- **Remove**: details", true},
		{"third-chunk", "label-cli-remove", "Remove", "heading_lead", "## Remove — details", true},
	} {
		var row struct {
			Domain  string `json:"domain"`
			DocID   string `json:"doc_id"`
			ChunkID string `json:"chunk_id"`
			Key     string `json:"entity_key"`
			Name    string `json:"name"`
			Type    string `json:"type"`
			Rule    string `json:"rule"`
			Removed bool   `json:"entity_removed"`
			Line    string `json:"line"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &row); err != nil {
			t.Fatal(err)
		}
		if row.Domain != scope || row.DocID != docID || row.ChunkID != want.chunk || row.Type != "topic" ||
			row.Key != want.key || row.Name != want.name || row.Rule != want.rule || row.Removed != want.removed ||
			row.Line != want.line || !utf8.ValidString(row.Line) || len(row.Line) > 200 {
			t.Errorf("report row %d = %+v, want %+v", i, row, want)
		}
	}
	applyPath := filepath.Join(dir, "apply.jsonl")
	check(matched, "-report", applyPath)
	if appliedReport, err := os.ReadFile(applyPath); err != nil || !bytes.Equal(appliedReport, data) {
		t.Fatalf("apply report = %q, %v, want %q", appliedReport, err, data)
	}
	check("scanned_chunks=207 spans=0 heading_lead=0 lead_in=0 facts_confirmed=0 facts_other=0 candidates=0 rejected=0 entities_removed=0 skipped_docs=2\n", "-dry-run")
	locationRows := migrationQuery(t, `MATCH (:Chunk {id: $chunk})-[m:MENTIONS {type: 'location'}]->(:Entity {key: 'label-cli-kelmora'})
RETURN count(m) AS n`, map[string]any{"chunk": chunkID})
	if n, _ := locationRows[0].Get("n"); n != int64(1) {
		t.Errorf("location mentions after repair = %v, want 1", n)
	}
	rows := migrationQuery(t, `MATCH (:Chunk)-[m:MENTIONS]->(:Entity {key: 'label-cli-skipped'}) RETURN count(m) AS n`, nil)
	if n, _ := rows[0].Get("n"); n != int64(4) {
		t.Errorf("skipped mentions = %v, want 4", n)
	}
}

func TestRepairLabelsCLIWithoutReport(t *testing.T) {
	graphtest.Store(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GKG_CONFIG", "")
	t.Setenv("GKG_MEMGRAPH_URI", os.Getenv(graphtest.URIEnv))
	t.Setenv("GKG_MEMGRAPH_USER", "")
	t.Setenv("GKG_MEMGRAPH_PASSWORD", "")
	t.Setenv("GKG_INGEST_STOPLIST_FILE", "")

	const scope = "repair-labels-no-report"
	const text = "## Overview: details\n- **Notes**: details\n## Project: details\nProject uses Atlas."
	proseStart := strings.LastIndex(text, "Project")
	atlasStart := strings.Index(text, "Atlas")
	t.Cleanup(func() {
		migrationQuery(t, `MATCH (n)
WHERE n.id IN [$scope + '-doc', $scope + '-chunk'] OR n.key STARTS WITH $scope + '-'
DETACH DELETE n`, map[string]any{"scope": scope})
	})
	migrationQuery(t, `CREATE (d:Document {id: $scope + '-doc', domain: $scope, status: 'done'}),
(c:Chunk {id: $scope + '-chunk', text: $text}), (d)-[:HAS_CHUNK]->(c),
(o:Entity {key: $scope + '-overview', name: 'Overview'}),
(n:Entity {key: $scope + '-notes', name: 'Notes'}),
(p:Entity {key: $scope + '-project', name: 'Project'}),
(a:Entity {key: $scope + '-atlas', name: 'Atlas'}),
(c)-[:MENTIONS {start: 3, end: 11, type: 'topic'}]->(o),
(c)-[:MENTIONS {start: $notes, end: $notes + 5, type: 'topic'}]->(n),
(c)-[:MENTIONS {start: $heading, end: $heading + 7, type: 'topic'}]->(p),
(c)-[:MENTIONS {start: $prose, end: $prose + 7, type: 'topic'}]->(p),
(c)-[:MENTIONS {start: $atlas, end: $atlas + 5, type: 'topic'}]->(a)`, map[string]any{
		"scope": scope, "text": text, "notes": strings.Index(text, "Notes"),
		"heading": strings.Index(text, "Project"), "prose": proseStart, "atlas": atlasStart,
	})

	check := func(want string) {
		t.Helper()
		var out, stderr bytes.Buffer
		if code := run([]string{"repair-labels", "-domain", scope}, &out, &stderr); code != 0 ||
			out.String() != want || stderr.Len() != 0 {
			t.Fatalf("repair-labels exit=%d, stdout %q, stderr %q, want %q", code, &out, &stderr, want)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("repair-labels without -report created files: %v", entries)
		}
	}
	check("scanned_chunks=1 spans=3 heading_lead=2 lead_in=1 facts_confirmed=0 facts_other=0 candidates=0 rejected=0 entities_removed=2 skipped_docs=0\n")

	params := map[string]any{"scope": scope}
	mentions := migrationQuery(t, `MATCH (:Chunk {id: $scope + '-chunk'})-[m:MENTIONS]->(e:Entity)
RETURN e.key AS key, m.start AS start, m.end AS end, m.type AS type ORDER BY m.start`, params)
	if len(mentions) != 2 {
		t.Fatalf("remaining mentions = %v, want only the two prose mentions", mentions)
	}
	for i, want := range []struct {
		name  string
		start int
	}{{"Project", proseStart}, {"Atlas", atlasStart}} {
		mention := mentions[i].AsMap()
		if mention["key"] != scope+"-"+strings.ToLower(want.name) || mention["start"] != int64(want.start) ||
			mention["end"] != int64(want.start+len(want.name)) || mention["type"] != "topic" {
			t.Errorf("remaining mention = %v, want the %s prose mention at byte %d", mention, want.name, want.start)
		}
	}
	entities := migrationQuery(t, `MATCH (e:Entity) WHERE e.key STARTS WITH $scope + '-' RETURN e.key AS key ORDER BY key`, params)
	if len(entities) != 2 || entities[0].AsMap()["key"] != scope+"-atlas" || entities[1].AsMap()["key"] != scope+"-project" {
		t.Errorf("remaining fixture entities = %v, want only Atlas and Project", entities)
	}

	before := graphtest.Snapshot(t)
	check("scanned_chunks=1 spans=0 heading_lead=0 lead_in=0 facts_confirmed=0 facts_other=0 candidates=0 rejected=0 entities_removed=0 skipped_docs=0\n")
	before.AssertUnchanged(t, "second repair-labels run without -report")
}
