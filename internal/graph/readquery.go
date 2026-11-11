package graph

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// QueryError marks a query-level failure — bad syntax, unknown function,
// forbidden clause that slipped past the caller's guard — as opposed to a
// connectivity or store failure. The HTTP layer maps it to a 400.
type QueryError struct{ Err error }

func (e *QueryError) Error() string { return e.Err.Error() }
func (e *QueryError) Unwrap() error { return e.Err }

// QueryResult is a read-only query's tabular result; Columns preserves the
// RETURN order.
type QueryResult struct {
	Columns []string
	Rows    []map[string]any
}

// RunReadQuery executes an already-guarded read-only query. rows > 0 is
// passed as the $cypher_rows parameter the guard's injected LIMIT consumes.
// Values are converted to JSON-serializable shapes: nodes, relationships
// and paths become maps; dates and local date-times become ISO 8601
// strings; durations and points become structured objects.
func (s *Store) RunReadQuery(ctx context.Context, query string, params map[string]any, rows int) (QueryResult, error) {
	if rows > 0 {
		if params == nil {
			params = map[string]any{}
		}
		params["cypher_rows"] = rows
	}
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{AccessMode: neo4j.AccessModeRead})
	defer func() { _ = sess.Close(ctx) }()
	res, err := sess.Run(ctx, query, params)
	if err != nil {
		return QueryResult{}, queryError(err)
	}
	keys, err := res.Keys()
	if err != nil {
		return QueryResult{}, queryError(err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		return QueryResult{}, queryError(err)
	}
	out := QueryResult{Columns: keys, Rows: make([]map[string]any, 0, len(recs))}
	for _, rec := range recs {
		row := make(map[string]any, len(keys))
		for _, k := range keys {
			v, _ := rec.Get(k)
			row[k] = boltToJSON(v)
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
}

// queryError wraps client-reported query failures (the ...ClientError....
// class in both Neo4j-style and Memgraph-style codes: syntax errors,
// unknown procedures) so callers can tell them from server-side or
// connectivity failures, which stay 5xx.
func queryError(err error) error {
	var nerr *neo4j.Neo4jError
	if errors.As(err, &nerr) && strings.Contains(nerr.Code, ".ClientError.") {
		return &QueryError{Err: err}
	}
	return err
}

// boltToJSON converts one Bolt value into a JSON-serializable shape.
func boltToJSON(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string, bool, int64, float64, int:
		return x
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case neo4j.Date:
		return x.Time().Format("2006-01-02")
	case neo4j.LocalTime:
		return x.Time().Format("15:04:05.999999999")
	case neo4j.LocalDateTime:
		return x.Time().Format("2006-01-02T15:04:05.999999999")
	case neo4j.Duration:
		return map[string]any{"months": x.Months, "days": x.Days, "seconds": x.Seconds, "nanos": x.Nanos}
	case neo4j.Point2D:
		return map[string]any{"srid": x.SpatialRefId, "x": x.X, "y": x.Y}
	case neo4j.Point3D:
		return map[string]any{"srid": x.SpatialRefId, "x": x.X, "y": x.Y, "z": x.Z}
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, boltToJSON(e))
		}
		return out
	case []string:
		out := make([]string, len(x))
		copy(out, x)
		return out
	case map[string]any:
		return propsToJSON(x)
	case neo4j.Node:
		return map[string]any{
			"labels":     append([]string{}, x.Labels...),
			"properties": propsToJSON(x.Props),
		}
	case neo4j.Relationship:
		return map[string]any{
			"type":       x.Type,
			"properties": propsToJSON(x.Props),
		}
	case neo4j.Path:
		nodes := make([]any, 0, len(x.Nodes))
		for _, n := range x.Nodes {
			nodes = append(nodes, boltToJSON(n))
		}
		rels := make([]any, 0, len(x.Relationships))
		for _, r := range x.Relationships {
			rels = append(rels, boltToJSON(r))
		}
		return map[string]any{"nodes": nodes, "relationships": rels}
	default:
		return x
	}
}

// propsToJSON converts a properties map value by value.
func propsToJSON(props map[string]any) map[string]any {
	out := make(map[string]any, len(props))
	for k, v := range props {
		out[k] = boltToJSON(v)
	}
	return out
}
