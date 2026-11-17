package graph_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestDomainKeys(t *testing.T) {
	for name, want := range map[string]string{"@Acme": "acme", "ops@zorbo.example": "ops@zorbo.example"} {
		if got := graph.EntityKey(name); got != want {
			t.Errorf("EntityKey(%q) = %q, want %q", name, got, want)
		}
	}
	if got := graph.DomainKey("a", "acme"); got != "@a:acme" {
		t.Errorf("DomainKey = %q", got)
	}
	for _, slug := range []string{"a", "a-b", "0", "-", strings.Repeat("a", 40)} {
		if !graph.ValidDomain(slug) {
			t.Errorf("ValidDomain(%q) = false", slug)
		}
	}
	for _, slug := range []string{"", "A", "a_b", "a:b", " a", "à", strings.Repeat("a", 41)} {
		if graph.ValidDomain(slug) {
			t.Errorf("ValidDomain(%q) = true", slug)
		}
	}
}

func TestUpsertDocumentRejectsInvalidDomain(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	for _, path := range []string{"new", "changed", "noop", "force"} {
		t.Run(path, func(t *testing.T) {
			ids := make(map[string]bool)
			t.Cleanup(func() {
				for id := range ids {
					if err := st.DeleteDocument(ctx, id); err != nil && !errors.Is(err, graph.ErrNotFound) {
						t.Error(err)
					}
				}
			})
			in := graph.DocumentInput{Kind: "text", Source: t.Name(), Hash: "original", Domain: "a:b", Title: "Quillon Ltd"}
			if path != "new" {
				original := in
				original.Domain = "a"
				res, err := st.UpsertDocument(ctx, original)
				if err != nil {
					t.Fatal(err)
				}
				ids[res.ID] = true
			}
			if path == "changed" {
				in.Hash = "changed"
			}
			in.Force = path == "force"
			in.Title = "Zorbo"
			id := graph.DocumentID(in.Kind, in.Source, in.Hash)
			ids[id] = true
			query := fmt.Sprintf(`MATCH (d:Document {source: '%s'}) RETURN properties(d) AS props ORDER BY d.id`, in.Source)
			before := rawRecords(t, uri, query)
			if _, err := st.UpsertDocument(ctx, in); err == nil || !strings.Contains(err.Error(), "a:b") {
				t.Errorf("UpsertDocument error = %v, want invalid domain naming a:b", err)
			}
			if after := rawRecords(t, uri, query); !reflect.DeepEqual(before, after) {
				t.Error("invalid domain changed stored documents")
			}
		})
	}
}

func TestUpsertExtractionRejectsInvalidDomain(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	for _, candidates := range []bool{false, true} {
		t.Run(fmt.Sprintf("candidates=%t", candidates), func(t *testing.T) {
			doc, cid := entitiesTestDoc(t, st, "Acme-Max in Zorbo.")
			t.Cleanup(func() {
				if err := st.DeleteDocument(ctx, doc); err != nil {
					t.Error(err)
				}
			})
			ents := []graph.EntityUpsert{
				{Key: "acme-" + doc, Name: "Acme-Max", Mentions: []graph.MentionSpan{{Start: 0, End: 8}}},
				{Key: "zorbo-" + doc, Name: "Zorbo", Mentions: []graph.MentionSpan{{Start: 12, End: 17}}},
			}
			facts := []graph.FactUpsert{{HeadKey: ents[0].Key, TailKey: ents[1].Key, Rel: "located_in", Confidence: 0.9,
				HeadMention: graph.Span{Start: 0, End: 8}, TailMention: graph.Span{Start: 12, End: 17}, JudgeInput: []byte("Acme-Max in Zorbo")}}
			opt := graph.WriteOptions{Domain: "a", FactsAsCandidates: candidates}
			if err := st.UpsertExtractionOpts(ctx, doc, cid, "mock:1", ents, facts, opt); err != nil {
				t.Fatal(err)
			}
			queries := []string{
				fmt.Sprintf(`MATCH (c:Chunk {id: '%s'}) OPTIONAL MATCH (c)-[m:MENTIONS]->(e:Entity) RETURN properties(c) AS chunk, properties(m) AS mention, properties(e) AS entity ORDER BY e.key`, cid),
				fmt.Sprintf(`MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id = '%s' RETURN type(r) AS rel, properties(r) AS props ORDER BY r.head_key, r.tail_key`, cid),
				fmt.Sprintf(`MATCH (f:FactCandidate {chunk_id: '%s'}) RETURN properties(f) AS props ORDER BY f.id`, cid),
			}
			before := make([]any, len(queries))
			for i, query := range queries {
				before[i] = rawRecords(t, uri, query)
			}
			opt.Domain = "a:b"
			ents[0].Name = "Quillon Ltd"
			if err := st.UpsertExtractionOpts(ctx, doc, cid, "mock:2", ents, facts, opt); err == nil || !strings.Contains(err.Error(), "a:b") {
				t.Errorf("UpsertExtractionOpts error = %v, want invalid domain naming a:b", err)
			}
			for i, query := range queries {
				if after := rawRecords(t, uri, query); !reflect.DeepEqual(before[i], after) {
					t.Errorf("invalid domain changed stored extraction: %s", query)
				}
			}
		})
	}
}

