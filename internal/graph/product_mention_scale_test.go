package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

type productReadCounter struct {
	neo4j.Driver
	statements     []int
	durations      []time.Duration
	deadlines      []time.Time
	statementsDone [][]time.Duration
	afterTx        func(int)
}

func (d *productReadCounter) NewSession(ctx context.Context, cfg neo4j.SessionConfig) neo4j.Session {
	return productReadCounterSession{Session: d.Driver.NewSession(ctx, cfg), counts: d}
}

type productReadCounterSession struct {
	neo4j.Session
	counts *productReadCounter
}

func (s productReadCounterSession) BeginTransaction(ctx context.Context, cfg ...func(*neo4j.TransactionConfig)) (neo4j.ExplicitTransaction, error) {
	start := time.Now()
	tx, err := s.Session.BeginTransaction(ctx, cfg...)
	if err != nil {
		return nil, err
	}
	n := len(s.counts.statements)
	s.counts.statements = append(s.counts.statements, 0)
	deadline, _ := ctx.Deadline()
	s.counts.deadlines = append(s.counts.deadlines, deadline)
	s.counts.durations = append(s.counts.durations, 0)
	s.counts.statementsDone = append(s.counts.statementsDone, nil)
	return &productReadCounterTx{ExplicitTransaction: tx, counts: s.counts, n: n, start: start}, nil
}

type productReadCounterTx struct {
	neo4j.ExplicitTransaction
	counts *productReadCounter
	n      int
	start  time.Time
	last   time.Time
}

func (tx *productReadCounterTx) finishStatement() {
	if !tx.last.IsZero() {
		tx.counts.statementsDone[tx.n] = append(tx.counts.statementsDone[tx.n], time.Since(tx.last))
	}
}

func (tx *productReadCounterTx) Run(ctx context.Context, q string, params map[string]any) (neo4j.Result, error) {
	tx.finishStatement()
	tx.last = time.Now()
	tx.counts.statements[tx.n]++
	return tx.ExplicitTransaction.Run(ctx, q, params)
}

func (tx *productReadCounterTx) Commit(ctx context.Context) error {
	tx.finishStatement()
	err := tx.ExplicitTransaction.Commit(ctx)
	tx.counts.durations[tx.n] = time.Since(tx.start)
	if err == nil && tx.counts.afterTx != nil {
		tx.counts.afterTx(tx.n)
	}
	return err
}

func countProductReadStatements(t *testing.T, st *Store) *productReadCounter {
	t.Helper()
	counter := &productReadCounter{Driver: st.driver}
	st.driver = counter
	t.Cleanup(func() { st.driver = counter.Driver })
	return counter
}

