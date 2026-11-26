// Package chunkread provides bounded chunk text reads shared by agent and MCP tools.
package chunkread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

const (
	MaxIDs         = 20
	MaxNeighbours  = 2
	ChunkTextCap   = 4 << 10
	ResponseBudget = 24 << 10
)

const ToolDescription = "Read chunk text, up to 4 KiB per chunk; `truncated` marks a cut, by id (ids from search hits), optionally with up to 2 neighbouring chunks of the same document. Use this instead of cypher to read chunk text. omitted_for_budget may also list neighbour ids." +
	" Chunks with an owning document include its effective document_date and document_date_source (caller or ingest). The 24 KiB response budget can omit chunks."

type Request struct {
	IDs           []string
	Neighbours    int
	Domain        string
	WithProducts  bool
	ProductsLimit *int
	// ProductStores scopes product links. Nil allows all stores.
	ProductStores []string
}

type Item struct {
	ID                       string          `json:"id"`
	DocID                    string          `json:"doc_id"`
	Seq                      int             `json:"seq"`
	Text                     string          `json:"text"`
	Truncated                bool            `json:"truncated,omitempty"`
	NeighbourOf              string          `json:"neighbour_of,omitempty"`
	Products                 *[]ChunkProduct `json:"products,omitempty"`
	ProductsTotal            *int            `json:"products_total,omitempty"`
	ProductsOmittedForBudget int             `json:"products_omitted_for_budget,omitempty"`

	DocumentDate       *time.Time `json:"document_date,omitempty"`
	DocumentDateSource *string    `json:"document_date_source,omitempty"`
}

type Meta struct {
	Returned         int      `json:"returned"`
	Missing          []string `json:"missing"`
	Truncated        []string `json:"truncated"`
	OmittedForBudget []string `json:"omitted_for_budget"`
}

type Result struct {
	Chunks        []Item `json:"chunks"`
	Meta          Meta   `json:"meta"`
	ProductsError string `json:"products_error,omitempty"`
	LinksPartial  string `json:"links_partial,omitempty"`
}

type Store interface {
	// ChunkNeighbourhood returns the center and its neighbours with the owner's date metadata when available.
	ChunkNeighbourhood(ctx context.Context, id string, n int) (graph.Chunk, []graph.Chunk, error)
}

// DocumentProductLinker is optional on the chunk Store.
type DocumentProductLinker interface {
	DocumentProductLinks(context.Context, []string) (map[string]graph.DocumentProductLinks, error)
}

func Read(ctx context.Context, st Store, req Request) (Result, error) {
	if req.Domain != "" && !graph.ValidDomain(req.Domain) {
		return Result{}, argerr.New("invalid domain slug")
	}
	if len(req.IDs) < 1 || len(req.IDs) > MaxIDs {
		return Result{}, argerr.New("ids must hold 1–20 chunk ids")
	}
	if req.Neighbours < 0 || req.Neighbours > MaxNeighbours {
		return Result{}, argerr.New("neighbours must be between 0 and 2")
	}
	for _, id := range req.IDs {
		if len(id) > 128 {
			return Result{}, argerr.New("chunk ids must be at most 128 bytes")
		}
	}
	productsLimit := DefaultProductsLimit
	if req.ProductsLimit != nil {
		productsLimit = *req.ProductsLimit
		if productsLimit < 1 || productsLimit > MaxProductsLimit {
			return Result{}, argerr.New(ProductsLimitError)
		}
	}
	result := Result{
		Chunks: []Item{},
		Meta:   Meta{Missing: []string{}, Truncated: []string{}, OmittedForBudget: []string{}},
	}
	seen, requested := map[string]bool{}, map[string]bool{}
	var fullTexts []string
	add := func(c graph.Chunk, neighbourOf string) {
		if (req.Domain != "" && c.Domain != req.Domain) || seen[c.ID] {
			return
		}
		seen[c.ID] = true
		item := Item{
			ID: c.ID, DocID: c.DocID, Seq: c.Seq, Text: c.Text, NeighbourOf: neighbourOf,
			DocumentDate: c.DocumentDate, DocumentDateSource: c.DocumentDateSource,
		}
		if len(item.Text) > ChunkTextCap {
			end := ChunkTextCap
			for end > 0 && !utf8.RuneStart(item.Text[end]) {
				end--
			}
			item.Text, item.Truncated = item.Text[:end], true
			result.Meta.Truncated = append(result.Meta.Truncated, item.ID)
		}
		result.Chunks = append(result.Chunks, item)
		if req.WithProducts {
			fullTexts = append(fullTexts, c.Text)
		}
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
		if req.Domain != "" && center.Domain != req.Domain {
			result.Meta.Missing = append(result.Meta.Missing, id)
			continue
		}
		add(center, "")
		for _, c := range around {
			add(c, id)
		}
	}
	result.Meta.Returned = len(result.Chunks)
	if len(fullTexts) > 0 {
		if err := enrichProducts(ctx, st, &result, fullTexts, productsLimit, req.ProductStores); err != nil {
			return Result{}, err
		}
	}
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
