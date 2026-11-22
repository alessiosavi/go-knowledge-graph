package graph

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/productobs"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestProductMentionCandidateLimitWiring(t *testing.T) {
	if !strings.Contains(mentionCandidateQuery, fmt.Sprintf("{limit: %d}", mentionRawLimit)) ||
		!strings.Contains(mentionCandidateQuery, fmt.Sprintf("\nLIMIT %d\n", mentionEligibleLimit+1)) {
		t.Fatalf("candidate query must use the raw cap and fetch one extra eligible row: %s", mentionCandidateQuery)
	}
}

func TestProductMentionQueryBuilder(t *testing.T) {
	for _, tc := range []struct {
		name, key, value, domain, want string
		partial                        bool
	}{
		{"operator key", "OR", "", "", `data.text:"OR"`, false},
		{"joined", "AB12345", "ab12345", "", `data.text:"AB12345"`, false},
		{"split", "ZX98765", "ZX_98765", "", `data.text:"ZX98765" OR (data.text:"ZX" AND data.text:"98765")`, false},
		{"uncut transition", "TC2SL", "TC2SL", "", `data.text:"TC2SL"`, false},
		{"operators", "OR12345", "OR-AND-NOT", "", `data.text:"OR12345" OR (data.text:"OR" AND data.text:"AND" AND data.text:"NOT")`, false},
		{"scoped", "AB12345", "ab12345", "reads", `data.domain_tok:"` + domain.DomainToken("reads") + `" AND (data.text:"AB12345")`, false},
		{"non-ASCII", "ABÉ12345", "abé12345", "", "", true},
		{"combining", "A\u030112345", "A\u0301-12345", "", "", true},
		{"invalid", `AB"12345`, `AB"12345`, "", "", true},
		{"long only", strings.Repeat("X", 39) + "1", "", "", "", true},
		{"long joined, split survives", strings.Repeat("X", 39) + "1", strings.Repeat("X", 39) + "-1", "",
			`(data.text:"` + strings.Repeat("X", 39) + `" AND data.text:"1")`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := &ProductMentionIndex{keys: map[string]productMentionKey{tc.key: {product: &Product{ID: "p"}, evidence: []ProductMentionEvidence{{ValueFold: tc.value}}}}}
			got := i.buildMentionQuery("p", tc.domain)
			if got.text != tc.want || got.partial != tc.partial || (got.keys == 0) != (tc.want == "") {
				t.Fatalf("query = %+v, want %q partial=%v", got, tc.want, tc.partial)
			}
		})
	}
	t.Run("key cap", func(t *testing.T) {
		i := &ProductMentionIndex{keys: map[string]productMentionKey{}}
		for n := range 65 {
			i.keys[fmt.Sprintf("AB%05d", n)] = productMentionKey{product: &Product{ID: "p"}}
		}
		got := i.buildMentionQuery("p", "")
		if got.keys != 64 || !got.partial || strings.Contains(got.text, "AB00064") {
			t.Fatal(got)
		}
	})
	t.Run("key with only duplicate clauses", func(t *testing.T) {
		p := &Product{ID: "p"}
		evidence := []ProductMentionEvidence{{ValueFold: strings.Repeat("A", 38) + "-12345"}}
		// Only the first synthetic key can contribute the shared split clause.
		i := &ProductMentionIndex{keys: map[string]productMentionKey{
			strings.Repeat("A", 38) + "12345": {product: p, evidence: evidence},
			strings.Repeat("A", 38) + "67890": {product: p, evidence: evidence},
		}}
		got := i.buildMentionQuery("p", "")
		if got.keys != 1 || got.droppedKeys != 1 || !got.partial {
			t.Fatalf("expected one emitted and one dropped key: %+v", got)
		}
	})
	t.Run("term cap", func(t *testing.T) {
		p := &Product{ID: "p"}
		i := &ProductMentionIndex{keys: map[string]productMentionKey{}}
		for n := range 64 {
			key := fmt.Sprintf("AB%05d", n)
			i.keys[key] = productMentionKey{product: p, evidence: []ProductMentionEvidence{{ValueFold: fmt.Sprintf("A B C D E F G %05d", n)}}}
		}
		got := i.buildMentionQuery("p", "")
		if got.keys != 56 || !got.partial || strings.Count(got.text, "data.text:") != 504 {
			t.Fatalf("keys=%d partial=%v terms=%d", got.keys, got.partial, strings.Count(got.text, "data.text:"))
		}
	})
	t.Run("byte cap", func(t *testing.T) {
		p := &Product{ID: "p"}
		i := &ProductMentionIndex{keys: map[string]productMentionKey{}}
		for n := range 64 {
			key := fmt.Sprintf("AB%05d", n)
			i.keys[key] = productMentionKey{product: p, evidence: []ProductMentionEvidence{{ValueFold: fmt.Sprintf("%s-%s-%s-%s-%s-%05d", strings.Repeat("A", 38), strings.Repeat("B", 38), strings.Repeat("C", 38), strings.Repeat("D", 38), strings.Repeat("E", 38), n)}}}
		}
		got := i.buildMentionQuery("p", "reads")
		if got.keys >= 64 || !got.partial || len(got.text) > 16<<10 || strings.Count(got.text, "data.text:") >= 512 {
			t.Fatalf("keys=%d partial=%v bytes=%d", got.keys, got.partial, len(got.text))
		}
	})
}

