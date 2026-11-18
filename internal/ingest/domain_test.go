package ingest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/domain"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
	"github.com/alessiosavi/go-knowledge-graph/internal/obs"
)

type detectProvider struct {
	mu         sync.Mutex
	ids        []string
	calls      int
	secondPass bool
}

func (*detectProvider) Name() string    { return "detect-test" }
func (*detectProvider) Version() string { return "detect-test:1" }
func (p *detectProvider) Extract(_ context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	var out []extract.ChunkResult
	for _, c := range req.Chunks {
		p.ids = append(p.ids, c.ID)
		names := []string{"Acme-Max", "Zorbo", "Quillon"}
		if strings.Contains(c.Text, "Elsewhere") {
			names = []string{"Flimble", "Wexora", "Trindle"}
		}
		if p.secondPass {
			names = []string{"Quillon Ltd", "Descrizione"}
		}
		if strings.Contains(c.Text, "Empty") {
			names = nil
		}
		r := extract.ChunkResult{ChunkID: c.ID}
		for _, name := range names {
			r.Mentions = append(r.Mentions, extract.Mention{Text: name, Label: "organization", Score: .9, End: len(name)})
		}
		r.Candidates = []extract.Candidate{{Head: 0, Tail: 1, Label: "uses", Score: .9}}
		out = append(out, r)
	}
	return out, nil
}

func detectionEnv(t *testing.T, change func(*config.Config)) (pipelineEnv, *detectProvider) {
	t.Helper()
	env := newPipelineEnv(t)
	env.cfg.Ingest.ChunkDedup = "exact"
	env.cfg.Ingest.ChunkWords = 100
	env.cfg.Labels.PresetsDir = "../../labels"
	if change != nil {
		change(&env.cfg)
	}
	prov := &detectProvider{}
	env.p = ingest.NewPipeline(env.cfg, env.st, prov, env.ix, textLoader)
	env.p.SetWriteMode(ingest.WriteMode{Domains: true, Hygiene: true})
	return env, prov
}

func detectSubmit(t *testing.T, env pipelineEnv, suffix, text, preset string) string {
	t.Helper()
	res, err := env.p.Submit(context.Background(), ingest.Input{Kind: "text", Source: strings.ReplaceAll(t.Name(), "/", "-") + "-" + suffix, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	uri := os.Getenv(graphtest.URIEnv)
	t.Cleanup(func() {
		if err := env.st.DeleteDocument(context.Background(), res.DocumentID); err != nil {
			t.Error(err)
		}
		rawRecords(t, uri, fmt.Sprintf(`MATCH (n:DomainDecision {doc_id:'%s'}) DETACH DELETE n RETURN count(*)`, res.DocumentID))
		rawRecords(t, uri, fmt.Sprintf(`MATCH (n:Domain {id:'%s'}) DETACH DELETE n RETURN count(*)`, domain.NewDomainID(res.DocumentID)))
	})
	if preset != "" {
		rawRecords(t, uri, fmt.Sprintf(`MATCH (d:Document {id:'%s'}) SET d.domain='%s', d.domain_tok='%s' RETURN d.id`, res.DocumentID, preset, domain.DomainToken(preset)))
	}
	return res.DocumentID
}

func detectProcess(t *testing.T, env pipelineEnv, id string) *graph.Document {
	t.Helper()
	if err := env.p.Process(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	d, err := env.st.Document(context.Background(), id)
	if err != nil || d.Domain == "" || d.Status != graph.StatusDone {
		t.Fatalf("document=%+v error=%v", d, err)
	}
	return d
}

func storedProfile(t *testing.T, env pipelineEnv, id string) map[string]any {
	t.Helper()
	r := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (d:Document {id:'%s'}) RETURN d.name_profile AS p`, id))
	v, _ := r[0].Get("p")
	p, ok := v.(map[string]any)
	if !ok || len(p) == 0 {
		t.Fatalf("non-empty profile missing: %#v", v)
	}
	return p
}

func TestDetectAttachAndNew(t *testing.T) {
	env, _ := detectionEnv(t, nil)
	var slug string
	for i := 0; i < 3; i++ {
		id := detectSubmit(t, env, fmt.Sprint(i), fmt.Sprintf("Acme-Max explores Zorbo with Quillon number %d.", i), "")
		d := detectProcess(t, env, id)
		if i == 0 {
			slug = d.Domain
		} else if d.Domain != slug {
			t.Fatalf("domain=%s want %s", d.Domain, slug)
		}
		storedProfile(t, env, id)
		rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (r:DomainDecision {doc_id:'%s'}) RETURN r.action AS action,r.top AS top,r.params AS params,r.by AS by,r.op_id AS op,r.at AS at`, id))
		if len(rows) != 1 {
			t.Fatalf("ledger entries=%d", len(rows))
		}
		wantAction := "attach"
		if i == 0 {
			wantAction = "new"
		}
		if testString(rows[0], "action") != wantAction {
			t.Errorf("ledger action=%q, want %q", testString(rows[0], "action"), wantAction)
		}
		params, _ := rows[0].Get("params")
		if !reflect.DeepEqual(params, map[string]any{"tau": .05, "delta": .02, "tau_override": .15}) {
			t.Errorf("params=%#v", params)
		}
		top, _ := rows[0].Get("top")
		if v, ok := top.([]any); !ok || len(v) > 3 || (i > 0 && len(v) == 0) {
			t.Errorf("top=%#v", top)
		}
		if testString(rows[0], "by") != "auto" || testString(rows[0], "op") == "" || testString(rows[0], "at") == "" {
			t.Errorf("ledger metadata=%+v", rows[0])
		}
	}
	id := detectSubmit(t, env, "other", "Elsewhere Flimble explores Wexora and Trindle.", "")
	if d := detectProcess(t, env, id); d.Domain == slug {
		t.Fatal("unrelated document attached")
	}
}

