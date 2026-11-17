package ingest_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
)

type hygieneProvider struct{}

func (hygieneProvider) Name() string    { return "hygiene-fixture" }
func (hygieneProvider) Version() string { return "hygiene-fixture:1" }
func (hygieneProvider) Extract(_ context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	var out []extract.ChunkResult
	for _, c := range req.Chunks {
		r := extract.ChunkResult{ChunkID: c.ID}
		for _, name := range []string{"l'Acme", "l'Zorbo", "Quillon Ltd", "Descrizione", "Indice", "'s"} {
			if start := strings.Index(c.Text, name); start >= 0 {
				r.Mentions = append(r.Mentions, extract.Mention{Label: "organization", Text: name, Start: start, End: start + len(name), Score: .9})
			}
		}
		for i := range r.Mentions {
			for j := range r.Mentions {
				if i != j {
					r.Candidates = append(r.Candidates, extract.Candidate{Head: i, Tail: j, Label: "uses", Score: .9})
				}
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func TestPipelineHygiene(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			env := newPipelineEnv(t)
			env.cfg.Labels.PresetsDir = "../../labels"
			env.cfg.Ingest.ChunkWords = 7
			env.cfg.Ingest.ChunkDedup = "off"
			env.p = ingest.NewPipeline(env.cfg, env.st, hygieneProvider{}, nil, textLoader)
			env.p.SetWriteMode(ingest.WriteMode{Hygiene: enabled})
			metrics := obs.New()
			env.p.SetMetrics(metrics)
			text := "l'Acme uses Quillon Ltd. Descrizione repeats. l'Zorbo uses Quillon Ltd. Indice repeats."
			source := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
			res := mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text})
			uri := os.Getenv(graphtest.URIEnv)
			rows := rawRecords(t, uri, fmt.Sprintf("MATCH (d:Document {id:'%s'}) RETURN d.hygiene AS hygiene", res.DocumentID))
			stored, _ := rows[0].Get("hygiene")
			if enabled {
				counts, ok := stored.(map[string]any)
				if !ok || counts["stoplist"] != int64(2) || counts["renamed"] != int64(4) {
					t.Errorf("Document.hygiene=%#v, want stoplist=2 renamed=4", stored)
				}
			} else if stored != nil {
				t.Errorf("legacy hygiene=%#v, want absent", stored)
			}
			chunks, err := env.st.Chunks(context.Background(), res.DocumentID)
			if err != nil {
				t.Fatal(err)
			}
			if len(chunks) != 2 {
				t.Fatalf("chunks=%d, want 2", len(chunks))
			}
			var wantKeys []string
			if enabled {
				wantKeys = []string{"acme", "quillon", "zorbo"}
			} else {
				wantKeys = []string{"descrizione", "indice", "l'acme", "l'zorbo", "quillon ltd"}
			}
			rows = rawRecords(t, uri, fmt.Sprintf("MATCH (:Document {id:'%s'})-[:HAS_CHUNK]->(:Chunk)-[:MENTIONS]->(e:Entity) RETURN DISTINCT e.key AS key ORDER BY key", res.DocumentID))
			var gotKeys []string
			for _, r := range rows {
				gotKeys = append(gotKeys, testString(r, "key"))
			}
			if !slices.Equal(gotKeys, wantKeys) {
				t.Errorf("keys=%v, want %v", gotKeys, wantKeys)
			}
			for _, c := range chunks {
				rows = rawRecords(t, uri, fmt.Sprintf("MATCH (:Chunk {id:'%s'})-[m:MENTIONS]->(e:Entity) RETURN e.key AS key, m.start AS start, m.end AS end", c.ID))
				for _, r := range rows {
					start, end := int(testInt64(r, "start")), int(testInt64(r, "end"))
					raw := c.Text[start:end]
					key := testString(r, "key")
					if (key == "acme" && raw != "l'Acme") || (key == "zorbo" && raw != "l'Zorbo") || (key == "quillon" && raw != "Quillon Ltd") {
						t.Errorf("key=%q raw span=%q", key, raw)
					}
				}
			}
			if enabled {
				view, err := env.st.EntityView(context.Background(), "quillon", false)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(view.Aliases, "Quillon Ltd") {
					t.Errorf("aliases=%v", view.Aliases)
				}
				if n := rawCount(t, uri, fmt.Sprintf("MATCH (a:Entity)-[r:uses]->(b:Entity) WHERE r.doc_id='%s' AND (a.key IN ['descrizione','indice'] OR b.key IN ['descrizione','indice']) RETURN count(r) AS n", res.DocumentID)); n != 0 {
					t.Errorf("facts with dropped endpoints=%d", n)
				}
				rows = rawRecords(t, uri, fmt.Sprintf("MATCH (:Entity)-[r:uses]->(:Entity) WHERE r.doc_id='%s' RETURN r.chunk_id AS chunk, r.head_start AS start, r.head_end AS end, r.head_key AS key", res.DocumentID))
				if len(rows) != 4 {
					t.Errorf("facts=%d, want 4", len(rows))
				}
				for _, r := range rows {
					for _, c := range chunks {
						if c.ID == testString(r, "chunk") {
							raw := c.Text[int(testInt64(r, "start")):int(testInt64(r, "end"))]
							if key := testString(r, "key"); key == "acme" && raw != "l'Acme" {
								t.Errorf("fact raw head=%q", raw)
							}
						}
					}
				}
				scrape := gather(t, metrics)
				for _, want := range []string{`gkg_hygiene_dropped_total{rule="stoplist"} 2`, "gkg_hygiene_renamed_total 4"} {
					if !strings.Contains(scrape, want) {
						t.Errorf("metrics missing %q:\n%s", want, scrape)
					}
				}
				// Reprocessing replaces durable counts instead of accumulating them.
				mustProcess(t, env, ingest.Input{Kind: "text", Source: source, Text: text, Force: true})
				again := rawRecords(t, uri, fmt.Sprintf("MATCH (d:Document {id:'%s'}) RETURN d.hygiene AS hygiene", res.DocumentID))
				updated, _ := again[0].Get("hygiene")
				if !reflect.DeepEqual(updated, stored) {
					t.Errorf("counts accumulated: %#v -> %#v", stored, updated)
				}
			}
		})
	}
}