func TestProductMentionBudgetAndExcerpt(t *testing.T) {
	input := []mentionCandidate{{ProductChunkMention: ProductChunkMention{ChunkID: "oversize"}, chars: (2<<20)/4 + 1}, {ProductChunkMention: ProductChunkMention{ChunkID: "exact"}, chars: (2 << 20) / 4}, {ProductChunkMention: ProductChunkMention{ChunkID: "over"}, chars: 1}}
	got, partial := selectMentionCandidates(input)
	if len(got) != 1 || got[0].ChunkID != "exact" || !partial {
		t.Fatalf("got %+v partial=%v", got, partial)
	}
	got, partial = selectMentionCandidates(input[1:2])
	if len(got) != 1 || partial {
		t.Fatalf("exact bound %+v partial=%v", got, partial)
	}
	text := strings.Repeat("界", 120) + "!" + strings.Repeat("-", 320) + "AB12345"
	excerpt := mentionExcerpt(text, 361)
	if len(excerpt) > 300 || !utf8.ValidString(excerpt) || !strings.Contains(excerpt, "!-") || !strings.Contains(text, excerpt) {
		t.Fatalf("invalid excerpt at the first byte of a >300-byte span: %q", excerpt)
	}
}

func TestProductMentionChunkIDOrder(t *testing.T) {
	value := "AB12345"
	index := BuildProductMentionIndex([]ProductIdentifierObservation{{
		Product: Product{ID: "product"}, Feature: "catalogue_code", ValueFold: &value,
	}})
	rows := []mentionCandidate{
		{ProductChunkMention: ProductChunkMention{DocSource: "a", DocID: "doc", Seq: 2, ChunkID: "c"}, text: value},
		{ProductChunkMention: ProductChunkMention{DocSource: "a", DocID: "doc", Seq: 2, ChunkID: "b"}, text: value},
		{ProductChunkMention: ProductChunkMention{DocSource: "a", DocID: "doc", Seq: 3, ChunkID: "a"}, text: value},
	}
	mentions, err := matchProductMentionCandidates(t.Context(), index, "product", rows)
	if ids := mentionChunkIDs(mentions); err != nil || !slices.Equal(ids, []string{"b", "c", "a"}) {
		t.Fatalf("chunk order: got %v, want [b c a], err %v", ids, err)
	}
}

