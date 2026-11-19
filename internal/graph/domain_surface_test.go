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

func domainSearchFixture(t *testing.T, st *graph.Store, slug, word string, count int) {
	t.Helper()
	uri := os.Getenv(graphtest.URIEnv)
	tag := fmt.Sprintf("ds%d", time.Now().UnixNano())
	rawRun(t, uri, fmt.Sprintf(`UNWIND range(1,%d) AS i CREATE (c:Chunk {id:'%s-c'+toString(i),text:'%s',domain:'%s',domain_tok:'%s'}) CREATE (e:Entity {id:'%s-e'+toString(i),key:'%s-e'+toString(i),name:'%s',name_key:'%s',domain:'%s',domain_tok:'%s'})`, count, tag, word, slug, domain.DomainToken(slug), tag, tag, word, graph.EntityKey(word), slug, domain.DomainToken(slug)))
	t.Cleanup(func() { rawRun(t, uri, fmt.Sprintf(`MATCH (n) WHERE n.id STARTS WITH '%s' DETACH DELETE n`, tag)) })
}

func TestDomainFilterBeyondCap(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	word := fmt.Sprintf("zorbo%d", time.Now().UnixNano())
	domainSearchFixture(t, st, "a", strings.Repeat(word+" ", 3), 1200)
	domainSearchFixture(t, st, "b", word, 2)
	chunks, err := st.ChunkTextSearchIn(ctx, word, "b", 5)
	if err != nil || len(chunks) != 2 {
		t.Fatalf("chunks = %d, %v; want 2", len(chunks), err)
	}
	ents, err := st.EntityTextSearchIn(ctx, word, "b", 5)
	if err != nil || len(ents) != 2 {
		t.Fatalf("entities = %d, %v; want 2", len(ents), err)
	}
	t.Logf("scoped hits below 1200 competing rows: chunks=%d entities=%d", len(chunks), len(ents))
}

func TestDomainTokenEdgeCases(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	word := fmt.Sprintf("quillon%d", time.Now().UnixNano())
	for _, slug := range []string{"b", "a-b", strings.Repeat("z", 40), "zorbotrips"} {
		domainSearchFixture(t, st, slug, word, 1)
	}
	for _, slug := range []string{"b", "a-b", strings.Repeat("z", 40)} {
		cs, err := st.ChunkTextSearchIn(ctx, word, slug, 5)
		if err != nil || len(cs) != 1 || cs[0].Domain != slug {
			t.Fatalf("%s chunks=%+v err=%v", slug, cs, err)
		}
		es, err := st.EntityTextSearchIn(ctx, word, slug, 5)
		if err != nil || len(es) != 1 || es[0].Domain != slug {
			t.Fatalf("%s entities=%+v err=%v", slug, es, err)
		}
	}
	for _, q := range []string{"zorbotrips", "a:b (and) OR not 'quillon'"} {
		cs, err := st.ChunkTextSearch(ctx, q, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			if c.Text == word {
				t.Fatalf("slug leaked into unscoped hits: %+v", c)
			}
		}
	}
	if _, err := st.ChunkTextSearchIn(ctx, word, "Bad:slug", 5); err == nil {
		t.Fatal("invalid slug accepted")
	}
}

