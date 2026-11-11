// Package ingest implements the document → knowledge-graph pipeline:
// decode, chunk, near-duplicate detection, extraction, confidence floors
// and the provenance merge — plus the in-process worker pool (job state
// lives on :Document nodes, so a restart resumes in-flight documents).
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alessiosavi/GoGPUtils/minhash"
	"go.yaml.in/yaml/v4"

	"github.com/alessiosavi/go-knowledge-graph/internal/chunk"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/dedup"
	"github.com/alessiosavi/go-knowledge-graph/internal/extract"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// Loaded is the decoded document handed over by the text loader.
type Loaded struct {
	Text  string
	Title string
	Hash  string
}

// TextLoader decodes an input into cleaned text. T8 uses a stub; T11 wires
// the real ingest.Decode (files, URL bodies, PDF).
type TextLoader func(ctx context.Context, in Input) (Loaded, error)

// Input is one submission (text, file bytes or URL).
type Input struct {
	Kind   string // url | file | text; inferred from the fields when empty
	URL    string
	Source string
	Title  string
	Bytes  []byte
	Text   string
	Labels string
	Force  bool
}

// Result tells the caller what a submission did.
type Result struct {
	DocumentID string
	Noop       bool
}

// ErrStore marks a submission failure caused by the graph store (an
// infrastructure problem) rather than the submitted input, so callers can
// map it to a 5xx. Wrapped store errors keep their own chain: errors.Is
// finds both this sentinel and the underlying error.
var ErrStore = errors.New("ingest: store failure")

// ErrBusy marks a document whose processing lock is held. Admissions that
// need to replace its payload must retry after the worker releases it.
var ErrBusy = errors.New("ingest: document is busy; retry later")

// Default label sets (sidecar starting points per the design: entity 0.4,
// relation 0.25). Named presets from Labels.PresetsDir override them.
var (
	defaultEntityLabels = []extract.Label{
		{Name: "person", Threshold: 0.4},
		{Name: "organization", Threshold: 0.4},
		{Name: "location", Threshold: 0.4},
		{Name: "product", Threshold: 0.4},
		{Name: "technology", Threshold: 0.35},
		{Name: "work", Threshold: 0.35},
		{Name: "event", Threshold: 0.4},
	}
	defaultRelationLabels = []extract.Label{
		{Name: "works_at", Threshold: 0.25},
		{Name: "located_in", Threshold: 0.25},
		{Name: "part_of", Threshold: 0.25},
		{Name: "uses", Threshold: 0.25},
		{Name: "wrote", Threshold: 0.25},
	}
)

const jobsBuffer = 1024

// presetNameRe restricts label preset names (they reach the filesystem).
var presetNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// labelNameRe requires alphanumeric content in every label name.
var labelNameRe = regexp.MustCompile(`[a-zA-Z0-9]`)

type submission struct {
	loaded Loaded
	labels string
}

// pipelineStore is the graph surface used by the pipeline. Production uses
// graph.Store; tests can exercise durable admission/recovery without a server.
type pipelineStore interface {
	UpsertDocument(context.Context, graph.DocumentInput) (graph.UpsertResult, error)
	Document(context.Context, string) (*graph.Document, error)
	InFlight(context.Context) ([]graph.Document, error)
	SetStatus(context.Context, string, graph.Status, string) error
	DeleteChunksFrom(context.Context, string, int) error
	PutChunks(context.Context, string, []graph.Chunk) error
	Chunks(context.Context, string) ([]graph.Chunk, error)
	PutFingerprint(context.Context, string, dedup.FP) error
	UpsertExtraction(context.Context, string, string, string, []graph.EntityUpsert, []graph.FactUpsert) error
	WriteFactAudit(context.Context, graph.FactAudit) error
	SetFactJudge(context.Context, string, string, string, string, string) error
	RejectFact(context.Context, string, string, string, string, graph.FactAudit) error
	EnsureVectorIndex(context.Context, int) error
	SetEntityEmbeddings(context.Context, []string, [][]float64) (int, error)
	OlderLiveVersions(context.Context, string) ([]string, error)
	Supersede(context.Context, string, string) error
	SameIdentityDocumentIDs(context.Context, string, string) (map[string]bool, error)
	ChunkRefsByExactKey(context.Context, string, []string, string) ([]graph.ChunkRef, error)
	ChunkByID(context.Context, string) (graph.Chunk, error)
}

// Metrics receives the pipeline counters and latencies; nil means
// disabled. It is structurally implemented by obs.Metrics.
type Metrics interface {
	DocumentStatus(status string)
	Chunks(n int)
	Entities(n int)
	Facts(n int)
	FactsDropped(n int)
	ExtractorError()
	ExtractSeconds(seconds float64)
	JudgeVerdict(status string)
	JudgeFailure()
	JudgeSeconds(seconds float64)
}

// Embedder embeds batch texts as documents (structurally satisfied by
// embed.Provider); nil means embeddings are disabled.
type Embedder interface {
	EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error)
}

// embedBatch caps one embedding call.
const embedBatch = 64

