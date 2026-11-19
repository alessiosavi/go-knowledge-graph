package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

type SampleCounts struct {
	Documents int `json:"documents"`
	Chunks    int `json:"chunks"`
}

type sampleHeader struct {
	Type   string       `json:"type"`
	Domain string       `json:"domain"`
	Counts SampleCounts `json:"counts"`
	Seed   int64        `json:"seed"`
}

type sampleDocument struct {
	Type        string        `json:"type"`
	DocID       string        `json:"doc_id"`
	Title       string        `json:"title"`
	TitleSource string        `json:"title_source"`
	Source      string        `json:"source"`
	Labels      *string       `json:"labels"` // nil for samples exported before labels were recorded
	Chunks      []sampleChunk `json:"-"`
}

type sampleChunk struct {
	Type          string `json:"type"`
	DocID         string `json:"doc_id"`
	ChunkID       string `json:"chunk_id"`
	Seq           int    `json:"seq"`
	Start         int    `json:"start"`
	End           int    `json:"end"`
	Text          string `json:"text"`
	DuplicateOf   string `json:"duplicate_of"`
	DupDocID      string `json:"dup_doc_id"`
	DuplicateRoot string `json:"duplicate_root,omitempty"`
}

type sample struct {
	Header    sampleHeader
	Documents []sampleDocument
	SHA256    string
}

// WriteSample exports a domain's completed documents and their stored chunks.
// Sampling starts from the entire domain and is stable for the same ids and seed.
func WriteSample(ctx context.Context, st *graph.Store, domain, path string, limit int, seed int64) (SampleCounts, error) {
	docs, err := st.DocumentsIn(ctx, graph.StatusDone, domain, 0)
	if err != nil {
		return SampleCounts{}, err
	}
	byID := func(a, b graph.Document) int { return strings.Compare(a.ID, b.ID) }
	slices.SortFunc(docs, byID)
	if limit > 0 && limit < len(docs) {
		rng := rand.New(rand.NewSource(seed))
		rng.Shuffle(len(docs), func(i, j int) { docs[i], docs[j] = docs[j], docs[i] })
		docs = docs[:limit]
		slices.SortFunc(docs, byID)
	}

	counts := SampleCounts{Documents: len(docs)}
	rows := make([]sampleDocument, 0, len(docs))
	for _, d := range docs {
		chunks, err := st.Chunks(ctx, d.ID)
		if err != nil {
			return SampleCounts{}, err
		}
		row := sampleDocument{
			Type: "document", DocID: d.ID, Title: d.Title, TitleSource: d.TitleSource, Source: d.Source, Labels: &d.Labels,
		}
		for _, c := range chunks {
			chunk := sampleChunk{
				Type: "chunk", DocID: d.ID, ChunkID: c.ID, Seq: c.Seq, Start: c.Start, End: c.End,
				Text: c.Text,
			}
			root, err := ingest.ResolveDuplicateRoot(ctx, st, c)
			if err != nil {
				return SampleCounts{}, err
			}
			if root.CanBorrow(d.ID, d.Domain) {
				chunk.DuplicateOf, chunk.DupDocID = root.ID, root.DocID
			}
			if root.Cycle {
				chunk.DuplicateRoot = duplicateRootCycle
			}
			row.Chunks = append(row.Chunks, chunk)
		}
		rows = append(rows, row)
		counts.Chunks += len(chunks)
	}
	err = writeArtifact(path, func(enc *json.Encoder) (any, error) {
		if err := enc.Encode(sampleHeader{Type: "header", Domain: domain, Counts: counts, Seed: seed}); err != nil {
			return nil, err
		}
		for _, d := range rows {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := enc.Encode(d); err != nil {
				return nil, err
			}
			for _, c := range d.Chunks {
				if err := enc.Encode(c); err != nil {
					return nil, err
				}
			}
		}
		return counts, nil
	})
	return counts, err
}

func readSample(path string) (sample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sample{}, err
	}
	var out sample
	var counts SampleCounts
	seenDocs, seenChunks := make(map[string]bool), make(map[string]bool)
	var current *sampleDocument
	lineNumber, offset := 0, 0
	for line := range bytes.Lines(data) {
		lineNumber++
		var tag struct{ Type string }
		if err := json.Unmarshal(line, &tag); err != nil {
			return sample{}, fmt.Errorf("sample line %d: %w", lineNumber, err)
		}
		if lineNumber == 1 && tag.Type != "header" {
			return sample{}, errors.New("sample must start with a header")
		}
		switch tag.Type {
		case "header":
			if lineNumber != 1 || json.Unmarshal(line, &out.Header) != nil || !graph.ValidDomain(out.Header.Domain) {
				return sample{}, errors.New("invalid sample header")
			}
		case "document":
			var doc sampleDocument
			if json.Unmarshal(line, &doc) != nil || doc.DocID == "" || seenDocs[doc.DocID] {
				return sample{}, fmt.Errorf("sample line %d: invalid or repeated document", lineNumber)
			}
			out.Documents = append(out.Documents, doc)
			current = &out.Documents[len(out.Documents)-1]
			seenDocs[doc.DocID] = true
			counts.Documents++
		case "chunk":
			var chunk sampleChunk
			if json.Unmarshal(line, &chunk) != nil || chunk.ChunkID == "" || seenChunks[chunk.ChunkID] ||
				current == nil || current.DocID != chunk.DocID {
				return sample{}, fmt.Errorf("sample line %d: invalid, repeated or orphaned chunk", lineNumber)
			}
			current.Chunks = append(current.Chunks, chunk)
			seenChunks[chunk.ChunkID] = true
			counts.Chunks++
		case "trailer":
			var trailer struct {
				Counts SampleCounts `json:"counts"`
				SHA256 string       `json:"sha256"`
			}
			if offset+len(line) != len(data) || json.Unmarshal(line, &trailer) != nil ||
				counts != out.Header.Counts || counts != trailer.Counts || sha256Hex(data[:offset]) != trailer.SHA256 {
				return sample{}, errors.New("sample trailer, counts or SHA-256 do not match")
			}
			out.SHA256 = sha256Hex(data)
			return out, nil
		default:
			return sample{}, fmt.Errorf("sample line %d: unknown row type %q", lineNumber, tag.Type)
		}
		offset += len(line)
	}
	return sample{}, errors.New("sample is missing its trailer")
}