func TestEntityByNameOldContract(t *testing.T) {
	st := graphtest.Store(t)
	ctx := context.Background()
	name := fmt.Sprintf("Acme-Max %d", time.Now().UnixNano())
	domainSearchFixture(t, st, "a", name, 1)
	for _, q := range []string{name, strings.ToUpper(name)} {
		if _, err := st.EntityByName(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEntityCandidatesByName(t *testing.T) {
	st := graphtest.MigrationStore(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	name := fmt.Sprintf("Acme%d", time.Now().UnixNano())
	// Several same-named entities in one domain (document-scoped keys) must
	// reduce to one candidate per domain.
	domainSearchFixture(t, st, "a", name, 5)
	domainSearchFixture(t, st, "b", name, 1)
	t.Cleanup(func() {
		rawRun(t, uri, fmt.Sprintf(`MATCH (m:GkgMeta {id:'extraction_schema'}) SET m.value=%d`, graph.ExtractionSchemaVersion))
	})
	rawRun(t, uri, `MERGE (m:GkgMeta {id:'extraction_schema'}) SET m.value=4`)
	for _, tc := range []struct {
		d string
		n int
	}{{"", 2}, {"a", 1}} {
		refs, err := st.EntityCandidatesByName(ctx, name+"'s", tc.d)
		if err != nil || len(refs) != tc.n {
			t.Fatalf("scope=%q refs=%+v err=%v", tc.d, refs, err)
		}
		for _, ref := range refs {
			if ref.Match != graph.EntityMatchExact {
				t.Errorf("scope=%q exact name match = %+v", tc.d, ref)
			}
		}
	}
	rows := rawRecords(t, uri, fmt.Sprintf(`EXPLAIN MATCH (e:Entity) WHERE e.name_key = '%s' RETURN e.id`, graph.EntityKey(name)))
	indexed := false
	for _, r := range rows {
		for _, v := range r.Values {
			line := fmt.Sprint(v)
			_, op, ok := strings.Cut(line, " * ")
			if ok && strings.HasPrefix(op, "ScanAllByLabelProperties") {
				indexed = true
			}
		}
	}
	if !indexed {
		t.Fatalf("missing indexed operator: %+v", rows)
	}
	old := rawRecords(t, uri, fmt.Sprintf(`EXPLAIN MATCH (e:Entity) WHERE toLower(trim(e.name)) = toLower(trim('%s')) RETURN e.id`, name))
	scanned := false
	for _, r := range old {
		for _, v := range r.Values {
			_, op, ok := strings.Cut(fmt.Sprint(v), " * ")
			if ok && (op == "ScanAll" || strings.HasPrefix(op, "ScanAll ")) {
				scanned = true
			}
		}
	}
	if !scanned {
		t.Fatalf("negative control lacks ScanAll: %+v", old)
	}
	rawRun(t, uri, fmt.Sprintf(`MATCH (e:Entity {name:'%s',domain:'a'}) WITH e ORDER BY e.id LIMIT 1 SET e.key='#zorbo:acme', e.mention_count=99`, name))
	refs, err := st.EntityCandidatesByName(ctx, name, "a")
	if err != nil || len(refs) != 1 || !strings.HasPrefix(refs[0].Key, "#") {
		t.Fatalf("document-scoped name=%+v %v", refs, err)
	}
	rawRun(t, uri, `MATCH (m:GkgMeta {id:'extraction_schema'}) SET m.value=3`)
	rawRun(t, uri, fmt.Sprintf(`MATCH (e:Entity {name:'%s'}) REMOVE e.name_key`, name))
	refs, err = st.EntityCandidatesByName(ctx, strings.ToUpper(name), "a")
	if err != nil || len(refs) != 1 || refs[0].Match != graph.EntityMatchExact {
		t.Fatalf("legacy=%+v %v", refs, err)
	}
	_, err = st.EntityViewOpts(ctx, refs[0].ID, graph.ViewOptions{Domain: "b"})
	if !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("out-of-scope view: %v", err)
	}
	_, err = st.TraverseOpts(ctx, refs[0].Key, "", "out", 1, 10, graph.ViewOptions{Domain: "b"})
	if !errors.Is(err, graph.ErrNotFound) {
		t.Fatalf("out-of-scope traverse: %v", err)
	}
}

func TestScopedViewsPreserveVisibility(t *testing.T) {
	f := graphtest.Visibility(t)
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	rawRun(t, uri, fmt.Sprintf(`MATCH (e:Entity {id:'%s'}) SET e.domain='a'`, f.ID))
	for _, low := range []bool{false, true} {
		for _, unverified := range []bool{false, true} {
			opt := graph.ViewOptions{IncludeLowConfidence: low, IncludeUnverified: unverified}
			full, err := f.Store.EntityViewOpts(ctx, f.ID, opt)
			if err != nil {
				t.Fatal(err)
			}
			sub, err := f.Store.TraverseOpts(ctx, f.ID, "", "both", 2, 50, opt)
			if err != nil {
				t.Fatal(err)
			}
			opt.Domain = "a"
			scoped, err := f.Store.EntityViewOpts(ctx, f.ID, opt)
			if err != nil {
				t.Fatal(err)
			}
			scopedSub, err := f.Store.TraverseOpts(ctx, f.ID, "", "both", 2, 50, opt)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(scoped, full) || !reflect.DeepEqual(scopedSub, sub) {
				t.Fatalf("scope changed visibility: low=%v unverified=%v", low, unverified)
			}
			if scoped.Hidden != (graph.HiddenCounts{Unclear: 1, Unjudged: 2, Rejected: 1}) {
				t.Fatalf("hidden=%+v", scoped.Hidden)
			}
		}
	}
}

// TestScopedSearchReturnsMoreThanTen pins that scoped text search is not a
// fixed top-10: 15 matching chunks in one domain all come back for limit 20.
func TestScopedSearchReturnsMoreThanTen(t *testing.T) {
	st := graphtest.Store(t)
	word := fmt.Sprintf("quillonx%d", time.Now().UnixNano())
	domainSearchFixture(t, st, "b", word, 15)
	chunks, err := st.ChunkTextSearchIn(context.Background(), word, "b", 20)
	if err != nil || len(chunks) != 15 {
		t.Fatalf("chunks = %d, %v; want 15", len(chunks), err)
	}
}
