package mcp

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/alessiosavi/go-knowledge-graph/internal/config"
	"github.com/alessiosavi/go-knowledge-graph/internal/graph/graphtest"
)

func TestDateWindowMCP(t *testing.T) {
	f := graphtest.DatedEvidence(t)
	cs := connectInMemory(t, config.Config{}, f.Store, nil)
	for _, tool := range []struct {
		name, field, idKey string
		args               map[string]any
	}{
		{"search", "hits", "document_id", map[string]any{"query": f.Domain, "kind": "chunk", "domain": f.Domain}},
		{"get_entity", "facts", "doc_id", map[string]any{"id": f.EntityID}},
		{"traverse", "edges", "doc_id", map[string]any{"entity": f.EntityID}},
		{"list_documents", "documents", "id", map[string]any{"domain": f.Domain}},
	} {
		t.Run(tool.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, since, until, wantID, wantError string
			}{
				{name: "since", since: "2026-03-01", wantID: f.NewID},
				{name: "until", until: "2024-03-01", wantID: f.OldID},
				{name: "both", since: "2024-03-01T18:00:00+01:00", until: "2024-03-01", wantID: f.OldID},
				{name: "bad_since", since: "yesterday", wantError: "since must be YYYY-MM-DD"},
				{name: "bad_until", until: "yesterday", wantError: "until must be YYYY-MM-DD"},
				{name: "inverted", since: "2026-03-01", until: "2024-03-01", wantError: "since must be at or before until"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					args := maps.Clone(tool.args)
					if tc.since != "" {
						args["since"] = tc.since
					}
					if tc.until != "" {
						args["until"] = tc.until
					}
					res := callTool(t, cs, tool.name, args)
					if tc.wantError != "" {
						if detail := toolErrorText(res); !res.IsError || !strings.Contains(detail, tc.wantError) {
							t.Fatalf("tool error = %t, detail = %q, want %q", res.IsError, detail, tc.wantError)
						}
						return
					}
					out := structured[map[string]json.RawMessage](t, res)
					var rows []map[string]any
					if err := json.Unmarshal(out[tool.field], &rows); err != nil {
						t.Fatal(err)
					}
					if len(rows) != 1 || rows[0][tool.idKey] != tc.wantID {
						t.Fatalf("%s = %v, want one row from %s", tool.field, rows, tc.wantID)
					}
				})
			}
		})
	}
}