// Pipeline runs one document submission end to end.
type Pipeline struct {
	cfg   config.Config
	store pipelineStore
	prov  extract.Provider
	ix    *dedup.Index
	load  TextLoader
	spool *submissionSpool

	jobs chan string

	mu      sync.Mutex
	pending map[string]submission

	procLocks sync.Map // docID → *sync.Mutex, serializes Process per document

	judge   FactJudge
	emb     Embedder
	metrics Metrics

	beforeSupersede func(olds []string) // Test hook; nil in production.
}

// NewPipeline wires the pipeline. ix may be nil (near-duplicate detection
// degrades to exact-only); load may be nil only when every Process call is
// covered by the in-memory cache.
func NewPipeline(cfg config.Config, st *graph.Store, prov extract.Provider, ix *dedup.Index, load TextLoader) *Pipeline {
	return &Pipeline{
		cfg:     cfg,
		store:   st,
		prov:    prov,
		ix:      ix,
		load:    load,
		jobs:    make(chan string, jobsBuffer),
		pending: make(map[string]submission),
		spool:   &submissionSpool{dir: cfg.Ingest.SpoolDir, maxCount: cfg.Ingest.SpoolMaxCount, maxBytes: int64(cfg.Ingest.SpoolMaxBytes)},
	}
}

// Submit resolves the identity, decodes the content, applies the hash no-op
// (bypassed by Force) and queues the document for processing.
func (p *Pipeline) Submit(ctx context.Context, in Input) (Result, error) {
	// Fail fast on an unusable label preset instead of failing the document
	// later (the name is persisted with the document for recovery).
	if _, _, err := p.labelsFor(in.Labels); err != nil {
		return Result{}, err
	}
	loaded, err := p.loadInput(ctx, in)
	if err != nil {
		return Result{}, fmt.Errorf("ingest: load: %w", err)
	}
	hash := sha256Hex(loaded.Text)
	kind, source, err := resolveIdentity(in, hash)
	if err != nil {
		return Result{}, err
	}
	title := in.Title
	if title == "" {
		title = loaded.Title
	}
	finalURL := ""
	if kind == "url" {
		finalURL = in.URL
	}
	loaded.Hash = hash
	loaded.Title = title
	res, err := p.admit(ctx, graph.DocumentInput{
		Kind: kind, Source: source, FinalURL: finalURL, Title: title, Hash: hash, Labels: in.Labels,
	}, submission{loaded: loaded, labels: in.Labels}, in.Force)
	if err != nil || res.Noop {
		return res, err
	}
	// Release admission's locks before applying queue backpressure.
	p.enqueue(ctx, res.DocumentID)
	return res, nil
}

func (p *Pipeline) admit(ctx context.Context, in graph.DocumentInput, sub submission, force bool) (Result, error) {
	id := graph.DocumentID(in.Kind, in.Source, in.Hash)
	// A duplicate is only a read: do not wait on processing or touch the
	// spool, which may be full or unavailable while this version is live.
	doc, err := p.store.Document(ctx, id)
	if err != nil && !errors.Is(err, graph.ErrNotFound) {
		return Result{}, fmt.Errorf("%w: %w", ErrStore, err)
	}
	if err == nil && doc.Status != graph.StatusFailed && doc.Status != graph.StatusSuperseded && !force {
		return Result{DocumentID: id, Noop: true}, nil
	}
	lock := p.docLock(id)
	if !lock.TryLock() {
		return Result{}, ErrBusy
	}
	defer lock.Unlock()
	p.spool.mu.Lock()
	defer p.spool.mu.Unlock()
	// Recheck after acquiring the locks: another admission may have
	// published this version since our first read. Keep payload writes and
	// publication serialized with Process's forget and the spool sweep.
	doc, err = p.store.Document(ctx, id)
	if err != nil && !errors.Is(err, graph.ErrNotFound) {
		return Result{}, fmt.Errorf("%w: %w", ErrStore, err)
	}
	if err == nil && doc.Status != graph.StatusFailed && doc.Status != graph.StatusSuperseded && !force {
		return Result{DocumentID: id, Noop: true}, nil
	}
	if err := p.spool.putLocked(id, sub); err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrSpool, err)
	}
	in.Force = force
	res, err := p.store.UpsertDocument(ctx, in)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrStore, err)
	}
	if res.Noop && !force {
		return Result{DocumentID: res.ID, Noop: true}, nil
	}
	p.mu.Lock()
	p.pending[res.ID] = sub
	p.mu.Unlock()
	if p.metrics != nil {
		p.metrics.DocumentStatus(string(graph.StatusReceived))
	}
	return Result{DocumentID: res.ID}, nil
}

