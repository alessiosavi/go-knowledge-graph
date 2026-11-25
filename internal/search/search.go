// Package search implements the text read paths: search over entities and
// chunks with optional rank fusion against entity embeddings. Traversal
// lives in the graph store (graph.Store.Traverse).
package search

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// QueryEmbedder embeds a query text (structurally satisfied by the embed
// providers); nil means text-only search.
type QueryEmbedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float64, error)
}

// QueryEmbedding shares one embedding of a fixed query among search legs.
type QueryEmbedding struct {
	done chan struct{}
	vec  []float64
	err  error
}

// StartQueryEmbedding starts the provider call under the request context.
// Each leg can stop waiting independently of the provider and other legs.
func StartQueryEmbedding(ctx context.Context, emb QueryEmbedder, query string) *QueryEmbedding {
	future := &QueryEmbedding{done: make(chan struct{})}
	go func() {
		future.vec, future.err = emb.EmbedQuery(ctx, query)
		close(future.done)
	}()
	return future
}

// EmbedQuery lets Query's vector leg wait within its existing embed budget.
// The query was already supplied to StartQueryEmbedding.
func (f *QueryEmbedding) EmbedQuery(ctx context.Context, _ string) ([]float64, error) {
	return f.Wait(ctx)
}

// Wait returns the shared result if it arrives within the caller's context.
func (f *QueryEmbedding) Wait(ctx context.Context) ([]float64, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return f.vec, f.err
	}
}

// Store provides the text, vector and batched entity lookups used by search.
type Store interface {
	EntityTextSearchIn(ctx context.Context, q, slug string, limit int) ([]graph.EntitySearchRow, error)
	ChunkTextSearchInWindow(ctx context.Context, q, slug string, limit int, window graph.DateWindow) ([]graph.ChunkSearchRow, error)
	EntityVectorSearch(ctx context.Context, vec []float64, limit int) ([]string, error)
	EntitySearchRowsByID(ctx context.Context, ids []string) ([]graph.EntitySearchRow, error)
}

// Hit is one search result.
type Hit struct {
	Domain  string
	Kind    string // entity | chunk
	ID      string
	DocID   string // chunk hits only: the owning document id
	Title   string
	Score   float64
	Snippet string

	DocumentDate       *time.Time `json:"document_date,omitempty"`
	DocumentDateSource *string    `json:"document_date_source,omitempty"`

	mentions int
}

const rrfK = 60

// RRF is the reciprocal rank fusion weight of a rank (0-based).
func RRF(rank int) float64 {
	return 1.0 / (rrfK + float64(rank))
}

// FuseRanks merges per-leg ordered id lists into fused scores (duplicate
// ids within one leg count once, at their first rank).
func FuseRanks(legs [][]string) map[string]float64 {
	out := make(map[string]float64)
	for _, leg := range legs {
		seen := make(map[string]bool, len(leg))
		rank := 0
		for _, id := range leg {
			if seen[id] {
				continue
			}
			seen[id] = true
			out[id] += RRF(rank)
			rank++
		}
	}
	return out
}

type options struct {
	domain string
	window graph.DateWindow
}

// Option configures a search.
type Option func(*options)

// InDomain scopes both text and vector hits to a domain; empty selects all.
func InDomain(d string) Option { return func(o *options) { o.domain = d } }

// InDateWindow restricts chunk hits by effective document date.
func InDateWindow(w graph.DateWindow) Option { return func(o *options) { o.window = w } }

