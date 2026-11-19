package ingest

import "github.com/alessiosavi/go-knowledge-graph/internal/graph"

// WriteMode enables ingestion writes supported by the graph's schema marker.
// The zero value keeps the legacy extraction behavior.
type WriteMode struct {
	Hygiene, Domains, RequireConfirmation bool
}

// WriteModeForSchema derives the write mode from the extraction schema marker
// and the configured fact confirmation setting.
func WriteModeForSchema(schemaVersion int, requireConfirmation bool) WriteMode {
	v4 := schemaVersion >= graph.SchemaVersionV4
	return WriteMode{Hygiene: v4, Domains: v4, RequireConfirmation: v4 && requireConfirmation}
}

// SetWriteMode configures the pipeline before processing documents.
func (p *Pipeline) SetWriteMode(m WriteMode) { p.writeMode = m }

// WriteMode returns the configured ingestion write mode.
func (p *Pipeline) WriteMode() WriteMode { return p.writeMode }

func (p *Pipeline) writeOptions(doc *graph.Document) graph.WriteOptions {
	opt := graph.WriteOptions{FactsAsCandidates: p.writeMode.RequireConfirmation}
	if p.writeMode.Domains {
		opt.Domain = doc.Domain
	}
	return opt
}
