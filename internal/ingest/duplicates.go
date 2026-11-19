package ingest

import (
	"context"
	"errors"

	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

type duplicateRootStore interface {
	ChunkByID(context.Context, string) (graph.Chunk, error)
	Document(context.Context, string) (*graph.Document, error)
}

// DuplicateRoot is the available end of a duplicate chain. A zero Chunk
// requires local extraction. Cycle identifies a chain that loops.
type DuplicateRoot struct {
	graph.Chunk
	Cycle bool
}

// CanBorrow checks domain eligibility once the document's domain is known.
func (r DuplicateRoot) CanBorrow(docID, domain string) bool {
	return r.ID != "" && (r.DocID == docID || r.Domain == domain)
}

// ResolveDuplicateRoot follows a stored duplicate chain. A root from another
// document is available only after that document has completed ingest.
func ResolveDuplicateRoot(ctx context.Context, st duplicateRootStore, c graph.Chunk) (DuplicateRoot, error) {
	if c.DuplicateOf == "" {
		return DuplicateRoot{}, nil
	}
	seen := map[string]bool{c.ID: true}
	for next := c.DuplicateOf; ; {
		if seen[next] {
			return DuplicateRoot{Cycle: true}, nil
		}
		seen[next] = true
		root, err := st.ChunkByID(ctx, next)
		if errors.Is(err, graph.ErrNotFound) {
			return DuplicateRoot{}, nil
		}
		if err != nil {
			return DuplicateRoot{}, err
		}
		if root.DuplicateOf != "" {
			next = root.DuplicateOf
			continue
		}
		doc, err := st.Document(ctx, root.DocID)
		if errors.Is(err, graph.ErrNotFound) {
			return DuplicateRoot{}, nil
		}
		if err != nil {
			return DuplicateRoot{}, err
		}
		if root.DocID != c.DocID && doc.Status != graph.StatusDone {
			return DuplicateRoot{}, nil
		}
		root.Domain = doc.Domain
		return DuplicateRoot{Chunk: root}, nil
	}
}