func TestPipelineHygieneOverlay(t *testing.T) {
	env := newPipelineEnv(t)
	env.cfg.Labels.PresetsDir = "../../labels"
	env.cfg.Ingest.ChunkDedup = "off"
	overlay := filepath.Join(t.TempDir(), "stop.txt")
	// The overlay is checked after normalization, so use the normalized name.
	if err := os.WriteFile(overlay, []byte("Quillon\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env.cfg.Ingest.StoplistFile = overlay
	env.p = ingest.NewPipeline(env.cfg, env.st, hygieneProvider{}, nil, textLoader)
	env.p.SetWriteMode(ingest.WriteMode{Hygiene: true})
	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()), Text: "l'Acme uses Quillon Ltd."})
	if n := rawCount(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (:Document {id:'%s'})-[:HAS_CHUNK]->(:Chunk)-[:MENTIONS]->(e:Entity) RETURN count(e) AS n", res.DocumentID)); n != 1 {
		t.Errorf("mentions=%d, want 1", n)
	}
}

func TestPipelineHygieneEmptyKey(t *testing.T) {
	env := newPipelineEnv(t)
	env.cfg.Labels.PresetsDir = t.TempDir()
	env.cfg.Ingest.ChunkDedup = "off"
	if err := os.WriteFile(filepath.Join(env.cfg.Labels.PresetsDir, "custom.yaml"), []byte("entities:\n  - name: organization\nallowlist: [\"'s\"]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env.p = ingest.NewPipeline(env.cfg, env.st, hygieneProvider{}, nil, textLoader)
	env.p.SetWriteMode(ingest.WriteMode{Hygiene: true})
	metrics := obs.New()
	env.p.SetMetrics(metrics)
	res := mustProcess(t, env, ingest.Input{Kind: "text", Source: fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()), Text: "'s uses l'Acme.", Labels: "custom"})
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf("MATCH (d:Document {id:'%s'}) RETURN d.hygiene AS hygiene", res.DocumentID))
	stored, _ := rows[0].Get("hygiene")
	counts, ok := stored.(map[string]any)
	if !ok || counts["empty_key"] != int64(1) {
		t.Errorf("Document.hygiene=%#v, want empty_key=1", stored)
	}
	if scrape := gather(t, metrics); !strings.Contains(scrape, `gkg_hygiene_dropped_total{rule="empty_key"} 1`) {
		t.Errorf("empty_key metric missing:\n%s", scrape)
	}
}
