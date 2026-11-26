package graph

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func TestResolveIngestDomain(t *testing.T) {
	row := func(id, name string) *neo4j.Record {
		return &neo4j.Record{Keys: []string{"id", "name"}, Values: []any{id, name}}
	}
	down := errors.New("graph unavailable")
	for _, tc := range []struct {
		name, value, want, code, detail string
		exact, names                    []*neo4j.Record
		idErr, namesErr                 error
		wantReads                       int
	}{
		{name: "id before colliding names", value: "exact", want: "exact",
			exact: []*neo4j.Record{row("exact", "Different name")}, wantReads: 1},
		{name: "case insensitive name", value: "RICERCA / CITTÀ", want: "research",
			names: []*neo4j.Record{row("research", "Ricerca / Città")}, wantReads: 2},
		{name: "ambiguous name", value: "research", code: "ambiguous_domain", detail: "one, two",
			names: []*neo4j.Record{row("two", "RESEARCH"), row("one", "Research")}, wantReads: 2},
		{name: "unknown with punctuation", value: ": /", code: "unknown_domain",
			detail: "GET /v1/domains lists them", wantReads: 2},
		{name: "id lookup failure", value: "exact", idErr: down, wantReads: 1},
		{name: "name lookup failure", value: "research", namesErr: down, wantReads: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			got, err := resolveIngestDomain(t.Context(), tc.value, func(_ context.Context, query string, params map[string]any) ([]*neo4j.Record, error) {
				reads++
				if reads == 1 {
					if !strings.Contains(query, "{id:$id}") || params["id"] != tc.value {
						t.Fatalf("first read is not an exact ID lookup: %s %v", query, params)
					}
					return tc.exact, tc.idErr
				}
				if strings.Contains(query, ":Document") || strings.Contains(query, ":Product") {
					t.Fatalf("name lookup counts corpus data: %s", query)
				}
				return tc.names, tc.namesErr
			})
			if got != tc.want || reads != tc.wantReads {
				t.Errorf("resolved %q after %d reads, want %q after %d reads", got, reads, tc.want, tc.wantReads)
			}
			switch {
			case tc.code != "":
				var rejection *IngestDomainError
				if !errors.As(err, &rejection) || rejection.Conflict || rejection.Code != tc.code || !strings.Contains(rejection.Detail, tc.detail) {
					t.Errorf("error = %v, want %s with %q", err, tc.code, tc.detail)
				}
			default:
				want := tc.idErr
				if want == nil {
					want = tc.namesErr
				}
				if !errors.Is(err, want) {
					t.Errorf("error = %v, want %v", err, want)
				}
			}
		})
	}
}