func TestProductMentionCandidateMemgraph(t *testing.T) {
	f := newPushFixture(t)
	seed := func(name, domain string, status Status, texts []string) string {
		t.Helper()
		r, err := f.store.UpsertDocument(t.Context(), DocumentInput{Source: "test/product-mention-read/" + name, Domain: domain, Hash: name, Title: name, Kind: "text"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.store.DeleteDocument(context.Background(), r.ID); err != nil {
				t.Error(err)
			}
		})
		chunks := make([]Chunk, len(texts))
		for i, text := range texts {
			chunks[i] = Chunk{Seq: i, Text: text}
		}
		if err := f.store.PutChunks(t.Context(), r.ID, chunks); err != nil {
			t.Fatal(err)
		}
		f.query(`MATCH (d:Document {id:$id}) SET d.status=$status`, map[string]any{"id": r.ID, "status": string(status)})
		return r.ID
	}
	const scope = "mention-one"
	done := seed("done", scope, StatusDone, []string{"ab12345", "zx 98765", "TC 2SL", "151L-Z", "ZZ-AB12345", "AM12345", "TC 2SL; AB12345"})
	other := seed("other", "mention-two", StatusDone, []string{"AB12345"})
	f.query(`MATCH (d:Document {id:$id})-[:HAS_CHUNK]->(c:Chunk {seq:0}) CREATE (d)-[:HAS_CHUNK]->(c)`, map[string]any{"id": done})
	f.query(`CREATE (f:FeatureRev {id:'catalogue_code',key:'mr-code'}),
		(a:Product {id:'mr-a',store:$store}),
		(:ProductStore {store:'mr-other',domain:'mention-two'}),
		(b:Product {id:'mr-b',store:'mr-other'}),
		(a)-[:HAS_FEATURE {release_id:$rel,items_fold:['AB12345','ZX_98765','TC2SL','151LZ','AM12345']}]->(f),
		(b)-[:HAS_FEATURE {release_id:$rel,value_fold:'AM12345'}]->(f)`, map[string]any{"store": pushStore, "rel": pushA})
	read := func(domain string) ProductDetailWithMentions {
		t.Helper()
		out, _, err := f.store.GetProductWithMentions(t.Context(), ProductLookup{ID: "mr-a", Domain: domain})
		if err != nil || out.ProductMentionFields == nil || out.MentionsError != "" {
			t.Fatalf("read=%+v err=%v", out, err)
		}
		return out
	}
	first := read(scope)
	if ids := mentionChunkIDs(first.Mentions); !slices.Equal(ids, []string{ChunkID(done, 0), ChunkID(done, 1), ChunkID(done, 6)}) || first.MentionsKeys != 4 || first.MentionsPartial {
		t.Fatalf("scoped IDs=%v fields=%+v", ids, first.ProductMentionFields)
	}
	if got := first.Mentions[2].Entries; len(got) != 2 || got[0].Key != "TC2SL" || got[0].Span != [2]int{0, 6} {
		t.Fatalf("full text entries=%+v", got)
	}
	if got := read("").Mentions; len(got) != 4 || got[3].DocID != other {
		t.Fatalf("global mentions=%+v", got)
	}
	f.query(`MATCH (a:Product {id:'mr-a'})-[:HAS_FEATURE]->(f) CREATE
		(a)-[:HAS_FEATURE {release_id:$rel,value_fold:'RAW12345'}]->(f),
		(a)-[:HAS_FEATURE {release_id:$rel,value_fold:'ELIG12345'}]->(f),
		(a)-[:HAS_FEATURE {release_id:$rel,value_fold:'BIG12345'}]->(f)`, map[string]any{"rel": pushA})
	fill := func(n int, text string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = text
		}
		return out
	}
	seed("pending-under-cap", "mention-status", StatusFetched, fill(201, "raw12345"))
	doneUnderCap := seed("done-under-cap", "mention-status", StatusDone, []string{"raw12345"})
	if out := read("mention-status"); out.MentionsPartial || !slices.Equal(mentionChunkIDs(out.Mentions), []string{ChunkID(doneUnderCap, 0)}) {
		t.Fatalf("non-done hits consumed the eligible cap: %+v", out.ProductMentionFields)
	}
	pending := seed("inflight", "mention-raw", StatusFetched, fill(1000, "raw12345"))
	if out := read("mention-raw"); !out.MentionsPartial || out.MentionsTotal != 0 {
		t.Fatalf("raw saturation with zero eligible: %+v", out.ProductMentionFields)
	}
	if err := f.store.PutChunks(t.Context(), pending, []Chunk{{Seq: 1000, Text: "raw12345"}}); err != nil {
		t.Fatal(err)
	}
	complete := seed("raw-done", "mention-raw", StatusDone, []string{"raw12345"})
	if out := read("mention-raw"); !out.MentionsPartial || out.MentionsTotal > 1 || (out.MentionsTotal == 1 && out.Mentions[0].DocID != complete) {
		t.Fatalf("raw saturation: %+v", out.ProductMentionFields)
	}
	eligible := seed("eligible", "mention-eligible", StatusDone, fill(201, "elig12345"))
	out := read("mention-eligible")
	if !out.MentionsPartial || out.MentionsTotal != 200 {
		t.Fatalf("eligible cap fields=%+v", out.ProductMentionFields)
	}
	want := make([]string, 201)
	for i := range want {
		want[i] = ChunkID(eligible, i)
	}
	slices.Sort(want)
	got := mentionChunkIDs(out.Mentions)
	slices.Sort(got)
	if !slices.Equal(got, want[:200]) {
		t.Fatal("score ties did not use chunk ID before the cap")
	}
	maxChars := (2 << 20) / 4
	exact := "big12345!" + strings.Repeat("界", maxChars-len("big12345!"))
	big := seed("size", "mention-size", StatusDone, []string{exact, exact + "界"})
	if out := read("mention-size"); !out.MentionsPartial || !slices.Equal(mentionChunkIDs(out.Mentions), []string{ChunkID(big, 0)}) {
		t.Fatalf("byte budget: %+v", out.ProductMentionFields)
	}
}

