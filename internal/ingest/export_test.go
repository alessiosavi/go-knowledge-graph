package ingest

import (
	"context"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph"
)

// SetBeforeSupersede installs a test hook between listing older live versions
// and superseding them. Set it before calling Process.
func SetBeforeSupersede(p *Pipeline, f func(olds []string)) {
	p.beforeSupersede = f
}

type domainWriteBarrier struct {
	pipelineStore
	before func(string)
}

func (s domainWriteBarrier) UpsertExtractionOpts(ctx context.Context, doc, chunk, ver string, ents []graph.EntityUpsert, facts []graph.FactUpsert, opt graph.WriteOptions) error {
	s.before(doc)
	return s.pipelineStore.UpsertExtractionOpts(ctx, doc, chunk, ver, ents, facts, opt)
}

func SetBeforeDomainWrite(p *Pipeline, f func(string)) {
	p.store = domainWriteBarrier{pipelineStore: p.store, before: f}
}