// Process runs the pipeline stages for one document, resuming from the
// persisted stage on recovery: received/fetched re-chunk, chunked/extracted
// re-extract, merged/verifying only finish (supersede + done). Documents
// that are done or superseded are a no-op. Processing is serialized per
// document, so duplicate queue entries (submit + recover) cannot run two
// passes at once.
func (p *Pipeline) Process(ctx context.Context, docID string) error {
	lock := p.docLock(docID)
	lock.Lock()
	defer lock.Unlock()

	doc, err := p.store.Document(ctx, docID)
	if err != nil {
		if errors.Is(err, graph.ErrNotFound) {
			p.forget(docID)
		}
		return err
	}
	switch doc.Status {
	case graph.StatusDone, graph.StatusSuperseded:
		p.forget(docID)
		return nil
	}
	fail := func(stage string, err error) error {
		// A concurrent supersede wins: this version is discarded benignly.
		if doc2, derr := p.store.Document(ctx, docID); derr == nil && doc2.Status == graph.StatusSuperseded {
			p.forget(docID)
			return nil
		}
		msg := stage + ": " + err.Error()
		if serr := p.setStatus(ctx, docID, graph.StatusFailed, msg); serr != nil {
			slog.Warn("ingest: could not record failure", "doc", docID, "err", serr)
		} else {
			p.forget(docID)
		}
		return fmt.Errorf("ingest: %s: %w", stage, err)
	}

	st := doc.Status
	if st == graph.StatusReceived || st == graph.StatusFetched || st == graph.StatusFailed {
		if err := p.setStatus(ctx, docID, graph.StatusFetched, ""); err != nil {
			return fail("status", err)
		}
		loaded, err := p.loadFor(ctx, doc)
		if err != nil {
			return fail("load", err)
		}
		// The reloaded content must match the document's content hash: a
		// source that changed after submission is a new version, not this
		// document (otherwise the node would carry a stale identity).
		if hash := sha256Hex(loaded.Text); hash != doc.Hash {
			return fail("load", fmt.Errorf("source content changed since submission (hash %s, want %s); resubmit to ingest the new version", hash, doc.Hash))
		}
		chunks, err := p.buildChunks(ctx, doc, loaded.Text)
		if err != nil {
			return fail("chunk", err)
		}
		if len(chunks) > p.cfg.Ingest.MaxChunks {
			return fail("chunk", fmt.Errorf("too many chunks (%d>%d)", len(chunks), p.cfg.Ingest.MaxChunks))
		}
		// A full re-chunk replaces the document's chunks: prune any stale
		// tail from an earlier attempt before writing.
		if err := p.store.DeleteChunksFrom(ctx, docID, len(chunks)); err != nil {
			return fail("chunk", err)
		}
		if err := p.store.PutChunks(ctx, docID, chunks); err != nil {
			return fail("chunk", err)
		}
		if p.metrics != nil {
			p.metrics.Chunks(len(chunks))
		}
		for _, c := range chunks {
			p.index(c.ID, chunkFP(c))
		}
		if err := p.setStatus(ctx, docID, graph.StatusChunked, ""); err != nil {
			return fail("status", err)
		}
		st = graph.StatusChunked
	}
	// Chunks now contain everything recovery needs, including the text.
	// Release spool capacity before slow extraction starts.
	p.forget(docID)

	if st == graph.StatusChunked || st == graph.StatusExtracted {
		chunks, err := p.store.Chunks(ctx, docID)
		if err != nil {
			return fail("chunk", err)
		}
		if len(chunks) == 0 {
			return fail("chunk", errors.New("no chunks persisted"))
		}
		// Recovery gap: make sure every chunk carries its fingerprint.
		for i := range chunks {
			if len(chunks[i].MinhashSig) == 0 {
				fp, ferr := dedup.Fingerprint(chunks[i].Text)
				if ferr == nil {
					chunks[i].ExactKey = fp.ExactKey
					chunks[i].MinhashSig = fp.Sig
					chunks[i].BandKeys = fp.Bands
					chunks[i].SignatureVersion = fp.Version
					if err := p.store.PutFingerprint(ctx, chunks[i].ID, fp); err != nil {
						return fail("chunk", err)
					}
					p.index(chunks[i].ID, fp)
				}
			}
		}

		ents, rels, err := p.labelsFor(p.labelName(doc))
		if err != nil {
			return fail("labels", err)
		}
		docScoped := make(map[string]bool)
		for _, l := range ents {
			if l.Scope == "document" {
				docScoped[l.Name] = true
			}
		}
		req := extract.Request{Chunks: make([]extract.RequestChunk, 0, len(chunks)), Entities: ents, Relations: rels}
		for _, c := range chunks {
			if c.DuplicateOf == "" {
				req.Chunks = append(req.Chunks, extract.RequestChunk{ID: c.ID, Text: c.Text})
			}
		}
		var results []extract.ChunkResult
		if len(req.Chunks) > 0 {
			start := time.Now()
			results, err = p.prov.Extract(ctx, req)
			if p.metrics != nil {
				p.metrics.ExtractSeconds(time.Since(start).Seconds())
				if err != nil {
					p.metrics.ExtractorError()
				}
			}
			if err != nil {
				return fail("extract", err)
			}
		}
		if err := p.setStatus(ctx, docID, graph.StatusExtracted, ""); err != nil {
			return fail("status", err)
		}

		byChunk := make(map[string]extract.ChunkResult, len(results))
		for _, r := range results {
			byChunk[r.ChunkID] = r
		}
		version := p.prov.Version()
		var judgeCandidates []judgeCandidate
		embedNames := make(map[string]string)
		for _, c := range chunks {
			if c.DuplicateOf != "" {
				// A duplicate skips extraction and must not keep facts from
				// an earlier run.
				if err := p.store.UpsertExtraction(ctx, docID, c.ID, version, nil, nil); err != nil {
					return fail("merge", err)
				}
				continue
			}
			r := byChunk[c.ID]
			ents, facts, audits := p.buildExtraction(doc, c, r, version, docScoped)
			if err := p.store.UpsertExtraction(ctx, docID, c.ID, version, ents, facts); err != nil {
				return fail("merge", err)
			}
			if p.metrics != nil {
				p.metrics.Entities(len(ents))
				p.metrics.Facts(len(facts))
				p.metrics.FactsDropped(len(audits))
			}
			for _, e := range ents {
				if e.Key != "" && e.Name != "" {
					embedNames[graph.EntityID(e.Key)] = e.Name
				}
			}
			for _, a := range audits {
				if err := p.store.WriteFactAudit(ctx, a); err != nil {
					return fail("audit", err)
				}
			}
			// Judge exactly what was written: UpsertExtraction dedupes per
			// (head, rel, tail) keeping max confidence, so re-apply that
			// before collecting judge candidates.
			for _, f := range dedupeFactCandidates(facts) {
				judgeCandidates = append(judgeCandidates, judgeCandidate{fact: f, chunkText: c.Text, chunkID: c.ID})
			}
		}
		if err := p.setStatus(ctx, docID, graph.StatusMerged, ""); err != nil {
			return fail("status", err)
		}
		if p.judge != nil && judgeModeActive(p.cfg.Judge.Mode) && len(judgeCandidates) > 0 {
			if err := p.setStatus(ctx, docID, graph.StatusVerifying, ""); err != nil {
				return fail("status", err)
			}
			p.verifyFacts(ctx, doc, judgeCandidates, version)
		}
		if p.emb != nil && len(embedNames) > 0 {
			p.embedEntities(ctx, embedNames)
		}
		st = graph.StatusMerged
	}

	// Finish: supersede every older live version of this identity (ordering by
	// submit time, so versions submitted after this one are untouched) and mark
	// the document done. The list is re-derived from persisted state so a
	// recovery run performs the supersede too. A vanished old version is
	// skipped because its goal state already holds. A missing new version still
	// fails the sweep, so a node recreated by a concurrent identical resubmit
	// is marked failed and reprocessed by its own job instead of being marked
	// done without chunks.
	olds, err := p.store.OlderLiveVersions(ctx, docID)
	if err != nil {
		return fail("supersede", err)
	}
	if p.beforeSupersede != nil {
		p.beforeSupersede(olds)
	}
	for _, oldID := range olds {
		err := p.store.Supersede(ctx, oldID, docID)
		if errors.Is(err, graph.ErrNotFound) {
			if _, derr := p.store.Document(ctx, oldID); errors.Is(derr, graph.ErrNotFound) {
				continue
			}
			return fail("supersede", err)
		}
		if err != nil && !errors.Is(err, graph.ErrSuperseded) {
			return fail("supersede", err)
		}
		p.forgetSuperseded(ctx, oldID)
		if err == nil && p.metrics != nil {
			p.metrics.DocumentStatus(string(graph.StatusSuperseded))
		}
	}
	if err := p.setStatus(ctx, docID, graph.StatusDone, ""); err != nil {
		return fail("status", err)
	}
	p.forget(docID)
	return nil
}