// Query searches entities, chunks or both. With an embedder, entity search
// fuses text and vector hits. Chunks use text search.
func Query(ctx context.Context, st Store, emb QueryEmbedder, q, kind string, limit int, opts ...Option) ([]Hit, error) {
	opt := options{}
	for _, apply := range opts {
		apply(&opt)
	}
	if opt.domain != "" && !graph.ValidDomain(opt.domain) {
		return nil, argerr.Errorf("search: invalid domain slug %q", opt.domain)
	}
	if strings.TrimSpace(q) == "" {
		return nil, argerr.New("search: empty query")
	}
	switch kind {
	case "entity", "chunk", "auto":
	default:
		return nil, argerr.Errorf("search: kind %q not in entity|chunk|auto", kind)
	}
	// A query with no searchable word (only punctuation or symbols) matches
	// nothing: skip every leg, the vector leg included, rather than return
	// embedding neighbours of punctuation.
	if len(graph.TextQueryWords(q)) == 0 {
		return []Hit{}, nil
	}
	if limit <= 0 {
		limit = 20
	}
	switch kind {
	case "chunk":
		return chunkHits(ctx, st, q, limit, opt)
	case "entity", "auto":
		hits, err := entityCandidates(ctx, st, emb, q, limit, opt.domain)
		if err != nil {
			return nil, err
		}
		if kind == "auto" {
			chunks, err := chunkHits(ctx, st, q, limit, opt)
			if err != nil {
				return nil, err
			}
			hits = append(hits, chunks...)
		}
		return topHits(hits, limit), nil
	default:
		return nil, fmt.Errorf("search: kind %q not in entity|chunk|auto", kind)
	}
}

// entityCandidates keeps both legs' hits until the final ranking, which can
// also include chunks in auto mode.
func entityCandidates(ctx context.Context, st Store, emb QueryEmbedder, q string, limit int, slug string) ([]Hit, error) {
	recs, err := st.EntityTextSearchIn(ctx, q, slug, limit)
	if err != nil {
		return nil, fmt.Errorf("search: entity text leg: %w", err)
	}
	rows := make(map[string]graph.EntitySearchRow, len(recs))
	textLeg := make([]string, 0, len(recs))
	for _, r := range recs {
		rows[r.ID] = r
		textLeg = append(textLeg, r.ID)
	}

	legs := [][]string{textLeg}
	if emb != nil {
		vectorLimit := limit
		if slug != "" {
			vectorLimit *= 5
		}
		vecLeg, err := vectorLeg(ctx, st, emb, q, vectorLimit)
		if err != nil {
			return nil, err
		}
		if len(vecLeg) > 0 {
			var unknown []string
			for _, id := range vecLeg {
				if _, known := rows[id]; !known {
					unknown = append(unknown, id)
				}
			}
			if len(unknown) > 0 {
				resolved, err := st.EntitySearchRowsByID(ctx, unknown)
				if err != nil {
					if _, timed := ctx.Deadline(); timed && graph.IsQueryTimeout(ctx, err) {
						return nil, fmt.Errorf("search: vector entity lookup: %w", err)
					}
					slog.Warn("search: vector entity lookup failed", "err", err)
				} else {
					for _, r := range resolved {
						if _, known := rows[r.ID]; !known {
							rows[r.ID] = r
						}
					}
				}
			}
			// Rank only resolved, in-domain ids in their original vector order.
			kept := make([]string, 0, min(len(vecLeg), limit))
			for _, id := range vecLeg {
				if len(kept) == limit {
					break
				}
				row, known := rows[id]
				if !known || (slug != "" && row.Domain != slug) {
					continue
				}
				kept = append(kept, id)
			}
			legs = append(legs, kept)
		}
	}

	fused := FuseRanks(legs)
	hits := make([]Hit, 0, len(fused))
	for id, score := range fused {
		row := rows[id]
		hits = append(hits, Hit{
			Domain: row.Domain, Kind: "entity", ID: id, Title: row.Name, Score: score, Snippet: row.Type, mentions: row.MentionCount,
		})
	}
	return hits, nil
}

// vectorLeg embeds the query and returns entity ids ordered by vector
// distance. Failures skip the leg unless the search reaches its deadline.
func vectorLeg(ctx context.Context, st Store, emb QueryEmbedder, q string, limit int) ([]string, error) {
	embedCtx := ctx
	cancel := func() {}
	if deadline, ok := ctx.Deadline(); ok {
		now := time.Now()
		embedCtx, cancel = context.WithDeadline(ctx, now.Add(deadline.Sub(now)/2))
	}
	vec, err := emb.EmbedQuery(embedCtx, q)
	shareExpired := embedCtx.Err() == context.DeadlineExceeded
	if err == nil && shareExpired {
		err = context.DeadlineExceeded
	}
	cancel()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("search: embedding: %w", context.DeadlineExceeded)
	}
	if err != nil {
		if shareExpired {
			// By design, not an outage: a slow embedder leaves the read budget to the text leg.
			slog.Warn("search: embedding exceeded its share of the read budget, text-only results", "err", err)
		} else {
			slog.Warn("search: embedding failed, text-only results", "err", err)
		}
		return nil, nil
	}
	if len(vec) == 0 {
		return nil, nil
	}
	ids, err := st.EntityVectorSearch(ctx, vec, limit)
	if err != nil {
		if _, timed := ctx.Deadline(); timed && graph.IsQueryTimeout(ctx, err) {
			return nil, fmt.Errorf("search: entity vector leg: %w", err)
		}
		slog.Warn("search: vector index unavailable, text-only results", "err", err)
		return nil, nil
	}
	return ids, nil
}