func mentionChunkIDs(mentions []ProductChunkMention) []string {
	ids := make([]string, len(mentions))
	for i, m := range mentions {
		ids[i] = m.ChunkID
	}
	return ids
}

func TestProductMentionMemgraphQuerySemantics(t *testing.T) {
	f := newPushFixture(t)
	ctx := t.Context()
	const scope = "mention-semantics"
	token39, token40, token41 := strings.Repeat("x", 39), strings.Repeat("y", 40), strings.Repeat("z", 41)
	seed := func(name, domain string, texts []string) string {
		t.Helper()
		r, err := f.store.UpsertDocument(ctx, DocumentInput{Kind: "text", Domain: domain, Source: "product-mention-semantics/" + name, Hash: name})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.store.DeleteDocument(context.Background(), r.ID); err != nil {
				t.Error(err)
			}
		})
		chunks := make([]Chunk, len(texts))
		for i, text := range texts {
			chunks[i] = Chunk{Seq: i, Text: text}
		}
		if err := f.store.PutChunks(ctx, r.ID, chunks); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	local := seed("local", scope, []string{
		"qmqalfa qmqbeta or 1234567 " + token39 + " " + token40 + " " + token41,
		"qmqgamma and or12345 9876543",
		"qmqalfa not z\u0301comb999",
		"qmqbeta qmqgamma",
		"qmqalfa z comb999",
	})
	remote := seed("other", "mention-semantics-other", []string{"qmqalfa qmqbeta and not 1234567 " + token39 + " " + token40 + " " + token41})
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprintf("scoped=%v", scoped), func(t *testing.T) {
			search := func(expression string, limit int) []string {
				t.Helper()
				if scoped {
					expression = `data.domain_tok:"` + domain.DomainToken(scope) + `" AND (` + expression + `)`
				}
				rows, err := f.store.runRead(ctx, fmt.Sprintf(`
CALL text_search.search('chunk_text_v2',$query,{limit: %d}) YIELD node, score
RETURN node.id AS id ORDER BY node.id`, limit), map[string]any{"query": expression})
				if err != nil {
					t.Fatal(err)
				}
				ids := make([]string, len(rows))
				for i, r := range rows {
					ids[i] = getString(r, "id")
				}
				if scoped && slices.Contains(ids, ChunkID(remote, 0)) {
					t.Fatalf("scoped query reached another domain: %v", ids)
				}
				return ids
			}
			check := func(query string, other bool, seqs ...int) {
				t.Helper()
				var want []string
				for _, seq := range seqs {
					want = append(want, ChunkID(local, seq))
				}
				if other && !scoped {
					want = append(want, ChunkID(remote, 0))
				}
				slices.Sort(want)
				if got := search(query, 1000); !slices.Equal(got, want) {
					t.Fatalf("%s -> %v, want %v", query, got, want)
				}
			}
			check(`data.text:"QMQALFA" AND (data.text:"QMQBETA" OR data.text:"QMQGAMMA")`, true, 0)
			check(`(data.text:"QMQALFA" AND data.text:"QMQBETA") OR data.text:"QMQGAMMA"`, true, 0, 1, 3)
			// Add a fixture-only guard so incidental words elsewhere in the
			// disposable database cannot affect the literal-operator assertions.
			check(`data.text:"QMQALFA" AND data.text:"OR"`, false, 0)
			check(`data.text:"AND" AND (data.text:"OR12345" OR data.text:"QMQALFA")`, true, 1)
			check(`data.text:"QMQALFA" AND data.text:"NOT"`, true, 2)
			check(`data.text:"1234567" AND data.text:"QMQALFA"`, true, 0)
			check(`data.text:"QMQALFA" AND `+mentionTerm("Z\u0301COMB999"), false, 2, 4)
			check(`data.text:"QMQALFA" AND data.text:"COMB999"`, false, 2, 4)
			check(mentionTerm(strings.ToUpper(token39)), true, 0)
			for _, dropped := range []string{token40, token41} {
				if scoped {
					// Tantivy removes the text clause, leaving the domain
					// alone. The builder never emits this query.
					check(mentionTerm(dropped), false, 0, 1, 2, 3, 4)
				} else {
					check(mentionTerm(dropped), false)
				}
			}
			if got := search(`data.text:"QMQBETA"`, 1); len(got) != 1 {
				t.Fatalf("explicit limit 1 returned %d hits", len(got))
			}
			t.Log("Memgraph 3.14 token boundary: 39 searchable, 40 and 41 dropped")
		})
	}
}

