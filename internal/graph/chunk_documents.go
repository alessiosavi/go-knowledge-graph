package graph

import (
	"context"
	"fmt"
	"time"
)

type ChunkDocument struct {
	DocumentID, Title, Source string
	DocumentDate              *time.Time
	DocumentDateSource        *string
}

// ChunkDocuments reads the owning document metadata for chunks in the given domain.
func (s *Store) ChunkDocuments(ctx context.Context, domain string, chunkIDs []string) (map[string]ChunkDocument, error) {
	out := make(map[string]ChunkDocument)
	if len(chunkIDs) == 0 {
		return out, nil
	}
	rows, err := s.runRead(ctx, `
UNWIND $ids AS id
MATCH (c:Chunk {id:id})
MATCH (d:Document {id:c.doc_id})
WHERE d.domain = $domain
RETURN c.id AS chunk_id, d.id AS doc_id, d.title AS title, d.source AS source, `+documentDateColumns("d"),
		map[string]any{"domain": domain, "ids": chunkIDs})
	if err != nil {
		return nil, fmt.Errorf("graph: chunk documents: %w", err)
	}
	for _, row := range rows {
		date, source := recordDocumentDate(row)
		out[getString(row, "chunk_id")] = ChunkDocument{
			DocumentID: getString(row, "doc_id"), Title: getString(row, "title"), Source: getString(row, "source"),
			DocumentDate: date, DocumentDateSource: source,
		}
	}
	return out, nil
}