func TestConcurrentNewCorpusOneDomain(t *testing.T) {
	env, _ := detectionEnv(t, nil)
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = detectSubmit(t, env, fmt.Sprint(i), fmt.Sprintf("Acme-Max follows Zorbo through Quillon sector %d.", i), "")
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	start := make(chan struct{})
	for _, id := range ids {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; errs <- env.p.Process(context.Background(), id) }()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var slug string
	for _, id := range ids {
		d, err := env.st.Document(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if slug == "" {
			slug = d.Domain
		}
		if d.Domain != slug || slug == "" {
			t.Fatalf("domain=%q want %q", d.Domain, slug)
		}
	}
}

func TestCrossDomainDuplicateExtracted(t *testing.T) {
	env, prov := detectionEnv(t, nil)
	text := "Acme-Max explores Zorbo with Quillon in the archive."
	a := detectSubmit(t, env, "a", text, "")
	detectProcess(t, env, a)
	b := detectSubmit(t, env, "b", text, "quillon-other")
	detectProcess(t, env, b)
	chunks, err := env.st.Chunks(context.Background(), b)
	if err != nil || len(chunks) != 1 || chunks[0].DuplicateOf != "" {
		t.Fatalf("chunks=%+v err=%v", chunks, err)
	}
	if len(prov.ids) != 2 {
		t.Fatalf("extracted chunks=%v", prov.ids)
	}
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (:Chunk {doc_id:'%s'})-[:MENTIONS]->(e:Entity) RETURN e.domain AS domain`, b))
	if len(rows) != 3 {
		t.Fatalf("mentions=%d", len(rows))
	}
	for _, r := range rows {
		if testString(r, "domain") != "quillon-other" {
			t.Errorf("cross-domain entity=%+v", r)
		}
	}
	storedProfile(t, env, b)
}

func TestSameDocumentDuplicateKeepsExtraction(t *testing.T) {
	for _, tc := range []struct {
		name         string
		confirmation bool
		preset       string
	}{
		{name: "facts"},
		{name: "candidates", confirmation: true},
		{name: "assigned", preset: "quillon-local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, prov := detectionEnv(t, func(cfg *config.Config) { cfg.Ingest.ChunkWords = 5 })
			env.p.SetWriteMode(ingest.WriteMode{Domains: true, RequireConfirmation: tc.confirmation})
			const sentence = "Acme-Max visits Zorbo with Quillon."
			id := detectSubmit(t, env, "duplicate", sentence+" "+sentence, tc.preset)
			detectProcess(t, env, id)
			chunks, err := env.st.Chunks(context.Background(), id)
			if err != nil || len(chunks) != 2 {
				t.Fatalf("chunks=%+v err=%v", chunks, err)
			}
			if chunks[1].DuplicateOf != chunks[0].ID {
				t.Errorf("duplicate_of=%q, want %q", chunks[1].DuplicateOf, chunks[0].ID)
			}
			if prov.calls != 1 || !reflect.DeepEqual(prov.ids, []string{chunks[0].ID}) {
				t.Errorf("provider calls=%d chunks=%v, want one call for root only", prov.calls, prov.ids)
			}
			want := map[string]any{"acmemax": int64(2), "zorbo": int64(2), "quillon": int64(2)}
			if got := storedProfile(t, env, id); !reflect.DeepEqual(got, want) {
				t.Errorf("profile=%v, want %v", got, want)
			}
			uri := os.Getenv(graphtest.URIEnv)
			for i, c := range chunks {
				wantMentions, wantFacts, wantCandidates := int64(0), int64(0), int64(0)
				if i == 0 {
					wantMentions = 3
					if tc.confirmation {
						wantCandidates = 1
					} else {
						wantFacts = 1
					}
				}
				for _, check := range []struct {
					query string
					want  int64
				}{
					{fmt.Sprintf(`MATCH (:Chunk {id:'%s'})-[r:MENTIONS]->(:Entity) RETURN count(r) AS n`, c.ID), wantMentions},
					{fmt.Sprintf(`MATCH (:Entity)-[r]->(:Entity) WHERE r.chunk_id='%s' RETURN count(r) AS n`, c.ID), wantFacts},
					{fmt.Sprintf(`MATCH (n:FactCandidate {chunk_id:'%s'}) RETURN count(n) AS n`, c.ID), wantCandidates},
				} {
					if got := rawCount(t, uri, check.query); got != check.want {
						t.Errorf("%s = %d, want %d", check.query, got, check.want)
					}
				}
			}
		})
	}
}

func TestDuplicateChainRoot(t *testing.T) {
	env, prov := detectionEnv(t, nil)
	text := "Acme-Max explores Zorbo with Quillon in the archive."
	a := detectSubmit(t, env, "a", text, "")
	ad := detectProcess(t, env, a)
	b := detectSubmit(t, env, "b", text, "")
	detectProcess(t, env, b)
	c := detectSubmit(t, env, "c", text, "")
	// Recovery fixture: C points to B, whose chunk is a duplicate of A.
	if err := env.st.PutChunks(context.Background(), c, []graph.Chunk{{Seq: 0, Text: text, DuplicateOf: b + "-c0"}}); err != nil {
		t.Fatal(err)
	}
	if err := env.st.SetStatus(context.Background(), c, graph.StatusChunked, ""); err != nil {
		t.Fatal(err)
	}
	if cd := detectProcess(t, env, c); cd.Domain != ad.Domain {
		t.Fatalf("domain=%s want %s", cd.Domain, ad.Domain)
	}
	if len(prov.ids) != 1 {
		t.Fatalf("borrowable duplicates extracted: %v", prov.ids)
	}
	if got := storedProfile(t, env, c); got["acmemax"] != int64(1) {
		t.Errorf("profile=%v", got)
	}
}

func TestBorrowedDuplicateProfileExcludesDocumentScopedDate(t *testing.T) {
	env, prov := detectionEnv(t, nil)
	ctx := context.Background()
	const text = "Acme-Max explores Zorbo with Quillon on 2042-03-04."
	root := detectSubmit(t, env, "root", text, "")
	rootDoc := detectProcess(t, env, root)
	// Replace the root extraction with a global name and a document-scoped
	// date, as a preset with scope: document would write them.
	ents := []graph.EntityUpsert{
		{Key: graph.DomainKey(rootDoc.Domain, graph.EntityKey("Acme-Max")), Name: "Acme-Max", Type: "organization", Mentions: []graph.MentionSpan{{Start: 0, End: 8}}},
		{Key: graph.ScopedEntityKey(root, "2042-03-04"), Name: "2042-03-04", Type: "date", Mentions: []graph.MentionSpan{{Start: 40, End: 50}}},
	}
	if err := env.st.UpsertExtractionOpts(ctx, root, root+"-c0", "mock:1", ents, nil, graph.WriteOptions{Domain: rootDoc.Domain}); err != nil {
		t.Fatal(err)
	}
	borrower := detectSubmit(t, env, "borrower", text, "")
	if d := detectProcess(t, env, borrower); d.Domain != rootDoc.Domain {
		t.Errorf("borrower domain=%q, want %q", d.Domain, rootDoc.Domain)
	}
	chunks, err := env.st.Chunks(ctx, borrower)
	if err != nil || len(chunks) != 1 || chunks[0].DuplicateOf != root+"-c0" {
		t.Fatalf("borrower chunks=%+v err=%v, want borrowed root", chunks, err)
	}
	if !reflect.DeepEqual(prov.ids, []string{root + "-c0"}) {
		t.Errorf("provider chunks=%v, want extraction of root only", prov.ids)
	}
	if got, want := storedProfile(t, env, borrower), (map[string]any{"acmemax": int64(1)}); !reflect.DeepEqual(got, want) {
		t.Errorf("borrower profile=%v, want %v (no document-scoped date)", got, want)
	}
}

func TestSecondPassHygiene(t *testing.T) {
	env, prov := detectionEnv(t, nil)
	text := "Acme-Max explores Zorbo with Quillon in the archive."
	a := detectSubmit(t, env, "a", text, "")
	detectProcess(t, env, a)
	prov.secondPass = true
	metrics := obs.New()
	env.p.SetMetrics(metrics)
	b := detectSubmit(t, env, "b", text, "quillon-second")
	// One fresh chunk and one duplicate exercise both provider passes and the
	// aggregation of their hygiene counters and per-chunk frequencies.
	if err := env.st.PutChunks(context.Background(), b, []graph.Chunk{
		{Seq: 0, Text: text, DuplicateOf: a + "-c0"},
		{Seq: 1, Text: "Quillon Ltd uses Descrizione in another sentence."},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.st.SetStatus(context.Background(), b, graph.StatusChunked, ""); err != nil {
		t.Fatal(err)
	}
	detectProcess(t, env, b)
	if got := storedProfile(t, env, b); !reflect.DeepEqual(got, map[string]any{"quillon": int64(2)}) {
		t.Errorf("second-pass profile=%v", got)
	}
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (d:Document {id:'%s'}) RETURN d.hygiene AS h`, b))
	h, _ := rows[0].Get("h")
	if counts, ok := h.(map[string]any); !ok || counts["stoplist"] != int64(2) || counts["renamed"] != int64(2) {
		t.Errorf("hygiene=%#v", h)
	}
	if n := rawCount(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (:Entity)-[r]->(:Entity) WHERE r.doc_id='%s' RETURN count(r) AS n`, b)); n != 0 {
		t.Errorf("dropped endpoint retained %d facts", n)
	}
	if scrape := gather(t, metrics); !strings.Contains(scrape, `gkg_hygiene_dropped_total{rule="stoplist"} 2`) || !strings.Contains(scrape, "gkg_hygiene_renamed_total 2") {
		t.Errorf("metrics=%s", scrape)
	}
	if !reflect.DeepEqual(prov.ids, []string{a + "-c0", b + "-c1", b + "-c0"}) {
		t.Errorf("provider passes=%v", prov.ids)
	}
}

func TestBrokenDuplicateRootsExtracted(t *testing.T) {
	for _, kind := range []string{"missing", "cycle", "unfinished"} {
		t.Run(kind, func(t *testing.T) {
			env, prov := detectionEnv(t, nil)
			id := detectSubmit(t, env, "root", "Acme-Max visits Quillon and Zorbo.", "quillon-roots")
			root := "missing-chunk"
			if kind == "cycle" {
				root = id + "-c1"
			}
			if kind == "unfinished" {
				other := detectSubmit(t, env, "unfinished", "Quillon visits Acme-Max and Zorbo.", "quillon-roots")
				if err := env.st.PutChunks(context.Background(), other, []graph.Chunk{{Text: "Quillon visits Acme-Max and Zorbo."}}); err != nil {
					t.Fatal(err)
				}
				if err := env.st.SetStatus(context.Background(), other, graph.StatusChunked, ""); err != nil {
					t.Fatal(err)
				}
				root = other + "-c0"
			}
			chunks := []graph.Chunk{{Seq: 0, Text: "Acme-Max visits Quillon and Zorbo.", DuplicateOf: root}}
			if kind == "cycle" {
				back := id + "-c0"
				chunks = append(chunks, graph.Chunk{Seq: 1, Text: "Quillon visits Acme-Max and Zorbo.", DuplicateOf: back})
			}
			if err := env.st.PutChunks(context.Background(), id, chunks); err != nil {
				t.Fatal(err)
			}
			if err := env.st.SetStatus(context.Background(), id, graph.StatusChunked, ""); err != nil {
				t.Fatal(err)
			}
			detectProcess(t, env, id)
			got, err := env.st.Chunks(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range got {
				if c.DuplicateOf != "" {
					t.Errorf("unusable duplicate retained: %+v", c)
				}
			}
			if len(prov.ids) != len(chunks) {
				t.Errorf("extracted=%v, want %d chunks", prov.ids, len(chunks))
			}
			storedProfile(t, env, id)
		})
	}
}

func TestReingestInherits(t *testing.T) {
	env, _ := detectionEnv(t, nil)
	a := detectSubmit(t, env, "version", "Acme-Max explores Zorbo with Quillon.", "")
	ad := detectProcess(t, env, a)
	// A different established corpus is the detector's confident proposal.
	other := detectSubmit(t, env, "other", "Elsewhere Flimble explores Wexora with Trindle.", "")
	od := detectProcess(t, env, other)
	b := detectSubmit(t, env, "version", "Elsewhere Flimble now visits Wexora and Trindle.", "")
	if bd := detectProcess(t, env, b); bd.Domain != ad.Domain {
		t.Fatalf("domain=%s want inherited %s", bd.Domain, ad.Domain)
	}
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (r:DomainDecision {doc_id:'%s'}) RETURN r.action AS action,r.domain AS domain,r.previous_domain AS previous ORDER BY action`, b))
	if len(rows) != 2 || testString(rows[0], "action") != "inherit" || testString(rows[1], "action") != "proposed-move" || testString(rows[1], "domain") != od.Domain || testString(rows[1], "previous") != ad.Domain {
		t.Fatalf("inherit/proposal=%+v", rows)
	}
}