func TestProductMentionReadLifecycle(t *testing.T) {
	f := newPushFixtureWithFeature(t, "catalogue_code")
	ctx := t.Context()
	require := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	doc := func(hash, text string, done bool) string {
		t.Helper()
		out, err := f.store.UpsertDocument(ctx, DocumentInput{Kind: "text", Source: "product-mention-lifecycle", Domain: "mention-lifecycle", Hash: hash})
		require(err)
		t.Cleanup(func() {
			if err := f.store.DeleteDocument(context.Background(), out.ID); !errors.Is(err, ErrNotFound) {
				require(err)
			}
		})
		require(f.store.PutChunks(ctx, out.ID, []Chunk{{Text: text}}))
		if done {
			require(f.store.SetStatus(ctx, out.ID, StatusDone, ""))
		}
		return out.ID
	}
	old := doc("old", "AA12345; BB12345", true)
	f.snapshot(pushA, 1, "imported", pushRecord(pushOne, "Original", "null", `{"scalar":"AA12345"}`))
	lookup := ProductLookup{ID: productobs.ProductID(pushStore, pushOne)}
	count := countProductReadStatements(t, f.store)
	read := func(title, match string, docs ...string) {
		t.Helper()
		count.statements = nil
		count.deadlines = nil
		out, _, err := f.store.GetProductWithMentions(ctx, lookup)
		if title == "" {
			if !errors.Is(err, ErrNotFound) || !slices.Equal(count.statements, []int{1}) {
				t.Fatalf("missing product: %+v, %v, statements=%v", out, err, count.statements)
			}
			return
		}
		if err != nil || out.Title != title || out.MentionsError != "" || out.ProductMentionFields == nil || len(out.Mentions) != len(docs) {
			t.Fatalf("read: %+v %v, expected title %q and %d mentions", out, err, title, len(docs))
		}
		for i, mention := range out.Mentions {
			if mention.DocID != docs[i] || len(mention.Matched) != 1 || mention.Matched[0].Value != match {
				t.Fatalf("mention %d = %+v, expected doc %s value %s", i, mention, docs[i], match)
			}
		}
	}
	read("Original", "AA12345", old)
	if !slices.Equal(count.statements, []int{3, 4, 2}) {
		t.Fatalf("T0 includes optional URL page lookup: statements=%v", count.statements)
	}
	if count.deadlines[1].IsZero() || !count.deadlines[1].Equal(count.deadlines[2]) {
		t.Fatalf("T1 and T2 must share a deadline: %v", count.deadlines)
	}
	count.afterTx = func(n int) {
		if n == 0 {
			_, err := f.other.PushProduct(ctx, 2, []byte(pushRecord(pushOne, "Pushed", "null", `{"scalar":"BB12345"}`)))
			require(err)
		}
	}
	read("Original", "BB12345", old)
	count.afterTx = nil
	read("Pushed", "BB12345", old)
	newID := doc("new", "BB12345", false)
	count.afterTx = func(n int) {
		if n == 1 {
			require(f.other.Supersede(ctx, old, newID))
		}
	}
	read("Pushed", "")
	count.afterTx = nil
	require(f.store.SetStatus(ctx, newID, StatusDone, ""))
	read("Pushed", "BB12345", newID)
	require(f.store.DeleteDocument(ctx, newID))
	read("Pushed", "")
	current := doc("current", "BB12345", true)
	read("Pushed", "BB12345", current)
	_, err := f.store.RemoveProduct(ctx, pushOne, 3)
	require(err)
	read("", "")
	f.snapshot(pushA, 4, "imported", pushRecord(pushOne, "Recreated", "null", `{"scalar":"BB12345"}`))
	read("Recreated", "BB12345", current)
	// Simulate an unbound store with a surviving product, which the public
	// unbind operation prevents.
	const setStoreDomain = `MATCH (s:ProductStore {store:$store}) SET s.domain=$domain`
	f.query(setStoreDomain, map[string]any{"store": pushStore, "domain": nil})
	out, _, err := f.store.GetProductWithMentions(ctx, lookup)
	if err != nil || out.Title != "Recreated" || out.MentionsError != "" || out.ProductMentionFields == nil ||
		out.Mentions == nil || len(out.Mentions) != 0 || out.MentionsTotal != 0 || out.MentionsKeys != 0 || out.MentionsPartial {
		t.Fatalf("expected empty mentions for surviving product of unbound store: %+v, %v", out, err)
	}
	f.query(setStoreDomain, map[string]any{"store": pushStore, "domain": pushDomain})
	require(f.store.DeleteProductStore(ctx, pushStore))
	read("", "")
	require(f.store.UnbindProductStore(ctx, pushStore))
	read("", "")
	require(f.store.runProductWrite(ctx, func(tx neo4j.ManagedTransaction) error {
		return txBindProductStore(ctx, tx, pushStore, "push-rebound")
	}))
	f.seedRelease("mention-rebound", "push-rebound")
	f.activate("mention-rebound")
	read("", "")
	f.snapshot("mention-rebound", 5, "imported", pushRecord(pushOne, "Rebound", "null", `{"scalar":"BB12345"}`))
	read("Rebound", "BB12345", current)
}