func TestProductMentionReadScale(t *testing.T) {
	f := newPushFixture(t)
	ctx := t.Context()
	const code = "mention-scale-code"
	f.query(`MATCH (s:ProductStore {store:$store}) DETACH DELETE s`, map[string]any{"store": pushStore})
	f.query(`CREATE (:FeatureRev {id:'catalogue_code', key:$key})`, map[string]any{"key": code})
	for storeIdx := range 2 {
		store := fmt.Sprintf("mention-scale-%d.test", storeIdx)
		f.query(`CREATE (:ProductStore {store:$store, domain:$domain, product_count:10000})`,
			map[string]any{"store": store, "domain": fmt.Sprintf("mention-scale-%d", storeIdx)})
		for start := 0; start < 10000; start += 1000 {
			f.query(`MATCH (f:FeatureRev {key:$key})
				UNWIND range($start, $end) AS i
				WITH f, i, $store + ':' + toString(i) AS id, 'QZ' + toString($j) + 'X' + toString(i) + 'Y' AS code
				CREATE (p:Product {id:id, store:$store, product_id:id, title:'Scale fixture product'})
				CREATE (p)-[:HAS_FEATURE {release_id:$release,
					items_fold:[j IN range(0, CASE WHEN i = 0 THEN 63 ELSE 0 END) | code + 'L' + toString(j)]}]->(f)
				WITH p, f, i, code UNWIND range(1, 5) AS j
				CREATE (p)-[:HAS_FEATURE {release_id:$release,
					value_fold:CASE WHEN i < 2 AND j = 1 THEN code + '-' + toString(j) ELSE code + toString(j) END}]->(f)`,
				map[string]any{"key": code, "start": start, "end": start + 999, "store": store, "j": storeIdx, "release": pushA})
		}
	}
	startCold := time.Now()
	index, err := f.store.LoadProductMentionIndex(ctx)
	coldLoad := time.Since(startCold)
	if err != nil {
		t.Fatal(err)
	}
	if index.Stats.IdentifierValues != 120126 || index.Stats.EligibleKeys != 120126 || len(index.KeyedProducts()) != 20_000 {
		t.Fatalf("index size: %+v", index.Stats)
	}
	var largeChunkID string
	fixtureIDs := make(map[string][]string)
	for i := range 2 {
		scope := fmt.Sprintf("mention-scale-%d", i)
		for j := range 10 {
			source := fmt.Sprintf("scale/product-mention/%d/%02d", i, j)
			r, err := f.store.UpsertDocument(ctx, DocumentInput{Kind: "text", Source: source, Hash: source, Domain: scope})
			if err != nil {
				t.Fatal(err)
			}
			fixtureIDs[scope] = append(fixtureIDs[scope], r.ID)
			docID := r.ID
			t.Cleanup(func() {
				if err := f.store.DeleteDocument(context.Background(), docID); err != nil {
					t.Error(err)
				}
			})
			chunks := make([]Chunk, 1000)
			for k := range chunks {
				n := j*1000 + k
				text := "Technical catalogue specifications and product instructions."
				switch {
				case n < 2500:
					// At least one real passage for every sampled product.
					var vals []string
					for x := range 1 + n%5 {
						product := n % 2500
						vals = append(vals, fmt.Sprintf("QZ%dX%dY%d", i, product, x+1))
					}
					if n < 2 {
						vals = append(vals, fmt.Sprintf("QZ%dX%dY-1", i, n))
					} else if n < 850 {
						// Genuine hits contain both forms and outrank the split-run decoys.
						vals = append(vals[:min(len(vals), 3)], fmt.Sprintf("QZ%dX%dY1", i, n%2), fmt.Sprintf("QZ%dX%dY-1", i, n%2))
					}
					text += " " + strings.Join(vals, "; ")
					if n == 10 && i == 0 {
						text += "!" + strings.Repeat("界", 25_000)
						largeChunkID = ChunkID(docID, k)
					}
				case n < 5000:
					// Both split runs occur but can never form an exact match.
					text += fmt.Sprintf(" QZ%dX%dY. Unrelated item. 1", i, n%2)
				}
				chunks[k] = Chunk{Seq: k, Text: text}
			}
			if err := f.store.PutChunks(ctx, docID, chunks); err != nil {
				t.Fatal(err)
			}
			if err := f.store.SetStatus(ctx, docID, StatusDone, ""); err != nil {
				t.Fatal(err)
			}
		}
	}

	type sample struct {
		store, product int
		scoped         bool
	}
	// Include six saturated reads, two scopes, a large-text hit, and a fixed
	// random sample of ordinary citing products.
	rng := rand.New(rand.NewPCG(20261122, 2))
	samples := []sample{{0, 0, false}, {0, 0, true}, {0, 1, false}, {1, 0, false}, {1, 1, true}, {1, 1, false}, {0, 10, true}}
	for len(samples) < 50 {
		samples = append(samples, sample{rng.IntN(2), 2 + rng.IntN(2498), rng.IntN(2) == 0})
	}
	lookupFor := func(s sample) ProductLookup {
		lookup := ProductLookup{ID: fmt.Sprintf("mention-scale-%d.test:%d", s.store, s.product)}
		if s.scoped {
			lookup.Domain = fmt.Sprintf("mention-scale-%d", s.store)
		}
		return lookup
	}
	counter := countProductReadStatements(t, f.store)
	measureRead := func(lookup ProductLookup) (ProductDetailWithMentions, ProductMentionRead, time.Duration) {
		t.Helper()
		counter.statements = nil
		counter.durations = nil
		counter.deadlines = nil
		counter.statementsDone = nil
		start := time.Now()
		out, read, err := f.store.GetProductWithMentions(ctx, lookup)
		elapsed := time.Since(start)
		if err != nil || out.ProductMentionFields == nil || out.MentionsError != "" || out.MentionsTotal == 0 {
			t.Fatalf("%+v: unsuccessful enrichment (%s): %v, %q", lookup, elapsed, err, out.MentionsError)
		}
		if !slices.Equal(counter.statements, []int{2, 1, 2}) {
			t.Fatalf("statements per transaction = %v", counter.statements)
		}
		return out, read, elapsed
	}
	for i := range 5 {
		measureRead(lookupFor(sample{i % 2, i + 20, i%2 == 0}))
	}
	var endToEnd, indexLoad, tokenTx, tokenStatement, searchMatch, baseline []time.Duration
	var partial, raw, eligible int
	var sawLarge bool
	for _, sample := range samples {
		out, read, elapsed := measureRead(lookupFor(sample))
		endToEnd = append(endToEnd, elapsed)
		indexLoad = append(indexLoad, read.IndexLoad)
		tokenTx = append(tokenTx, counter.durations[1])
		tokenStatement = append(tokenStatement, counter.statementsDone[1][0])
		searchMatch = append(searchMatch, read.SearchMatch)
		if out.MentionsPartial {
			partial++
		}
		if read.RawSaturated {
			raw++
		}
		if read.EligibleSaturated {
			eligible++
		}
		for _, mention := range out.Mentions {
			sawLarge = sawLarge || mention.ChunkID == largeChunkID
		}
	}
	if !sawLarge || raw == 0 || eligible == 0 {
		t.Errorf("scale fixture exercised large=%v raw saturation=%d eligible saturation=%d", sawLarge, raw, eligible)
	}
	statements := slices.Clone(counter.statements)
	for _, sample := range samples {
		lookup := lookupFor(sample)
		ids := fixtureIDs[lookup.Domain]
		if lookup.Domain == "" {
			ids = append(slices.Clone(fixtureIDs["mention-scale-0"]), fixtureIDs["mention-scale-1"]...)
		}
		start := time.Now()
		if err := exactProductMentionBaseline(ctx, f.store, lookup, ids); err != nil {
			t.Fatal(err)
		}
		baseline = append(baseline, time.Since(start))
	}
	p95 := func(values []time.Duration) float64 {
		slices.Sort(values)
		return float64(values[(95*len(values)+99)/100-1]) / float64(time.Millisecond)
	}
	// Both read paths reuse the warmed index. Report the cold load separately.
	t.Logf("cold index load %s, warm token tx p95 %.3fms, token statement p95 %.3fms",
		coldLoad, p95(tokenTx), p95(tokenStatement))
	measurement := struct {
		Samples            int     `json:"samples"`
		EndToEndMS         float64 `json:"end_to_end_p95_ms"`
		SearchMatchMS      float64 `json:"search_match_p95_ms"`
		IndexLoadMS        float64 `json:"index_load_p95_ms"`
		ExactBaselineMS    float64 `json:"exact_baseline_p95_ms"`
		PartialRate        float64 `json:"partial_rate"`
		RawSaturation      int     `json:"raw_saturation"`
		EligibleSaturation int     `json:"eligible_saturation"`
		StatementsPerTx    []int   `json:"statements_per_tx"`
	}{len(samples), p95(endToEnd), p95(searchMatch), p95(indexLoad), p95(baseline), float64(partial) / float64(len(samples)), raw, eligible, statements}
	data, err := json.MarshalIndent(measurement, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scale results: %s", data)
	if path := os.Getenv("GKG_TEST_MENTION_SCALE_REPORT"); path != "" {
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("GKG_TEST_MENTION_SCALE_GATE") == "1" && (measurement.EndToEndMS > 600 || measurement.SearchMatchMS > 300) {
		t.Errorf("scale gates exceeded (end-to-end <=600 ms, search+match <=300 ms): %s", data)
	}
}

func exactProductMentionBaseline(ctx context.Context, st *Store, lookup ProductLookup, ids []string) error {
	out, err := st.GetProduct(ctx, lookup)
	if err != nil {
		return err
	}
	index, err := st.LoadProductMentionIndex(ctx)
	if err != nil {
		return err
	}
	keys := make(map[string]struct{})
	for key, item := range index.keys {
		if item.product.ID == out.ID {
			keys[key] = struct{}{}
		}
	}
	count := 0
	err = st.runReadTx(ctx, func(tx neo4j.ManagedTransaction) error {
		rows, err := tx.Run(ctx, `
UNWIND $ids AS id
MATCH (d:Document {id:id})-[:HAS_CHUNK]->(c:Chunk)
WHERE d.status='done' AND ($domain='' OR d.domain=$domain)
RETURN c.text AS text`, map[string]any{"ids": ids, "domain": lookup.Domain})
		if err != nil {
			return err
		}
		for rows.Next(ctx) {
			if len(productobs.MatchCodes(getString(rows.Record(), "text"), keys)) != 0 {
				count++
			}
		}
		return rows.Err()
	})
	if err == nil && count == 0 {
		return fmt.Errorf("no exact baseline hits for %s", out.ID)
	}
	return err
}
