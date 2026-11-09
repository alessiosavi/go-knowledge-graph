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
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// Embedder is the optional embedding provider (structurally satisfied by
// the embed provider); nil means text-only search.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float64, error)
}

// Hit is one search result.
type Hit struct {
	Kind    string // entity | chunk
	ID      string
	Title   string
	Score   float64
	Snippet string
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

// Query searches entities, chunks or both. Entity search fuses text hits
// with vector hits when an embedder is configured — a vector miss never
// drops text recall. Chunks are text-only in v0.
func Query(ctx context.Context, st *graph.Store, emb Embedder, q, kind string, limit int) ([]Hit, error) {
	if strings.TrimSpace(q) == "" {
		return nil, fmt.Errorf("search: empty query")
	}
	if limit <= 0 {
		limit = 20
	}
	switch kind {
	case "entity":
		return entityHits(ctx, st, emb, q, limit)
	case "chunk":
		return chunkHits(ctx, st, q, limit)
	case "auto":
		ents, err := entityHits(ctx, st, emb, q, limit)
		if err != nil {
			return nil, err
		}
		chunks, err := chunkHits(ctx, st, q, limit)
		if err != nil {
			return nil, err
		}
		return topHits(append(ents, chunks...), limit), nil
	default:
		return nil, fmt.Errorf("search: kind %q not in entity|chunk|auto", kind)
	}
}

type entityRow struct {
	id, name, typ string
}

func entityHits(ctx context.Context, st *graph.Store, emb Embedder, q string, limit int) ([]Hit, error) {
	recs, err := st.EntityTextSearch(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("search: entity text leg: %w", err)
	}
	rows := make(map[string]entityRow, len(recs))
	textLeg := make([]string, 0, len(recs))
	for _, r := range recs {
		rows[r.ID] = entityRow{id: r.ID, name: r.Name, typ: r.Type}
		textLeg = append(textLeg, r.ID)
	}

	legs := [][]string{textLeg}
	if emb != nil {
		if vecLeg, ok := vectorLeg(ctx, st, emb, q, limit); ok {
			legs = append(legs, vecLeg)
			for _, id := range vecLeg {
				if _, known := rows[id]; !known {
					if r, err := st.EntityRef(ctx, id); err == nil {
						rows[id] = entityRow{id: r.ID, name: r.Name, typ: r.Type}
					}
				}
			}
		}
	}

	fused := FuseRanks(legs)
	hits := make([]Hit, 0, len(fused))
	for id, score := range fused {
		row, known := rows[id]
		title := row.name
		if !known {
			title = id
		}
		hits = append(hits, Hit{Kind: "entity", ID: id, Title: title, Score: score, Snippet: row.typ})
	}
	return topHits(hits, limit), nil
}

// vectorLeg embeds the query and returns entity ids ordered by vector
// distance. Any failure (no embedder output, missing vector index) skips
// the leg — text recall is never dropped by a vector problem.
func vectorLeg(ctx context.Context, st *graph.Store, emb Embedder, q string, limit int) ([]string, bool) {
	vecs, err := emb.Embed(ctx, []string{q})
	if err != nil {
		slog.Warn("search: embedding failed, text-only results", "err", err)
		return nil, false
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil, false
	}
	ids, err := st.EntityVectorSearch(ctx, vecs[0], limit)
	if err != nil {
		slog.Warn("search: vector index unavailable, text-only results", "err", err)
		return nil, false
	}
	return ids, true
}

func chunkHits(ctx context.Context, st *graph.Store, q string, limit int) ([]Hit, error) {
	recs, err := st.ChunkTextSearch(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("search: chunk text leg: %w", err)
	}
	leg := make([]string, 0, len(recs))
	byID := make(map[string]Hit, len(recs))
	for _, r := range recs {
		leg = append(leg, r.ID)
		byID[r.ID] = Hit{
			Kind:    "chunk",
			ID:      r.ID,
			Title:   r.Title,
			Snippet: Snippet(r.Text, q),
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

// topHits sorts by score descending (id as tiebreaker) and truncates.
func topHits(hits []Hit, limit int) []Hit {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].ID < hits[j].ID
	})
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
