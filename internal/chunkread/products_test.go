package chunkread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

type mentionTestStore struct {
	*fakeStore
	load func(context.Context) (*graph.ProductMentionIndex, error)
}

func (s mentionTestStore) LoadProductMentionIndex(ctx context.Context) (*graph.ProductMentionIndex, error) {
	return s.load(ctx)
}

type documentTestStore struct {
	mentionTestStore
	documents func([]string) (map[string]graph.DocumentProductLinks, error)
}

func (s documentTestStore) DocumentProductLinks(_ context.Context, ids []string) (map[string]graph.DocumentProductLinks, error) {
	return s.documents(ids)
}

func mentionObservation(id, code string) graph.ProductIdentifierObservation {
	return graph.ProductIdentifierObservation{
		Product: graph.Product{ID: id, Store: "products.test", ProductID: "products.test:" + id, Title: id},
		Feature: "catalogue_code", ValueFold: &code,
	}
}

func indexedChunks(chunks []graph.Chunk, observations []graph.ProductIdentifierObservation) mentionTestStore {
	index := graph.BuildProductMentionIndex(observations)
	return mentionTestStore{&fakeStore{chunks: chunks}, func(context.Context) (*graph.ProductMentionIndex, error) {
		return index, nil
	}}
}

func productStoresFixture() documentTestStore {
	inScope := graph.ProductIdentifierObservation{
		Product: graph.Product{ID: "in-scope", Store: "shop.example", ProductID: "shop.example:1", Title: "In scope"},
		Feature: "catalogue_code", ValueFold: new("AB12345"),
	}
	foreignCode := graph.ProductIdentifierObservation{
		Product: graph.Product{ID: "foreign-code", Store: "other.example", ProductID: "other.example:2", Title: "Foreign code"},
		Feature: "catalogue_code", ValueFold: new("CD12345"),
	}
	foreignRef := graph.Product{ID: "foreign-ref", Store: "other.example", ProductID: "other.example:1", Title: "Foreign ref"}
	return documentTestStore{
		mentionTestStore: indexedChunks([]graph.Chunk{
			{ID: "chunk", DocID: "doc", Text: "AB12345 CD12345"},
		}, []graph.ProductIdentifierObservation{inScope, foreignCode}),
		documents: func([]string) (map[string]graph.DocumentProductLinks, error) {
			return map[string]graph.DocumentProductLinks{"doc": {Status: graph.StatusDone, Refs: []graph.Product{foreignRef}}}, nil
		},
	}
}

func TestReadProductStoresEmptyDropsAll(t *testing.T) {
	out, err := Read(t.Context(), productStoresFixture(), Request{
		IDs: []string{"chunk"}, WithProducts: true, ProductStores: []string{},
	})
	if err != nil || len(out.Chunks) != 1 {
		t.Fatalf("Read = %+v, %v", out, err)
	}
	item := out.Chunks[0]
	if item.Products == nil || len(*item.Products) != 0 || item.ProductsTotal == nil || *item.ProductsTotal != 0 {
		t.Fatalf("products = %s, want an empty list and total 0", resultJSON(t, out))
	}
}

func TestReadProductStoresFilterBeforeCap(t *testing.T) {
	out, err := Read(t.Context(), productStoresFixture(), Request{
		IDs: []string{"chunk"}, WithProducts: true, ProductsLimit: new(1), ProductStores: []string{"shop.example"},
	})
	if err != nil || len(out.Chunks) != 1 {
		t.Fatalf("Read = %+v, %v", out, err)
	}
	item := out.Chunks[0]
	if item.Products == nil || len(*item.Products) != 1 || (*item.Products)[0].ProductID != "shop.example:1" ||
		!slices.Equal((*item.Products)[0].Via, []string{"code"}) || item.ProductsTotal == nil || *item.ProductsTotal != 1 {
		t.Fatalf("products = %s, want the in-scope code match and total 1", resultJSON(t, out))
	}
}