func chunkHits(ctx context.Context, st Store, q string, limit int, opt options) ([]Hit, error) {
	recs, err := st.ChunkTextSearchInWindow(ctx, q, opt.domain, limit, opt.window)
	if err != nil {
		return nil, fmt.Errorf("search: chunk text leg: %w", err)
	}
	leg := make([]string, 0, len(recs))
	byID := make(map[string]Hit, len(recs))
	for _, r := range recs {
		leg = append(leg, r.ID)
		byID[r.ID] = Hit{
			Domain:  r.Domain,
			Kind:    "chunk",
			ID:      r.ID,
			DocID:   r.DocID,
			Title:   r.Title,
			Snippet: Snippet(r.Text, snippetQuery(q)),

			DocumentDate:       r.DocumentDate,
			DocumentDateSource: r.DocumentDateSource,
		}
	}
	fused := FuseRanks([][]string{leg})
	hits := make([]Hit, 0, len(fused))
	for id, score := range fused {
		h := byID[id]
		h.Score = score
		hits = append(hits, h)
	}
	return topHits(hits, limit), nil
}

// snippetQuery returns the query's search words, longest first, so the
// snippet centres on the most specific word the index matched (a raw query
// such as "l'Acme-Max" never occurs verbatim in the text).
func snippetQuery(q string) string {
	words := graph.TextQueryWords(q)
	sort.SliceStable(words, func(i, j int) bool {
		return utf8.RuneCountInString(words[i]) > utf8.RuneCountInString(words[j])
	})
	return strings.Join(words, " ")
}

// topHits sorts by descending score and ascending id. Within each tied run
// of entities, it ranks by descending mentions without crossing chunk hits.
func topHits(hits []Hit, limit int) []Hit {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
	for start := 0; start < len(hits); {
		end := start + 1
		if hits[start].Kind == "entity" {
			for end < len(hits) && hits[end].Kind == "entity" && hits[end].Score == hits[start].Score {
				end++
			}
			if end-start > 1 {
				tied := hits[start:end]
				sort.SliceStable(tied, func(i, j int) bool {
					return tied[i].mentions > tied[j].mentions
				})
			}
		}
		start = end
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// Snippet returns a 120-byte window of text around the first occurrence of
// a query token, falling back to the text start.
func Snippet(text, q string) string {
	center := 0
	for _, tok := range strings.Fields(strings.ToLower(q)) {
		if idx, ok := caseInsensitiveIndex(text, tok); ok {
			center = idx + len(tok)/2
			break
		}
	}
	return graph.ExcerptWindow(text, center, 120)
}

// caseInsensitiveIndex finds the byte offset of tok inside text with a
// rune-wise EqualFold comparison. A lowercased copy is not usable for this:
// Unicode case mappings (U+212A KELVIN SIGN, U+1E9E, U+0130, ...) change
// UTF-8 lengths, so offsets into the copy drift from the original.
func caseInsensitiveIndex(text, tok string) (int, bool) {
	toks := []rune(tok)
	if len(toks) == 0 {
		return 0, false
	}
	for i := 0; i < len(text); {
		j := i
		matched := 0
		for _, tr := range toks {
			if j >= len(text) {
				break
			}
			r, size := utf8.DecodeRuneInString(text[j:])
			if !strings.EqualFold(string(r), string(tr)) && strings.ToLower(string(r)) != strings.ToLower(string(tr)) {
				break
			}
			j += size
			matched++
		}
		if matched == len(toks) {
			return i, true
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		i += size
	}
	return 0, false
}