// forget drops the payload once chunks are durable or the document is terminal.
func (p *Pipeline) forget(docID string) {
	p.mu.Lock()
	delete(p.pending, docID)
	p.mu.Unlock()
	if err := p.spool.remove(docID); err != nil {
		slog.Warn("ingest: spool cleanup failed", "doc", docID, "err", err)
	}
}

// A superseded id may already have been submitted again by another worker.
// Check under the admission lock before discarding that version's payload.
func (p *Pipeline) forgetSuperseded(ctx context.Context, docID string) {
	p.spool.mu.Lock()
	defer p.spool.mu.Unlock()
	doc, err := p.store.Document(ctx, docID)
	if err != nil || doc.Status != graph.StatusSuperseded {
		return
	}
	p.mu.Lock()
	delete(p.pending, docID)
	p.mu.Unlock()
	if err := p.spool.removeLocked(docID); err != nil {
		slog.Warn("ingest: spool cleanup failed", "doc", docID, "err", err)
	}
}

// SetJudge attaches the online fact-verification judge; nil means off.
// Additive to the constructor; attach it before starting workers.
func (p *Pipeline) SetJudge(j FactJudge) {
	p.judge = j
}

// SetEmbedder attaches the optional embeddings provider; without one the
// pipeline never touches vectors (text-only deployments stay as before).
// Additive to the constructor; attach it before starting workers.
func (p *Pipeline) SetEmbedder(e Embedder) {
	p.emb = e
}

