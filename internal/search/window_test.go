package search_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/dedup"
	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/search"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func windowTestWriter(t testing.TB) func(string, map[string]any) error {
	t.Helper()
	driver, err := neo4j.NewDriver(os.Getenv(graphtest.URIEnv), neo4j.NoAuth())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Close(context.Background()) })
	return func(query string, params map[string]any) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		sess := driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeWrite})
		defer sess.Close(ctx)
		res, err := sess.Run(ctx, query, params)
		if err != nil {
			return err
		}
		_, err = res.Consume(ctx)
		return err
	}
}

func searchWindow(t testing.TB, since, until string) graph.DateWindow {
	t.Helper()
	w, err := graph.ParseDateWindow(since, until)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestSearchDateWindowIdenticalPassageBothIngestOrders(t *testing.T) {
	st := graphtest.Store(t)
	write := windowTestWriter(t)
	ctx := t.Context()
	const oldDate, newDate = "2024-03-01", "2026-03-01"
	old := searchWindow(t, "", "2024-12-31")
	recent := searchWindow(t, "2025-01-01", "")
	token := uniqueToken(t)
	text := "The " + token + " catalogue passage appears verbatim in both editions."
	sources := [2]string{token + "-a", token + "-b"}
	var ids [2]string
	for _, order := range [][2]int{{0, 1}, {1, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			cfg, err := config.Load("")
			if err != nil {
				t.Fatal(err)
			}
			cfg.Ingest.SpoolDir = t.TempDir()
			cfg.Ingest.ChunkDedup = "exact"
			cfg.Ingest.ChunkWords = 100
			provider := &extract.Mock{}
			p := ingest.NewPipeline(cfg, st, provider, dedup.New(), func(_ context.Context, in ingest.Input) (ingest.Loaded, error) {
				return ingest.Loaded{Text: in.Text}, nil
			})
			var inputs [2]ingest.Input
			for i, at := range order {
				date := []string{oldDate, newDate}[i]
				raw, err := graph.ParseDocumentDate(date, time.Now)
				if err != nil {
					t.Fatal(err)
				}
				inputs[at] = ingest.Input{Kind: "text", Source: sources[at], Text: text, DocumentDate: raw}
				res, err := p.Submit(ctx, inputs[at])
				if err != nil || res.Noop {
					t.Fatalf("submit = %+v, %v", res, err)
				}
				doc := res.DocumentID
				t.Cleanup(func() {
					if err := st.DeleteDocument(context.Background(), doc); err != nil {
						t.Error(err)
					}
				})
				if ids[at] != "" && ids[at] != doc {
					t.Fatalf("source %s changed id from %s to %s", sources[at], ids[at], doc)
				}
				ids[at] = doc
				if err := write(`
MATCH (d:Document {id:$id}) SET d.domain = $domain, d.domain_tok = $token`,
					map[string]any{"id": doc, "domain": token, "token": domain.DomainToken(token)}); err != nil {
					t.Fatal(err)
				}
				if err := p.Process(ctx, doc); err != nil {
					t.Fatal(err)
				}
			}
			chunks, err := st.Chunks(ctx, ids[order[1]])
			if err != nil || len(chunks) != 1 || chunks[0].DuplicateOf != ids[order[0]]+"-c0" || provider.Calls() != 1 {
				t.Fatalf("second document did not retain its duplicate chunk: %+v, %v, extract calls = %d", chunks, err, provider.Calls())
			}
			query := func(opts ...search.Option) []search.Hit {
				t.Helper()
				hits, err := search.Query(ctx, st, nil, token, "chunk", 10, append([]search.Option{search.InDomain(token)}, opts...)...)
				if err != nil {
					t.Fatal(err)
				}
				return hits
			}
			baseline := query()
			if len(baseline) != 2 || baseline[0].DocID == baseline[1].DocID ||
				!slices.Contains(ids[:], baseline[0].DocID) || !slices.Contains(ids[:], baseline[1].DocID) {
				t.Fatalf("unwindowed search = %+v, want both documents", baseline)
			}
			check := func(w graph.DateWindow, id, date string) {
				t.Helper()
				hits := query(search.InDateWindow(w))
				if len(hits) != 1 || hits[0].DocID != id || hits[0].DocumentDate == nil ||
					hits[0].DocumentDate.Format(time.DateOnly) != date {
					t.Fatalf("windowed hits = %+v, want one from %s on %s", hits, id, date)
				}
			}
			check(old, ids[order[0]], oldDate)
			check(recent, ids[order[1]], newDate)

			inputs[0].DocumentDate, inputs[1].DocumentDate = inputs[1].DocumentDate, inputs[0].DocumentDate
			for i, input := range inputs {
				res, err := p.Submit(ctx, input)
				if err != nil || !res.Noop || res.DocumentID != ids[i] {
					t.Fatalf("redating submit = %+v, %v, want no-op on %s", res, err, ids[i])
				}
			}
			check(old, ids[order[1]], oldDate)
			check(recent, ids[order[0]], newDate)
			after := query()
			if len(after) != len(baseline) {
				t.Fatalf("unwindowed search after redating = %+v", after)
			}
			for i, h := range after {
				if h.ID != baseline[i].ID || h.Score != baseline[i].Score {
					t.Fatalf("redating changed unwindowed ranking from %+v to %+v", baseline, after)
				}
			}
		})
	}
}

