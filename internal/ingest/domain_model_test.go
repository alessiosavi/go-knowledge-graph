package ingest_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

type domainProvider struct{}

func (domainProvider) Name() string    { return "domain-test" }
func (domainProvider) Version() string { return "domain-test:1" }
func (domainProvider) Extract(_ context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	var out []extract.ChunkResult
	for _, c := range req.Chunks {
		out = append(out, extract.ChunkResult{ChunkID: c.ID, Mentions: []extract.Mention{
			{Text: "Acme", Label: "organization", Start: 0, End: 4, Score: 0.9},
			{Text: "Zorbo", Label: "location", Start: 8, End: 13, Score: 0.9},
			{Text: "2026", Label: "date", Start: 18, End: 22, Score: 0.9},
		}, Candidates: []extract.Candidate{{Label: "located_in", Head: 0, Tail: 1, Score: 0.9}}})
	}
	return out, nil
}

func domainPipeline(t *testing.T, mode ingest.WriteMode) pipelineEnv {
	t.Helper()
	env := newPipelineEnv(t)
	env.cfg.Ingest.ChunkDedup = "off" // Domain-local dedup is Task 37.14b.
	env.cfg.Ingest.ChunkWords = 100
	env.cfg.Labels.PresetsDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(env.cfg.Labels.PresetsDir, "domain-test.yaml"), []byte("entities:\n  - name: organization\n  - name: location\n  - name: date\n    scope: document\nrelations:\n  - name: located_in\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.p = ingest.NewPipeline(env.cfg, env.st, domainProvider{}, nil, textLoader)
	env.p.SetWriteMode(mode)
	return env
}

func assignedDomainDoc(t *testing.T, env pipelineEnv, slug string) string {
	t.Helper()
	ctx := context.Background()
	res, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: t.Name() + "/" + slug, Text: "Acme in Zorbo for 2026.", Labels: "domain-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.st.DeleteDocument(ctx, res.DocumentID); err != nil {
			t.Error(err)
		}
	})
	rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (d:Document {id:'%s'}) SET d.domain='%s', d.domain_tok='%s' RETURN d.id`, res.DocumentID, slug, domain.DomainToken(slug)))
	if err := env.p.Process(ctx, res.DocumentID); err != nil {
		t.Fatal(err)
	}
	return res.DocumentID
}

func TestSameNamesTwoDomains(t *testing.T) {
	env := domainPipeline(t, ingest.WriteMode{Domains: true})
	uri := os.Getenv(graphtest.URIEnv)
	ids := map[string]bool{}
	for _, slug := range []string{"a", "b"} {
		doc := assignedDomainDoc(t, env, slug)
		rows := rawRecords(t, uri, fmt.Sprintf(`MATCH (c:Chunk {doc_id:'%s'})-[:MENTIONS]->(e:Entity) RETURN e.id AS id,e.key AS key,e.domain AS domain,e.domain_tok AS tok ORDER BY e.key`, doc))
		if len(rows) != 3 {
			t.Fatalf("domain %s: %d entities, want 3", slug, len(rows))
		}
		want := map[string]bool{"@" + slug + ":acme": true, "@" + slug + ":zorbo": true, "#" + doc + ":2026": true}
		for _, r := range rows {
			key := testString(r, "key")
			if !want[key] {
				t.Errorf("unexpected entity key %q", key)
			}
			delete(want, key)
			id := testString(r, "id")
			if ids[id] {
				t.Errorf("entity id %s shared across domains", id)
			}
			ids[id] = true
			if got := testString(r, "domain"); got != slug {
				t.Errorf("entity domain=%q, want %s", got, slug)
			}
			if got := testString(r, "tok"); got != domain.DomainToken(slug) {
				t.Errorf("entity token=%q", got)
			}
		}
		if len(want) != 0 {
			t.Errorf("missing keys: %v", want)
		}
		rows = rawRecords(t, uri, fmt.Sprintf(`MATCH (h:Entity)-[r:located_in]->(z:Entity) WHERE r.doc_id='%s' RETURN h.key AS head,z.key AS tail,h.domain AS hd,z.domain AS td`, doc))
		if len(rows) != 1 {
			t.Fatalf("domain %s: %d facts, want 1", slug, len(rows))
		}
		for key, want := range map[string]string{"head": "@" + slug + ":acme", "tail": "@" + slug + ":zorbo", "hd": slug, "td": slug} {
			if got := testString(rows[0], key); got != want {
				t.Errorf("%s=%q, want %q", key, got, want)
			}
		}
	}
	t.Log("2 domains: 6 distinct entity ids, 2 domain-local facts; document-scoped keys preserved")
}

func TestHygieneWithConfirmation(t *testing.T) {
	env := domainPipeline(t, ingest.WriteMode{Domains: true, RequireConfirmation: true})
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	doc := assignedDomainDoc(t, env, "a")
	rows := rawRecords(t, uri, fmt.Sprintf(`MATCH (c:FactCandidate {doc_id:'%s'}) RETURN c.id AS id,c.input_hash AS hash,c.judge_input AS input,c.head_key AS head,c.tail_key AS tail,c.domain AS domain,c.domain_tok AS tok`, doc))
	if len(rows) != 1 {
		t.Fatalf("candidates=%d, want 1", len(rows))
	}
	for key, want := range map[string]string{"head": "@a:acme", "tail": "@a:zorbo", "domain": "a", "tok": domain.DomainToken("a")} {
		if got := testString(rows[0], key); got != want {
			t.Errorf("%s=%q, want %q", key, got, want)
		}
	}
	input := testString(rows[0], "input")
	if want := fmt.Sprintf("%x", sha256.Sum256([]byte(input))); testString(rows[0], "hash") != want {
		t.Error("candidate hash differs from judge input")
	}
	if n := rawCount(t, uri, fmt.Sprintf(`MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id='%s' RETURN count(r) AS n`, doc)); n != 0 {
		t.Fatalf("candidate visible as %d edges", n)
	}
	ok, err := env.st.PromoteCandidate(ctx, testString(rows[0], "id"), testString(rows[0], "hash"), "")
	if err != nil || !ok {
		t.Fatalf("promote=%v,%v", ok, err)
	}
	rows = rawRecords(t, uri, fmt.Sprintf(`MATCH (h:Entity)-[r:located_in]->(z:Entity) WHERE r.doc_id='%s' RETURN h.key AS head,z.key AS tail,r.judge AS judge`, doc))
	if len(rows) != 1 || testString(rows[0], "head") != "@a:acme" || testString(rows[0], "tail") != "@a:zorbo" || testString(rows[0], "judge") != "confirmed" {
		t.Fatalf("promoted fact=%+v", rows)
	}
	if n := rawCount(t, uri, fmt.Sprintf(`MATCH (c:FactCandidate {doc_id:'%s'}) RETURN count(c) AS n`, doc)); n != 0 {
		t.Errorf("remaining candidates=%d", n)
	}
}

func TestDomainsDetectAssignment(t *testing.T) {
	env := domainPipeline(t, ingest.WriteMode{Domains: true})
	ctx := context.Background()
	res, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: t.Name(), Text: "Acme in Zorbo for 2026.", Labels: "domain-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.st.DeleteDocument(ctx, res.DocumentID); err != nil {
			t.Error(err)
		}
		uri := os.Getenv(graphtest.URIEnv)
		rawRecords(t, uri, fmt.Sprintf(`MATCH (r:DomainDecision {doc_id:'%s'}) DELETE r RETURN count(*)`, res.DocumentID))
		rawRecords(t, uri, fmt.Sprintf(`MATCH (n:Domain {id:'%s'}) DELETE n RETURN count(*)`, domain.NewDomainID(res.DocumentID)))
	})
	err = env.p.Process(ctx, res.DocumentID)
	if err != nil {
		t.Fatalf("Process=%v", err)
	}
	doc, err := env.st.Document(ctx, res.DocumentID)
	if err != nil || doc.Status != graph.StatusDone || doc.Domain == "" {
		t.Errorf("detected document=%+v,%v", doc, err)
	}
	if n := rawCount(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (c:Chunk {doc_id:'%s'})-[:MENTIONS]->(e:Entity) RETURN count(e) AS n`, res.DocumentID)); n != 3 {
		t.Errorf("detected domain wrote %d mentions, want 3", n)
	}
}