func TestReadProductStoresNilKeepsBehaviour(t *testing.T) {
	out, err := Read(t.Context(), productStoresFixture(), Request{
		IDs: []string{"chunk"}, WithProducts: true, ProductsLimit: new(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"chunks":[{"id":"chunk","doc_id":"doc","seq":0,"text":"AB12345 CD12345","products":[{"id":"foreign-ref","store":"other.example","product_id":"other.example:1","title":"Foreign ref","brand":"","via":["ref"],"matched":[]}],"products_total":3}],"meta":{"returned":1,"missing":[],"truncated":[],"omitted_for_budget":[]}}`
	if got := string(resultJSON(t, out)); got != want {
		t.Fatalf("Read = %s, want %s", got, want)
	}
}

func TestReadProductSpans(t *testing.T) {
	// The first prefix splits a multibyte rune in a matching span. The
	// second prefix ends in an apparent whole code inside a longer token.
	const boundaryCode = "AB12345"
	st := indexedChunks([]graph.Chunk{
		{ID: "cross", Text: strings.Repeat(" ", ChunkTextCap-3) + "AB界12345"},
		{ID: "apparent", Text: strings.Repeat(" ", ChunkTextCap-7) + "AB12345EXTRA"},
		{ID: "inside", Text: "AB界12345, " + strings.Repeat(" ", ChunkTextCap) + "AB界12345"},
		{ID: "tail", Text: strings.Repeat(" ", ChunkTextCap+1) + "AB12345"},
		{ID: "boundary", Text: strings.Repeat(" ", ChunkTextCap-len(boundaryCode)) + boundaryCode + ", rest"},
	}, []graph.ProductIdentifierObservation{mentionObservation("actual", "AB界12345"), mentionObservation("apparent", "AB12345")})
	out, err := Read(t.Context(), st, Request{IDs: []string{"cross", "apparent", "inside", "tail", "boundary"}, WithProducts: true})
	if err != nil || len(out.Chunks) != 5 {
		t.Fatalf("Read = %+v, %v", out.Meta, err)
	}
	for i, want := range []string{
		`[{"id":"actual","store":"products.test","product_id":"products.test:actual","title":"actual","brand":"","via":["code"],"matched":[{"feature":"catalogue_code","value":"AB界12345"}],"outside_returned_text":true}]`,
		`[]`,
		`[{"id":"actual","store":"products.test","product_id":"products.test:actual","title":"actual","brand":"","via":["code"],"matched":[{"feature":"catalogue_code","value":"AB界12345"}]}]`,
		`[{"id":"apparent","store":"products.test","product_id":"products.test:apparent","title":"apparent","brand":"","via":["code"],"matched":[{"feature":"catalogue_code","value":"AB12345"}],"outside_returned_text":true}]`,
		`[{"id":"apparent","store":"products.test","product_id":"products.test:apparent","title":"apparent","brand":"","via":["code"],"matched":[{"feature":"catalogue_code","value":"AB12345"}]}]`,
	} {
		t.Run(out.Chunks[i].ID, func(t *testing.T) {
			var item map[string]json.RawMessage
			encoded, err := json.Marshal(out.Chunks[i])
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &item); err != nil {
				t.Fatal(err)
			}
			if string(item["products"]) != want {
				t.Errorf("products = %s, want %s", item["products"], want)
			}
		})
	}
	if len(out.Chunks[0].Text) != ChunkTextCap-1 || !strings.HasSuffix(out.Chunks[1].Text, "AB12345") {
		t.Fatal("fixture missed the expected clipping boundary")
	}
	if chunk := out.Chunks[4]; !chunk.Truncated || len(chunk.Text) != ChunkTextCap || !strings.HasSuffix(chunk.Text, boundaryCode) {
		t.Fatal("fixture missed the exact clipping boundary")
	}
}

func TestReadProductLimits(t *testing.T) {
	var observations []graph.ProductIdentifierObservation
	var text strings.Builder
	for i := 11; i >= 0; i-- {
		code := fmt.Sprintf("AB123%02d", i)
		obs := mentionObservation(fmt.Sprintf("%02d", i), code)
		observations = append(observations, obs)
		fmt.Fprintf(&text, "%s, ", code)
	}
	for i := 10; i >= 0; i-- {
		code := fmt.Sprintf("BC123%02d", i)
		obs := mentionObservation("00", code)
		if i < 2 {
			obs.Feature = "product_reference"
		}
		observations = append(observations, obs, obs)
		fmt.Fprintf(&text, "%s, %s, ", code, code)
	}
	st := indexedChunks([]graph.Chunk{{ID: "a", Text: text.String()}}, observations)
	for _, tc := range []struct {
		limit *int
		want  int
	}{{nil, 10}, {new(1), 1}, {new(50), 12}} {
		out, err := Read(t.Context(), st, Request{IDs: []string{"a"}, WithProducts: true, ProductsLimit: tc.limit})
		if err != nil || len(out.Chunks) != 1 {
			t.Fatalf("Read: %+v %v", out, err)
		}
		item := out.Chunks[0]
		if item.ProductsTotal == nil || *item.ProductsTotal != 12 || item.Products == nil || len(*item.Products) != tc.want {
			t.Fatalf("products: %+v", item)
		}
		for i, p := range *item.Products {
			if p.ID != fmt.Sprintf("%02d", i) {
				t.Fatalf("products out of order: %+v", *item.Products)
			}
		}
		p := (*item.Products)[0]
		var values []string
		for _, match := range p.Matched {
			values = append(values, match.Feature+":"+match.Value)
		}
		want := []string{"catalogue_code:AB12300", "catalogue_code:BC12302", "catalogue_code:BC12303",
			"catalogue_code:BC12304", "catalogue_code:BC12305", "catalogue_code:BC12306", "catalogue_code:BC12307", "catalogue_code:BC12308"}
		if !slices.Equal(values, want) {
			t.Errorf("matched = %v, want %v", values, want)
		}
	}
}

func TestReadProductDegradation(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		load func(context.Context) (*graph.ProductMentionIndex, error)
		text string
	}{
		{"missing loader", "index_unavailable", nil, "AB12345"},
		{"load error", "index_unavailable", func(context.Context) (*graph.ProductMentionIndex, error) {
			return nil, errors.New("raw database error")
		}, "AB12345"},
		{"oversized index", "index_too_large", func(context.Context) (*graph.ProductMentionIndex, error) {
			return nil, fmt.Errorf("database context: %w", graph.ErrProductMentionIndexTooLarge)
		}, "AB12345"},
		{"server timeout", "index_unavailable", func(ctx context.Context) (*graph.ProductMentionIndex, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
				t.Errorf("deadline = %v, present = %t", deadline, ok)
			}
			return nil, graph.ErrQueryTimeout
		}, "AB12345"},
		{"match budget", "match_budget", func(context.Context) (*graph.ProductMentionIndex, error) {
			t.Error("loaded index despite exceeding match budget")
			return nil, nil
		}, strings.Repeat("x", 2<<20+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var st Store = &fakeStore{chunks: []graph.Chunk{{ID: "x", Text: tc.text}}}
			if tc.load != nil {
				st = mentionTestStore{st.(*fakeStore), tc.load}
			}
			out, err := Read(t.Context(), st, Request{IDs: []string{"x"}, WithProducts: true})
			if err != nil || len(out.Chunks) != 1 || out.ProductsError != tc.want {
				t.Fatalf("Read: meta=%+v products_error=%q err=%v", out.Meta, out.ProductsError, err)
			}
			wire := string(resultJSON(t, out))
			if strings.Contains(wire, `"products":`) || strings.Contains(wire, `"products_total":`) || strings.Contains(wire, "raw database error") {
				t.Errorf("degraded output leaked enrichment fields or raw error")
			}
		})
	}
	t.Run("parent deadline", func(t *testing.T) {
		st := mentionTestStore{&fakeStore{chunks: []graph.Chunk{{ID: "x", Text: "AB12345"}}}, func(ctx context.Context) (*graph.ProductMentionIndex, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		out, err := Read(ctx, st, Request{IDs: []string{"x"}, WithProducts: true})
		if err != nil || len(out.Chunks) != 1 || out.ProductsError != "index_unavailable" {
			t.Fatalf("Read: %+v %v", out, err)
		}
	})
}

func TestReadProductsOffJSON(t *testing.T) {
	st := mentionTestStore{&fakeStore{chunks: []graph.Chunk{{ID: "c1", DocID: "d1", Text: "AB12345 <tag>"}}},
		func(context.Context) (*graph.ProductMentionIndex, error) {
			t.Fatal("loaded index with enrichment off")
			return nil, nil
		}}
	out, err := Read(t.Context(), st, Request{IDs: []string{"absent", "c1"}, WithProducts: false})
	const want = `{"chunks":[{"id":"c1","doc_id":"d1","seq":0,"text":"AB12345 \u003ctag\u003e"}],"meta":{"returned":1,"missing":["absent"],"truncated":[],"omitted_for_budget":[]}}`
	if err != nil || string(resultJSON(t, out)) != want {
		t.Fatalf("Read: %s, %v", resultJSON(t, out), err)
	}
}

func TestReadProductMatchBudgetAcrossChunks(t *testing.T) {
	for _, tc := range []struct {
		name       string
		chunkBytes int
		wantError  string
	}{
		{"over budget", 800 << 10, "match_budget"},
		{"under budget", 600 << 10, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const code = "AB12345"
			text := code + strings.Repeat(" ", tc.chunkBytes-len(code))
			ids := []string{"a", "b", "c"}
			var chunks []graph.Chunk
			for seq, id := range ids {
				chunks = append(chunks, graph.Chunk{ID: id, Seq: seq, Text: text})
			}
			st := mentionTestStore{&fakeStore{chunks: chunks}, func(context.Context) (*graph.ProductMentionIndex, error) {
				if tc.wantError == "match_budget" {
					t.Fatal("loaded index despite the combined chunk text exceeding the match budget")
				}
				return graph.BuildProductMentionIndex([]graph.ProductIdentifierObservation{mentionObservation("a", code)}), nil
			}}
			out, err := Read(t.Context(), st, Request{IDs: ids, WithProducts: true})
			if err != nil || len(out.Chunks) != len(ids) || out.ProductsError != tc.wantError {
				t.Fatalf("Read: meta=%+v products_error=%q, want %q, err=%v", out.Meta, out.ProductsError, tc.wantError, err)
			}
			for _, chunk := range out.Chunks {
				if tc.wantError == "match_budget" {
					if chunk.Products != nil || chunk.ProductsTotal != nil {
						t.Errorf("chunk %q returned products after exceeding the match budget", chunk.ID)
					}
				} else if chunk.ProductsTotal == nil || *chunk.ProductsTotal != 1 || chunk.Products == nil || len(*chunk.Products) != 1 {
					t.Errorf("chunk %q did not return its matching product", chunk.ID)
				}
			}
		})
	}
}

func TestReadProductEncodedBudget(t *testing.T) {
	a := mentionObservation("a", "AA12345")
	b := mentionObservation("b", "BB12345")
	// Escapes make each product expensive despite a short stored title.
	a.Product.Title, b.Product.Title = strings.Repeat("<", 3400), strings.Repeat("<", 3400)
	store := indexedChunks([]graph.Chunk{
		{ID: "first", Text: `AA12345 BB12345`},
		{ID: "last", Text: `AA12345 BB12345`},
	}, []graph.ProductIdentifierObservation{a, b})
	req := Request{IDs: []string{"first", strings.Repeat("\"\\", 44), "last"}, WithProducts: true}
	read := func() Result {
		t.Helper()
		got, err := Read(t.Context(), store, req)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := read()
	if len(got.Chunks) != 2 || got.Chunks[0].Products == nil || got.Chunks[1].Products == nil ||
		len(*got.Chunks[0].Products) != 1 || len(*got.Chunks[1].Products) != 0 ||
		got.Chunks[0].ProductsOmittedForBudget != 1 || got.Chunks[1].ProductsOmittedForBudget != 2 ||
		got.Chunks[1].ProductsTotal == nil || *got.Chunks[1].ProductsTotal != 2 {
		t.Fatalf("unexpected trim or chunk drop: %+v", got.Meta)
	}
	if (*got.Chunks[0].Products)[0].ID != "a" || !reflect.DeepEqual(got.Meta.Missing, req.IDs[1:2]) {
		t.Fatalf("product order or metadata changed: %+v", got.Meta)
	}
	// Fill the entire remaining wire budget with plain bytes in the text.
	extra := ResponseBudget - len(resultJSON(t, got))
	if extra <= 0 || extra+len(store.chunks[0].Text)+1 > ChunkTextCap {
		t.Fatalf("bad fixture: %d spare bytes", extra)
	}
	store.chunks[0].Text += "," + strings.Repeat("x", extra-1)
	if exact := len(resultJSON(t, read())); exact != ResponseBudget {
		t.Fatalf("encoded boundary = %d, want %d", exact, ResponseBudget)
	}
	store.chunks[0].Text += "x"
	got = read()
	if len(got.Chunks) != 2 || len(*got.Chunks[0].Products) != 0 || got.Chunks[0].ProductsOmittedForBudget != 2 ||
		len(resultJSON(t, got)) > ResponseBudget {
		t.Fatalf("one byte over the boundary did not trim one more product: %+v", got.Meta)
	}

	// When text alone is too large, all product entries are trimmed before
	// the original suffix drop, including budget/truncation metadata.
	store.chunks[0].Text = "AA12345, " + strings.Repeat("<", 3000) + strings.Repeat(" ", ChunkTextCap)
	store.chunks[1].Text = "BB12345, " + strings.Repeat("<", 3000) + strings.Repeat(" ", ChunkTextCap)
	got = read()
	if len(got.Chunks) != 1 || len(*got.Chunks[0].Products) != 0 || got.Chunks[0].ProductsOmittedForBudget != 1 ||
		!slices.Equal(got.Meta.OmittedForBudget, []string{"last"}) ||
		!slices.Equal(got.Meta.Truncated, []string{"first"}) || len(resultJSON(t, got)) > ResponseBudget {
		t.Fatalf("expected products trimmed before a chunk is dropped: %+v", got.Meta)
	}
}

func TestReadDocumentProductEncodedBudget(t *testing.T) {
	linked := graph.Product{ID: "linked", Title: "Ref title", Brand: strings.Repeat("<", 3000)}
	st := documentTestStore{
		mentionTestStore: indexedChunks([]graph.Chunk{
			{ID: "first", DocID: "doc", Text: "AB12345"},
			{ID: "last", DocID: "doc", Text: "AB12345"},
		}, []graph.ProductIdentifierObservation{mentionObservation("linked", "AB12345")}),
		documents: func(ids []string) (map[string]graph.DocumentProductLinks, error) {
			if !slices.Equal(ids, []string{"doc"}) {
				t.Fatalf("loaded documents %v, want one distinct ID", ids)
			}
			return map[string]graph.DocumentProductLinks{"doc": {Status: graph.StatusDone, Refs: []graph.Product{linked}}}, nil
		},
	}
	read := func() Result {
		t.Helper()
		out, err := Read(t.Context(), st, Request{IDs: []string{"first", "last"}, WithProducts: true})
		if err != nil || len(out.Chunks) != 2 {
			t.Fatalf("read: %+v, %v", out.Meta, err)
		}
		return out
	}
	out := read()
	first, last := out.Chunks[0], out.Chunks[1]
	if first.Products == nil || len(*first.Products) != 1 ||
		!slices.Equal((*first.Products)[0].Via, []string{"ref", "code"}) ||
		last.Products == nil || len(*last.Products) != 0 ||
		last.ProductsOmittedForBudget != 1 || last.ProductsTotal == nil || *last.ProductsTotal != 1 {
		t.Fatalf("encoded budget trimmed the wrong products or totals: %+v", out.Chunks)
	}
	// Adjust escaped bytes in the brand and ordinary bytes in the text
	// so this reaches the boundary without causing chunk text truncation.
	spare := ResponseBudget - len(resultJSON(t, out))
	if spare <= 0 {
		t.Fatalf("expected spare wire bytes, got %d", spare)
	}
	linked.Brand += strings.Repeat("<", spare/6)
	st.chunks[0].Text += strings.Repeat(" ", spare%6)
	if size := len(resultJSON(t, read())); size != ResponseBudget {
		t.Fatalf("encoded boundary = %d, want %d", size, ResponseBudget)
	}
	st.chunks[0].Text += " "
	out = read()
	if first := out.Chunks[0]; len(*first.Products) != 0 || first.ProductsOmittedForBudget != 1 {
		t.Fatalf("expected one additional trim at the boundary: %+v", first)
	}
}

func TestReadDocumentProductsWhenCodeUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name, text, productsError string
	}{
		{"timeout", "AB12345", "index_unavailable"},
		{"match budget", strings.Repeat(" ", maxMatchBytes+1), "match_budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := documentTestStore{
				mentionTestStore: mentionTestStore{&fakeStore{chunks: []graph.Chunk{{ID: "c0", DocID: "doc", Text: tc.text}}},
					func(context.Context) (*graph.ProductMentionIndex, error) {
						if tc.productsError == "match_budget" {
							t.Error("loaded code index after exceeding the match budget")
						}
						return nil, graph.ErrQueryTimeout
					}},
				documents: func([]string) (map[string]graph.DocumentProductLinks, error) {
					return map[string]graph.DocumentProductLinks{"doc": {
						Status: graph.StatusDone, Domain: "catalogue", Refs: []graph.Product{{ID: "linked"}},
					}}, nil
				},
			}
			out, err := Read(t.Context(), st, Request{IDs: []string{"c0"}, WithProducts: true})
			if err != nil || len(out.Chunks) != 1 || out.ProductsError != tc.productsError || out.LinksPartial != "index_unavailable" {
				t.Fatalf("read: %+v, %v", out, err)
			}
			if p := out.Chunks[0].Products; p == nil || len(*p) != 1 || !slices.Equal((*p)[0].Via, []string{"ref"}) {
				t.Fatalf("read lost an available ref: %v", p)
			}
		})
	}
}
