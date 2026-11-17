// Package chunkread provides bounded chunk text reads shared by agent and MCP tools.
package chunkread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const (
	MaxIDs         = 20
	MaxNeighbours  = 2
	ChunkTextCap   = 4 << 10
	ResponseBudget = 24 << 10
)

type Request struct {
	IDs        []string
	Neighbours int
	Domain     string
}

type Item struct {
	ID          string `json:"id"`
	DocID       string `json:"doc_id"`
	Seq         int    `json:"seq"`
	Text        string `json:"text"`
	Truncated   bool   `json:"truncated,omitempty"`
	NeighbourOf string `json:"neighbour_of,omitempty"`
}

type Meta struct {
	Returned         int      `json:"returned"`
	Missing          []string `json:"missing"`
	Truncated        []string `json:"truncated"`
	OmittedForBudget []string `json:"omitted_for_budget"`
}

type Result struct {
	Chunks []Item `json:"chunks"`
	Meta   Meta   `json:"meta"`
}

type Store interface {
	ChunkNeighbourhood(ctx context.Context, id string, n int) (graph.Chunk, []graph.Chunk, error)
}

func Read(ctx context.Context, st Store, req Request) (Result, error) {
	if len(req.IDs) < 1 || len(req.IDs) > MaxIDs {
		return Result{}, errors.New("ids must hold 1–20 chunk ids")
	}
	if req.Neighbours < 0 || req.Neighbours > MaxNeighbours {
		return Result{}, errors.New("neighbours must be between 0 and 2")
	}
	for _, id := range req.IDs {
		if len(id) > 128 {
			return Result{}, errors.New("chunk ids must be at most 128 bytes")
		}
	}
	result := Result{
		Chunks: []Item{},
		Meta:   Meta{Missing: []string{}, Truncated: []string{}, OmittedForBudget: []string{}},
	}
	seen, requested := map[string]bool{}, map[string]bool{}
	add := func(c graph.Chunk, neighbourOf string) {
		if seen[c.ID] {
			return
		}
		seen[c.ID] = true
		item := Item{ID: c.ID, DocID: c.DocID, Seq: c.Seq, Text: c.Text, NeighbourOf: neighbourOf}
		if len(item.Text) > ChunkTextCap {
			end := ChunkTextCap
			for end > 0 && !utf8.RuneStart(item.Text[end]) {
				end--
			}
			item.Text, item.Truncated = item.Text[:end], true
			result.Meta.Truncated = append(result.Meta.Truncated, item.ID)
		}
		result.Chunks = append(result.Chunks, item)
	}
	for _, id := range req.IDs {
		if requested[id] {
			continue
		}
		requested[id] = true
		center, around, err := st.ChunkNeighbourhood(ctx, id, req.Neighbours)
		if errors.Is(err, graph.ErrNotFound) {
			result.Meta.Missing = append(result.Meta.Missing, id)
			continue
		}
		if err != nil {
			return Result{}, fmt.Errorf("read chunk %s: %w", id, err)
		}
		add(center, "")
		for _, c := range around {
			add(c, id)
		}
	}
	result.Meta.Returned = len(result.Chunks)
	// Remove only a suffix, measuring the entire wire result each time.
	// This includes escaping, missing ids, truncation and omitted-id metadata.
	for {
		encoded, err := json.Marshal(result)
		if err != nil {
			return Result{}, err
		}
		if len(encoded) <= ResponseBudget {
			return result, nil
		}
		if len(result.Chunks) == 0 {
			return Result{}, errors.New("chunk metadata exceeds response budget")
		}
		last := result.Chunks[len(result.Chunks)-1]
		result.Chunks = result.Chunks[:len(result.Chunks)-1]
		result.Meta.Returned--
		result.Meta.OmittedForBudget = append([]string{last.ID}, result.Meta.OmittedForBudget...)
		if last.Truncated {
			result.Meta.Truncated = result.Meta.Truncated[:len(result.Meta.Truncated)-1]
		}
	}
}