func TestSearchDateWindowFiltersBeforeLimit(t *testing.T) {
	st := graphtest.Store(t)
	token := uniqueToken(t)
	secondTerm := token + "edition"
	textQuery := token + " " + secondTerm
	var ids []string
	for i, input := range []struct{ date, text string }{
		{"2024-03-01T00:00:00.000000000Z", textQuery},
		{"2026-03-01T00:00:00.000000000Z", token + strings.Repeat(" background", 100)},
	} {
		res, err := st.UpsertDocument(t.Context(), graph.DocumentInput{
			Kind: "text", Source: fmt.Sprintf("%s-%d", token, i), Hash: token, Domain: token, DocumentDate: input.date,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.ID)
		t.Cleanup(func() {
			if err := st.DeleteDocument(context.Background(), res.ID); err != nil {
				t.Error(err)
			}
		})
		if err := st.PutChunks(t.Context(), res.ID, []graph.Chunk{{Seq: 0, Text: input.text}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, slug := range []string{"", token} {
		query := func(opts ...search.Option) []search.Hit {
			t.Helper()
			hits, err := search.Query(t.Context(), st, nil, textQuery, "chunk", 1, append([]search.Option{search.InDomain(slug)}, opts...)...)
			if err != nil {
				t.Fatal(err)
			}
			return hits
		}
		hits := query()
		if len(hits) != 1 || !slices.Contains(ids, hits[0].DocID) {
			t.Fatalf("unwindowed search in %q = %+v, want one fixture hit", slug, hits)
		}
		// The scoped query adds a domain term and ranking depends on the
		// index's corpus. Exclude the observed winner for each query.
		want, w := ids[1], searchWindow(t, "2025-01-01", "")
		if hits[0].DocID == ids[1] {
			want, w = ids[0], searchWindow(t, "", "2024-12-31")
		}
		if hits := query(search.InDateWindow(w)); len(hits) != 1 || hits[0].DocID != want {
			t.Fatalf("windowed search in %q = %+v, want the lower-ranked doc %s", slug, hits, want)
		}
	}
}

func TestSearchDateWindowLeavesEntityLegsUnfiltered(t *testing.T) {
	fx := newSearchFixture(t)
	t.Cleanup(func() {
		if err := fx.st.DeleteDocument(context.Background(), fx.docID); err != nil {
			t.Error(err)
		}
	})
	empty := searchWindow(t, "2099-01-01", "2099-01-01")
	for _, kind := range []string{"entity", "auto"} {
		hits, err := search.Query(t.Context(), fx.st, nil, fx.token, kind, 10, search.InDateWindow(empty))
		if err != nil || len(hits) != 1 || hits[0].ID != fx.entityID || hits[0].Kind != "entity" {
			t.Fatalf("%s search = %+v, %v, want only the entity hit", kind, hits, err)
		}
	}
}