// SetMetrics attaches the observability counters; nil (the default) leaves
// them disabled. Additive to the constructor.
func (p *Pipeline) SetMetrics(m Metrics) {
	p.metrics = m
}

// setStatus updates a document's status and counts the transition when the
// write succeeds. Note: a terminal superseded document silently drops late
// writes (store contract) and such a refused transition is still counted —
// the counters are advisory and the window is a concurrent same-source
// version race.
func (p *Pipeline) setStatus(ctx context.Context, docID string, st graph.Status, errMsg string) error {
	if err := p.store.SetStatus(ctx, docID, st, errMsg); err != nil {
		return err
	}
	if p.metrics != nil {
		p.metrics.DocumentStatus(string(st))
	}
	return nil
}

type judgeCandidate struct {
	fact      graph.FactUpsert
	chunkText string
	chunkID   string
}

// dedupeFactCandidates mirrors UpsertExtraction's per-chunk dedupe: one
// candidate per (head, rel, tail), keeping the highest confidence.
func dedupeFactCandidates(facts []graph.FactUpsert) []graph.FactUpsert {
	best := make(map[string]graph.FactUpsert, len(facts))
	order := make([]string, 0, len(facts))
	for _, f := range facts {
		k := f.HeadKey + "\x00" + graph.SanitizeRel(f.Rel) + "\x00" + f.TailKey
		if cur, ok := best[k]; ok {
			if f.Confidence > cur.Confidence {
				best[k] = f
			}
			continue
		}
		best[k] = f
		order = append(order, k)
	}
	out := make([]graph.FactUpsert, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	return out
}

// verifyFacts runs the judge over selected facts. Failures never fail the
// document: unverified facts simply keep judge=”.
func (p *Pipeline) verifyFacts(ctx context.Context, doc *graph.Document, candidates []judgeCandidate, version string) {
	mode := p.cfg.Judge.Mode
	for _, cand := range candidates {
		f := cand.fact
		factKey := f.HeadKey + "|" + f.Rel + "|" + f.TailKey
		if !ShouldJudge(mode, f.Confidence, p.cfg.Judge.VerifyFloor, p.cfg.Judge.SamplePct, factKey) {
			continue
		}
		evidence := EvidenceSentence(cand.chunkText, f.HeadMention.Start, f.HeadMention.End)
		start := time.Now()
		verdict, err := p.judge.VerifyFact(ctx, FactInput{
			Head: f.HeadKey, Rel: f.Rel, Tail: f.TailKey,
			Evidence: evidence, DocContext: doc.Title, Confidence: f.Confidence,
		})
		if p.metrics != nil {
			p.metrics.JudgeSeconds(time.Since(start).Seconds())
		}
		if err != nil {
			slog.Warn("ingest: judge unavailable, fact left unverified",
				"doc", doc.ID, "head", f.HeadKey, "rel", f.Rel, "err", err)
			if p.metrics != nil {
				p.metrics.JudgeFailure()
			}
			continue
		}
		if p.metrics != nil {
			p.metrics.JudgeVerdict(verdict.Status)
		}
		switch verdict.Status {
		case "confirmed", "unclear":
			if err := p.store.SetFactJudge(ctx, f.HeadKey, f.Rel, f.TailKey, cand.chunkID, verdict.Status); err != nil {
				slog.Warn("ingest: could not record verdict", "doc", doc.ID, "err", err)
			}
		case "refuted":
			audit := graph.FactAudit{
				HeadKey: f.HeadKey, Rel: f.Rel, TailKey: f.TailKey,
				DocID: doc.ID, ChunkID: cand.chunkID, Score: f.Confidence,
				JudgeVerdict: "refuted", Reason: verdict.Reason,
				ExtractorVersion: version,
			}
			if err := p.store.RejectFact(ctx, f.HeadKey, f.Rel, f.TailKey, cand.chunkID, audit); err != nil {
				slog.Warn("ingest: could not reject fact", "doc", doc.ID, "err", err)
			}
		default:
			slog.Warn("ingest: judge returned an unknown verdict status", "doc", doc.ID, "status", verdict.Status)
		}
	}
}

// embedEntities stores embeddings for the document's distinct entity names,
// one embedder call per ≤embedBatch names and one batched write per call,
// and ensures the vector index on the first successful batch. Any failure
// is logged and processing continues: embeddings are additive and never
// gate the document.
func (p *Pipeline) embedEntities(ctx context.Context, candidates map[string]string) {
	byName := make(map[string][]string, len(candidates))
	for id, name := range candidates {
		byName[name] = append(byName[name], id)
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	indexEnsured := false
	for start := 0; start < len(names); start += embedBatch {
		end := min(start+embedBatch, len(names))
		batch := names[start:end]
		vecs, err := p.emb.EmbedDocuments(ctx, batch)
		if err != nil {
			slog.Warn("ingest: embedding failed, entities left unembedded", "names", len(batch), "err", err)
			return
		}
		if len(vecs) != len(batch) || len(vecs[0]) == 0 {
			slog.Warn("ingest: embedder returned an unusable batch", "names", len(batch), "vectors", len(vecs))
			return
		}
		if !indexEnsured {
			if err := p.store.EnsureVectorIndex(ctx, len(vecs[0])); err != nil {
				slog.Warn("ingest: vector index unavailable, entities left unembedded", "err", err)
				return
			}
			indexEnsured = true
		}
		ids := make([]string, 0, len(batch))
		rowsVecs := make([][]float64, 0, len(batch))
		for i, name := range batch {
			for _, id := range byName[name] {
				ids = append(ids, id)
				rowsVecs = append(rowsVecs, vecs[i])
			}
		}
		missing, err := p.store.SetEntityEmbeddings(ctx, ids, rowsVecs)
		if err != nil {
			slog.Warn("ingest: could not store embeddings", "entities", len(ids), "err", err)
			return
		}
		if missing > 0 {
			slog.Warn("ingest: some entities vanished before their embedding write", "missing", missing)
		}
	}
}

// Recover requeues every in-flight document (restart path). Start the
// worker pool before calling it: enqueue applies backpressure, so a full
// queue blocks until workers drain it (or ctx is cancelled).
func (p *Pipeline) Recover(ctx context.Context) error {
	docs, err := p.store.InFlight(ctx)
	if err != nil {
		return err
	}
	for _, d := range docs {
		p.enqueue(ctx, d.ID)
	}
	if err := p.spool.sweep(ctx, p.store); err != nil {
		slog.Warn("ingest: spool recovery cleanup failed", "err", err)
	}
	return nil
}

// docLock returns the per-document processing lock.
func (p *Pipeline) docLock(docID string) *sync.Mutex {
	v, _ := p.procLocks.LoadOrStore(docID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// enqueue pushes a document id for the worker pool, applying backpressure
// when the queue is full (bounded by ctx) so an accepted submission is
// never silently parked until restart.
func (p *Pipeline) enqueue(ctx context.Context, docID string) {
	select {
	case p.jobs <- docID:
	case <-ctx.Done():
		slog.Warn("ingest: enqueue cancelled", "doc", docID, "err", ctx.Err())
	}
}

// loadInput runs the TextLoader for a submission.
func (p *Pipeline) loadInput(ctx context.Context, in Input) (Loaded, error) {
	if p.load == nil {
		return Loaded{}, errors.New("no text loader configured")
	}
	return p.load(ctx, in)
}

// loadFor tries the cache, then the durable converted payload, then the
// original source. Only legacy URL documents can be loaded without a spool.
func (p *Pipeline) loadFor(ctx context.Context, doc *graph.Document) (Loaded, error) {
	p.mu.Lock()
	sub, ok := p.pending[doc.ID]
	p.mu.Unlock()
	if ok {
		return sub.loaded, nil
	}
	sub, err := p.spool.load(doc)
	if err == nil {
		return sub.loaded, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Loaded{}, fmt.Errorf("spool: %w", err)
	}
	target := doc.Source
	if doc.SourceKind == "url" && doc.FinalURL != "" {
		target = doc.FinalURL
	}
	return p.loadInput(ctx, Input{Kind: doc.SourceKind, Source: doc.Source, URL: target, Title: doc.Title})
}

func (p *Pipeline) labelName(doc *graph.Document) string {
	p.mu.Lock()
	sub, ok := p.pending[doc.ID]
	p.mu.Unlock()
	if ok {
		return sub.labels
	}
	return doc.Labels
}

// index adds a chunk fingerprint to the candidate index when present.
func (p *Pipeline) index(chunkID string, fp dedup.FP) {
	if p.ix == nil || len(fp.Sig) == 0 {
		return
	}
	if err := p.ix.Add(chunkID, fp); err != nil {
		slog.Warn("ingest: index add failed", "chunk", chunkID, "err", err)
	}
}

// buildChunks splits the text, fingerprints every chunk and marks
// near-duplicates per the configured mode (off | exact | near). Exact
// matches run first, including against earlier chunks of the same document.
func (p *Pipeline) buildChunks(ctx context.Context, doc *graph.Document, text string) ([]graph.Chunk, error) {
	spans := chunk.Split(text, chunk.Options{
		TargetWords:      p.cfg.Ingest.ChunkWords,
		OverlapSentences: p.cfg.Ingest.OverlapSentences,
	})
	chunks := make([]graph.Chunk, 0, len(spans))
	for _, s := range spans {
		c := graph.Chunk{Seq: s.Seq, Text: s.Text, Start: s.Start, End: s.End}
		c.Hash = sha256Hex(s.Text)
		if fp, err := dedup.Fingerprint(s.Text); err == nil {
			c.ExactKey = fp.ExactKey
			c.MinhashSig = fp.Sig
			c.BandKeys = fp.Bands
			c.SignatureVersion = fp.Version
		} else if !errors.Is(err, minhash.ErrEmptySet) {
			return nil, err
		}
		chunks = append(chunks, c)
	}

	mode := p.cfg.Ingest.ChunkDedup
	if mode == "" {
		mode = "near"
	}
	if mode == "off" {
		return chunks, nil
	}
	// Chunks of the same document (a re-chunk over persisted state) and of
	// same-identity versions (the predecessor is superseded — its chunks get
	// deleted) are never dedup targets; dedup exists for boilerplate across
	// independent documents.
	siblings, err := p.store.SameIdentityDocumentIDs(ctx, doc.SourceKind, doc.Source)
	if err != nil {
		return nil, err
	}
	eligible := func(refDocID string) bool {
		return refDocID != doc.ID && !siblings[refDocID]
	}

	seenExact := make(map[string]string, len(chunks))
	type seenChunk struct {
		id       string
		shingles []uint64
	}
	bandIdx := make(map[int64][]int)
	var seenNear []seenChunk

	excludeDocs := make([]string, 0, len(siblings)+1)
	excludeDocs = append(excludeDocs, doc.ID)
	for id := range siblings {
		excludeDocs = append(excludeDocs, id)
	}

	for i := range chunks {
		c := &chunks[i]
		if len(c.MinhashSig) == 0 {
			continue
		}
		cid := doc.ID + "-c" + strconv.Itoa(c.Seq)

		if prev, ok := seenExact[c.ExactKey]; ok {
			c.DuplicateOf = prev
			continue
		}
		refs, err := p.store.ChunkRefsByExactKey(ctx, c.ExactKey, excludeDocs, cid)
		if err != nil {
			return nil, err
		}
		dup := ""
		for _, ref := range refs {
			if ref.ID == cid || !eligible(ref.DocID) {
				continue
			}
			dup = ref.ID
			break
		}

		if dup == "" && mode == "near" {
			tokens, terr := dedup.Tokens(c.Text)
			if terr == nil {
				shingles, serr := dedup.Shingles(tokens)
				if serr == nil {
					// Intra-document candidates: earlier chunks of this same
					// pass (cross-document candidates come from the index).
					// Shared band keys keep the comparison set small.
					candidates := map[int]bool{}
					for _, bk := range c.BandKeys {
						for _, idx := range bandIdx[bk] {
							candidates[idx] = true
						}
					}
					for idx := range candidates {
						if dedup.SimilarShingles(shingles, seenNear[idx].shingles) >= p.cfg.Ingest.JaccardThreshold {
							dup = seenNear[idx].id
							break
						}
					}
					if dup == "" && p.ix != nil {
						for _, cand := range p.ix.Lookup(chunkFP(*c)) {
							if cand == cid {
								continue
							}
							cc, cerr := p.store.ChunkByID(ctx, cand)
							if cerr != nil {
								continue // stale index entry
							}
							if !eligible(cc.DocID) {
								continue
							}
							ctokens, cerr := dedup.Tokens(cc.Text)
							if cerr != nil {
								continue
							}
							cshingles, cerr := dedup.Shingles(ctokens)
							if cerr != nil {
								continue
							}
							if dedup.SimilarShingles(shingles, cshingles) >= p.cfg.Ingest.JaccardThreshold {
								dup = cand
								break
							}
						}
					}
					if dup == "" {
						seenNear = append(seenNear, seenChunk{id: cid, shingles: shingles})
						for _, bk := range c.BandKeys {
							bandIdx[bk] = append(bandIdx[bk], len(seenNear)-1)
						}
					}
				}
			}
		}
		if dup != "" {
			c.DuplicateOf = dup
			continue
		}
		seenExact[c.ExactKey] = cid
	}
	return chunks, nil
}

// buildExtraction turns one provider chunk result into store upserts,
// applying the confidence floors: below store_floor the fact is dropped and
// audited; below assert_floor it is stored as low-confidence.
func (p *Pipeline) buildExtraction(doc *graph.Document, c graph.Chunk, r extract.ChunkResult, version string, docScoped map[string]bool) ([]graph.EntityUpsert, []graph.FactUpsert, []graph.FactAudit) {
	ents := make(map[string]graph.EntityUpsert, len(r.Mentions))
	keys := make([]string, len(r.Mentions))
	bestScores := make(map[string]float64, len(r.Mentions))
	type mentionKey struct {
		key        string
		start, end int
	}
	bestMentions := make(map[mentionKey]extract.Mention, len(r.Mentions))
	mentionOrder := make([]mentionKey, 0, len(r.Mentions))
	for i, m := range r.Mentions {
		if strings.TrimSpace(m.Text) == "" || m.Label == "" {
			continue
		}
		key := graph.EntityKey(m.Text)
		if key == "" {
			continue
		}
		if docScoped[m.Label] {
			key = graph.ScopedEntityKey(doc.ID, m.Text)
		}
		keys[i] = key
		e, ok := ents[key]
		if !ok {
			e = graph.EntityUpsert{Key: key, Name: graph.DisplayName(m.Text), Type: m.Label}
		}
		if !ok || m.Score > bestScores[key] || (m.Score == bestScores[key] && m.Label < e.Type) {
			e.Type = m.Label
			bestScores[key] = m.Score
		}
		ents[key] = e
		mk := mentionKey{key, m.Start, m.End}
		best, seen := bestMentions[mk]
		if !seen {
			mentionOrder = append(mentionOrder, mk)
		}
		if !seen || m.Score > best.Score || (m.Score == best.Score && m.Label < best.Label) {
			bestMentions[mk] = m
		}
	}
	for _, mk := range mentionOrder {
		e := ents[mk.key]
		e.Mentions = append(e.Mentions, graph.MentionSpan{Start: mk.start, End: mk.end, Type: bestMentions[mk].Label})
		ents[mk.key] = e
	}

	facts := make([]graph.FactUpsert, 0, len(r.Candidates))
	audits := make([]graph.FactAudit, 0)
	for _, cand := range r.Candidates {
		if cand.Head < 0 || cand.Head >= len(r.Mentions) || cand.Tail < 0 || cand.Tail >= len(r.Mentions) {
			continue
		}
		hm, tm := r.Mentions[cand.Head], r.Mentions[cand.Tail]
		if strings.TrimSpace(hm.Text) == "" || strings.TrimSpace(tm.Text) == "" ||
			cand.Label == "" || hm.Label == "" || tm.Label == "" {
			continue
		}
		headKey, tailKey := keys[cand.Head], keys[cand.Tail]
		if headKey == "" || tailKey == "" {
			continue
		}
		if cand.Score < p.cfg.Ingest.StoreFloor {
			audits = append(audits, graph.FactAudit{
				HeadKey: headKey, Rel: cand.Label, TailKey: tailKey,
				DocID: doc.ID, ChunkID: c.ID, Score: cand.Score,
				JudgeVerdict: "store_floor", Reason: "below store_floor",
				ExtractorVersion: version,
			})
			continue
		}
		facts = append(facts, graph.FactUpsert{
			HeadKey: headKey, TailKey: tailKey, Rel: cand.Label, Confidence: cand.Score,
			HeadMention:   graph.Span{Start: hm.Start, End: hm.End, Text: hm.Text},
			TailMention:   graph.Span{Start: tm.Start, End: tm.End, Text: tm.Text},
			LowConfidence: cand.Score < p.cfg.Ingest.AssertFloor,
		})
	}

	list := make([]graph.EntityUpsert, 0, len(ents))
	for _, e := range ents {
		list = append(list, e)
	}
	return list, facts, audits
}

// labelsFor resolves the label set: built-in defaults or a named preset
// from Labels.PresetsDir. Preset names are restricted to a safe charset
// because they reach the filesystem, and every label must contain at least
// one alphanumeric character (the store rejects relations without one).
func (p *Pipeline) labelsFor(name string) ([]extract.Label, []extract.Label, error) {
	if name == "" {
		return defaultEntityLabels, defaultRelationLabels, nil
	}
	if !presetNameRe.MatchString(name) {
		return nil, nil, fmt.Errorf("label preset %q: invalid name", name)
	}
	path := filepath.Join(p.cfg.Labels.PresetsDir, name+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("label preset %q: %w", name, err)
	}
	var preset struct {
		Entities  []extract.Label `yaml:"entities"`
		Relations []extract.Label `yaml:"relations"`
	}
	if err := yaml.Unmarshal(data, &preset); err != nil {
		return nil, nil, fmt.Errorf("label preset %q: %w", name, err)
	}
	for _, l := range append(append([]extract.Label{}, preset.Entities...), preset.Relations...) {
		if !labelNameRe.MatchString(l.Name) {
			return nil, nil, fmt.Errorf("label preset %q: label %q has no alphanumeric content", name, l.Name)
		}
		if l.Scope != "" && l.Scope != "document" {
			return nil, nil, fmt.Errorf("label preset %q: label %q has unknown scope %q", name, l.Name, l.Scope)
		}
	}
	return preset.Entities, preset.Relations, nil
}

// resolveIdentity derives (source_kind, source) per the design: url uses
// the submitted URL, file requires a caller-supplied source, text falls
// back to the first hash bytes.
func resolveIdentity(in Input, hash string) (kind, source string, err error) {
	kind = in.Kind
	if kind == "" {
		switch {
		case in.URL != "":
			kind = "url"
		case len(in.Bytes) > 0:
			kind = "file"
		default:
			kind = "text"
		}
	}
	switch kind {
	case "url":
		source = in.Source
		if source == "" {
			source = in.URL
		}
		if source == "" {
			return "", "", errors.New("ingest: url input without a URL")
		}
	case "file":
		source = in.Source
		if source == "" {
			return "", "", errors.New("ingest: file input requires a source (filename)")
		}
	case "text":
		source = in.Source
		if source == "" {
			source = hash[:8]
		}
	default:
		return "", "", fmt.Errorf("ingest: unknown input kind %q", kind)
	}
	return kind, source, nil
}

func chunkFP(c graph.Chunk) dedup.FP {
	return dedup.FP{
		ExactKey: c.ExactKey,
		Sig:      c.MinhashSig,
		Bands:    c.BandKeys,
		Version:  c.SignatureVersion,
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
