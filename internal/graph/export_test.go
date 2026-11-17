package graph

import (
	"context"
	"maps"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func AssignDomainFailForTest(ctx context.Context, s *Store, a Assignment, err error) error {
	return s.assignDomain(ctx, a, func() error { return err })
}

// Expose the production queries only to the external integration tests.
const (
	EntityByIDQuery  = entityByIDQuery
	EntityByKeyQuery = entityByKeyQuery
)

type CapturedReadQuery struct {
	Query  string
	Params map[string]any
}

// CaptureReadQueries observes real store reads without changing their execution.
// Callers must not use the store concurrently while recording.
func CaptureReadQueries(st *Store, read func()) []CapturedReadQuery {
	var queries []CapturedReadQuery
	driver := st.driver
	st.driver = recordingReadDriver{Driver: driver, queries: &queries}
	defer func() { st.driver = driver }()
	read()
	return queries
}

type recordingReadDriver struct {
	neo4j.Driver
	queries *[]CapturedReadQuery
}

func (d recordingReadDriver) NewSession(ctx context.Context, cfg neo4j.SessionConfig) neo4j.Session {
	return recordingReadSession{Session: d.Driver.NewSession(ctx, cfg), queries: d.queries}
}

type recordingReadSession struct {
	neo4j.Session
	queries *[]CapturedReadQuery
}

func (s recordingReadSession) Run(ctx context.Context, query string, params map[string]any, cfg ...func(*neo4j.TransactionConfig)) (neo4j.Result, error) {
	*s.queries = append(*s.queries, CapturedReadQuery{query, maps.Clone(params)})
	return s.Session.Run(ctx, query, params, cfg...)
}
