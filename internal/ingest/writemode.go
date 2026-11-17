package ingest

import "github.com/alessiosavi/go-knowledge-graph/internal/graph"

// WriteMode enables ingestion writes supported by the graph's schema marker.
// The zero value keeps the legacy extraction behavior.
type WriteMode struct {
	Hygiene, Domains, RequireConfirmation bool
}

// SetWriteMode configures the pipeline before processing documents.
func (p *Pipeline) SetWriteMode(m WriteMode) { p.writeMode = m }

// WriteMode returns the configured ingestion write mode.
func (p *Pipeline) WriteMode() WriteMode { return p.writeMode }

func (p *Pipeline) writeOptions(doc *graph.Document) graph.WriteOptions {
	return graph.WriteOptions{FactsAsCandidates: p.writeMode.RequireConfirmation}
}
