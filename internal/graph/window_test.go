package graph_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func parseWindow(t testing.TB, since, until string) graph.DateWindow {
	t.Helper()
	w, err := graph.ParseDateWindow(since, until)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestParseDateWindow(t *testing.T) {
	for _, tc := range []struct {
		name, since, until string
		invalid            bool
	}{
		{name: "omitted"},
		{name: "same_day", since: "2024-03-01", until: "2024-03-01"},
		{name: "offset_crosses_midnight", since: "2024-03-02T00:00:00+01:00", until: "2024-03-01T23:00:00Z"},
		{name: "inverted", since: "2024-03-01T23:00:00.000000001Z", until: "2024-03-02T00:00:00+01:00", invalid: true},
		{name: "future", since: "2099-03-01", until: "2099-03-01"},
		{name: "before_1900_utc", since: "1900-01-01T00:00:00+01:00", invalid: true},
		{name: "malformed_since", since: "2024-02-30", invalid: true},
		{name: "missing_offset_until", until: "2024-03-01T11:30:00", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := graph.ParseDateWindow(tc.since, tc.until)
			var invalid argerr.Error
			if tc.invalid && !errors.As(err, &invalid) || !tc.invalid && err != nil {
				t.Fatalf("ParseDateWindow(%q, %q) = %v, want argerr: %t", tc.since, tc.until, err, tc.invalid)
			}
		})
	}
}

func TestDocumentWindowEffectiveDateAndInclusiveBounds(t *testing.T) {
	st := graphtest.Store(t)
	scope := fmt.Sprintf("winlist%d", time.Now().UnixNano())
	uri := os.Getenv(graphtest.URIEnv)
	seed := func(suffix, raw, version, created, updated string) string {
		id := scope + suffix
		rawRun(t, uri, fmt.Sprintf(`CREATE (:Document {id:%q, domain:%q, status:%q,
			document_date:%q, version_at:%q, created_at:%q, updated_at:%q})`,
			id, scope, graph.StatusDone, raw, version, created, updated))
		rawRun(t, uri, fmt.Sprintf(`MATCH (d:Document {id:%q}) FOREACH (p IN CASE WHEN d.document_date = '' THEN [1] ELSE [] END | REMOVE d.document_date)
			FOREACH (p IN CASE WHEN d.version_at = '' THEN [1] ELSE [] END | REMOVE d.version_at)`, id))
		return id
	}
	t.Cleanup(func() {
		rawRun(t, uri, fmt.Sprintf("MATCH (d:Document) WHERE d.id STARTS WITH %q DETACH DELETE d", scope))
	})
	const (
		before = "2024-02-29T23:59:59.999999999Z"
		start  = "2024-03-01T00:00:00.000000000Z"
		middle = "2024-03-01T23:00:00.123456789Z"
		end    = "2024-03-01T23:59:59.999999999Z"
		after  = "2024-03-02T00:00:00.000000000Z"
	)
	first := seed("-start", start, before, before, after)
	last := seed("-end", "", end, before, end)
	legacy := seed("-legacy", "", "", middle, before)
	seed("-before", before, start, start, after)
	seed("-after", "", after, start, after)

	w := parseWindow(t, "2024-03-01", "2024-03-01")
	list := func(window graph.DateWindow, limit int, want ...string) {
		t.Helper()
		rows, err := st.DocumentsInWindow(t.Context(), graph.StatusDone, scope, limit, window)
		var ids []string
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		if err != nil || !slices.Equal(ids, want) {
			t.Fatalf("DocumentsInWindow = %v, %v, want %v", ids, err, want)
		}
	}
	list(w, 0, first, last, legacy)
	list(w, 1, first)
	list(parseWindow(t, "2024-03-02T00:00:00.123456789+01:00", middle), 0, legacy)
	list(parseWindow(t, end, end), 0, last)
}

