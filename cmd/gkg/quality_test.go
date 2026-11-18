package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/quality"
)

func TestQualityFlags(t *testing.T) {
	for _, args := range [][]string{{"--bogus"}, {"positional"}, {"--hygiene-sample", "-1"}, {"--hygiene-sample", "1"}, {"--out", "sample.json"}, {"--domain", "BAD"}} {
		var out, stderr bytes.Buffer
		if code := run(append([]string{"quality"}, args...), &out, &stderr); code != 2 {
			t.Errorf("%v: exit=%d stderr=%s", args, code, stderr.String())
		}
	}
	var out, stderr bytes.Buffer
	if code := run([]string{"quality", "--help"}, &out, &stderr); code != 0 {
		t.Fatalf("help exit=%d stderr=%s", code, stderr.String())
	}
	for _, word := range []string{"-domain", "-seed", "-json", "-hygiene-sample", "-sample-seed", "-out", "-duplicates-out", "per-document label preset", "every supporting document", "entity.type", "read-only"} {
		if !strings.Contains(stderr.String(), word) {
			t.Errorf("help missing %s", word)
		}
	}
	out.Reset()
	usage(&out)
	if !strings.Contains(out.String(), "quality") {
		t.Fatal("global usage missing quality")
	}
}

func TestQualityCLISeedSampleAndReadOnly(t *testing.T) {
	st, id, _ := migrationCommandFixture(t)
	migrationQuery(t, `MATCH (m:GkgMeta {id:'extraction_schema'}) DELETE m`, nil)
	migrationQuery(t, `MATCH (d:Document {id:$id}) SET d.domain='quality-cli' WITH d MATCH (d)-[:HAS_CHUNK]->(c:Chunk) WITH c LIMIT 1 CREATE (a:Entity {key:$id+'-drop',name:'--zorbo',type:'entity'}), (b:Entity {key:$id+'-rename',name:'Quillon Ltd',type:'organization'}), (c)-[:MENTIONS]->(a), (c)-[:MENTIONS]->(b)`, map[string]any{"id": id})
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GKG_CONFIG", cfg)
	t.Setenv("GKG_MEMGRAPH_URI", os.Getenv(graphtest.URIEnv))
	before := graphtest.Snapshot(t)
	doc, err := st.Document(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	seedBytes, _ := json.Marshal(map[string]any{"schema": 1, "names": map[string]string{}, "rules": []map[string]string{{"match": doc.Source, "domain": "quality-cli-seed"}}})
	seedFile := filepath.Join(dir, "seed.json")
	if err := os.WriteFile(seedFile, seedBytes, 0600); err != nil {
		t.Fatal(err)
	}
	sample := filepath.Join(dir, "sample.json")
	// Existing files must also become private, and sample names stay out of stdout.
	if err := os.WriteFile(sample, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, seedPath := range []string{seedFile, "-"} {
		stdin, err := os.Open(seedFile)
		if err != nil {
			t.Fatal(err)
		}
		previous := os.Stdin
		os.Stdin = stdin
		var out, stderr bytes.Buffer
		code := run([]string{"quality", "--json", "--seed", seedPath, "--domain", "quality-cli-seed", "--hygiene-sample", "1", "--sample-seed", "7", "--out", sample}, &out, &stderr)
		os.Stdin = previous
		stdin.Close()
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, stderr.String())
		}
		var report map[string]any
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		if report["metrics"] != float64(3) || report["documents"] != float64(1) || report["cross_domain_entities"] != nil || report["domain_coverage"] != float64(1) {
			t.Fatalf("report %s", out.String())
		}
		if strings.Contains(out.String(), "Quillon") || strings.Contains(out.String(), "--zorbo") || strings.Contains(stderr.String(), "Quillon") {
			t.Fatal("sample leaked")
		}
		data, err := os.ReadFile(sample)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Dropped, Renamed []struct{ Original, Normalized, Rule string }
		}
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Dropped) != 1 || len(got.Renamed) != 1 || got.Dropped[0].Original != "--zorbo" || got.Renamed[0].Normalized != "Quillon" {
			t.Fatalf("sample %s", data)
		}
		info, err := os.Stat(sample)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("sample permissions: %v %v", info, err)
		}
	}
	var out, stderr bytes.Buffer
	if code := run([]string{"quality", "--domain", "no-such-domain"}, &out, &stderr); code != 0 || !strings.Contains(out.String(), "n/a") || !strings.Contains(out.String(), "per-document label preset") {
		t.Fatalf("text report exit=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	before.AssertUnchanged(t, "quality CLI")
}

// TestQualityTextKeepsLargeCounts: text output prints counts as literals,
// not in exponent form.
func TestQualityTextKeepsLargeCounts(t *testing.T) {
	var b strings.Builder
	if err := writeQualityText(&b, quality.Report{Metrics: 1, Mentions: 1234567, Documents: 1000000}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mentions: 1234567\n", "documents: 1000000\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("text output lacks %q:\n%s", want, b.String())
		}
	}
}