func TestNameKeyWritten(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	for _, slug := range []string{"", "a"} {
		t.Run("domain="+slug, func(t *testing.T) {
			doc, cid := entitiesTestDoc(t, st, "Acme-Max and Quillon Ltd.")
			t.Cleanup(func() {
				if err := st.DeleteDocument(ctx, doc); err != nil {
					t.Error(err)
				}
			})
			key := "acme-max-" + doc
			if slug != "" {
				key = "@a:" + key
			}
			ents := []graph.EntityUpsert{
				{Key: key, Name: "Acme-Max", Type: "organization", Mentions: []graph.MentionSpan{{Start: 0, End: 8}}},
				{Key: "#" + doc + ":quillon ltd", Name: "Quillon Ltd", Type: "organization", Mentions: []graph.MentionSpan{{Start: 13, End: 24}}},
			}
			for _, name := range []string{"Acme-Max", "ACME-MAX"} {
				ents[0].Name = name
				if err := st.UpsertExtractionOpts(ctx, doc, cid, "mock:1", ents, nil, graph.WriteOptions{Domain: slug}); err != nil {
					t.Fatal(err)
				}
				rows := rawRecords(t, uri, fmt.Sprintf(`MATCH (:Chunk {id: '%s'})-[:MENTIONS]->(e:Entity) RETURN e.key AS key, e.name_key AS name_key, e.domain AS domain, e.domain_tok AS tok ORDER BY e.key`, cid))
				if len(rows) != 2 {
					t.Fatalf("entities = %d, want 2", len(rows))
				}
				for _, r := range rows {
					key, _ := r.Get("key")
					want := "acme-max"
					if strings.HasPrefix(key.(string), "#") {
						want = "quillon ltd"
					}
					if got, _ := r.Get("name_key"); got != want {
						t.Errorf("name_key = %v, want %s", got, want)
					}
					if slug != "" {
						if got, _ := r.Get("domain"); got != slug {
							t.Errorf("domain = %v, want %s", got, slug)
						}
						if got, _ := r.Get("tok"); got != domain.DomainToken(slug) {
							t.Errorf("domain_tok = %v", got)
						}
					}
				}
			}
		})
	}
}