// elidedProvider emits an Italian elided article ("l'Acme") so hygiene has a
// name to normalize.
type elidedProvider struct{}

func (elidedProvider) Name() string    { return "domain-test" }
func (elidedProvider) Version() string { return "domain-test:1" }
func (elidedProvider) Extract(_ context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	var out []extract.ChunkResult
	for _, c := range req.Chunks {
		out = append(out, extract.ChunkResult{ChunkID: c.ID, Mentions: []extract.Mention{
			{Text: "l'Acme", Label: "organization", Start: 0, End: 6, Score: 0.9},
			{Text: "Zorbo", Label: "location", Start: 10, End: 15, Score: 0.9},
		}, Candidates: []extract.Candidate{{Label: "located_in", Head: 0, Tail: 1, Score: 0.9}}})
	}
	return out, nil
}

// TestHygieneDomainsConfirmation runs all three v4 write modes together:
// hygiene normalizes "l'Acme" before the domain prefix, the fact is a
// candidate whose judge input keeps the raw mention, and promotion connects
// the normalized domain keys.
func TestHygieneDomainsConfirmation(t *testing.T) {
	mode := ingest.WriteMode{Hygiene: true, Domains: true, RequireConfirmation: true}
	env := domainPipeline(t, mode)
	env.p = ingest.NewPipeline(env.cfg, env.st, elidedProvider{}, nil, textLoader)
	env.p.SetWriteMode(mode)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	res, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: t.Name() + "/a", Text: "l'Acme in Zorbo for 2026.", Labels: "domain-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.st.DeleteDocument(ctx, res.DocumentID); err != nil {
			t.Error(err)
		}
	})
	rawRecords(t, uri, fmt.Sprintf(`MATCH (d:Document {id:'%s'}) SET d.domain='a', d.domain_tok='%s' RETURN d.id`, res.DocumentID, domain.DomainToken("a")))
	if err := env.p.Process(ctx, res.DocumentID); err != nil {
		t.Fatal(err)
	}
	rows := rawRecords(t, uri, fmt.Sprintf(`MATCH (c:FactCandidate {doc_id:'%s'}) RETURN c.id AS id,c.input_hash AS hash,c.judge_input AS input,c.head_key AS head,c.tail_key AS tail`, res.DocumentID))
	if len(rows) != 1 {
		t.Fatalf("candidates=%d, want 1", len(rows))
	}
	if head, tail := testString(rows[0], "head"), testString(rows[0], "tail"); head != "@a:acme" || tail != "@a:zorbo" {
		t.Fatalf("candidate keys = %q -> %q, want @a:acme -> @a:zorbo", head, tail)
	}
	if input := testString(rows[0], "input"); !strings.Contains(input, "l'Acme") {
		t.Errorf("judge input %s lost the raw mention text", input)
	}
	ok, err := env.st.PromoteCandidate(ctx, testString(rows[0], "id"), testString(rows[0], "hash"), "")
	if err != nil || !ok {
		t.Fatalf("promote=%v,%v", ok, err)
	}
	edges := rawRecords(t, uri, fmt.Sprintf(`MATCH (h:Entity)-[r:located_in]->(z:Entity) WHERE r.doc_id='%s' RETURN h.key AS head,h.name AS name,z.key AS tail`, res.DocumentID))
	if len(edges) != 1 || testString(edges[0], "head") != "@a:acme" || testString(edges[0], "tail") != "@a:zorbo" || testString(edges[0], "name") != "Acme" {
		t.Fatalf("promoted fact=%+v", edges)
	}
}
