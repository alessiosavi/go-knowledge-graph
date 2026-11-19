package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"slices"
	"strings"

	"github.com/alessiosavi/go-knowledge-graph/internal/argerr"
	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
	"github.com/alessiosavi/go-knowledge-graph/internal/ingest"
)

type SampleCounts struct {
	Documents int `json:"documents"`
	Chunks    int `json:"chunks"`
}

type sampleHeader struct {
	Type           string       `json:"type"`
	Domain         string       `json:"domain"`
	Counts         SampleCounts `json:"counts"`
	Seed           int64        `json:"seed"`
	Origin         string       `json:"origin,omitempty"`
	InputSHA256    string       `json:"input_sha256,omitempty"`
	DuplicateLinks string       `json:"duplicate_links,omitempty"`
}

type sampleDocument struct {
	Type        string          `json:"type"`
	DocID       string          `json:"doc_id"`
	Title       string          `json:"title"`
	TitleSource string          `json:"title_source"`
	Source      string          `json:"source"`
	Labels      *string         `json:"labels"` // nil when there is no stored preset
	ProductID   json.RawMessage `json:"product_id,omitempty"`
	Kind        json.RawMessage `json:"kind,omitempty"`
	Chunks      []sampleChunk   `json:"-"`
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

type externalDocument struct {
	ID        string          `json:"id"`
	Source    string          `json:"source"`
	Title     string          `json:"title"`
	Text      string          `json:"text"`
	ProductID json.RawMessage `json:"product_id"`
	Kind      json.RawMessage `json:"kind"`
}

// WriteSampleFromFile builds a replay sample from inline text documents in a
// JSONL file. Documents and chunks are emitted in file and sequence order.
func WriteSampleFromFile(ctx context.Context, cfg config.IngestConfig, domain, input, output string) (SampleCounts, error) {
	if !graph.ValidDomain(domain) {
		return SampleCounts{}, argerr.Errorf("invalid domain slug %q", domain)
	}
	data, err := os.ReadFile(input)
	if err != nil {
		return SampleCounts{}, err
	}
	var rows []sampleDocument
	var counts SampleCounts
	seen := make(map[string]bool)
	lineNumber := 0
	for line := range bytes.Lines(data) {
		lineNumber++
		if err := ctx.Err(); err != nil {
			return SampleCounts{}, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			return SampleCounts{}, argerr.Errorf("input line %d: blank line", lineNumber)
		}
		var in externalDocument
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return SampleCounts{}, argerr.Errorf("input line %d: %w", lineNumber, err)
		}
		var extra json.RawMessage
		if err := dec.Decode(&extra); err != io.EOF {
			return SampleCounts{}, argerr.Errorf("input line %d: expected one JSON object", lineNumber)
		}
		if trimmed := strings.TrimSpace(in.ID); trimmed == "" {
			return SampleCounts{}, argerr.Errorf("input line %d: id is required", lineNumber)
		} else if trimmed != in.ID {
			return SampleCounts{}, argerr.Errorf("input line %d: id has leading or trailing whitespace", lineNumber)
		}
		if seen[in.ID] {
			return SampleCounts{}, argerr.Errorf("input line %d: duplicate id %q", lineNumber, in.ID)
		}
		if emptyMetadata(in.ProductID) {
			return SampleCounts{}, argerr.Errorf("input line %d: product_id is required and must be non-empty", lineNumber)
		}
		if emptyMetadata(in.Kind) {
			return SampleCounts{}, argerr.Errorf("input line %d: kind is required and must be non-empty", lineNumber)
		}
		seen[in.ID] = true
		doc, chunks, err := ingest.PrepareText(ctx, cfg, ingest.Input{Source: in.Source, Title: in.Title, Text: in.Text})
		if err != nil {
			return SampleCounts{}, fmt.Errorf("input line %d (id %q): %w", lineNumber, in.ID, err)
		}
		row := sampleDocument{
			Type: "document", DocID: in.ID, Title: doc.Title, TitleSource: doc.TitleSource, Source: doc.Source,
			ProductID: in.ProductID, Kind: in.Kind, Chunks: make([]sampleChunk, 0, len(chunks)),
		}
		for _, c := range chunks {
			row.Chunks = append(row.Chunks, sampleChunk{
				Type: "chunk", DocID: in.ID, ChunkID: graph.ChunkID(in.ID, c.Seq), Seq: c.Seq, Start: c.Start, End: c.End, Text: c.Text,
			})
		}
		rows = append(rows, row)
		counts.Documents++
		counts.Chunks += len(chunks)
	}
	if counts.Documents == 0 {
		return SampleCounts{}, argerr.New("input contains no document rows")
	}
	header := sampleHeader{
		Type: "header", Domain: domain, Counts: counts, Origin: "from-file", InputSHA256: sha256Hex(data),
		DuplicateLinks: "none",
	}
	return counts, writeSample(ctx, output, header, rows)
}

func emptyMetadata(value json.RawMessage) bool {
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return true
	}
	switch value[0] {
	case '"':
		var s string
		return json.Unmarshal(value, &s) == nil && strings.TrimSpace(s) == ""
	case '[', '{':
		return len(bytes.TrimSpace(value[1:len(value)-1])) == 0
	default:
		return false
	}
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
	header := sampleHeader{Type: "header", Domain: domain, Counts: counts, Seed: seed}
	return counts, writeSample(ctx, path, header, rows)
}

func writeSample(ctx context.Context, path string, header sampleHeader, rows []sampleDocument) error {
	return writeArtifact(path, func(enc *json.Encoder) (any, error) {
		if err := enc.Encode(header); err != nil {
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
		return header.Counts, nil
	})
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