func TestChunkTextSearchWindowMissingProvenance(t *testing.T) {
	st := graphtest.Store(t)
	uri := os.Getenv(graphtest.URIEnv)
	token := fmt.Sprintf("winchunk%d", time.Now().UnixNano())
	doc := token + "-doc"
	dated, noDocID, orphan := token+"-dated", token+"-no-doc-id", token+"-orphan"
	rawRun(t, uri, fmt.Sprintf(`
CREATE (d:Document {id:%q,document_date:'2024-03-01T00:00:00.000000000Z'}),
       (d)-[:HAS_CHUNK]->(:Chunk {id:%q,doc_id:d.id,text:%q}),
       (:Chunk {id:%q,text:%q}),
       (:Chunk {id:%q,doc_id:%q,text:%q})`, doc, dated, token, noDocID, token, orphan, token+"-missing", token))
	t.Cleanup(func() {
		rawRun(t, uri, fmt.Sprintf(`MATCH (n) WHERE n.id IN [%q, %q, %q, %q] DETACH DELETE n`, doc, dated, noDocID, orphan))
	})

	check := func(window graph.DateWindow, want ...string) {
		t.Helper()
		rows, err := st.ChunkTextSearchInWindow(t.Context(), token, "", 10, window)
		var ids []string
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		slices.Sort(ids)
		if err != nil || !slices.Equal(ids, want) {
			t.Fatalf("ChunkTextSearchInWindow = %v, %v, want %v", ids, err, want)
		}
	}
	check(graph.DateWindow{}, dated, noDocID, orphan)
	check(parseWindow(t, "2024-03-01", "2024-03-01"), dated)
}

