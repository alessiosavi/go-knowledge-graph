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

type domainAssignmentHook struct {
	pipelineStore
	before func(graph.Assignment) error
}

func (s domainAssignmentHook) AssignDomain(ctx context.Context, a graph.Assignment) error {
	if err := s.before(a); err != nil {
		return err
	}
	return s.pipelineStore.AssignDomain(ctx, a)
}

func SetBeforeDomainAssignment(p *Pipeline, f func(graph.Assignment) error) {
	p.store = domainAssignmentHook{pipelineStore: p.store, before: f}
}

// SetDomainOpStore intercepts admin boundaries while keeping a real graph store.
func SetDomainOpStore(p *Pipeline, st interface {
	pipelineStore
	domainOpStore
}) {
	p.store = st
}

// SetBeforeDocumentUpsert installs a hook after the candidate is spooled.
func SetBeforeDocumentUpsert(p *Pipeline, f func()) {
	p.store = documentUpsertHook{pipelineStore: p.store, before: f}
}

type documentUpsertHook struct {
	pipelineStore
	before func()
}

func (s documentUpsertHook) UpsertDocument(ctx context.Context, in graph.DocumentInput) (graph.UpsertResult, error) {
	s.before()
	return s.pipelineStore.UpsertDocument(ctx, in)
}