func TestProposedMoveFailureDoesNotFailIngest(t *testing.T) {
	env, _ := detectionEnv(t, nil)
	a := detectSubmit(t, env, "version", "Acme-Max explores Zorbo with Quillon.", "")
	ad := detectProcess(t, env, a)
	other := detectSubmit(t, env, "other", "Elsewhere Flimble explores Wexora with Trindle.", "")
	od := detectProcess(t, env, other)
	b := detectSubmit(t, env, "version", "Elsewhere Flimble now visits Wexora and Trindle.", "")
	var logs bytes.Buffer
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(logger) })
	proposalAttempted := false
	ingest.SetBeforeDomainAssignment(env.p, func(a graph.Assignment) error {
		if a.Decision.Action != "proposed-move" {
			return nil
		}
		proposalAttempted = true
		if a.Decision.Domain != od.Domain || a.Decision.PreviousDomain != ad.Domain {
			t.Errorf("proposal=%+v", a.Decision)
		}
		return errors.New("injected proposal write failure")
	})
	if d := detectProcess(t, env, b); d.Domain != ad.Domain {
		t.Errorf("domain=%q, want inherited %q", d.Domain, ad.Domain)
	}
	if !proposalAttempted {
		t.Fatal("proposal failure was not exercised")
	}
	rows := rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (r:DomainDecision {doc_id:'%s'}) RETURN r.action AS action,r.domain AS domain`, b))
	if len(rows) != 1 || testString(rows[0], "action") != "inherit" || testString(rows[0], "domain") != ad.Domain {
		t.Errorf("committed inheritance=%+v", rows)
	}
	storedProfile(t, env, b)
	if output := logs.String(); !strings.Contains(output, "level=WARN") || !strings.Contains(output, "proposed-move") || !strings.Contains(output, b) || !strings.Contains(output, "injected proposal write failure") {
		t.Errorf("missing proposal warning: %s", output)
	}
}

func TestInheritedAssignmentProfileExcludesCrossDomainRoots(t *testing.T) {
	env, prov := detectionEnv(t, nil)
	const ownText = "Acme-Max explores Zorbo with Quillon."
	const foreignText = "Elsewhere Flimble explores Wexora with Trindle."
	a := detectSubmit(t, env, "version", ownText, "")
	ad := detectProcess(t, env, a)
	other := detectSubmit(t, env, "other", foreignText, "")
	od := detectProcess(t, env, other)
	if ad.Domain == od.Domain {
		t.Fatal("fixture domains must differ")
	}
	b := detectSubmit(t, env, "version", ownText+" "+foreignText, "")
	if err := env.st.PutChunks(context.Background(), b, []graph.Chunk{
		{Seq: 0, Text: "Acme-Max revisits Zorbo with Quillon."},
		{Seq: 1, Text: ownText, DuplicateOf: a + "-c0"},
		{Seq: 2, Text: foreignText, DuplicateOf: other + "-c0"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.st.SetStatus(context.Background(), b, graph.StatusChunked, ""); err != nil {
		t.Fatal(err)
	}
	observed := false
	ingest.SetBeforeDomainWrite(env.p, func(id string) {
		if id != b || observed {
			return
		}
		observed = true
		want := map[string]any{"acmemax": int64(2), "zorbo": int64(2), "quillon": int64(2)}
		if got := storedProfile(t, env, b); !reflect.DeepEqual(got, want) {
			t.Errorf("assignment profile=%v, want only own-domain names %v", got, want)
		}
	})
	if d := detectProcess(t, env, b); d.Domain != ad.Domain {
		t.Errorf("domain=%q, want inherited %q", d.Domain, ad.Domain)
	}
	if !observed {
		t.Fatal("assignment profile was not observed before final refresh")
	}
	want := map[string]any{"acmemax": int64(2), "zorbo": int64(2), "quillon": int64(2), "flimble": int64(1), "wexora": int64(1), "trindle": int64(1)}
	if got := storedProfile(t, env, b); !reflect.DeepEqual(got, want) {
		t.Errorf("final profile=%v, want %v", got, want)
	}
	chunks, err := env.st.Chunks(context.Background(), b)
	if err != nil || len(chunks) != 3 || chunks[1].DuplicateOf != a+"-c0" || chunks[2].DuplicateOf != "" {
		t.Errorf("chunks=%+v err=%v", chunks, err)
	}
	if !reflect.DeepEqual(prov.ids, []string{a + "-c0", other + "-c0", b + "-c0", b + "-c2"}) {
		t.Errorf("provider chunks=%v", prov.ids)
	}
}

func TestReingestKeepsMovedDomain(t *testing.T) {
	env, _ := detectionEnv(t, nil)
	a := detectSubmit(t, env, "version", "Acme-Max explores Zorbo with Quillon.", "")
	detectProcess(t, env, a)
	rawRecords(t, os.Getenv(graphtest.URIEnv), fmt.Sprintf(`MATCH (d:Document {id:'%s'}) SET d.domain='quillon-moved',d.domain_tok='%s' RETURN d.id`, a, domain.DomainToken("quillon-moved")))
	// Same-id forced re-ingest and then a changed version both retain the move.
	res, err := env.p.Submit(context.Background(), ingest.Input{Kind: "text", Source: strings.ReplaceAll(t.Name(), "/", "-") + "-version", Text: "Acme-Max explores Zorbo with Quillon.", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := detectProcess(t, env, res.DocumentID); d.Domain != "quillon-moved" {
		t.Fatalf("forced domain=%q", d.Domain)
	}
	b := detectSubmit(t, env, "version", "Acme-Max revisits Zorbo with Quillon.", "")
	if d := detectProcess(t, env, b); d.Domain != "quillon-moved" {
		t.Fatalf("new version domain=%q", d.Domain)
	}
}

func TestReingestInheritsNewestLiveDomain(t *testing.T) {
	env, _ := detectionEnv(t, nil)
	a := detectSubmit(t, env, "version", "Acme-Max visits Quillon first.", "quillon-older")
	detectProcess(t, env, a)
	// Publish a newer live version without superseding the first, as can
	// happen while versions are queued. Its assignment must win inheritance.
	b := detectSubmit(t, env, "version", "Acme-Max visits Quillon second.", "quillon-newer")
	if err := env.st.SetStatus(context.Background(), b, graph.StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	c := detectSubmit(t, env, "version", "Acme-Max visits Quillon third.", "")
	if d := detectProcess(t, env, c); d.Domain != "quillon-newer" {
		t.Errorf("domain=%q, want newest live version's domain", d.Domain)
	}
}

func TestDomainPartialConfigKeepsPrefixPrior(t *testing.T) {
	env, _ := detectionEnv(t, func(cfg *config.Config) { cfg.Domain = config.DomainConfig{Tau: .07, Delta: .03} })
	uri := os.Getenv(graphtest.URIEnv)
	for i := 0; i < 2; i++ {
		id := detectSubmit(t, env, fmt.Sprint(i), "Acme-Max visits Zorbo.", "z-prior")
		detectProcess(t, env, id)
		rawRecords(t, uri, fmt.Sprintf(`MATCH (d:Document {id:'%s'}) SET d.source='https://quillon.example/%d' RETURN d.id`, id, i))
	}
	other := detectSubmit(t, env, "other", "Elsewhere Flimble visits Wexora.", "a-zero")
	detectProcess(t, env, other)
	id := detectSubmit(t, env, "target", "An empty profile will exercise the prior.", "")
	rawRecords(t, uri, fmt.Sprintf(`MATCH (d:Document {id:'%s'}) SET d.source='https://quillon.example/target',d.status='chunked' RETURN d.id`, id))
	// A chunk without extracted names gives every domain score zero.
	if err := env.st.PutChunks(context.Background(), id, []graph.Chunk{{Text: "Empty extraction contains no names."}}); err != nil {
		t.Fatal(err)
	}
	if d := detectProcess(t, env, id); d.Domain != "z-prior" {
		t.Fatalf("zero-score override chose %q", d.Domain)
	}
	rows := rawRecords(t, uri, fmt.Sprintf(`MATCH (r:DomainDecision {doc_id:'%s'}) RETURN r.params AS params,r.action AS action`, id))
	params, _ := rows[0].Get("params")
	if !reflect.DeepEqual(params, map[string]any{"tau": .07, "delta": .03, "tau_override": .15}) || testString(rows[0], "action") != "prefix" {
		t.Errorf("decision=%+v", rows)
	}
}

func TestConcurrentDuplicateNewCorpus(t *testing.T) {
	env, prov := detectionEnv(t, nil)
	text := "Acme-Max explores Zorbo with Quillon in a new archive."
	a := detectSubmit(t, env, "a", text, "")
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	ingest.SetBeforeDomainWrite(env.p, func(id string) {
		if id == a {
			close(held)
			<-release
		}
	})
	done := make(chan error, 1)
	go func() { done <- env.p.Process(context.Background(), a) }()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("A failed before write barrier: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("write barrier timeout")
	}
	b := detectSubmit(t, env, "b", text, "")
	bd := detectProcess(t, env, b)
	storedProfile(t, env, b)
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	ad, err := env.st.Document(context.Background(), a)
	if err != nil || ad.Domain != bd.Domain {
		t.Fatalf("A=%+v B=%+v err=%v", ad, bd, err)
	}
	if len(prov.ids) != 2 {
		t.Fatalf("unfinished duplicate skipped: %v", prov.ids)
	}
}

// datedProvider gives two unrelated documents disjoint organization names
// that share one document-scoped date value.
type datedProvider struct{}

func (datedProvider) Name() string    { return "dated-test" }
func (datedProvider) Version() string { return "dated-test:1" }
func (datedProvider) Extract(_ context.Context, req extract.Request) ([]extract.ChunkResult, error) {
	var out []extract.ChunkResult
	for _, c := range req.Chunks {
		orgs := []string{"Alphawick", "Alphorne", "Alvenmoor"}
		if strings.Contains(c.Text, "Brindle") {
			orgs = []string{"Brindlecor", "Brostavik", "Burlewen"}
		}
		r := extract.ChunkResult{ChunkID: c.ID}
		for _, name := range orgs {
			r.Mentions = append(r.Mentions, extract.Mention{Text: name, Label: "organization", Score: .9, End: len(name)})
		}
		r.Mentions = append(r.Mentions, extract.Mention{Text: "2026", Label: "date", Score: .9, End: 4})
		out = append(out, r)
	}
	return out, nil
}

// TestProfileSkipsDocumentScopedValues: document-scoped values (dates,
// amounts) are not domain vocabulary, so two unrelated documents that share
// only a date must not land in the same domain, and the stored profile has
// no date.
func TestProfileSkipsDocumentScopedValues(t *testing.T) {
	env, _ := detectionEnv(t, nil)
	env.p = ingest.NewPipeline(env.cfg, env.st, datedProvider{}, env.ix, textLoader)
	env.p.SetWriteMode(ingest.WriteMode{Domains: true, Hygiene: true})
	ctx := context.Background()
	uri := os.Getenv(graphtest.URIEnv)
	var domains []string
	for i, text := range []string{"Alphawick meets Alphorne in 2026.", "Brindlecor meets Brostavik in 2026."} {
		res, err := env.p.Submit(ctx, ingest.Input{Kind: "text", Source: fmt.Sprintf("%s-%d", strings.ReplaceAll(t.Name(), "/", "-"), i), Text: text, Labels: "mail"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = env.st.DeleteDocument(context.Background(), res.DocumentID)
			rawRecords(t, uri, fmt.Sprintf(`MATCH (n:DomainDecision {doc_id:'%s'}) DETACH DELETE n RETURN count(*)`, res.DocumentID))
			rawRecords(t, uri, fmt.Sprintf(`MATCH (n:Domain {id:'%s'}) DETACH DELETE n RETURN count(*)`, domain.NewDomainID(res.DocumentID)))
		})
		d := detectProcess(t, env, res.DocumentID)
		domains = append(domains, d.Domain)
		if p := storedProfile(t, env, res.DocumentID); p["2026"] != nil {
			t.Errorf("profile of %s holds the document-scoped date: %v", text, p)
		}
	}
	if domains[0] == domains[1] {
		t.Fatalf("documents sharing only a date attached to one domain %q", domains[0])
	}
}