func seedWindowEvidence(t *testing.T, st *graph.Store, date string, status graph.Status) string {
	t.Helper()
	var raw string
	if date != "" {
		var err error
		raw, err = graph.ParseDocumentDate(date, time.Now)
		if err != nil {
			t.Fatal(err)
		}
	}
	doc := dateUpsert(t, st, graph.DocumentInput{Kind: "text", Source: uniqueSource(t), Hash: "window", DocumentDate: raw}).ID
	t.Cleanup(func() {
		if err := st.DeleteDocument(context.Background(), doc); err != nil {
			t.Error(err)
		}
	})
	if err := st.SetStatus(t.Context(), doc, status, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.PutChunks(t.Context(), doc, []graph.Chunk{{Seq: 0, Text: doc + " typed"}, {Seq: 1, Text: doc + " candidate"}}); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestEntityViewDateWindowGroupsAndMissingDocID(t *testing.T) {
	st := graphtest.Store(t)
	recent := seedWindowEvidence(t, st, "2024-03-01", graph.StatusDone)
	old := seedWindowEvidence(t, st, "2024-02-01", graph.StatusDone)
	root, a, z := recent+"-root", recent+"-a", recent+"-z"
	ents := []graph.EntityUpsert{{Key: root, Name: "Root"}, {Key: a, Name: "A"}, {Key: z, Name: "Z"}}
	fact := func(head, tail, rel string) graph.FactUpsert {
		return graph.FactUpsert{HeadKey: head, TailKey: tail, Rel: rel, Confidence: .9, JudgeInput: []byte("evidence")}
	}
	for _, doc := range []string{old, recent} {
		if err := st.UpsertExtraction(t.Context(), doc, doc+"-c0", "mock:1", ents,
			[]graph.FactUpsert{fact(root, z, "z_link")}); err != nil {
			t.Fatal(err)
		}
		candidates := []graph.FactUpsert{fact(z, root, "z_link")}
		if doc == recent {
			candidates = append(candidates, fact(root, a, "a_link"), fact(a, root, "a_link"))
		} else {
			candidates = append(candidates, fact(root, z, "legacy_link"))
		}
		if err := st.UpsertExtractionOpts(t.Context(), doc, doc+"-c1", "mock:1", ents, candidates,
			graph.WriteOptions{FactsAsCandidates: true}); err != nil {
			t.Fatal(err)
		}
	}
	uri := os.Getenv(graphtest.URIEnv)
	// Evidence eligibility is independent of its document's domain and status.
	rawRun(t, uri, fmt.Sprintf(`
MATCH (d:Document {id:%q}) SET d.domain = 'other-window-domain', d.status = 'superseded'`, recent))
	rawRun(t, uri, fmt.Sprintf(`
MATCH (a:Entity {key:%q}), (b:Entity {key:%q})
CREATE (a)-[:z_link {chunk_id:'no-doc',head_key:a.key,tail_key:b.key}]->(b),
       (a)-[:z_link {chunk_id:'orphan-doc',doc_id:%q,head_key:a.key,tail_key:b.key}]->(b)`, root, z, recent+"-missing"))
	t.Cleanup(func() {
		rawRun(t, uri, fmt.Sprintf(`MATCH (:Entity {key:%q})-[r]->() WHERE r.chunk_id IN ['no-doc', 'orphan-doc'] DELETE r`, root))
	})

	opt := graph.ViewOptions{IncludeUnverified: true}
	all, err := st.EntityViewOpts(t.Context(), root, opt)
	if err != nil || len(all.Facts) != 9 || all.Facts[0].Other.Key != z {
		t.Fatalf("unwindowed view = %+v, %v, want nine facts with Z first", all, err)
	}
	if !slices.ContainsFunc(all.Facts, func(f graph.FactView) bool { return f.ChunkID == "no-doc" && f.DocID == "" }) {
		t.Fatal("unwindowed view lost the fact without doc_id")
	}
	if !slices.ContainsFunc(all.Facts, func(f graph.FactView) bool { return f.ChunkID == "orphan-doc" && f.DocID == recent+"-missing" }) {
		t.Fatal("unwindowed view lost the fact whose Document is missing")
	}
	opt.DateWindow = parseWindow(t, "2024-03-01", "")
	var win graph.EntityView
	reads := graph.CaptureReadQueries(st, func() {
		win, err = st.EntityViewOpts(t.Context(), root, opt)
	})
	if err != nil || len(win.Facts) != 4 {
		t.Fatalf("windowed view = %+v, %v, want four facts", win, err)
	}
	// The groups now tie at two. A sorts first only if older evidence did
	// not contribute to the Z group count.
	for i, f := range win.Facts {
		want := a
		if i >= 2 {
			want = z
		}
		if f.DocID != recent || f.Other.Key != want {
			t.Fatalf("fact %d = %+v, want recent evidence for %s", i, f, want)
		}
	}
	if win.Hidden != all.Hidden || !slices.Equal(win.Together, all.Together) {
		t.Error("window changed the global hidden counts or co-mentions")
	}
	joins := 0
	for _, read := range reads {
		joins += strings.Count(read.Query, "MATCH (wd:Document {id: r.doc_id})")
	}
	if joins != 3 {
		t.Errorf("entity view has %d window joins, want one in the typed query and one in each candidate arm", joins)
	}
	opt.DateWindow = parseWindow(t, "2024-02-15", "2024-02-15")
	empty, err := st.EntityViewOpts(t.Context(), root, opt)
	if err != nil || len(empty.Facts) != 0 {
		t.Fatalf("empty window view = %+v, %v, want no facts", empty, err)
	}

	t.Run("traverse_missing_provenance", func(t *testing.T) {
		check := func(window graph.DateWindow, want ...string) {
			t.Helper()
			sub, err := st.TraverseOpts(t.Context(), graph.EntityID(root), "z_link", "out", 1, 10, graph.ViewOptions{DateWindow: window})
			var ids []string
			for _, edge := range sub.Edges {
				ids = append(ids, edge.ChunkID)
			}
			slices.Sort(ids)
			slices.Sort(want)
			if err != nil || !slices.Equal(ids, want) {
				t.Fatalf("TraverseOpts edge chunks = %v, %v, want %v", ids, err, want)
			}
		}
		check(graph.DateWindow{}, old+"-c0", recent+"-c0", "no-doc", "orphan-doc")
		check(parseWindow(t, "2024-03-01", ""), recent+"-c0")
	})
}

func TestTraverseDateWindowAcrossTopUps(t *testing.T) {
	st := graphtest.Store(t)
	first := seedWindowEvidence(t, st, "2024-03-01", graph.StatusDone)
	second := seedWindowEvidence(t, st, "2024-03-02", graph.StatusDone)
	old := seedWindowEvidence(t, st, "2024-02-01", graph.StatusDone)
	for _, candidate := range []bool{false, true} {
		for _, direction := range []string{"out", "in", "both"} {
			t.Run(fmt.Sprintf("candidate=%t/%s", candidate, direction), func(t *testing.T) {
				prefix := first + t.Name()
				var ents []graph.EntityUpsert
				entity := func(suffix string) string {
					key := prefix + "-" + suffix
					ents = append(ents, graph.EntityUpsert{Key: key, Name: suffix})
					return key
				}
				root, hub, spare := entity("root"), entity("hub"), entity("spare")
				fact := func(a, b string, score float64) graph.FactUpsert {
					if direction == "in" || direction == "both" && a == hub {
						a, b = b, a
					}
					return graph.FactUpsert{HeadKey: a, TailKey: b, Rel: "links", Confidence: score, JudgeInput: []byte("evidence")}
				}
				leaves := make([]graph.FactUpsert, 10)
				for i := range leaves {
					leaves[i] = fact(hub, entity(fmt.Sprintf("leaf%02d", i)), .95-float64(i)/100)
				}
				oldLeaf := fact(hub, entity("old-leaf"), 1)
				roots := []graph.FactUpsert{fact(root, hub, .95), fact(root, spare, .9)}
				// Each subtest uses separate chunks because upserting an extraction
				// replaces the preceding extraction for that chunk.
				for _, group := range []struct {
					doc   string
					facts []graph.FactUpsert
					cand  bool
				}{{first, roots, false}, {first, leaves[:5], candidate}, {second, leaves[5:], candidate}, {old, []graph.FactUpsert{oldLeaf}, candidate}} {
					chunk := prefix + group.doc + fmt.Sprint(len(group.facts))
					uri := os.Getenv(graphtest.URIEnv)
					rawRun(t, uri, fmt.Sprintf(`MATCH (d:Document {id:%q}) CREATE (d)-[:HAS_CHUNK]->(:Chunk {id:%q,doc_id:d.id,text:'window evidence'})`, group.doc, chunk))
					if err := st.UpsertExtractionOpts(t.Context(), group.doc, chunk, "mock:1", ents, group.facts,
						graph.WriteOptions{FactsAsCandidates: group.cand}); err != nil {
						t.Fatal(err)
					}
				}

				opt := graph.ViewOptions{IncludeUnverified: candidate, DateWindow: parseWindow(t, "2024-03-01", "2024-03-02")}
				var sub graph.Subgraph
				reads := graph.CaptureReadQueries(st, func() {
					var err error
					sub, err = st.TraverseOpts(t.Context(), graph.EntityID(root), "links", direction, 2, 12, opt)
					if err != nil {
						t.Fatal(err)
					}
				})
				if len(sub.Edges) != 12 {
					t.Fatalf("traverse returned %d edges, want 12", len(sub.Edges))
				}
				assertTraverseEdgeOrder(t, sub.Edges[:2], roots, false)
				assertTraverseEdgeOrder(t, sub.Edges[2:], leaves, candidate)
				for i, edge := range sub.Edges {
					want := first
					if i >= 7 {
						want = second
					}
					if edge.DocID != want {
						t.Fatalf("edge %d uses doc %q, want %q", i, edge.DocID, want)
					}
				}
				var topUps int
				for _, read := range reads {
					_, typed := read.Params["ids"]
					_, candidates := read.Params["anchors"]
					if !typed && !candidates {
						continue
					}
					if _, topUp := read.Params["filled"]; topUp {
						topUps++
					}
					if !strings.Contains(read.Query, "MATCH (wd:Document {id: r.doc_id})") ||
						read.Params["since"] != "2024-03-01T00:00:00.000000000Z" ||
						read.Params["until"] != "2024-03-02T23:59:59.999999999Z" {
						t.Fatalf("expansion did not apply the requested window: %s, %v", read.Query, read.Params)
					}
				}
				if topUps == 0 {
					t.Fatal("expected at least one top-up")
				}
			})
		}
	}
}