func TestRechunkKeepsDomain(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	res, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: t.Name(), Hash: "domain-chunks", Domain: "a-b"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, res.ID); err != nil {
			t.Error(err)
		}
	})
	for pass := 0; pass < 2; pass++ {
		if pass == 1 {
			if err := st.DeleteChunksFrom(ctx, res.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.PutChunks(ctx, res.ID, []graph.Chunk{{Seq: 0, Text: "Acme-Max one", Domain: "wrong"}, {Seq: 1, Text: "Quillon two"}}); err != nil {
			t.Fatal(err)
		}
		center, around, err := st.ChunkNeighbourhood(ctx, res.ID+"-c0", 1)
		if err != nil || center.Domain != "a-b" || len(around) != 1 || around[0].Domain != "a-b" {
			t.Errorf("ChunkNeighbourhood = %+v, %+v, %v", center, around, err)
		}
		chunks, err := st.Chunks(ctx, res.ID)
		if err != nil || len(chunks) != 2 {
			t.Fatalf("chunks = %+v, %v", chunks, err)
		}
		for _, c := range chunks {
			if c.Domain != "a-b" {
				t.Errorf("chunk domain = %q", c.Domain)
			}
			got, err := st.ChunkByID(ctx, c.ID)
			if err != nil || got.Domain != "a-b" {
				t.Errorf("ChunkByID = %+v, %v", got, err)
			}
		}
		rows := rawRecords(t, uri, fmt.Sprintf(`MATCH (d:Document {id:'%s'})-[:HAS_CHUNK]->(c:Chunk) RETURN d.domain_tok AS dt, c.domain_tok AS ct`, res.ID))
		for _, r := range rows {
			for _, key := range []string{"dt", "ct"} {
				if got, _ := r.Get(key); got != domain.DomainToken("a-b") {
					t.Errorf("%s = %v", key, got)
				}
			}
		}
	}
	inFlight, err := st.InFlight(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range inFlight {
		if d.ID == res.ID && d.Domain != "a-b" {
			t.Errorf("InFlight domain=%q", d.Domain)
		}
	}
	doc, err := st.Document(ctx, res.ID)
	if err != nil || doc.Domain != "a-b" {
		t.Fatalf("Document = %+v, %v", doc, err)
	}
	for _, status := range []graph.Status{graph.StatusReceived, graph.StatusDone} {
		if err := st.SetStatus(ctx, res.ID, status, ""); err != nil {
			t.Fatal(err)
		}
		docs, err := st.DocumentsWithStatus(ctx, status, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range docs {
			if d.ID == res.ID && d.Domain != "a-b" {
				t.Errorf("DocumentsWithStatus domain = %q", d.Domain)
			}
		}
	}
	docs, err := st.Documents(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if d.ID == res.ID && d.Domain != "a-b" {
			t.Errorf("Documents domain = %q", d.Domain)
		}
	}
	// Same-content no-op and Force paths leave an existing domain untouched,
	// even when the input supplies a different valid domain.
	for _, force := range []bool{false, true} {
		if _, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: t.Name(), Hash: "domain-chunks", Domain: "b", Force: force}); err != nil {
			t.Fatal(err)
		}
		doc, err = st.Document(ctx, res.ID)
		if err != nil || doc.Domain != "a-b" {
			t.Errorf("same-content upsert (Force=%t) = %+v, %v", force, doc, err)
		}
	}
}

func TestDomainTokensIndexed(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	doc, cid := entitiesTestDoc(t, st, "Quillon makes tools.")
	t.Cleanup(func() {
		if err := st.DeleteDocument(ctx, doc); err != nil {
			t.Error(err)
		}
	})
	key := "quillon-" + doc
	for _, slug := range []string{"zorbo-trips", "a-b", strings.Repeat("x", 40)} {
		// Exercise indexed SET updates on an existing chunk and entity.
		rawRun(t, uri, fmt.Sprintf(`MATCH (d:Document {id:'%s'}) SET d.domain='%s',d.domain_tok='%s'`, doc, slug, domain.DomainToken(slug)))
		if err := st.PutChunks(ctx, doc, []graph.Chunk{{Text: "Quillon makes tools."}}); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertExtractionOpts(ctx, doc, cid, "mock:1", []graph.EntityUpsert{{Key: key, Name: "Quillon", Mentions: []graph.MentionSpan{{Start: 0, End: 7}}}}, nil, graph.WriteOptions{Domain: slug}); err != nil {
			t.Fatal(err)
		}
		for _, ix := range []struct{ name, field, id string }{{"chunk_text_v2", "text", cid}, {"entity_text_v2", "name", graph.EntityID(key)}} {
			q := fmt.Sprintf(`CALL text_search.search('%s','data.domain_tok:"%s" AND (data.%s:"quillon")') YIELD node RETURN node.id AS id`, ix.name, domain.DomainToken(slug), ix.field)
			rows := rawRecords(t, uri, q)
			found := false
			for _, r := range rows {
				if id, _ := r.Get("id"); id == ix.id {
					found = true
				}
			}
			if !found {
				t.Errorf("scoped index %s missing fixture for slug %q", ix.name, slug)
			}
			rows = rawRecords(t, uri, fmt.Sprintf(`CALL text_search.search_all('%s','"trips"') YIELD node RETURN node.id AS id`, ix.name))
			for _, r := range rows {
				if id, _ := r.Get("id"); id == ix.id {
					t.Errorf("slug leaked into unscoped index %s", ix.name)
				}
			}
		}
	}
}

// TestChangedContentTakesNewDomain pins the changed-content upsert path: the
// new version carries the supplied domain and token, and so do its chunks.
func TestChangedContentTakesNewDomain(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	src := fmt.Sprintf("text/%s-%d", t.Name(), time.Now().UnixNano())
	first, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: src, Hash: "original", Domain: "zorbo-a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.UpsertDocument(ctx, graph.DocumentInput{Kind: "text", Source: src, Hash: "changed", Domain: "zorbo-b"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, id := range []string{first.ID, second.ID} {
			if err := st.DeleteDocument(ctx, id); err != nil && !errors.Is(err, graph.ErrNotFound) {
				t.Error(err)
			}
		}
	})
	if err := st.PutChunks(ctx, second.ID, []graph.Chunk{{Text: "Quillon makes tools."}}); err != nil {
		t.Fatal(err)
	}
	doc, err := st.Document(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Domain != "zorbo-b" {
		t.Fatalf("new version domain = %q, want zorbo-b", doc.Domain)
	}
	uri := os.Getenv(graphtest.URIEnv)
	rows := rawRecords(t, uri, fmt.Sprintf(`MATCH (d:Document {id:'%s'})-[:HAS_CHUNK]->(c:Chunk) RETURN d.domain_tok AS dtok, c.domain AS cdom, c.domain_tok AS ctok`, second.ID))
	want := domain.DomainToken("zorbo-b")
	if len(rows) != 1 || testString(rows[0], "dtok") != want || testString(rows[0], "cdom") != "zorbo-b" || testString(rows[0], "ctok") != want {
		t.Fatalf("version/chunk domain = %+v, want zorbo-b / %s", rows, want)
	}
}
